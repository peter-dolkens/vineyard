package mesh

import (
	"encoding/json"
	"errors"
	"slices"
	"sort"
	"time"

	"github.com/peter-dolkens/vineyard/daemon/internal/protocol"
)

// Uplinks. A daemon that nobody can connect to (a laptop behind NAT on hotel or phone Wi-Fi) is
// invisible while it is not watched itself: it holds no connections, so neither a watcher nor a
// relay can reach it. With uplink "auto" (the default) a daemon checks whether it is reachable by
// asking a member to dial it back, and if not, holds one idle link to that member: an uplink. Over
// it the member can ask for a callback (tunnel.go), which is how a watcher elsewhere reaches the
// laptop through that member. Nothing but a ping every few minutes travels on it.
//
// Reachability is checked at startup, when this machine's addresses change, and when the uplink
// drops; there is no other timer. A reachable daemon holds nothing. uplink "off" disables all of it;
// any other value names the member to prefer.

const (
	uplinkPing       = 4 * time.Minute  // under the idle timeout of home routers and mobile NATs
	uplinkDeadAfter  = 10 * time.Minute // idle limit for an uplink link, on both ends
	uplinkBackoffMin = time.Minute
	uplinkBackoffMax = 30 * time.Minute
	addrCheckEvery   = time.Minute // local interface check only: no network traffic
	dialbackTimeout  = 15 * time.Second
)

type reachability int

const (
	reachUnknown reachability = iota
	reachable
	unreachable
)

type uplinkState struct {
	reach     reachability
	attempt   bool // a probe or uplink attempt is running
	link      *link
	lastID    string // member that last held our uplink
	backoff   time.Duration
	nextTry   time.Time
	addrs     []string // our addresses at the last check
	lastCheck time.Time
}

func (n *Node) uplinkMode() string {
	if n.cfg.Uplink == "" {
		return "auto"
	}
	return n.cfg.Uplink
}

// uplinkTick runs on every maintenance tick and is cheap when there is nothing to do.
func (n *Node) uplinkTick() {
	n.mu.Lock()
	if n.uplinkMode() == "off" {
		l := n.up.link
		n.up.link = nil
		n.mu.Unlock()
		if l != nil {
			l.conn.Close()
		}
		return
	}
	now := time.Now()
	if now.Sub(n.up.lastCheck) >= addrCheckEvery {
		n.up.lastCheck = now
		n.mu.Unlock()
		addrs := n.selfAddrs()
		n.mu.Lock()
		if !slices.Equal(addrs, n.up.addrs) {
			changed := n.up.addrs != nil
			n.up.addrs = addrs
			if changed {
				// New network: whether anyone can reach us, and whether relayed peers are now
				// reachable directly, both need asking again. An uplink from the old network is
				// most likely dead; drop it so the check runs straight away.
				n.up.reach = reachUnknown
				n.up.backoff = 0
				n.up.nextTry = time.Time{}
				old := n.up.link
				n.up.link = nil
				n.mu.Unlock()
				n.logf("this machine's addresses changed; checking reachability again")
				if old != nil {
					old.conn.Close()
				}
				n.probeRelayed()
				n.mu.Lock()
			}
		}
	}
	if n.up.attempt || n.up.link != nil || now.Before(n.up.nextTry) || n.up.reach == reachable || len(n.peers) == 0 {
		n.mu.Unlock()
		return
	}
	n.up.attempt = true
	n.mu.Unlock()
	go n.uplinkAttempt()
}

// uplinkCandidates orders the members to ask: the configured preference, the last uplink, then by id.
func (n *Node) uplinkCandidates() []*peerState {
	pref := n.uplinkMode()
	var out []*peerState
	for _, p := range n.peers {
		if len(p.addrs) > 0 {
			out = append(out, p)
		}
	}
	rank := func(p *peerState) int {
		switch p.id {
		case pref:
			return 0
		case n.up.lastID:
			return 1
		}
		return 2
	}
	sort.Slice(out, func(i, j int) bool {
		if rank(out[i]) != rank(out[j]) {
			return rank(out[i]) < rank(out[j])
		}
		return out[i].id < out[j].id
	})
	return out
}

