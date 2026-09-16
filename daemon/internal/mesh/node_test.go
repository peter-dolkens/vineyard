package mesh

import (
	"crypto/tls"
	"encoding/json"
	"log"
	"net"
	"os"
	"testing"
	"time"

	"github.com/peter-dolkens/vineyard/daemon/internal/config"
	"github.com/peter-dolkens/vineyard/daemon/internal/model"
	"github.com/peter-dolkens/vineyard/daemon/internal/protocol"
)

func newTestNode(t *testing.T, machineID string) *Node {
	t.Helper()
	t.Setenv("VINEYARD_DIR", t.TempDir())
	if err := config.GenerateFleetCert(); err != nil {
		t.Fatal(err)
	}
	cfg := config.New(machineID, "", 0, "")
	n, err := New(Options{
		Config:  cfg,
		Version: "test",
		Log:     log.New(os.Stderr, "", 0),
		Collect: func() model.Snapshot { return model.Snapshot{} },
	})
	if err != nil {
		t.Fatal(err)
	}
	n.wantFleet = true
	return n
}

func expectMessage(t *testing.T, c *Conn, want string) {
	t.Helper()
	b, err := c.Recv(2 * time.Second)
	if err != nil {
		t.Fatalf("waiting for %q: %v", want, err)
	}
	var env protocol.Envelope
	_ = json.Unmarshal(b, &env)
	if env.T != want {
		t.Fatalf("got %q, want %q (%s)", env.T, want, b)
	}
}

// A second link to a peer that replaces the first must be subscribed afresh. This is the bug that
// left magpie<->falcon and frogmouth->falcon connected but silent: the surviving link inherited
// weSubscribed=true from the link being closed and never received a subscribe of its own.
func TestReplacedDuplicateLinkIsResubscribed(t *testing.T) {
	// Our id sorts after the peer's, so a duplicate outbound link takes the "replace" path.
	n := newTestNode(t, "zebra")
	hello := protocol.Hello{T: "hello", Role: "peer", MachineID: "apple", Listen: "apple:7734", Protocol: protocol.Version}

	ours1, theirs1 := net.Pipe()
	l1 := &link{conn: NewConn(ours1), outbound: true, peerID: "apple", role: "peer"}
	n.register(l1, hello)
	far1 := NewConn(theirs1)
	expectMessage(t, far1, "subscribe")

	ours2, theirs2 := net.Pipe()
	l2 := &link{conn: NewConn(ours2), outbound: true, peerID: "apple", role: "peer"}
	n.register(l2, hello)
	far2 := NewConn(theirs2)
	expectMessage(t, far2, "subscribe")

	select {
	case <-l1.conn.Done():
	case <-time.After(time.Second):
		t.Fatal("old link was not closed")
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.peers["apple"].link != l2 {
		t.Fatal("peer state does not point at the surviving link")
	}
	if !l2.weSubscribed {
		t.Fatal("surviving link not marked subscribed")
	}
}

// When our id sorts before the peer's, the first link wins and the newcomer is closed; the winner
// keeps its subscription and must not be sent a second subscribe.
func TestDuplicateLinkKeepsOriginal(t *testing.T) {
	n := newTestNode(t, "apple")
	hello := protocol.Hello{T: "hello", Role: "peer", MachineID: "zebra", Listen: "zebra:7734", Protocol: protocol.Version}

	ours1, theirs1 := net.Pipe()
	l1 := &link{conn: NewConn(ours1), outbound: true, peerID: "zebra", role: "peer"}
	n.register(l1, hello)
	far1 := NewConn(theirs1)
	expectMessage(t, far1, "subscribe")

	ours2, _ := net.Pipe()
	l2 := &link{conn: NewConn(ours2), outbound: true, peerID: "zebra", role: "peer"}
	n.register(l2, hello)
	select {
	case <-l2.conn.Done():
	case <-time.After(time.Second):
		t.Fatal("newcomer link was not closed")
	}
	if _, err := far1.Recv(200 * time.Millisecond); err == nil {
		t.Fatal("original link received an unexpected message")
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.peers["zebra"].link != l1 {
		t.Fatal("peer state should still point at the original link")
	}
}

// A peer whose TCP connection is open but whose hello has not arrived yet must not be dialled again
// by the next reconcile tick.
func TestDialStaysMarkedUntilHandshakeEnds(t *testing.T) {
	n := newTestNode(t, "zebra")
	srvTLS, _, err := FleetTLS()
	if err != nil {
		t.Fatal(err)
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", srvTLS)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		c, err := ln.Accept()
		if err == nil {
			_ = c.(*tls.Conn).Handshake()
			accepted <- c // hold it open, never say hello
		}
	}()

	p := &peerState{id: "apple", addr: ln.Addr().String(), addrs: []string{ln.Addr().String()}, dialing: true}
	n.mu.Lock()
	n.peers["apple"] = p
	n.mu.Unlock()
	n.dial(p)

	n.mu.Lock()
	dialing := p.dialing
	n.mu.Unlock()
	if !dialing {
		t.Fatal("dialing cleared before the handshake finished")
	}
	n.reconcileSubscriptions() // would start a second dial if the flag were clear
	n.mu.Lock()
	dialing = p.dialing
	n.mu.Unlock()
	if !dialing {
		t.Fatal("dialing cleared by reconcile")
	}

	// Dropping the server side ends the handshake wait; the flag must clear so a redial can happen.
	select {
	case c := <-accepted:
		c.Close()
	case <-time.After(2 * time.Second):
		t.Fatal("dial never reached the listener")
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		n.mu.Lock()
		dialing = p.dialing
		n.mu.Unlock()
		if !dialing {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("dialing never cleared after the connection dropped")
}
