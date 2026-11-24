package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/coachpo/cloudex/apps/api/internal/control"
	"github.com/coachpo/cloudex/apps/api/internal/store"
)

func TestHostDisableInvalidationFinishesBeforeResume(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		db, ctx := newDB(t), t.Context()
		const token = "host-mode-session-fixture"
		if err := db.CreateAdmin(ctx, "admin", syntheticPassword, digest(token), time.Now().Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		other, err := db.AddHost(ctx, "other host", "fixture-token-digest", time.Now().Add(time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		invalidating, allowInvalidation := make(chan struct{}), make(chan struct{})
		release := sync.OnceFunc(func() { close(allowInvalidation) })
		defer release()
		handler := (&Server{
			Control: &control.Control{Store: db},
			Origin:  testOrigin,
			DisconnectAgent: func(id string) {
				if id == store.LocalHostID {
					close(invalidating)
					<-allowInvalidation
				}
			},
		}).Handler(http.NotFoundHandler())
		changeMode := func(id, mode string) <-chan *httptest.ResponseRecorder {
			reply := make(chan *httptest.ResponseRecorder, 1)
			go func() {
				req := httptest.NewRequestWithContext(ctx, http.MethodPut, "/api/v1/hosts/"+id+"/mode",
					strings.NewReader(`{"mode":"`+mode+`"}`))
				req.Header.Set("Origin", testOrigin)
				req.AddCookie(&http.Cookie{Name: cookieName, Value: token})
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, req)
				reply <- response
			}()
			return reply
		}
		disabled := changeMode(store.LocalHostID, "disabled")
		select {
		case <-invalidating:
		case response := <-disabled:
			t.Fatalf("disable returned HTTP %d before invalidating the Agent: %s", response.Code, response.Body)
		}
		resumed := changeMode(store.LocalHostID, "enabled")
		independent := changeMode(other.ID, "cordoned")
		synctest.Wait()
		select {
		case response := <-resumed:
			t.Fatalf("resume returned HTTP %d while the disabled Agent link was still being invalidated: %s", response.Code, response.Body)
		default:
		}
		if host, err := db.Host(ctx, store.LocalHostID); err != nil || host.Scheduling != "disabled" {
			t.Fatalf("source Host no longer disabled while invalidating: %+v, %v", host, err)
		}
		select {
		case response := <-independent:
			if response.Code != http.StatusOK {
				t.Fatalf("other Host could not be cordoned: HTTP %d %s", response.Code, response.Body)
			}
		default:
			t.Fatal("another Host waited for the source Host's invalidation")
		}
		release()
		for _, reply := range []<-chan *httptest.ResponseRecorder{disabled, resumed} {
			if response := <-reply; response.Code != http.StatusOK {
				t.Fatalf("mode change returned HTTP %d: %s", response.Code, response.Body)
			}
		}
		if host, err := db.Host(ctx, store.LocalHostID); err != nil || host.Scheduling != "enabled" {
			t.Fatalf("source Host did not resume after invalidation: %+v, %v", host, err)
		}
	})
}

func TestSetHostCapacity(t *testing.T) {
	db := newDB(t)
	a := newTestAPI(t, &control.Control{Store: db})
	const path = "/hosts/local/capacity"
	const budget = 5 << 30
	body := map[string]int64{"disk_bytes": budget}
	a.call(http.MethodPut, path, body, 401, nil)
	a.login()

	t.Run("returns updated host", func(t *testing.T) {
		a := a
		a.t = t
		var h store.Host
		a.call(http.MethodPut, path, body, 200, &h)
		if h.ID != store.LocalHostID || h.Capacity.Disk != budget || h.Available.Disk != budget {
			t.Fatalf("updated host = %+v", h)
		}
	})
	t.Run("rejects negative capacity", func(t *testing.T) {
		a := a
		a.t = t
		a.expectProblem(http.MethodPut, path, map[string]int64{"disk_bytes": -1}, 400, "INVALID_DISK_BYTES")
	})
	t.Run("requires capacity", func(t *testing.T) {
		a := a
		a.t = t
		a.expectProblem(http.MethodPut, path, map[string]int64{}, 400, "INVALID_DISK_BYTES")
	})
	t.Run("missing host", func(t *testing.T) {
		a := a
		a.t = t
		a.expectProblem(http.MethodPut, "/hosts/missing/capacity", body, 404, "NOT_FOUND")
	})
	t.Run("accepts zero on disabled host", func(t *testing.T) {
		a := a
		a.t = t
		a.call(http.MethodPut, "/hosts/local/mode", map[string]string{"mode": "disabled"}, 200, nil)
		var h store.Host
		a.call(http.MethodPut, path, map[string]int64{"disk_bytes": 0}, 200, &h)
		if h.Scheduling != "disabled" || h.Capacity.Disk != 0 {
			t.Fatalf("update capacity on disabled host = %+v", h)
		}
	})
}

