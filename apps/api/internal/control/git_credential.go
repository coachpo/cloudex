package control

import (
	"context"
	"errors"

	"github.com/coachpo/cloudex/apps/api/internal/runtime"
	"github.com/coachpo/cloudex/apps/api/internal/store"
)

func (c *Control) applyGitCredential(ctx context.Context, engine runtime.Engine, target runtime.Spec, id string) error {
	credential, err := c.Store.RepositoryCredential(ctx, target.WorkspaceID)
	if errors.Is(err, store.ErrMissing) {
		return nil
	}
	if err != nil {
		return err
	}
	writer, ok := engine.(runtime.GitCredentialWriter)
	if !ok {
		return errors.New("the Workspace runtime cannot apply Git credentials")
	}
	if err := writer.ApplyGitCredential(ctx, target, id, runtime.GitCredential(credential)); err != nil {
		return runtime.GitCredentialWriteError{Err: err}
	}
	return nil
}
