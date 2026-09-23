package mesh

import (
	"bufio"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sort"
	"strings"
	"time"

	"github.com/peter-dolkens/vineyard/daemon/internal/protocol"
)

// Relaying. When a watching daemon cannot reach a member directly, it asks another member it does
// reach to splice it through: a fresh fleet-TLS connection to the relay whose first line is
// `tunnel {target}` instead of hello. The relay gets a second leg to the target and from then on only
// copies bytes. The requester and the target then run their own TLS handshake inside the splice, so
// the relay carries ciphertext it cannot read or alter, and after that the ordinary hello and serve
// loop: subscriptions, requests and upgrades need no relay-specific code.
//
// The relay gets its second leg one of two ways:
//  1. by dialling the target itself (a relay on the office LAN reaching a desktop for a viewer at
//     home), or, failing that,
//  2. by asking a target that already holds a link to it to call back with a fresh connection (a
//     laptop behind NAT that keeps an uplink to the relay).
//
// Exactly one hop: a daemon only asks members it reaches directly, and a relay only dials or calls
// back over direct links, so tunnels never chain. Any member relays when asked. Nothing here runs
// unless someone is watching the target: the tunnel lives exactly as long as the watcher's link.

const (
	tunnelWait   = 20 * time.Second // requester waiting for tunnel-ok
	callbackWait = 10 * time.Second // relay waiting for the target to call back
	firstLineMax = 64 << 10         // tunnel messages are tiny; anything bigger is not one
)

// bufConn is a net.Conn whose reads come through a bufio.Reader that may already hold read-ahead.
type bufConn struct {
	net.Conn
	rd *bufio.Reader
}

func (c bufConn) Read(p []byte) (int, error) { return c.rd.Read(p) }

// readLine reads one newline-terminated message from rd within timeout.
func readLine(raw net.Conn, rd *bufio.Reader, timeout time.Duration) ([]byte, error) {
	_ = raw.SetReadDeadline(time.Now().Add(timeout))
	defer raw.SetReadDeadline(time.Time{})
	line, err := rd.ReadSlice('\n')
	if errors.Is(err, bufio.ErrBufferFull) {
		return nil, errors.New("first line too long")
	}
	if err != nil {
		return nil, err
	}
	return append([]byte(nil), line...), nil
}

func writeLine(raw net.Conn, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_ = raw.SetWriteDeadline(time.Now().Add(10 * time.Second))
	defer raw.SetWriteDeadline(time.Time{})
	_, err = raw.Write(append(b, '\n'))
	return err
}

// innerTLS runs the end-to-end handshake over a spliced stream and returns the encrypted connection.
func (n *Node) innerTLS(raw net.Conn, rd *bufio.Reader, client bool) (net.Conn, error) {
	under := bufConn{Conn: raw, rd: rd}
	var tc *tls.Conn
	if client {
		tc = tls.Client(under, n.clientTLS)
	} else {
		tc = tls.Server(under, n.strictTLS)
	}
	_ = tc.SetDeadline(time.Now().Add(helloTimeout))
	if err := tc.Handshake(); err != nil {
		return nil, err
	}
	_ = tc.SetDeadline(time.Time{})
	return tc, nil
}

// ---- requester side ---------------------------------------------------------------------------------

// relayCandidates lists members worth asking to relay to target: those we hold a direct link to,
// the relay that last worked for this target first.
func (n *Node) relayCandidates(target string) []*peerState {
	n.mu.Lock()
	defer n.mu.Unlock()
	var preferred string
	if p := n.peers[target]; p != nil {
		preferred = p.lastRelay
	}
	var out []*peerState
	for id, q := range n.peers {
		if id == target || q.link == nil || q.link.via != "" || len(q.addrs) == 0 {
			continue
		}
		out = append(out, q)
	}
	sort.Slice(out, func(i, j int) bool {
		if (out[i].id == preferred) != (out[j].id == preferred) {
			return out[i].id == preferred
		}
		return out[i].id < out[j].id
	})
	return out
}

// viaRelay asks each candidate in turn and returns an end-to-end encrypted connection to target plus
// the id of the relay carrying it.
func (n *Node) viaRelay(target string) (net.Conn, string, error) {
	relays := n.relayCandidates(target)
	if len(relays) == 0 {
		return nil, "", errors.New("no member to relay through")
	}
	var errs []string
	for _, r := range relays {
		c, err := n.askRelay(r, target)
		if err == nil {
			return c, r.id, nil
		}
		errs = append(errs, fmt.Sprintf("via %s: %v", r.id, err))
	}
	return nil, "", errors.New(strings.Join(errs, "; "))
}

func (n *Node) askRelay(r *peerState, target string) (net.Conn, error) {
	n.mu.Lock()
	addrs := append([]string(nil), r.addrs...)
	n.mu.Unlock()
	raw, _, err := n.dialAddrs(addrs)
	if err != nil {
		return nil, err
	}
	rd := bufio.NewReaderSize(raw, firstLineMax)
	id := newID()
	if err := writeLine(raw, protocol.Tunnel{T: "tunnel", ID: id, Target: target, From: n.cfg.MachineID}); err != nil {
		raw.Close()
		return nil, err
	}
	b, err := readLine(raw, rd, tunnelWait)
	if err != nil {
		raw.Close()
		return nil, err
	}
	var res protocol.TunnelResult
	if json.Unmarshal(b, &res) != nil || res.T != "tunnel-ok" || res.ID != id {
		raw.Close()
		if res.Error == "" {
			res.Error = "relay refused"
		}
		return nil, errors.New(res.Error)
	}
	inner, err := n.innerTLS(raw, rd, true)
	if err != nil {
		raw.Close()
		return nil, fmt.Errorf("end-to-end handshake: %w", err)
	}
	return inner, nil
}

