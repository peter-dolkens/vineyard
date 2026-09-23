package mesh

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/peter-dolkens/vineyard/daemon/internal/auth"
	"github.com/peter-dolkens/vineyard/daemon/internal/claude"
	"github.com/peter-dolkens/vineyard/daemon/internal/config"
	"github.com/peter-dolkens/vineyard/daemon/internal/managed"
	"github.com/peter-dolkens/vineyard/daemon/internal/model"
	"github.com/peter-dolkens/vineyard/daemon/internal/protocol"
	"github.com/peter-dolkens/vineyard/daemon/internal/service"
)

// Timing knobs. The design goal is silence when nobody is looking: an idle daemon holds no
// connections and runs no timers except the listener accept loop.
const (
	helloTimeout   = 10 * time.Second
	dialTimeout    = 6 * time.Second
	dialStagger    = 250 * time.Millisecond // head start each address gets over the next in dialPeer
	pingEvery      = 30 * time.Second       // outbound side only
	deadAfter      = 95 * time.Second
	collectEvery   = 1 * time.Second
	viewerGrace    = 30 * time.Second // keep peer subscriptions this long after the last viewer leaves
	backoffMin     = 2 * time.Second
	backoffMax     = 60 * time.Second
	cacheSaveDelay = 5 * time.Second
	reqTimeout     = 60 * time.Second
	cacheFile      = "cache.json"
)

type Options struct {
	Config  *config.Config
	Version string
	Log     *log.Logger
	// Collect produces this machine's snapshot (At/Seq are filled in by the node).
	Collect func() model.Snapshot
	// ClaudeDir is used to sandbox transcript reads.
	ClaudeDir string
	// Managed runs daemon-controlled sessions (optional).
	Managed *managed.Manager
	// Auth relays `claude auth login` for viewers on other machines (optional).
	Auth *auth.Manager
}

type link struct {
	conn           *Conn
	peerID         string
	name           string
	role           string
	outbound       bool
	theySubscribed bool
	weSubscribed   bool
	// via is "" for a direct link, or "relay:<id>" when another member splices it (see tunnel.go).
	via      string
	lastSeen time.Time
	// uplink: this link is (or may become) an uplink (uplink.go): long idle limit, rare pings, kept
	// when nobody is watching.
	uplink     bool
	lastPing   time.Time
	onRegister func() // called once register has accepted the link (uplink.go waits on it)
}

type peerState struct {
	id   string
	addr string // primary (last known good); persisted in config
	// addrs are all candidates in preference order: primary, advertised, observed. Dialing tries each.
	addrs    []string
	link     *link
	dialing  bool
	nextDial time.Time
	backoff  time.Duration
	lastErr  string
	lastSeen time.Time
	macs     []string // hardware addresses the peer has reported, for Wake-on-LAN
	lastWake time.Time
	// lastRelay is the member that last relayed us to this peer; asked first next time.
	lastRelay string
	// probing is set while a direct dial is tried under a relayed link (see probeDirect).
	probing bool
	// uplinkTo is the member this peer keeps an uplink to, as that member's hello told us.
	uplinkTo string
}

type pendingReq struct {
	origin  *link
	origID  string
	expires time.Time
	// ch is set for requests this daemon itself originated (see request in distribute.go); the answer
	// goes there instead of being relayed back to origin.
	ch chan protocol.Response
}

// fail answers a pending request with an error, whichever side is waiting for it.
func (pr pendingReq) fail(msg string) {
	res := protocol.Response{T: "res", ID: pr.origID, OK: false, Error: msg}
	if pr.ch != nil {
		select {
		case pr.ch <- res:
		default:
		}
		return
	}
	_ = pr.origin.conn.Send(res)
}

type Node struct {
	opts      Options
	cfg       *config.Config
	serverTLS *tls.Config
	clientTLS *tls.Config
	strictTLS *tls.Config // serverTLS without the invite exception: for the end-to-end handshake in a relay

	mu          sync.Mutex
	peers       map[string]*peerState
	links       map[*link]struct{}
	viewers     map[*link]struct{}
	store       map[string]model.FleetEntry
	selfCanon   []byte
	seq         uint64
	wantFleet   bool
	graceTimer  *time.Timer
	pending     map[string]pendingReq
	cacheDirty  bool
	cacheTimer  *time.Timer
	subscribers int // links that want our self snapshot
	invites     map[string]invite
	upgrade     upgrader
	dist        distributor
	awake       service.Awake
	callbacks   map[string]chan legConn // relay tunnels waiting for their target to call back
	up          uplinkState
	// dialFilter, when set (tests only), drops addresses this node must not be able to reach.
	dialFilter func(addrs []string) []string

	wake chan struct{}
}

func New(opts Options) (*Node, error) {
	srv, cli, err := FleetTLS()
	if err != nil {
		return nil, fmt.Errorf("load fleet certificate: %w", err)
	}
	if opts.Log == nil {
		opts.Log = log.Default()
	}
	n := &Node{
		opts:      opts,
		cfg:       opts.Config,
		serverTLS: srv,
		clientTLS: cli,
		peers:     map[string]*peerState{},
		links:     map[*link]struct{}{},
		viewers:   map[*link]struct{}{},
		store:     map[string]model.FleetEntry{},
		pending:   map[string]pendingReq{},
		invites:   map[string]invite{},
		callbacks: map[string]chan legConn{},
		wake:      make(chan struct{}, 1),
	}
	// Closed by default: only while an invite is outstanding may a client connect without a certificate.
	strict := srv
	lenient := lenientFor(srv)
	n.serverTLS = &tls.Config{
		GetConfigForClient: func(*tls.ClientHelloInfo) (*tls.Config, error) {
			if n.hasActiveInvites() {
				return lenient, nil
			}
			return strict, nil
		},
	}
	n.serverTLS.Certificates = srv.Certificates
	n.strictTLS = strict
	n.awake.Disabled = !opts.Config.KeepAwake()
	for _, p := range opts.Config.Peers {
		n.peers[p.MachineID] = &peerState{id: p.MachineID, addr: p.Addr, addrs: config.MergeAddrs(p.Addr, p.Addrs, nil)}
	}
	n.loadCache()
	return n, nil
}

func (n *Node) logf(format string, args ...any) { n.opts.Log.Printf(format, args...) }

// Run blocks until ctx is cancelled.
func (n *Node) Run(ctx context.Context) error {
	ln, err := net.Listen("tcp", n.cfg.Listen)
	if err != nil {
		return fmt.Errorf("listen %s: %w", n.cfg.Listen, err)
	}
	tlsLn := tls.NewListener(ln, n.serverTLS)
	n.logf("listening on %s as %s (advertise %s)", n.cfg.Listen, n.cfg.MachineID, n.cfg.Advertise)

	go n.acceptLoop(ctx, tlsLn)
	go n.collectorLoop(ctx)
	go n.maintenanceLoop(ctx)

	<-ctx.Done()
	_ = tlsLn.Close()
	n.mu.Lock()
	for l := range n.links {
		l.conn.Close()
	}
	n.mu.Unlock()
	n.saveCacheNow()
	return nil
}

// ---- accept / dial ----------------------------------------------------------------------------

func (n *Node) acceptLoop(ctx context.Context, ln net.Listener) {
	for {
		raw, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			n.logf("accept: %v", err)
			time.Sleep(200 * time.Millisecond)
			continue
		}
		go n.acceptOne(raw)
	}
}

