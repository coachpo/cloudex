// Package agentlink binds the control plane to Host Agents over outbound gRPC
// streams. Only explicitly listed runtime calls cross the protocol boundary.
package agentlink

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"

	"github.com/coachpo/cloudex/apps/api/internal/runtime"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

const serviceName = "cloudex.agent.v1.AgentLink"
const chunkSize = 32 * 1024

// Inventory is the latest capacity observation from an authenticated agent.
type Inventory struct {
	Host          runtime.Host  `json:"host"`
	Images        []ImageStatus `json:"images,omitzero"`
	MaxConcurrent int           `json:"max_concurrent"`
	Version       string        `json:"version"`
	Error         string        `json:"error,omitempty"`
}

type ImageStatus struct {
	Image string `json:"image"`
	Ready bool   `json:"ready"`
}

type AgentConfig struct {
	MaxConcurrent int `json:"max_concurrent"`
	// Nil leaves the set unchanged. An explicit empty list clears it.
	Images []string `json:"images,omitzero"`
}

type frame struct {
	Kind  string          `json:"kind"`
	Value json.RawMessage `json:"value,omitempty"`
	Data  []byte          `json:"data,omitempty"`
	Error *wireError      `json:"error,omitempty"`
}

type request struct {
	AuthorizedKeys    []string       `json:"authorized_keys,omitempty"`
	Method            string         `json:"method"`
	ID                string         `json:"id,omitempty"`
	Spec              *runtime.Spec  `json:"spec,omitempty"`
	Address           netip.AddrPort `json:"address,omitempty"`
	IncludeSSHHostKey bool           `json:"include_ssh_host_key,omitempty"`
	OperationID       string         `json:"operation_id,omitempty"`
	WorkspaceID       string         `json:"workspace_id,omitempty"`

	Exec          *execRequest           `json:"exec,omitempty"`
	Config        *AgentConfig           `json:"config,omitempty"`
	GitCredential *runtime.GitCredential `json:"git_credential,omitempty"`
	CodexConfig   *runtime.CodexConfig   `json:"codex_config,omitempty"`
}

type execRequest struct {
	Args []string `json:"args"`
	Dir  string   `json:"dir"`
}

type wireContainer struct {
	runtime.Container
	IPs map[string]netip.Addr `json:"network_ips"`
}

func encodeContainer(c runtime.Container) wireContainer {
	return wireContainer{Container: c, IPs: c.NetworkIPs}
}

func (c wireContainer) decode() runtime.Container {
	c.Container.NetworkIPs = c.IPs
	return c.Container
}

type dimensions struct {
	Cols uint `json:"cols"`
	Rows uint `json:"rows"`
}

type wireError struct {
	Message string   `json:"message"`
	Codes   []string `json:"codes,omitempty"`
}

var errorCodes = []struct {
	code string
	err  error
}{
	{"missing", runtime.ErrMissing},
	{"ownership", runtime.ErrOwnership},
	{"exec_paused", runtime.ErrExecPaused},
	{"exec_start_conflict", runtime.ErrExecStartConflict},
	{"start_unknown", runtime.ErrStartOutcomeUnknown},
	{"host_unavailable", runtime.ErrHostUnavailable},
	{"metrics_not_ready", runtime.ErrMetricsNotReady},
	{"deadline", context.DeadlineExceeded},
	{"cancelled", context.Canceled},
}

func encodeError(err error) *wireError {
	if err == nil {
		return nil
	}
	w := &wireError{Message: err.Error()}
	for _, entry := range errorCodes {
		if errors.Is(err, entry.err) {
			w.Codes = append(w.Codes, entry.code)
		}
	}
	return w
}

func (e *wireError) Error() string { return e.Message }

func (e *wireError) Is(target error) bool {
	for _, code := range e.Codes {
		for _, entry := range errorCodes {
			if code == entry.code && target == entry.err {
				return true
			}
		}
	}
	return false
}

func valueFrame(kind string, value any) (frame, error) {
	data, err := json.Marshal(value)
	return frame{Kind: kind, Value: data}, err
}

func decode[T any](f frame, want string) (T, error) {
	var value T
	if f.Kind != want {
		return value, fmt.Errorf("unexpected agent frame %q, expected %q", f.Kind, want)
	}
	return value, json.Unmarshal(f.Value, &value)
}

// BytesValue is the public v1 protobuf envelope. Its payload is a JSON frame
// drawn from the private, versioned call vocabulary above.
type stream interface {
	SendMsg(any) error
	RecvMsg(any) error
}

func send(s stream, f frame) error {
	data, err := json.Marshal(f)
	if err != nil {
		return err
	}
	return s.SendMsg(wrapperspb.Bytes(data))
}

func receive(s stream) (frame, error) {
	var msg wrapperspb.BytesValue
	if err := s.RecvMsg(&msg); err != nil {
		return frame{}, err
	}
	var f frame
	return f, json.Unmarshal(msg.Value, &f)
}

const controlStream = 0
const callStream = 1

type agentService interface {
	connect(grpc.ServerStream) error
	call(grpc.ServerStream) error
}

var service = grpc.ServiceDesc{
	ServiceName: serviceName,
	HandlerType: (*agentService)(nil),
	Streams: []grpc.StreamDesc{
		{
			StreamName: "Connect", ClientStreams: true, ServerStreams: true,
			Handler: func(srv any, stream grpc.ServerStream) error { return srv.(agentService).connect(stream) },
		},
		{
			StreamName: "Exchange", ClientStreams: true, ServerStreams: true,
			Handler: func(srv any, stream grpc.ServerStream) error { return srv.(agentService).call(stream) },
		},
	},
}

func openStream(ctx context.Context, conn grpc.ClientConnInterface, kind int) (grpc.ClientStream, error) {
	desc := &service.Streams[kind]
	return conn.NewStream(ctx, desc, "/"+serviceName+"/"+desc.StreamName)
}
