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
	T         string `json:"t"`    // "hello"
	Role      string `json:"role"` // "peer" | "viewer"
	MachineID string `json:"machineId"`
	Name      string `json:"name,omitempty"`
	Version   string `json:"version,omitempty"`
	Protocol  int    `json:"protocol"`
	Listen    string `json:"listen,omitempty"`
	// Addrs lists every address the sender can be reached at (advertised name first, then LAN IPs),
	// so peers survive stale DNS and DHCP changes without anyone editing config.
	Addrs []string   `json:"addrs,omitempty"`
	Peers []PeerAddr `json:"peers,omitempty"`
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
	// Offset, when > 0, asks for only the complete lines written after this byte offset (as returned
	// in a previous TranscriptData.Offset). Lets a viewer stream a live transcript cheaply.
	Offset int64 `json:"offset,omitempty"`
}

type TranscriptData struct {
	Path    string            `json:"path"`
	Entries []json.RawMessage `json:"entries"`
	// Offset is the byte position just after the last complete line returned; pass it back to get
	// only newer lines. Size is the file size at read time.
	Offset int64 `json:"offset"`
	Size   int64 `json:"size"`
	// Truncated is true when the file shrank or was replaced since the caller's offset (re-read).
	Truncated bool `json:"truncated,omitempty"`
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

// SendArgs posts a message into a running session on the target machine.
type SendArgs struct {
	SessionID string `json:"sessionId"`
	Text      string `json:"text"`
}

// UpgradeArgs streams a new vineyardd binary to the target machine in base64 chunks. The first chunk
// (Offset 0) opens a staging file for SHA256; every chunk must continue exactly where the last one
// ended; Done on the final chunk verifies the hash and hands over to the new binary, which installs
// itself and restarts the service. The viewer relays these through its local daemon like any request,
// so no SSH is involved.
type UpgradeArgs struct {
	Version string `json:"version"`         // version of the binary being sent (informational)
	SHA256  string `json:"sha256"`          // hex digest of the whole file
	Size    int64  `json:"size"`            // total bytes
	Offset  int64  `json:"offset"`          // byte offset of this chunk
	Data    string `json:"data,omitempty"`  // base64 chunk
	Done    bool   `json:"done,omitempty"`  // last chunk: verify and install
	Force   bool   `json:"force,omitempty"` // install even if the version matches the running one
}

type UpgradeResult struct {
	Received  int64  `json:"received"`            // bytes staged so far
	Installed bool   `json:"installed,omitempty"` // the new binary was verified and is taking over
	Version   string `json:"version,omitempty"`   // version reported by the staged binary
}

// ConfigureArgs changes a running managed session's model, effort or permission mode. A nil field is
// left alone; an empty string resets to Claude Code's default where that makes sense (model).
type ConfigureArgs struct {
	SessionID      string  `json:"sessionId"`
	Model          *string `json:"model,omitempty"`
	Effort         *string `json:"effort,omitempty"`
	PermissionMode *string `json:"permissionMode,omitempty"`
}

// SessionsArgs lists past transcripts on the target machine (Cwd narrows to one workspace).
type SessionsArgs struct {
	Cwd   string `json:"cwd,omitempty"`
	Limit int    `json:"limit,omitempty"`
}

// LoginArgs drives `claude auth login` on the target: "start" returns {id, url} for the viewer to open
// in its own browser, "code" delivers the pasted authorization code, "cancel" abandons the attempt.
type LoginArgs struct {
	Action  string `json:"action"`
	ID      string `json:"id,omitempty"`
	Code    string `json:"code,omitempty"`
	Console bool   `json:"console,omitempty"` // Anthropic Console (API billing) instead of a Claude subscription
}

// RespondArgs answers a managed session's pending control request.
type RespondArgs struct {
	SessionID string          `json:"sessionId"`
	RequestID string          `json:"requestId"`
	Response  json.RawMessage `json:"response"` // {"behavior":"allow","updatedInput":{...}} | {"behavior":"deny","message":"..."}
}
