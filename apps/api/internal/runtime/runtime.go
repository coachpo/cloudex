package runtime

import (
	"context"
	"errors"
	"io"
	"net/netip"
	"time"
)

var ErrMissing = errors.New("Docker object not found")
var ErrExecPaused = errors.New("Docker refused to create exec while the container was paused")
var ErrExecStartConflict = errors.New("Docker refused to start exec because the container was unavailable")
var ErrStartOutcomeUnknown = errors.New("Docker container start outcome is unknown")
var ErrOwnership = errors.New("Docker object is not owned by this workspace")
var ErrHostUnavailable = errors.New("Host is unavailable or the command outcome is unknown")
var ErrMetricsNotReady = errors.New("Docker has no live metrics sample for the container")

const WorkDir = "/workspace/project"

type operationKey struct{}

type operation struct {
	id, workspace string
}

// WithOperation binds commands and retries to the durable control-plane operation.
func WithOperation(ctx context.Context, operationID, workspaceID string) context.Context {
	return context.WithValue(ctx, operationKey{}, operation{operationID, workspaceID})
}

func OperationFromContext(ctx context.Context) (operationID, workspaceID string) {
	o, _ := ctx.Value(operationKey{}).(operation)
	return o.id, o.workspace
}

type Spec struct {
	WorkspaceID, Image    string
	Generation            int
	ProjectVolumeRevision int
	CPU, Memory           int64
	SSHNetwork            string
}

type Container struct {
	ID           string                `json:"id"`
	Name         string                `json:"name"`
	Image        string                `json:"image"`
	ImageID      string                `json:"image_id"`
	ImageDigests []string              `json:"image_digests"`
	State        string                `json:"state"`
	PID          int                   `json:"pid"`
	ExitCode     int                   `json:"exit_code"`
	StartedAt    string                `json:"started_at"`
	FinishedAt   string                `json:"finished_at"`
	Mounts       []Mount               `json:"mounts"`
	Networks     []string              `json:"networks"`
	EnvNames     []string              `json:"env_names"`
	Labels       map[string]string     `json:"labels"`
	NetworkIPs   map[string]netip.Addr `json:"-"`
}

type Mount struct {
	Name        string `json:"name"`
	Destination string `json:"destination"`
	Writable    bool   `json:"writable"`
}

type Host struct {
	Name    string `json:"name"`
	Engine  string `json:"engine"`
	OS      string `json:"os"`
	Arch    string `json:"arch"`
	CPUs    int    `json:"cpus"`
	Memory  int64  `json:"memory_bytes"`
	Running int    `json:"running_containers"`
	Total   int    `json:"total_containers"`
}
type HostContainer struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Image       string `json:"image"`
	State       string `json:"state"`
	WorkspaceID string `json:"workspace_id,omitempty"`
}

type Image struct {
	ID   string   `json:"id"`
	Tags []string `json:"tags"`
	Size int64    `json:"size_bytes"`
}
type Metrics struct {
	CPU    *float64 `json:"cpu_percent"`
	Memory uint64   `json:"memory_bytes"`
	Limit  uint64   `json:"memory_limit_bytes"`
}
type Exec struct {
	Args           []string
	Dir            string
	Stdout, Stderr io.Writer
}
type Terminal interface {
	io.ReadWriteCloser
	SetWriteDeadline(time.Time) error
	Resize(context.Context, uint, uint) error
}

type Engine interface {
	Ensure(context.Context, Spec) (Container, error)
	Inspect(context.Context, Spec) (Container, error)
	Start(context.Context, string) error
	Stop(context.Context, string) error
	Remove(context.Context, Spec) error
	// Purge removes the project volume and, when purgeAll is true, the home and SSH host key volumes.
	Purge(ctx context.Context, spec Spec, purgeAll bool) error
	Run(context.Context, string, Exec) (int, error)
	SSHHostKey(context.Context, string) (string, error)
	SyncAuthorizedKeys(context.Context, string, []string) error
	Terminal(context.Context, string) (Terminal, error)
	Logs(context.Context, string, io.Writer) error
	Metrics(context.Context, string) (Metrics, error)
	Host(context.Context) (Host, error)
	Containers(context.Context) ([]HostContainer, error)
	Images(context.Context) ([]Image, error)
}
