// Package protocol defines the newline-delimited JSON messages exchanged between daemons (peers) and
// between a daemon and a viewer (the VS Code extension or `vineyardd status`).
package protocol

import (
	"encoding/json"

	"github.com/peter-dolkens/vineyard/daemon/internal/model"
)

const Version = 1

// PeerAddr is everything known about how to reach one machine. Addr is the primary (the last address
// a dial succeeded on); Addrs holds up to 10 candidate host:port, most recently used or seen first,
// primary first of all. Daemons older than 0.3.21 send and read Addr only.
type PeerAddr struct {
	MachineID string   `json:"machineId"`
	Addr      string   `json:"addr"` // host:port
	Addrs     []string `json:"addrs,omitempty"`
	// Added is when someone last deliberately added this machine (Add Machine, an invite, peer add),
	// Unix ms. It beats any removal recorded before it; see Removal.
	Added int64 `json:"added,omitempty"`
}

// Removal records that a machine was removed from the fleet, and when (Unix ms). Removals travel in
// hellos and `removed` messages; the later of a removal and a deliberate add wins everywhere.
type Removal struct {
	MachineID string `json:"machineId"`
	At        int64  `json:"at"`
}

// Removed tells connected peers about removals as they happen.
type Removed struct {
	T        string    `json:"t"` // "removed"
	Removals []Removal `json:"removals"`
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
	Addrs []string `json:"addrs,omitempty"`
	// Peers is every other member the sender knows and how to reach it, sent once per connection so
	// a machine that can reach any one member learns about all of them. It is never re-broadcast.
	Peers []PeerAddr `json:"peers,omitempty"`
	// Uplink marks a connection opened by a daemon that may keep it as its uplink (it cannot be
	// reached otherwise); the receiver allows it long silences.
	Uplink bool `json:"uplink,omitempty"`
	// Uplinks lists machines holding an uplink to the sender, so a watcher asks the sender first when
	// it needs a relay to one of them.
	Uplinks []string `json:"uplinks,omitempty"`
	// Removed is every removal the sender knows of, so removals reach the whole fleet.
	Removed []Removal `json:"removed,omitempty"`
	// Added is when the sender itself was last deliberately added (joined with an invite), so a
	// member still holding an older removal of it lets it back in.
	Added int64 `json:"added,omitempty"`
	// KeyAt is when the sender's fleet key was made (Unix ms, 0 = original); the side with the newer
	// key pushes it to the other in a rekey.
	KeyAt int64 `json:"keyAt,omitempty"`
}

// KeySet is a fleet key and, during a rotation grace period, the certificates that bridge it to
// the previous key (see config/keys.go). PEM throughout.
type KeySet struct {
	Cert      string `json:"cert"`
	Key       string `json:"key"`
	Cross     string `json:"cross,omitempty"`
	Prev      string `json:"prev,omitempty"`
	KeyAt     int64  `json:"keyAt"`
	PrevUntil int64  `json:"prevUntil,omitempty"`
}

// Rekey passes a newer fleet key to a peer.
type Rekey struct {
	T    string `json:"t"` // "rekey"
	Keys KeySet `json:"keys"`
}

// RotateArgs asks the local daemon to rotate the fleet key (op "rotatekey", viewers only).
type RotateArgs struct {
	Exclude    []string `json:"exclude,omitempty"`
	GraceHours int      `json:"graceHours,omitempty"`
}

// DialbackArgs asks a member to try connecting to the sender at these addresses (op "dialback").
type DialbackArgs struct {
	Addrs []string `json:"addrs"`
}

type DialbackResult struct {
	Reachable bool   `json:"reachable"`
	Error     string `json:"error,omitempty"`
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
	// Via is how the link runs: "direct", or "relay:<machine id>" when another member relays it.
	Via string `json:"via,omitempty"`
	// LastError is why the last dial failed. While relayed it says why the direct path did not work.
	LastError string `json:"lastError,omitempty"`
	LastSeen  int64  `json:"lastSeen,omitempty"`
}

// Tunnel messages set up a one-hop relay. Each is the first line on a fresh fleet-TLS connection,
// except tunnel-callback, which travels on an existing link.
//
//	tunnel          requester -> relay   "splice me through to Target"
//	tunnel-in       relay -> target      "this connection carries a relayed peer; be its TLS server"
//	tunnel-callback relay -> target      "I cannot dial you: open a fresh connection to me for ID"
//	tunnel-accept   target -> relay      that fresh connection, naming ID
//
// After tunnel-ok the requester and the target run their own TLS handshake inside the spliced bytes,
// so the relay only ever copies ciphertext, and then the ordinary hello and serve loop.
type Tunnel struct {
	T      string `json:"t"`
	ID     string `json:"id"`
	Target string `json:"target,omitempty"`
	From   string `json:"from,omitempty"`
	Relay  string `json:"relay,omitempty"`
}

