package mesh

import (
	"testing"

	"github.com/peter-dolkens/vineyard/daemon/internal/protocol"
)

// orchard is behind NAT: nobody can dial it, and it reaches only forge. With uplink auto it finds
// out it is unreachable and holds an uplink to forge while nobody watches it. atelier, watching,
// then reaches it through forge, which gets a callback over the uplink.
func TestUnreachableMachineHoldsAnUplinkAndIsReachedThroughIt(t *testing.T) {
	newMeshDir(t)
	orchard, orchardAddr := meshNode(t, "orchard")
	forge, forgeAddr := meshNode(t, "forge", protocol.PeerAddr{MachineID: "orchard", Addr: orchardAddr})
	atelier, atelierAddr := meshNode(t, "atelier", protocol.PeerAddr{MachineID: "forge", Addr: forgeAddr}, protocol.PeerAddr{MachineID: "orchard", Addr: orchardAddr})
	orchard.cfg.Uplink = "auto"
	for _, pa := range []protocol.PeerAddr{{MachineID: "forge", Addr: forgeAddr}, {MachineID: "atelier", Addr: atelierAddr}} {
		orchard.cfg.AddPeer(pa)
		orchard.peers[pa.MachineID] = &peerState{id: pa.MachineID, addr: pa.Addr, addrs: []string{pa.Addr}}
	}
	block(atelier, orchardAddr)
	block(forge, orchardAddr)
	block(orchard, atelierAddr)
	runNodes(t, orchard, forge, atelier)

	waitFor(t, "orchard to hold an uplink to forge", func() bool {
		orchard.mu.Lock()
		defer orchard.mu.Unlock()
		return orchard.up.link != nil && orchard.up.link.peerID == "forge" && orchard.up.reach == unreachable
	})
	waitFor(t, "forge to treat it as an uplink", func() bool {
		forge.mu.Lock()
		defer forge.mu.Unlock()
		return len(forge.uplinksLocked()) == 1
	})

	atelier.setWantFleet(true)
	waitFor(t, "orchard's snapshot through the uplink", func() bool {
		e, ok := atelier.entry("orchard")
		return ok && e.Online && e.Via == "relay:forge" && e.Snapshot.Uplink == "forge"
	})
	atelier.mu.Lock()
	uplinkTo := atelier.peers["orchard"].uplinkTo
	atelier.mu.Unlock()
	if uplinkTo != "forge" {
		t.Fatalf("forge's hello should have named orchard's uplink; got %q", uplinkTo)
	}
}

// A machine others can reach checks once and then holds nothing.
func TestReachableMachineHoldsNoUplink(t *testing.T) {
	newMeshDir(t)
	forge, forgeAddr := meshNode(t, "forge")
	orchard, _ := meshNode(t, "orchard", protocol.PeerAddr{MachineID: "forge", Addr: forgeAddr})
	orchard.cfg.Uplink = "auto"
	runNodes(t, forge, orchard)
	waitFor(t, "orchard to find it is reachable", func() bool {
		orchard.mu.Lock()
		defer orchard.mu.Unlock()
		return orchard.up.reach == reachable && !orchard.up.attempt
	})
	waitFor(t, "the check's connection to close", func() bool {
		forge.mu.Lock()
		defer forge.mu.Unlock()
		return len(forge.links) == 0
	})
	orchard.mu.Lock()
	defer orchard.mu.Unlock()
	if orchard.up.link != nil {
		t.Fatal("a reachable machine is holding an uplink")
	}
}

// uplink off: no check, no connection.
func TestUplinkOffDoesNothing(t *testing.T) {
	newMeshDir(t)
	forge, forgeAddr := meshNode(t, "forge")
	orchard, _ := meshNode(t, "orchard", protocol.PeerAddr{MachineID: "forge", Addr: forgeAddr})
	runNodes(t, forge, orchard)
	for range 30 {
		orchard.uplinkTick()
	}
	orchard.mu.Lock()
	defer orchard.mu.Unlock()
	if orchard.up.attempt || orchard.up.reach != reachUnknown || len(orchard.links) != 0 {
		t.Fatalf("uplink off still acted: %+v, %d links", orchard.up, len(orchard.links))
	}
}

// orchard advertises an address where a different fleet machine (decoy) answers. forge's dialback
// reaches the decoy, not orchard, so orchard must not conclude it is reachable.
func TestDialbackThatReachesAnotherMachineDoesNotCount(t *testing.T) {
	newMeshDir(t)
	decoy, decoyAddr := meshNode(t, "decoy")
	forge, forgeAddr := meshNode(t, "forge")
	orchard, orchardAddr := meshNode(t, "orchard", protocol.PeerAddr{MachineID: "forge", Addr: forgeAddr})
	orchard.cfg.Uplink = "auto"
	orchard.cfg.Advertise = decoyAddr // what orchard believes its address is; the decoy lives there
	block(forge, orchardAddr)         // orchard's real listener is not reachable from forge
	runNodes(t, decoy, forge, orchard)
	waitFor(t, "orchard to decide it is unreachable and hold an uplink", func() bool {
		orchard.mu.Lock()
		defer orchard.mu.Unlock()
		return orchard.up.reach == unreachable && orchard.up.link != nil
	})
}
