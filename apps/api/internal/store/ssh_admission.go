package store

import (
	"context"
	"sync"
)

type workspaceGate struct {
	token chan struct{}
	users int
}

type workspaceGates struct {
	mu   sync.Mutex
	byID map[string]*workspaceGate
}

// Coordinate admission and changes for one workspace without holding the
// SQLite connection while an SSH client receives its channel confirmation.
func (g *workspaceGates) lock(ctx context.Context, id string) (func(), error) {
	g.mu.Lock()
	if g.byID == nil {
		g.byID = make(map[string]*workspaceGate)
	}
	gate := g.byID[id]
	if gate == nil {
		gate = &workspaceGate{token: make(chan struct{}, 1)}
		g.byID[id] = gate
	}
	gate.users++
	g.mu.Unlock()

	unref := func() {
		g.mu.Lock()
		defer g.mu.Unlock()
		gate.users--
		if gate.users == 0 {
			delete(g.byID, id)
		}
	}
	select {
	case gate.token <- struct{}{}:
		unlock := func() {
			<-gate.token
			unref()
		}
		if err := ctx.Err(); err != nil {
			unlock()
			return nil, err
		}
		return unlock, nil
	case <-ctx.Done():
		unref()
		return nil, ctx.Err()
	}
}

// LockSSHKeySync orders key delivery to a Workspace with the Docker Start
// that must run with the current keys.
func (s *Store) LockSSHKeySync(ctx context.Context, id string) (func(), error) {
	return s.sshKeySync.lock(ctx, id)
}
