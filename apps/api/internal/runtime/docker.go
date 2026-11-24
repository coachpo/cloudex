package runtime

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/netip"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coachpo/cloudex/apps/api/internal/remote"
	"github.com/containerd/errdefs"
	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
)

const instanceLabel = "cloud.instance"
const workspaceLabel = "cloud.workspace_id"
const hostLabel = "cloud.host_id"
const generationLabel = "cloud.generation"

var sshContainerPort = network.MustParsePort(strconv.Itoa(remote.BackendPort) + "/tcp")

type Docker struct {
	cli      *client.Client
	instance string
	hostID   string
}

func NewDocker(instance string) (*Docker, error) {
	return newDocker(instance, client.FromEnv)
}

func NewHostDocker(instance, hostID string) (*Docker, error) {
	d, err := newDocker(instance, client.WithHost("unix:///var/run/docker.sock"))
	if err == nil {
		d.hostID = hostID
	}
	return d, err
}

func newDocker(instance string, opts ...client.Opt) (*Docker, error) {
	cli, err := client.New(append(opts, client.WithHTTPResponseHook(recordDockerPostStatus))...)
	if err != nil {
		return nil, err
	}
	return &Docker{cli: cli, instance: instance}, nil
}

type dockerPostStatusKey struct{}

// The SDK discards the error response body on a failed exec upgrade. Record
// the actual status for the caller without reading the body or parsing errors.
func recordDockerPostStatus(resp *http.Response) {
	if resp.Request != nil && resp.Request.Method == http.MethodPost {
		if status, ok := resp.Request.Context().Value(dockerPostStatusKey{}).(*int); ok {
			*status = resp.StatusCode
		}
	}
}

func (d *Docker) Close() error          { return d.cli.Close() }
func (d *Docker) name(id string) string { return "cloudex-" + d.instance[:12] + "-" + id }
func (d *Docker) containerName(s Spec) string {
	return d.name(s.WorkspaceID) + "-g" + strconv.Itoa(s.Generation)
}
func (d *Docker) labels(id string) map[string]string {
	return map[string]string{instanceLabel: d.instance, workspaceLabel: id, hostLabel: d.hostID, "cloud.managed": "true"}
}
func (d *Docker) owns(labels map[string]string, id string) bool {
	host, present := labels[hostLabel]
	return labels[instanceLabel] == d.instance && labels[workspaceLabel] == id && labels["cloud.managed"] == "true" &&
		present && host == d.hostID
}

const (
	workspaceMount  = "/workspace"
	sshHostKeyMount = "/var/lib/cloudex-ssh"
)

func volumeNames(id string) [2]string {
	name := "cloudex-" + id
	return [2]string{name + "-data", name + "-ssh"}
}

// ManagedWorkspace identifies the Workspace that owns an exact Docker ID.
func (d *Docker) ManagedWorkspace(ctx context.Context, id string) (string, error) {
	c, err := d.ownedContainer(ctx, id)
	if err != nil {
		return "", err
	}
	return c.Config.Labels[workspaceLabel], nil
}

func containerStopped(s *container.State) bool {
	return s != nil && (s.Status == container.StateCreated || s.Status == container.StateExited) &&
		!s.Running && !s.Paused && !s.Restarting && !s.Dead && s.Pid == 0
}

func (d *Docker) inspectWorkspace(ctx context.Context, s Spec) (container.InspectResponse, error) {
	out, e := d.cli.ContainerInspect(ctx, d.containerName(s), client.ContainerInspectOptions{})
	if errdefs.IsNotFound(e) {
		return container.InspectResponse{}, ErrMissing
	}
	if e != nil {
		return container.InspectResponse{}, e
	}
	c := out.Container
	if c.Config == nil || !d.owns(c.Config.Labels, s.WorkspaceID) || c.Config.Labels[generationLabel] != strconv.Itoa(s.Generation) {
		return container.InspectResponse{}, ErrOwnership
	}
	return c, nil
}

func (d *Docker) Inspect(ctx context.Context, s Spec) (Container, error) {
	c, err := d.inspectWorkspace(ctx, s)
	if err != nil {
		return Container{}, err
	}
	return workspaceContainer(c), nil
}

