package agentlink

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"testing"
	"testing/synctest"
	"time"
)

func TestConfigureAgentReplacesImagesOnNextHeartbeat(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 6*heartbeatInterval)
		defer cancel()
		a := executor{
			Agent: Agent{
				Inventory: func(context.Context) (Inventory, error) { return Inventory{Version: "2.5.0"}, nil },
				HasImage: func(_ context.Context, image string) (bool, error) {
					switch image {
					case "workspace:missing":
						return false, nil
					case "workspace:unreachable":
						return false, errors.New("image inspection failed")
					default:
						return true, nil
					}
				},
			},
			mutations: mutationSemaphore{limit: 3},
		}
		peer := &exchange{ctx: ctx, outgoing: make(chan outbound)}
		go func() { _ = a.sendHeartbeats(ctx, peer) }()
		assertNext := func(limit int, images []ImageStatus) {
			t.Helper()
			var f frame
			select {
			case out := <-peer.outgoing:
				f = out.frame
				out.sent <- nil
			case <-ctx.Done():
				t.Fatal("heartbeat not received")
			}
			got, err := decode[Inventory](f, "heartbeat")
			if err != nil || !slices.Equal(got.Images, images) || got.MaxConcurrent != limit || got.Error != "" {
				t.Fatalf("heartbeat = %+v, %v, want limit %d, images %v and healthy Host", got, err, limit, images)
			}
			var payload map[string]json.RawMessage
			if err := json.Unmarshal(f.Value, &payload); err != nil {
				t.Fatal(err)
			}
			if len(images) == 0 && string(payload["images"]) != "[]" {
				t.Fatalf("unexpected image fields on wire: %s", f.Value)
			}
		}
		configure := func(limit int, images []string) {
			t.Helper()
			// Use the request envelope to preserve nil versus empty across the wire.
			f, err := valueFrame("request", request{Method: "configure", Config: &AgentConfig{MaxConcurrent: limit, Images: images}})
			if err != nil {
				t.Fatal(err)
			}
			req, err := decode[request](f, "request")
			if err != nil {
				t.Fatal(err)
			}
			if _, err = a.execute(ctx, nil, req); err != nil {
				t.Fatal(err)
			}
		}

		assertNext(3, nil)
		images := []ImageStatus{{Image: "workspace:missing"}, {Image: "workspace:ready", Ready: true}}
		configure(4, []string{"workspace:missing", "workspace:ready"})
		assertNext(4, images)
		configure(5, nil)
		assertNext(5, images)
		configure(6, []string{"workspace:unreachable", "workspace:new"})
		assertNext(6, []ImageStatus{{Image: "workspace:unreachable"}, {Image: "workspace:new", Ready: true}})
		configure(7, []string{})
		assertNext(7, nil)
	})
}

func TestHeartbeatStillReportsReadyImagesWhenOtherProbesTimeOut(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		for _, slowHost := range []bool{false, true} {
			var images []string
			var want []ImageStatus
			for i := range 16 {
				image := fmt.Sprintf("workspace:%02d", i)
				images = append(images, image)
				want = append(want, ImageStatus{Image: image, Ready: i >= 8})
			}
			a := executor{
				Agent: Agent{
					Inventory: func(ctx context.Context) (Inventory, error) {
						if slowHost {
							<-ctx.Done()
							return Inventory{}, ctx.Err()
						}
						return Inventory{}, nil
					},
					HasImage: func(ctx context.Context, image string) (bool, error) {
						if image < "workspace:08" {
							<-ctx.Done()
							return false, ctx.Err()
						}
						return ctx.Err() == nil, ctx.Err()
					},
				},
				images: images,
			}
			ctx, cancel := context.WithTimeout(t.Context(), heartbeatInterval)
			got := a.heartbeatInventory(ctx)
			cancel()
			if (got.Error != "") != slowHost || !slices.Equal(got.Images, want) {
				t.Fatalf("slow Host info: %t, got %+v; want image statuses %+v", slowHost, got, want)
			}
		}
	})
}

func TestHeartbeatReportsSlowImageWithinHeartbeatDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const slow = "workspace:slow"
		images := []string{"workspace:01", slow, "workspace:02", "workspace:03", "workspace:04", "workspace:05", "workspace:06", "workspace:07", "workspace:08"}
		a := executor{
			Agent: Agent{
				Inventory: func(context.Context) (Inventory, error) { return Inventory{}, nil },
				HasImage: func(ctx context.Context, image string) (bool, error) {
					if image == slow {
						select {
						case <-time.After(3 * time.Second):
						case <-ctx.Done():
							return false, ctx.Err()
						}
					}
					return true, nil
				},
			},
			images: images,
		}
		ctx, cancel := context.WithTimeout(t.Context(), heartbeatInterval)
		defer cancel()
		got := a.heartbeatInventory(ctx)
		if !slices.Contains(got.Images, ImageStatus{Image: slow, Ready: true}) {
			t.Fatalf("heartbeat marked an image that finishes within its deadline not ready: %+v", got.Images)
		}
	})
}
