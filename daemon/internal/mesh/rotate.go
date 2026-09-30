package mesh

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/peter-dolkens/vineyard/daemon/internal/config"
	"github.com/peter-dolkens/vineyard/daemon/internal/protocol"
)

// TLS configurations and fleet root rotation. The pre-0.3.23 rotation, which pushed a new shared
// private key to every member, is gone; graceTick still ends the grace period of one made before.

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

// ---- re-issue: Rotate Fleet Key from 0.3.23 -------------------------------------------------------
//
// A rotation no longer hands anyone a private key. The rotating machine makes a new root, signs with
// it a fresh certificate for every current member's existing key (the excluded machines are removed
// first), throws the root key away and sends out the result: the new root, the certificates, its own
// signature. Members add the root, switch to their new certificate and pass the package on in hellos,
// so machines offline at the time catch up within the grace period. After it, only the new root is
// trusted: certificates under the old one, including anything minted with the pre-0.3.23 shared key,
// stop working.

// reissueDigest is what the rotating machine signs.
func reissueDigest(r protocol.Reissue) []byte {
	r.Signer, r.Sig = "", ""
	b, _ := json.Marshal(r)
	h := sha256.Sum256(b)
	return h[:]
}

// rotateKey is the "rotatekey" op: re-issue every member's certificate under a new root.
func (n *Node) rotateKey(exclude []string, grace time.Duration) (int64, error) {
	if grace <= 0 {
		grace = DefaultRotationGrace
	}
	n.mu.Lock()
	me := n.me
	n.mu.Unlock()
	if me == nil {
		return 0, errors.New("this machine has no certificate of its own yet; it cannot rotate the fleet key")
	}
	now := time.Now().UnixMilli()
	// The excluded machines go first, their keys revoked, so they get nothing.
	var removals []protocol.Removal
	n.mu.Lock()
	for _, id := range exclude {
		r := protocol.Removal{MachineID: id, At: now}
		if k := n.cfg.MemberKeys[id].Key; k != "" {
			r.Keys = []string{k}
		}
		if n.applyRemovalLocked(r) {
			removals = append(removals, r)
		}
	}
	members := map[string]string{}
	for id, mk := range n.cfg.MemberKeys {
		if !slices.Contains(exclude, id) && !n.cfg.IsRemoved(id) && mk.Pub != "" {
			members[id] = mk.Pub
		}
	}
	n.mu.Unlock()

	rootPEM, rootKey, err := config.NewRoot(time.UnixMilli(now).UTC().Format("2006-01-02 15:04"))
	if err != nil {
		return 0, err
	}
	roots, err := config.ParseCerts(rootPEM)
	if err != nil {
		return 0, err
	}
	r := protocol.Reissue{Root: string(rootPEM), At: now, Until: now + grace.Milliseconds()}
	issue := func(id string, pub any) error {
		der, err := config.IssueMachineCert(roots[0], rootKey, id, pub)
		if err != nil {
			return err
		}
		r.Chains = append(r.Chains, protocol.Vouch{MachineID: id, Chain: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), At: now})
		return nil
	}
	if err := issue(n.cfg.MachineID, me.Leaf.PublicKey); err != nil {
		return 0, err
	}
	ids := make([]string, 0, len(members))
	for id := range members {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		der, err := base64.StdEncoding.DecodeString(members[id])
		if err != nil {
			continue
		}
		pub, err := x509.ParsePKIXPublicKey(der)
		if err != nil {
			continue
		}
		if err := issue(id, pub); err != nil {
			return 0, err
		}
	}
	rootKey = nil // nothing keeps it: no machine can mint identities under the new root
	bridge, err := me.Bridge(roots[0])
	if err != nil {
		return 0, err
	}
	r.Bridge = string(bridge)
	r.Signer = string(me.ChainPEM())
	sig, err := me.Key.Sign(rand.Reader, reissueDigest(r), crypto.SHA256)
	if err != nil {
		return 0, err
	}
	r.Sig = base64.StdEncoding.EncodeToString(sig)
	if err := n.applyReissue(r, true); err != nil {
		return 0, err
	}

	n.mu.Lock()
	var peerLinks []*link
	for l := range n.links {
		if l.role == "peer" {
			peerLinks = append(peerLinks, l)
		}
	}
	n.mu.Unlock()
	for _, l := range peerLinks {
		if len(removals) > 0 {
			_ = l.conn.Send(protocol.Removed{T: "removed", Removals: removals})
		}
		_ = l.conn.Send(protocol.ReissueMsg{T: "reissue", Reissue: r})
	}
	if len(removals) > 0 {
		n.refreshViewers()
	}
	n.logf("fleet root re-issued for %d machines; the previous root is accepted until %s; told %d connected members", len(r.Chains), time.UnixMilli(r.Until).Format(time.DateTime), len(peerLinks))
	return now, nil
}

