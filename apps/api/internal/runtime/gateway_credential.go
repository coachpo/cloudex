package runtime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"strings"
	"time"

	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
)

const gatewayMount = "/run/cloudex"

// The Compose gateway is the only HTTP origin issued to local Workspaces.
const LocalGatewayAddress = "cloudex-gateway:8081"
const LocalGatewayOrigin = "http://" + LocalGatewayAddress

type GatewayInstance struct {
	ContainerID string `json:"docker_id"`
	StartedAt   string `json:"started_at"`
}

type GatewayCredentialInstall struct {
	Token           string `json:"token"`
	Origin          string `json:"origin"`
	BindingRevision int64  `json:"binding_revision"`
	GrantEpoch      int64  `json:"grant_epoch"`
	Sequence        int64  `json:"sequence"`
}

type GatewayCredentialConfirmation struct {
	Digest          string `json:"digest"`
	StartedAt       string `json:"started_at"`
	Origin          string `json:"origin"`
	BindingRevision int64  `json:"binding_revision"`
	GrantEpoch      int64  `json:"grant_epoch"`
	Sequence        int64  `json:"sequence"`
}

type GatewayCredentialRuntime interface {
	InstallGatewayCredential(context.Context, Spec, GatewayInstance, GatewayCredentialInstall) (GatewayCredentialConfirmation, error)
	InspectGatewayCredential(context.Context, Spec, GatewayInstance) (GatewayCredentialConfirmation, error)
}

type gatewayState struct {
	GatewayCredentialConfirmation
	BootMarker string `json:"boot_marker"`
}

var errGatewayHelper = errors.New("workspace gateway credential helper did not confirm the requested state")

// gatewayRuntimeError retains the transport classification while keeping
// Docker diagnostics and the attached process output out of API errors and logs.
type gatewayRuntimeError struct{ cause error }

func (gatewayRuntimeError) Error() string   { return "workspace gateway credential operation failed" }
func (e gatewayRuntimeError) Unwrap() error { return e.cause }

func (d *Docker) verifyGatewayInstance(ctx context.Context, spec Spec, instance GatewayInstance) error {
	c, err := d.inspectWorkspace(ctx, spec)
	if err != nil {
		return err
	}
	if c.ID != instance.ContainerID || c.State == nil || instance.StartedAt == "" || c.State.StartedAt != instance.StartedAt ||
		c.State.Status != container.StateRunning || c.State.Paused || c.State.Restarting || !c.State.Running {
		return ErrOwnership
	}
	if c.HostConfig == nil || c.HostConfig.Tmpfs[gatewayMount] == "" {
		return errors.New("the Workspace image requires rebuild for the gateway credential mount")
	}
	return nil
}

func (d *Docker) InspectGatewayCredential(ctx context.Context, spec Spec, instance GatewayInstance) (confirmation GatewayCredentialConfirmation, err error) {
	defer d.classifyOutcome(&err)
	state, err := d.inspectGatewayState(ctx, spec, instance)
	return state.GatewayCredentialConfirmation, err
}

func (d *Docker) inspectGatewayState(ctx context.Context, spec Spec, instance GatewayInstance) (gatewayState, error) {
	var empty gatewayState
	if err := d.verifyGatewayInstance(ctx, spec, instance); err != nil {
		return empty, err
	}
	state, err := d.gatewayHelper(ctx, instance.ContainerID, "gateway-state", nil)
	if err != nil {
		return empty, err
	}
	if state.StartedAt != "" && state.StartedAt != instance.StartedAt {
		return empty, ErrOwnership
	}
	if err := d.verifyGatewayInstance(ctx, spec, instance); err != nil {
		return empty, err
	}
	state.StartedAt = instance.StartedAt
	return state, nil
}