func workspaceContainer(c container.InspectResponse) Container {
	info := Container{ID: c.ID, Name: strings.TrimPrefix(c.Name, "/"), Image: c.Config.Image, ImageID: c.Image,
		Mounts: []Mount{}, Networks: []string{}, EnvNames: []string{}, Labels: c.Config.Labels}
	if c.State != nil {
		info.State, info.PID, info.ExitCode, info.StartedAt, info.FinishedAt = string(c.State.Status), c.State.Pid, c.State.ExitCode, c.State.StartedAt, c.State.FinishedAt
	}
	for _, m := range c.Mounts {
		info.Mounts = append(info.Mounts, Mount{m.Name, m.Destination, m.RW})
	}
	for _, v := range c.Config.Env {
		n, _, _ := strings.Cut(v, "=")
		info.EnvNames = append(info.EnvNames, n)
	}
	sort.Strings(info.EnvNames)
	if c.NetworkSettings != nil {
		info.NetworkIPs = make(map[string]netip.Addr)
		for n, endpoint := range c.NetworkSettings.Networks {
			info.Networks = append(info.Networks, n)
			if endpoint != nil && endpoint.IPAddress.IsValid() {
				info.NetworkIPs[n] = endpoint.IPAddress
			}
		}
		sort.Strings(info.Networks)
	}
	return info
}

func (d *Docker) InspectDigests(ctx context.Context, c *Container) {
	image, err := d.cli.ImageInspect(ctx, c.ImageID)
	if err == nil {
		c.ImageDigests = image.RepoDigests
	}
}

func (d *Docker) HasImage(ctx context.Context, image string) (bool, error) {
	_, err := d.cli.ImageInspect(ctx, image)
	if errdefs.IsNotFound(err) {
		return false, nil
	}
	return err == nil, err
}

func (d *Docker) ValidateContainer(ctx context.Context, id string) error {
	_, err := d.ownedContainer(ctx, id)
	return err
}

func (d *Docker) ownedContainer(ctx context.Context, id string) (container.InspectResponse, error) {
	out, err := d.cli.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
	if errdefs.IsNotFound(err) {
		return container.InspectResponse{}, ErrMissing
	}
	if err != nil {
		return container.InspectResponse{}, err
	}
	c := out.Container
	if c.ID != id || c.Config == nil {
		return container.InspectResponse{}, ErrOwnership
	}
	workspace := c.Config.Labels[workspaceLabel]
	generation, err := strconv.Atoi(c.Config.Labels[generationLabel])
	if workspace == "" || err != nil || generation < 1 || !d.owns(c.Config.Labels, workspace) ||
		c.Name != "/"+d.containerName(Spec{WorkspaceID: workspace, Generation: generation}) {
		return container.InspectResponse{}, ErrOwnership
	}
	return c, nil
}

func (d *Docker) ensureVolume(ctx context.Context, name, id string) error {
	v, e := d.cli.VolumeInspect(ctx, name, client.VolumeInspectOptions{})
	if errdefs.IsNotFound(e) {
		r, err := d.cli.VolumeCreate(ctx, client.VolumeCreateOptions{Name: name, Driver: "local", Labels: d.labels(id)})
		if err != nil {
			return err
		}
		if !d.owns(r.Volume.Labels, id) {
			return ErrOwnership
		}
		return nil
	}
	if e != nil {
		return e
	}
	if !d.owns(v.Volume.Labels, id) {
		return ErrOwnership
	}
	return nil
}