// acceptOne completes the TLS handshake and routes the connection: authenticated peers/viewers go
// to serve, certificate-less connections may only attempt a join.
func (n *Node) acceptOne(raw net.Conn) {
	tc, ok := raw.(*tls.Conn)
	if !ok {
		raw.Close()
		return
	}
	_ = tc.SetDeadline(time.Now().Add(helloTimeout))
	if err := tc.Handshake(); err != nil {
		raw.Close()
		return
	}
	_ = tc.SetDeadline(time.Time{})
	if len(tc.ConnectionState().PeerCertificates) == 0 {
		n.handleJoin(&link{conn: NewConn(raw), outbound: false})
		return
	}
	// The first line says what this connection is: a hello for an ordinary link, or one of the relay
	// messages in tunnel.go.
	rd := bufio.NewReaderSize(tc, firstLineMax)
	first, err := readLine(tc, rd, helloTimeout)
	if err != nil {
		raw.Close()
		return
	}
	var t protocol.Tunnel
	_ = json.Unmarshal(first, &t)
	switch t.T {
	case "tunnel":
		n.handleTunnel(tc, rd, t)
	case "tunnel-in":
		n.handleTunnelIn(tc, rd, t)
	case "tunnel-accept":
		n.handleTunnelAccept(tc, rd, t)
	case "probe": // a member checking that it can reach us (uplink.go)
		raw.Close()
	default:
		n.serveFirst(&link{conn: NewConn(bufConn{Conn: tc, rd: rd}), outbound: false}, first)
	}
}

// dialPeer tries every candidate address, happy-eyeballs style: each gets dialStagger's head start
// over the next, the first fleet-authenticated connection wins, and any that complete afterwards
// are closed. A machine whose primary answers costs one connection; a dead primary costs a quarter
// of a second instead of a whole dial timeout.
func dialPeer(addrs []string, tcfg *tls.Config) (*tls.Conn, string, error) {
	if len(addrs) == 0 {
		return nil, "", errors.New("no addresses to dial")
	}
	type result struct {
		tc   *tls.Conn
		addr string
		err  error
	}
	ctx, cancel := context.WithTimeout(context.Background(), dialTimeout+time.Duration(len(addrs)-1)*dialStagger)
	defer cancel()
	results := make(chan result, len(addrs))
	failed := make(chan struct{}, len(addrs)) // lets the next address start early when one fails fast
	started := 0
	start := func() {
		a := addrs[started]
		started++
		go func() {
			d := &tls.Dialer{NetDialer: &net.Dialer{Timeout: dialTimeout}, Config: tcfg}
			c, err := d.DialContext(ctx, "tcp", a)
			if err != nil {
				failed <- struct{}{}
				results <- result{addr: a, err: err}
				return
			}
			results <- result{tc: c.(*tls.Conn), addr: a}
		}()
	}
	start()
	var lastErr error
	for done := 0; done < len(addrs); {
		var next <-chan time.Time
		if started < len(addrs) {
			next = time.After(dialStagger)
		}
		select {
		case <-next:
			start()
		case <-failed:
			if started < len(addrs) {
				start()
			}
		case r := <-results:
			done++
			if r.err != nil {
				lastErr = fmt.Errorf("%s: %w", r.addr, r.err)
				continue
			}
			// Winner. Close whatever else is still in flight once it lands.
			go func(pending int) {
				for ; pending > 0; pending-- {
					if o := <-results; o.tc != nil {
						o.tc.Close()
					}
				}
			}(started - done)
			return r.tc, r.addr, nil
		}
	}
	return nil, "", lastErr
}

// dialAddrs is dialPeer from this node, honouring the test-only dialFilter.
func (n *Node) dialAddrs(addrs []string) (*tls.Conn, string, error) {
	if n.dialFilter != nil {
		addrs = n.dialFilter(addrs)
	}
	return dialPeer(addrs, n.clientTLS)
}

func (n *Node) dial(p *peerState) {
	n.mu.Lock()
	candidates := append([]string(nil), p.addrs...)
	n.mu.Unlock()
	raw, used, err := n.dialAddrs(candidates)
	var conn net.Conn = raw
	via := ""
	if err != nil {
		// No direct path. Any member we do reach may be able to relay (tunnel.go).
		if rc, relay, rerr := n.viaRelay(p.id); rerr == nil {
			n.logf("peer %s: no direct address answered (%v); relaying through %s", p.id, trimErr(err), relay)
			conn, via = rc, "relay:"+relay
			n.mu.Lock()
			p.lastRelay = relay
			p.lastErr = "direct: " + trimErr(err)
			n.mu.Unlock()
			err = nil
		}
	}
	n.mu.Lock()
	if err != nil {
		p.dialing = false
		p.lastErr = trimErr(err)
		if p.backoff == 0 {
			p.backoff = backoffMin
		} else {
			p.backoff = min(p.backoff*2, backoffMax)
		}
		p.nextDial = time.Now().Add(p.backoff)
		watching := n.wantFleet
		n.mu.Unlock()
		n.broadcastPeerStatus()
		if watching {
			n.maybeWake(p, false)
		}
		return
	}
	p.backoff = 0
	if via == "" {
		p.lastErr = ""
	}
	persist := false
	if via == "" && used != "" && used != p.addr {
		// Remember what actually worked as the primary for next time.
		p.addr = used
		p.addrs = promote(p.addrs, used)
		persist = n.cfg.AddPeer(protocol.PeerAddr{MachineID: p.id, Addr: used, Addrs: p.addrs})
	}
	n.mu.Unlock()
	if persist {
		if err := n.cfg.Save(); err != nil {
			n.logf("save config: %v", err)
		}
	}
	// p.dialing stays set until this outbound link is gone. Clearing it as soon as the TCP connection
	// opened (as an earlier version did) let the one-second reconcile tick dial the same peer again
	// while the hello round trip was still in flight, which produced duplicate links and, through the
	// duplicate-resolution path in register, silently unsubscribed links.
	l := &link{conn: NewConn(conn), outbound: true, peerID: p.id, via: via}
	go func() {
		defer func() {
			n.mu.Lock()
			p.dialing = false
			if p.link == nil && p.nextDial.Before(time.Now()) {
				p.nextDial = time.Now().Add(backoffMin)
			}
			n.mu.Unlock()
		}()
		if err := l.conn.Send(n.hello("peer")); err != nil {
			l.conn.Close()
			return
		}
		n.serve(l)
	}()
}

// selfAddrs is every address this machine can be reached at: the advertised name, then LAN IPs.
func (n *Node) selfAddrs() []string {
	_, port, _ := net.SplitHostPort(n.cfg.Listen)
	addrs := []string{}
	if n.cfg.Advertise != "" {
		addrs = append(addrs, n.cfg.Advertise)
	}
	for _, a := range localAddrs(port) {
		if a != n.cfg.Advertise {
			addrs = append(addrs, a)
		}
	}
	return addrs
}

// hello must be called without n.mu held.
func (n *Node) hello(role string) protocol.Hello {
	return protocol.Hello{
		T: "hello", Role: role, MachineID: n.cfg.MachineID, Name: n.cfg.Name,
		Version: n.opts.Version, Protocol: protocol.Version, Listen: n.cfg.Advertise, Addrs: n.selfAddrs(),
		Peers: n.knownPeers(), Uplinks: n.uplinks(), Removed: n.removals(), Added: n.cfg.Added,
	}
}

func (n *Node) removals() []protocol.Removal {
	n.mu.Lock()
	defer n.mu.Unlock()
	return slices.Clone([]protocol.Removal(n.cfg.Removed))
}

// applyRemovalLocked takes a removal (ours or a peer's) and, if it stands, drops the machine: its
// link, its peer state, and its entry in the view. Returns true if anything changed.
func (n *Node) applyRemovalLocked(r protocol.Removal) bool {
	if !n.cfg.ApplyRemoval(r) {
		return false
	}
	if p := n.peers[r.MachineID]; p != nil {
		if p.link != nil {
			p.link.conn.Close()
		}
		delete(n.peers, r.MachineID)
	}
	delete(n.store, r.MachineID)
	n.markCacheDirty()
	return true
}

// refreshViewers sends every viewer a fresh fleet, for when a machine has gone from it.
func (n *Node) refreshViewers() {
	n.mu.Lock()
	vs := make([]*link, 0, len(n.viewers))
	for v := range n.viewers {
		vs = append(vs, v)
	}
	n.mu.Unlock()
	for _, v := range vs {
		n.sendFleet(v)
	}
	n.broadcastPeerStatus()
}

