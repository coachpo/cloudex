package codexupstream

import (
	"context"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
)

const (
	dynamicClientID = "dynamic_agent_client"
	directScope     = "chatgpt.tokens.use.direct"
	chatGPTScope    = "openid profile email offline_access resource.invoke " + directScope
	formContentType = "application/x-www-form-urlencoded"
)

func ChatGPTConfig() Config {
	return Config{
		Channel: ChannelChatGPT, Issuer: openAIIssuer,
		AuthorizationURL: openAIIssuer + "/api/accounts/authorize",
		TokenURL:         openAIIssuer + "/api/accounts/oauth/token",
		JWKSURL:          openAIIssuer + "/.well-known/jwks.json",
		DiscoveryURL:     openAIIssuer + "/.well-known/openid-configuration",
		ModelsURL:        ChatGPTResource + "/models",
		ResponsesURL:     ChatGPTResource + "/responses",
	}
}

func chatGPTAuthorize(values url.Values, a Authorization) {
	values.Set("scope", chatGPTScope)
	values.Set("resource", ChatGPTResource)
	values.Set("nonce", a.Nonce)
	values.Set("ext_agent_host_id", a.AgentHostID)
	if a.Previous.ClientID == "" {
		values.Set("client_id", dynamicClientID)
		values.Set("agent_name_hint", productName)
		return
	}
	values.Set("client_id", a.Previous.ClientID)
	if a.Previous.IDToken != "" {
		values.Set("id_token_hint", a.Previous.IDToken)
	}
	if a.Previous.Email != "" {
		values.Set("login_hint", a.Previous.Email)
	}
	if a.Consent {
		values.Set("prompt", "consent")
	}
}

func validIssuedClientID(id string) bool {
	return validBearer(id) && len(id) <= 512 && id != dynamicClientID && id != CodexClientID
}

func (c *Client) chatGPTExchange(ctx context.Context, form url.Values, nonce, callbackClient string, previous Grant) (Grant, error) {
	clientID := previous.ClientID
	if clientID == "" {
		clientID = callbackClient
	}
	if nonce == "" || !validIssuedClientID(clientID) || callbackClient != "" && callbackClient != clientID {
		return Grant{}, identityMismatch()
	}
	form.Set("client_id", clientID)
	form.Set("resource", ChatGPTResource)
	tokens, now, err := c.submitTokens(ctx, strings.NewReader(form.Encode()), formContentType, false)
	if err != nil {
		return Grant{}, err
	}
	result, ok := tokens.apply(Grant{ClientID: clientID, Resource: ChatGPTResource}, now)
	if !ok || result.IDToken == "" {
		return Grant{}, invalidResponse("exchange")
	}
	claims, err := c.verifyIDToken(ctx, result.IDToken, clientID, nonce)
	if err != nil {
		return Grant{}, err
	}
	if previous.Subject != "" && claims.Subject != previous.Subject {
		return Grant{}, identityMismatch()
	}
	setProfile(&result, claims)
	if c.PlanGranted(result) || slices.Contains(result.Scopes, "offline_access") {
		if result.AccessToken == "" || result.RefreshToken == "" || tokens.ExpiresIn == 0 || !strings.EqualFold(tokens.TokenType, "bearer") {
			return Grant{}, invalidResponse("exchange")
		}
		if strings.Count(result.AccessToken, ".") == 2 && !c.verifyChatGPTAccess(&result) {
			return Grant{}, identityMismatch()
		}
	}
	if result.ExpiresAt.IsZero() {
		result.ExpiresAt = time.Unix(claims.Expires, 0).UTC()
	}
	return result, nil
}

func (c *Client) chatGPTRefresh(ctx context.Context, previous Grant) (Grant, error) {
	if !validIssuedClientID(previous.ClientID) || previous.Resource != ChatGPTResource {
		return Grant{}, &UpstreamError{StatusCode: http.StatusConflict, Code: "invalid_client", Stage: "refresh"}
	}
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {previous.RefreshToken},
		"client_id":     {previous.ClientID},
		"resource":      {previous.Resource},
	}
	tokens, now, err := c.submitTokens(ctx, strings.NewReader(form.Encode()), formContentType, true)
	if err != nil {
		return Grant{}, err
	}
	result, ok := tokens.apply(previous, now)
	if !ok || tokens.AccessToken == nil || tokens.RefreshToken == nil ||
		result.RefreshToken == previous.RefreshToken || tokens.ExpiresIn == 0 ||
		!strings.EqualFold(tokens.TokenType, "bearer") {
		return Grant{}, refreshUnknown()
	}
	if strings.Count(result.AccessToken, ".") == 2 && !c.verifyChatGPTAccess(&result) ||
		c.PlanGranted(previous) != c.PlanGranted(result) ||
		slices.Contains(previous.Scopes, "resource.invoke") != slices.Contains(result.Scopes, "resource.invoke") ||
		slices.Contains(previous.Scopes, "offline_access") != slices.Contains(result.Scopes, "offline_access") {
		return Grant{}, renewalRequired()
	}
	if tokens.IDToken != nil {
		claims, err := c.verifyIDToken(ctx, result.IDToken, previous.ClientID, "")
		if err != nil {
			return Grant{}, refreshUnknown()
		}
		if claims.Subject != previous.Subject {
			return Grant{}, renewalRequired()
		}
		setProfile(&result, claims)
	} else if strings.Count(result.AccessToken, ".") != 2 {
		return Grant{}, refreshUnknown()
	}
	return result, nil
}

func (c *Client) verifyChatGPTAccess(grant *Grant) bool {
	claims, ok := c.readAccessTokenClaims(grant.AccessToken)
	if !ok || claims.Subject != grant.Subject || claims.ClientID != grant.ClientID || !validAudience(claims.Audience, grant.Resource, "") {
		return false
	}
	setAccessExpiry(grant, claims.Expires, true)
	return true
}

func (c *Client) chatGPTRevoke(ctx context.Context, grant Grant) error {
	if grant.RefreshToken == "" {
		return nil
	}
	if !validIssuedClientID(grant.ClientID) {
		return &UpstreamError{StatusCode: http.StatusConflict, Code: "invalid_client", Stage: "revoke"}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.config.DiscoveryURL, nil)
	if err != nil {
		return invalidResponse("revoke")
	}
	res, err := c.oauth.Do(req)
	if err != nil {
		return transportError(ctx, "revoke")
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return responseError(res, "revoke", false)
	}
	var discovery struct {
		Issuer             string `json:"issuer"`
		RevocationEndpoint string `json:"revocation_endpoint"`
	}
	if readJSON(res.Body, &discovery) != nil || discovery.Issuer != c.config.Issuer ||
		!validEndpoint(discovery.RevocationEndpoint) || !sameOrigin(c.config.Issuer, discovery.RevocationEndpoint) {
		return invalidResponse("revoke")
	}
	form := url.Values{
		"token":           {grant.RefreshToken},
		"token_type_hint": {"refresh_token"},
		"client_id":       {grant.ClientID},
	}
	return c.revoke(ctx, discovery.RevocationEndpoint, strings.NewReader(form.Encode()), formContentType)
}

func sameOrigin(a, b string) bool {
	left, _ := url.Parse(a)
	right, _ := url.Parse(b)
	return left.Scheme == right.Scheme && strings.EqualFold(left.Host, right.Host)
}