// checkReissue verifies a re-issue from a peer: newer than ours, signed by a member whose key we
// already hold on record, and its signer's chain valid under the roots we trust now.
func (n *Node) checkReissue(r protocol.Reissue) error {
	signer, err := config.ParseCerts([]byte(r.Signer))
	if err != nil {
		return fmt.Errorf("signer: %w", err)
	}
	id := config.CertMachineID(signer[0])
	n.mu.Lock()
	known := n.cfg.MemberKeys[id].Key
	revoked := n.revokedLocked()
	n.mu.Unlock()
	if id == "" || known == "" || known != config.KeyFingerprint(signer[0]) {
		return fmt.Errorf("signed by %q, whose key this machine has not seen", id)
	}
	for _, c := range signer {
		if revoked[config.KeyFingerprint(c)] {
			return errors.New("signed through a revoked key")
		}
	}
	trusted, err := config.Roots(n.opts.Dir)
	if err != nil {
		return err
	}
	pool, inter := x509.NewCertPool(), x509.NewCertPool()
	for _, c := range trusted {
		pool.AddCert(c)
	}
	for _, c := range signer[1:] {
		inter.AddCert(c)
	}
	if _, err := signer[0].Verify(x509.VerifyOptions{Roots: pool, Intermediates: inter, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny}}); err != nil {
		return fmt.Errorf("signer's certificate: %w", err)
	}
	sig, err := base64.StdEncoding.DecodeString(r.Sig)
	if err != nil {
		return err
	}
	pub, ok := signer[0].PublicKey.(*ecdsa.PublicKey)
	if !ok || !ecdsa.VerifyASN1(pub, reissueDigest(r), sig) {
		return errors.New("bad signature")
	}
	return nil
}

// applyReissue adds the new root, switches this machine to its new certificate and keeps the
// package to pass on until the grace period ends.
func (n *Node) applyReissue(r protocol.Reissue, ours bool) error {
	n.mu.Lock()
	cur := n.cfg.Reissue
	me := n.me
	n.mu.Unlock()
	if cur != nil && cur.At >= r.At {
		return nil
	}
	if !ours {
		if err := n.checkReissue(r); err != nil {
			return err
		}
	}
	newRoot, err := config.ParseCerts([]byte(r.Root))
	if err != nil {
		return err
	}
	rootsPath := config.PathIn(n.opts.Dir, config.CertFile)
	have, _ := os.ReadFile(rootsPath)
	if !bytes.Contains(have, []byte(strings.TrimSpace(r.Root))) {
		if err := writeFileAtomic(rootsPath, append(append([]byte(nil), have...), []byte(r.Root)...)); err != nil {
			return err
		}
	}
	for _, v := range r.Chains {
		if v.MachineID != n.cfg.MachineID || me == nil {
			continue
		}
		certs, err := config.ParseCerts([]byte(v.Chain))
		if err != nil || config.KeyFingerprint(certs[0]) != config.KeyFingerprint(me.Leaf) {
			continue
		}
		if err := certs[0].CheckSignatureFrom(newRoot[0]); err != nil {
			return fmt.Errorf("our new certificate is not signed by the new root: %w", err)
		}
		// Until the grace period ends, present the bridge behind it for machines on the old root.
		if err := config.WriteMachine(n.opts.Dir, nil, append([]byte(v.Chain), []byte(r.Bridge)...)); err != nil {
			return err
		}
	}
	n.mu.Lock()
	rr := r
	n.cfg.Reissue = &rr
	n.mu.Unlock()
	if err := n.saveConfig(); err != nil {
		n.logf("save config: %v", err)
	}
	if err := n.reloadIdentity(); err != nil {
		return err
	}
	if !ours {
		n.logf("switched to the fleet root re-issued at %s; the previous one is accepted until %s", time.UnixMilli(r.At).Format(time.DateTime), time.UnixMilli(r.Until).Format(time.DateTime))
	}
	return nil
}

// reissueTick ends a re-issue's grace period: only the newest root is trusted from then on.
func (n *Node) reissueTick() {
	n.mu.Lock()
	r := n.cfg.Reissue
	over := r != nil && time.Now().UnixMilli() > r.Until
	if over {
		n.cfg.Reissue = nil
	}
	n.mu.Unlock()
	if !over {
		return
	}
	if err := writeFileAtomic(config.PathIn(n.opts.Dir, config.CertFile), []byte(r.Root)); err != nil {
		n.logf("drop old fleet roots: %v", err)
		return
	}
	// The bridge has done its job: present the new certificate alone.
	if me, err := config.LoadMachine(n.opts.Dir); err == nil && len(me.Chain) > 1 {
		if root, err := config.ParseCerts([]byte(r.Root)); err == nil && me.Leaf.CheckSignatureFrom(root[0]) == nil {
			_ = config.WriteMachine(n.opts.Dir, nil, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: me.Leaf.Raw}))
			_ = n.reloadIdentity()
		}
	}
	if err := n.saveConfig(); err != nil {
		n.logf("save config: %v", err)
	}
	if err := n.rebuildTLS(); err != nil {
		n.logf("reload fleet roots: %v", err)
	}
	n.logf("re-issue grace period over; only the new fleet root is trusted")
}

func writeFileAtomic(path string, data []byte) error {
	return config.WriteFileAtomic(path, data)
}

// handleRekey refuses a shared fleet key pushed by a daemon from before 0.3.23: machines with their
// own certificates never take one again.
func (n *Node) handleRekey(l *link, _ protocol.KeySet) {
	n.logf("ignored a shared fleet key from %s: this fleet uses machine certificates (update vineyardd there)", l.peerID)
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
