package codexupstream

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	CodexDeviceAuthorizationLifetime = 15 * time.Minute
	codexDeviceVerificationURL       = openAIIssuer + "/codex/device"
	codexDeviceRedirectURI           = openAIIssuer + "/deviceauth/callback"
)

type CodexDeviceAuthorization struct {
	VerificationURL string `json:"verification_url"`
	UserCode        string `json:"user_code"`
	deviceAuthID    string
	interval        time.Duration
}

func (c *Client) RequestCodexDeviceAuthorization(ctx context.Context) (CodexDeviceAuthorization, error) {
	const stage = "device_start"
	res, err := c.postCodexDevice(ctx, "usercode", map[string]string{"client_id": CodexClientID}, stage)
	if err != nil {
		return CodexDeviceAuthorization{}, err
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return CodexDeviceAuthorization{}, responseError(res, stage, false)
	}
	var result struct {
		DeviceAuthID string  `json:"device_auth_id"`
		UserCode     string  `json:"user_code"`
		LegacyCode   string  `json:"usercode"`
		Interval     *string `json:"interval"`
	}
	if readJSON(res.Body, &result) != nil {
		return CodexDeviceAuthorization{}, invalidResponse(stage)
	}
	if result.UserCode == "" {
		result.UserCode = result.LegacyCode
	}
	if !validBearer(result.DeviceAuthID) || !validBearer(result.UserCode) {
		return CodexDeviceAuthorization{}, invalidResponse(stage)
	}
	var seconds uint64
	if result.Interval != nil {
		seconds, err = strconv.ParseUint(strings.TrimSpace(*result.Interval), 10, 64)
		if err != nil {
			return CodexDeviceAuthorization{}, invalidResponse(stage)
		}
	}
	// A missing or zero interval must not cause a tight loop on pending responses.
	interval := time.Duration(min(max(seconds, 1), uint64(CodexDeviceAuthorizationLifetime/time.Second))) * time.Second
	return CodexDeviceAuthorization{VerificationURL: codexDeviceVerificationURL, UserCode: result.UserCode,
		deviceAuthID: result.DeviceAuthID, interval: interval}, nil
}

func (c *Client) CompleteCodexDeviceAuthorization(ctx context.Context, device CodexDeviceAuthorization, previous Grant) (Grant, error) {
	request := map[string]string{"device_auth_id": device.deviceAuthID, "user_code": device.UserCode}
	for {
		result, pending, err := c.pollCodexDevice(ctx, request)
		if err != nil {
			return Grant{}, err
		}
		if !pending {
			if err := ctx.Err(); err != nil {
				return Grant{}, err
			}
			form := authorizationCodeForm(result.AuthorizationCode, result.CodeVerifier, codexDeviceRedirectURI)
			return c.codexExchange(ctx, form, "", previous)
		}
		select {
		case <-ctx.Done():
			return Grant{}, ctx.Err()
		case <-time.After(device.interval):
		}
	}
}

type codexDeviceCode struct {
	AuthorizationCode string `json:"authorization_code"`
	CodeChallenge     string `json:"code_challenge"`
	CodeVerifier      string `json:"code_verifier"`
}

func (c *Client) pollCodexDevice(ctx context.Context, fields map[string]string) (codexDeviceCode, bool, error) {
	const stage = "device_poll"
	if err := ctx.Err(); err != nil {
		return codexDeviceCode{}, false, err
	}
	if deadline, ok := ctx.Deadline(); ok && !time.Now().Before(deadline) {
		return codexDeviceCode{}, false, context.DeadlineExceeded
	}
	res, err := c.postCodexDevice(ctx, "token", fields, stage)
	if err != nil {
		return codexDeviceCode{}, false, err
	}
	defer res.Body.Close()
	// The pinned Codex protocol uses HTTP 403 and 404 while authorization is pending.
	if res.StatusCode == http.StatusForbidden || res.StatusCode == http.StatusNotFound {
		return codexDeviceCode{}, true, nil
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return codexDeviceCode{}, false, responseError(res, stage, false)
	}
	var result codexDeviceCode
	if readJSON(res.Body, &result) != nil || !validBearer(result.AuthorizationCode) ||
		!validBearer(result.CodeChallenge) || !validBearer(result.CodeVerifier) {
		return codexDeviceCode{}, false, invalidResponse(stage)
	}
	return result, false, nil
}

func (c *Client) postCodexDevice(ctx context.Context, endpoint string, fields map[string]string, stage string) (*http.Response, error) {
	// The request fields contain strings only, so JSON encoding cannot fail.
	body, _ := json.Marshal(fields)
	u := strings.TrimRight(c.config.Issuer, "/") + "/api/accounts/deviceauth/" + endpoint
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return nil, unavailable(stage)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	res, err := c.oauth.Do(req)
	if err != nil {
		return nil, transportError(ctx, stage)
	}
	return res, nil
}
