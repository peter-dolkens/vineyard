package mesh

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"github.com/peter-dolkens/vineyard/daemon/internal/config"
	"github.com/peter-dolkens/vineyard/daemon/internal/protocol"
)

const inviteTTL = 15 * time.Minute

type invite struct {
	expires time.Time
}

// Fingerprint of a certificate for pinning: base64url(SHA-256(DER)), truncated to 22 chars (128 bits).
func Fingerprint(cert *x509.Certificate) string {
	sum := sha256.Sum256(cert.Raw)
	return base64.RawURLEncoding.EncodeToString(sum[:])[:22]
}

func randomToken() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return base64.RawURLEncoding.EncodeToString(b[:])
}

// localAddrs lists non-loopback IPv4/IPv6 addresses with the given port, as fallbacks for hosts
// whose DNS name may not resolve from another network.
func localAddrs(port string) []string {
	var out []string
	ifaces, err := net.Interfaces()
	if err != nil {
		return out
	}
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok || ipn.IP.IsLoopback() || ipn.IP.IsLinkLocalUnicast() {
				continue
			}
			if ipn.IP.To4() != nil {
				out = append(out, net.JoinHostPort(ipn.IP.String(), port))
			}
		}
	}
	return out
}

// CreateInvite mints a single-use code valid for inviteTTL and opens the unauthenticated join path.
func (n *Node) CreateInvite() (string, error) {
	leaf := n.serverTLS.Certificates[0]
	cert, err := x509.ParseCertificate(leaf.Certificate[0])
	if err != nil {
		return "", err
	}
	token := randomToken()
	n.mu.Lock()
	n.invites[token] = invite{expires: time.Now().Add(inviteTTL)}
	n.mu.Unlock()

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
	inv := protocol.Invite{V: 1, Addrs: addrs, Fingerprint: Fingerprint(cert), Token: token, Name: n.cfg.Name, MachineID: n.cfg.MachineID}
	b, err := json.Marshal(inv)
	if err != nil {
		return "", err
	}
	n.logf("invite created (valid %s)", inviteTTL)
	return protocol.InvitePrefix + base64.RawURLEncoding.EncodeToString(b), nil
}

func DecodeInvite(code string) (*protocol.Invite, error) {
	code = strings.TrimSpace(code)
	// Accept the raw code, the vineyard: form, and a vscode://…/join?code= URL.
	if i := strings.Index(code, "code="); i >= 0 {
		code = code[i+len("code="):]
		if j := strings.IndexAny(code, "&#"); j >= 0 {
			code = code[:j]
		}
	}
	code = strings.TrimPrefix(code, protocol.InvitePrefix)
	b, err := base64.RawURLEncoding.DecodeString(code)
	if err != nil {
		return nil, fmt.Errorf("invite code is not valid: %w", err)
	}
	var inv protocol.Invite
	if err := json.Unmarshal(b, &inv); err != nil || inv.V != 1 || inv.Token == "" || len(inv.Addrs) == 0 {
		return nil, errors.New("invite code is not valid")
	}
	return &inv, nil
}

func (n *Node) hasActiveInvites() bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	now := time.Now()
	for t, inv := range n.invites {
		if now.After(inv.expires) {
			delete(n.invites, t)
		}
	}
	return len(n.invites) > 0
}

// consumeInvite validates and burns a token.
func (n *Node) consumeInvite(token string) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	for t, inv := range n.invites {
		if time.Now().After(inv.expires) {
			delete(n.invites, t)
			continue
		}
		if subtle.ConstantTimeCompare([]byte(t), []byte(token)) == 1 {
			delete(n.invites, t)
			return true
		}
	}
	return false
}

