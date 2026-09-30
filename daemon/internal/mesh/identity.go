package mesh

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/peter-dolkens/vineyard/daemon/internal/config"
	"github.com/peter-dolkens/vineyard/daemon/internal/protocol"
)

// Machine identity on the wire (certificates: config/identity.go). The TLS handshake proves the far
// end holds a key whose chain leads to a fleet root. After it, a connection is admitted only when:
//
//   - no key in its chain is revoked (a removed machine, and everything it vouched for);
//   - a peer's hello names the machine its certificate names, and a machine we dialed is the one we
//     meant to reach; a viewer holds this machine's own certificate;
//   - or, while this machine still allows it, it presents the shared certificate from before 0.3.23.
//
// Keys members present on direct links are remembered (config.MemberKeys) so a removal can revoke
// them and a removed machine's descendants can be re-vouched.

// LegacyGrace is how long a migrated machine keeps accepting the shared certificate.
const LegacyGrace = 14 * 24 * time.Hour

// identity is who is on the other end of a connection, from its verified chain.
type identity struct {
	id     string   // the machine the leaf names; "" for the shared certificate
	legacy bool     // the shared certificate from before 0.3.23
	key    string   // leaf key fingerprint
	chain  []string // key fingerprints, leaf first, root excluded
	pub    []byte   // leaf SubjectPublicKeyInfo, for re-vouching
}

// tlsStateOf finds the TLS state under the wrappers a link may sit on.
func tlsStateOf(c net.Conn) (tls.ConnectionState, bool) {
	switch v := c.(type) {
	case *tls.Conn:
		return v.ConnectionState(), true
	case bufConn:
		return tlsStateOf(v.Conn)
	case *bufConn:
		return tlsStateOf(v.Conn)
	}
	return tls.ConnectionState{}, false
}

// identityOf reads the far end's identity from a completed handshake. With several verified chains
// (two roots during a re-issue) the first is taken; revocation is checked on every one by allowed.
func identityOf(cs tls.ConnectionState) (identity, [][]*x509.Certificate, error) {
	chains := cs.VerifiedChains
	if len(chains) == 0 || len(chains[0]) == 0 {
		return identity{}, nil, errors.New("no verified certificate")
	}
	leaf := chains[0][0]
	id := identity{id: config.CertMachineID(leaf), key: config.KeyFingerprint(leaf), pub: leaf.RawSubjectPublicKeyInfo}
	c := chains[0]
	if len(c) == 1 && id.id == "" {
		id.legacy = true // the root itself, presented as a leaf: the old shared certificate
	}
	for _, x := range c[:max(1, len(c)-1)] {
		id.chain = append(id.chain, config.KeyFingerprint(x))
	}
	if id.id == "" && !id.legacy {
		id.legacy = true // a cross certificate from a pre-0.3.23 rotation names no machine either
	}
	return id, chains, nil
}

func (n *Node) revokedLocked() map[string]bool {
	out := make(map[string]bool, len(n.cfg.RevokedKeys))
	for _, k := range n.cfg.RevokedKeys {
		out[k] = true
	}
	return out
}

func (n *Node) legacyAllowedLocked() bool {
	return n.cfg.LegacyUntil > time.Now().UnixMilli()
}

// allowed checks a connection's identity against revocations and the legacy grace. It does not know
// what the far end claims to be; admit does.
func (n *Node) allowed(c net.Conn) (identity, error) {
	cs, ok := tlsStateOf(c)
	if !ok {
		return identity{}, errors.New("not a TLS connection")
	}
	id, chains, err := identityOf(cs)
	if err != nil {
		return id, err
	}
	n.mu.Lock()
	revoked := n.revokedLocked()
	legacyOK := n.legacyAllowedLocked()
	n.mu.Unlock()
	clean := false
	for _, ch := range chains {
		bad := false
		for _, x := range ch[:max(1, len(ch)-1)] {
			if revoked[config.KeyFingerprint(x)] {
				bad = true
				break
			}
		}
		if !bad {
			clean = true
			break
		}
	}
	if !clean {
		return id, errRevoked
	}
	if id.legacy && !legacyOK {
		return id, errors.New("presents the shared fleet certificate from before 0.3.23, which this fleet no longer accepts; update vineyardd on that machine or join it again with an invite")
	}
	return id, nil
}

