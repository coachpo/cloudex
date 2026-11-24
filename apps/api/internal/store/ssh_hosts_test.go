package store

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/coachpo/cloudex/apps/api/internal/hostssh"
)

func TestSSHHostCredentialStorage(t *testing.T) {
	s, ctx := testStore(t), t.Context()
	block, err := aes.NewCipher(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	key, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	cfg := hostssh.Config{Address: "host:22", User: "tester", HostKey: "host key", PrivateKey: "test private key", Passphrase: "test passphrase"}
	h, err := s.AddSSHHost(ctx, "remote", cfg, key)
	if err != nil {
		t.Fatal(err)
	}
	if h.ConnectionType != "ssh" || h.Status != "offline" || h.SSHAddress != cfg.Address || h.SSHUser != cfg.User || h.SSHHostKey != cfg.HostKey {
		t.Fatalf("unexpected public SSH Host metadata: %+v", h)
	}
	got, err := s.SSHConfig(ctx, h.ID, key)
	if err != nil || got != cfg {
		t.Fatalf("credential round trip failed: %v", err)
	}
	var sealed []byte
	if err := s.db.QueryRowContext(ctx, "SELECT ssh_config FROM host_nodes WHERE id=?", h.ID).Scan(&sealed); err != nil {
		t.Fatal(err)
	}
	public, err := json.Marshal(h)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{cfg.PrivateKey, cfg.Passphrase} {
		if bytes.Contains(public, []byte(secret)) || bytes.Contains(sealed, []byte(secret)) {
			t.Fatal("private SSH credential visible in listing or stored record")
		}
	}
	other, err := s.AddSSHHost(ctx, "second", hostssh.Config{Address: "second:22", User: "second", HostKey: "other key", Password: "second password"}, key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, "UPDATE host_nodes SET ssh_config=? WHERE id=?", sealed, other.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SSHConfig(ctx, other.ID, key); err == nil {
		t.Fatal("accepted credential encrypted for another Host")
	}
	if _, err := s.RenewEnrollment(ctx, h.ID, "token", time.Now().Add(time.Minute)); !errors.Is(err, ErrInvalidEnrollment) {
		t.Fatal("SSH host accepted an Agent enrollment", err)
	}
	if err := s.Heartbeat(ctx, h.ID, "", hostReport(1, 1)); !errors.Is(err, ErrHostUnauthorized) {
		t.Fatal("SSH host accepted an Agent heartbeat", err)
	}
}

func TestSSHHostLeaseAndCredentialRotation(t *testing.T) {
	s, ctx := testStore(t), t.Context()
	block, _ := aes.NewCipher(make([]byte, 32))
	key, _ := cipher.NewGCM(block)
	cfg := hostssh.Config{Address: "remote:22", User: "tester", HostKey: "first key", Password: "first password"}
	h, err := s.AddSSHHost(ctx, "remote", cfg, key)
	if err != nil {
		t.Fatal(err)
	}
	report := hostReport(8000, 8*gib)
	if err := s.SSHHeartbeat(ctx, h.ID, h.SSHRevision, report); err != nil {
		t.Fatal(err)
	}
	if err := s.SSHFailure(ctx, h.ID, h.SSHRevision, "SSH unreachable"); err != nil {
		t.Fatal(err)
	}
	failed, err := s.Host(ctx, h.ID)
	if err != nil || failed.Status != "offline" || failed.ConnectionError == "" || failed.Capacity.CPU != report.CPU || len(failed.Images) != 1 {
		t.Fatalf("lost offline inventory: %+v, %v", failed, err)
	}
	other := cfg
	other.Address = "second:22"
	if _, err := s.UpdateSSHHost(ctx, h.ID, other, key); !errors.Is(err, ErrConflict) {
		t.Fatal("credential rotation changed host address", err)
	}
	cfg.HostKey, cfg.Password = "rotated key", "rotated password"
	rotated, err := s.UpdateSSHHost(ctx, h.ID, cfg, key)
	if err != nil || rotated.SSHRevision == h.SSHRevision {
		t.Fatal("credential rotation did not revoke prior revision", err)
	}
	if got, err := s.SSHConfig(ctx, h.ID, key); err != nil || got != cfg {
		t.Fatal("rotated credentials were not saved", err)
	}
	if err := s.SSHHeartbeat(ctx, h.ID, rotated.SSHRevision, report); err != nil {
		t.Fatal(err)
	}
	if err := s.SSHFailure(ctx, h.ID, h.SSHRevision, "old"); !errors.Is(err, ErrHostUnauthorized) {
		t.Fatal("superseded credentials expired current lease", err)
	}
	if err := s.SSHDegraded(ctx, h.ID, h.SSHRevision, "stale inventory"); !errors.Is(err, ErrHostUnauthorized) {
		t.Fatal("superseded inventory marked current session degraded", err)
	}
	online, err := s.Host(ctx, h.ID)
	if err != nil || online.Status != "online" || online.ConnectionError != "" {
		t.Fatalf("invalid restored SSH lease: %+v, %v", online, err)
	}
	if _, err := s.SetHostScheduling(ctx, h.ID, "disabled"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetHostScheduling(ctx, h.ID, "enabled"); err != nil {
		t.Fatal(err)
	}
	if err := s.SSHHeartbeat(ctx, h.ID, rotated.SSHRevision, report); !errors.Is(err, ErrHostUnauthorized) {
		t.Fatal("disabled connection renewed lease after reenabling host", err)
	}
}