func (n *Node) uplinks() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.uplinksLocked()
}

// knownPeers is what our hello tells the other side about everyone else, so a machine that reaches
// any one member learns how to reach all of them: one added over SSH, or joined from the command
// line, shows up everywhere without ever being watched itself.
func (n *Node) knownPeers() []protocol.PeerAddr {
	n.mu.Lock()
	defer n.mu.Unlock()
	out := make([]protocol.PeerAddr, 0, len(n.peers))
	for _, p := range n.peers {
		if len(p.addrs) > 0 {
			out = append(out, protocol.PeerAddr{MachineID: p.id, Addr: p.addr, Addrs: append([]string(nil), p.addrs...), Added: n.cfg.PeerAdded(p.id)})
		}
	}
	return out
}

// learnPeersLocked folds a hello's peer list into what we know. Unknown machines are added (and
// dialed by reconcileSubscriptions while someone is watching); known ones only gain candidates,
// after their own, so second-hand news never displaces what we have seen work. Machines removed
// here are skipped. Returns whether config changed, and relayed peers that gained an address and are
// worth a direct probe.
func (n *Node) learnPeersLocked(peers []protocol.PeerAddr) (changed bool, probe []*peerState) {
	for _, pa := range peers {
		if pa.MachineID == "" || pa.MachineID == n.cfg.MachineID || n.cfg.RemovalBeats(pa.MachineID, pa.Added) {
			continue
		}
		p := n.peers[pa.MachineID]
		if p == nil {
			p = &peerState{id: pa.MachineID}
			n.peers[pa.MachineID] = p
		}
		before := p.addrs
		p.addrs = config.MergeAddrs(p.addr, p.addrs, append([]string{pa.Addr}, pa.Addrs...))
		if p.link != nil && p.link.via != "" && gained(before, p.addrs) {
			probe = append(probe, p)
		}
		if len(p.addrs) == 0 {
			delete(n.peers, pa.MachineID)
			continue
		}
		if p.addr == "" {
			p.addr = p.addrs[0]
		}
		if n.cfg.AddPeer(protocol.PeerAddr{MachineID: p.id, Addr: p.addr, Addrs: p.addrs, Added: pa.Added}) {
			changed = true
		}
	}
	return changed, probe
}

// promote moves addr to the front of list (adding it if absent), deduplicating.
func promote(list []string, addr string) []string {
	out := []string{addr}
	for _, a := range list {
		if a != addr && a != "" {
			out = append(out, a)
		}
	}
	return out
}

// addCandidates merges addresses the peer itself reported (or we saw it at). The primary (last
// successful dial) stays first; these count as just seen and go ahead of older candidates, so the
// MaxAddrs cap evicts whatever has gone unused longest.
func addCandidates(p *peerState, addrs ...string) {
	p.addrs = config.MergeAddrs(p.addr, addrs, p.addrs)
	if p.addr == "" && len(p.addrs) > 0 {
		p.addr = p.addrs[0]
	}
}

// serve runs one connection to completion (either direction).
func (n *Node) serve(l *link) {
	first, err := l.conn.Recv(helloTimeout)
	if err != nil {
		n.unregister(l)
		return
	}
	n.serveFirst(l, first)
}

// serveFirst is serve once the first message has been read.
func (n *Node) serveFirst(l *link, first []byte) {
	defer n.unregister(l)

	// Handshake.
	var h protocol.Hello
	if json.Unmarshal(first, &h) != nil || h.T != "hello" {
		n.logf("%s: expected hello", l.conn.RemoteAddr())
		return
	}
	if h.Protocol != protocol.Version {
		_ = l.conn.Send(protocol.Response{T: "res", ID: "hello", OK: false, Error: fmt.Sprintf("protocol %d unsupported (want %d)", h.Protocol, protocol.Version)})
		return
	}
	if h.MachineID == n.cfg.MachineID && h.Role == "peer" {
		n.logf("%s: rejected peer claiming our own machine id %q", l.conn.RemoteAddr(), h.MachineID)
		return
	}
	if !l.outbound {
		if err := l.conn.Send(n.hello("peer")); err != nil {
			return
		}
	}
	l.role = h.Role
	l.name = h.Name
	l.lastSeen = time.Now()
	if l.role == "viewer" {
		if h.MachineID == "" {
			h.MachineID = "viewer"
		}
		l.peerID = h.MachineID
	} else {
		l.peerID = h.MachineID
	}

	n.register(l, h)

	// Main loop.
	for {
		n.mu.Lock()
		idle := linkIdle(l)
		n.mu.Unlock()
		b, err := l.conn.Recv(idle)
		if err != nil {
			return
		}
		n.mu.Lock()
		l.lastSeen = time.Now()
		n.mu.Unlock()
		n.dispatch(l, b)
	}
}

func (n *Node) register(l *link, h protocol.Hello) {
	n.mu.Lock()
	n.links[l] = struct{}{}
	if l.role == "viewer" {
		n.viewers[l] = struct{}{}
		n.mu.Unlock()
		n.logf("viewer attached from %s (%d viewers)", l.conn.RemoteAddr(), len(n.viewers))
		n.sendFleet(l)
		n.setWantFleet(true)
		return
	}

	// Peer.
	if n.cfg.RemovalBeats(l.peerID, h.Added) {
		n.mu.Unlock()
		n.logf("refused %s: removed from the fleet", l.peerID)
		l.conn.Close()
		return
	}
	if h.Uplink {
		l.uplink = true
	}
	for _, id := range h.Uplinks {
		if q := n.peers[id]; q != nil && id != n.cfg.MachineID {
			q.uplinkTo = l.peerID
		}
	}
	p := n.peers[l.peerID]
	if p == nil {
		p = &peerState{id: l.peerID}
		n.peers[l.peerID] = p
	}
	// Learn every address the peer claims, plus the one we actually see it coming from. The primary
	// (what we persist) only changes when a dial to a different address succeeds, so a stale DNS name
	// advertised by the peer cannot clobber an address that works.
	learned := append([]string{h.Listen}, h.Addrs...)
	if !l.outbound && l.via == "" { // a relayed link's remote address is the relay's
		if host, _, err := net.SplitHostPort(l.conn.RemoteAddr()); err == nil {
			if _, port, err := net.SplitHostPort(h.Listen); err == nil && port != "" {
				learned = append(learned, net.JoinHostPort(host, port))
			}
		}
	}
	before := slices.Clone(p.addrs)
	addCandidates(p, learned...)
	// A relayed peer that tells us an address we did not have may be directly reachable after all.
	probe := []*peerState{}
	if l.via != "" && gained(before, p.addrs) {
		probe = append(probe, p)
	}
	persist := p.addr != "" && n.cfg.AddPeer(protocol.PeerAddr{MachineID: l.peerID, Addr: p.addr, Addrs: p.addrs, Added: h.Added})
	learnedCfg, learnedProbe := n.learnPeersLocked(h.Peers)
	if learnedCfg {
		persist = true
	}
	removedAny := false
	for _, r := range h.Removed {
		if r.MachineID != l.peerID && n.applyRemovalLocked(r) {
			n.logf("%s removed from the fleet (heard from %s)", r.MachineID, l.peerID)
			removedAny, persist = true, true
		}
	}
	probe = append(probe, learnedProbe...)
	if persist {
		if err := n.cfg.Save(); err != nil {
			n.logf("save config: %v", err)
		}
	}
	p.lastSeen = time.Now()
	if p.link != nil && p.link != l {
		// Two links to the same peer. A direct link beats a relayed one; otherwise (both sides
		// dialed) keep the one dialed by the smaller id.
		keepOutbound := n.cfg.MachineID < l.peerID
		old := p.link
		keepOld := old.outbound == keepOutbound
		if (old.via == "") != (l.via == "") {
			keepOld = old.via == ""
		}
		if keepOld {
			n.mu.Unlock()
			n.logf("duplicate link to %s, closing the new one", l.peerID)
			l.conn.Close()
			return
		}
		// The old link carried our subscription; the new one has not been asked for anything yet, so it
		// must start unsubscribed and let reconcileSubscriptions below send a fresh subscribe. (Copying
		// the flag here, as an earlier version did, left the surviving link permanently silent.)
		p.link = l
		l.weSubscribed = false
		n.mu.Unlock()
		n.logf("duplicate link to %s, replacing the old one", l.peerID)
		old.conn.Close()
	} else {
		p.link = l
		n.mu.Unlock()
	}
	how := ""
	if l.via != "" {
		how = ", " + l.via
	}
	n.logf("peer %s connected (%s, outbound=%v%s)", l.peerID, l.conn.RemoteAddr(), l.outbound, how)
	n.broadcastPeerStatus()
	n.reconcileSubscriptions()
	n.considerPeer(l.peerID, h.Version, "", "")
	if l.onRegister != nil {
		l.onRegister()
	}
	if removedAny {
		n.refreshViewers()
	}
	for _, q := range probe {
		go n.probeDirect(q)
	}
	if !l.outbound {
		n.sendSync(l)
	}
}

