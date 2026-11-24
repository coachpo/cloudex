package agentauth

import (
	"context"
	"crypto/cipher"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"math/big"
	"sync"
	"time"

	"github.com/coachpo/cloudex/apps/api/internal/store"
)

const certificateLifetime = 30 * 24 * time.Hour

type Authority struct {
	db          *store.Store
	instance    string
	certificate *x509.Certificate
	private     ed25519.PrivateKey
	lifetime    time.Duration

	mu              sync.Mutex
	issued          map[string]issuedCertificate
	enrollmentGates sync.Map
}

// issuedCertificate is the latest client certificate issued for a Host.
type issuedCertificate struct {
	key     string
	expires time.Time
}

func Open(ctx context.Context, db *store.Store, instance string, key cipher.AEAD) (*Authority, error) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	template := &x509.Certificate{
		SerialNumber: serial(), Subject: pkix.Name{CommonName: "cloudex-agent-" + instance[:12] + ".internal"},
		NotBefore: now.Add(-time.Minute), NotAfter: now.AddDate(10, 0, 0),
		BasicConstraintsValid: true, IsCA: true, MaxPathLenZero: true,
		KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, public, private)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, key.NonceSize())
	rand.Read(nonce)
	aad := []byte("cloudex:agent_ca:v1")
	sealed, err := db.AgentAuthority(ctx, key.Seal(nonce, nonce, append(private.Seed(), der...), aad))
	if err != nil {
		return nil, err
	}
	if len(sealed) < len(nonce) {
		return nil, errors.New("invalid Agent CA record")
	}
	clear, err := key.Open(nil, sealed[:len(nonce)], sealed[len(nonce):], aad)
	if err != nil || len(clear) <= ed25519.SeedSize {
		return nil, errors.New("cannot decrypt Agent CA with CLOUDEX_MASTER_KEY")
	}
	cert, err := x509.ParseCertificate(clear[ed25519.SeedSize:])
	if err != nil {
		return nil, err
	}
	return &Authority{db: db, instance: instance, certificate: cert, private: ed25519.NewKeyFromSeed(clear[:ed25519.SeedSize]),
		lifetime: certificateLifetime, issued: make(map[string]issuedCertificate)}, nil
}

func (a *Authority) CertificatePEM() string {
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: a.certificate.Raw}))
}

func serial() *big.Int {
	var value [16]byte
	rand.Read(value[:])
	value[0] |= 1
	return new(big.Int).SetBytes(value[:])
}

func (a *Authority) TLSConfig() (*tls.Config, error) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	template := &x509.Certificate{
		SerialNumber: serial(), DNSNames: []string{a.certificate.Subject.CommonName},
		NotBefore: time.Now().Add(-time.Minute), NotAfter: a.certificate.NotAfter,
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, a.certificate, public, a.private)
	if err != nil {
		return nil, err
	}
	roots := x509.NewCertPool()
	roots.AddCert(a.certificate)
	return &tls.Config{
		MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: private}},
		ClientAuth: tls.VerifyClientCertIfGiven, ClientCAs: roots, SessionTicketsDisabled: true,
	}, nil
}

func (a *Authority) clientCertificate(host string, csr *x509.CertificateRequest) ([]byte, time.Time, error) {
	if _, ok := csr.PublicKey.(ed25519.PublicKey); !ok {
		return nil, time.Time{}, errors.New("expected an Ed25519 Agent key")
	}
	now := time.Now()
	expires := now.Add(a.lifetime)
	if a.certificate.NotAfter.Before(expires) {
		expires = a.certificate.NotAfter
	}
	template := &x509.Certificate{
		SerialNumber: serial(), Subject: pkix.Name{CommonName: host},
		NotBefore: now.Add(-time.Minute), NotAfter: expires,
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, a.certificate, csr.PublicKey, a.private)
	if err != nil {
		return nil, time.Time{}, err
	}
	// The encoded expiry has whole-second precision, unlike template.NotAfter.
	issued, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, time.Time{}, err
	}
	return der, issued.NotAfter, nil
}

// recordIssued remembers the Host's latest certificate once it is authorized.
func (a *Authority) recordIssued(host, key string, expires time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.issued[host] = issuedCertificate{key: key, expires: expires}
}

// validUntil returns when a connection authenticated by cert stops being valid.
// A TLS connection presents its certificate only in the handshake, so an Agent
// that renews the certificate for the same key keeps its established
// connection. New handshakes still need an unexpired certificate.
func (a *Authority) validUntil(host, key string, cert *x509.Certificate) time.Time {
	a.mu.Lock()
	defer a.mu.Unlock()
	if latest, ok := a.issued[host]; ok && latest.key == key && latest.expires.After(cert.NotAfter) {
		return latest.expires
	}
	return cert.NotAfter
}

func TokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func (a *Authority) HostIdentity(ctx context.Context, state *tls.ConnectionState) (host, key string, err error) {
	if state == nil || len(state.VerifiedChains) == 0 || len(state.PeerCertificates) == 0 {
		return "", "", errors.New("Host Agent client certificate required")
	}
	cert := state.PeerCertificates[0]
	host, key = cert.Subject.CommonName, TokenHash(string(cert.RawSubjectPublicKeyInfo))
	if !time.Now().Before(a.validUntil(host, key, cert)) {
		return "", "", errors.New("Host Agent client certificate expired")
	}
	if err := a.db.AuthenticateHost(ctx, host, key); err != nil {
		return "", "", err
	}
	return host, key, nil
}