// ---- relay side ---------------------------------------------------------------------------------------

type legConn struct {
	raw net.Conn
	rd  *bufio.Reader
}

// handleTunnel serves a connection whose first line asked us to relay. It owns raw.
func (n *Node) handleTunnel(raw net.Conn, rd *bufio.Reader, t protocol.Tunnel) {
	fail := func(msg string) {
		_ = writeLine(raw, protocol.TunnelResult{T: "tunnel-fail", ID: t.ID, Error: msg})
		raw.Close()
		n.logf("relay %s -> %s refused: %s", t.From, t.Target, msg)
	}
	if t.Target == "" || t.Target == n.cfg.MachineID || t.Target == t.From {
		fail("bad relay target")
		return
	}
	n.mu.Lock()
	p := n.peers[t.Target]
	var addrs []string
	var existing *link
	if p != nil {
		addrs = append(addrs, p.addrs...)
		if p.link != nil && p.link.via == "" {
			existing = p.link
		}
	}
	n.mu.Unlock()
	if p == nil {
		fail("this machine does not know " + t.Target)
		return
	}

	// Leg 2, first choice: dial the target ourselves.
	var far legConn
	if len(addrs) > 0 {
		if tc, used, err := n.dialAddrs(addrs); err == nil {
			if err := writeLine(tc, protocol.Tunnel{T: "tunnel-in", ID: t.ID, From: t.From, Relay: n.cfg.MachineID}); err == nil {
				far = legConn{raw: tc, rd: bufio.NewReaderSize(tc, firstLineMax)}
				n.logf("relay %s -> %s: dialled %s", t.From, t.Target, used)
			} else {
				tc.Close()
			}
		}
	}
	// Second choice: the target already talks to us (an uplink); ask it to call back.
	if far.raw == nil && existing != nil {
		ready := make(chan legConn, 1)
		n.mu.Lock()
		n.callbacks[t.ID] = ready
		n.mu.Unlock()
		if err := existing.conn.Send(protocol.Tunnel{T: "tunnel-callback", ID: t.ID, From: t.From}); err == nil {
			select {
			case far = <-ready:
				n.logf("relay %s -> %s: target called back", t.From, t.Target)
			case <-time.After(callbackWait):
			}
		}
		n.mu.Lock()
		delete(n.callbacks, t.ID)
		n.mu.Unlock()
	}
	if far.raw == nil {
		fail(fmt.Sprintf("%s cannot reach %s either", n.cfg.MachineID, t.Target))
		return
	}
	if err := writeLine(raw, protocol.TunnelResult{T: "tunnel-ok", ID: t.ID}); err != nil {
		raw.Close()
		far.raw.Close()
		return
	}
	n.logf("relaying %s -> %s", t.From, t.Target)
	splice(raw, rd, far.raw, far.rd)
	n.logf("relay %s -> %s closed", t.From, t.Target)
}

// handleTunnelAccept is the callback leg arriving: hand it to the tunnel waiting for it.
func (n *Node) handleTunnelAccept(raw net.Conn, rd *bufio.Reader, t protocol.Tunnel) {
	n.mu.Lock()
	ready := n.callbacks[t.ID]
	n.mu.Unlock()
	if ready == nil {
		raw.Close()
		return
	}
	select {
	case ready <- legConn{raw: raw, rd: rd}:
	default:
		raw.Close()
	}
}

// ---- target side ----------------------------------------------------------------------------------

// handleTunnelIn serves a relayed peer that arrived on a connection the relay dialled to us.
func (n *Node) handleTunnelIn(raw net.Conn, rd *bufio.Reader, t protocol.Tunnel) {
	inner, err := n.innerTLS(raw, rd, false)
	if err != nil {
		n.logf("relayed connection from %s via %s: %v", t.From, t.Relay, err)
		raw.Close()
		return
	}
	n.serve(&link{conn: NewConn(inner), outbound: false, via: "relay:" + t.Relay})
}

// callBack answers tunnel-callback: dial the relay afresh, name the tunnel, then be the TLS server
// for the requester whose handshake arrives through the splice.
func (n *Node) callBack(relayID string, t protocol.Tunnel) {
	n.mu.Lock()
	var addrs []string
	if r := n.peers[relayID]; r != nil {
		addrs = append(addrs, r.addrs...)
	}
	n.mu.Unlock()
	raw, _, err := n.dialAddrs(addrs)
	if err != nil {
		n.logf("relay callback to %s for %s failed: %v", relayID, t.From, err)
		return
	}
	if err := writeLine(raw, protocol.Tunnel{T: "tunnel-accept", ID: t.ID}); err != nil {
		raw.Close()
		return
	}
	n.handleTunnelIn(raw, bufio.NewReaderSize(raw, firstLineMax), protocol.Tunnel{From: t.From, Relay: relayID})
}

// splice copies bytes both ways until either side ends, then closes both. The readers may hold
// read-ahead, so they are the sources rather than the raw connections.
func splice(a net.Conn, ar *bufio.Reader, b net.Conn, br *bufio.Reader) {
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(b, ar); done <- struct{}{} }()
	go func() { _, _ = io.Copy(a, br); done <- struct{}{} }()
	<-done
	a.Close()
	b.Close()
	<-done
}