// handleJoin serves an unauthenticated connection: exactly one join message, one reply, close.
func (n *Node) handleJoin(l *link) {
	defer l.conn.Close()
	b, err := l.conn.Recv(helloTimeout)
	if err != nil {
		return
	}
	var j protocol.Join
	if json.Unmarshal(b, &j) != nil || j.T != "join" {
		return
	}
	if !n.consumeInvite(j.Token) {
		n.logf("join from %s rejected: bad or expired token", l.conn.RemoteAddr())
		time.Sleep(time.Second) // blunt brute-force damper
		_ = l.conn.Send(protocol.Joined{T: "joined", OK: false, Error: "invalid or expired invite"})
		return
	}
	certPEM, err1 := os.ReadFile(config.Path(config.CertFile))
	keyPEM, err2 := os.ReadFile(config.Path(config.KeyFile))
	if err1 != nil || err2 != nil {
		_ = l.conn.Send(protocol.Joined{T: "joined", OK: false, Error: "fleet certificate unavailable"})
		return
	}
	selfAddrs := n.selfAddrs()
	n.mu.Lock()
	peers := []protocol.PeerAddr{{MachineID: n.cfg.MachineID, Addr: n.cfg.Advertise, Addrs: selfAddrs}}
	for _, p := range n.cfg.Peers {
		if p.MachineID != j.MachineID {
			peers = append(peers, p)
		}
	}
	changed := false
	if j.MachineID != "" && j.Listen != "" && j.MachineID != n.cfg.MachineID {
		changed = n.cfg.AddPeer(protocol.PeerAddr{MachineID: j.MachineID, Addr: j.Listen})
		if p := n.peers[j.MachineID]; p != nil {
			addCandidates(p, j.Listen)
		} else {
			n.peers[j.MachineID] = &peerState{id: j.MachineID, addr: j.Listen, addrs: []string{j.Listen}}
		}
		if host, _, err := net.SplitHostPort(l.conn.RemoteAddr()); err == nil {
			if _, port, err := net.SplitHostPort(j.Listen); err == nil {
				addCandidates(n.peers[j.MachineID], net.JoinHostPort(host, port))
			}
		}
	}
	n.mu.Unlock()
	if changed {
		if err := n.cfg.Save(); err != nil {
			n.logf("save config: %v", err)
		}
	}
	_ = l.conn.Send(protocol.Joined{T: "joined", OK: true, Cert: string(certPEM), Key: string(keyPEM), Peers: peers})
	n.logf("machine %s (%s) joined via invite from %s", j.MachineID, j.Name, l.conn.RemoteAddr())
	n.broadcastPeerStatus()
	n.reconcileSubscriptions()
	// Give the writer a moment to flush before the deferred close.
	time.Sleep(200 * time.Millisecond)
}

// JoinFleet is the joiner side: dial the inviter, pin its certificate, present the token, receive
// membership. Returns the fleet cert/key PEM and the peer list.
func JoinFleet(inv *protocol.Invite, machineID, name, listen string) (*protocol.Joined, string, error) {
	var lastErr error
	for _, addr := range inv.Addrs {
		tcfg := &tls.Config{
			InsecureSkipVerify: true, // we pin instead: the joiner has no CA yet
			MinVersion:         tls.VersionTLS13,
			VerifyConnection: func(cs tls.ConnectionState) error {
				if len(cs.PeerCertificates) == 0 {
					return errors.New("no server certificate")
				}
				if Fingerprint(cs.PeerCertificates[0]) != inv.Fingerprint {
					return errors.New("server certificate does not match the invite")
				}
				return nil
			},
		}
		raw, err := tls.DialWithDialer(&net.Dialer{Timeout: 6 * time.Second}, "tcp", addr, tcfg)
		if err != nil {
			lastErr = fmt.Errorf("%s: %w", addr, err)
			continue
		}
		c := NewConn(raw)
		if err := c.Send(protocol.Join{T: "join", Token: inv.Token, MachineID: machineID, Name: name, Listen: listen}); err != nil {
			c.Close()
			lastErr = err
			continue
		}
		b, err := c.Recv(10 * time.Second)
		c.Close()
		if err != nil {
			lastErr = fmt.Errorf("%s: %w", addr, err)
			continue
		}
		var j protocol.Joined
		if err := json.Unmarshal(b, &j); err != nil {
			return nil, addr, err
		}
		if !j.OK {
			return nil, addr, errors.New(j.Error)
		}
		return &j, addr, nil
	}
	if lastErr == nil {
		lastErr = errors.New("no addresses in invite")
	}
	return nil, "", fmt.Errorf("could not reach the inviter, or the invite was already used or has expired (%v)", lastErr)
}