// gained reports whether after holds an address before did not.
func gained(before, after []string) bool {
	for _, a := range after {
		if !slices.Contains(before, a) {
			return true
		}
	}
	return false
}

// probeDirect is how a relayed link heals: try the peer's direct addresses and, if one answers, bring
// up a direct link, which register then prefers over the relayed one on both sides. It runs on events
// only (new addresses for the peer, a viewer attaching, our own addresses changing), never on a timer.
func (n *Node) probeDirect(p *peerState) {
	n.mu.Lock()
	if p.probing || p.link == nil || p.link.via == "" {
		n.mu.Unlock()
		return
	}
	p.probing = true
	addrs := append([]string(nil), p.addrs...)
	n.mu.Unlock()
	raw, used, err := n.dialAddrs(addrs)
	n.mu.Lock()
	p.probing = false
	if err != nil {
		p.lastErr = "direct: " + trimErr(err)
		n.mu.Unlock()
		return
	}
	p.lastErr = ""
	if used != p.addr {
		p.addr = used
		p.addrs = promote(p.addrs, used)
	}
	persist := n.cfg.AddPeer(protocol.PeerAddr{MachineID: p.id, Addr: p.addr, Addrs: p.addrs})
	n.mu.Unlock()
	if persist {
		if err := n.cfg.Save(); err != nil {
			n.logf("save config: %v", err)
		}
	}
	n.logf("peer %s: direct address %s answers; leaving the relay", p.id, used)
	l := &link{conn: NewConn(raw), outbound: true, peerID: p.id}
	if err := l.conn.Send(n.hello("peer")); err != nil {
		l.conn.Close()
		return
	}
	n.serve(l)
}

// probeRelayed tries every relayed peer's direct path (a viewer attached, or our addresses changed).
func (n *Node) probeRelayed() {
	n.mu.Lock()
	var ps []*peerState
	for _, p := range n.peers {
		if p.link != nil && p.link.via != "" {
			ps = append(ps, p)
		}
	}
	n.mu.Unlock()
	for _, p := range ps {
		go n.probeDirect(p)
	}
}

// syncMaxAge bounds the second-hand state passed on in a sync: older news is not worth the bytes.
const syncMaxAge = 7 * 24 * time.Hour

// sendSync tells a peer that just connected to us what we last knew of every other machine, so a
// watcher that cannot reach one gets "last seen 10m ago, reported by forge" instead of its own older
// cache. Once per connection; the receiver keeps only what is newer than its own and not live.
func (n *Node) sendSync(l *link) {
	cutoff := time.Now().Add(-syncMaxAge).UnixMilli()
	n.mu.Lock()
	var entries []model.FleetEntry
	for id, e := range n.store {
		if id == n.cfg.MachineID || id == l.peerID || e.LastSeen < cutoff || n.cfg.IsRemoved(id) {
			continue
		}
		entries = append(entries, e)
	}
	n.mu.Unlock()
	if len(entries) > 0 {
		_ = l.conn.Send(protocol.Sync{T: "sync", Entries: entries})
	}
}

// applySync folds a peer's sync into our store: only machines we have no live link to, and only
// when the report is newer than what we have.
func (n *Node) applySync(l *link, entries []model.FleetEntry) {
	var updated []model.FleetEntry
	n.mu.Lock()
	if !n.wantFleet {
		n.mu.Unlock()
		return
	}
	for _, e := range entries {
		id := e.Snapshot.MachineID
		if id == "" || id == n.cfg.MachineID || id == l.peerID || n.cfg.IsRemoved(id) {
			continue
		}
		if p := n.peers[id]; p != nil && p.link != nil {
			continue
		}
		if cur, ok := n.store[id]; ok && cur.LastSeen >= e.LastSeen {
			continue
		}
		e.Online = false
		e.Via = "reported:" + l.peerID
		n.store[id] = e
		updated = append(updated, e)
	}
	if len(updated) > 0 {
		n.markCacheDirty()
	}
	n.mu.Unlock()
	for _, e := range updated {
		n.broadcastUpdate(e)
	}
}

func (n *Node) unregister(l *link) {
	l.conn.Close()
	n.mu.Lock()
	if _, ok := n.links[l]; !ok {
		n.mu.Unlock()
		return
	}
	delete(n.links, l)
	n.uplinkDroppedLocked(l)
	if l.theySubscribed {
		l.theySubscribed = false
		n.subscribers--
	}
	wasViewer := false
	if _, ok := n.viewers[l]; ok {
		delete(n.viewers, l)
		wasViewer = true
	}
	var peerDropped string
	if p := n.peers[l.peerID]; p != nil && p.link == l {
		p.link = nil
		p.nextDial = time.Now().Add(backoffMin)
		p.backoff = backoffMin
		peerDropped = l.peerID
		if e, ok := n.store[l.peerID]; ok && e.Online {
			e.Online = false
			e.LastSeen = time.Now().UnixMilli()
			n.store[l.peerID] = e
			n.markCacheDirty()
		}
	}
	var orphaned []pendingReq
	for id, pr := range n.pending {
		if pr.origin == l {
			delete(n.pending, id)
			if pr.ch != nil {
				orphaned = append(orphaned, pr)
			}
		}
	}
	remaining := len(n.viewers)
	n.mu.Unlock()
	for _, pr := range orphaned {
		pr.fail(fmt.Sprintf("%s disconnected", l.peerID))
	}

	if wasViewer {
		n.logf("viewer detached (%d viewers)", remaining)
		if remaining == 0 {
			n.setWantFleet(false)
		}
	}
	if peerDropped != "" {
		n.logf("peer %s disconnected: %v", peerDropped, l.conn.Err())
		if e, ok := n.entry(peerDropped); ok {
			n.broadcastUpdate(e)
		}
		n.broadcastPeerStatus()
	}
}

// ---- demand management --------------------------------------------------------------------------

// setWantFleet flips the daemon between "quiet" and "watching". Leaving watching mode is delayed by
// a grace period so a VS Code reload does not tear down and rebuild every connection.
func (n *Node) setWantFleet(want bool) {
	n.mu.Lock()
	if want {
		if n.graceTimer != nil {
			n.graceTimer.Stop()
			n.graceTimer = nil
		}
		if !n.wantFleet {
			n.wantFleet = true
			for _, p := range n.peers {
				p.nextDial = time.Time{}
				p.backoff = 0
			}
			n.mu.Unlock()
			n.logf("watching: connecting to %d peers", len(n.peers))
			n.reconcileSubscriptions()
			n.kickCollector()
			return
		}
		n.mu.Unlock()
		n.probeRelayed() // another viewer: a moment to check whether relayed peers are reachable now
		return
	}
	if n.graceTimer == nil && n.wantFleet {
		n.graceTimer = time.AfterFunc(viewerGrace, n.leaveWatching)
	}
	n.mu.Unlock()
}

