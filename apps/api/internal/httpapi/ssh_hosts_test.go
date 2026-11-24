package httpapi

import (
	"net/http"
	"testing"

	"github.com/coachpo/cloudex/apps/api/internal/control"
)

// Probe runs before authentication to the remote Host. Keep it separate from
// credential submission so a UI mistake cannot send credentials to an unpinned host.
func TestSSHHostRegistrationBoundaries(t *testing.T) {
	db := newDB(t)
	a := newTestAPI(t, &control.Control{Store: db})
	a.call(http.MethodPost, "/hosts/ssh/probe", map[string]string{"address": "example:22"}, 401, nil)
	a.call(http.MethodPost, "/hosts/ssh", map[string]string{}, 401, nil)
	a.login()
	a.expectProblem("POST", "/hosts/ssh/probe", map[string]string{"address": "example:22", "password": "fixture"}, 400, "INVALID_REQUEST")
	a.expectProblem("POST", "/hosts/ssh/probe", map[string]string{"address": "example:70000"}, 400, "INVALID_REQUEST")
	a.expectProblem("POST", "/hosts/ssh", map[string]string{"name": "remote", "address": "example:22", "username": "tester", "password": "fixture"}, 400, "INVALID_REQUEST")
	hosts, err := db.Hosts(t.Context())
	if err != nil || len(hosts) != 1 {
		t.Fatalf("rejected registration changed Host list: %d, %v", len(hosts), err)
	}
}
