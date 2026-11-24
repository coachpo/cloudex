package agentlink

import (
	"context"
	"sync"
)

// mutationSemaphore limits concurrent Workspace mutations.
// Reducing the limit leaves acquired slots in use until their holders finish.
type mutationSemaphore struct {
	mu      sync.Mutex
	limit   int
	active  int
	changed chan struct{}
}

func (s *mutationSemaphore) Acquire(ctx context.Context) error {
	for {
		s.mu.Lock()
		if err := ctx.Err(); err != nil {
			s.mu.Unlock()
			return err
		}
		if s.active < s.limit {
			s.active++
			s.mu.Unlock()
			return nil
		}
		if s.changed == nil {
			s.changed = make(chan struct{})
		}
		changed := s.changed
		s.mu.Unlock()
		select {
		case <-changed:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (s *mutationSemaphore) Release() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.active--
	s.notifyAvailable()
}

func (s *mutationSemaphore) SetLimit(ctx context.Context, limit int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	// An interrupted exchange from a superseded control stream cannot
	// overwrite the replacement stream's limit after waiting for this lock.
	if err := ctx.Err(); err != nil {
		return err
	}
	s.limit = limit
	s.notifyAvailable()
	return nil
}

func (s *mutationSemaphore) Limit() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.limit
}

// Call with mu held so waiters cannot miss the transition to available capacity.
func (s *mutationSemaphore) notifyAvailable() {
	if s.active < s.limit && s.changed != nil {
		close(s.changed)
		s.changed = nil
	}
}
