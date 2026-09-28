package web

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/peter-dolkens/vineyard/daemon/internal/mesh"
	"github.com/peter-dolkens/vineyard/daemon/internal/protocol"
)

// linger keeps the viewer link this long after the last browser leaves, so a reload or a hop between
// pages does not detach and re-attach (the daemon adds its own 30 s grace on top). A var for tests.
var linger = 15 * time.Second

const (
	// viewerPing keeps the link inside the daemon's 95 s idle limit; it goes over loopback only.
	viewerPing = 30 * time.Second
	// upstreamIdle is how long a silent daemon link is trusted: it answers our pings within this.
	upstreamIdle = 95 * time.Second
	maxBackoff   = 30 * time.Second
)

// Bridge is the web server's one viewer link to the local daemon, shared by every browser. It is
// open only while at least one browser holds the event stream (plus linger), so a phone that is put
// away leaves the fleet as quiet as a closed VS Code window.
type Bridge struct {
	dial func() (*mesh.Conn, error)
	logf func(string, ...any)

	mu      sync.Mutex
	conn    *mesh.Conn
	state   string // idle | connecting | connected | disconnected
	lastErr string
	self    string
	order   []string                   // machine ids in first-seen order, for a stable fleet message
	entries map[string]json.RawMessage // machine id → FleetEntry, as the daemon sent it
	peers   json.RawMessage
	clients map[*client]struct{}
	pending map[string]chan protocol.Response
	linger  *time.Timer
	running bool // the dial loop is alive; there is never more than one
}

type client struct {
	ch chan []byte
}

func NewBridge(dial func() (*mesh.Conn, error), logf func(string, ...any)) *Bridge {
	return &Bridge{
		dial:    dial,
		logf:    logf,
		state:   "idle",
		entries: map[string]json.RawMessage{},
		clients: map[*client]struct{}{},
		pending: map[string]chan protocol.Response{},
	}
}

// subscribe registers a browser. The returned channel starts with the current state and, when the
// daemon link is up, the whole fleet; it is closed if the browser falls too far behind.
func (b *Bridge) subscribe() *client {
	c := &client{ch: make(chan []byte, 512)}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.clients[c] = struct{}{}
	if b.linger != nil {
		b.linger.Stop()
		b.linger = nil
	}
	if !b.running {
		b.running = true
		b.state = "connecting"
		b.broadcastLocked(b.stateMsgLocked())
		go b.run()
	} else {
		c.ch <- b.stateMsgLocked()
	}
	if b.state == "connected" && len(b.entries) > 0 {
		c.ch <- b.fleetMsgLocked()
	}
	return c
}

func (b *Bridge) unsubscribe(c *client) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.clients[c]; !ok {
		return
	}
	delete(b.clients, c)
	if len(b.clients) == 0 && b.linger == nil {
		b.linger = time.AfterFunc(linger, b.detachIfUnwatched)
	}
}

func (b *Bridge) detachIfUnwatched() {
	b.mu.Lock()
	b.linger = nil
	if len(b.clients) > 0 {
		b.mu.Unlock()
		return
	}
	conn := b.conn
	b.conn = nil
	b.state = "idle"
	b.entries = map[string]json.RawMessage{}
	b.order = nil
	b.peers = nil
	b.mu.Unlock()
	if conn != nil {
		b.logf("no browsers left; detached from the daemon")
		conn.Close()
	}
}

// stillWatched reports whether a browser is attached; when none is, the dial loop ends.
func (b *Bridge) stillWatched() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.clients) == 0 {
		b.running = false
		return false
	}
	return true
}

// run dials the daemon and pumps its messages until the link drops, then retries with backoff for
// as long as someone is watching.
func (b *Bridge) run() {
	backoff := time.Second
	for b.stillWatched() {
		conn, err := b.dial()
		if err != nil {
			b.setState("disconnected", err.Error())
			time.Sleep(backoff)
			backoff = min(backoff*2, maxBackoff)
			continue
		}
		backoff = time.Second
		b.mu.Lock()
		if len(b.clients) == 0 {
			b.running = false
			b.mu.Unlock()
			conn.Close()
			return
		}
		b.conn = conn
		b.state = "connected"
		b.lastErr = ""
		b.broadcastLocked(b.stateMsgLocked())
		b.mu.Unlock()
		b.logf("attached to the local daemon as a viewer")

		b.pump(conn)

		b.mu.Lock()
		if b.conn == conn {
			b.conn = nil
		}
		for id, ch := range b.pending {
			close(ch)
			delete(b.pending, id)
		}
		b.mu.Unlock()
		msg := "daemon connection closed"
		if err := conn.Err(); err != nil {
			msg = err.Error()
		}
		b.setState("disconnected", msg)
		time.Sleep(backoff)
	}
}

func (b *Bridge) setState(state, errText string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.clients) == 0 {
		return // detached on purpose; the state stays idle
	}
	b.state = state
	b.lastErr = errText
	b.broadcastLocked(b.stateMsgLocked())
}