func (n *Node) leaveWatching() {
	n.mu.Lock()
	n.graceTimer = nil
	if len(n.viewers) > 0 || !n.wantFleet {
		n.mu.Unlock()
		return
	}
	n.wantFleet = false
	var toClose []*link
	var toUnsub []*link
	for l := range n.links {
		if l.role != "peer" {
			continue
		}
		if l.weSubscribed {
			l.weSubscribed = false
			toUnsub = append(toUnsub, l)
		}
		if l.outbound && !l.theySubscribed && !l.uplink {
			toClose = append(toClose, l)
		}
	}
	for id, e := range n.store {
		if id != n.cfg.MachineID && e.Online {
			e.Online = false
			e.LastSeen = time.Now().UnixMilli()
			n.store[id] = e
		}
	}
	n.mu.Unlock()
	for _, l := range toUnsub {
		_ = l.conn.Send(protocol.Ping{T: "unsubscribe"})
	}
	for _, l := range toClose {
		l.conn.Close()
	}
	n.logf("quiet: no viewers; closed %d peer connections", len(toClose))
	n.saveCacheNow()
}

// reconcileSubscriptions makes sure every connected peer link carries our subscription while we
// are watching, and dials peers we have no link to.
func (n *Node) reconcileSubscriptions() {
	n.mu.Lock()
	if !n.wantFleet {
		n.mu.Unlock()
		return
	}
	var subs []*link
	var dials []*peerState
	now := time.Now()
	for _, p := range n.peers {
		if p.link != nil {
			if !p.link.weSubscribed {
				p.link.weSubscribed = true
				subs = append(subs, p.link)
			}
			continue
		}
		if len(p.addrs) == 0 || p.dialing || now.Before(p.nextDial) {
			continue
		}
		p.dialing = true
		dials = append(dials, p)
	}
	n.mu.Unlock()
	for _, l := range subs {
		_ = l.conn.Send(protocol.Ping{T: "subscribe"})
	}
	for _, p := range dials {
		go n.dial(p)
	}
}

func (n *Node) maintenanceLoop(ctx context.Context) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	tick := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		tick++
		n.reconcileSubscriptions()
		n.uplinkTick()
		if tick%int(pingEvery/time.Second) == 0 {
			n.pingAndReap()
		}
		n.mu.Lock()
		now := time.Now()
		var expired []pendingReq
		for id, pr := range n.pending {
			if now.After(pr.expires) {
				delete(n.pending, id)
				expired = append(expired, pr)
			}
		}
		n.mu.Unlock()
		for _, pr := range expired {
			pr.fail("request timed out")
		}
	}
}

func (n *Node) pingAndReap() {
	n.mu.Lock()
	var pings, dead []*link
	now := time.Now()
	for l := range n.links {
		if now.Sub(l.lastSeen) > linkIdle(l) {
			dead = append(dead, l)
		} else if l.outbound && (!l.uplink || now.Sub(l.lastPing) >= uplinkPing) {
			l.lastPing = now
			pings = append(pings, l)
		}
	}
	n.mu.Unlock()
	for _, l := range dead {
		l.conn.Close()
	}
	for _, l := range pings {
		_ = l.conn.Send(protocol.Ping{T: "ping"})
	}
}

// ---- collector -----------------------------------------------------------------------------------

// Kick asks the collector to re-read state now (used when managed sessions change).
func (n *Node) Kick() { n.kickCollector() }

func (n *Node) kickCollector() {
	select {
	case n.wake <- struct{}{}:
	default:
	}
}

func (n *Node) collectorActive() bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.viewers) > 0 || n.subscribers > 0
}

// collectorLoop polls the local Claude state once a second while anyone is interested and
// otherwise blocks with no timer at all.
func (n *Node) collectorLoop(ctx context.Context) {
	force := true
	defer n.awake.Set(false)
	for {
		if !n.collectorActive() {
			if n.awake.Held() {
				n.awake.Set(false)
				n.logf("nobody watching; letting the machine sleep")
			}
			select {
			case <-ctx.Done():
				return
			case <-n.wake:
				force = true
				continue
			}
		}
		if !n.awake.Held() && !n.awake.Disabled && runtime.GOOS == "darwin" {
			n.awake.Set(true)
			n.logf("being watched; holding off idle sleep")
		}
		n.collectSelf(force)
		force = false
		select {
		case <-ctx.Done():
			return
		case <-n.wake:
			force = true
		case <-time.After(collectEvery):
		}
	}
}

func (n *Node) collectSelf(force bool) {
	snap := n.opts.Collect()
	snap.MachineID = n.cfg.MachineID
	snap.Name = n.cfg.Name
	snap.Listen = n.cfg.Advertise
	snap.DaemonVersion = n.opts.Version
	n.mu.Lock()
	if n.up.link != nil {
		snap.Uplink = n.up.link.peerID
	}
	n.mu.Unlock()
	canon := canonical(snap)

	n.mu.Lock()
	if !force && bytes.Equal(canon, n.selfCanon) {
		n.mu.Unlock()
		return
	}
	n.selfCanon = canon
	n.seq++
	snap.Seq = n.seq
	snap.At = time.Now().UnixMilli()
	entry := model.FleetEntry{Snapshot: snap, Online: true, Via: "self", LastSeen: snap.At, ReceivedAt: snap.At}
	n.store[n.cfg.MachineID] = entry
	var subs []*link
	for l := range n.links {
		if l.theySubscribed {
			subs = append(subs, l)
		}
	}
	n.mu.Unlock()

	msg := protocol.SnapshotMsg{T: "snapshot", Snapshot: snap}
	for _, l := range subs {
		_ = l.conn.Send(msg)
	}
	n.broadcastUpdate(entry)
}

func canonical(s model.Snapshot) []byte {
	s.At, s.Seq, s.Host.Now = 0, 0, 0
	b, _ := json.Marshal(s)
	return b
}

// ---- dispatch ------------------------------------------------------------------------------------

