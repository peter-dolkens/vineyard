//go:build !windows

package claude

import (
	"errors"
	"net"
	"time"
)

func dialPipe(path string, timeout time.Duration) (net.Conn, error) {
	return nil, errors.New("named pipes are only available on Windows")
}
