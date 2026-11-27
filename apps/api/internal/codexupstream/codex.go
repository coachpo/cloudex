package codexupstream

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"slices"
	"strings"
)

const (
	codexScope   = "openid profile email offline_access api.connectors.read api.connectors.invoke"
	codexBaseURL = "https://chatgpt.com/backend-api/codex"
)

func OpenAICodexConfig() Config {
	return Config{
		Channel: ChannelCodex, Issuer: openAIIssuer,
		AuthorizationURL: openAIIssuer + "/oauth/authorize",
		TokenURL:         openAIIssuer + "/oauth/token",
		JWKSURL:          openAIIssuer + "/.well-known/jwks.json",
		RevokeURL:        openAIIssuer + "/oauth/revoke",
		AccountsURL:      "https://chatgpt.com/backend-api/wham/accounts/check",
		ModelsURL:        codexBaseURL + "/models?client_version=" + CodexClientVersion,
		ResponsesURL:     codexBaseURL + "/responses",
	}
}

func codexAuthorize(values url.Values, a Authorization) {
	values.Set("client_id", CodexClientID)
	values.Set("scope", codexScope)
	values.Set("id_token_add_organizations", "true")
	values.Set("codex_cli_simplified_flow", "true")
	values.Set("originator", originator)
	if a.Previous.AccountID != "" {
		values.Set("allowed_workspace_id", a.Previous.AccountID)
	}
}

func (c *Client) codexExchange(ctx context.Context, form url.Values, callbackClient string, previous Grant) (Grant, error) {
	if callbackClient != "" && callbackClient != CodexClientID ||
		previous.ClientID != "" && previous.ClientID != CodexClientID {
		return Grant{}, identityMismatch()
	}
	form.Set("client_id", CodexClientID)
	tokens, now, err := c.submitTokens(ctx, strings.NewReader(form.Encode()), formContentType, false)
	if err != nil {
		return Grant{}, err
	}
	result, ok := tokens.apply(Grant{ClientID: CodexClientID, Scopes: strings.Fields(codexScope)}, now)
	if !ok || result.AccessToken == "" || result.IDToken == "" || result.RefreshToken == "" {
		return Grant{}, invalidResponse("exchange")
	}
	id, err := c.verifyIDToken(ctx, result.IDToken, CodexClientID, "")
	if err != nil {
		return Grant{}, err
	}
	if !validBearer(id.Workspace.userID()) || !validBearer(id.Workspace.AccountID) {
		return Grant{}, noCodexWorkspace()
	}
	setProfile(&result, id)
	result.UserID, result.AccountID = id.Workspace.userID(), id.Workspace.AccountID
	result.FedRAMP = id.Workspace.FedRAMP != nil && *id.Workspace.FedRAMP
	access, ok := c.readAccessTokenClaims(result.AccessToken)
	if !ok {
		return Grant{}, invalidResponse("identity")
	}
	if !matchCodexIdentity(result, access) || previous.Subject != "" &&
		(previous.Subject != result.Subject || previous.UserID != result.UserID || previous.AccountID != result.AccountID) {
		return Grant{}, identityMismatch()
	}
	setAccessExpiry(&result, access.Expires, tokens.ExpiresIn > 0)
	return result, nil
}

func (c *Client) codexRefresh(ctx context.Context, previous Grant) (Grant, error) {
	if previous.ClientID != CodexClientID || !validBearer(previous.AccountID) || !validBearer(previous.UserID) {
		return Grant{}, renewalRequired()
	}
	payload, err := json.Marshal(map[string]string{
		"grant_type": "refresh_token", "client_id": CodexClientID,
		"refresh_token": previous.RefreshToken, "refresh_reason": "proactive_expiry",
	})
	if err != nil {
		return Grant{}, err
	}
	tokens, now, err := c.submitTokens(ctx, bytes.NewReader(payload), "application/json", true)
	if err != nil {
		return Grant{}, err
	}
	result, ok := tokens.apply(previous, now)
	if !ok {
		return Grant{}, refreshUnknown()
	}
	if tokens.Scope != nil && !equalScopes(result.Scopes, previous.Scopes) {
		return Grant{}, renewalRequired()
	}
	if tokens.AccessToken != nil {
		access, ok := c.readAccessTokenClaims(result.AccessToken)
		if !ok {
			return Grant{}, refreshUnknown()
		}
		if !matchCodexIdentity(previous, access) {
			return Grant{}, renewalRequired()
		}
		setAccessExpiry(&result, access.Expires, tokens.ExpiresIn > 0)
	}
	if tokens.IDToken != nil {
		id, err := c.verifyIDToken(ctx, result.IDToken, CodexClientID, "")
		if err != nil {
			return Grant{}, refreshUnknown()
		}
		if !matchCodexIdentity(previous, id) {
			return Grant{}, renewalRequired()
		}
		setProfile(&result, id)
	}
	return result, nil
}