// uplinkAttempt asks members in turn to dial us back. Reachable: done, nothing held. Unreachable:
// the link to that member becomes the uplink.
func (n *Node) uplinkAttempt() {
	n.mu.Lock()
	cands := n.uplinkCandidates()
	n.mu.Unlock()
	self := n.selfAddrs()
	defer func() {
		n.mu.Lock()
		n.up.attempt = false
		n.mu.Unlock()
	}()
	for _, m := range cands {
		l, own, err := n.linkTo(m)
		if err != nil {
			continue
		}
		data, err := n.request(l, "dialback", protocol.DialbackArgs{Addrs: self}, dialbackTimeout)
		if err != nil {
			if own {
				l.conn.Close()
			}
			continue
		}
		var res protocol.DialbackResult
		_ = json.Unmarshal(data, &res)
		n.mu.Lock()
		n.up.backoff = 0
		if res.Reachable {
			n.up.reach = reachable
			watching := n.wantFleet
			n.mu.Unlock()
			n.logf("reachable from %s; no uplink needed", m.id)
			if own && !watching {
				l.conn.Close()
			}
			return
		}
		n.up.reach = unreachable
		n.up.link = l
		n.up.lastID = m.id
		l.uplink = true
		n.mu.Unlock()
		_ = l.conn.Send(protocol.Ping{T: "uplink"}) // the far end relaxes its idle limit for this link
		n.logf("%s cannot reach this machine (%s); holding an uplink to it", m.id, res.Error)
		n.kickCollector() // the snapshot names the uplink
		return
	}
	n.mu.Lock()
	n.up.backoff = min(max(n.up.backoff*2, uplinkBackoffMin), uplinkBackoffMax)
	n.up.nextTry = time.Now().Add(n.up.backoff)
	n.mu.Unlock()
}

// linkTo returns a direct link to m: the existing one, or a fresh one (own=true) that says in its
// hello that it may become an uplink.
func (n *Node) linkTo(m *peerState) (l *link, own bool, err error) {
	n.mu.Lock()
	if m.link != nil && m.link.via == "" {
		l = m.link
		n.mu.Unlock()
		return l, false, nil
	}
	addrs := append([]string(nil), m.addrs...)
	n.mu.Unlock()
	raw, _, err := n.dialAddrs(addrs)
	if err != nil {
		return nil, false, err
	}
	l = &link{conn: NewConn(raw), outbound: true, peerID: m.id, uplink: true}
	h := n.hello("peer")
	h.Uplink = true
	if err := l.conn.Send(h); err != nil {
		l.conn.Close()
		return nil, false, err
	}
	registered := make(chan struct{})
	l.onRegister = func() { close(registered) }
	go n.serve(l)
	select {
	case <-registered:
		return l, true, nil
	case <-l.conn.Done():
		return nil, false, errors.New("closed during handshake")
	case <-time.After(helloTimeout):
		l.conn.Close()
		return nil, false, errors.New("no hello")
	}
}

// uplinkDropped is called from unregister: try again after a short wait, re-checking reachability.
func (n *Node) uplinkDroppedLocked(l *link) {
	if n.up.link != l {
		return
	}
	n.up.link = nil
	n.up.backoff = uplinkBackoffMin
	n.up.nextTry = time.Now().Add(n.up.backoff)
	go n.kickCollector()
}

// answerDialback is the member side: dial the asker's addresses (plus what we know of it) and say
// whether any answered. The connection opens with `probe` so the asker simply closes it.
func (n *Node) answerDialback(l *link, r protocol.Request) {
	var a protocol.DialbackArgs
	_ = json.Unmarshal(r.Args, &a)
	n.mu.Lock()
	addrs := slices.Clone(a.Addrs)
	if len(addrs) > 10 {
		addrs = addrs[:10]
	}
	if p := n.peers[l.peerID]; p != nil {
		for _, c := range p.addrs {
			if !slices.Contains(addrs, c) {
				addrs = append(addrs, c)
			}
		}
	}
	n.mu.Unlock()
	res := protocol.DialbackResult{}
	if raw, _, err := n.dialAddrs(addrs); err == nil {
		_ = writeLine(raw, protocol.Ping{T: "probe"})
		raw.Close()
		res.Reachable = true
	} else {
		res.Error = trimErr(err)
	}
	data, _ := json.Marshal(res)
	_ = l.conn.Send(protocol.Response{T: "res", ID: r.ID, OK: true, Data: data})
}

// uplinksLocked lists machines holding an uplink to us, for our hello: a watcher then asks us first
// when it needs a relay to one of them.
func (n *Node) uplinksLocked() []string {
	var out []string
	for l := range n.links {
		if l.uplink && !l.outbound && l.role == "peer" {
			out = append(out, l.peerID)
		}
	}
	sort.Strings(out)
	return out
}

// linkIdle is how long a link may be silent before it is treated as dead.
func linkIdle(l *link) time.Duration {
	if l.uplink {
		return uplinkDeadAfter
	}
	return deadAfter
}
