package codexupstream

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"slices"
	"strings"
	"time"
)

type workspaceClaims struct {
	AccountID string `json:"chatgpt_account_id"`
	UserID    string `json:"chatgpt_user_id"`
	LegacyID  string `json:"user_id"`
	FedRAMP   *bool  `json:"chatgpt_account_is_fedramp"`
}

func (w workspaceClaims) userID() string {
	if w.UserID != "" {
		return w.UserID
	}
	return w.LegacyID
}

type tokenClaims struct {
	Subject       string          `json:"sub"`
	Issuer        string          `json:"iss"`
	Audience      json.RawMessage `json:"aud"`
	Party         string          `json:"azp"`
	ClientID      string          `json:"client_id"`
	Expires       int64           `json:"exp"`
	NotBefore     int64           `json:"nbf"`
	Nonce         string          `json:"nonce"`
	Name          string          `json:"name"`
	Email         string          `json:"email"`
	EmailVerified bool            `json:"email_verified"`
	Workspace     workspaceClaims `json:"https://api.openai.com/auth"`
}

func (c *Client) verifyIDToken(ctx context.Context, token, clientID, nonce string) (tokenClaims, error) {
	invalid := invalidResponse("identity")
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return tokenClaims{}, invalid
	}
	var header struct{ Alg, Kid string }
	var claims tokenClaims
	if !decodeJWTPart(parts[0], &header) || header.Alg != "RS256" || header.Kid == "" ||
		!decodeJWTPart(parts[1], &claims) || claims.Issuer != c.config.Issuer ||
		!validTokenTimes(claims) || !validBearer(claims.Subject) ||
		!validAudience(claims.Audience, clientID, claims.Party) || nonce != "" && claims.Nonce != nonce {
		return tokenClaims{}, invalid
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.config.JWKSURL, nil)
	if err != nil {
		return tokenClaims{}, invalid
	}
	res, err := c.oauth.Do(req)
	if err != nil {
		return tokenClaims{}, transportError(ctx, "identity")
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return tokenClaims{}, unavailable("identity")
	}
	var jwks struct {
		Keys []struct{ Kid, Kty, Alg, Use, N, E string }
	}
	if readJSON(res.Body, &jwks) != nil {
		return tokenClaims{}, invalid
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return tokenClaims{}, invalid
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	for _, key := range jwks.Keys {
		if key.Kid != header.Kid || key.Kty != "RSA" || key.Alg != "" && key.Alg != header.Alg || key.Use != "" && key.Use != "sig" {
			continue
		}
		n, nErr := base64.RawURLEncoding.DecodeString(key.N)
		e, eErr := base64.RawURLEncoding.DecodeString(key.E)
		if nErr != nil || eErr != nil || len(n) < 256 || len(n) > 1024 || len(e) == 0 || len(e) > 4 {
			continue
		}
		exponent := new(big.Int).SetBytes(e).Int64()
		if exponent < 3 || exponent > 1<<31-1 || exponent%2 == 0 {
			continue
		}
		public := &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(exponent)}
		if rsa.VerifyPKCS1v15(public, crypto.SHA256, digest[:], sig) == nil {
			return claims, nil
		}
	}
	return tokenClaims{}, invalid
}

// readAccessTokenClaims checks response consistency for a token obtained
// directly from the selected issuer. Its claims never establish a sign-in
// identity in place of the separately verified ID token.
func (c *Client) readAccessTokenClaims(token string) (tokenClaims, bool) {
	parts := strings.Split(token, ".")
	var claims tokenClaims
	if len(parts) != 3 || parts[0] == "" || parts[2] == "" ||
		!decodeJWTPart(parts[1], &claims) || claims.Issuer != c.config.Issuer ||
		!validBearer(claims.Subject) || !validTokenTimes(claims) {
		return tokenClaims{}, false
	}
	return claims, true
}

func validTokenTimes(claims tokenClaims) bool {
	now := time.Now().Unix()
	return claims.NotBefore <= now && claims.Expires > now && claims.Expires <= lastRFC3339UnixSecond
}

func decodeJWTPart(part string, value any) bool {
	raw, err := base64.RawURLEncoding.DecodeString(part)
	return err == nil && json.Unmarshal(raw, value) == nil
}

func validAudience(raw json.RawMessage, target, party string) bool {
	if party != "" && party != target {
		return false
	}
	var single string
	if json.Unmarshal(raw, &single) == nil {
		return single == target
	}
	var multiple []string
	return json.Unmarshal(raw, &multiple) == nil && slices.Contains(multiple, target) &&
		(len(multiple) == 1 || party == target)
}

func setProfile(g *Grant, claims tokenClaims) {
	g.Subject = claims.Subject
	g.DisplayName = strings.TrimSpace(claims.Name)
	g.Email = ""
	if claims.EmailVerified {
		g.Email = strings.TrimSpace(claims.Email)
	}
}

func setAccessExpiry(g *Grant, expiry int64, hasExpiryDuration bool) {
	fromToken := time.Unix(expiry, 0).UTC()
	if !hasExpiryDuration || fromToken.Before(g.ExpiresAt) {
		g.ExpiresAt = fromToken
	}
}
