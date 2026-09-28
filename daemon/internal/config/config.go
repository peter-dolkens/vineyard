// Package config handles ~/.vineyard: config.json plus the fleet certificate every member shares.
package config

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/peter-dolkens/vineyard/daemon/internal/protocol"
)

const (
	DefaultPort = 7734
	CertFile    = "fleet.crt"
	KeyFile     = "fleet.key"
	ConfigFile  = "config.json"
	// FleetServerName is the SAN on the shared certificate; every member presents and expects it.
	FleetServerName = "vineyard"
)

type Config struct {
	MachineID string              `json:"machineId"`
	Name      string              `json:"name"`
	Listen    string              `json:"listen"`              // e.g. ":7734"
	Advertise string              `json:"advertise,omitempty"` // host:port peers should dial
	Peers     []protocol.PeerAddr `json:"peers"`
	// Removed records machines removed from the fleet and when. Removals spread to every member; a
	// machine is let back in only by a deliberate add (PeerAddr.Added) later than its removal.
	Removed Removals `json:"removed,omitempty"`
	// Added is when this machine was last deliberately admitted (the inviter's clock, from an invite).
	Added int64 `json:"added,omitempty"`
	// KeyAt is when the fleet key in fleet.key was made by a rotation (Unix ms); 0 is the original.
	// A member with a newer key pushes it to one with an older key when they connect.
	KeyAt int64 `json:"keyAt,omitempty"`
	// PrevUntil is when the grace period after the last rotation ends (Unix ms): until then the
	// previous key is still accepted, so machines offline at rotation time can catch up.
	PrevUntil int64 `json:"prevUntil,omitempty"`
	// KeyMembers are the machines that may be handed the current key (see protocol.KeySet).
	KeyMembers []string `json:"keyMembers,omitempty"`
	// Uplink: "auto" (default) holds one idle link to a member when no member can connect to this
	// machine, so it stays reachable through that member while unwatched; "off" never does; any
	// other value is the machine id to prefer for it.
	Uplink    string `json:"uplink,omitempty"`
	ClaudeDir string `json:"claudeDir,omitempty"`
	ClaudeBin string `json:"claudeBin,omitempty"` // path to the claude CLI for managed sessions
	// DisableManaged turns off spawning sessions from Vineyard on this machine.
	DisableManaged bool   `json:"disableManaged,omitempty"`
	TailLines      int    `json:"tailLines,omitempty"`
	LogLevel       string `json:"logLevel,omitempty"`
	// KeepAwakeWhileWatched (macOS): hold off idle sleep while a viewer or peer is subscribed to this
	// machine, so a dozing Mac stays reachable exactly as long as someone is looking. Default true.
	KeepAwakeWhileWatched *bool `json:"keepAwakeWhileWatched,omitempty"`
	// WakePeers: send Wake-on-LAN / a sleep-proxy nudge to peers that do not answer while someone is
	// watching. Default true.
	WakePeersEnabled *bool `json:"wakePeers,omitempty"`
	// WebApp is where this daemon serves the mobile web app, e.g. ":7735"; empty (the default) is
	// off. VS Code's vineyard.webApp setting sets it over the mesh ("webapp" op), stamping WebAppAt
	// so the latest choice wins. Plain HTTP with pairing: securing the route is up to the user.
	WebApp   string `json:"webApp,omitempty"`
	WebAppAt int64  `json:"webAppAt,omitempty"`
	// WebAppHosts are extra Host names the web app answers to (a reverse proxy's, a VPN name).
	WebAppHosts []string `json:"webAppHosts,omitempty"`
	// AllowUnsignedUpgrades lets this daemon install and store vineyardd builds that carry no valid
	// release signature: for a development machine only. It is never set over the mesh.
	AllowUnsignedUpgrades bool `json:"allowUnsignedUpgrades,omitempty"`
}

func (c *Config) WakePeers() bool { return c.WakePeersEnabled == nil || *c.WakePeersEnabled }

