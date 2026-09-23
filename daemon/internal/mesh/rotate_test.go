package mesh

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/peter-dolkens/vineyard/daemon/internal/config"
	"github.com/peter-dolkens/vineyard/daemon/internal/model"
	"github.com/peter-dolkens/vineyard/daemon/internal/protocol"
)

// keyedNode is meshNode with its own key directory (a copy of the fleet key in VINEYARD_DIR), so a
// rotation on one daemon does not touch the others' files.
func keyedNode(t *testing.T, id string, peers ...protocol.PeerAddr) (*Node, string, string) {
	t.Helper()
	dir := t.TempDir()
	for _, f := range []string{config.CertFile, config.KeyFile} {
		b, err := os.ReadFile(config.Path(f))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, f), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	addr := "127.0.0.1:" + strconv.Itoa(freePort(t))
	cfg := config.New(id, id, 0, addr)
	cfg.Listen = addr
	cfg.Peers = peers
	cfg.Uplink = "off"
	n, err := New(Options{Config: cfg, Version: "test", Dir: dir, Log: log.New(os.Stderr, id+" ", 0), Collect: func() model.Snapshot { return model.Snapshot{} }})
	if err != nil {
		t.Fatal(err)
	}
	return n, addr, dir
}

func keyAtOf(n *Node) int64 {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.cfg.KeyAt
}

// Rotating on atelier reaches forge at once over their link; orchard, offline at the time, still
// connects on the old key during the grace period and is handed the new one.
func TestRotationReachesConnectedAndLateMembers(t *testing.T) {
	newMeshDir(t)
	original, _ := os.ReadFile(config.Path(config.CertFile))
	orchard, orchardAddr, orchardDir := keyedNode(t, "orchard")
	forge, forgeAddr, forgeDir := keyedNode(t, "forge")
	atelier, _, _ := keyedNode(t, "atelier", protocol.PeerAddr{MachineID: "forge", Addr: forgeAddr}, protocol.PeerAddr{MachineID: "orchard", Addr: orchardAddr})
	runNodes(t, forge, atelier)
	atelier.setWantFleet(true)
	waitFor(t, "atelier linked to forge", func() bool { return linkVia(atelier, "forge") == "direct" })

	at, err := atelier.rotateKey(nil, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "forge to take the new key", func() bool { return keyAtOf(forge) == at })
	if b, _ := os.ReadFile(filepath.Join(forgeDir, config.CertFile)); bytes.Equal(b, original) {
		t.Fatal("forge's fleet.crt was not replaced")
	}

	runNodes(t, orchard) // was offline during the rotation; still on the original key
	waitFor(t, "orchard to connect on the old key and be handed the new one", func() bool { return keyAtOf(orchard) == at })
	if b, _ := os.ReadFile(filepath.Join(orchardDir, config.CertFile)); bytes.Equal(b, original) {
		t.Fatal("orchard's fleet.crt was not replaced")
	}
	// Two machines that both rotated (one of them late) connect with the new key.
	orchard.mu.Lock()
	orchard.peers["forge"] = &peerState{id: "forge", addr: forgeAddr, addrs: []string{forgeAddr}}
	orchard.mu.Unlock()
	orchard.setWantFleet(true)
	waitFor(t, "orchard and forge linked", func() bool { return linkVia(orchard, "forge") == "direct" })
}

// Once the grace period is over, a machine still on the previous key is refused.
func TestAfterGraceThePreviousKeyIsRefused(t *testing.T) {
	newMeshDir(t)
	orchard, _, _ := keyedNode(t, "orchard")
	atelier, atelierAddr, atelierDir := keyedNode(t, "atelier")
	if _, err := atelier.rotateKey(nil, time.Hour); err != nil {
		t.Fatal(err)
	}
	atelier.mu.Lock()
	atelier.cfg.PrevUntil = time.Now().Add(-time.Second).UnixMilli()
	atelier.mu.Unlock()
	atelier.graceTick()
	if _, err := os.Stat(filepath.Join(atelierDir, config.PrevCertFile)); err == nil {
		t.Fatal("previous certificate kept after grace")
	}
	runNodes(t, orchard, atelier)
	orchard.mu.Lock()
	orchard.peers["atelier"] = &peerState{id: "atelier", addr: atelierAddr, addrs: []string{atelierAddr}}
	orchard.mu.Unlock()
	orchard.setWantFleet(true)
	waitFor(t, "orchard's dial to fail", func() bool {
		orchard.mu.Lock()
		defer orchard.mu.Unlock()
		p := orchard.peers["atelier"]
		return p.lastErr != "" && p.link == nil
	})
	if keyAtOf(orchard) != 0 {
		t.Fatal("a machine past grace was handed the new key")
	}
}

