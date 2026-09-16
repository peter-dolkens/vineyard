//go:build windows

package claude

import (
	"net"
	"os"
	"time"
)

// pipeConn adapts an *os.File opened on a named pipe to net.Conn (byte mode is enough for our
// write-then-close usage).
type pipeConn struct{ *os.File }

func (p pipeConn) LocalAddr() net.Addr                { return pipeAddr(p.Name()) }
func (p pipeConn) RemoteAddr() net.Addr               { return pipeAddr(p.Name()) }
func (p pipeConn) SetDeadline(t time.Time) error      { return nil }
func (p pipeConn) SetReadDeadline(t time.Time) error  { return nil }
func (p pipeConn) SetWriteDeadline(t time.Time) error { return nil }

type pipeAddr string

func (a pipeAddr) Network() string { return "pipe" }
func (a pipeAddr) String() string  { return string(a) }

func dialPipe(path string, timeout time.Duration) (net.Conn, error) {
	deadline := time.Now().Add(timeout)
	for {
		f, err := os.OpenFile(path, os.O_RDWR, 0)
		if err == nil {
			return pipeConn{f}, nil
		}
		if time.Now().After(deadline) {
			return nil, err
		}
		time.Sleep(50 * time.Millisecond) // ERROR_PIPE_BUSY: server is between accepts
	}
}
