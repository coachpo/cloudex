package agentauth

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"
)

const (
	renewBefore = 7 * 24 * time.Hour
	renewRetry  = time.Minute
)

type Client struct {
	Address  string
	Socket   string
	CAFile   string
	StateDir string
	Token    string
}

type Identity struct {
	enrollmentResponse
	Seed      []byte `json:"key_seed"`
	TokenHash string `json:"enrollment_hash,omitempty"`
}

func (c Client) Bootstrap(ctx context.Context) (Identity, error) {
	var identity Identity
	b, err := os.ReadFile(c.identityPath())
	switch {
	case err == nil:
		if err := json.Unmarshal(b, &identity); err != nil {
			return Identity{}, fmt.Errorf("read Agent identity: %w", err)
		}
		if c.Socket != "" {
			config, err := identity.TLSConfig()
			if err != nil {
				return Identity{}, err
			}
			if time.Now().Before(config.Certificates[0].Leaf.NotAfter) {
				return identity, nil
			}
		} else if c.Token == "" || identity.TokenHash == TokenHash(c.Token) {
			return identity, nil
		}
	case !errors.Is(err, os.ErrNotExist):
		return Identity{}, err
	}
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return Identity{}, err
	}
	transport := &http.Transport{}
	defer transport.CloseIdleConnections()
	address := "https://" + c.Address
	if c.Socket == "" {
		if c.Token == "" {
			return Identity{}, errors.New("CLOUDEX_AGENT_ENROLLMENT_TOKEN is required for initial enrollment")
		}
		ca, err := os.ReadFile(c.CAFile)
		if err != nil {
			return Identity{}, fmt.Errorf("read the Agent CA file: %w", err)
		}
		transport.TLSClientConfig, err = clientTLS(string(ca))
		if err != nil {
			return Identity{}, err
		}
	} else {
		transport.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", c.Socket)
		}
		address = "http://unix"
	}
	result, err := requestCertificate(ctx, transport, address+"/enroll", c.Token, key)
	if err != nil {
		return Identity{}, err
	}
	identity = Identity{enrollmentResponse: result, Seed: key.Seed()}
	if c.Token != "" {
		identity.TokenHash = TokenHash(c.Token)
	}
	return identity, c.save(identity)
}

func (c Client) Renew(ctx context.Context, identity Identity) (Identity, error) {
	config, err := identity.TLSConfig()
	if err != nil {
		return identity, err
	}
	transport := &http.Transport{TLSClientConfig: config}
	defer transport.CloseIdleConnections()
	renewed, err := requestCertificate(ctx, transport, "https://"+c.Address+"/renew", "", ed25519.NewKeyFromSeed(identity.Seed))
	if err != nil {
		return identity, err
	}
	if renewed.HostID != identity.HostID || renewed.Instance != identity.Instance || renewed.CA != identity.CA {
		return identity, errors.New("unexpected identity in Agent renewal")
	}
	after := identity
	after.enrollmentResponse = renewed
	if err := c.save(after); err != nil {
		return identity, err
	}
	return after, nil
}

func (i Identity) TLSConfig() (*tls.Config, error) {
	config, err := clientTLS(i.CA)
	if err != nil {
		return nil, err
	}
	cert, err := i.clientCertificate()
	if err != nil {
		return nil, err
	}
	config.Certificates = []tls.Certificate{*cert}
	return config, nil
}

func (i Identity) clientCertificate() (*tls.Certificate, error) {
	if len(i.Seed) != ed25519.SeedSize {
		return nil, errors.New("invalid local Agent key")
	}
	leaf, err := x509.ParseCertificate(i.Certificate)
	if err != nil {
		return nil, err
	}
	return &tls.Certificate{Certificate: [][]byte{i.Certificate}, PrivateKey: ed25519.NewKeyFromSeed(i.Seed), Leaf: leaf}, nil
}

