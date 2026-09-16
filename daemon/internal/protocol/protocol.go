// Package protocol defines the newline-delimited JSON messages exchanged between daemons (peers) and
// between a daemon and a viewer (the VS Code extension or `vineyardd status`).
package protocol

import (
	"encoding/json"

	"github.com/peter-dolkens/vineyard/daemon/internal/model"
)

const Version = 1

type PeerAddr struct {
	MachineID string `json:"machineId"`
	Addr      string `json:"addr"` // host:port
}

// Envelope is decoded first to learn the message type.
type Envelope struct {
	T string `json:"t"`
}

type Hello struct {
	T         string     `json:"t"`    // "hello"
	Role      string     `json:"role"` // "peer" | "viewer"
	MachineID string     `json:"machineId"`
	Name      string     `json:"name,omitempty"`
	Version   string     `json:"version,omitempty"`
	Protocol  int        `json:"protocol"`
	Listen    string     `json:"listen,omitempty"`
	Peers     []PeerAddr `json:"peers,omitempty"`
}

type SnapshotMsg struct {
	T        string         `json:"t"` // "snapshot"
	Snapshot model.Snapshot `json:"snapshot"`
}

// Sync is sent right after hello: everything the sender knows, so a fresh connection needs no history.
type Sync struct {
	T       string             `json:"t"` // "sync"
	Entries []model.FleetEntry `json:"entries"`
}

type Ping struct {
	T string `json:"t"` // "ping" | "pong"
}

type PeersMsg struct {
	T     string     `json:"t"` // "peers"
	Peers []PeerAddr `json:"peers"`
}

// Fleet is the viewer's full picture; Update is one entry that changed.
type Fleet struct {
	T       string             `json:"t"` // "fleet"
	Self    string             `json:"self"`
	Entries []model.FleetEntry `json:"entries"`
	Peers   []PeerStatus       `json:"peers"`
}

type Update struct {
	T     string           `json:"t"` // "update"
	Entry model.FleetEntry `json:"entry"`
}

type PeerStatus struct {
	MachineID string `json:"machineId"`
	Addr      string `json:"addr"`
	Connected bool   `json:"connected"`
	LastError string `json:"lastError,omitempty"`
	LastSeen  int64  `json:"lastSeen,omitempty"`
}

type PeersUpdate struct {
	T     string       `json:"t"` // "peerstatus"
	Peers []PeerStatus `json:"peers"`
}

// Request/Response carry one-shot operations (fetch a transcript, force a probe) from a viewer to any
// machine; the local daemon relays to the target over its direct peer connection.
type Request struct {
	T      string          `json:"t"` // "req"
	ID     string          `json:"id"`
	Target string          `json:"target"` // machineId
	Op     string          `json:"op"`     // "transcript" | "probe" | "ping"
	Args   json.RawMessage `json:"args,omitempty"`
}

type Response struct {
	T     string          `json:"t"` // "res"
	ID    string          `json:"id"`
	OK    bool            `json:"ok"`
	Data  json.RawMessage `json:"data,omitempty"`
	Error string          `json:"error,omitempty"`
}

type TranscriptArgs struct {
	SessionID string `json:"sessionId"`
	Path      string `json:"path,omitempty"`
	Cwd       string `json:"cwd,omitempty"`
	Lines     int    `json:"lines,omitempty"`
}

type TranscriptData struct {
	Path    string            `json:"path"`
	Entries []json.RawMessage `json:"entries"`
}

// Join is the only message an unauthenticated (no client certificate) connection may send, and only
// while the receiving daemon has an outstanding invite. Joined delivers fleet membership.
type Join struct {
	T         string `json:"t"` // "join"
	Token     string `json:"token"`
	MachineID string `json:"machineId"`
	Name      string `json:"name,omitempty"`
	Listen    string `json:"listen,omitempty"` // joiner's advertised host:port, if it can accept connections
}

type Joined struct {
	T     string     `json:"t"` // "joined"
	OK    bool       `json:"ok"`
	Error string     `json:"error,omitempty"`
	Cert  string     `json:"cert,omitempty"` // PEM
	Key   string     `json:"key,omitempty"`  // PEM
	Peers []PeerAddr `json:"peers,omitempty"`
}

// Invite is what gets encoded into the shareable code.
type Invite struct {
	V           int      `json:"v"`
	Addrs       []string `json:"a"` // inviter addresses to try, host:port
	Fingerprint string   `json:"f"` // base64url(SHA-256(cert DER))[:22] – pins the inviter
	Token       string   `json:"t"` // single-use secret
	Name        string   `json:"n"` // inviter display name
	MachineID   string   `json:"m"` // inviter machine id
}

const InvitePrefix = "vineyard:"
