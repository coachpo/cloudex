package hostssh

import (
	"io"
	"net"
	"time"
)

type channelStream interface {
	io.ReadWriteCloser
	CloseWrite() error
}

// SSH channels share their underlying TCP connection and do not support deadlines.
// Independent pipe ends give each Docker stream cancellable reads and writes
// without affecting other sessions. Each pump buffers at most one copy block.
type streamConn struct {
	read, write net.Conn
	channel     channelStream
}

func newStreamConn(ch channelStream) *streamConn {
	read, incoming := net.Pipe()
	write, outgoing := net.Pipe()
	go func() {
		io.Copy(incoming, ch)
		incoming.Close()
	}()
	go func() {
		io.Copy(ch, outgoing)
		outgoing.Close()
		ch.CloseWrite()
	}()
	return &streamConn{read, write, ch}
}

func (c *streamConn) Read(p []byte) (int, error)  { return c.read.Read(p) }
func (c *streamConn) Write(p []byte) (int, error) { return c.write.Write(p) }
func (c *streamConn) CloseWrite() error           { return c.write.Close() }
func (c *streamConn) Close() error {
	c.read.Close()
	c.write.Close()
	return c.channel.Close()
}
func (c *streamConn) LocalAddr() net.Addr                { return c.read.LocalAddr() }
func (c *streamConn) RemoteAddr() net.Addr               { return c.read.RemoteAddr() }
func (c *streamConn) SetReadDeadline(t time.Time) error  { return c.read.SetReadDeadline(t) }
func (c *streamConn) SetWriteDeadline(t time.Time) error { return c.write.SetWriteDeadline(t) }
func (c *streamConn) SetDeadline(t time.Time) error {
	if err := c.SetReadDeadline(t); err != nil {
		return err
	}
	return c.SetWriteDeadline(t)
}
