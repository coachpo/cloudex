package control

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/coachpo/cloudex/apps/api/internal/runtime"
	"github.com/coachpo/cloudex/apps/api/internal/store"
)

func Git(ctx context.Context, engine runtime.Engine, id string) (*store.Git, error) {
	var out capped
	code, e := engine.Run(ctx, id, runtime.Exec{Args: []string{"workspace-control", "git"}, Dir: runtime.WorkDir, Stdout: &out})
	if e != nil {
		return nil, e
	}
	if code != 0 {
		return nil, errors.New("Git snapshot unavailable")
	}
	var g store.Git
	if e = json.Unmarshal(out.Bytes(), &g); e != nil {
		return nil, e
	}
	return &g, nil
}