func (d *Docker) InstallGatewayCredential(ctx context.Context, spec Spec, instance GatewayInstance, install GatewayCredentialInstall) (confirmation GatewayCredentialConfirmation, err error) {
	defer d.classifyOutcome(&err)
	decoded, decodeErr := base64.RawURLEncoding.DecodeString(install.Token)
	origin, originErr := url.Parse(install.Origin)
	if decodeErr != nil || len(decoded) != sha256.Size || originErr != nil || (origin.Scheme != "https" && install.Origin != LocalGatewayOrigin) || origin.Hostname() == "" ||
		origin.User != nil || origin.Opaque != "" || origin.Fragment != "" || origin.ForceQuery || origin.RawQuery != "" ||
		(strings.TrimSuffix(origin.Path, "/") != "") || install.BindingRevision < 1 || install.GrantEpoch < 1 || install.Sequence < 1 {
		return confirmation, errors.New("invalid gateway credential installation")
	}
	state, err := d.inspectGatewayState(ctx, spec, instance)
	if err != nil {
		return confirmation, err
	}
	hexDigest := sha256.Sum256([]byte(install.Token))
	confirmation = GatewayCredentialConfirmation{
		Digest: hex.EncodeToString(hexDigest[:]), StartedAt: instance.StartedAt, Origin: install.Origin,
		BindingRevision: install.BindingRevision, GrantEpoch: install.GrantEpoch, Sequence: install.Sequence,
	}
	if state.Sequence == install.Sequence && state.GatewayCredentialConfirmation == confirmation {
		return confirmation, nil // An earlier acknowledgement may have been lost.
	}
	if state.Sequence >= install.Sequence {
		return GatewayCredentialConfirmation{}, ErrOwnership
	}
	payload, err := json.Marshal(struct {
		GatewayCredentialInstall
		BootMarker string `json:"boot_marker"`
		StartedAt  string `json:"started_at"`
	}{install, state.BootMarker, instance.StartedAt})
	if err != nil {
		return GatewayCredentialConfirmation{}, errGatewayHelper
	}
	result, err := d.gatewayHelper(ctx, instance.ContainerID, "gateway-install", append(payload, '\n'))
	if err != nil {
		return GatewayCredentialConfirmation{}, err
	}
	if result.BootMarker != state.BootMarker || result.GatewayCredentialConfirmation != confirmation {
		return GatewayCredentialConfirmation{}, errGatewayHelper
	}
	if err := d.verifyGatewayInstance(ctx, spec, instance); err != nil {
		return GatewayCredentialConfirmation{}, err
	}
	return confirmation, nil
}

func (d *Docker) gatewayHelper(ctx context.Context, id, command string, payload []byte) (state gatewayState, err error) {
	defer func() {
		if err != nil {
			err = gatewayRuntimeError{cause: err}
		}
	}()
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	created, err := d.cli.ExecCreate(ctx, id, client.ExecCreateOptions{
		User: "0:0", Cmd: []string{"/usr/local/bin/workspace-control", command},
		AttachStdin: len(payload) > 0, AttachStdout: true, AttachStderr: true,
	})
	if err != nil {
		return state, err
	}
	attached, err := d.cli.ExecAttach(ctx, created.ID, client.ExecAttachOptions{})
	if err != nil {
		return state, err
	}
	defer attached.Close()
	stop := context.AfterFunc(ctx, attached.Close)
	defer stop()
	// The credential travels as stdin, never in the Docker ExecCreate request
	// where commands and environment variables are recorded and inspectable.
	if len(payload) > 0 {
		if _, err := io.Copy(attached.Conn, bytes.NewReader(payload)); err != nil {
			return state, err
		}
		if err := attached.CloseWrite(); err != nil {
			return state, err
		}
	}
	var stdout bytes.Buffer
	if _, err := stdcopy.StdCopy(&stdout, io.Discard, io.LimitReader(attached.Reader, 4096)); err != nil {
		return state, err
	}
	if err := json.Unmarshal(stdout.Bytes(), &state); err != nil || len(state.BootMarker) != 32 {
		return gatewayState{}, errGatewayHelper
	}
	return state, nil
}
