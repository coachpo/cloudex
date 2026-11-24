package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
)

func TestRunRejectsPausedContainerAtExecCreate(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"message":"container is paused"}`))
	}))
	defer server.Close()
	cli, err := client.New(client.WithHost(server.URL), client.WithAPIVersion("1.54"))
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()
	d := &Docker{cli: cli}
	_, err = d.Run(t.Context(), "fixture", Exec{})
	if !errors.Is(err, ErrExecPaused) || requests.Load() != 1 {
		t.Fatalf("expected ExecCreate to report pause before attach: requests=%d err=%v", requests.Load(), err)
	}
}

func TestRunDoesNotMisclassifyExitedContainerAsPaused(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"message":"container fixture is not running"}`))
	}))
	defer server.Close()
	cli, err := client.New(client.WithHost(server.URL), client.WithAPIVersion("1.54"))
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()
	_, err = (&Docker{cli: cli}).Run(t.Context(), "fixture", Exec{})
	if !errdefs.IsConflict(err) || errors.Is(err, ErrExecPaused) {
		t.Fatalf("expected a conflict other than pause, got %v", err)
	}
}

func dockerHTTPFixture(t *testing.T, handler http.HandlerFunc) *Docker {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	d, err := newDocker("fixture-instance", client.WithHost("tcp://"+server.Listener.Addr().String()), client.WithAPIVersion("1.54"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d
}

func TestMetricsDistinguishesUnavailableSamplesFromDockerErrors(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		err    error
	}{
		{name: "container removed while collecting", status: http.StatusOK, err: ErrMetricsNotReady},
		{name: "container stopped while collecting", status: http.StatusOK, body: `{"read":"0001-01-01T00:00:00Z","memory_stats":{}}`, err: ErrMetricsNotReady},
		{name: "container removed before collecting", status: http.StatusNotFound, body: `{"message":"No such container: fixture"}`, err: ErrMissing},
		{name: "incomplete response", status: http.StatusOK, body: `{"read":`, err: io.ErrUnexpectedEOF},
		{name: "live sample", status: http.StatusOK, body: `{"read":"2026-01-01T00:00:00Z","memory_stats":{"usage":1024,"limit":4096}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := dockerHTTPFixture(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1.54/containers/fixture/stats" || r.URL.Query().Get("stream") != "false" {
					t.Errorf("unexpected Docker stats request: %s", r.URL)
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			})
			got, err := d.Metrics(t.Context(), "fixture")
			switch {
			case tc.err != nil:
				if !errors.Is(err, tc.err) {
					t.Fatalf("got %v, want %v", err, tc.err)
				}
			default:
				if err != nil || got.Memory != 1024 || got.Limit != 4096 {
					t.Fatalf("unexpected live sample: %+v, %v", got, err)
				}
			}
		})
	}
}

func dropDockerResponse(t *testing.T, w http.ResponseWriter) {
	conn, _, err := http.NewResponseController(w).Hijack()
	if err != nil {
		t.Errorf("could not drop Docker response: %v", err)
		return
	}
	_ = conn.Close()
}

func TestRunDistinguishesExecAttachRejectionFromLostResponse(t *testing.T) {
	for _, tc := range []struct {
		name     string
		conflict bool
	}{{"exec start rejected", true}, {"no response from exec start", false}} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			d := dockerHTTPFixture(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/v1.54/containers/fixture/exec":
					w.WriteHeader(http.StatusCreated)
					_, _ = w.Write([]byte(`{"Id":"fixture-exec"}`))
				case "/v1.54/exec/fixture-exec/start":
					if tc.conflict {
						w.WriteHeader(http.StatusConflict)
						_, _ = w.Write([]byte(`{"message":"container fixture is paused"}`))
					} else {
						dropDockerResponse(t, w)
					}
				default:
					t.Errorf("unexpected Docker request: %s", r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			})
			_, err := d.Run(t.Context(), "fixture", Exec{Args: []string{"fixture"}})
			if err == nil || errors.Is(err, ErrExecStartConflict) != tc.conflict ||
				errdefs.IsConflict(err) != tc.conflict || calls.Load() != 2 {
				t.Fatalf("wrong start evidence: requests=%d err=%v", calls.Load(), err)
			}
		})
	}
}

func TestStartDistinguishesLostResponseFromRejection(t *testing.T) {
	for _, tc := range []struct {
		name   string
		reject bool
	}{{"response lost", false}, {"daemon rejects start", true}} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			d := dockerHTTPFixture(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.URL.Path != "/v1.54/containers/fixture/start" {
					t.Errorf("unexpected Docker request: %s", r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
					return
				}
				if !tc.reject {
					dropDockerResponse(t, w)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"message":"No such container: fixture"}`))
			})
			err := d.Start(t.Context(), "fixture")
			if err == nil || errors.Is(err, ErrStartOutcomeUnknown) == tc.reject || calls.Load() != 1 {
				t.Fatalf("wrong container start evidence: requests=%d err=%v", calls.Load(), err)
			}
		})
	}
}

