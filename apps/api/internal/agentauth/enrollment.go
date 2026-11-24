package agentauth

import (
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"

	"github.com/coachpo/cloudex/apps/api/internal/store"
)

type enrollmentRequest struct {
	Token string `json:"token,omitempty"`
	CSR   []byte `json:"csr"`
}

type enrollmentResponse struct {
	HostID      string `json:"host_id"`
	Instance    string `json:"instance"`
	Certificate []byte `json:"certificate"`
	CA          string `json:"ca_certificate"`
}

func (a *Authority) Enrollment(local bool, disconnect func(string)) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /enroll", func(w http.ResponseWriter, r *http.Request) {
		a.enroll(w, r, local, false, disconnect)
	})
	if !local {
		mux.HandleFunc("POST /renew", func(w http.ResponseWriter, r *http.Request) {
			a.enroll(w, r, false, true, disconnect)
		})
	}
	return mux
}

func (a *Authority) enroll(w http.ResponseWriter, r *http.Request, local, renewal bool, disconnect func(string)) {
	w.Header().Set("Cache-Control", "no-store")
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192))
	dec.DisallowUnknownFields()
	var input enrollmentRequest
	if dec.Decode(&input) != nil || !errors.Is(dec.Decode(new(any)), io.EOF) {
		http.Error(w, "Invalid enrollment request", http.StatusBadRequest)
		return
	}
	csr, err := x509.ParseCertificateRequest(input.CSR)
	if err != nil || csr.CheckSignature() != nil {
		http.Error(w, "Invalid certificate signing request", http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	id := store.LocalHostID
	key := TokenHash(string(csr.RawSubjectPublicKeyInfo))
	hash := TokenHash(input.Token)
	switch {
	case renewal:
		var registered string
		id, registered, err = a.HostIdentity(ctx, r.TLS)
		if err != nil || registered != key {
			http.Error(w, "Certificate renewal denied", http.StatusForbidden)
			return
		}
	case !local:
		host, err := a.db.PendingHost(ctx, hash)
		if err != nil || input.Token == "" {
			http.Error(w, "Invalid or expired enrollment token", http.StatusForbidden)
			return
		}
		id = host.ID
	}
	// Keep each Host's key update and connection invalidation in the same
	// order. A delayed invalidation must not close a newer identity's link.
	permit, _ := a.enrollmentGates.LoadOrStore(id, make(chan struct{}, 1))
	gate := permit.(chan struct{})
	select {
	case gate <- struct{}{}:
	case <-ctx.Done():
		return
	}
	defer func() { <-gate }()
	if renewal {
		_, registered, err := a.HostIdentity(ctx, r.TLS)
		if err != nil || registered != key {
			http.Error(w, "Certificate renewal denied", http.StatusForbidden)
			return
		}
	}
	cert, expires, err := a.clientCertificate(id, csr)
	if err != nil {
		http.Error(w, "Could not issue Agent certificate", http.StatusBadRequest)
		return
	}
	switch {
	case local:
		err = a.db.SetLocalHostCertificate(ctx, key)
	case !renewal:
		err = a.db.EnrollHost(ctx, id, hash, key)
	}
	if errors.Is(err, store.ErrInvalidEnrollment) {
		http.Error(w, "Invalid or expired enrollment token", http.StatusForbidden)
		return
	}
	if err != nil {
		slog.Error("authorize Agent enrollment", "host", id, "error", err)
		http.Error(w, "Could not enroll Host Agent", http.StatusInternalServerError)
		return
	}
	a.recordIssued(id, key, expires)
	if !renewal {
		disconnect(id)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(enrollmentResponse{HostID: id, Instance: a.instance, Certificate: cert, CA: a.CertificatePEM()})
}
