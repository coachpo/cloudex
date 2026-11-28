package remote

import (
	"errors"
	"net/netip"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"golang.org/x/crypto/ssh"
)

const (
	User        = "coder"
	BackendPort = 2222
)

type Config struct {
	Bind    netip.Addr
	Port    int
	Network string
}

var hostLabel = regexp.MustCompile(`^[a-zA-Z0-9](?:[a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?$`)
var workspaceHostName = regexp.MustCompile(`^[a-zA-Z0-9](?:[a-zA-Z0-9._-]*[a-zA-Z0-9])?$`)
var workspaceID = regexp.MustCompile(`^[0-9a-f]{32}$`)
var dockerNetwork = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)
var mesh = netip.MustParsePrefix("100.64.0.0/10")

func NewConfig(bind, port, network string) (Config, error) {
	ip, e := netip.ParseAddr(bind)
	if e != nil || !ip.Is4() || !(ip.IsLoopback() || ip.IsPrivate() || mesh.Contains(ip)) {
		return Config{}, errors.New("CLOUDEX_SSH_BIND must be a loopback, LAN or mesh IPv4 address on the Docker host")
	}
	p, e := strconv.Atoi(port)
	if e != nil || p < 1 || p > 65535 {
		return Config{}, errors.New("invalid CLOUDEX_SSH_PORT (1–65535)")
	}
	if !ValidWorkspaceNetwork(network) {
		return Config{}, errors.New("CLOUDEX_SSH_NETWORK must name a dedicated Docker bridge")
	}
	return Config{Bind: ip, Port: p, Network: network}, nil
}

func ValidWorkspaceNetwork(network string) bool {
	return dockerNetwork.MatchString(network) && network != "bridge" && network != "host" && network != "none" && network != "default"
}

func ValidHost(host string) bool {
	name := strings.TrimSuffix(host, ".")
	if len(name) > 253 {
		return false
	}
	for _, label := range strings.Split(name, ".") {
		if !hostLabel.MatchString(label) {
			return false
		}
	}
	return true
}

// Keys normalizes plain public keys. Never pass authorized_keys options,
// multiple records per array element, or private material to the container.
func Keys(values []string) ([]string, error) {
	if len(values) > 32 {
		return nil, errors.New("Use at most 32 SSH public keys.")
	}
	out := make([]string, 0, len(values))
	for _, v := range values {
		line := strings.TrimSpace(v)
		fields := strings.Fields(line)
		if len(line) > 8192 || len(fields) < 2 || strings.ContainsAny(line, "\r\n\x00") {
			return nil, errors.New("Enter one OpenSSH public key per line, not a private key.")
		}
		key, _, options, rest, e := ssh.ParseAuthorizedKey([]byte(line))
		if e != nil || len(options) != 0 || len(rest) != 0 || fields[0] != key.Type() {
			return nil, errors.New("Enter plain OpenSSH public keys without authorized_keys options.")
		}
		switch key.Type() {
		case ssh.KeyAlgoED25519, ssh.KeyAlgoRSA, ssh.KeyAlgoECDSA256, ssh.KeyAlgoECDSA384, ssh.KeyAlgoECDSA521, ssh.KeyAlgoSKED25519, ssh.KeyAlgoSKECDSA256:
		default:
			return nil, errors.New("Use an Ed25519, ECDSA or RSA public key (not a certificate).")
		}
		out = append(out, strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key))))
	}
	slices.Sort(out)
	out = slices.Compact(out)
	if len(strings.Join(out, "\n")) > 60*1024 {
		return nil, errors.New("SSH public keys exceed the allowed request size.")
	}
	return out, nil
}

const workspacePrefix = "cloudex-"

func WorkspaceIdentity(id string) string { return workspacePrefix + id }

// GatewayHostSuffix turns a workspace Host alias into the Host alias of its
// gateway hop. HostAlias never ends in it, so one workspace's Host block cannot
// shadow another workspace's gateway block in the same ssh_config.
const GatewayHostSuffix = "-gateway"

func HostAlias(id, name string) string {
	alias := strings.ToLower(name)
	if workspaceHostName.MatchString(name) && !strings.HasSuffix(alias, GatewayHostSuffix) {
		return workspacePrefix + alias
	}
	return workspacePrefix + id[:8]
}

func WorkspaceID(identity string) (string, bool) {
	id, ok := strings.CutPrefix(identity, workspacePrefix)
	return id, ok && workspaceID.MatchString(id)
}
