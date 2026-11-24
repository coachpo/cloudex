package agentlink

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
)

func TestMutationSemaphoreChangesLimit(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		semaphore := mutationSemaphore{limit: 1}
		if err := semaphore.Acquire(ctx); err != nil {
			t.Fatal(err)
		}

		acquired := make(chan error, 1)
		assertBlocked := func() {
			t.Helper()
			synctest.Wait()
			select {
			case err := <-acquired:
				t.Fatalf("an acquire completed at capacity: %v", err)
			default:
			}
		}
		assertAcquired := func() {
			t.Helper()
			synctest.Wait()
			select {
			case err := <-acquired:
				if err != nil {
					t.Fatal(err)
				}
			default:
				t.Fatal("an acquire is still blocked with capacity available")
			}
		}

		go func() { acquired <- semaphore.Acquire(ctx) }()
		assertBlocked()
		if err := semaphore.SetLimit(ctx, 2); err != nil {
			t.Fatal(err)
		}
		assertAcquired()

		if err := semaphore.SetLimit(ctx, 1); err != nil {
			t.Fatal(err)
		}
		go func() { acquired <- semaphore.Acquire(ctx) }()
		assertBlocked()
		semaphore.Release()
		assertBlocked()
		semaphore.Release()
		assertAcquired()
		semaphore.Release()
	})
}

func TestMutationSemaphoreCancelsWaitingAcquire(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		semaphore := mutationSemaphore{limit: 1}
		if err := semaphore.Acquire(t.Context()); err != nil {
			t.Fatal(err)
		}
		acquired := make(chan error, 1)
		go func() { acquired <- semaphore.Acquire(ctx) }()
		synctest.Wait()
		cancel()
		if err := <-acquired; !errors.Is(err, context.Canceled) {
			t.Fatalf("acquire error = %v, want context canceled", err)
		}
		semaphore.Release()
		if err := semaphore.Acquire(t.Context()); err != nil {
			t.Fatal(err)
		}
		semaphore.Release()
	})
}

type observedConfigureContext struct {
	context.Context
	afterCheck func()
}

func (c observedConfigureContext) Err() error {
	err := c.Context.Err()
	c.afterCheck()
	return err
}

func TestConfigureCannotOverwriteReplacement(t *testing.T) {
	a := executor{
		Agent:     Agent{HasImage: func(context.Context, string) (bool, error) { return true, nil }},
		mutations: mutationSemaphore{limit: 3},
	}
	configure := func(ctx context.Context, limit int, image string) error {
		_, err := a.execute(ctx, nil, request{Method: "configure", Config: &AgentConfig{MaxConcurrent: limit, Images: []string{image}}})
		return err
	}
	old, disconnect := context.WithCancel(t.Context())
	defer disconnect()
	var replacementApplied bool
	applyReplacement := func() {
		t.Helper()
		if err := configure(t.Context(), 5, "workspace:replacement"); err != nil {
			t.Fatal(err)
		}
		replacementApplied = true
	}
	interrupted := observedConfigureContext{Context: old, afterCheck: func() {
		disconnect()
		// Apply the replacement at the earliest possible point after the old
		// request checks its context. The update lock may still serialize it.
		if a.mutations.mu.TryLock() {
			a.mutations.mu.Unlock()
			applyReplacement()
		}
	}}
	if err := configure(interrupted, 1, "workspace:superseded"); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if !replacementApplied {
		applyReplacement()
	}
	if err := configure(old, 1, "workspace:superseded"); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled configure error = %v", err)
	}
	if got := a.mutations.Limit(); got != 5 {
		t.Fatalf("effective limit = %d after replacement set 5", got)
	}
	if len(a.images) != 1 || a.images[0] != "workspace:replacement" {
		t.Fatalf("effective images = %v after replacement set workspace:replacement", a.images)
	}
}
