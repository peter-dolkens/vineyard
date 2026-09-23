package mesh

import (
	"crypto/tls"
	"errors"
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
	ks := protocol.KeySet{KeyAt: n.cfg.KeyAt, PrevUntil: n.cfg.PrevUntil}
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

// installKeySet takes a key set (our own rotation, or one pushed to us) and switches to it.
func (n *Node) installKeySet(ks protocol.KeySet) error {
	if err := config.InstallKeys(n.opts.Dir, []byte(ks.Cert), []byte(ks.Key), []byte(ks.Cross), []byte(ks.Prev)); err != nil {
		return err
	}
	n.mu.Lock()
	n.cfg.KeyAt = ks.KeyAt
	n.cfg.PrevUntil = ks.PrevUntil
	n.mu.Unlock()
	if err := n.cfg.Save(); err != nil {
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
	ks := protocol.KeySet{Cert: string(cert), Key: string(key), Cross: string(cross), Prev: string(prevCert), KeyAt: now, PrevUntil: now + grace.Milliseconds()}
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

// sendRekey pushes our key set to a peer that connected with an older key.
func (n *Node) sendRekey(l *link) {
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
	if err := n.cfg.Save(); err != nil {
		n.logf("save config: %v", err)
	}
	config.RemoveGraceFiles(n.opts.Dir)
	if err := n.rebuildTLS(); err != nil {
		n.logf("reload fleet key: %v", err)
	}
	n.logf("key rotation grace period over; the previous fleet key is no longer accepted")
}