// An excluded machine is removed from the fleet and never handed the new key.
func TestRotationExcludesTheLostMachine(t *testing.T) {
	newMeshDir(t)
	forge, forgeAddr, _ := keyedNode(t, "forge")
	atelier, _, _ := keyedNode(t, "atelier", protocol.PeerAddr{MachineID: "forge", Addr: forgeAddr})
	runNodes(t, forge, atelier)
	atelier.setWantFleet(true)
	waitFor(t, "atelier linked to forge", func() bool { return linkVia(atelier, "forge") == "direct" })
	if _, err := atelier.rotateKey([]string{"forge"}, time.Hour); err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond)
	atelier.mu.Lock()
	removed := atelier.cfg.IsRemoved("forge")
	atelier.mu.Unlock()
	if !removed || keyAtOf(forge) != 0 {
		t.Fatalf("forge removed=%v keyAt=%d", removed, keyAtOf(forge))
	}
}

// A machine that was not a member when the key was rotated (a thief who renamed an excluded laptop)
// connects on the old key during grace but is never handed the new one.
func TestRotationDoesNotHandTheKeyToStrangers(t *testing.T) {
	newMeshDir(t)
	atelier, atelierAddr, _ := keyedNode(t, "atelier")
	if _, err := atelier.rotateKey([]string{"forge"}, time.Hour); err != nil {
		t.Fatal(err)
	}
	thief, _, _ := keyedNode(t, "forge-renamed")
	runNodes(t, atelier, thief)
	thief.mu.Lock()
	thief.peers["atelier"] = &peerState{id: "atelier", addr: atelierAddr, addrs: []string{atelierAddr}}
	thief.mu.Unlock()
	thief.setWantFleet(true)
	waitFor(t, "the thief to connect on the old key", func() bool { return linkVia(thief, "atelier") == "direct" })
	time.Sleep(500 * time.Millisecond)
	if keyAtOf(thief) != 0 {
		t.Fatal("a machine outside the rotation's member list was handed the new key")
	}
}

// A pushed key set that is not signed by a key we trust is refused before anything is written.
func TestMadeUpKeySetIsRefused(t *testing.T) {
	newMeshDir(t)
	victim, _, dir := keyedNode(t, "atelier")
	before, _ := os.ReadFile(filepath.Join(dir, config.CertFile))
	// A key set from an unrelated fleet: its cross certificate chains to that fleet's key, not ours.
	other := t.TempDir()
	t.Setenv("VINEYARD_DIR", other)
	if err := config.GenerateFleetCert(); err != nil {
		t.Fatal(err)
	}
	oc, _ := os.ReadFile(filepath.Join(other, config.CertFile))
	ok, _ := os.ReadFile(filepath.Join(other, config.KeyFile))
	cert, key, cross, err := config.NewFleetKey(oc, ok, time.Now().UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	ks := protocol.KeySet{Cert: string(cert), Key: string(key), Cross: string(cross), KeyAt: time.Now().UnixMilli(), PrevUntil: time.Now().Add(time.Hour).UnixMilli()}
	if err := victim.validateKeySet(ks); err == nil {
		t.Fatal("a key set from another fleet validated")
	}
	victim.handleRekey(&link{role: "peer", peerID: "mallory"}, ks)
	after, _ := os.ReadFile(filepath.Join(dir, config.CertFile))
	if !bytes.Equal(before, after) || keyAtOf(victim) != 0 {
		t.Fatal("a refused key set was installed")
	}
	bad := ks
	bad.Key = "not a key"
	if victim.validateKeySet(bad) == nil {
		t.Fatal("a malformed key set validated")
	}
}

// Rotating again inside a grace period: a machine left on the middle key and one still on the
// original key both still connect to the newest, and catch up to it.
func TestOverlappingRotationsKeepEveryGraceKeyWorking(t *testing.T) {
	newMeshDir(t)
	a, aAddr, _ := keyedNode(t, "atelier")
	middle, _, _ := keyedNode(t, "forge")
	oldest, _, _ := keyedNode(t, "orchard")
	a.mu.Lock()
	a.peers["forge"] = &peerState{id: "forge"}
	a.peers["orchard"] = &peerState{id: "orchard"}
	a.mu.Unlock()
	k2, err := a.rotateKey(nil, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	ks2, err := a.keySet()
	if err != nil {
		t.Fatal(err)
	}
	if err := middle.validateKeySet(ks2); err != nil {
		t.Fatal(err)
	}
	if err := middle.installKeySet(ks2); err != nil { // forge took K2, then went offline
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	k3, err := a.rotateKey(nil, time.Hour)
	if err != nil || k3 <= k2 {
		t.Fatal(err)
	}
	runNodes(t, a, middle, oldest)
	for _, n := range []*Node{middle, oldest} {
		n.mu.Lock()
		n.peers["atelier"] = &peerState{id: "atelier", addr: aAddr, addrs: []string{aAddr}}
		n.mu.Unlock()
		n.setWantFleet(true)
	}
	waitFor(t, "forge (on K2) to reach K3", func() bool { return keyAtOf(middle) == k3 })
	waitFor(t, "orchard (on K1) to reach K3", func() bool { return keyAtOf(oldest) == k3 })
}
