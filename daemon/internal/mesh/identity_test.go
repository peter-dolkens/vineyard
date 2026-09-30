package mesh

import (
	"crypto/tls"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/peter-dolkens/vineyard/daemon/internal/config"
	"github.com/peter-dolkens/vineyard/daemon/internal/protocol"
)

// A machine with a valid fleet certificate cannot pass itself off as another: evil answers at the
// address atelier has for forge and says it is forge, but its certificate names evil.
func TestImpersonationIsRefused(t *testing.T) {
	newMeshDir(t)
	evil, evilAddr := meshNode(t, "evil")
	evil.cfg.MachineID = "forge" // what its hellos will claim
	atelier, _ := meshNode(t, "atelier", protocol.PeerAddr{MachineID: "forge", Addr: evilAddr})
	runNodes(t, evil, atelier)
	atelier.setWantFleet(true)
	time.Sleep(1500 * time.Millisecond)
	if linkVia(atelier, "forge") != "" {
		t.Fatal("atelier linked to a machine whose certificate names someone else")
	}
}

// makeDescendant re-signs child's key with parent's, as if parent had invited it.
func makeDescendant(t *testing.T, parent, child *Node) {
	t.Helper()
	chain, err := parent.me.Issue(child.cfg.MachineID, child.me.Leaf.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := config.WriteMachine(child.opts.Dir, nil, chain); err != nil {
		t.Fatal(err)
	}
	if err := child.reloadIdentity(); err != nil {
		t.Fatal(err)
	}
}

func threeWithDescendant(t *testing.T) (atelier, forge, orchard *Node) {
	t.Helper()
	newMeshDir(t)
	orchard, orchardAddr := meshNode(t, "orchard")
	forge, forgeAddr := meshNode(t, "forge", protocol.PeerAddr{MachineID: "orchard", Addr: orchardAddr})
	atelier, atelierAddr := meshNode(t, "atelier", protocol.PeerAddr{MachineID: "forge", Addr: forgeAddr}, protocol.PeerAddr{MachineID: "orchard", Addr: orchardAddr})
	orchard.cfg.AddPeer(protocol.PeerAddr{MachineID: "atelier", Addr: atelierAddr})
	makeDescendant(t, forge, orchard) // orchard joined through forge
	runNodes(t, orchard, forge, atelier)
	atelier.setWantFleet(true)
	orchard.setWantFleet(true)
	linkedAll(t, [2]*Node{atelier, forge}, [2]*Node{atelier, orchard})
	if got := atelier.descendants("forge"); len(got) != 1 || got[0] != "orchard" {
		t.Fatalf("descendants of forge: %v", got)
	}
	return atelier, forge, orchard
}

// Removing forge revokes its key, and with it every certificate it signed: orchard, which joined
// through forge, is cut off too.
func TestRemovalRevokesTheKeyAndWhatItSigned(t *testing.T) {
	atelier, _, orchard := threeWithDescendant(t)
	if _, err := atelier.handleLocal(protocol.Request{Op: "removepeer", Args: []byte(`{"machineId":"forge"}`)}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "orchard's link to close", func() bool { return linkVia(atelier, "orchard") == "" })
	time.Sleep(1500 * time.Millisecond)
	if linkVia(atelier, "orchard") != "" {
		t.Fatal("a machine vouched for by a removed one was let back in")
	}
	_ = orchard
}

// Kept when forge is removed: atelier vouches for orchard, orchard switches to a certificate signed
// by atelier and stays in the fleet.
func TestKeptDescendantIsVouchedForAndStays(t *testing.T) {
	atelier, _, orchard := threeWithDescendant(t)
	if _, err := atelier.handleLocal(protocol.Request{Op: "removepeer", Args: []byte(`{"machineId":"forge","keep":["orchard"]}`)}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "orchard to take atelier's vouch", func() bool {
		orchard.mu.Lock()
		defer orchard.mu.Unlock()
		return len(orchard.me.Chain) > 1 && config.CertMachineID(orchard.me.Chain[1]) == "atelier"
	})
	// The same vouch can arrive by several paths at once; installing it again, concurrently, must
	// leave machine.crt whole (it once raced into an empty file and a revoked certificate on the wire).
	atelier.mu.Lock()
	var v protocol.Vouch
	for _, x := range atelier.cfg.Vouches {
		if x.MachineID == "orchard" {
			v = x
		}
	}
	atelier.mu.Unlock()
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := orchard.installVouch(v); err != nil {
				t.Errorf("installing the vouch again: %v", err)
			}
		}()
	}
	wg.Wait()
	if _, err := config.LoadMachine(orchard.opts.Dir); err != nil {
		t.Fatalf("machine.crt after concurrent installs: %v", err)
	}
	waitFor(t, "orchard back in with its new certificate", func() bool { return linkVia(atelier, "orchard") == "direct" })
}

// A machine cannot remove the machine it was vouched for by: that would lock itself out.
func TestRemovingYourOwnVoucherIsRefused(t *testing.T) {
	newMeshDir(t)
	forge, _ := meshNode(t, "forge")
	orchard, _ := meshNode(t, "orchard")
	makeDescendant(t, forge, orchard)
	orchard.mu.Lock()
	orchard.cfg.MemberKeys = map[string]config.MemberKey{"forge": {Key: config.KeyFingerprint(forge.me.Leaf)}}
	orchard.mu.Unlock()
	if _, err := orchard.handleLocal(protocol.Request{Op: "removepeer", Args: []byte(`{"machineId":"forge"}`)}); err == nil {
		t.Fatal("orchard removed the machine its own certificate depends on")
	}
}

