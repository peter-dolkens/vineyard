package agenttools

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/peter-dolkens/vineyard/daemon/internal/mesh"
	"github.com/peter-dolkens/vineyard/daemon/internal/model"
	"github.com/peter-dolkens/vineyard/daemon/internal/protocol"
)

const (
	// idleClose drops the viewer link this long after the last tool call, unless a reply is awaited,
	// so a session that used a tool once does not keep the fleet watched for the rest of its life.
	idleClose = time.Minute
	// viewerPing keeps the link inside the daemon's 95 s idle limit, as the web app's bridge does.
	viewerPing = 30 * time.Second
	// settleMax / settleQuiet: after connecting, wait for peers' snapshots to come in, up to settleMax
	// and until none has arrived for settleQuiet, so the first listing is not just the cache.
	settleMax   = 3 * time.Second
	settleQuiet = 500 * time.Millisecond
)

// Client is the MCP server's viewer link to the local daemon: connected on first use, kept while
// used or while a reply is awaited, and closed when idle.
type Client struct {
	dial func() (*mesh.Conn, error)

	mu       sync.Mutex
	conn     *mesh.Conn
	self     string
	entries  map[string]model.FleetEntry
	order    []string
	pending  map[string]chan protocol.Response
	changed  chan struct{} // closed and replaced on every fleet change
	lastMsg  time.Time
	lastUse  time.Time
	holds    int
	gotFleet chan struct{}
}

func NewClient(dial func() (*mesh.Conn, error)) *Client {
	c := &Client{dial: dial, entries: map[string]model.FleetEntry{}, pending: map[string]chan protocol.Response{}, changed: make(chan struct{})}
	go c.reaper()
	return c
}

// Hold keeps the link open until the returned release is called (a reply being waited for).
func (c *Client) Hold() (release func()) {
	c.mu.Lock()
	c.holds++
	c.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			c.mu.Lock()
			c.holds--
			c.lastUse = time.Now()
			c.mu.Unlock()
		})
	}
}

func (c *Client) reaper() {
	t := time.NewTicker(10 * time.Second)
	defer t.Stop()
	for range t.C {
		c.mu.Lock()
		conn := c.conn
		idle := conn != nil && c.holds == 0 && time.Since(c.lastUse) > idleClose
		if idle {
			c.conn = nil
		}
		c.mu.Unlock()
		if idle {
			conn.Close()
		}
	}
}

// ensure connects if there is no link and waits for the fleet to settle.
func (c *Client) ensure() error {
	c.mu.Lock()
	c.lastUse = time.Now()
	if c.conn != nil {
		c.mu.Unlock()
		return nil
	}
	c.mu.Unlock()
	conn, err := c.dial()
	if err != nil {
		return err
	}
	got := make(chan struct{})
	c.mu.Lock()
	if c.conn != nil { // another call connected meanwhile
		c.mu.Unlock()
		conn.Close()
		return nil
	}
	c.conn = conn
	c.gotFleet = got
	c.mu.Unlock()
	go c.pump(conn)
	deadline := time.Now().Add(settleMax)
	select {
	case <-got:
	case <-time.After(settleMax):
		return nil // answer from whatever arrives; requests still work
	}
	for time.Now().Before(deadline) {
		c.mu.Lock()
		quiet := time.Since(c.lastMsg)
		c.mu.Unlock()
		if quiet >= settleQuiet {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	return nil
}

func (c *Client) pump(conn *mesh.Conn) {
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		t := time.NewTicker(viewerPing)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-conn.Done():
				return
			case <-t.C:
				_ = conn.Send(protocol.Ping{T: "ping"})
			}
		}
	}()
	for {
		line, err := conn.Recv(95 * time.Second)
		if err != nil {
			break
		}
		c.handle(conn, line)
	}
	conn.Close()
	c.mu.Lock()
	if c.conn == conn {
		c.conn = nil
	}
	for id, ch := range c.pending {
		close(ch)
		delete(c.pending, id)
	}
	c.mu.Unlock()
}

func (c *Client) handle(conn *mesh.Conn, line []byte) {
	var env protocol.Envelope
	if json.Unmarshal(line, &env) != nil {
		return
	}
	switch env.T {
	case "ping":
		_ = conn.Send(protocol.Ping{T: "pong"})
	case "fleet":
		var f protocol.Fleet
		if json.Unmarshal(line, &f) != nil {
			return
		}
		c.mu.Lock()
		c.self = f.Self
		for _, e := range f.Entries {
			c.putLocked(e)
		}
		if c.gotFleet != nil {
			close(c.gotFleet)
			c.gotFleet = nil
		}
		c.notifyLocked()
		c.mu.Unlock()
	case "update":
		var u protocol.Update
		if json.Unmarshal(line, &u) != nil || u.Entry.Snapshot.MachineID == "" {
			return
		}
		c.mu.Lock()
		c.putLocked(u.Entry)
		c.notifyLocked()
		c.mu.Unlock()
	case "res":
		var r protocol.Response
		if json.Unmarshal(line, &r) != nil {
			return
		}
		c.mu.Lock()
		ch := c.pending[r.ID]
		delete(c.pending, r.ID)
		c.mu.Unlock()
		if ch != nil {
			ch <- r
		}
	}
}

func (c *Client) putLocked(e model.FleetEntry) {
	id := e.Snapshot.MachineID
	if id == "" {
		return
	}
	if _, ok := c.entries[id]; !ok {
		c.order = append(c.order, id)
	}
	c.entries[id] = e
	c.lastMsg = time.Now()
}

func (c *Client) notifyLocked() {
	close(c.changed)
	c.changed = make(chan struct{})
}

// Changed is closed at the next fleet change.
func (c *Client) Changed() <-chan struct{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.changed
}

// Fleet is this machine's id and every machine's entry, in the order the daemon first listed them.
func (c *Client) Fleet() (string, []model.FleetEntry, error) {
	if err := c.ensure(); err != nil {
		return "", nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]model.FleetEntry, 0, len(c.order))
	for _, id := range c.order {
		out = append(out, c.entries[id])
	}
	return c.self, out, nil
}

// Entry is one machine's entry, matched by id or (case-insensitively) by name.
func (c *Client) Entry(machine string) (model.FleetEntry, error) {
	_, all, err := c.Fleet()
	if err != nil {
		return model.FleetEntry{}, err
	}
	if e, ok := matchMachine(all, machine); ok {
		return e, nil
	}
	return model.FleetEntry{}, errors.New("no machine called " + machine + " in the fleet (list_machines shows them)")
}

// Request relays one operation through the local daemon to target ("" for this machine).
func (c *Client) Request(target, op string, args any, timeout time.Duration) (json.RawMessage, error) {
	if err := c.ensure(); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(args)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	conn := c.conn
	if conn == nil {
		c.mu.Unlock()
		return nil, errors.New("not connected to the local Vineyard daemon")
	}
	if c.self != "" && target == c.self {
		target = ""
	}
	id := newID()
	ch := make(chan protocol.Response, 1)
	c.pending[id] = ch
	c.mu.Unlock()
	if err := conn.Send(protocol.Request{T: "req", ID: id, Target: target, Op: op, Args: raw}); err != nil {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, err
	}
	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case r, ok := <-ch:
		if !ok {
			return nil, errors.New("the local Vineyard daemon disconnected")
		}
		if !r.OK {
			if r.Error == "" {
				r.Error = op + " failed"
			}
			return nil, errors.New(r.Error)
		}
		return r.Data, nil
	case <-t.C:
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, errors.New(op + " timed out")
	}
}

func newID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
