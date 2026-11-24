package agentauth

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/coachpo/cloudex/apps/api/internal/store"
)

func TestEnrollmentConsumesTokenAndRenewalKeepsHostIdentity(t *testing.T) {
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
	key, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	authority, err := Open(ctx, db, instance, key)
	if err != nil {
		t.Fatal(err)
	}
	config, err := authority.TLSConfig()
	if err != nil {
		t.Fatal(err)
	}
	disconnected := make(chan string, 2)
	server := httptest.NewUnstartedServer(authority.Enrollment(false, func(host string) {
		select {
		case disconnected <- host:
		default:
			t.Error("unexpected connection invalidation")
		}
	}))
	server.TLS = config
	server.StartTLS()
	defer server.Close()

	token := "enrollment-token-test-fixture"
	host, err := db.AddHost(ctx, "remote fixture", TokenHash(token), time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	ca := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(ca, []byte(authority.CertificatePEM()), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := Client{Address: strings.TrimPrefix(server.URL, "https://"), CAFile: ca, Token: token, StateDir: t.TempDir()}
	client, err := cfg.Bootstrap(ctx)
	if err != nil || client.HostID != host.ID || client.Instance != instance {
		t.Fatalf("initial enrollment: host=%q instance=%q err=%v", client.HostID, client.Instance, err)
	}
	if info, err := os.Stat(cfg.identityPath()); err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("Agent identity permissions: %v, %v", info, err)
	}
	if err := db.AuthenticateHost(ctx, host.ID, leafFingerprint(t, client)); err != nil {
		t.Fatal(err)
	}
	replayed := cfg
	replayed.StateDir = t.TempDir()
	if _, err := replayed.Bootstrap(ctx); err == nil || !strings.Contains(err.Error(), "HTTP 403") {
		t.Fatalf("used token accepted: %v", err)
	}
	renewed, err := cfg.Renew(ctx, client)
	if err != nil || renewed.HostID != host.ID || string(renewed.Certificate) == string(client.Certificate) {
		t.Fatalf("renewal did not issue a new certificate: %v", err)
	}
	if leafFingerprint(t, renewed) != leafFingerprint(t, client) {
		t.Fatal("renewal changed the registered public key")
	}
	replacement := "different-enrollment-token-test-fixture"
	if _, err := db.RenewEnrollment(ctx, host.ID, TokenHash(replacement), time.Now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	cfg.Token = replacement
	rotated, err := cfg.Bootstrap(ctx)
	if err != nil || rotated.HostID != host.ID || leafFingerprint(t, rotated) == leafFingerprint(t, client) {
		t.Fatalf("new token did not rotate key for the original Host: %v", err)
	}
	if err := db.AuthenticateHost(ctx, host.ID, leafFingerprint(t, renewed)); !errors.Is(err, store.ErrHostUnauthorized) {
		t.Fatalf("previous key still authenticates: %v", err)
	}
	if _, err := db.SetHostScheduling(ctx, host.ID, "disabled"); err != nil {
		t.Fatal(err)
	}
	if _, err := cfg.Renew(ctx, rotated); err == nil || !strings.Contains(err.Error(), "HTTP 403") {
		t.Fatalf("disabled Host renewed certificate: %v", err)
	}
	if len(disconnected) != 2 || <-disconnected != host.ID || <-disconnected != host.ID {
		t.Fatal("incorrect connection invalidations")
	}
	reopened, err := Open(ctx, db, instance, key)
	if err != nil || reopened.CertificatePEM() != authority.CertificatePEM() {
		t.Fatalf("control restart did not restore the Agent CA: %v", err)
	}
}

func TestEnrollmentInvalidationPrecedesNextHostIdentity(t *testing.T) {
	expires := time.Now().Add(time.Minute)
	synctest.Test(t, func(t *testing.T) {
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
		key, err := cipher.NewGCM(block)
		if err != nil {
			t.Fatal(err)
		}
		authority, err := Open(ctx, db, instance, key)
		if err != nil {
			t.Fatal(err)
		}
		const initial, replacement = "initial", "replacement"
		host, err := db.AddHost(ctx, "worker", TokenHash(initial), expires)
		if err != nil {
			t.Fatal(err)
		}
		first, firstKey := newEnrollmentRequest(t, initial)
		next, nextKey := newEnrollmentRequest(t, replacement)

		firstInvalidation := make(chan struct{})
		resume := make(chan struct{})
		release := sync.OnceFunc(func() { close(resume) })
		var invalidations atomic.Int32
		enroll := authority.Enrollment(false, func(id string) {
			if id == host.ID && invalidations.Add(1) == 1 {
				close(firstInvalidation)
				<-resume
			}
		})
		responses := make(chan int, 2)
		var requests sync.WaitGroup
		defer func() {
			release()
			requests.Wait()
		}()
		submit := func(req *http.Request) {
			requests.Go(func() {
				w := httptest.NewRecorder()
				enroll.ServeHTTP(w, req)
				responses <- w.Code
			})
		}
		submit(first)
		select {
		case <-firstInvalidation:
		case status := <-responses:
			t.Fatalf("initial enrollment did not reach invalidation: HTTP %d", status)
		}
		if _, err := db.RenewEnrollment(ctx, host.ID, TokenHash(replacement), expires); err != nil {
			t.Fatal(err)
		}
		submit(next)
		synctest.Wait()
		if err := db.AuthenticateHost(ctx, host.ID, firstKey); err != nil {
			t.Fatalf("replacement was authorized before the first invalidation completed: %v", err)
		}

		release()
		for range 2 {
			if status := <-responses; status != http.StatusOK {
				t.Fatalf("enrollment returned HTTP %d", status)
			}
		}
		if err := db.AuthenticateHost(ctx, host.ID, nextKey); err != nil {
			t.Fatalf("replacement was not authorized after invalidation: %v", err)
		}
	})
}

func newEnrollmentRequest(t *testing.T, token string) (*http.Request, string) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{}, private)
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(enrollmentRequest{Token: token, CSR: csr})
	if err != nil {
		t.Fatal(err)
	}
	spki, err := x509.MarshalPKIXPublicKey(public)
	if err != nil {
		t.Fatal(err)
	}
	return httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/enroll", bytes.NewReader(body)), TokenHash(string(spki))
}

func leafFingerprint(t *testing.T, identity Identity) string {
	t.Helper()
	config, err := identity.TLSConfig()
	if err != nil {
		t.Fatal(err)
	}
	return TokenHash(string(config.Certificates[0].Leaf.RawSubjectPublicKeyInfo))
}