func (c *Config) KeepAwake() bool { return c.KeepAwakeWhileWatched == nil || *c.KeepAwakeWhileWatched }

func Dir() string {
	if d := os.Getenv("VINEYARD_DIR"); d != "" {
		return d
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".vineyard"
	}
	return filepath.Join(home, ".vineyard")
}

func Path(name string) string { return filepath.Join(Dir(), name) }

func Load() (*Config, error) {
	b, err := os.ReadFile(Path(ConfigFile))
	if err != nil {
		return nil, err
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", Path(ConfigFile), err)
	}
	c.applyDefaults()
	return &c, nil
}

func (c *Config) applyDefaults() {
	if c.Listen == "" {
		c.Listen = fmt.Sprintf(":%d", DefaultPort)
	}
	if c.TailLines <= 0 {
		c.TailLines = 80
	}
	if c.Name == "" {
		c.Name = ShortName(c.MachineID)
	}
	if c.Peers == nil {
		c.Peers = []protocol.PeerAddr{}
	}
}

func (c *Config) Save() error {
	if err := os.MkdirAll(Dir(), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	tmp := Path(ConfigFile + ".tmp")
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, Path(ConfigFile))
}

// MaxAddrs caps the candidates kept per machine. The list is most recently used first, so when a
// machine moves between networks its new addresses push out the ones it has not used for longest,
// and a laptop that gets a new DHCP lease every day does not grow every member's config forever.
const MaxAddrs = 10

// MergeAddrs returns primary, then fresh, then old, without blanks or duplicates, capped at
// MaxAddrs: a least-recently-used list where primary and fresh are the ones just used or seen.
func MergeAddrs(primary string, fresh, old []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, list := range [][]string{{primary}, fresh, old} {
		for _, a := range list {
			if a == "" || seen[a] || len(out) == MaxAddrs {
				continue
			}
			seen[a] = true
			out = append(out, a)
		}
	}
	return out
}

// Removals is the removal record. It also reads the plain id list written by 0.3.21.
type Removals []protocol.Removal

func (r *Removals) UnmarshalJSON(b []byte) error {
	var recs []protocol.Removal
	if err := json.Unmarshal(b, &recs); err == nil {
		*r = recs
		return nil
	}
	var ids []string
	if err := json.Unmarshal(b, &ids); err != nil {
		return err
	}
	*r = nil
	for _, id := range ids {
		*r = append(*r, protocol.Removal{MachineID: id})
	}
	return nil
}

// AddPeer merges what we know about a peer and returns true if anything changed. p.Addr, when set,
// becomes the primary; p.Addrs are added as candidates. A removed machine comes back only when
// p.Added (a deliberate add) is later than its removal.
func (c *Config) AddPeer(p protocol.PeerAddr) bool {
	if p.MachineID == "" || (p.Addr == "" && len(p.Addrs) == 0) || p.MachineID == c.MachineID {
		return false
	}
	changed := false
	if at, ok := c.RemovedAt(p.MachineID); ok {
		if p.Added <= at {
			return false
		}
		c.unremove(p.MachineID)
		changed = true
	}
	for i, e := range c.Peers {
		if e.MachineID != p.MachineID {
			continue
		}
		primary := e.Addr
		if p.Addr != "" {
			primary = p.Addr
		}
		addrs := MergeAddrs(primary, p.Addrs, e.Addrs)
		if primary == "" {
			primary = addrs[0]
		}
		added := max(e.Added, p.Added)
		if primary == e.Addr && slices.Equal(addrs, e.Addrs) && added == e.Added {
			return changed
		}
		c.Peers[i].Addr = primary
		c.Peers[i].Addrs = addrs
		c.Peers[i].Added = added
		return true
	}
	addrs := MergeAddrs(p.Addr, p.Addrs, nil)
	c.Peers = append(c.Peers, protocol.PeerAddr{MachineID: p.MachineID, Addr: addrs[0], Addrs: addrs, Added: p.Added})
	return true
}

