package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/coachpo/cloudex/apps/api/internal/runtime"
)

func serveLocalGateway(ctx context.Context, handler http.Handler) (*http.Server, <-chan error, error) {
	listener, err := net.Listen("tcp", runtime.LocalGatewayAddress)
	if err != nil {
		return nil, nil, fmt.Errorf("listen for local Workspace gateway: %w", err)
	}
	server := &http.Server{Handler: handler,
		BaseContext:       func(net.Listener) context.Context { return ctx },
		ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 * 1024}
	exited := make(chan error, 1)
	go func() { exited <- server.Serve(listener) }()
	return server, exited, nil
}
