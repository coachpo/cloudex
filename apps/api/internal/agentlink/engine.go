package agentlink

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"

	"github.com/coachpo/cloudex/apps/api/internal/runtime"
)

// Engine returns a stable runtime binding for a Host. Each call uses that
// Host's currently authenticated session or returns ErrHostUnavailable.
func (h *Hub) Engine(hostID string) runtime.Engine { return &engine{hub: h, host: hostID} }

type engine struct {
	hub     *Hub
	host    string
	session *session
}

func (e *engine) begin(ctx context.Context, req request) (*exchange, error) {
	return e.beginWithSetup(ctx, req, func(_ context.Context, _ *exchange) error { return nil })
}

func (e *engine) beginWithSetup(ctx context.Context, req request, handshake func(context.Context, *exchange) error) (*exchange, error) {
	if op, ok := ctx.Value(operationKey{}).(operation); ok {
		req.OperationID, req.WorkspaceID = op.id, op.workspace
	}
	setup, stop := context.WithTimeout(ctx, setupTimeout)
	defer stop()
	peer, err := e.hub.callAgent(ctx, setup, e.host, e.session, req)
	if err != nil {
		return nil, err
	}
	if err = handshake(setup, peer); err != nil {
		peer.cancel()
		return nil, err
	}
	return peer, nil
}

func callValue[T any](ctx context.Context, e *engine, req request) (T, error) {
	var zero T
	peer, err := e.begin(ctx, req)
	if err != nil {
		return zero, err
	}
	defer peer.cancel()
	f, err := peer.recv(ctx)
	if err != nil {
		return zero, err
	}
	return resultValue[T](f)
}

func resultValue[T any](f frame) (T, error) {
	var zero T
	if f.Kind != "result" {
		return zero, unavailable(fmt.Errorf("unexpected runtime response %q", f.Kind))
	}
	if f.Error != nil {
		return zero, f.Error
	}
	value, err := decode[T](f, "result")
	return value, unavailable(err)
}

func (e *engine) configure(ctx context.Context, config AgentConfig) error {
	_, err := callValue[struct{}](ctx, e, request{Method: "configure", Config: &config})
	return err
}

func (e *engine) Inspect(ctx context.Context, spec runtime.Spec) (runtime.Container, error) {
	c, err := callValue[wireContainer](ctx, e, request{Method: "inspect", Spec: &spec})
	return c.decode(), err
}

func (e *engine) Ensure(ctx context.Context, spec runtime.Spec) (runtime.Container, error) {
	c, err := callValue[wireContainer](ctx, e, request{Method: "ensure", Spec: &spec})
	return c.decode(), err
}

func (e *engine) Start(ctx context.Context, id string) error {
	_, err := callValue[struct{}](ctx, e, request{Method: "start", ID: id})
	return err
}

func (e *engine) WriteInitialCodexFiles(ctx context.Context, spec runtime.Spec, id string, config runtime.InitialCodexFiles) error {
	_, err := callValue[struct{}](ctx, e, request{Method: "write-initial-codex-files", Spec: &spec, ID: id, InitialCodexFiles: &config})
	return err
}

func (e *engine) ApplyGitCredential(ctx context.Context, spec runtime.Spec, id string, credential runtime.GitCredential) error {
	_, err := callValue[struct{}](ctx, e, request{Method: "apply-git-credential", Spec: &spec, ID: id, GitCredential: &credential})
	return err
}

func (e *engine) Stop(ctx context.Context, id string) error {
	_, err := callValue[struct{}](ctx, e, request{Method: "stop", ID: id})
	return err
}

func (e *engine) Remove(ctx context.Context, spec runtime.Spec) error {
	_, err := callValue[struct{}](ctx, e, request{Method: "remove", Spec: &spec})
	return err
}

func (e *engine) Purge(ctx context.Context, id string, purgeAll bool) error {
	_, err := callValue[struct{}](ctx, e, request{Method: "purge", ID: id, PurgeAll: purgeAll})
	return err
}