func TestSetHostMaxOperations(t *testing.T) {
	db := newDB(t)
	a := newTestAPI(t, &control.Control{Store: db})
	const path = "/hosts/local/max-operations"
	body := map[string]int{"max_operations": 7}
	a.call(http.MethodPut, path, body, 401, nil)
	a.login()

	t.Run("returns updated host", func(t *testing.T) {
		a := a
		a.t = t
		var before, after store.Host
		a.call(http.MethodGet, "/hosts/local", nil, 200, &before)
		a.call(http.MethodPut, path, body, 200, &after)
		if after.ID != store.LocalHostID || after.MaxOperationsTarget != 7 || after.MaxOperations != before.MaxOperations {
			t.Fatalf("updated host: %+v", after)
		}
	})
	t.Run("rejects out of range or missing limits", func(t *testing.T) {
		a := a
		a.t = t
		for _, n := range []int{-1, 0, 101} {
			a.expectProblem(http.MethodPut, path, map[string]int{"max_operations": n}, 400, "INVALID_MAX_OPERATIONS")
		}
		a.expectProblem(http.MethodPut, path, map[string]int{}, 400, "INVALID_MAX_OPERATIONS")
		a.expectProblem(http.MethodPut, path, map[string]float64{"max_operations": 1.5}, 400, "INVALID_REQUEST")
	})
	t.Run("missing host", func(t *testing.T) {
		a := a
		a.t = t
		a.expectProblem(http.MethodPut, "/hosts/missing/max-operations", body, 404, "NOT_FOUND")
	})
	t.Run("accepts cordoned and disabled Hosts", func(t *testing.T) {
		a := a
		a.t = t
		ctx := t.Context()
		if err := db.SetLocalHostCertificate(ctx, "fixture-serial"); err != nil {
			t.Fatal(err)
		}
		if err := db.Heartbeat(ctx, store.LocalHostID, "fixture-serial", store.HostReport{Healthy: true, MaxOperations: 3}); err != nil {
			t.Fatal(err)
		}
		var h store.Host
		a.call(http.MethodPut, "/hosts/local/mode", map[string]string{"mode": "cordoned"}, 200, nil)
		a.call(http.MethodPut, path, map[string]int{"max_operations": 1}, 200, &h)
		if h.Scheduling != "cordoned" || h.MaxOperationsTarget != 1 {
			t.Fatalf("update on cordoned Host: %+v", h)
		}
		a.call(http.MethodPut, "/hosts/local/mode", map[string]string{"mode": "disabled"}, 200, nil)
		a.call(http.MethodPut, path, map[string]int{"max_operations": 100}, 200, &h)
		if h.Scheduling != "disabled" || h.MaxOperationsTarget != 100 {
			t.Fatalf("update on disabled Host: %+v", h)
		}
	})
}

func TestWorkspacePlacementUsesConfiguredHostDisk(t *testing.T) {
	db := newDB(t)
	ctx := t.Context()
	const serial = "local-test-serial"
	if err := db.SetLocalHostCertificate(ctx, serial); err != nil {
		t.Fatal(err)
	}
	if err := db.Heartbeat(ctx, store.LocalHostID, serial, store.HostReport{
		Healthy: true, Images: []store.ImageStatus{{Image: store.DefaultWorkspaceImage, Ready: true}}, MaxOperations: 1,
		CPU: 4000, Memory: 8 << 30,
	}); err != nil {
		t.Fatal(err)
	}
	a := newTestAPI(t, &control.Control{Store: db})
	a.login()

	const path, requested = "/hosts/local/capacity", 2 << 30
	create := map[string]any{
		"name":      "disk workspace",
		"placement": map[string]string{"type": "manual", "host_id": store.LocalHostID},
		"resources": map[string]int64{"disk_bytes": requested},
	}
	a.call(http.MethodPut, path, map[string]int64{"disk_bytes": requested - 1}, 200, nil)
	a.expectProblem(http.MethodPost, "/workspaces", create, 409, "INSUFFICIENT_HOST_CAPACITY")
	a.call(http.MethodPut, path, map[string]int64{"disk_bytes": requested}, 200, nil)
	a.call(http.MethodPost, "/workspaces", create, 202, nil)

	var h store.Host
	a.call(http.MethodPut, path, map[string]int64{"disk_bytes": 0}, 200, &h)
	if h.Capacity.Disk != 0 || h.Reserved.Disk != requested || h.Available.Disk != 0 {
		t.Fatalf("lowering capacity affected the existing reservation: %+v", h)
	}
	create["name"] = "second disk workspace"
	a.expectProblem(http.MethodPost, "/workspaces", create, 409, "INSUFFICIENT_HOST_CAPACITY")
}
