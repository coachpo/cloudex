package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/moby/moby/client"
)

func TestRemoteRejectsUnownedContainerBeforeAccess(t *testing.T) {
	var accessed atomic.Int32
	d := dockerHTTPFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1.54/containers/foreign/json" {
			io.WriteString(w, `{"Id":"foreign","Name":"/another","Config":{"Labels":{}}}`)
			return
		}
		accessed.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	})
	d.remote = true
	ctx := t.Context()
	actions := map[string]func() error{
		"start":    func() error { return d.Start(ctx, "foreign") },
		"stop":     func() error { return d.Stop(ctx, "foreign") },
		"exec":     func() error { _, err := d.Run(ctx, "foreign", Exec{}); return err },
		"terminal": func() error { _, err := d.Terminal(ctx, "foreign"); return err },
		"logs":     func() error { return d.Logs(ctx, "foreign", io.Discard) },
		"metrics":  func() error { _, err := d.Metrics(ctx, "foreign"); return err },
	}
	for name, action := range actions {
		if err := action(); !errors.Is(err, ErrOwnership) {
			t.Errorf("%s returned %v", name, err)
		}
	}
	if accessed.Load() != 0 {
		t.Fatalf("unexpected container access (%d)", accessed.Load())
	}
}

func TestRemoteStartSeparatesLostReplyAndDockerRejection(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		lost   bool
	}{
		{"reply lost", 0, true},
		{"Docker failure", http.StatusInternalServerError, false},
		{"Docker rejection", http.StatusConflict, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := dockerHTTPFixture(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/v1.54/containers/owned/json":
					io.WriteString(w, `{"Id":"owned","Name":"/cloudex-fixture-inst-ws-g1","Config":{"Labels":{"cloud.host_id":"host","cloud.workspace_id":"ws","cloud.instance":"fixture-instance","cloud.managed":"true","cloud.generation":"1"}}}`)
				case "/v1.54/containers/owned/start":
					if tc.lost {
						dropDockerResponse(t, w)
					} else {
						w.WriteHeader(tc.status)
						io.WriteString(w, `{"message":"start failed"}`)
					}
				default:
					t.Errorf("unexpected path %s", r.URL.Path)
				}
			})
			d.hostID, d.remote = "host", true
			if err := d.Start(WithOperation(t.Context(), "operation", "other"), "owned"); !errors.Is(err, ErrOwnership) {
				t.Fatalf("cross-workspace start: %v", err)
			}
			err := d.Start(WithOperation(t.Context(), "operation", "ws"), "owned")
			if err == nil || errors.Is(err, ErrOwnership) || errors.Is(err, ErrHostUnavailable) != tc.lost {
				t.Fatalf("start returned %v", err)
			}
		})
	}
}

func TestEnsureSSHNetwork(t *testing.T) {
	var exists atomic.Bool
	var created atomic.Int32
	d := dockerHTTPFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1.54/networks/isolated":
			if exists.Load() {
				io.WriteString(w, `{"Name":"isolated","Scope":"local","Driver":"bridge","IPAM":{"Config":[{"Subnet":"172.20.0.0/16"}]}}`)
				return
			}
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, `{"message":"network not found"}`)
		case "/v1.54/networks/create":
			var v struct{ Name, Driver string }
			if err := json.NewDecoder(r.Body).Decode(&v); err != nil || v.Name != "isolated" || v.Driver != "bridge" {
				t.Errorf("unexpected creation: %+v, %v", v, err)
			}
			exists.Store(true)
			created.Add(1)
			w.WriteHeader(http.StatusCreated)
			io.WriteString(w, `{"Id":"new-network"}`)
		case "/v1.54/networks/overlay":
			io.WriteString(w, `{"Name":"overlay","Scope":"swarm","Driver":"overlay"}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	})
	for _, invalid := range []string{"bridge", "overlay"} {
		if err := d.EnsureSSHNetwork(context.Background(), invalid); err == nil || !strings.Contains(err.Error(), "bridge") {
			t.Errorf("network %s returned %v", invalid, err)
		}
	}
	for range 2 {
		if err := d.EnsureSSHNetwork(t.Context(), "isolated"); err != nil {
			t.Fatal(err)
		}
	}
	if created.Load() != 1 {
		t.Fatalf("created %d times", created.Load())
	}
}

func TestRemoteSessionSetupBoundsStalledDockerInspect(t *testing.T) {
	for _, tc := range []struct {
		name string
		open func(context.Context, *Docker) error
	}{
		{"terminal", func(ctx context.Context, d *Docker) error { _, err := d.Terminal(ctx, "owned"); return err }},
		{"gateway", func(ctx context.Context, d *Docker) error {
			_, err := d.DialWorkspaceSSH(ctx, "owned", netip.MustParseAddrPort("192.0.2.10:2222"), "workspaces")
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				stalledInspect := func(r *http.Request) error {
					if r.URL.Path != "/v1.54/containers/owned/json" {
						t.Fatalf("unexpected Docker request: %s", r.URL.Path)
					}
					<-r.Context().Done()
					return r.Context().Err()
				}
				d, err := newDocker("fixture-instance", client.WithAPIVersion("1.54"), client.WithHTTPRequestHook(stalledInspect))
				if err != nil {
					t.Fatal(err)
				}
				defer d.Close()
				d.remote = true
				// The caller's long-lived session still has time left after admission fails.
				ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
				defer cancel()
				err = tc.open(ctx, d)
				if !errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
					t.Fatalf("opening a stalled session: err=%v, caller context=%v", err, ctx.Err())
				}
			})
		})
	}
}
