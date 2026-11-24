package agentauth

import (
	"crypto/aes"
	"crypto/cipher"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/coachpo/cloudex/apps/api/internal/store"
)

type probeResult struct {
	status int
	body   string
	reused bool
}

func TestRenewalScheduleWaitsWhenExpiryCannotExtend(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		limit := time.Now().Add(6 * 24 * time.Hour)
		if delay := renewalDelay(limit); delay != 0 {
			t.Fatalf("first renewal inside the window delayed by %v", delay)
		}
		if delay := delayAfterRenewal(limit, limit); delay != time.Minute {
			t.Fatalf("renewal with unchanged expiry scheduled after %v", delay)
		}
		if delay := delayAfterRenewal(limit, time.Now().Add(certificateLifetime)); delay != certificateLifetime-renewBefore {
			t.Fatalf("ordinary renewal scheduled after %v", delay)
		}
	})
}

// probe sends one request and reports whether it reused an open connection.
func probe(t *testing.T, transport *http.Transport, url string) (probeResult, error) {
	t.Helper()
	var result probeResult
	trace := &httptrace.ClientTrace{GotConn: func(info httptrace.GotConnInfo) { result.reused = info.Reused }}
	req, err := http.NewRequestWithContext(httptrace.WithClientTrace(t.Context(), trace), http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	reply, err := (&http.Client{Transport: transport, Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		return result, err
	}
	defer reply.Body.Close()
	body, err := io.ReadAll(reply.Body)
	result.status, result.body = reply.StatusCode, strings.TrimSpace(string(body))
	return result, err
}

func TestRenewedCertificateKeepsEstablishedConnectionPastExpiry(t *testing.T) {
	ctx := t.Context()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "control.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	instance, err := db.Instance(ctx)
	if err != nil {
		t.Fatal(err)
	}
	block, err := aes.NewCipher(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	authority, err := Open(ctx, db, instance, aead)
	if err != nil {
		t.Fatal(err)
	}
	authority.lifetime = 4 * time.Second
	config, err := authority.TLSConfig()
	if err != nil {
		t.Fatal(err)
	}
	enrollment := authority.Enrollment(false, func(string) {})
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/probe" {
			enrollment.ServeHTTP(w, r)
			return
		}
		host, _, err := authority.HostIdentity(r.Context(), r.TLS)
		if err != nil {
			http.Error(w, err.Error(), http.StatusUnauthorized)
			return
		}
		fmt.Fprint(w, host)
	}))
	server.TLS = config
	server.StartTLS()
	defer server.Close()

	ca := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(ca, []byte(authority.CertificatePEM()), 0600); err != nil {
		t.Fatal(err)
	}
	enroll := func(name string) (Client, Identity) {
		t.Helper()
		token := name + "-enrollment-token"
		if _, err := db.AddHost(ctx, name, TokenHash(token), time.Now().Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
		cfg := Client{Address: strings.TrimPrefix(server.URL, "https://"), CAFile: ca, Token: token, StateDir: t.TempDir()}
		identity, err := cfg.Bootstrap(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return cfg, identity
	}
	renewingClient, renewing := enroll("renewing")
	_, unrenewed := enroll("unrenewed")

	credential, err := NewCredential(renewing)
	if err != nil {
		t.Fatal(err)
	}
	renewingConn := &http.Transport{TLSClientConfig: credential.TLSConfig()}
	defer renewingConn.CloseIdleConnections()
	staticConfig, err := unrenewed.TLSConfig()
	if err != nil {
		t.Fatal(err)
	}
	unrenewedConn := &http.Transport{TLSClientConfig: staticConfig}
	defer unrenewedConn.CloseIdleConnections()
	url := server.URL + "/probe"
	for name, transport := range map[string]*http.Transport{"renewing": renewingConn, "unrenewed": unrenewedConn} {
		if got, err := probe(t, transport, url); err != nil || got.status != http.StatusOK || got.reused {
			t.Fatalf("%s Host could not connect: %+v %v", name, got, err)
		}
	}

	// Certificate expiry has whole-second precision: renew late enough for the new
	// certificate to outlive the old one.
	time.Sleep(1200 * time.Millisecond)
	renewed, err := renewingClient.Renew(ctx, renewing)
	if err != nil {
		t.Fatal(err)
	}
	if err := credential.Update(renewed); err != nil {
		t.Fatal(err)
	}
	expiry := staticConfig.Certificates[0].Leaf.NotAfter
	for time.Now().Before(expiry.Add(50 * time.Millisecond)) {
		time.Sleep(25 * time.Millisecond)
	}

	if got, err := probe(t, renewingConn, url); err != nil || got.status != http.StatusOK || !got.reused {
		t.Fatalf("renewal did not keep the established connection: %+v %v", got, err)
	}
	if got, err := probe(t, unrenewedConn, url); err != nil || got.status != http.StatusUnauthorized || !strings.Contains(got.body, "expired") {
		t.Fatalf("established connection outlived a certificate that was never renewed: %+v %v", got, err)
	}

	renewingConn.CloseIdleConnections()
	if got, err := probe(t, renewingConn, url); err != nil || got.status != http.StatusOK || got.reused {
		t.Fatalf("new connection did not present the renewed certificate: %+v %v", got, err)
	}
	oldConfig, err := renewing.TLSConfig()
	if err != nil {
		t.Fatal(err)
	}
	oldConn := &http.Transport{TLSClientConfig: oldConfig}
	defer oldConn.CloseIdleConnections()
	if got, err := probe(t, oldConn, url); err == nil {
		t.Fatalf("an expired certificate opened a new connection: %+v", got)
	}

	// Replacing the Agent identity still revokes a connection that holds a valid certificate.
	replacement := "replacement-enrollment-token"
	if _, err := db.RenewEnrollment(ctx, renewed.HostID, TokenHash(replacement), time.Now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	rotation := Client{Address: strings.TrimPrefix(server.URL, "https://"), CAFile: ca, Token: replacement, StateDir: t.TempDir()}
	if _, err := rotation.Bootstrap(ctx); err != nil {
		t.Fatal(err)
	}
	if got, err := probe(t, renewingConn, url); err != nil || got.status != http.StatusUnauthorized || !got.reused ||
		!strings.Contains(got.body, "no longer authorized") {
		t.Fatalf("replaced identity kept its connection: %+v %v", got, err)
	}
}
