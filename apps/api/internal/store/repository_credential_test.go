package store

import (
	"errors"
	"testing"
)

func TestRepositoryCredentialRemovedWithWorkspace(t *testing.T) {
	for _, action := range []string{"cancel create", "delete workspace"} {
		t.Run(action, func(t *testing.T) {
			s, ctx := testStore(t), t.Context()
			w := Workspace{ID: ID(), Name: "private", Image: "fixture", Branch: "main", Desired: "running", CPU: 500, Memory: 512,
				RepoURL: "https://git.example.test:8443/group/repo"}
			credential := RepositoryCredential{Host: "git.example.test:8443", Username: "fixture", Token: "synthetic-git-token"}
			op, err := s.Create(ctx, w, "", "", &credential)
			if err != nil {
				t.Fatal(err)
			}
			if saved, err := s.RepositoryCredential(ctx, w.ID); err != nil || saved != credential {
				t.Fatalf("create did not store its repository credential: %v", err)
			}
			if action == "cancel create" {
				err = s.CancelOp(ctx, op.ID)
			} else {
				if err = s.FinishOp(ctx, op, "running", &Container{Generation: 1, ID: "container", Status: "running"}, ""); err != nil {
					t.Fatal(err)
				}
				op, err = s.Enqueue(ctx, w.ID, "delete", false, "", "")
				if err != nil {
					t.Fatal(err)
				}
				err = s.FinishOp(ctx, op, "deleted", nil, "")
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.RepositoryCredential(ctx, w.ID); !errors.Is(err, ErrMissing) {
				t.Fatalf("credential row remains after %s: %v", action, err)
			}
			if err := s.SetRepositoryCredential(ctx, w.ID, &credential); !errors.Is(err, ErrMissing) {
				t.Fatalf("credential write succeeded after %s: %v", action, err)
			}
		})
	}
}