var errRevoked = errors.New("its certificate chain includes a revoked key (a machine removed from the fleet)")

// admit decides whether a link may register, given its hello. It records the identity on the link.
func (n *Node) admit(l *link, h protocol.Hello) error {
	id, err := n.allowed(l.conn.raw)
	if errors.Is(err, errRevoked) {
		n.offerVouch(l, h.MachineID, id)
	}
	if err != nil {
		return err
	}
	l.ident = id
	if id.legacy {
		return nil
	}
	switch h.Role {
	case "viewer":
		if id.id != n.cfg.MachineID {
			return fmt.Errorf("a viewer must hold this machine's certificate; it presents %s's", id.id)
		}
	default:
		if id.id != h.MachineID {
			return fmt.Errorf("its certificate names %s but its hello claims to be %s", id.id, h.MachineID)
		}
		if l.outbound && l.peerID != "" && l.peerID != id.id {
			return fmt.Errorf("dialed %s but reached %s", l.peerID, id.id)
		}
	}
	return nil
}

// recordMemberLocked remembers the identity a member presented on a direct link. Returns true when
// config changed.
func (n *Node) recordMemberLocked(machineID string, id identity) bool {
	if id.legacy || machineID == "" {
		return false
	}
	if n.cfg.MemberKeys == nil {
		n.cfg.MemberKeys = map[string]config.MemberKey{}
	}
	prev, had := n.cfg.MemberKeys[machineID]
	mk := config.MemberKey{Key: id.key, Chain: slices.Clone(id.chain), Pub: base64.StdEncoding.EncodeToString(id.pub), Seen: time.Now().UnixMilli()}
	changed := !had || prev.Key != mk.Key || !slices.Equal(prev.Chain, mk.Chain)
	if !changed && time.Duration(mk.Seen-prev.Seen)*time.Millisecond < time.Hour {
		return false
	}
	n.cfg.MemberKeys[machineID] = mk
	// A vouch this member has now taken up (its chain is the one we carried) is no longer needed.
	n.cfg.Vouches = slices.DeleteFunc(n.cfg.Vouches, func(v protocol.Vouch) bool {
		return v.MachineID == machineID && vouchLeafKey(v) == id.key
	})
	return true
}

// revokeKeysLocked adds keys to the permanent revocation list and closes any live link whose chain
// includes one. Returns true when anything was new.
func (n *Node) revokeKeysLocked(keys []string) bool {
	added := false
	for _, k := range keys {
		if k != "" && !slices.Contains(n.cfg.RevokedKeys, k) {
			n.cfg.RevokedKeys = append(n.cfg.RevokedKeys, k)
			added = true
		}
	}
	if !added {
		return false
	}
	revoked := n.revokedLocked()
	for l := range n.links {
		for _, k := range l.ident.chain {
			if revoked[k] {
				l.conn.Close()
				break
			}
		}
	}
	for id, mk := range n.cfg.MemberKeys {
		for _, k := range mk.Chain {
			if revoked[k] {
				delete(n.cfg.MemberKeys, id) // presents a revoked chain now; re-learned from a vouch
				break
			}
		}
	}
	// Our own chain just went: the vouch sent ahead of this removal is our way back in.
	if v := n.pendingVouch; v != nil && n.ownChainRevokedLocked() {
		n.pendingVouch = nil
		go func() {
			if err := n.installVouch(*v); err != nil {
				n.logf("vouch for this machine refused: %v", err)
			}
		}()
	}
	return true
}