func (d *Docker) Ensure(ctx context.Context, s Spec) (Container, error) {
	current, e := d.Inspect(ctx, s)
	if !errors.Is(e, ErrMissing) {
		return current, e
	}
	if _, e := d.cli.ImageInspect(ctx, s.Image); errdefs.IsNotFound(e) {
		// The image is an operator-provided local template, never a request field.
		return Container{}, fmt.Errorf("workspace image %q not found; build or load it first", s.Image)
	} else if e != nil {
		return Container{}, e
	}
	names := volumeNames(s.WorkspaceID)
	labels := d.labels(s.WorkspaceID)
	labels[generationLabel] = strconv.Itoa(s.Generation)
	init := true
	pids := int64(512)
	cfg := &container.Config{Image: s.Image, User: "0:0", WorkingDir: "/workspace", Labels: labels,
		Cmd: []string{"/usr/local/bin/workspace-sshd"}, ExposedPorts: network.PortSet{sshContainerPort: {}},
	}
	host := &container.HostConfig{
		Init: &init, CapDrop: []string{"ALL"}, SecurityOpt: []string{"no-new-privileges:true"},
		NetworkMode: container.NetworkMode(s.SSHNetwork),
		// Only OpenSSH starts as root. Debian OpenSSH records PTY sessions through Linux audit.
		CapAdd:        []string{"CHOWN", "DAC_OVERRIDE", "SETUID", "SETGID", "SYS_CHROOT", "KILL", "AUDIT_WRITE"},
		Resources:     container.Resources{NanoCPUs: s.CPU * 1_000_000, Memory: s.Memory * 1024 * 1024, PidsLimit: &pids},
		LogConfig:     container.LogConfig{Type: "json-file", Config: map[string]string{"max-size": "10m", "max-file": "2"}},
		RestartPolicy: container.RestartPolicy{Name: container.RestartPolicyUnlessStopped},
	}
	for i, target := range []string{workspaceMount, sshHostKeyMount} {
		name := names[i]
		if err := d.ensureVolume(ctx, name, s.WorkspaceID); err != nil {
			return Container{}, err
		}
		host.Mounts = append(host.Mounts, mount.Mount{Type: mount.TypeVolume, Source: name, Target: target})
	}
	_, e = d.cli.ContainerCreate(ctx, client.ContainerCreateOptions{Name: d.containerName(s), Config: cfg, HostConfig: host})
	if e != nil && !errdefs.IsConflict(e) {
		return Container{}, e
	}
	return d.Inspect(ctx, s)
}

func (d *Docker) Start(ctx context.Context, id string) error {
	var status int
	_, e := d.cli.ContainerStart(context.WithValue(ctx, dockerPostStatusKey{}, &status), id, client.ContainerStartOptions{})
	if e != nil && (status < http.StatusBadRequest || status >= http.StatusInternalServerError) {
		return fmt.Errorf("%w: %w", ErrStartOutcomeUnknown, e)
	}
	return e
}
func (d *Docker) Stop(ctx context.Context, id string) error {
	seconds := 10
	_, e := d.cli.ContainerStop(ctx, id, client.ContainerStopOptions{Timeout: &seconds})
	return e
}
func (d *Docker) Remove(ctx context.Context, s Spec) error {
	c, e := d.Inspect(ctx, s)
	if errors.Is(e, ErrMissing) {
		return nil
	}
	if e != nil {
		return e
	}
	_, e = d.cli.ContainerRemove(ctx, c.ID, client.ContainerRemoveOptions{Force: true})
	if errdefs.IsNotFound(e) {
		return nil
	}
	return e
}
func (d *Docker) Purge(ctx context.Context, id string, includeSSHHostKey bool) error {
	names := volumeNames(id)
	selected := names[:1]
	if includeSSHHostKey {
		selected = names[:]
	}
	for _, n := range selected {
		v, e := d.cli.VolumeInspect(ctx, n, client.VolumeInspectOptions{})
		if errdefs.IsNotFound(e) {
			continue
		}
		if e != nil {
			return e
		}
		if !d.owns(v.Volume.Labels, id) {
			return ErrOwnership
		}
		if _, e = d.cli.VolumeRemove(ctx, n, client.VolumeRemoveOptions{}); e != nil && !errdefs.IsNotFound(e) {
			return e
		}
	}
	return nil
}

func (d *Docker) NoWorkspaceArtifacts(ctx context.Context, s Spec) (bool, error) {
	if _, err := d.Inspect(ctx, s); !errors.Is(err, ErrMissing) {
		return false, err
	}
	for _, name := range volumeNames(s.WorkspaceID) {
		if _, err := d.cli.VolumeInspect(ctx, name, client.VolumeInspectOptions{}); !errdefs.IsNotFound(err) {
			return false, err
		}
	}
	return true, nil
}

