package mesh

import (
	"context"
	"crypto/tls"
	"log"
	"net"
	"os"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/peter-dolkens/vineyard/daemon/internal/config"
	"github.com/peter-dolkens/vineyard/daemon/internal/model"
	"github.com/peter-dolkens/vineyard/daemon/internal/protocol"
)

// A hello's peer list teaches us machines we did not know, skipping ourselves and machines removed
// here, and only adds candidates to machines we already know.
func TestHelloPeerListIsLearned(t *testing.T) {
	n := newTestNode(t, "atelier")
	n.cfg.AddPeer(protocol.PeerAddr{MachineID: "forge", Addr: "forge.local:7734"})
	n.peers["forge"] = &peerState{id: "forge", addr: "forge.local:7734", addrs: []string{"forge.local:7734"}}
	n.cfg.RemovePeer("retired")

	hello := protocol.Hello{T: "hello", Role: "peer", MachineID: "forge", Listen: "forge.local:7734", Protocol: protocol.Version,
		Peers: []protocol.PeerAddr{
			{MachineID: "orchard", Addr: "orchard.local:7734", Addrs: []string{"orchard.local:7734", "192.168.20.30:7734"}},
			{MachineID: "atelier", Addr: "atelier.local:7734"},
			{MachineID: "retired", Addr: "retired.local:7734"},
		}}
	ours, theirs := net.Pipe()
	defer theirs.Close()
	n.register(&link{conn: NewConn(ours), outbound: true, peerID: "forge", role: "peer"}, hello)

	n.mu.Lock()
	defer n.mu.Unlock()
	o := n.peers["orchard"]
	if o == nil || o.addr != "orchard.local:7734" || !slices.Equal(o.addrs, []string{"orchard.local:7734", "192.168.20.30:7734"}) {
		t.Fatalf("orchard not learned: %+v", o)
	}
	if n.peers["atelier"] != nil {
		t.Fatal("learned ourselves")
	}
	if n.peers["retired"] != nil || slices.ContainsFunc(n.cfg.Peers, func(p protocol.PeerAddr) bool { return p.MachineID == "retired" }) {
		t.Fatal("a removed machine was learned back")
	}
	if !slices.ContainsFunc(n.cfg.Peers, func(p protocol.PeerAddr) bool { return p.MachineID == "orchard" && len(p.Addrs) == 2 }) {
		t.Fatalf("orchard not persisted with its candidates: %+v", n.cfg.Peers)
	}
}

func TestHelloCarriesKnownPeers(t *testing.T) {
	n := newTestNode(t, "atelier")
	n.peers["forge"] = &peerState{id: "forge", addr: "forge.local:7734", addrs: []string{"forge.local:7734", "192.168.20.9:7734"}}
	n.peers["ghost"] = &peerState{id: "ghost"} // no address: nothing to tell anyone
	h := n.hello("peer")
	if len(h.Peers) != 1 || h.Peers[0].MachineID != "forge" || len(h.Peers[0].Addrs) != 2 {
		t.Fatalf("hello peers: %+v", h.Peers)
	}
}

// A primary that accepts TCP but never finishes TLS must not hold up the next candidate for a whole
// dial timeout.
func TestDialPeerMovesPastAStalledAddress(t *testing.T) {
	newTestNode(t, "atelier") // fleet certificate in VINEYARD_DIR
	srvTLS, cliTLS, err := FleetTLS()
	if err != nil {
		t.Fatal(err)
	}
	stall, err := net.Listen("tcp", "127.0.0.1:0") // accepts, then says nothing
	if err != nil {
		t.Fatal(err)
	}
	defer stall.Close()
	go func() {
		for {
			c, err := stall.Accept()
			if err != nil {
				return
			}
			defer c.Close()
		}
	}()
	good, err := tls.Listen("tcp", "127.0.0.1:0", srvTLS)
	if err != nil {
		t.Fatal(err)
	}
	defer good.Close()
	go func() {
		for {
			c, err := good.Accept()
			if err != nil {
				return
			}
			go func() { _ = c.(*tls.Conn).Handshake() }()
		}
	}()

	began := time.Now()
	tc, used, err := dialPeer([]string{stall.Addr().String(), good.Addr().String()}, cliTLS)
	if err != nil {
		t.Fatal(err)
	}
	tc.Close()
	if used != good.Addr().String() {
		t.Fatalf("used %s", used)
	}
	if took := time.Since(began); took > 2*time.Second {
		t.Fatalf("took %s; the stalled address held up the dial", took)
	}

	if _, _, err := dialPeer([]string{"127.0.0.1:1"}, cliTLS); err == nil {
		t.Fatal("dial to a closed port succeeded")
	}
}

// Three daemons over loopback. atelier (watching) knows only forge; forge knows orchard, the way a
// machine added over SSH from forge would be. atelier must learn orchard from forge's hello and
// connect to it, and orchard learns atelier. Once removed on atelier, orchard is not learned back.
func TestMachineListSpreadsThroughHello(t *testing.T) {
	// One VINEYARD_DIR for all three: they share the fleet certificate, and their config saves land in
	// the same file, which nothing here reads back.
	t.Setenv("VINEYARD_DIR", t.TempDir())
	if err := config.GenerateFleetCert(); err != nil {
		t.Fatal(err)
	}
	node := func(id string, peers ...protocol.PeerAddr) (*Node, string) {
		t.Helper()
		addr := "127.0.0.1:" + strconv.Itoa(freePort(t))
		cfg := config.New(id, id, 0, addr)
		cfg.Listen = addr
		cfg.Peers = peers
		n, err := New(Options{Config: cfg, Version: "test", Log: log.New(os.Stderr, id+" ", 0), Collect: func() model.Snapshot { return model.Snapshot{} }})
		if err != nil {
			t.Fatal(err)
		}
		return n, addr
	}
	orchard, orchardAddr := node("orchard")
	forge, forgeAddr := node("forge", protocol.PeerAddr{MachineID: "orchard", Addr: orchardAddr})
	atelier, _ := node("atelier", protocol.PeerAddr{MachineID: "forge", Addr: forgeAddr})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for _, n := range []*Node{orchard, forge, atelier} {
		go func() { _ = n.Run(ctx) }()
	}
	time.Sleep(100 * time.Millisecond) // listeners up
	atelier.setWantFleet(true)

	waitFor(t, "atelier to receive orchard's snapshot", func() bool {
		e, ok := atelier.entry("orchard")
		return ok && e.Online
	})
	waitFor(t, "orchard to learn atelier", func() bool {
		orchard.mu.Lock()
		defer orchard.mu.Unlock()
		return orchard.peers["atelier"] != nil
	})

	if _, err := atelier.handleLocal(protocol.Request{Op: "removepeer", Args: []byte(`{"machineId":"orchard","addr":""}`)}); err != nil {
		t.Fatal(err)
	}
	// Reconnect to forge so its hello (which still lists orchard) arrives again.
	atelier.mu.Lock()
	if l := atelier.peers["forge"].link; l != nil {
		l.conn.Close()
	}
	atelier.mu.Unlock()
	waitFor(t, "atelier to reconnect to forge", func() bool {
		atelier.mu.Lock()
		defer atelier.mu.Unlock()
		p := atelier.peers["forge"]
		return p != nil && p.link != nil && p.link.weSubscribed
	})
	time.Sleep(300 * time.Millisecond)
	atelier.mu.Lock()
	defer atelier.mu.Unlock()
	if atelier.peers["orchard"] != nil {
		t.Fatal("orchard was learned back after being removed")
	}
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}
