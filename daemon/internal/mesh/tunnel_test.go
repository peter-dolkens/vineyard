package mesh

import (
	"context"
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

// meshNode builds a daemon listening on loopback. All nodes in a test share VINEYARD_DIR, and so the
// fleet certificate; call newMeshDir first.
func meshNode(t *testing.T, id string, peers ...protocol.PeerAddr) (*Node, string) {
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

func newMeshDir(t *testing.T) {
	t.Helper()
	t.Setenv("VINEYARD_DIR", t.TempDir())
	if err := config.GenerateFleetCert(); err != nil {
		t.Fatal(err)
	}
}

func runNodes(t *testing.T, nodes ...*Node) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	for _, n := range nodes {
		go func() { _ = n.Run(ctx) }()
	}
	time.Sleep(100 * time.Millisecond) // listeners up
}

// block makes the machines listening at addrs unreachable from n, standing in for "not on my
// network". It matches on port, so the LAN addresses a hello adds for the same daemon are blocked too.
func block(n *Node, addrs ...string) {
	var ports []string
	for _, a := range addrs {
		_, port, _ := net.SplitHostPort(a)
		ports = append(ports, port)
	}
	n.dialFilter = func(in []string) []string {
		return slices.DeleteFunc(slices.Clone(in), func(a string) bool {
			_, port, _ := net.SplitHostPort(a)
			return slices.Contains(ports, port)
		})
	}
}

func linkVia(n *Node, peer string) string {
	n.mu.Lock()
	defer n.mu.Unlock()
	p := n.peers[peer]
	if p == nil || p.link == nil {
		return ""
	}
	if p.link.via == "" {
		return "direct"
	}
	return p.link.via
}

// atelier cannot reach orchard, but forge can: atelier relays through forge, which dials orchard.
func TestRelayDialsTheTarget(t *testing.T) {
	newMeshDir(t)
	orchard, orchardAddr := meshNode(t, "orchard")
	forge, forgeAddr := meshNode(t, "forge", protocol.PeerAddr{MachineID: "orchard", Addr: orchardAddr})
	atelier, _ := meshNode(t, "atelier", protocol.PeerAddr{MachineID: "forge", Addr: forgeAddr}, protocol.PeerAddr{MachineID: "orchard", Addr: orchardAddr})
	block(atelier, orchardAddr)
	runNodes(t, orchard, forge, atelier)
	atelier.setWantFleet(true)

	waitFor(t, "orchard's snapshot through the relay", func() bool {
		e, ok := atelier.entry("orchard")
		return ok && e.Online && e.Via == "relay:forge"
	})
	if v := linkVia(orchard, "atelier"); v != "relay:forge" {
		t.Fatalf("orchard sees atelier via %q", v)
	}
	atelier.mu.Lock()
	st := atelier.peerStatusLocked()
	atelier.mu.Unlock()
	i := slices.IndexFunc(st, func(p protocol.PeerStatus) bool { return p.MachineID == "orchard" })
	if i < 0 || st[i].Via != "relay:forge" || st[i].LastError == "" {
		t.Fatalf("peer status should show the relay and why direct failed: %+v", st)
	}
}

// Nobody can dial orchard (it is behind NAT), but orchard holds a link to forge. forge cannot dial
// it, so it asks orchard to call back over that link.
func TestRelayFallsBackToCallback(t *testing.T) {
	newMeshDir(t)
	orchard, orchardAddr := meshNode(t, "orchard")
	forge, forgeAddr := meshNode(t, "forge", protocol.PeerAddr{MachineID: "orchard", Addr: orchardAddr})
	atelier, atelierAddr := meshNode(t, "atelier", protocol.PeerAddr{MachineID: "forge", Addr: forgeAddr}, protocol.PeerAddr{MachineID: "orchard", Addr: orchardAddr})
	orchard.cfg.AddPeer(protocol.PeerAddr{MachineID: "forge", Addr: forgeAddr})
	orchard.peers["forge"] = &peerState{id: "forge", addr: forgeAddr, addrs: []string{forgeAddr}}
	block(atelier, orchardAddr)
	block(forge, orchardAddr)
	block(orchard, atelierAddr) // orchard reaches forge only
	runNodes(t, orchard, forge, atelier)

	orchard.setWantFleet(true) // holds its link to forge
	waitFor(t, "orchard's link to forge", func() bool { return linkVia(forge, "orchard") == "direct" })
	atelier.setWantFleet(true)
	waitFor(t, "orchard's snapshot through the callback", func() bool {
		e, ok := atelier.entry("orchard")
		return ok && e.Online && e.Via == "relay:forge"
	})
}

// A relay that cannot reach the target either says so, and the requester gives up cleanly.
func TestRelayThatCannotReachSaysSo(t *testing.T) {
	newMeshDir(t)
	orchard, orchardAddr := meshNode(t, "orchard")
	forge, forgeAddr := meshNode(t, "forge", protocol.PeerAddr{MachineID: "orchard", Addr: orchardAddr})
	atelier, _ := meshNode(t, "atelier", protocol.PeerAddr{MachineID: "forge", Addr: forgeAddr}, protocol.PeerAddr{MachineID: "orchard", Addr: orchardAddr})
	block(atelier, orchardAddr)
	block(forge, orchardAddr)
	runNodes(t, orchard, forge, atelier)
	atelier.setWantFleet(true)
	waitFor(t, "forge to be linked", func() bool { return linkVia(atelier, "forge") == "direct" })

	atelier.mu.Lock()
	p := atelier.peers["orchard"]
	atelier.mu.Unlock()
	waitFor(t, "orchard's dial to fail through the relay too", func() bool {
		atelier.mu.Lock()
		defer atelier.mu.Unlock()
		return p.lastErr != "" && p.link == nil && !p.dialing
	})
	if _, _, err := atelier.viaRelay("orchard"); err == nil {
		t.Fatal("relay succeeded to an unreachable target")
	}
}
