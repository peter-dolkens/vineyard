package mesh

import (
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
	"strings"
	"sync"
	"time"

	"github.com/peter-dolkens/vineyard/daemon/internal/claude"
	"github.com/peter-dolkens/vineyard/daemon/internal/config"
	"github.com/peter-dolkens/vineyard/daemon/internal/model"
	"github.com/peter-dolkens/vineyard/daemon/internal/protocol"
)

// Timing knobs. The design goal is silence when nobody is looking: an idle daemon holds no
// connections and runs no timers except the listener accept loop.
const (
	helloTimeout   = 10 * time.Second
	dialTimeout    = 6 * time.Second
	pingEvery      = 30 * time.Second // outbound side only
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
}

type link struct {
	conn           *Conn
	peerID         string
	name           string
	role           string
	outbound       bool
	theySubscribed bool
	weSubscribed   bool
	lastSeen       time.Time
}

type peerState struct {
	id       string
	addr     string
	link     *link
	dialing  bool
	nextDial time.Time
	backoff  time.Duration
	lastErr  string
	lastSeen time.Time
}

type pendingReq struct {
	origin  *link
	origID  string
	expires time.Time
}

type Node struct {
	opts      Options
	cfg       *config.Config
	serverTLS *tls.Config
	clientTLS *tls.Config

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
	for _, p := range opts.Config.Peers {
		n.peers[p.MachineID] = &peerState{id: p.MachineID, addr: p.Addr}
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
	l := &link{conn: NewConn(raw), outbound: false}
	if len(tc.ConnectionState().PeerCertificates) == 0 {
		n.handleJoin(l)
		return
	}
	n.serve(l)
}

func (n *Node) dial(p *peerState) {
	d := &net.Dialer{Timeout: dialTimeout}
	raw, err := tls.DialWithDialer(d, "tcp", p.addr, n.clientTLS)
	n.mu.Lock()
	p.dialing = false
	if err != nil {
		p.lastErr = trimErr(err)
		if p.backoff == 0 {
			p.backoff = backoffMin
		} else {
			p.backoff = min(p.backoff*2, backoffMax)
		}
		p.nextDial = time.Now().Add(p.backoff)
		n.mu.Unlock()
		n.broadcastPeerStatus()
		return
	}
	p.backoff = 0
	p.lastErr = ""
	n.mu.Unlock()
	l := &link{conn: NewConn(raw), outbound: true, peerID: p.id}
	if err := l.conn.Send(n.hello("peer")); err != nil {
		return
	}
	go n.serve(l)
}

func (n *Node) hello(role string) protocol.Hello {
	return protocol.Hello{
		T: "hello", Role: role, MachineID: n.cfg.MachineID, Name: n.cfg.Name,
		Version: n.opts.Version, Protocol: protocol.Version, Listen: n.cfg.Advertise,
	}
}

// serve runs one connection to completion (either direction).
func (n *Node) serve(l *link) {
	defer n.unregister(l)

	// Handshake.
	first, err := l.conn.Recv(helloTimeout)
	if err != nil {
		return
	}
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
		b, err := l.conn.Recv(deadAfter)
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
	p := n.peers[l.peerID]
	if p == nil {
		p = &peerState{id: l.peerID}
		n.peers[l.peerID] = p
	}
	if h.Listen != "" && p.addr != h.Listen {
		p.addr = h.Listen
		if n.cfg.AddPeer(protocol.PeerAddr{MachineID: l.peerID, Addr: h.Listen}) {
			if err := n.cfg.Save(); err != nil {
				n.logf("save config: %v", err)
			}
		}
	}
	p.lastSeen = time.Now()
	if p.link != nil && p.link != l {
		// Two links to the same peer (both sides dialed). Keep the one dialed by the smaller id.
		keepOutbound := n.cfg.MachineID < l.peerID
		old := p.link
		if old.outbound == keepOutbound {
			n.mu.Unlock()
			n.logf("duplicate link to %s, closing the new one", l.peerID)
			l.conn.Close()
			return
		}
		p.link = l
		l.weSubscribed = old.weSubscribed
		n.mu.Unlock()
		n.logf("duplicate link to %s, replacing the old one", l.peerID)
		old.conn.Close()
	} else {
		p.link = l
		n.mu.Unlock()
	}
	n.logf("peer %s connected (%s, outbound=%v)", l.peerID, l.conn.RemoteAddr(), l.outbound)
	n.broadcastPeerStatus()
	n.reconcileSubscriptions()
}

func (n *Node) unregister(l *link) {
	l.conn.Close()
	n.mu.Lock()
	if _, ok := n.links[l]; !ok {
		n.mu.Unlock()
		return
	}
	delete(n.links, l)
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
	for id, pr := range n.pending {
		if pr.origin == l {
			delete(n.pending, id)
		}
	}
	remaining := len(n.viewers)
	n.mu.Unlock()

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
		if l.outbound && !l.theySubscribed {
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
		if p.addr == "" || p.dialing || now.Before(p.nextDial) {
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
		if tick%int(pingEvery/time.Second) == 0 {
			n.pingAndReap()
		}
		n.mu.Lock()
		now := time.Now()
		for id, pr := range n.pending {
			if now.After(pr.expires) {
				delete(n.pending, id)
				_ = pr.origin.conn.Send(protocol.Response{T: "res", ID: pr.origID, OK: false, Error: "request timed out"})
			}
		}
		n.mu.Unlock()
	}
}

func (n *Node) pingAndReap() {
	n.mu.Lock()
	var pings, dead []*link
	now := time.Now()
	for l := range n.links {
		if now.Sub(l.lastSeen) > deadAfter {
			dead = append(dead, l)
		} else if l.outbound {
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
	for {
		if !n.collectorActive() {
			select {
			case <-ctx.Done():
				return
			case <-n.wake:
				force = true
				continue
			}
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
		entry := model.FleetEntry{Snapshot: m.Snapshot, Online: true, Via: "direct", LastSeen: now, ReceivedAt: now}
		n.mu.Lock()
		n.store[l.peerID] = entry
		if p := n.peers[l.peerID]; p != nil {
			p.lastSeen = time.Now()
		}
		n.markCacheDirty()
		n.mu.Unlock()
		n.broadcastUpdate(entry)
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
		if ok {
			r.ID = pr.origID
			_ = pr.origin.conn.Send(r)
		}
	}
}

func (n *Node) handleRequest(l *link, r protocol.Request) {
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
		if r.Op == "addpeer" {
			changed = n.cfg.AddPeer(a)
			if p := n.peers[a.MachineID]; p != nil {
				p.addr = a.Addr
				p.nextDial = time.Time{}
				p.backoff = 0
			} else if a.MachineID != "" && a.MachineID != n.cfg.MachineID {
				n.peers[a.MachineID] = &peerState{id: a.MachineID, addr: a.Addr}
			}
		} else {
			changed = n.cfg.RemovePeer(a.MachineID)
			if p := n.peers[a.MachineID]; p != nil {
				if p.link != nil {
					p.link.conn.Close()
				}
				delete(n.peers, a.MachineID)
			}
			delete(n.store, a.MachineID)
			n.markCacheDirty()
		}
		n.mu.Unlock()
		if changed {
			if err := n.cfg.Save(); err != nil {
				return nil, err
			}
		}
		n.reconcileSubscriptions()
		n.broadcastPeerStatus()
		if r.Op == "removepeer" {
			// Viewers need to drop the machine; send a fresh fleet to each.
			n.mu.Lock()
			vs := make([]*link, 0, len(n.viewers))
			for v := range n.viewers {
				vs = append(vs, v)
			}
			n.mu.Unlock()
			for _, v := range vs {
				n.sendFleet(v)
			}
		}
		return json.RawMessage(`{"ok":true}`), nil
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
	raw, err := claude.TailLines(abs, lines, 8<<20)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("no transcript at %s", abs)
		}
		return nil, err
	}
	out := protocol.TranscriptData{Path: abs, Entries: make([]json.RawMessage, 0, len(raw))}
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
		if id == "" || id == n.cfg.MachineID {
			continue
		}
		e.Online = false
		e.Via = "cache"
		n.store[id] = e
		if _, ok := n.peers[id]; !ok && e.Snapshot.Listen != "" {
			n.peers[id] = &peerState{id: id, addr: e.Snapshot.Listen}
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