// TunnelResult is the relay's one reply to a tunnel request.
type TunnelResult struct {
	T     string `json:"t"` // "tunnel-ok" | "tunnel-fail"
	ID    string `json:"id"`
	Error string `json:"error,omitempty"`
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
	// Added is when the inviter admitted the joiner (Unix ms); the joiner puts it in its hellos.
	Added int64 `json:"added,omitempty"`
	// During a rotation grace period, the bridge certificates and the key's timing (see KeySet).
	Cross     string `json:"cross,omitempty"`
	Prev      string `json:"prev,omitempty"`
	KeyAt     int64  `json:"keyAt,omitempty"`
	PrevUntil int64  `json:"prevUntil,omitempty"`
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
	// Attachments ride along with the prompt: image blocks and inlined files for a managed session,
	// inlined text only over the cross-session inbox (images are refused there).
	Attachments []model.Attachment `json:"attachments,omitempty"`
}

// UpgradeArgs streams a new vineyardd binary to the target machine in base64 chunks. The first chunk
// (Offset 0) opens a staging file for SHA256; every chunk must continue exactly where the last one
// ended; Done on the final chunk verifies the hash and hands over to the new binary, which installs
// itself and restarts the service. A daemon accepts "upgrade" from a viewer (relayed by the viewer's
// local daemon) and from a peer: after a daemon is upgraded it pushes its own version to every peer
// that reports an older one, so the extension only ever has to update the daemon next to it.
//
// The same chunk stream under the "stage" op stores a binary for another platform (Platform set to
// "<os>-<arch>", Version equal to the receiving daemon's own) in that daemon's distribution store
// instead of installing it. A second sender starting at Offset 0 while an upload of the same digest is
// active is answered with an "already in progress" error; an upload idle for a minute is abandoned.
type UpgradeArgs struct {
	Version  string `json:"version"`            // version of the binary being sent
	SHA256   string `json:"sha256"`             // hex digest of the whole file
	Size     int64  `json:"size"`               // total bytes
	Offset   int64  `json:"offset"`             // byte offset of this chunk
	Data     string `json:"data,omitempty"`     // base64 chunk
	Done     bool   `json:"done,omitempty"`     // last chunk: verify and install (or store)
	Force    bool   `json:"force,omitempty"`    // install even if the version matches or is older than the running one
	Platform string `json:"platform,omitempty"` // "stage" only: "<os>-<arch>" of the binary
}

type UpgradeResult struct {
	Received  int64  `json:"received"`            // bytes staged so far
	Installed bool   `json:"installed,omitempty"` // the new binary was verified and is taking over
	Stored    bool   `json:"stored,omitempty"`    // "stage": the binary is in the distribution store
	Version   string `json:"version,omitempty"`   // version reported by the staged binary
}

// DistResult answers "dist": which platforms this daemon can already upgrade (its own build plus the
// store), and which its known peers run that it has no binary for. The extension seeds Want.
type DistResult struct {
	Version string   `json:"version"`        // the daemon's own version, the only one it distributes
	Have    []string `json:"have"`           // "<os>-<arch>" available now
	Want    []string `json:"want,omitempty"` // "<os>-<arch>" of peers that Have does not cover
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

// RenameArgs gives a session a custom title (what /rename does in Claude Code).
type RenameArgs struct {
	SessionID string `json:"sessionId"`
	Title     string `json:"title"`
	Path      string `json:"path,omitempty"` // transcript path, for sessions this daemon does not manage
	Cwd       string `json:"cwd,omitempty"`  // used to locate the transcript when Path is empty
}

// StopTaskArgs stops one running background task of a managed session ("stoptask", Claude Code's
// stop_task control request): TaskID is the id of a backgrounded shell command as the Bash
// tool_result reported it, or a subagent's agent id (the <id> of subagents/agent-<id>.jsonl).
type StopTaskArgs struct {
	SessionID string `json:"sessionId"`
	TaskID    string `json:"taskId"`
}

// RespondArgs answers a managed session's pending control request.
type RespondArgs struct {
	SessionID string          `json:"sessionId"`
	RequestID string          `json:"requestId"`
	Response  json.RawMessage `json:"response"` // {"behavior":"allow","updatedInput":{...}} | {"behavior":"deny","message":"..."}
}