func (n *Node) dispatch(l *link, b []byte) {
	var env protocol.Envelope
	if json.Unmarshal(b, &env) != nil {
		return
	}
	switch env.T {
	case "removed":
		var m protocol.Removed
		if json.Unmarshal(b, &m) != nil || l.role != "peer" {
			return
		}
		n.mu.Lock()
		changed := false
		for _, r := range m.Removals {
			if r.MachineID != l.peerID && n.applyRemovalLocked(r) {
				n.logf("%s removed from the fleet (heard from %s)", r.MachineID, l.peerID)
				changed = true
			}
		}
		n.mu.Unlock()
		if changed {
			if err := n.cfg.Save(); err != nil {
				n.logf("save config: %v", err)
			}
			n.refreshViewers()
		}
	case "uplink":
		n.mu.Lock()
		l.uplink = true
		n.mu.Unlock()
	case "sync":
		var m protocol.Sync
		if json.Unmarshal(b, &m) == nil && l.role == "peer" {
			n.applySync(l, m.Entries)
		}
	case "tunnel-callback":
		var t protocol.Tunnel
		if json.Unmarshal(b, &t) == nil && l.role == "peer" && l.via == "" {
			go n.callBack(l.peerID, t)
		}
	case "ping":
		_ = l.conn.Send(protocol.Ping{T: "pong"})
	case "pong":
	case "subscribe":
		n.mu.Lock()
		if !l.theySubscribed {
			l.theySubscribed = true
			n.subscribers++
		}
		e, have := n.store[n.cfg.MachineID]
		n.mu.Unlock()
		if have {
			_ = l.conn.Send(protocol.SnapshotMsg{T: "snapshot", Snapshot: e.Snapshot})
		}
		n.kickCollector()
	case "unsubscribe":
		n.mu.Lock()
		if l.theySubscribed {
			l.theySubscribed = false
			n.subscribers--
		}
		n.mu.Unlock()
	case "snapshot":
		var m protocol.SnapshotMsg
		if json.Unmarshal(b, &m) != nil || l.role != "peer" {
			return
		}
		if m.Snapshot.MachineID != l.peerID {
			n.logf("peer %s sent snapshot for %s; ignored", l.peerID, m.Snapshot.MachineID)
			return
		}
		now := time.Now().UnixMilli()
		via := "direct"
		if l.via != "" {
			via = l.via
		}
		entry := model.FleetEntry{Snapshot: m.Snapshot, Online: true, Via: via, LastSeen: now, ReceivedAt: now}
		n.mu.Lock()
		n.store[l.peerID] = entry
		if p := n.peers[l.peerID]; p != nil {
			p.lastSeen = time.Now()
			if macs := normaliseMACs(m.Snapshot.Host.MACs); len(macs) > 0 {
				p.macs = macs
			}
		}
		n.markCacheDirty()
		n.mu.Unlock()
		n.broadcastUpdate(entry)
		n.considerPeer(l.peerID, m.Snapshot.DaemonVersion, m.Snapshot.Host.OS, m.Snapshot.Host.Arch)
	case "req":
		var r protocol.Request
		if json.Unmarshal(b, &r) != nil {
			return
		}
		n.handleRequest(l, r)
	case "res":
		var r protocol.Response
		if json.Unmarshal(b, &r) != nil {
			return
		}
		n.mu.Lock()
		pr, ok := n.pending[r.ID]
		delete(n.pending, r.ID)
		n.mu.Unlock()
		if !ok {
			return
		}
		if pr.ch != nil {
			select {
			case pr.ch <- r:
			default:
			}
			return
		}
		r.ID = pr.origID
		_ = pr.origin.conn.Send(r)
	}
}

func (n *Node) handleRequest(l *link, r protocol.Request) {
	if r.Op == "dialback" && l.role == "peer" && (r.Target == "" || r.Target == n.cfg.MachineID) {
		go n.answerDialback(l, r) // dials out; keep this link's read loop free meanwhile
		return
	}
	if r.Target == "" || r.Target == n.cfg.MachineID {
		data, err := n.handleLocal(r)
		if err != nil {
			_ = l.conn.Send(protocol.Response{T: "res", ID: r.ID, OK: false, Error: err.Error()})
			return
		}
		_ = l.conn.Send(protocol.Response{T: "res", ID: r.ID, OK: true, Data: data})
		return
	}
	if l.role != "viewer" {
		_ = l.conn.Send(protocol.Response{T: "res", ID: r.ID, OK: false, Error: "relay is only available to viewers"})
		return
	}
	n.mu.Lock()
	p := n.peers[r.Target]
	var target *link
	if p != nil {
		target = p.link
	}
	if target == nil {
		n.mu.Unlock()
		_ = l.conn.Send(protocol.Response{T: "res", ID: r.ID, OK: false, Error: fmt.Sprintf("%s is not connected", r.Target)})
		return
	}
	id := newID()
	n.pending[id] = pendingReq{origin: l, origID: r.ID, expires: time.Now().Add(reqTimeout)}
	n.mu.Unlock()
	fwd := r
	fwd.ID = id
	_ = target.conn.Send(fwd)
}