func TestDockerExecAttachRejectsPauseBeforeStart(t *testing.T) {
	if os.Getenv("CLOUDEX_TEST_DOCKER") != "1" {
		t.Skip("set CLOUDEX_TEST_DOCKER=1 on a disposable Docker daemon with cloudex/workspace:local available")
	}
	ctx := t.Context()
	cli, err := client.New(client.FromEnv)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cli.Close() })
	created, err := cli.ContainerCreate(ctx, client.ContainerCreateOptions{Config: &container.Config{
		Image: "cloudex/workspace:local", Cmd: []string{"sleep", "infinity"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := cli.ContainerRemove(cleanup, created.ID, client.ContainerRemoveOptions{Force: true}); err != nil {
			t.Error(err)
		}
	})
	if _, err := cli.ContainerStart(ctx, created.ID, client.ContainerStartOptions{}); err != nil {
		t.Fatal(err)
	}
	d, err := newDocker("fixture-instance", client.FromEnv, client.WithHTTPRequestHook(func(req *http.Request) error {
		if req.Method == http.MethodPost && strings.Contains(req.URL.Path, "/exec/") && strings.HasSuffix(req.URL.Path, "/start") {
			// ExecCreate already succeeded. Pause before the actual start POST.
			pause, cancel := context.WithTimeout(req.Context(), 5*time.Second)
			defer cancel()
			_, err := cli.ContainerPause(pause, created.ID, client.ContainerPauseOptions{})
			return err
		}
		return nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	exec, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_, err = d.Run(exec, created.ID, Exec{Args: []string{"true"}})
	if !errors.Is(err, ErrExecStartConflict) || !errdefs.IsConflict(err) || errors.Is(err, ErrExecPaused) {
		t.Fatalf("Docker did not reject the new exec at attach time: %v", err)
	}
}

func TestPurgeRemovesProjectAndOptionallyOtherVolumes(t *testing.T) {
	const workspace = "workspace"
	const project = "cloudex-fixture-inst-workspace-data"
	const ssh = "cloudex-fixture-inst-workspace-ssh"
	const home = "cloudex-fixture-inst-workspace-home"
	for _, tc := range []struct {
		name     string
		purgeAll bool
		want     []string
	}{
		{"reset", false, []string{project}},
		{"purge", true, []string{project, ssh, home}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			var removed []string
			d := dockerHTTPFixture(t, func(w http.ResponseWriter, r *http.Request) {
				name, ok := strings.CutPrefix(r.URL.Path, "/v1.54/volumes/")
				if !ok || !slices.Contains(tc.want, name) {
					t.Errorf("unexpected Docker request: %s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
					return
				}
				switch r.Method {
				case http.MethodGet:
					w.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(w).Encode(map[string]any{
						"Name": name, "Labels": map[string]string{
							instanceLabel: "fixture-instance", "cloud.managed": "true", hostLabel: "", workspaceLabel: workspace,
						},
					})
				case http.MethodDelete:
					mu.Lock()
					removed = append(removed, name)
					mu.Unlock()
					w.WriteHeader(http.StatusNoContent)
				default:
					t.Errorf("unexpected request method: %s", r.Method)
					w.WriteHeader(http.StatusBadRequest)
				}
			})
			if err := d.Purge(t.Context(), workspace, tc.purgeAll); err != nil {
				t.Fatal(err)
			}
			mu.Lock()
			defer mu.Unlock()
			if !slices.Equal(removed, tc.want) {
				t.Errorf("removed %v, want %v", removed, tc.want)
			}
		})
	}
}
