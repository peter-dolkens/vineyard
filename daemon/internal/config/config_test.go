package config

import (
	"fmt"
	"slices"
	"testing"

	"github.com/peter-dolkens/vineyard/daemon/internal/protocol"
)

func TestAddPeerMergesCandidates(t *testing.T) {
	c := New("atelier", "", 0, "")
	if !c.AddPeer(protocol.PeerAddr{MachineID: "forge", Addr: "forge.local:7734"}) {
		t.Fatal("new peer not added")
	}
	// A legacy entry (Addr only) gains candidates; the primary stays when none is given.
	if !c.AddPeer(protocol.PeerAddr{MachineID: "forge", Addrs: []string{"192.168.20.9:7734", "forge.local:7734"}}) {
		t.Fatal("candidates not merged")
	}
	got := c.Peers[0]
	if got.Addr != "forge.local:7734" || !slices.Equal(got.Addrs, []string{"forge.local:7734", "192.168.20.9:7734"}) {
		t.Fatalf("got %+v", got)
	}
	if c.AddPeer(protocol.PeerAddr{MachineID: "forge", Addrs: []string{"192.168.20.9:7734"}}) {
		t.Fatal("no-op merge reported a change")
	}
	// A new primary moves to the front.
	c.AddPeer(protocol.PeerAddr{MachineID: "forge", Addr: "192.168.20.9:7734"})
	if got := c.Peers[0]; got.Addr != "192.168.20.9:7734" || got.Addrs[0] != "192.168.20.9:7734" {
		t.Fatalf("primary not promoted: %+v", got)
	}
	if c.AddPeer(protocol.PeerAddr{MachineID: "atelier", Addr: "atelier.local:7734"}) {
		t.Fatal("added ourselves as a peer")
	}
}

func TestMergeAddrsCapsAndPrefersFresh(t *testing.T) {
	var old []string
	for i := range MaxAddrs {
		old = append(old, fmt.Sprintf("10.0.0.%d:7734", i))
	}
	got := MergeAddrs("forge.local:7734", []string{"192.168.1.5:7734"}, old)
	if len(got) != MaxAddrs {
		t.Fatalf("len %d, want %d", len(got), MaxAddrs)
	}
	if got[0] != "forge.local:7734" || got[1] != "192.168.1.5:7734" {
		t.Fatalf("primary and fresh should lead: %v", got)
	}
	if slices.Contains(got, old[MaxAddrs-1]) {
		t.Fatal("the stalest candidate should have been dropped")
	}
}

func TestRemovedPeerIsRecordedUntilAddedAgain(t *testing.T) {
	c := New("atelier", "", 0, "")
	c.AddPeer(protocol.PeerAddr{MachineID: "orchard", Addr: "orchard.local:7734"})
	if !c.RemovePeer("orchard") || len(c.Peers) != 0 || !c.IsRemoved("orchard") {
		t.Fatalf("remove: peers %v removed %v", c.Peers, c.Removed)
	}
	if !c.AddPeer(protocol.PeerAddr{MachineID: "orchard", Addr: "orchard.local:7734"}) || c.IsRemoved("orchard") {
		t.Fatalf("re-adding should un-remove: removed %v", c.Removed)
	}
}