func (n *Node) handleLocal(r protocol.Request) (json.RawMessage, error) {
	switch r.Op {
	case "ping":
		return json.RawMessage(`{"ok":true}`), nil
	case "probe":
		n.kickCollector()
		return json.RawMessage(`{"ok":true}`), nil
	case "invite":
		code, err := n.CreateInvite()
		if err != nil {
			return nil, err
		}
		return json.Marshal(map[string]any{"code": code, "expiresInSeconds": int(inviteTTL.Seconds())})
	case "addpeer", "removepeer":
		var a protocol.PeerAddr
		if err := json.Unmarshal(r.Args, &a); err != nil {
			return nil, err
		}
		n.mu.Lock()
		var changed bool
		var removal protocol.Removal
		if r.Op == "addpeer" {
			if a.Added == 0 {
				a.Added = time.Now().UnixMilli() // a deliberate add beats any earlier removal
			}
			changed = n.cfg.AddPeer(a)
			if p := n.peers[a.MachineID]; p != nil {
				p.addr = a.Addr
				p.addrs = promote(p.addrs, a.Addr)
				p.nextDial = time.Time{}
				p.backoff = 0
			} else if a.MachineID != "" && a.MachineID != n.cfg.MachineID {
				n.peers[a.MachineID] = &peerState{id: a.MachineID, addr: a.Addr, addrs: []string{a.Addr}}
			}
		} else {
			removal = protocol.Removal{MachineID: a.MachineID, At: time.Now().UnixMilli()}
			changed = n.applyRemovalLocked(removal)
		}
		var peerLinks []*link
		if r.Op == "removepeer" && changed {
			for l := range n.links {
				if l.role == "peer" {
					peerLinks = append(peerLinks, l)
				}
			}
		}
		n.mu.Unlock()
		// Tell whoever is connected now; everyone else hears it in hellos.
		for _, l := range peerLinks {
			_ = l.conn.Send(protocol.Removed{T: "removed", Removals: []protocol.Removal{removal}})
		}
		if changed {
			if err := n.cfg.Save(); err != nil {
				return nil, err
			}
		}
		n.reconcileSubscriptions()
		n.broadcastPeerStatus()
		if r.Op == "removepeer" {
			n.refreshViewers() // viewers need to drop the machine
		}
		return json.RawMessage(`{"ok":true}`), nil
	case "send":
		var a protocol.SendArgs
		if err := json.Unmarshal(r.Args, &a); err != nil {
			return nil, err
		}
		if n.opts.Managed != nil && n.opts.Managed.Has(a.SessionID) {
			if err := n.opts.Managed.SendWithAttachments(a.SessionID, a.Text, a.Attachments); err != nil {
				return nil, err
			}
			n.kickCollector()
			return json.RawMessage(`{"managed":true}`), nil
		}
		text, err := managed.InlineAttachments(a.Text, a.Attachments)
		if err != nil {
			return nil, err
		}
		id, err := claude.SendToSession(n.opts.ClaudeDir, a.SessionID, text)
		if err != nil {
			return nil, err
		}
		n.kickCollector()
		return json.Marshal(map[string]any{"msgId": id})
	case "spawn":
		if n.opts.Managed == nil {
			return nil, errors.New("managed sessions are disabled on this daemon")
		}
		var o managed.SpawnOptions
		if err := json.Unmarshal(r.Args, &o); err != nil {
			return nil, err
		}
		var b spawnBasis
		if err := json.Unmarshal(r.Args, &b); err != nil {
			return nil, err
		}
		// A remembered choice gives way to a settings default changed since it was made; the reply
		// says which, so the extension can forget them.
		defaults := claude.SettingsDefaults(n.opts.ClaudeDir, o.Cwd)
		stale := dropStaleChoices(&o, b.Basis, defaults)
		sid, err := n.opts.Managed.Spawn(o)
		if err != nil {
			return nil, err
		}
		n.kickCollector()
		return json.Marshal(map[string]any{"sessionId": sid, "defaults": defaults, "stale": stale})
	case "respond":
		if n.opts.Managed == nil {
			return nil, errors.New("managed sessions are disabled on this daemon")
		}
		var a protocol.RespondArgs
		if err := json.Unmarshal(r.Args, &a); err != nil {
			return nil, err
		}
		if err := n.opts.Managed.Respond(a.SessionID, a.RequestID, a.Response); err != nil {
			return nil, err
		}
		n.kickCollector()
		return json.RawMessage(`{"ok":true}`), nil
	case "interrupt", "stop":
		if n.opts.Managed == nil {
			return nil, errors.New("managed sessions are disabled on this daemon")
		}
		var a protocol.SendArgs
		if err := json.Unmarshal(r.Args, &a); err != nil {
			return nil, err
		}
		var err error
		if r.Op == "interrupt" {
			err = n.opts.Managed.Interrupt(a.SessionID)
		} else {
			err = n.opts.Managed.Stop(a.SessionID)
		}
		if err != nil {
			return nil, err
		}
		return json.RawMessage(`{"ok":true}`), nil
	case "stoptask":
		// Stops one background task or subagent; only a session this daemon spawned has the control
		// channel that carries stop_task, so an observed session gets a plain refusal.
		var a protocol.StopTaskArgs
		if err := json.Unmarshal(r.Args, &a); err != nil {
			return nil, err
		}
		if strings.TrimSpace(a.TaskID) == "" {
			return nil, errors.New("taskId is required")
		}
		if n.opts.Managed == nil || !n.opts.Managed.Has(a.SessionID) {
			return nil, errors.New("only sessions started by Vineyard can stop their tasks")
		}
		if err := n.opts.Managed.StopTask(a.SessionID, a.TaskID); err != nil {
			return nil, err
		}
		n.kickCollector()
		return json.RawMessage(`{"ok":true}`), nil
	case "configure":
		if n.opts.Managed == nil {
			return nil, errors.New("managed sessions are disabled on this daemon")
		}
		var a protocol.ConfigureArgs
		if err := json.Unmarshal(r.Args, &a); err != nil {
			return nil, err
		}
		if a.Model != nil {
			if err := n.opts.Managed.SetModel(a.SessionID, *a.Model); err != nil {
				return nil, err
			}
		}
		if a.Effort != nil {
			if err := n.opts.Managed.SetEffort(a.SessionID, *a.Effort); err != nil {
				return nil, err
			}
		}
		if a.PermissionMode != nil {
			if err := n.opts.Managed.SetPermissionMode(a.SessionID, *a.PermissionMode); err != nil {
				return nil, err
			}
		}
		n.kickCollector()
		// The settings defaults the choice was made against: the extension keeps them with the
		// choice, and a later spawn drops the choice once they change.
		return json.Marshal(map[string]any{"ok": true, "defaults": claude.SettingsDefaults(n.opts.ClaudeDir, n.opts.Managed.Cwd(a.SessionID))})
	case "upgrade":
		var a protocol.UpgradeArgs
		if err := json.Unmarshal(r.Args, &a); err != nil {
			return nil, err
		}
		return n.handleUpgrade(a)
	case "stage":
		var a protocol.UpgradeArgs
		if err := json.Unmarshal(r.Args, &a); err != nil {
			return nil, err
		}
		return n.handleStage(a)
	case "dist":
		return n.handleDist()
	case "version":
		return json.Marshal(map[string]any{"version": n.opts.Version, "protocol": protocol.Version})
	case "wake":
		// Manual wake of a sleeping peer: send the wake signals now and dial again right away.
		var a protocol.PeerAddr
		if err := json.Unmarshal(r.Args, &a); err != nil {
			return nil, err
		}
		n.mu.Lock()
		p := n.peers[a.MachineID]
		if p == nil {
			n.mu.Unlock()
			return nil, fmt.Errorf("unknown machine %s", a.MachineID)
		}
		if p.link != nil {
			n.mu.Unlock()
			return json.Marshal(map[string]any{"awake": true})
		}
		p.nextDial = time.Time{}
		p.backoff = 0
		macs := len(p.macs)
		n.mu.Unlock()
		n.maybeWake(p, true)
		n.reconcileSubscriptions()
		return json.Marshal(map[string]any{"awake": false, "hardwareAddresses": macs})
	case "rename":
		var a protocol.RenameArgs
		if err := json.Unmarshal(r.Args, &a); err != nil {
			return nil, err
		}
		if strings.TrimSpace(a.Title) == "" {
			return nil, errors.New("title is empty")
		}
		if n.opts.Managed != nil && n.opts.Managed.Has(a.SessionID) {
			if err := n.opts.Managed.Rename(a.SessionID, strings.TrimSpace(a.Title)); err == nil {
				n.kickCollector()
				return json.RawMessage(`{"managed":true}`), nil
			} else {
				n.logf("rename via control channel failed (%v); appending to the transcript instead", err)
			}
		}
		path := a.Path
		if path == "" {
			if a.Cwd == "" {
				return nil, errors.New("path or cwd is required")
			}
			path = filepath.Join(n.opts.ClaudeDir, "projects", claude.EncodeProjectDir(a.Cwd), a.SessionID+".jsonl")
		}
		if err := claude.AppendCustomTitle(n.opts.ClaudeDir, path, a.SessionID, a.Title); err != nil {
			return nil, err
		}
		n.kickCollector()
		return json.RawMessage(`{"ok":true}`), nil
	case "login":
		if n.opts.Auth == nil {
			return nil, errors.New("sign-in relay is disabled on this daemon")
		}
		var a protocol.LoginArgs
		if err := json.Unmarshal(r.Args, &a); err != nil {
			return nil, err
		}
		switch a.Action {
		case "start":
			id, url, err := n.opts.Auth.Start(a.Console)
			if err != nil {
				return nil, err
			}
			return json.Marshal(map[string]any{"id": id, "url": url})
		case "code":
			msg, err := n.opts.Auth.Code(a.ID, a.Code)
			if err != nil {
				return nil, err
			}
			n.kickCollector()
			return json.Marshal(map[string]any{"ok": true, "message": msg})
		case "cancel":
			n.opts.Auth.Cancel(a.ID)
			return json.RawMessage(`{"ok":true}`), nil
		}
		return nil, fmt.Errorf("unknown login action %q", a.Action)
	case "sessions":
		var a protocol.SessionsArgs
		if len(r.Args) > 0 {
			if err := json.Unmarshal(r.Args, &a); err != nil {
				return nil, err
			}
		}
		list, err := claude.ListSessions(n.opts.ClaudeDir, a.Cwd, a.Limit)
		if err != nil {
			return nil, err
		}
		return json.Marshal(map[string]any{"sessions": list})
	case "takeover":
		// Bring an observed session under this daemon: end its process, wait for it to be gone
		// (two writers on one transcript interleave), then resume it as a managed child with no
		// model/effort/mode overrides, so Claude Code restores the session's own. A question it was
		// blocked on travels along and is asked again from the chat.
		if n.opts.Managed == nil {
			return nil, errors.New("managed sessions are disabled on this daemon")
		}
		var a protocol.SendArgs
		if err := json.Unmarshal(r.Args, &a); err != nil {
			return nil, err
		}
		if n.opts.Managed.Has(a.SessionID) {
			return nil, fmt.Errorf("session %s is already managed by this daemon", a.SessionID)
		}
		var target *model.Agent
		n.mu.Lock()
		if e, ok := n.store[n.cfg.MachineID]; ok {
			for i := range e.Snapshot.Agents {
				if e.Snapshot.Agents[i].SessionID == a.SessionID {
					cp := e.Snapshot.Agents[i]
					target = &cp
					break
				}
			}
		}
		n.mu.Unlock()
		if target == nil || target.WorkspacePath == "" {
			return nil, fmt.Errorf("no session %s is known on this machine", a.SessionID)
		}
		cwd := target.Cwd // where it really ran: a scratchpad session is listed under its parent project
		if cwd == "" {
			cwd = target.WorkspacePath
		}
		o := managed.SpawnOptions{Cwd: cwd, Resume: a.SessionID}
		for _, pt := range target.PendingTools {
			if pt.Name == "AskUserQuestion" && len(pt.Input) > 0 {
				o.Recover = &managed.RecoveredQuestion{ToolUseID: pt.ID, Input: pt.Input}
				break
			}
		}
		if target.Alive && target.PID > 0 {
			if err := claude.Terminate(target.PID); err != nil {
				return nil, err
			}
			if !claude.WaitExit(target.PID, 8*time.Second) {
				return nil, fmt.Errorf("process %d of session %s did not exit", target.PID, a.SessionID)
			}
			n.logf("takeover: ended session %s (pid %d)", a.SessionID, target.PID)
		}
		sid, err := n.opts.Managed.Spawn(o)
		if err != nil {
			return nil, err
		}
		n.logf("takeover: resumed session %s as a managed child (question carried: %v)", sid, o.Recover != nil)
		n.kickCollector()
		return json.Marshal(map[string]any{"sessionId": sid, "recovered": o.Recover != nil})
	case "kill":
		// End any session on this machine: managed ones through their control channel, observed
		// ones by signalling the process the registry reports for that session id.
		var a protocol.SendArgs
		if err := json.Unmarshal(r.Args, &a); err != nil {
			return nil, err
		}
		if n.opts.Managed != nil && n.opts.Managed.Has(a.SessionID) {
			if err := n.opts.Managed.Stop(a.SessionID); err != nil {
				return nil, err
			}
			n.kickCollector()
			return json.RawMessage(`{"managed":true}`), nil
		}
		n.mu.Lock()
		pid := 0
		if e, ok := n.store[n.cfg.MachineID]; ok {
			for _, ag := range e.Snapshot.Agents {
				if ag.SessionID == a.SessionID {
					pid = ag.PID
					break
				}
			}
		}
		n.mu.Unlock()
		if pid <= 0 {
			return nil, fmt.Errorf("no running process is known for session %s", a.SessionID)
		}
		if err := claude.Terminate(pid); err != nil {
			return nil, err
		}
		n.logf("killed session %s (pid %d) on request", a.SessionID, pid)
		time.AfterFunc(1500*time.Millisecond, n.kickCollector)
		return json.Marshal(map[string]any{"pid": pid})
	case "transcript":
		var a protocol.TranscriptArgs
		if len(r.Args) > 0 {
			if err := json.Unmarshal(r.Args, &a); err != nil {
				return nil, err
			}
		}
		return n.readTranscript(a)
	}
	return nil, fmt.Errorf("unknown op %q", r.Op)
}