func (b *Bridge) pump(conn *mesh.Conn) {
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
		line, err := conn.Recv(upstreamIdle)
		if err != nil {
			return
		}
		b.handle(conn, line)
	}
}

func (b *Bridge) handle(conn *mesh.Conn, line []byte) {
	var env protocol.Envelope
	if json.Unmarshal(line, &env) != nil {
		return
	}
	switch env.T {
	case "ping":
		_ = conn.Send(protocol.Ping{T: "pong"})
	case "fleet":
		var f struct {
			Self    string            `json:"self"`
			Entries []json.RawMessage `json:"entries"`
			Peers   json.RawMessage   `json:"peers"`
		}
		if json.Unmarshal(line, &f) != nil {
			return
		}
		b.mu.Lock()
		b.self = f.Self
		b.entries = map[string]json.RawMessage{}
		b.order = nil
		for _, e := range f.Entries {
			b.putEntryLocked(e)
		}
		b.peers = f.Peers
		b.broadcastLocked(b.fleetMsgLocked())
		b.mu.Unlock()
	case "update":
		var u struct {
			Entry json.RawMessage `json:"entry"`
		}
		if json.Unmarshal(line, &u) != nil || len(u.Entry) == 0 {
			return
		}
		b.mu.Lock()
		b.putEntryLocked(u.Entry)
		b.broadcastLocked(line)
		b.mu.Unlock()
	case "peerstatus":
		var p struct {
			Peers json.RawMessage `json:"peers"`
		}
		if json.Unmarshal(line, &p) != nil {
			return
		}
		b.mu.Lock()
		b.peers = p.Peers
		b.broadcastLocked(line)
		b.mu.Unlock()
	case "res":
		var r protocol.Response
		if json.Unmarshal(line, &r) != nil {
			return
		}
		b.mu.Lock()
		ch := b.pending[r.ID]
		delete(b.pending, r.ID)
		b.mu.Unlock()
		if ch != nil {
			ch <- r
		}
	}
}

func (b *Bridge) putEntryLocked(raw json.RawMessage) {
	var e struct {
		Snapshot struct {
			MachineID string `json:"machineId"`
		} `json:"snapshot"`
	}
	if json.Unmarshal(raw, &e) != nil || e.Snapshot.MachineID == "" {
		return
	}
	id := e.Snapshot.MachineID
	if _, ok := b.entries[id]; !ok {
		b.order = append(b.order, id)
	}
	b.entries[id] = raw
}

func (b *Bridge) stateMsgLocked() []byte {
	out, _ := json.Marshal(map[string]string{"t": "state", "state": b.state, "error": b.lastErr})
	return out
}

func (b *Bridge) fleetMsgLocked() []byte {
	entries := make([]json.RawMessage, 0, len(b.order))
	for _, id := range b.order {
		entries = append(entries, b.entries[id])
	}
	peers := b.peers
	if len(peers) == 0 {
		peers = json.RawMessage("[]")
	}
	out, _ := json.Marshal(map[string]any{"t": "fleet", "self": b.self, "entries": entries, "peers": peers})
	return out
}

// broadcastLocked hands a message to every browser; one that cannot keep up is dropped (its stream
// ends and the page reconnects) rather than holding up the rest.
func (b *Bridge) broadcastLocked(msg []byte) {
	dropped := false
	for c := range b.clients {
		select {
		case c.ch <- msg:
		default:
			delete(b.clients, c)
			close(c.ch)
			dropped = true
		}
	}
	if dropped && len(b.clients) == 0 && b.linger == nil {
		b.linger = time.AfterFunc(linger, b.detachIfUnwatched)
	}
}

var errNotConnected = errors.New("not connected to the local daemon")

// Request relays one operation to the daemon (and through it to any machine) and waits for the answer.
func (b *Bridge) Request(target, op string, args json.RawMessage, timeout time.Duration) (json.RawMessage, error) {
	b.mu.Lock()
	conn := b.conn
	if conn == nil {
		b.mu.Unlock()
		return nil, errNotConnected
	}
	id := newID()
	ch := make(chan protocol.Response, 1)
	b.pending[id] = ch
	b.mu.Unlock()

	if err := conn.Send(protocol.Request{T: "req", ID: id, Target: target, Op: op, Args: args}); err != nil {
		b.mu.Lock()
		delete(b.pending, id)
		b.mu.Unlock()
		return nil, err
	}
	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case r, ok := <-ch:
		if !ok {
			return nil, errors.New("daemon disconnected")
		}
		if !r.OK {
			if r.Error == "" {
				r.Error = "request failed"
			}
			return nil, errors.New(r.Error)
		}
		return r.Data, nil
	case <-t.C:
		b.mu.Lock()
		delete(b.pending, id)
		b.mu.Unlock()
		return nil, errors.New("request timed out")
	}
}

func newID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