// oldDaemonHello connects the way a daemon from before 0.3.23 does, with the shared certificate, and
// reports whether the node answered its hello.
func oldDaemonHello(t *testing.T, addr string) bool {
	t.Helper()
	certPEM, _ := os.ReadFile(config.Path(config.CertFile))
	keyPEM, _ := os.ReadFile(config.Path(config.KeyFile))
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	roots, _ := config.ParseCerts(certPEM)
	pool := x509Pool(roots)
	c, err := tls.Dial("tcp", addr, &tls.Config{Certificates: []tls.Certificate{pair}, RootCAs: pool, ServerName: config.FleetServerName, MinVersion: tls.VersionTLS13})
	if err != nil {
		return false
	}
	conn := NewConn(c)
	defer conn.Close()
	_ = conn.Send(protocol.Hello{T: "hello", Role: "peer", MachineID: "oldbox", Protocol: protocol.Version})
	b, err := conn.Recv(2 * time.Second)
	if err != nil {
		return false
	}
	var h protocol.Hello
	return json.Unmarshal(b, &h) == nil && h.T == "hello"
}

// During the grace period a daemon from before 0.3.23 is still accepted with the shared certificate;
// after it, it is refused.
func TestSharedCertificateOnlyDuringGrace(t *testing.T) {
	newMeshDir(t)
	atelier, addr := meshNode(t, "atelier")
	runNodes(t, atelier)
	if !oldDaemonHello(t, addr) {
		t.Fatal("an old daemon was refused during the grace period")
	}
	atelier.mu.Lock()
	atelier.cfg.LegacyUntil = time.Now().Add(-time.Minute).UnixMilli()
	atelier.mu.Unlock()
	if oldDaemonHello(t, addr) {
		t.Fatal("an old daemon was accepted after the grace period")
	}
}

// Once every member has presented its own certificate, the grace period ends early and fleet.key,
// which could mint any identity, is deleted.
func TestSharedKeyRetiresWhenEveryMemberHasItsOwn(t *testing.T) {
	newMeshDir(t)
	orchard, orchardAddr := meshNode(t, "orchard")
	atelier, _ := meshNode(t, "atelier", protocol.PeerAddr{MachineID: "orchard", Addr: orchardAddr})
	if _, err := os.Stat(filepath.Join(atelier.opts.Dir, config.KeyFile)); err != nil {
		t.Fatal("migration should keep fleet.key until the fleet has moved")
	}
	runNodes(t, orchard, atelier)
	atelier.setWantFleet(true)
	linkedAll(t, [2]*Node{atelier, orchard})
	waitFor(t, "atelier to retire the shared key", func() bool {
		_, err := os.Stat(filepath.Join(atelier.opts.Dir, config.KeyFile))
		return os.IsNotExist(err)
	})
	atelier.mu.Lock()
	defer atelier.mu.Unlock()
	if atelier.cfg.LegacyUntil != 0 {
		t.Fatal("grace period still running after retirement")
	}
}

// Joining with an invite: the joiner keeps its private key; the inviter signs its public key.
func TestJoinIssuesACertificateAndNoKey(t *testing.T) {
	newMeshDir(t)
	atelier, _ := meshNode(t, "atelier")
	runNodes(t, atelier)
	code, err := atelier.CreateInvite()
	if err != nil {
		t.Fatal(err)
	}
	inv, err := DecodeInvite(code)
	if err != nil {
		t.Fatal(err)
	}
	key, _, err := config.NewMachineKey()
	if err != nil {
		t.Fatal(err)
	}
	pub, _ := config.PublicKeyPEM(&key.PublicKey)
	j, _, err := JoinFleet(inv, "newbie", "newbie", "", string(pub))
	if err != nil {
		t.Fatal(err)
	}
	if j.Key != "" {
		t.Fatal("the invite handed out a private key")
	}
	certs, err := config.ParseCerts([]byte(j.Chain))
	if err != nil {
		t.Fatal(err)
	}
	if config.CertMachineID(certs[0]) != "newbie" || !key.PublicKey.Equal(certs[0].PublicKey) {
		t.Fatalf("issued certificate names %q for another key", config.CertMachineID(certs[0]))
	}
	if certs[0].CheckSignatureFrom(atelier.me.Leaf) != nil {
		t.Fatal("the joiner's certificate is not signed by the inviter")
	}
	if got := atelier.descendants("atelier"); len(got) != 1 || got[0] != "newbie" {
		t.Fatalf("the inviter does not count the joiner as vouched for: %v", got)
	}
	// A join without a public key (an old joiner) is refused rather than handed the shared key.
	code2, _ := atelier.CreateInvite()
	inv2, _ := DecodeInvite(code2)
	if _, _, err := JoinFleet(inv2, "oldjoiner", "oldjoiner", "", ""); err == nil {
		t.Fatal("a join without a public key was accepted")
	}
}

// A member cannot re-parent a machine on a whim: a vouch for a machine whose chain is not revoked is
// held back, not installed.
func TestUnneededVouchIsNotInstalled(t *testing.T) {
	newMeshDir(t)
	orchard, orchardAddr := meshNode(t, "orchard")
	atelier, _ := meshNode(t, "atelier", protocol.PeerAddr{MachineID: "orchard", Addr: orchardAddr})
	runNodes(t, orchard, atelier)
	atelier.setWantFleet(true)
	linkedAll(t, [2]*Node{atelier, orchard})
	v, err := atelier.vouchFor("orchard")
	if err != nil {
		t.Fatal(err)
	}
	orchard.handleVouchMsg(v, false)
	time.Sleep(300 * time.Millisecond)
	orchard.mu.Lock()
	defer orchard.mu.Unlock()
	if len(orchard.me.Chain) != 1 {
		t.Fatal("an unneeded vouch replaced orchard's certificate")
	}
	if orchard.pendingVouch == nil {
		t.Fatal("the vouch should be held until orchard's chain is revoked")
	}
}
