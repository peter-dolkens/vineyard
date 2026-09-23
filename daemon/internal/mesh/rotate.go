package mesh

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"slices"
	"time"

	"github.com/peter-dolkens/vineyard/daemon/internal/config"
	"github.com/peter-dolkens/vineyard/daemon/internal/protocol"
)

// Fleet key rotation (certificates: config/keys.go). "Rotate Fleet Key" makes a new key here,
// optionally removing the machines it must not reach (a lost laptop), and pushes the key set over
// every live peer link. From then on every hello states the sender's KeyAt, and whichever side of a
// new connection holds the newer key pushes it to the other, so machines that were offline catch up
// the next time they meet any rotated member, as long as that is within the grace period. After it,
// the previous key is no longer trusted and a straggler has to join again with an invite.

const DefaultRotationGrace = 14 * 24 * time.Hour

// setTLSLocked installs freshly built configurations.
func (n *Node) setTLSLocked(server, client *tls.Config) {
	n.tlsStrict = server
	n.tlsLenient = lenientFor(server)
	n.tlsClient = client
	n.tlsGrace = inGrace(n.opts.Dir, n.cfg.PrevUntil)
}

func (n *Node) client() *tls.Config {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.tlsClient
}

func (n *Node) strict() *tls.Config {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.tlsStrict
}

// rebuildTLS reloads the key files; existing links are unaffected, new handshakes use the result.
func (n *Node) rebuildTLS() error {
	n.mu.Lock()
	prevUntil := n.cfg.PrevUntil
	n.mu.Unlock()
	srv, cli, err := fleetTLS(n.opts.Dir, prevUntil)
	if err != nil {
		return err
	}
	n.mu.Lock()
	n.setTLSLocked(srv, cli)
	n.mu.Unlock()
	return nil
}

// keySet reads the key files to pass on: the current key, plus the previous and cross certificates
// while the grace period runs.
func (n *Node) keySet() (protocol.KeySet, error) {
	read := func(name string) string {
		b, _ := os.ReadFile(config.PathIn(n.opts.Dir, name))
		return string(b)
	}
	n.mu.Lock()
	ks := protocol.KeySet{KeyAt: n.cfg.KeyAt, PrevUntil: n.cfg.PrevUntil, Members: slices.Clone(n.cfg.KeyMembers)}
	grace := n.tlsGrace
	n.mu.Unlock()
	ks.Cert, ks.Key = read(config.CertFile), read(config.KeyFile)
	if ks.Cert == "" || ks.Key == "" {
		return ks, errors.New("fleet key files missing")
	}
	if grace {
		ks.Cross, ks.Prev = read(config.CrossCertFile), read(config.PrevCertFile)
	} else {
		ks.PrevUntil = 0
	}
	return ks, nil
}

// validateKeySet checks a key set pushed to us before anything is written: the key must match its
// certificates, and the cross certificate must chain to a key we already trust. That keeps a
// malformed set from being installed (and spread), and a machine that has rotated from being handed
// a key someone made up.
func (n *Node) validateKeySet(ks protocol.KeySet) error {
	if _, err := tls.X509KeyPair([]byte(ks.Cert), []byte(ks.Key)); err != nil {
		return fmt.Errorf("certificate and key do not match: %w", err)
	}
	if ks.Cross == "" {
		return errors.New("no certificate linking the new key to ours")
	}
	pair, err := tls.X509KeyPair([]byte(ks.Cross), []byte(ks.Key))
	if err != nil {
		return fmt.Errorf("cross certificate: %w", err)
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return err
	}
	inter := x509.NewCertPool()
	for _, der := range pair.Certificate[1:] {
		if c, err := x509.ParseCertificate(der); err == nil {
			inter.AddCert(c)
		}
	}
	roots := x509.NewCertPool()
	for _, f := range []string{config.CertFile, config.PrevCertFile} {
		if b, err := os.ReadFile(config.PathIn(n.opts.Dir, f)); err == nil {
			roots.AppendCertsFromPEM(b)
		}
	}
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: roots, Intermediates: inter, DNSName: config.FleetServerName, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny}}); err != nil {
		return fmt.Errorf("the new key is not signed by a key we trust: %w", err)
	}
	return nil
}

// installKeySet takes a key set (our own rotation, or one pushed to us) and switches to it.
func (n *Node) installKeySet(ks protocol.KeySet) error {
	if err := config.InstallKeys(n.opts.Dir, []byte(ks.Cert), []byte(ks.Key), []byte(ks.Cross), []byte(ks.Prev)); err != nil {
		return err
	}
	n.mu.Lock()
	n.cfg.KeyAt = ks.KeyAt
	n.cfg.PrevUntil = ks.PrevUntil
	n.cfg.KeyMembers = ks.Members
	n.mu.Unlock()
	if err := n.saveConfig(); err != nil {
		n.logf("save config: %v", err)
	}
	return n.rebuildTLS()
}

