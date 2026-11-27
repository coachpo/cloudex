// Package codexupstream describes the two authorization and inference protocols
// accepted by the Cloudex gateway.
package codexupstream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
)

const (
	ChannelCodex   = "codex"
	ChannelChatGPT = "chatgpt"

	CodexClientVersion   = "0.172.0"
	CodexClientID        = "app_EMoamEEZ73f0CkXaXp7hrann"
	ChatGPTResource      = "https://api.openai.com/v1"
	CodexTurnStateHeader = "X-Codex-Turn-State"

	openAIIssuer     = "https://auth.openai.com"
	productName      = "Cloudex"
	originator       = "cloudex"
	maxResponseBytes = 8 << 20
)

type Config struct {
	Channel, Issuer, AuthorizationURL, TokenURL, JWKSURL string
	RevokeURL, DiscoveryURL, AccountsURL                 string
	ModelsURL, ResponsesURL                              string
	// CatalogRoutes holds independently established OAuth catalog URLs keyed
	// by dedicated Responses origin returned by the trusted accounts service.
	CatalogRoutes map[string]string
	HTTPClient    *http.Client
}

type Grant struct {
	ClientID, IDToken, AccessToken, RefreshToken string
	Subject, UserID, AccountID                   string
	DisplayName, Email, WorkspaceName, Resource  string
	Scopes                                       []string
	ExpiresAt                                    time.Time
	EarliestRefreshAt                            int64
	FedRAMP                                      bool
}

type Authorization struct {
	State, Challenge, Redirect, Nonce, AgentHostID string
	Previous                                       Grant
	Consent                                        bool
}

type Route struct {
	CatalogURL, ResponsesURL, RoutingOverride, WorkspaceName string
}

type Catalog struct {
	Models []json.RawMessage `json:"models"`
	Slugs  []string          `json:"-"`
}

type Client struct {
	config           Config
	oauth, responses *http.Client
}

