package agentlink

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

type resizeRequest struct {
	ID uint64 `json:"id"`
	dimensions
}

type resizeResult struct {
	ID    uint64     `json:"id"`
	Error *wireError `json:"error,omitempty"`
}

// duplex exposes a remote PTY or SSH socket through two local in-memory
// pipes. Separate pipes preserve half-close and provide net.Conn deadlines.
type duplex struct {
	peer     *exchange
	in, out  net.Conn
	seq      atomic.Uint64
	resizeMu sync.Mutex
	resizes  map[uint64]chan error
}

func newDuplex(peer *exchange) *duplex {
	in, receive := net.Pipe()
	out, send := net.Pipe()
	d := &duplex{peer: peer, in: in, out: out, resizes: make(map[uint64]chan error)}
	context.AfterFunc(peer.ctx, func() { receive.Close(); send.Close() })
	go d.receive(receive)
	go func() {
		if _, err := io.Copy(outputWriter{peer: peer, ctx: peer.ctx, kind: "stdin"}, send); err != nil {
			peer.cancel()
			return
		}
		if err := peer.put(peer.ctx, frame{Kind: "eof"}); err != nil {
			peer.cancel()
		}
	}()
	return d
}

func (d *duplex) receive(w io.WriteCloser) {
	defer d.peer.cancel()
	defer w.Close()
	for {
		f, err := d.peer.recv(d.peer.ctx)
		if err != nil {
			return
		}
		switch f.Kind {
		case "stdout":
			if err := writeFrame(w, f.Data); err != nil {
				return
			}
		case "eof":
			_ = w.Close()
		case "resized":
			result, err := decode[resizeResult](f, "resized")
			if err != nil {
				return
			}
			if result.Error != nil {
				err = result.Error
			}
			d.resizeMu.Lock()
			if pending := d.resizes[result.ID]; pending != nil {
				pending <- err
			}
			d.resizeMu.Unlock()
		default:
			return
		}
	}
}

func (d *duplex) Read(p []byte) (int, error)  { return d.in.Read(p) }
func (d *duplex) Write(p []byte) (int, error) { return d.out.Write(p) }
func (d *duplex) CloseWrite() error           { return d.out.Close() }

func (d *duplex) Close() error {
	d.peer.cancel()
	_ = d.in.Close()
	return d.out.Close()
}

func (d *duplex) LocalAddr() net.Addr                { return d.in.LocalAddr() }
func (d *duplex) RemoteAddr() net.Addr               { return d.out.RemoteAddr() }
func (d *duplex) SetReadDeadline(t time.Time) error  { return d.in.SetReadDeadline(t) }
func (d *duplex) SetWriteDeadline(t time.Time) error { return d.out.SetWriteDeadline(t) }
func (d *duplex) SetDeadline(t time.Time) error {
	return errors.Join(d.SetReadDeadline(t), d.SetWriteDeadline(t))
}

func (d *duplex) Resize(ctx context.Context, cols, rows uint) error {
	id := d.seq.Add(1)
	ack := make(chan error, 1)
	d.resizeMu.Lock()
	d.resizes[id] = ack
	d.resizeMu.Unlock()
	defer func() {
		d.resizeMu.Lock()
		delete(d.resizes, id)
		d.resizeMu.Unlock()
	}()
	req := resizeRequest{ID: id, dimensions: dimensions{Cols: cols, Rows: rows}}
	if err := d.peer.putValue(ctx, "resize", req); err != nil {
		return err
	}
	select {
	case err := <-ack:
		return err
	case <-d.peer.ctx.Done():
		return unavailable(d.peer.ctx.Err())
	case <-ctx.Done():
		return ctx.Err()
	}
}

func serveDuplex(ctx context.Context, peer *exchange, rw io.ReadWriteCloser, resize func(context.Context, uint, uint) error) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer rw.Close()
	stop := context.AfterFunc(ctx, func() { rw.Close() })
	defer stop()
	if err := peer.put(ctx, frame{Kind: "ready"}); err != nil {
		return err
	}
	written, read := make(chan error, 1), make(chan error, 1)
	go func() { written <- receiveInput(ctx, peer, rw, resize) }()
	go func() {
		_, err := io.Copy(outputWriter{peer: peer, ctx: ctx, kind: "stdout"}, rw)
		if err == nil {
			err = peer.put(ctx, frame{Kind: "eof"})
		}
		read <- err
	}()
	for written != nil || read != nil {
		select {
		case err := <-written:
			if err != nil || resize != nil {
				return err
			}
			written = nil
		case err := <-read:
			if err != nil || resize != nil {
				return err
			}
			read = nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

func receiveInput(ctx context.Context, peer *exchange, rw io.Writer, resize func(context.Context, uint, uint) error) error {
	for {
		f, err := peer.recv(ctx)
		if err != nil {
			return err
		}
		switch f.Kind {
		case "stdin":
			if err = writeFrame(rw, f.Data); err != nil {
				return err
			}
		case "eof":
			if half, ok := rw.(interface{ CloseWrite() error }); ok {
				return half.CloseWrite()
			}
			return nil
		case "resize":
			req, err := decode[resizeRequest](f, "resize")
			if err != nil || resize == nil || req.Cols < 1 || req.Cols > 500 || req.Rows < 1 || req.Rows > 300 {
				return errors.New("invalid terminal dimensions")
			}
			limit, stop := context.WithTimeout(ctx, 3*time.Second)
			err = resize(limit, req.Cols, req.Rows)
			stop()
			if err = peer.putValue(ctx, "resized", resizeResult{ID: req.ID, Error: encodeError(err)}); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unexpected connection input frame %q", f.Kind)
		}
	}
}