func (d *Docker) Run(ctx context.Context, id string, x Exec) (int, error) {
	execID, result, e := d.startExec(ctx, id, x)
	if e != nil {
		return -1, e
	}
	defer result.Close()
	stop := context.AfterFunc(ctx, func() { result.Close() })
	defer stop()
	if x.Stdout == nil {
		x.Stdout = io.Discard
	}
	if x.Stderr == nil {
		x.Stderr = io.Discard
	}
	if _, e = stdcopy.StdCopy(x.Stdout, x.Stderr, result.Reader); e != nil {
		return -1, e
	}
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		exit, e := d.cli.ExecInspect(ctx, execID, client.ExecInspectOptions{})
		if e != nil {
			return -1, e
		}
		if !exit.Running {
			return exit.ExitCode, nil
		}
		select {
		case <-ctx.Done():
			return -1, ctx.Err()
		case <-ticker.C:
		}
	}
}

func (d *Docker) startExec(ctx context.Context, id string, x Exec) (string, client.ExecAttachResult, error) {
	prepared, err := d.cli.ExecCreate(ctx, id, client.ExecCreateOptions{User: remote.User, Cmd: x.Args, WorkingDir: x.Dir,
		AttachStdout: true, AttachStderr: true})
	if err != nil {
		// Other Docker conflicts, such as an exited container, are not a pause.
		if errdefs.IsConflict(err) && strings.Contains(err.Error(), " is paused") {
			return "", client.ExecAttachResult{}, fmt.Errorf("%w: %w", ErrExecPaused, err)
		}
		return "", client.ExecAttachResult{}, err
	}
	// Once the start request has been sent, a transport failure does not
	// prove that the daemon failed to start the process.
	var status int
	result, err := d.cli.ExecAttach(context.WithValue(ctx, dockerPostStatusKey{}, &status), prepared.ID, client.ExecAttachOptions{})
	if err != nil && status == http.StatusConflict {
		// ExecAttach returns an untyped upgrade error. Preserve the 409
		// classification without assuming whether Docker paused or stopped.
		err = fmt.Errorf("%w: %w: %w", ErrExecStartConflict, errdefs.ErrConflict, err)
	}
	return prepared.ID, result, err
}

type dockerTerminal struct {
	engine     *Docker
	execID     string
	container  string
	session    string
	connection client.HijackedResponse
	stopLease  context.CancelFunc
	closeOnce  sync.Once
}

func (t *dockerTerminal) Read(p []byte) (int, error)  { return t.connection.Reader.Read(p) }
func (t *dockerTerminal) Write(p []byte) (int, error) { return t.connection.Conn.Write(p) }
func (t *dockerTerminal) SetWriteDeadline(deadline time.Time) error {
	return t.connection.Conn.SetWriteDeadline(deadline)
}
func (t *dockerTerminal) Close() error {
	t.closeOnce.Do(func() {
		t.stopLease()
		t.connection.Close()
		t.engine.endTerminal(t.container, t.session)
	})
	return nil
}
func (t *dockerTerminal) Resize(ctx context.Context, cols, rows uint) error {
	_, e := t.engine.cli.ExecResize(ctx, t.execID, client.ExecResizeOptions{Width: cols, Height: rows})
	return e
}

func (d *Docker) endTerminal(containerID, session string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	code, err := d.Run(ctx, containerID, Exec{Args: []string{"workspace-control", "cancel", session}, Dir: "/workspace"})
	if (err != nil && !errdefs.IsNotFound(err) && !errdefs.IsConflict(err)) || (err == nil && code != 0) {
		slog.Warn("could not confirm immediate terminal shutdown; its lease will expire", "error", err, "exit_code", code)
	}
}

func (d *Docker) Terminal(ctx context.Context, id string) (Terminal, error) {
	size := client.ConsoleSize{Height: 24, Width: 80}
	var token [16]byte
	rand.Read(token[:])
	session := hex.EncodeToString(token[:])
	p, e := d.cli.ExecCreate(ctx, id, client.ExecCreateOptions{User: remote.User, Cmd: []string{"workspace-control", "terminal", session}, WorkingDir: WorkDir, Env: []string{"TERM=xterm-256color"},
		TTY: true, AttachStdin: true, AttachStdout: true, AttachStderr: true, ConsoleSize: size})
	if e != nil {
		return nil, e
	}
	r, e := d.cli.ExecAttach(ctx, p.ID, client.ExecAttachOptions{TTY: true, ConsoleSize: size})
	if e != nil {
		d.endTerminal(id, session)
		return nil, e
	}
	// Docker can keep an exec PTY alive after the client socket is gone.
	// The lease also ends it if the control plane is killed without cleanup.
	lease, stop := context.WithCancel(context.Background())
	go func() {
		tick := time.NewTicker(20 * time.Second)
		defer tick.Stop()
		for {
			select {
			case <-lease.Done():
				return
			case <-tick.C:
			}
			beat, cancel := context.WithTimeout(lease, 5*time.Second)
			_, _ = d.Run(beat, id, Exec{Args: []string{"workspace-control", "renew", session}, Dir: "/workspace"})
			cancel()
		}
	}()
	return &dockerTerminal{engine: d, execID: p.ID, container: id, session: session, connection: r.HijackedResponse, stopLease: stop}, nil
}

