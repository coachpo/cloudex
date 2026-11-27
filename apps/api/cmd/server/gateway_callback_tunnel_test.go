package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"net"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// callbackSSHForward exercises browser listener → authenticated SSH channel →
// control-plane loopback. Both SSH roles use ephemeral keys in memory.
func callbackSSHForward(t *testing.T, ctx context.Context, browser net.Listener, destinationPort int) *atomic.Int32 {
	t.Helper()
	generateKey := func() ssh.Signer {
		t.Helper()
		_, private, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		signer, err := ssh.NewSignerFromKey(private)
		if err != nil {
			t.Fatal(err)
		}
		return signer
	}
	hostKey, browserKey := generateKey(), generateKey()
	transport, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = transport.Close() })
	serverCfg := &ssh.ServerConfig{PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
		if !bytes.Equal(key.Marshal(), browserKey.PublicKey().Marshal()) {
			return nil, errors.New("unknown browser fixture key")
		}
		return &ssh.Permissions{}, nil
	}}
	serverCfg.AddHostKey(hostKey)
	var count atomic.Int32
	go serveCallbackTunnel(ctx, transport, serverCfg, destinationPort, &count)

	client, err := ssh.Dial("tcp", transport.Addr().String(), &ssh.ClientConfig{
		User: "fixture", Auth: []ssh.AuthMethod{ssh.PublicKeys(browserKey)},
		HostKeyCallback: ssh.FixedHostKey(hostKey.PublicKey()), Timeout: 3 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	target := net.JoinHostPort("127.0.0.1", strconv.Itoa(destinationPort))
	go func() {
		for {
			request, err := browser.Accept()
			if err != nil {
				return
			}
			channel, err := client.DialContext(ctx, "tcp", target)
			if err != nil {
				_ = request.Close()
				continue
			}
			go copyCallbackTunnel(request, channel)
		}
	}()
	return &count
}

func serveCallbackTunnel(ctx context.Context, listener net.Listener, cfg *ssh.ServerConfig, port int, count *atomic.Int32) {
	socket, err := listener.Accept()
	if err != nil {
		return
	}
	defer socket.Close()
	_ = socket.SetDeadline(time.Now().Add(3 * time.Second))
	conn, channels, requests, err := ssh.NewServerConn(socket, cfg)
	if err != nil {
		return
	}
	defer conn.Close()
	_ = socket.SetDeadline(time.Time{})
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	go ssh.DiscardRequests(requests)
	for channel := range channels {
		var address struct {
			Host       string
			Port       uint32
			OriginHost string
			OriginPort uint32
		}
		if channel.ChannelType() != "direct-tcpip" || ssh.Unmarshal(channel.ExtraData(), &address) != nil ||
			address.Host != "127.0.0.1" || address.Port != uint32(port) {
			_ = channel.Reject(ssh.Prohibited, "only the fixture callback can be forwarded")
			continue
		}
		backend, err := (&net.Dialer{Timeout: 3 * time.Second}).DialContext(ctx, "tcp", net.JoinHostPort(address.Host, strconv.Itoa(port)))
		if err != nil {
			_ = channel.Reject(ssh.ConnectionFailed, "callback server unavailable")
			continue
		}
		peer, requests, err := channel.Accept()
		if err != nil {
			_ = backend.Close()
			continue
		}
		count.Add(1)
		go ssh.DiscardRequests(requests)
		go copyCallbackTunnel(backend, peer)
	}
}

func copyCallbackTunnel(a, b io.ReadWriteCloser) {
	defer a.Close()
	defer b.Close()
	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(a, b)
		_ = a.Close()
		close(done)
	}()
	_, _ = io.Copy(b, a)
	_ = b.Close()
	<-done
}
