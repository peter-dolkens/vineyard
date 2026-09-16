package mesh

import (
	"bufio"
	"encoding/json"
	"errors"
	"net"
	"sync"
	"time"
)

// maxLine bounds a single inbound message; transcripts are chunked well below this.
const maxLine = 32 << 20

// Conn is a newline-delimited JSON connection with a non-blocking send queue. Slow readers are
// disconnected rather than allowed to back up the whole daemon.
type Conn struct {
	raw    net.Conn
	rd     *bufio.Reader
	queue  chan []byte
	closed chan struct{}
	once   sync.Once
	err    error
	mu     sync.Mutex
}

func NewConn(raw net.Conn) *Conn {
	c := &Conn{
		raw:    raw,
		rd:     bufio.NewReaderSize(raw, 64<<10),
		queue:  make(chan []byte, 256),
		closed: make(chan struct{}),
	}
	go c.writer()
	return c
}

func (c *Conn) writer() {
	for {
		select {
		case <-c.closed:
			return
		case b := <-c.queue:
			_ = c.raw.SetWriteDeadline(time.Now().Add(30 * time.Second))
			if _, err := c.raw.Write(b); err != nil {
				c.closeWith(err)
				return
			}
		}
	}
}

// Send enqueues a message; it never blocks the caller. Returns an error only if the connection is
// closed or the queue is full (in which case the connection is dropped as unhealthy).
func (c *Conn) Send(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	select {
	case <-c.closed:
		return c.Err()
	default:
	}
	select {
	case c.queue <- b:
		return nil
	default:
		err := errors.New("send queue full")
		c.closeWith(err)
		return err
	}
}

// Recv blocks for the next message. idle is how long to wait before treating the peer as dead.
func (c *Conn) Recv(idle time.Duration) ([]byte, error) {
	_ = c.raw.SetReadDeadline(time.Now().Add(idle))
	var line []byte
	for {
		chunk, isPrefix, err := c.rd.ReadLine()
		if err != nil {
			c.closeWith(err)
			return nil, err
		}
		line = append(line, chunk...)
		if len(line) > maxLine {
			err := errors.New("message too large")
			c.closeWith(err)
			return nil, err
		}
		if !isPrefix {
			break
		}
	}
	return line, nil
}

func (c *Conn) closeWith(err error) {
	c.once.Do(func() {
		c.mu.Lock()
		c.err = err
		c.mu.Unlock()
		close(c.closed)
		_ = c.raw.Close()
	})
}

func (c *Conn) Close() { c.closeWith(errors.New("closed")) }

func (c *Conn) Done() <-chan struct{} { return c.closed }

func (c *Conn) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

func (c *Conn) RemoteAddr() string { return c.raw.RemoteAddr().String() }