func (n *Node) readTranscript(a protocol.TranscriptArgs) (json.RawMessage, error) {
	projects := filepath.Join(n.opts.ClaudeDir, "projects")
	path := a.Path
	if path == "" {
		if a.SessionID == "" || a.Cwd == "" {
			return nil, errors.New("sessionId and cwd (or path) are required")
		}
		path = filepath.Join(projects, claude.EncodeProjectDir(a.Cwd), a.SessionID+".jsonl")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	rel, err := filepath.Rel(projects, abs)
	if err != nil || strings.HasPrefix(rel, "..") {
		return nil, errors.New("transcript path must be inside the Claude projects directory")
	}
	lines := a.Lines
	if lines <= 0 {
		lines = 400
	}
	var raw [][]byte
	var offset, size int64
	truncated := false
	if a.Offset > 0 {
		raw, offset, size, err = claude.ReadFrom(abs, a.Offset, 8<<20)
		if err == nil && size < a.Offset {
			// File shrank or was replaced: start over with a tail.
			truncated = true
			raw, offset, err = claude.TailWithOffset(abs, lines, 8<<20)
			size = offset
		}
	} else {
		raw, offset, err = claude.TailWithOffset(abs, lines, 8<<20)
		size = offset
	}
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("no transcript at %s", abs)
		}
		return nil, err
	}
	out := protocol.TranscriptData{Path: abs, Entries: make([]json.RawMessage, 0, len(raw)), Offset: offset, Size: size, Truncated: truncated}
	for _, ln := range raw {
		if json.Valid(ln) {
			out.Entries = append(out.Entries, json.RawMessage(ln))
		}
	}
	return json.Marshal(out)
}

// ---- viewer fan-out ------------------------------------------------------------------------------

func (n *Node) entry(id string) (model.FleetEntry, bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	e, ok := n.store[id]
	return e, ok
}

func (n *Node) fleetMessage() protocol.Fleet {
	n.mu.Lock()
	defer n.mu.Unlock()
	f := protocol.Fleet{T: "fleet", Self: n.cfg.MachineID, Entries: make([]model.FleetEntry, 0, len(n.store)), Peers: n.peerStatusLocked()}
	for _, e := range n.store {
		f.Entries = append(f.Entries, e)
	}
	return f
}

func (n *Node) peerStatusLocked() []protocol.PeerStatus {
	out := make([]protocol.PeerStatus, 0, len(n.peers))
	for _, p := range n.peers {
		ps := protocol.PeerStatus{MachineID: p.id, Addr: p.addr, Connected: p.link != nil, LastError: p.lastErr}
		if p.link != nil {
			ps.Via = "direct"
			if p.link.via != "" {
				ps.Via = p.link.via
			}
		}
		if !p.lastSeen.IsZero() {
			ps.LastSeen = p.lastSeen.UnixMilli()
		}
		out = append(out, ps)
	}
	return out
}

func (n *Node) sendFleet(l *link) {
	_ = l.conn.Send(n.fleetMessage())
}

func (n *Node) broadcastUpdate(e model.FleetEntry) {
	n.mu.Lock()
	vs := make([]*link, 0, len(n.viewers))
	for v := range n.viewers {
		vs = append(vs, v)
	}
	n.mu.Unlock()
	if len(vs) == 0 {
		return
	}
	msg := protocol.Update{T: "update", Entry: e}
	for _, v := range vs {
		_ = v.conn.Send(msg)
	}
}

func (n *Node) broadcastPeerStatus() {
	n.mu.Lock()
	vs := make([]*link, 0, len(n.viewers))
	for v := range n.viewers {
		vs = append(vs, v)
	}
	msg := protocol.PeersUpdate{T: "peerstatus", Peers: n.peerStatusLocked()}
	n.mu.Unlock()
	for _, v := range vs {
		_ = v.conn.Send(msg)
	}
}

// ---- cache ---------------------------------------------------------------------------------------

func (n *Node) markCacheDirty() {
	n.cacheDirty = true
	if n.cacheTimer == nil {
		n.cacheTimer = time.AfterFunc(cacheSaveDelay, n.saveCacheNow)
	}
}

func (n *Node) saveCacheNow() {
	n.mu.Lock()
	n.cacheTimer = nil
	if !n.cacheDirty && len(n.store) == 0 {
		n.mu.Unlock()
		return
	}
	n.cacheDirty = false
	entries := make([]model.FleetEntry, 0, len(n.store))
	for id, e := range n.store {
		if id == n.cfg.MachineID {
			continue
		}
		e.Online = false
		e.Via = "cache"
		entries = append(entries, e)
	}
	n.mu.Unlock()
	b, err := json.Marshal(entries)
	if err != nil {
		return
	}
	tmp := config.Path(cacheFile + ".tmp")
	if err := os.WriteFile(tmp, b, 0o600); err == nil {
		_ = os.Rename(tmp, config.Path(cacheFile))
	}
}

func (n *Node) loadCache() {
	b, err := os.ReadFile(config.Path(cacheFile))
	if err != nil {
		return
	}
	var entries []model.FleetEntry
	if json.Unmarshal(b, &entries) != nil {
		return
	}
	for _, e := range entries {
		id := e.Snapshot.MachineID
		if id == "" || id == n.cfg.MachineID || n.cfg.IsRemoved(id) {
			continue
		}
		e.Online = false
		e.Via = "cache"
		n.store[id] = e
		if _, ok := n.peers[id]; !ok && e.Snapshot.Listen != "" {
			n.peers[id] = &peerState{id: id, addr: e.Snapshot.Listen, addrs: []string{e.Snapshot.Listen}}
		}
		if p := n.peers[id]; p != nil && len(p.macs) == 0 {
			p.macs = normaliseMACs(e.Snapshot.Host.MACs)
		}
	}
}

// ---- misc ----------------------------------------------------------------------------------------

func newID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func trimErr(err error) string {
	s := err.Error()
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}