func (d *Docker) Logs(ctx context.Context, id string, w io.Writer) error {
	r, e := d.cli.ContainerLogs(ctx, id, client.ContainerLogsOptions{ShowStdout: true, ShowStderr: true, Timestamps: true, Tail: "200"})
	if e != nil {
		return e
	}
	defer r.Close()
	_, e = stdcopy.StdCopy(w, w, r)
	return e
}
func (d *Docker) Metrics(ctx context.Context, id string) (Metrics, error) {
	r, e := d.cli.ContainerStats(ctx, id, client.ContainerStatsOptions{IncludePreviousSample: true})
	if errdefs.IsNotFound(e) {
		return Metrics{}, ErrMissing
	}
	if e != nil {
		return Metrics{}, e
	}
	defer r.Body.Close()
	var s container.StatsResponse
	e = json.NewDecoder(r.Body).Decode(&s)
	// A container can exit or be removed while Docker waits for two samples.
	if errors.Is(e, io.EOF) || (e == nil && s.Read.IsZero()) {
		return Metrics{}, ErrMetricsNotReady
	}
	if e != nil {
		return Metrics{}, e
	}
	m := Metrics{Memory: s.MemoryStats.Usage, Limit: s.MemoryStats.Limit}
	if cache := s.MemoryStats.Stats["inactive_file"]; cache > 0 && cache < m.Memory {
		m.Memory -= cache
	} else if cache := s.MemoryStats.Stats["total_inactive_file"]; cache > 0 && cache < m.Memory {
		m.Memory -= cache
	}
	c, p := s.CPUStats, s.PreCPUStats
	n := c.OnlineCPUs
	if n == 0 {
		n = uint32(len(c.CPUUsage.PercpuUsage))
	}
	if n > 0 && c.CPUUsage.TotalUsage >= p.CPUUsage.TotalUsage && c.SystemUsage > p.SystemUsage {
		v := float64(c.CPUUsage.TotalUsage-p.CPUUsage.TotalUsage) / float64(c.SystemUsage-p.SystemUsage) * float64(n) * 100
		m.CPU = &v
	}
	return m, nil
}
func (d *Docker) Host(ctx context.Context) (Host, error) {
	r, e := d.cli.Info(ctx, client.InfoOptions{})
	if e != nil {
		return Host{}, e
	}
	h := r.Info
	return Host{h.Name, h.ServerVersion, h.OperatingSystem, h.Architecture, h.NCPU, h.MemTotal, h.ContainersRunning, h.Containers}, nil
}
func (d *Docker) Containers(ctx context.Context) ([]HostContainer, error) {
	r, e := d.cli.ContainerList(ctx, client.ContainerListOptions{All: true})
	if e != nil {
		return nil, e
	}
	out := make([]HostContainer, 0, len(r.Items))
	for _, c := range r.Items {
		name := ""
		if len(c.Names) > 0 {
			name = strings.TrimPrefix(c.Names[0], "/")
		}
		w := c.Labels[workspaceLabel]
		if !d.owns(c.Labels, w) {
			w = ""
		}
		out = append(out, HostContainer{c.ID, name, c.Image, string(c.State), w})
	}
	return out, nil
}
func (d *Docker) Images(ctx context.Context) ([]Image, error) {
	r, e := d.cli.ImageList(ctx, client.ImageListOptions{})
	if e != nil {
		return nil, e
	}
	out := make([]Image, 0, len(r.Items))
	for _, i := range r.Items {
		out = append(out, Image{i.ID, i.RepoTags, i.Size})
	}
	return out, nil
}
