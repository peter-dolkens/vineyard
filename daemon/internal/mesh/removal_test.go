package mesh

import (
	"net"
	"testing"
	"time"

	"github.com/peter-dolkens/vineyard/daemon/internal/protocol"
)

// Removing orchard on atelier reaches forge at once over their link, forge then refuses orchard's
// connections, and a later deliberate add on atelier lets it back in everywhere.
func TestRemovalSpreadsAndIsRefusedUntilAddedAgain(t *testing.T) {
	newMeshDir(t)
	orchard, orchardAddr := meshNode(t, "orchard")
	forge, forgeAddr := meshNode(t, "forge", protocol.PeerAddr{MachineID: "orchard", Addr: orchardAddr})
	atelier, _ := meshNode(t, "atelier", protocol.PeerAddr{MachineID: "forge", Addr: forgeAddr}, protocol.PeerAddr{MachineID: "orchard", Addr: orchardAddr})
	runNodes(t, orchard, forge, atelier)
	atelier.setWantFleet(true)
	waitFor(t, "atelier linked to both", func() bool {
		return linkVia(atelier, "forge") == "direct" && linkVia(atelier, "orchard") == "direct"
	})

	if _, err := atelier.handleLocal(protocol.Request{Op: "removepeer", Args: []byte(`{"machineId":"orchard","addr":""}`)}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "forge to hear of the removal", func() bool {
		forge.mu.Lock()
		defer forge.mu.Unlock()
		return forge.cfg.IsRemoved("orchard") && forge.peers["orchard"] == nil
	})

	// orchard, still running and now watching, keeps trying forge and is turned away every time.
	orchard.setWantFleet(true)
	time.Sleep(1500 * time.Millisecond)
	forge.mu.Lock()
	for l := range forge.links {
		if l.peerID == "orchard" {
			forge.mu.Unlock()
			t.Fatal("forge accepted a link from a removed machine")
		}
	}
	back := forge.peers["orchard"] != nil
	forge.mu.Unlock()
	if back {
		t.Fatal("forge learned the removed machine back")
	}

	// Added back on atelier: forge learns it on atelier's next hello, with a later Added.
	if _, err := atelier.handleLocal(protocol.Request{Op: "addpeer", Args: []byte(`{"machineId":"orchard","addr":"` + orchardAddr + `"}`)}); err != nil {
		t.Fatal(err)
	}
	atelier.mu.Lock()
	if l := atelier.peers["forge"].link; l != nil {
		l.conn.Close()
	}
	atelier.mu.Unlock()
	waitFor(t, "forge to let orchard back in", func() bool {
		forge.mu.Lock()
		defer forge.mu.Unlock()
		return !forge.cfg.IsRemoved("orchard") && forge.peers["orchard"] != nil
	})
}

// Removals written by 0.3.21 (undated, and only ever meant for that one machine's view) stay local.
func TestUndatedRemovalsAreNotPassedOn(t *testing.T) {
	n := newTestNode(t, "atelier")
	n.cfg.Removed = append(n.cfg.Removed, protocol.Removal{MachineID: "hidden"}, protocol.Removal{MachineID: "gone", At: 5})
	h := n.hello("peer")
	if len(h.Removed) != 1 || h.Removed[0].MachineID != "gone" {
		t.Fatalf("hello removals: %+v", h.Removed)
	}
}

type remoteConn struct {
	net.Conn
	addr net.Addr
}

func (c remoteConn) RemoteAddr() net.Addr { return c.addr }

// Viewers must be on this machine: one arriving from another address is refused.
func TestRemoteViewerIsRefused(t *testing.T) {
	n := newTestNode(t, "atelier")
	ours, theirs := net.Pipe()
	defer theirs.Close()
	far := &net.TCPAddr{IP: net.ParseIP("192.168.20.9"), Port: 50000}
	l := &link{conn: NewConn(remoteConn{Conn: ours, addr: far}), role: "viewer", peerID: "forge#viewer"}
	n.register(l, protocol.Hello{T: "hello", Role: "viewer", MachineID: "forge#viewer", Protocol: protocol.Version})
	select {
	case <-l.conn.Done():
	case <-time.After(time.Second):
		t.Fatal("remote viewer not refused")
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if len(n.viewers) != 0 {
		t.Fatal("remote viewer registered")
	}
}
