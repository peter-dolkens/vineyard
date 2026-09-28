package mesh

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/peter-dolkens/vineyard/daemon/internal/config"
	"github.com/peter-dolkens/vineyard/daemon/internal/protocol"
)

// runOne runs a single node until the returned stop is called.
func runOne(t *testing.T, n *Node) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = n.Run(ctx); close(done) }()
	time.Sleep(100 * time.Millisecond)
	t.Cleanup(cancel)
	return func() {
		cancel()
		<-done
	}
}

// restart builds a fresh daemon from a stopped one's config and key directory.
func restart(t *testing.T, n *Node) *Node {
	t.Helper()
	m, err := New(Options{Config: n.cfg, Version: "test", Dir: n.opts.Dir, Log: n.opts.Log, Collect: n.opts.Collect})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// leafSignedBy reports whether the node's machine certificate is signed directly by root.
func leafSignedBy(t *testing.T, n *Node, rootPEM string) bool {
	t.Helper()
	m, err := config.LoadMachine(n.opts.Dir)
	if err != nil {
		t.Fatal(err)
	}
	root, err := config.ParseCerts([]byte(rootPEM))
	if err != nil {
		t.Fatal(err)
	}
	return m.Leaf.CheckSignatureFrom(root[0]) == nil
}

func reissueOf(n *Node) *protocol.Reissue {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.cfg.Reissue
}

func linkedAll(t *testing.T, pairs ...[2]*Node) {
	t.Helper()
	for _, p := range pairs {
		waitFor(t, p[0].cfg.MachineID+" linked to "+p[1].cfg.MachineID, func() bool { return linkVia(p[0], p[1].cfg.MachineID) == "direct" })
	}
}

// Re-issuing on atelier reaches forge at once over their link; orchard, offline at the time, gets it
// from forge's hello when it comes back, and every one of them ends up on a certificate signed by the
// new root, for the key it already had.
func TestReissueReachesConnectedAndLateMembers(t *testing.T) {
	newMeshDir(t)
	orchard, orchardAddr := meshNode(t, "orchard")
	forge, forgeAddr := meshNode(t, "forge", protocol.PeerAddr{MachineID: "orchard", Addr: orchardAddr})
	atelier, atelierAddr := meshNode(t, "atelier", protocol.PeerAddr{MachineID: "forge", Addr: forgeAddr}, protocol.PeerAddr{MachineID: "orchard", Addr: orchardAddr})
	orchard.cfg.AddPeer(protocol.PeerAddr{MachineID: "atelier", Addr: atelierAddr})
	stopOrchard := runOne(t, orchard)
	runOne(t, forge)
	runOne(t, atelier)
	for _, n := range []*Node{atelier, forge, orchard} {
		n.setWantFleet(true)
	}
	linkedAll(t, [2]*Node{atelier, forge}, [2]*Node{atelier, orchard}, [2]*Node{forge, orchard}, [2]*Node{orchard, atelier})
	orchard.mu.Lock()
	keyBefore := config.KeyFingerprint(orchard.me.Leaf)
	orchard.mu.Unlock()
	stopOrchard()

	if _, err := atelier.rotateKey(nil, time.Hour); err != nil {
		t.Fatal(err)
	}
	r := reissueOf(atelier)
	if r == nil || len(r.Chains) != 3 {
		t.Fatalf("re-issue: %+v", r)
	}
	waitFor(t, "forge to take the re-issue", func() bool { return reissueOf(forge) != nil })
	if !leafSignedBy(t, forge, r.Root) || !leafSignedBy(t, atelier, r.Root) {
		t.Fatal("connected members are not on the new root")
	}
	roots, _ := config.Roots(forge.opts.Dir)
	if len(roots) != 2 {
		t.Fatalf("forge trusts %d roots during grace, want 2", len(roots))
	}

	orchard = restart(t, orchard)
	runOne(t, orchard)
	orchard.setWantFleet(true)
	waitFor(t, "orchard to catch up", func() bool { return reissueOf(orchard) != nil })
	if !leafSignedBy(t, orchard, r.Root) {
		t.Fatal("orchard is not on the new root")
	}
	orchard.mu.Lock()
	keyAfter := config.KeyFingerprint(orchard.me.Leaf)
	orchard.mu.Unlock()
	if keyAfter != keyBefore {
		t.Fatal("re-issue changed orchard's key; it must only re-sign the key it had")
	}
}

// After the grace period only the new root is trusted, so a machine still on a certificate under the
// old one (anything minted with the pre-0.3.23 shared key, say) is refused.
func TestAfterReissueGraceTheOldRootIsRefused(t *testing.T) {
	newMeshDir(t)
	orchard, orchardAddr := meshNode(t, "orchard")
	atelier, atelierAddr := meshNode(t, "atelier", protocol.PeerAddr{MachineID: "orchard", Addr: orchardAddr})
	orchard.cfg.AddPeer(protocol.PeerAddr{MachineID: "atelier", Addr: atelierAddr})
	stopOrchard := runOne(t, orchard)
	runOne(t, atelier)
	atelier.setWantFleet(true)
	linkedAll(t, [2]*Node{atelier, orchard})
	stopOrchard()

	if _, err := atelier.rotateKey(nil, 200*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	atelier.reissueTick()
	roots, _ := config.Roots(atelier.opts.Dir)
	if len(roots) != 1 {
		t.Fatalf("atelier still trusts %d roots after the grace period", len(roots))
	}
	orchard = restart(t, orchard) // never saw the re-issue: still under the old root
	runOne(t, orchard)
	orchard.setWantFleet(true)
	time.Sleep(1500 * time.Millisecond)
	if linkVia(atelier, "orchard") != "" || linkVia(orchard, "atelier") != "" {
		t.Fatal("a machine under the old root was let in after the grace period")
	}
}

// Machines left out of a re-issue are removed first, their keys revoked, and get no certificate.
func TestReissueExcludesTheLostMachine(t *testing.T) {
	newMeshDir(t)
	orchard, orchardAddr := meshNode(t, "orchard")
	atelier, _ := meshNode(t, "atelier", protocol.PeerAddr{MachineID: "orchard", Addr: orchardAddr})
	runNodes(t, orchard, atelier)
	atelier.setWantFleet(true)
	linkedAll(t, [2]*Node{atelier, orchard})
	orchard.mu.Lock()
	lost := config.KeyFingerprint(orchard.me.Leaf)
	orchard.mu.Unlock()
	if _, err := atelier.rotateKey([]string{"orchard"}, time.Hour); err != nil {
		t.Fatal(err)
	}
	r := reissueOf(atelier)
	for _, c := range r.Chains {
		if c.MachineID == "orchard" {
			t.Fatal("the excluded machine was issued a certificate")
		}
	}
	atelier.mu.Lock()
	defer atelier.mu.Unlock()
	if !atelier.cfg.IsRemoved("orchard") || !atelier.revokedLocked()[lost] {
		t.Fatal("the excluded machine is not removed and revoked")
	}
}

// A re-issue must come from a member whose key is on record: a made-up one is refused and changes
// nothing.
func TestReissueFromAStrangerIsRefused(t *testing.T) {
	newMeshDir(t)
	atelier, _ := meshNode(t, "atelier")
	stranger, _ := meshNode(t, "stranger") // a valid fleet certificate, but atelier has never met it
	if _, err := stranger.rotateKey(nil, time.Hour); err != nil {
		t.Fatal(err)
	}
	r := *reissueOf(stranger)
	if err := atelier.applyReissue(r, false); err == nil {
		t.Fatal("a re-issue signed by an unknown machine was accepted")
	}
	r.Signer = string(atelier.me.ChainPEM()) // claim atelier signed it: the signature no longer matches
	atelier.mu.Lock()
	atelier.cfg.MemberKeys = map[string]config.MemberKey{"atelier": {Key: config.KeyFingerprint(atelier.me.Leaf)}}
	atelier.mu.Unlock()
	if err := atelier.applyReissue(r, false); err == nil {
		t.Fatal("a re-issue with a forged signer was accepted")
	}
	roots, _ := config.Roots(atelier.opts.Dir)
	if len(roots) != 1 || reissueOf(atelier) != nil {
		t.Fatal("a refused re-issue changed the trusted roots")
	}
	if _, err := os.Stat(filepath.Join(atelier.opts.Dir, config.KeyFile)); err != nil {
		t.Fatal("refusing a re-issue should not touch the legacy key")
	}
}