// PeerAdded is when a peer was last deliberately added, or 0.
func (c *Config) PeerAdded(machineID string) int64 {
	for _, e := range c.Peers {
		if e.MachineID == machineID {
			return e.Added
		}
	}
	return 0
}

// RemovePeer removes a machine from the fleet now: forgets it and records the removal, which then
// spreads to the other members.
func (c *Config) RemovePeer(machineID string) bool {
	return c.ApplyRemoval(protocol.Removal{MachineID: machineID, At: time.Now().UnixMilli()})
}

// ApplyRemoval takes a removal (ours or a peer's) unless a later deliberate add, or a record of the
// same removal at least as new, already beats it. Returns true if anything changed.
func (c *Config) ApplyRemoval(r protocol.Removal) bool {
	if r.MachineID == "" || r.MachineID == c.MachineID {
		return false
	}
	if c.PeerAdded(r.MachineID) > r.At {
		return false // added back after this removal
	}
	changed := false
	if at, ok := c.RemovedAt(r.MachineID); !ok || at < r.At {
		c.unremove(r.MachineID)
		c.Removed = append(c.Removed, r)
		changed = true
	}
	if i := slices.IndexFunc(c.Peers, func(p protocol.PeerAddr) bool { return p.MachineID == r.MachineID }); i >= 0 {
		c.Peers = slices.Delete(c.Peers, i, i+1)
		changed = true
	}
	return changed
}

// RemovedAt reports whether a machine is removed, and when.
func (c *Config) RemovedAt(machineID string) (int64, bool) {
	for _, r := range c.Removed {
		if r.MachineID == machineID {
			return r.At, true
		}
	}
	return 0, false
}

func (c *Config) IsRemoved(machineID string) bool {
	_, ok := c.RemovedAt(machineID)
	return ok
}

// RemovalBeats reports whether a machine last admitted at added is still removed.
func (c *Config) RemovalBeats(machineID string, added int64) bool {
	at, ok := c.RemovedAt(machineID)
	return ok && added <= at
}

func (c *Config) unremove(machineID string) {
	c.Removed = slices.DeleteFunc(c.Removed, func(r protocol.Removal) bool { return r.MachineID == machineID })
}

func ShortName(host string) string {
	if i := strings.IndexByte(host, '.'); i > 0 {
		return host[:i]
	}
	return host
}

func DefaultMachineID() string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		return "unknown"
	}
	return strings.ToLower(h)
}

// New builds a fresh config for this machine with sensible defaults.
func New(machineID, name string, port int, advertise string) *Config {
	if machineID == "" {
		machineID = DefaultMachineID()
	}
	if port == 0 {
		port = DefaultPort
	}
	if advertise == "" {
		advertise = fmt.Sprintf("%s:%d", machineID, port)
	}
	c := &Config{
		MachineID: machineID,
		Name:      name,
		Listen:    fmt.Sprintf(":%d", port),
		Advertise: advertise,
		Peers:     []protocol.PeerAddr{},
	}
	c.applyDefaults()
	return c
}

// HasFleetCert reports whether the shared certificate + key exist.
func HasFleetCert() bool {
	_, e1 := os.Stat(Path(CertFile))
	_, e2 := os.Stat(Path(KeyFile))
	return e1 == nil && e2 == nil
}

// GenerateFleetCert creates the one self-signed certificate every fleet member shares. Trust is
// "possession of this key", exactly like a pre-shared key, but expressed as mutual TLS so the standard
// library does all the hard parts. ECDSA P-256, valid for 100 years, acts as its own CA.
func GenerateFleetCert() error {
	if HasFleetCert() {
		return errors.New("fleet certificate already exists")
	}
	if err := os.MkdirAll(Dir(), 0o700); err != nil {
		return err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: FleetServerName, Organization: []string{"Vineyard fleet"}},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(100, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		DNSNames:              []string{FleetServerName},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	if err := os.WriteFile(Path(CertFile), certPEM, 0o600); err != nil {
		return err
	}
	return os.WriteFile(Path(KeyFile), keyPEM, 0o600)
}
