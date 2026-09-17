package mesh

import (
	"encoding/hex"
	"net"
	"strings"
	"time"
)

// Waking sleeping peers. A Mac that idle-sleeps (or any box with Wake-on-LAN) drops connections to
// ports it has not advertised, so a dial to 7734 just times out. While someone is watching and a peer
// is unreachable we try, at most once per wakeEvery per peer:
//   1. Wake-on-LAN magic packets for every hardware address the peer has ever reported (broadcast on
//      each local interface, plus unicast to its known addresses), and
//   2. a TCP connect to port 22 on its known addresses: Bonjour's sleep proxy wakes a Mac for
//      services it advertised (SSH is one), which is also what wakes it when you ssh in by hand.
// Both are a handful of packets and happen only while a viewer is attached.

const (
	wakeEvery   = 45 * time.Second
	wakeTimeout = 3 * time.Second
)

// maybeWake is called after a failed dial. force skips the rate limit (manual "Wake Machine").
func (n *Node) maybeWake(p *peerState, force bool) {
	if !n.cfg.WakePeers() {
		return
	}
	n.mu.Lock()
	if !force && time.Since(p.lastWake) < wakeEvery {
		n.mu.Unlock()
		return
	}
	p.lastWake = time.Now()
	macs := append([]string(nil), p.macs...)
	addrs := append([]string(nil), p.addrs...)
	if len(macs) == 0 && len(addrs) == 0 {
		n.mu.Unlock()
		return
	}
	if len(macs) > 0 {
		p.lastErr = "unreachable; sent wake-on-LAN"
	} else {
		p.lastErr = "unreachable; nudging it awake"
	}
	n.mu.Unlock()
	n.logf("peer %s unreachable; trying to wake it (%d hardware addresses, %d addresses)", p.id, len(macs), len(addrs))
	go func() {
		var hosts []string
		for _, a := range addrs {
			if h, _, err := net.SplitHostPort(a); err == nil {
				hosts = append(hosts, h)
			}
		}
		for _, mac := range macs {
			sendMagicPacket(mac, hosts)
		}
		for _, h := range hosts {
			nudge(h)
		}
	}()
	n.broadcastPeerStatus()
}

// sendMagicPacket broadcasts the standard WoL frame (6×0xFF + 16×MAC) as UDP to port 9 on every
// local broadcast domain, and unicast to the peer's known hosts.
func sendMagicPacket(mac string, hosts []string) {
	hw, err := net.ParseMAC(mac)
	if err != nil || len(hw) != 6 {
		return
	}
	pkt := make([]byte, 0, 102)
	for i := 0; i < 6; i++ {
		pkt = append(pkt, 0xFF)
	}
	for i := 0; i < 16; i++ {
		pkt = append(pkt, hw...)
	}
	targets := []string{"255.255.255.255"}
	if ifaces, err := net.Interfaces(); err == nil {
		for _, ifc := range ifaces {
			if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 || ifc.Flags&net.FlagBroadcast == 0 {
				continue
			}
			addrs, _ := ifc.Addrs()
			for _, a := range addrs {
				ipn, ok := a.(*net.IPNet)
				if !ok || ipn.IP.To4() == nil {
					continue
				}
				bc := make(net.IP, 4)
				for i := range bc {
					bc[i] = ipn.IP.To4()[i] | ^ipn.Mask[i]
				}
				targets = append(targets, bc.String())
			}
		}
	}
	targets = append(targets, hosts...)
	for _, t := range targets {
		if net.ParseIP(t) == nil {
			continue // a DNS name: skip, the broadcast covers the LAN
		}
		c, err := net.DialTimeout("udp", net.JoinHostPort(t, "9"), wakeTimeout)
		if err != nil {
			continue
		}
		_, _ = c.Write(pkt)
		_ = c.Close()
	}
}

// nudge opens (and immediately closes) a TCP connection to the SSH port so a Bonjour sleep proxy
// wakes the host on our behalf. Nothing is sent.
func nudge(host string) {
	c, err := net.DialTimeout("tcp", net.JoinHostPort(host, "22"), wakeTimeout)
	if err == nil {
		_ = c.Close()
	}
}

// normaliseMACs keeps well-formed, non-empty hardware addresses, lower-cased and de-duplicated.
func normaliseMACs(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range in {
		hw, err := net.ParseMAC(strings.TrimSpace(m))
		if err != nil || len(hw) != 6 || hex.EncodeToString(hw) == "000000000000" {
			continue
		}
		s := hw.String()
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
