package remote

import (
	"crypto/ed25519"
	"crypto/rand"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

func TestPublicKeyValidation(t *testing.T) {
	pub, _, e := ed25519.GenerateKey(rand.Reader)
	if e != nil {
		t.Fatal(e)
	}
	key, e := ssh.NewPublicKey(pub)
	if e != nil {
		t.Fatal(e)
	}
	line := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key)))
	parsed, e := Keys([]string{line + " laptop", line + " other comment"})
	if e != nil || len(parsed) != 1 || parsed[0] != line {
		t.Fatalf("normalize: %v, %v", parsed, e)
	}
	for _, bad := range []string{"-----BEGIN OPENSSH PRIVATE KEY-----", "command=\"false\" " + line, line + "\n" + line, "ssh-rsa" + strings.TrimPrefix(line, "ssh-ed25519")} {
		if _, e = Keys([]string{bad}); e == nil {
			t.Fatalf("accepted an invalid public-key record")
		}
	}
}

func TestBindRequiresPrivateInterface(t *testing.T) {
	for _, bind := range []string{"127.0.0.1", "10.3.4.5", "100.64.5.6"} {
		if _, e := NewConfig(bind, "2222", "cloudex-test-network"); e != nil {
			t.Fatalf("%s: %v", bind, e)
		}
	}
	for _, bind := range []string{"0.0.0.0", "::", "8.8.8.8", "localhost"} {
		if _, e := NewConfig(bind, "2222", "cloudex-test-network"); e == nil {
			t.Fatalf("allowed %s", bind)
		}
	}
}

func TestValidHost(t *testing.T) {
	if ValidHost("host\nProxyCommand") {
		t.Fatal("allowed a malformed SSH hostname")
	}
	if !ValidHost("lab--dev.example.org.") {
		t.Fatal("rejected an absolute DNS name")
	}
}