// descendants are the members whose certificates were vouched for, directly or not, by machineID's
// key: removing it revokes them too unless they are vouched for again.
func (n *Node) descendants(machineID string) []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	key := n.cfg.MemberKeys[machineID].Key
	if machineID == n.cfg.MachineID && n.me != nil {
		key = config.KeyFingerprint(n.me.Leaf)
	}
	if key == "" {
		return nil
	}
	var out []string
	for id, mk := range n.cfg.MemberKeys {
		if id != machineID && slices.Contains(mk.Chain[min(1, len(mk.Chain)):], key) {
			out = append(out, id)
		}
	}
	slices.Sort(out)
	return out
}

// ---- vouches -------------------------------------------------------------------------------------

func vouchLeafKey(v protocol.Vouch) string {
	certs, err := config.ParseCerts([]byte(v.Chain))
	if err != nil {
		return ""
	}
	return config.KeyFingerprint(certs[0])
}

// vouchFor re-signs a member's key with this machine's, so it survives the removal of a machine its
// chain ran through.
func (n *Node) vouchFor(machineID string) (protocol.Vouch, error) {
	n.mu.Lock()
	mk, ok := n.cfg.MemberKeys[machineID]
	me := n.me
	n.mu.Unlock()
	if !ok || mk.Pub == "" {
		return protocol.Vouch{}, fmt.Errorf("no key on record for %s", machineID)
	}
	if me == nil {
		return protocol.Vouch{}, errors.New("this machine has no certificate of its own to vouch with")
	}
	der, err := base64.StdEncoding.DecodeString(mk.Pub)
	if err != nil {
		return protocol.Vouch{}, err
	}
	pub, err := x509.ParsePKIXPublicKey(der)
	if err != nil {
		return protocol.Vouch{}, err
	}
	chain, err := me.Issue(machineID, pub)
	if err != nil {
		return protocol.Vouch{}, err
	}
	return protocol.Vouch{MachineID: machineID, Chain: string(chain), At: time.Now().UnixMilli()}, nil
}

// mergeVouchesLocked keeps the newest vouch per machine; one for this machine is installed. Returns
// whether config changed and the vouch to install, if any.
func (n *Node) mergeVouchesLocked(vs []protocol.Vouch) (bool, *protocol.Vouch) {
	changed := false
	var mine *protocol.Vouch
	for _, v := range vs {
		if v.MachineID == "" || v.Chain == "" {
			continue
		}
		if v.MachineID == n.cfg.MachineID {
			if n.me == nil || vouchLeafKey(v) != config.KeyFingerprint(n.me.Leaf) || slices.Equal(chainOf(v), chainKeys(n.me.Chain)) {
				continue
			}
			vv := v
			mine = &vv
			continue
		}
		i := slices.IndexFunc(n.cfg.Vouches, func(x protocol.Vouch) bool { return x.MachineID == v.MachineID })
		if i >= 0 {
			if n.cfg.Vouches[i].At >= v.At {
				continue
			}
			n.cfg.Vouches[i] = v
		} else {
			n.cfg.Vouches = append(n.cfg.Vouches, v)
		}
		changed = true
	}
	return changed, mine
}

func chainOf(v protocol.Vouch) []string {
	certs, err := config.ParseCerts([]byte(v.Chain))
	if err != nil {
		return nil
	}
	return chainKeys(certs)
}

func chainKeys(certs []*x509.Certificate) []string {
	out := make([]string, 0, len(certs))
	for _, c := range certs {
		out = append(out, config.KeyFingerprint(c))
	}
	return out
}

