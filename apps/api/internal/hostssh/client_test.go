package hostssh

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"io"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func signer(t *testing.T) ssh.Signer {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	s, err := ssh.NewSignerFromKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func testHost(t *testing.T, config *ssh.ServerConfig, accept func(ssh.NewChannel)) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var serving sync.WaitGroup
	serving.Go(func() {
		for {
			raw, err := l.Accept()
			if err != nil {
				return
			}
			serving.Go(func() {
				defer raw.Close()
				server, channels, requests, err := ssh.NewServerConn(raw, config)
				if err != nil {
					return
				}
				defer server.Close()
				go ssh.DiscardRequests(requests)
				for ch := range channels {
					serving.Go(func() { accept(ch) })
				}
			})
		}
	})
	t.Cleanup(func() { l.Close(); serving.Wait() })
	return l.Addr().String()
}

func TestPinBeforeSendingPassword(t *testing.T) {
	host := signer(t)
	var authCalls atomic.Int32
	config := &ssh.ServerConfig{PasswordCallback: func(ssh.ConnMetadata, []byte) (*ssh.Permissions, error) {
		authCalls.Add(1)
		return nil, nil
	}}
	config.AddHostKey(host)
	address := testHost(t, config, nil)
	key, err := Probe(t.Context(), address)
	if err != nil || !bytes.Equal(key.Marshal(), host.PublicKey().Marshal()) {
		t.Fatalf("probe returned incorrect key: %v", err)
	}
	cfg := Config{Address: address, User: "operator", Password: "test", HostKey: string(ssh.MarshalAuthorizedKey(signer(t).PublicKey()))}
	if _, err := Connect(t.Context(), cfg); !errors.Is(err, ErrHostKeyChanged) {
		t.Fatalf("changed key: %v", err)
	}
	if authCalls.Load() != 0 {
		t.Fatal("credentials sent before host key confirmation")
	}
	cfg.HostKey = string(ssh.MarshalAuthorizedKey(key))
	c, err := Connect(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	if authCalls.Load() != 1 {
		t.Fatal("confirmed host did not receive credentials")
	}
}

func TestProbeSelectsAKeyUsableForAuthentication(t *testing.T) {
	// This host has migrated to Ed25519 but still advertises its old RSA/SHA-1 key.
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	rsaSigner, err := ssh.NewSignerFromKey(rsaKey)
	if err != nil {
		t.Fatal(err)
	}
	old, err := ssh.NewSignerWithAlgorithms(rsaSigner.(ssh.AlgorithmSigner), []string{ssh.KeyAlgoRSA})
	if err != nil {
		t.Fatal(err)
	}
	cfg := &ssh.ServerConfig{PasswordCallback: func(ssh.ConnMetadata, []byte) (*ssh.Permissions, error) { return nil, nil }}
	cfg.AddHostKey(old)
	cfg.AddHostKey(signer(t))
	address := testHost(t, cfg, nil)
	key, err := Probe(t.Context(), address)
	if err != nil {
		t.Fatal(err)
	}
	c, err := Connect(t.Context(), Config{Address: address, User: "operator", Password: "test", HostKey: string(ssh.MarshalAuthorizedKey(key))})
	if err != nil {
		t.Fatalf("could not connect with freshly probed key: %v", err)
	}
	c.Close()
}

func TestDockerStreamDeadlineIsolationAndHalfClose(t *testing.T) {
	config := &ssh.ServerConfig{NoClientAuth: true}
	host := signer(t)
	config.AddHostKey(host)
	unblock := make(chan struct{})
	var opened atomic.Int32
	address := testHost(t, config, func(in ssh.NewChannel) {
		ch, reqs, err := in.Accept()
		if err != nil {
			return
		}
		defer ch.Close()
		req := <-reqs
		if req == nil {
			return
		}
		req.Reply(req.Type == "exec", nil)
		if opened.Add(1) == 1 {
			<-unblock
			return
		}
		p, err := io.ReadAll(ch)
		if err == nil {
			ch.Write(p)
		}
	})
	defer close(unblock)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	c, err := Connect(ctx, Config{Address: address, User: "operator", Password: "test", HostKey: string(ssh.MarshalAuthorizedKey(host.PublicKey()))})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	stalled, err := c.DialDocker(ctx, "", "")
	if err != nil {
		t.Fatal(err)
	}
	defer stalled.Close()
	if err := stalled.SetWriteDeadline(time.Now().Add(50 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if _, err := stalled.Write(make([]byte, 8<<20)); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("stalled channel write: %v", err)
	}
	next, err := c.DialDocker(ctx, "", "")
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	next.SetDeadline(time.Now().Add(time.Second))
	payload := []byte("full duplex")
	if _, err := next.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := next.(interface{ CloseWrite() error }).CloseWrite(); err != nil {
		t.Fatal(err)
	}
	reply, err := io.ReadAll(next)
	if err != nil || !bytes.Equal(reply, payload) {
		t.Fatalf("reply after half-close = %q, %v", reply, err)
	}
}