// Credential presents the Agent's current client certificate on every new TLS
// handshake. A renewed certificate takes effect for later connections without
// closing the ones already established.
type Credential struct {
	config *tls.Config
	cert   atomic.Pointer[tls.Certificate]
}

func NewCredential(identity Identity) (*Credential, error) {
	config, err := clientTLS(identity.CA)
	if err != nil {
		return nil, err
	}
	c := &Credential{config: config}
	if err := c.Update(identity); err != nil {
		return nil, err
	}
	config.GetClientCertificate = func(*tls.CertificateRequestInfo) (*tls.Certificate, error) { return c.cert.Load(), nil }
	return c, nil
}

func (c *Credential) TLSConfig() *tls.Config { return c.config }

func (c *Credential) Update(identity Identity) error {
	cert, err := identity.clientCertificate()
	if err != nil {
		return err
	}
	c.cert.Store(cert)
	return nil
}

// KeepRenewed renews the identity before its certificate expires and hands
// each new certificate to credential, until ctx ends.
func (c Client) KeepRenewed(ctx context.Context, identity Identity, credential *Credential) {
	expires := credential.cert.Load().Leaf.NotAfter
	timer := time.NewTimer(renewalDelay(expires))
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		next, err := c.Renew(ctx, identity)
		if err == nil {
			err = credential.Update(next)
		}
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			slog.Warn("Agent certificate renewal failed", "error", err)
			timer.Reset(renewRetry)
			continue
		}
		identity = next
		nextExpiry := credential.cert.Load().Leaf.NotAfter
		timer.Reset(delayAfterRenewal(expires, nextExpiry))
		expires = nextExpiry
	}
}

func delayAfterRenewal(previous, expires time.Time) time.Duration {
	delay := renewalDelay(expires)
	// Near CA expiry, new certificates can have the same expiry as the old
	// one, inside the renewal window. Retrying immediately cannot extend them.
	if !expires.After(previous) {
		return max(delay, renewRetry)
	}
	return delay
}

func renewalDelay(expires time.Time) time.Duration {
	return max(0, time.Until(expires.Add(-renewBefore)))
}

func clientTLS(ca string) (*tls.Config, error) {
	block, _ := pem.Decode([]byte(ca))
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, errors.New("invalid Agent CA file")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil || !cert.IsCA {
		return nil, errors.New("invalid Agent CA certificate")
	}
	roots := x509.NewCertPool()
	roots.AddCert(cert)
	// Agents may connect through an SSH tunnel or via a private address. The
	// pinned cluster CA and its cluster-specific server name stay the same.
	return &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: roots, ServerName: cert.Subject.CommonName}, nil
}

func requestCertificate(ctx context.Context, transport *http.Transport, url, token string, key ed25519.PrivateKey) (enrollmentResponse, error) {
	var response enrollmentResponse
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{}, key)
	if err != nil {
		return response, err
	}
	body, err := json.Marshal(enrollmentRequest{Token: token, CSR: csr})
	if err != nil {
		return response, err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return response, err
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 10 * time.Second, Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	reply, err := client.Do(req)
	if err != nil {
		return response, err
	}
	defer reply.Body.Close()
	if reply.StatusCode != http.StatusOK {
		return response, fmt.Errorf("Agent enrollment returned HTTP %d", reply.StatusCode)
	}
	err = json.NewDecoder(io.LimitReader(reply.Body, 16*1024)).Decode(&response)
	return response, err
}

func (c Client) identityPath() string {
	return filepath.Join(c.StateDir, "identity.json")
}

func (c Client) save(identity Identity) error {
	if err := os.MkdirAll(c.StateDir, 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(c.StateDir, "identity-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	writeErr := json.NewEncoder(f).Encode(identity)
	if writeErr == nil {
		writeErr = f.Sync()
	}
	if err := errors.Join(writeErr, f.Close()); err != nil {
		return err
	}
	return os.Rename(f.Name(), c.identityPath())
}