// installVouch switches this machine to a chain another member signed for it, after checking that it
// is for our own key, leads to a root we trust and runs through no revoked key.
func (n *Node) installVouch(v protocol.Vouch) error {
	certs, err := config.ParseCerts([]byte(v.Chain))
	if err != nil {
		return err
	}
	n.installMu.Lock()
	defer n.installMu.Unlock()
	n.mu.Lock()
	me := n.me
	revoked := n.revokedLocked()
	n.mu.Unlock()
	if me != nil && sameChain(me.Chain, certs) {
		return nil // already installed, by another path that delivered the same vouch
	}
	if me == nil || config.KeyFingerprint(certs[0]) != config.KeyFingerprint(me.Leaf) {
		return errors.New("the vouch is not for this machine's key")
	}
	if config.CertMachineID(certs[0]) != n.cfg.MachineID {
		return errors.New("the vouch names another machine")
	}
	for _, c := range certs {
		if revoked[config.KeyFingerprint(c)] {
			return errors.New("the vouch runs through a revoked key")
		}
	}
	roots, err := config.Roots(n.opts.Dir)
	if err != nil {
		return err
	}
	pool := x509.NewCertPool()
	for _, r := range roots {
		pool.AddCert(r)
	}
	inter := x509.NewCertPool()
	for _, c := range certs[1:] {
		inter.AddCert(c)
	}
	if _, err := certs[0].Verify(x509.VerifyOptions{Roots: pool, Intermediates: inter, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny}}); err != nil {
		return fmt.Errorf("the vouch does not lead to a fleet root: %w", err)
	}
	if err := config.WriteMachine(n.opts.Dir, nil, []byte(v.Chain)); err != nil {
		return err
	}
	if err := n.reloadIdentity(); err != nil {
		return err
	}
	n.logf("installed a new certificate for this machine, vouched for by %s", config.CertMachineID(certs[min(1, len(certs)-1)]))
	return nil
}

func sameChain(a, b []*x509.Certificate) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !a[i].Equal(b[i]) {
			return false
		}
	}
	return true
}

// offerVouch answers a connection refused for a revoked chain with a vouch we carry for it, if its
// key is the one the vouch certifies: it installs it and connects again.
func (n *Node) offerVouch(l *link, claimed string, id identity) {
	machineID := id.id
	if machineID == "" {
		machineID = claimed
	}
	n.mu.Lock()
	var v *protocol.Vouch
	for i := range n.cfg.Vouches {
		if n.cfg.Vouches[i].MachineID == machineID && vouchLeafKey(n.cfg.Vouches[i]) == id.key {
			vv := n.cfg.Vouches[i]
			v = &vv
		}
	}
	n.mu.Unlock()
	if v != nil {
		_ = l.conn.Send(protocol.VouchMsg{T: "vouch", Vouch: *v})
		time.Sleep(200 * time.Millisecond) // let it reach the far end before the close
	}
}

// handleVouchMsg takes a vouch pushed on a link: ours, or someone else's to carry. Ours is installed
// at once when it came as the answer to a refused connection (refused), or when our own chain is
// revoked already; otherwise it waits for the revocation it was sent ahead of.
func (n *Node) handleVouchMsg(v protocol.Vouch, refused bool) {
	n.mu.Lock()
	changed, mine := n.mergeVouchesLocked([]protocol.Vouch{v})
	if mine != nil && !refused && !n.ownChainRevokedLocked() {
		n.pendingVouch = mine
		mine = nil
	}
	n.mu.Unlock()
	if changed {
		if err := n.saveConfig(); err != nil {
			n.logf("save config: %v", err)
		}
	}
	if mine != nil {
		if err := n.installVouch(*mine); err != nil {
			n.logf("vouch for this machine refused: %v", err)
		}
	}
}

// ownChainRevokedLocked reports whether a key in this machine's own chain has been revoked.
func (n *Node) ownChainRevokedLocked() bool {
	if n.me == nil {
		return false
	}
	revoked := n.revokedLocked()
	for _, c := range n.me.Chain {
		if revoked[config.KeyFingerprint(c)] {
			return true
		}
	}
	return false
}

// ---- this machine's identity, migration and retirement of the shared key ----------------------------

// reloadIdentity reads machine.key / machine.crt again and rebuilds the TLS configurations.
func (n *Node) reloadIdentity() error {
	var me *config.Machine
	if config.HasMachine(n.opts.Dir) {
		m, err := config.LoadMachine(n.opts.Dir)
		if err != nil {
			return err
		}
		me = m
	}
	n.mu.Lock()
	n.me = me
	n.mu.Unlock()
	return n.rebuildTLS()
}

