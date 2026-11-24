package agentlink

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coachpo/cloudex/apps/api/internal/runtime"
)

type codexConfigRuntime struct {
	runtime.Engine
	apply func(runtime.Spec, string, runtime.CodexConfig) error
}

func (d *codexConfigRuntime) ApplyCodexConfig(_ context.Context, spec runtime.Spec, container string, config runtime.CodexConfig) error {
	return d.apply(spec, container, config)
}

func TestApplyCodexConfigReportsWriteErrors(t *testing.T) {
	hub, server, beats := testHub(t, nil)
	target := runtime.Spec{WorkspaceID: "ws", Generation: 2}
	config := runtime.CodexConfig{
		AgentsMD: "# Workspace instructions\n", ConfigTOML: runtime.CodexConfigSeed,
		AuthJSON: `{"fixture":"fake-login"}`,
	}
	var writes atomic.Int32
	writeError := errors.New("fixture archive write interrupted")
	docker := &codexConfigRuntime{apply: func(got runtime.Spec, id string, settings runtime.CodexConfig) error {
		if got != target || id != "container" || settings != config {
			return errors.New("config write did not reach the intended container")
		}
		if writes.Add(1) == 1 {
			return writeError
		}
		return nil
	}}
	startTestAgent(t, server, "host", Agent{Engine: docker, JournalPath: filepath.Join(t.TempDir(), "commands.sqlite")})
	awaitHeartbeat(t, beats, "host")
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
	defer cancel()
	eng := hub.Engine("host").(*engine)
	apply := WithOperation(ctx, "create", target.WorkspaceID)
	if err := eng.ApplyCodexConfig(apply, target, "container", config); err == nil || !strings.Contains(err.Error(), writeError.Error()) {
		t.Fatalf("expected agent write error, got %v", err)
	}
	if err := eng.ApplyCodexConfig(apply, target, "container", config); err != nil || writes.Load() != 2 {
		t.Fatalf("agent write returned %v, calls = %d", err, writes.Load())
	}
}