func (e *engine) NoWorkspaceArtifacts(ctx context.Context, spec runtime.Spec) (bool, error) {
	return callValue[bool](ctx, e, request{Method: "artifacts-absent", Spec: &spec})
}

func (e *engine) Host(ctx context.Context) (runtime.Host, error) {
	return callValue[runtime.Host](ctx, e, request{Method: "host"})
}

func (e *engine) Images(ctx context.Context) ([]runtime.Image, error) {
	return callValue[[]runtime.Image](ctx, e, request{Method: "images"})
}

func (e *engine) Containers(ctx context.Context) ([]runtime.HostContainer, error) {
	return callValue[[]runtime.HostContainer](ctx, e, request{Method: "containers"})
}

func (e *engine) Metrics(ctx context.Context, id string) (runtime.Metrics, error) {
	return callValue[runtime.Metrics](ctx, e, request{Method: "metrics", ID: id})
}

func (e *engine) SyncAuthorizedKeys(ctx context.Context, id string, keys []string) error {
	_, err := callValue[struct{}](ctx, e, request{Method: "sync-authorized-keys", ID: id, AuthorizedKeys: keys})
	return err
}

func (e *engine) SSHHostKey(ctx context.Context, id string) (string, error) {
	return callValue[string](ctx, e, request{Method: "ssh-host-key", ID: id})
}

func (e *engine) Logs(ctx context.Context, id string, w io.Writer) error {
	peer, err := e.begin(ctx, request{Method: "logs", ID: id})
	if err != nil {
		return err
	}
	defer peer.cancel()
	for {
		f, err := peer.recv(ctx)
		if err != nil {
			return err
		}
		if f.Kind != "stdout" {
			_, err = resultValue[struct{}](f)
			return err
		}
		if err := writeFrame(w, f.Data); err != nil {
			return err
		}
	}
}

func (e *engine) Run(ctx context.Context, id string, exec runtime.Exec) (int, error) {
	req := request{Method: "run", ID: id, Exec: &execRequest{
		Args: exec.Args, Dir: exec.Dir,
	}}
	peer, err := e.begin(ctx, req)
	if err != nil {
		return -1, err
	}
	defer peer.cancel()
	for {
		f, err := peer.recv(ctx)
		if err != nil {
			return -1, err
		}
		switch f.Kind {
		case "stdout", "stderr":
			w := exec.Stdout
			if f.Kind == "stderr" {
				w = exec.Stderr
			}
			if err := writeFrame(w, f.Data); err != nil {
				return -1, err
			}
		default:
			code, err := resultValue[int](f)
			if err != nil {
				return -1, err
			}
			return code, nil
		}
	}
}

func writeFrame(w io.Writer, data []byte) error {
	if w == nil {
		return nil
	}
	n, err := w.Write(data)
	if err == nil && n != len(data) {
		return io.ErrShortWrite
	}
	return err
}

func (e *engine) Terminal(ctx context.Context, id string) (runtime.Terminal, error) {
	peer, err := e.beginWithSetup(ctx, request{Method: "terminal", ID: id}, awaitReady)
	if err != nil {
		return nil, err
	}
	return newDuplex(peer), nil
}

// DialSSH opens a byte tunnel bound to a specific managed container. The
// Host Agent checks the inspected address and never dials a client-supplied
// destination. The caller owns the tunnel context for its lifetime.
func (h *Hub) DialSSH(ctx context.Context, hostID, containerID string, address netip.AddrPort) (net.Conn, error) {
	e := engine{hub: h, host: hostID}
	peer, err := e.beginWithSetup(ctx, request{Method: "dial-ssh", ID: containerID, Address: address}, awaitReady)
	if err != nil {
		return nil, err
	}
	return newDuplex(peer), nil
}

func awaitReady(ctx context.Context, peer *exchange) error {
	f, err := peer.recv(ctx)
	if err != nil {
		return err
	}
	if f.Kind == "ready" {
		return nil
	}
	_, err = resultValue[struct{}](f)
	if err != nil {
		return err
	}
	return unavailable(errors.New("agent closed connection before it was ready"))
}