func New(config Config) (*Client, error) {
	endpoints := []string{
		config.Issuer, config.AuthorizationURL, config.TokenURL, config.JWKSURL,
		config.ModelsURL, config.ResponsesURL,
	}
	switch config.Channel {
	case ChannelCodex:
		endpoints = append(endpoints, config.RevokeURL, config.AccountsURL)
	case ChannelChatGPT:
		endpoints = append(endpoints, config.DiscoveryURL)
	default:
		return nil, errors.New("unknown authorization channel")
	}
	for _, endpoint := range endpoints {
		if !validEndpoint(endpoint) {
			return nil, fmt.Errorf("%s adapter requires HTTPS endpoints or HTTP loopback test endpoints", config.Channel)
		}
	}
	for _, endpoint := range config.CatalogRoutes {
		if !validEndpoint(endpoint) {
			return nil, errors.New("invalid dedicated model catalog endpoint")
		}
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = 30 * time.Second
	defaultClient := &http.Client{Transport: transport}
	if config.HTTPClient != nil {
		defaultClient = config.HTTPClient
	}
	responses := *defaultClient
	// OAuth, WS and HTTP must all retain their selected endpoint and credential
	// if an upstream tries to redirect. Streaming has separate gateway deadlines.
	responses.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	responses.Jar = nil
	responses.Timeout = 0
	oauth := responses
	oauth.Timeout = 45 * time.Second
	return &Client{config: config, oauth: &oauth, responses: &responses}, nil
}

func validEndpoint(s string) bool {
	u, err := url.Parse(s)
	if err != nil || u.Hostname() == "" || u.User != nil || strings.Contains(s, "#") || u.Opaque != "" {
		return false
	}
	ip := net.ParseIP(u.Hostname())
	return u.Scheme == "https" || u.Scheme == "http" && ip != nil && ip.IsLoopback()
}

func (c *Client) Channel() string          { return c.config.Channel }
func (c *Client) Issuer() string           { return c.config.Issuer }
func (c *Client) HTTPClient() *http.Client { return c.responses }
func (c *Client) SupportsInterrupt() bool  { return c.Channel() == ChannelCodex }

func (c *Client) PlanGranted(grant Grant) bool {
	return c.Channel() == ChannelCodex || slices.Contains(grant.Scopes, directScope)
}

func (c *Client) RequirePlan(grant Grant) error {
	if !c.PlanGranted(grant) {
		return &UpstreamError{StatusCode: http.StatusForbidden, Code: "plan_permission_required", Stage: "scope"}
	}
	return nil
}

func (c *Client) AuthorizeURL(a Authorization) string {
	u, _ := url.Parse(c.config.AuthorizationURL)
	values := u.Query()
	values.Set("response_type", "code")
	values.Set("redirect_uri", a.Redirect)
	values.Set("state", a.State)
	values.Set("code_challenge", a.Challenge)
	values.Set("code_challenge_method", "S256")
	if c.Channel() == ChannelCodex {
		codexAuthorize(values, a)
	} else {
		chatGPTAuthorize(values, a)
	}
	u.RawQuery = values.Encode()
	return u.String()
}

func (c *Client) Exchange(ctx context.Context, code, verifier, redirect, nonce, callbackClient string, previous Grant) (Grant, error) {
	if code == "" || verifier == "" || !validRedirect(redirect, c.Channel()) {
		return Grant{}, &UpstreamError{StatusCode: http.StatusBadRequest, Code: "invalid_oauth_callback", Stage: "exchange"}
	}
	form := url.Values{
		"grant_type": {"authorization_code"}, "code": {code},
		"code_verifier": {verifier}, "redirect_uri": {redirect},
	}
	if c.Channel() == ChannelCodex {
		return c.codexExchange(ctx, form, callbackClient, previous)
	}
	return c.chatGPTExchange(ctx, form, nonce, callbackClient, previous)
}

func validRedirect(raw, channel string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "http" || u.Hostname() != "127.0.0.1" || u.User != nil ||
		u.ForceQuery || u.RawQuery != "" || strings.Contains(raw, "#") || u.RawPath != "" {
		return false
	}
	port := u.Port()
	if port == "" || port == "0" {
		return false
	}
	if channel == ChannelCodex {
		return u.Path == "/auth/callback" && (port == "1455" || port == "1457")
	}
	return strings.HasPrefix(u.Path, "/")
}

func (c *Client) Refresh(ctx context.Context, previous Grant) (Grant, error) {
	if !validBearer(previous.RefreshToken) {
		return Grant{}, ErrReauthorizationRequired
	}
	if c.Channel() == ChannelCodex {
		return c.codexRefresh(ctx, previous)
	}
	return c.chatGPTRefresh(ctx, previous)
}

func (c *Client) Revoke(ctx context.Context, grant Grant) error {
	if c.Channel() == ChannelCodex {
		return c.codexRevoke(ctx, grant)
	}
	return c.chatGPTRevoke(ctx, grant)
}

func (c *Client) ResolveRoute(ctx context.Context, grant Grant) (Route, error) {
	if err := c.RequirePlan(grant); err != nil {
		return Route{}, err
	}
	if c.Channel() == ChannelCodex {
		return c.codexRoute(ctx, grant)
	}
	return Route{CatalogURL: c.config.ModelsURL, ResponsesURL: c.config.ResponsesURL}, nil
}

func (c *Client) RequestHeaders(grant Grant, route Route, websocket bool) (http.Header, error) {
	if err := c.RequirePlan(grant); err != nil {
		return nil, err
	}
	if !validBearer(grant.AccessToken) {
		return nil, &UpstreamError{StatusCode: http.StatusUnauthorized, Code: "authorization_rejected", Stage: "authorization"}
	}
	headers := make(http.Header)
	headers.Set("Authorization", "Bearer "+grant.AccessToken)
	headers.Set("User-Agent", productName+" (Codex "+CodexClientVersion+")")
	if !websocket {
		headers.Set("Accept", "text/event-stream")
		headers.Set("Content-Type", "application/json")
	}
	if c.Channel() == ChannelCodex {
		if grant.ClientID != CodexClientID || !validBearer(grant.AccountID) {
			return nil, &UpstreamError{StatusCode: http.StatusForbidden, Code: "codex_workspace_unverified", Stage: "authorization"}
		}
		headers.Set("Originator", originator)
		headers.Set("ChatGPT-Account-ID", grant.AccountID)
		if route.RoutingOverride != "" {
			headers.Set("X-OpenAI-Account-Routing-Override", route.RoutingOverride)
		}
		if websocket {
			headers.Set("OpenAI-Beta", "responses_websockets=2026-02-06")
		}
	} else if !validIssuedClientID(grant.ClientID) || grant.Resource != ChatGPTResource {
		return nil, &UpstreamError{StatusCode: http.StatusForbidden, Code: "invalid_client", Stage: "authorization"}
	}
	return headers, nil
}

func validBearer(value string) bool {
	return value != "" && strings.IndexFunc(value, func(r rune) bool { return r < 33 || r > 126 }) < 0
}

func readJSON(r io.Reader, destination any) error {
	data, err := io.ReadAll(io.LimitReader(r, maxResponseBytes+1))
	if err != nil {
		return err
	}
	if len(data) > maxResponseBytes {
		return errors.New("upstream JSON exceeds the response limit")
	}
	return json.Unmarshal(data, destination)
}
