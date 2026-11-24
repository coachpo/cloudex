// Package hostssh connects the control plane to a Docker host over SSH.
package hostssh

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"time"

	"golang.org/x/crypto/ssh"
)

// Config identifies one host and its pinned OpenSSH public key.
type Config struct {
	Address, User, HostKey           string
	Password, PrivateKey, Passphrase string
}

type Client struct{ *ssh.Client }

var ErrHostKeyChanged = errors.New("SSH host key has changed")

// Probe retrieves the negotiated host key and stops before user authentication.
func Probe(ctx context.Context, address string) (ssh.PublicKey, error) {
	var hostKey ssh.PublicKey
	retrieved := errors.New("SSH host key retrieved")
	_, err := handshake(ctx, address, &ssh.ClientConfig{
		HostKeyAlgorithms: ssh.SupportedAlgorithms().HostKeys,
		HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
			hostKey = key
			return retrieved
		},
	})
	if errors.Is(err, retrieved) {
		return hostKey, nil
	}
	return nil, err
}

func Connect(ctx context.Context, cfg Config) (*Client, error) {
	pinned, _, _, rest, err := ssh.ParseAuthorizedKey([]byte(cfg.HostKey))
	if err != nil || len(bytes.TrimSpace(rest)) != 0 {
		return nil, errors.New("invalid SSH host public key")
	}
	if cfg.User == "" {
		return nil, errors.New("SSH username required")
	}
	var auth ssh.AuthMethod
	switch {
	case cfg.PrivateKey != "" && cfg.Password == "":
		var signer ssh.Signer
		if cfg.Passphrase != "" {
			signer, err = ssh.ParsePrivateKeyWithPassphrase([]byte(cfg.PrivateKey), []byte(cfg.Passphrase))
		} else {
			signer, err = ssh.ParsePrivateKey([]byte(cfg.PrivateKey))
		}
		if err != nil {
			return nil, errors.New("cannot unlock SSH private key")
		}
		auth = ssh.PublicKeys(signer)
	case cfg.Password != "" && cfg.PrivateKey == "" && cfg.Passphrase == "":
		auth = ssh.Password(cfg.Password)
	default:
		return nil, errors.New("provide an SSH password or private key")
	}
	algorithms := []string{pinned.Type()}
	switch pinned.Type() {
	case ssh.KeyAlgoRSA:
		algorithms = []string{ssh.KeyAlgoRSASHA256, ssh.KeyAlgoRSASHA512}
	case ssh.CertAlgoRSAv01:
		algorithms = []string{ssh.CertAlgoRSASHA256v01, ssh.CertAlgoRSASHA512v01}
	}
	client, err := handshake(ctx, cfg.Address, &ssh.ClientConfig{
		User: cfg.User, Auth: []ssh.AuthMethod{auth}, HostKeyAlgorithms: algorithms,
		HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
			if !bytes.Equal(pinned.Marshal(), key.Marshal()) {
				return ErrHostKeyChanged
			}
			return nil
		},
	})
	var negotiation *ssh.AlgorithmNegotiationError
	if errors.As(err, &negotiation) && negotiation.What == "host key" {
		return nil, ErrHostKeyChanged
	}
	if err != nil {
		return nil, err
	}
	return &Client{client}, nil
}

func handshake(ctx context.Context, address string, cfg *ssh.ClientConfig) (*ssh.Client, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", address)
	if err != nil {
		return nil, err
	}
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	c, channels, requests, err := ssh.NewClientConn(conn, address, cfg)
	if !stop() {
		conn.Close()
		return nil, ctx.Err()
	}
	if err != nil {
		return nil, err
	}
	return ssh.NewClient(c, channels, requests), nil
}

// DialDocker opens a Docker API stream. Pinning the socket in the fixed command
// keeps a remote CLI context from silently selecting a different host's daemon.
func (c *Client) DialDocker(ctx context.Context, _, _ string) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	type result struct {
		conn net.Conn
		err  error
	}
	done := make(chan result)
	go func() {
		conn, err := c.dockerSession(ctx)
		select {
		case done <- result{conn, err}:
		case <-ctx.Done():
			if conn != nil {
				conn.Close()
			}
		}
	}()
	select {
	case res := <-done:
		return res.conn, res.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (c *Client) dockerSession(ctx context.Context) (net.Conn, error) {
	ch, reqs, err := c.OpenChannel("session", nil)
	if err != nil {
		return nil, err
	}
	go ssh.DiscardRequests(reqs)
	go io.Copy(io.Discard, ch.Stderr())
	stop := context.AfterFunc(ctx, func() { ch.Close() })
	cmd := struct{ Command string }{"docker --host=unix:///var/run/docker.sock system dial-stdio"}
	ok, err := ch.SendRequest("exec", true, ssh.Marshal(cmd))
	if !stop() {
		return nil, ctx.Err()
	}
	if err != nil || !ok {
		ch.Close()
		if err != nil {
			return nil, err
		}
		return nil, errors.New("SSH host rejected Docker dial-stdio")
	}
	return newStreamConn(ch), nil
}
