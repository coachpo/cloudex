package runtime

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
)

func (d *Docker) checkWorkspaceBeforeStart(ctx context.Context, s Spec, id string) error {
	if id == "" {
		return ErrOwnership
	}
	target, err := d.inspectWorkspace(ctx, s)
	if err != nil {
		return err
	}
	if target.ID != id || target.Name != "/"+d.containerName(s) {
		return ErrOwnership
	}
	if !containerStopped(target.State) || target.State.Status != container.StateCreated {
		return errors.New("Docker has not confirmed a Workspace container awaiting its first start")
	}
	return nil
}

type workspaceFile struct {
	name, content string
	mode          int64
}

func (d *Docker) copyWorkspaceFiles(ctx context.Context, id, directory string, files []workspaceFile, owner int) (outErr error) {
	defer d.classifyOutcome(&outErr)
	var archive bytes.Buffer
	if err := writeWorkspaceFiles(&archive, files, owner); err != nil {
		return err
	}
	// CopyUIDGID would override the archive ownership with the container's
	// configured user, which is root for Workspace containers.
	_, err := d.cli.CopyToContainer(ctx, id, client.CopyToContainerOptions{
		DestinationPath: directory, Content: &archive,
	})
	return err
}

func writeWorkspaceFiles(dst io.Writer, files []workspaceFile, owner int) error {
	modified := time.Now()
	w := tar.NewWriter(dst)
	for _, file := range files {
		if err := w.WriteHeader(&tar.Header{
			Name: file.name, Typeflag: tar.TypeReg, Mode: file.mode, ModTime: modified,
			Size: int64(len(file.content)), Uid: owner, Gid: owner,
		}); err != nil {
			return err
		}
		if _, err := w.Write([]byte(file.content)); err != nil {
			return err
		}
	}
	return w.Close()
}