func equalScopes(a, b []string) bool {
	left, right := slices.Clone(a), slices.Clone(b)
	slices.Sort(left)
	slices.Sort(right)
	return slices.Equal(slices.Compact(left), slices.Compact(right))
}

func matchCodexIdentity(grant Grant, claims tokenClaims) bool {
	return claims.Subject == grant.Subject && claims.Workspace.userID() == grant.UserID &&
		claims.Workspace.AccountID == grant.AccountID &&
		(claims.ClientID == "" || claims.ClientID == CodexClientID) &&
		(claims.Workspace.FedRAMP == nil || *claims.Workspace.FedRAMP == grant.FedRAMP)
}

func noCodexWorkspace() *UpstreamError {
	return &UpstreamError{Code: "codex_workspace_unverified", Stage: "identity", StatusCode: http.StatusForbidden}
}

func (c *Client) codexRevoke(ctx context.Context, grant Grant) error {
	if grant.AccessToken == "" && grant.RefreshToken == "" {
		return nil
	}
	if grant.ClientID != CodexClientID {
		return &UpstreamError{Code: "invalid_client", Stage: "revoke", StatusCode: http.StatusConflict}
	}
	payload := map[string]string{"token": grant.AccessToken, "token_type_hint": "access_token"}
	if grant.RefreshToken != "" {
		payload["token"], payload["token_type_hint"], payload["client_id"] = grant.RefreshToken, "refresh_token", CodexClientID
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return c.revoke(ctx, c.config.RevokeURL, bytes.NewReader(encoded), "application/json")
}

type codexAccount struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	Deactivated     bool   `json:"is_deactivated"`
	FedRAMP         bool   `json:"is_fedramp_compliant_workspace"`
	SessionAllowed  *bool  `json:"can_access_with_session"`
	BackendOrigin   string `json:"workspace_backend_origin"`
	RoutingOverride string `json:"account_routing_override"`
}

func (c *Client) codexRoute(ctx context.Context, grant Grant) (Route, error) {
	stage := "responses_route"
	missing := &UpstreamError{Code: "codex_route_unavailable", Stage: stage, StatusCode: http.StatusServiceUnavailable}
	if !validBearer(grant.UserID) || !validBearer(grant.AccountID) {
		return Route{}, noCodexWorkspace()
	}
	headers, err := c.RequestHeaders(grant, Route{}, false)
	if err != nil {
		return Route{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.config.AccountsURL, nil)
	if err != nil {
		return Route{}, missing
	}
	req.Header = headers
	req.Header.Set("Accept", "application/json")
	req.Header.Del("Content-Type")
	res, err := c.oauth.Do(req)
	if err != nil {
		return Route{}, transportError(ctx, stage)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return Route{}, responseError(res, stage, false)
	}

	var source struct {
		Accounts json.RawMessage `json:"accounts"`
	}
	if readJSON(res.Body, &source) != nil {
		return Route{}, invalidResponse(stage)
	}
	var accounts []codexAccount
	if json.Unmarshal(source.Accounts, &accounts) != nil {
		// Accounts responses without per-workspace routing metadata cannot
		// establish an ordinary route for this identity.
		return Route{}, missing
	}
	var selected *codexAccount
	for _, account := range accounts {
		if account.ID == grant.AccountID {
			if selected != nil {
				return Route{}, noCodexWorkspace()
			}
			selected = &account
		}
	}
	if selected == nil || selected.Deactivated || selected.SessionAllowed != nil && !*selected.SessionAllowed {
		return Route{}, noCodexWorkspace()
	}
	result := Route{
		CatalogURL: c.config.ModelsURL, ResponsesURL: c.config.ResponsesURL,
		WorkspaceName: selected.Name,
	}
	switch selected.RoutingOverride {
	case "NO_CONSTRAINT":
	case "us", "us_cr":
		result.RoutingOverride = selected.RoutingOverride
	default:
		return Route{}, missing
	}
	dedicatedRequired := grant.FedRAMP || selected.FedRAMP
	if selected.BackendOrigin == "NO_CONSTRAINT" {
		if dedicatedRequired {
			return Route{}, missing
		}
		return result, nil
	}
	origin, err := url.Parse(selected.BackendOrigin)
	if err != nil || origin.Scheme != "https" || !validEndpoint(selected.BackendOrigin) ||
		origin.Path != "" && origin.Path != "/" || origin.RawQuery != "" || origin.ForceQuery {
		return Route{}, missing
	}
	destination, _ := url.Parse(result.ResponsesURL)
	destination.Scheme, destination.Host = origin.Scheme, origin.Host
	result.ResponsesURL = destination.String()
	if dedicatedRequired && sameOrigin(c.config.ResponsesURL, result.ResponsesURL) {
		return Route{}, missing
	}
	if !sameOrigin(result.CatalogURL, result.ResponsesURL) {
		result.CatalogURL = c.config.CatalogRoutes[origin.Scheme+"://"+origin.Host]
	}
	return result, nil
}