// migrateIdentity gives a machine of a pre-0.3.23 fleet its own certificate (offline, with the shared
// key) and starts the grace period in which peers may still present the shared certificate.
func migrateIdentity(cfg *config.Config, dir string, logf func(string, ...any)) {
	made, err := config.Migrate(dir, cfg.MachineID)
	if err != nil {
		if !config.HasMachine(dir) {
			logf("identity: %v; presenting the shared fleet certificate", err)
		}
		return
	}
	if !made {
		return
	}
	cfg.LegacyUntil = time.Now().Add(LegacyGrace).UnixMilli()
	if err := cfg.Save(); err != nil {
		logf("save config: %v", err)
	}
	logf("identity: this machine now has its own certificate; peers may present the shared one until %s", time.UnixMilli(cfg.LegacyUntil).Format(time.DateTime))
}

// maybeRetireSharedKey ends the grace period once every known member has presented its own
// certificate, or the time is up: the shared certificate is refused from then on, and fleet.key, which
// could mint any identity, is deleted.
func (n *Node) maybeRetireSharedKey() {
	n.mu.Lock()
	if n.cfg.LegacyUntil == 0 || n.me == nil {
		n.mu.Unlock()
		return
	}
	all := true
	var waiting []string
	for _, p := range n.cfg.Peers {
		if _, ok := n.cfg.MemberKeys[p.MachineID]; !ok {
			all = false
			waiting = append(waiting, p.MachineID)
		}
	}
	expired := time.Now().UnixMilli() > n.cfg.LegacyUntil
	if !all && !expired {
		n.mu.Unlock()
		return
	}
	n.cfg.LegacyUntil = 0
	err := n.cfg.Save()
	n.mu.Unlock()
	if err != nil {
		n.logf("save config: %v", err)
	}
	if err := config.RetireFleetKey(n.opts.Dir); err != nil {
		n.logf("identity: could not delete fleet.key: %v", err)
		return
	}
	if expired && len(waiting) > 0 {
		n.logf("identity: grace period over; refusing the shared certificate and deleted fleet.key (still on it: %s)", strings.Join(waiting, ", "))
	} else {
		n.logf("identity: every member has its own certificate; refusing the shared one and deleted fleet.key")
	}
}

// certify signs a new machine's public key with this machine's ("certify", SSH setup): the remote
// made its own key, and only certificates travel back. Returns {chain, roots} PEM.
func (n *Node) certify(args json.RawMessage) (json.RawMessage, error) {
	var a struct {
		MachineID string `json:"machineId"`
		PublicKey string `json:"publicKey"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return nil, err
	}
	if a.MachineID == "" || a.MachineID == n.cfg.MachineID {
		return nil, errors.New("bad machine id")
	}
	pub, err := config.ParsePublicKeyPEM([]byte(a.PublicKey))
	if err != nil {
		return nil, err
	}
	n.mu.Lock()
	me := n.me
	n.mu.Unlock()
	if me == nil {
		return nil, errors.New("this machine has no certificate of its own to vouch with")
	}
	chain, err := me.Issue(a.MachineID, pub)
	if err != nil {
		return nil, err
	}
	roots, err := os.ReadFile(config.PathIn(n.opts.Dir, config.CertFile))
	if err != nil {
		return nil, err
	}
	if certs, err := config.ParseCerts(chain); err == nil {
		n.mu.Lock()
		changed := n.recordMemberLocked(a.MachineID, identity{id: a.MachineID, key: config.KeyFingerprint(certs[0]), chain: chainKeys(certs), pub: certs[0].RawSubjectPublicKeyInfo})
		n.mu.Unlock()
		if changed {
			_ = n.saveConfig()
		}
	}
	n.logf("certified a key for %s", a.MachineID)
	return json.Marshal(map[string]string{"chain": string(chain), "roots": string(roots)})
}
