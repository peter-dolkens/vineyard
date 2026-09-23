package mesh

import (
	"testing"

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

	// orchard, still running and now watching, is turned away by forge.
	orchard.setWantFleet(true)
	waitFor(t, "orchard's attempt on forge to be refused", func() bool {
		orchard.mu.Lock()
		defer orchard.mu.Unlock()
		p := orchard.peers["forge"]
		return p != nil && p.link == nil && !p.lastSeen.IsZero()
	})

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
