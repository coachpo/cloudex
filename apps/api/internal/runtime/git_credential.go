package runtime

import (
	"context"
	"encoding/json"
)

type GitCredential struct {
	Host     string `json:"host"`
	Username string `json:"username"`
	Token    string `json:"token"`
}

type GitCredentialWriter interface {
	ApplyGitCredential(context.Context, Spec, string, GitCredential) error
}

// GitCredentialWriteError preserves retry classification without exposing Docker's response body.
type GitCredentialWriteError struct{ Err error }

func (GitCredentialWriteError) Error() string {
	return "could not write the managed repository credential"
}
func (e GitCredentialWriteError) Unwrap() error { return e.Err }

func (d *Docker) ApplyGitCredential(ctx context.Context, s Spec, id string, credential GitCredential) error {
	if err := d.checkWorkspaceBeforeStart(ctx, s, id); err != nil {
		return err
	}
	data, _ := json.Marshal(credential)
	if err := d.copyWorkspaceFiles(ctx, id, "/etc/cloudex", []workspaceFile{
		{"git-credential.json", string(data), 0600},
	}, 10001); err != nil {
		return GitCredentialWriteError{Err: err}
	}
	return nil
}