// rotateKey makes a new fleet key, removes the excluded machines from the fleet, and pushes the key
// to every connected member. Returns the new key's time.
func (n *Node) rotateKey(exclude []string, grace time.Duration) (int64, error) {
	if grace <= 0 {
		grace = DefaultRotationGrace
	}
	now := time.Now().UnixMilli()
	var removals []protocol.Removal
	n.mu.Lock()
	for _, id := range exclude {
		r := protocol.Removal{MachineID: id, At: now}
		if n.applyRemovalLocked(r) {
			removals = append(removals, r)
		}
	}
	n.mu.Unlock()
	prevCert, err := os.ReadFile(config.PathIn(n.opts.Dir, config.CertFile))
	if err != nil {
		return 0, err
	}
	prevKey, err := os.ReadFile(config.PathIn(n.opts.Dir, config.KeyFile))
	if err != nil {
		return 0, err
	}
	cert, key, cross, err := config.NewFleetKey(prevCert, prevKey, now)
	if err != nil {
		return 0, err
	}
	prev := prevCert
	n.mu.Lock()
	overlapping := n.tlsGrace
	members := []string{n.cfg.MachineID}
	for id := range n.peers {
		if !slices.Contains(exclude, id) {
			members = append(members, id)
		}
	}
	n.mu.Unlock()
	slices.Sort(members)
	if overlapping {
		// Rotating again inside a grace period: machines still on the key before last must be able
		// to verify the new one too, so the old bridge certificates come along.
		if b, err := os.ReadFile(config.PathIn(n.opts.Dir, config.CrossCertFile)); err == nil {
			cross = append(cross, b...)
		}
		if b, err := os.ReadFile(config.PathIn(n.opts.Dir, config.PrevCertFile)); err == nil {
			prev = append(append([]byte(nil), prevCert...), b...)
		}
	}
	ks := protocol.KeySet{Cert: string(cert), Key: string(key), Cross: string(cross), Prev: string(prev), KeyAt: now, PrevUntil: now + grace.Milliseconds(), Members: members}
	if err := n.installKeySet(ks); err != nil {
		return 0, err
	}
	n.mu.Lock()
	var peerLinks []*link
	for l := range n.links {
		if l.role == "peer" && !slices.Contains(exclude, l.peerID) {
			peerLinks = append(peerLinks, l)
		}
	}
	n.mu.Unlock()
	for _, l := range peerLinks {
		if len(removals) > 0 {
			_ = l.conn.Send(protocol.Removed{T: "removed", Removals: removals})
		}
		_ = l.conn.Send(protocol.Rekey{T: "rekey", Keys: ks})
	}
	if len(removals) > 0 {
		n.refreshViewers()
	}
	n.logf("fleet key rotated; the previous key is accepted until %s; told %d connected members", time.UnixMilli(ks.PrevUntil).Format(time.DateTime), len(peerLinks))
	return now, nil
}

// sendRekey pushes our key set to a peer that connected with an older key, if the rotation listed it.
func (n *Node) sendRekey(l *link) {
	n.mu.Lock()
	listed := slices.Contains(n.cfg.KeyMembers, l.peerID)
	n.mu.Unlock()
	if !listed {
		n.logf("not passing the fleet key to %s: it was not a member when the key was rotated", l.peerID)
		return
	}
	ks, err := n.keySet()
	if err != nil {
		n.logf("cannot pass the fleet key to %s: %v", l.peerID, err)
		return
	}
	if err := l.conn.Send(protocol.Rekey{T: "rekey", Keys: ks}); err == nil {
		n.logf("passed the current fleet key to %s", l.peerID)
	}
}

// handleRekey takes a newer key set from a peer.
func (n *Node) handleRekey(l *link, ks protocol.KeySet) {
	n.mu.Lock()
	newer := ks.KeyAt > n.cfg.KeyAt
	n.mu.Unlock()
	if !newer || l.role != "peer" {
		return
	}
	if err := n.validateKeySet(ks); err != nil {
		n.logf("refused a fleet key from %s: %v", l.peerID, err)
		return
	}
	if err := n.installKeySet(ks); err != nil {
		n.logf("fleet key from %s: %v", l.peerID, err)
		return
	}
	n.logf("switched to the fleet key rotated at %s (from %s)", time.UnixMilli(ks.KeyAt).Format(time.DateTime), l.peerID)
}

// graceTick ends a rotation grace period once it is over: the previous key stops being trusted.
func (n *Node) graceTick() {
	n.mu.Lock()
	over := n.cfg.PrevUntil != 0 && time.Now().UnixMilli() > n.cfg.PrevUntil
	if over {
		n.cfg.PrevUntil = 0
	}
	n.mu.Unlock()
	if !over {
		return
	}
	if err := n.saveConfig(); err != nil {
		n.logf("save config: %v", err)
	}
	config.RemoveGraceFiles(n.opts.Dir)
	if err := n.rebuildTLS(); err != nil {
		n.logf("reload fleet key: %v", err)
	}
	n.logf("key rotation grace period over; the previous fleet key is no longer accepted")
}
