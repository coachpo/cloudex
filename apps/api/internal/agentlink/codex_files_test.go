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
	apply func(runtime.Spec, string, runtime.InitialCodexFiles) error
}

func (d *codexConfigRuntime) WriteInitialCodexFiles(_ context.Context, spec runtime.Spec, container string, config runtime.InitialCodexFiles) error {
	return d.apply(spec, container, config)
}

func TestWriteInitialCodexFilesReportsWriteErrors(t *testing.T) {
	hub, server, beats := testHub(t, nil)
	target := runtime.Spec{WorkspaceID: "ws", Generation: 2}
	config := runtime.InitialCodexFiles{
		AgentsMD: "# Workspace instructions\n", ConfigTOML: runtime.CodexConfigSeed,
	}
	var writes atomic.Int32
	writeError := errors.New("fixture archive write interrupted")
	docker := &codexConfigRuntime{apply: func(got runtime.Spec, id string, settings runtime.InitialCodexFiles) error {
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
	apply := runtime.WithOperation(ctx, "create", target.WorkspaceID)
	if err := eng.WriteInitialCodexFiles(apply, target, "container", config); err == nil || !strings.Contains(err.Error(), writeError.Error()) {
		t.Fatalf("expected agent write error, got %v", err)
	}
	if err := eng.WriteInitialCodexFiles(apply, target, "container", config); err != nil || writes.Load() != 2 {
		t.Fatalf("agent write returned %v, calls = %d", err, writes.Load())
	}
}
