// Package model holds the wire types shared between the daemon and the VS Code extension.
// JSON field names are camelCase to match the TypeScript side exactly.
package model

import "encoding/json"

// AgentState describes what an agent is doing, ordered roughly by how urgently the human is needed.
type AgentState string

const (
	StateQuestion   AgentState = "question"   // asked a question and is blocked on an answer
	StatePermission AgentState = "permission" // blocked on a permission prompt or other input
	StateWorking    AgentState = "working"    // the model is generating
	StateThinking   AgentState = "thinking"   // extended thinking block in progress
	StateTool       AgentState = "tool"       // a tool call is executing
	StateShell      AgentState = "shell"      // the user dropped into a shell inside the CLI
	StateIdle       AgentState = "idle"       // finished its turn, waiting for the next prompt
	StateDone       AgentState = "done"       // a subagent that returned its result to its parent
	StateExited     AgentState = "exited"     // process gone; registry entry is stale
	StateUnknown    AgentState = "unknown"
)

// StatePriority: lower = needs attention sooner.
var StatePriority = map[AgentState]int{
	StateQuestion: 0, StatePermission: 1, StateWorking: 2, StateThinking: 3,
	StateTool: 4, StateShell: 5, StateIdle: 6, StateDone: 7, StateUnknown: 8, StateExited: 9,
}

func NeedsAttention(s AgentState) bool { return s == StateQuestion || s == StatePermission }
func IsBusy(s AgentState) bool {
	return s == StateWorking || s == StateThinking || s == StateTool
}

type PendingTool struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Summary string `json:"summary,omitempty"`
}

type Agent struct {
	ID             string        `json:"id"` // machineId::sessionId
	Provider       string        `json:"provider"`
	MachineID      string        `json:"machineId"`
	WorkspacePath  string        `json:"workspacePath"`
	SessionID      string        `json:"sessionId"`
	PID            int           `json:"pid,omitempty"`
	Alive          bool          `json:"alive"`
	Name           string        `json:"name,omitempty"`
	Title          string        `json:"title,omitempty"`
	Kind           string        `json:"kind,omitempty"`
	Entrypoint     string        `json:"entrypoint,omitempty"`
	Version        string        `json:"version,omitempty"`
	State          AgentState    `json:"state"`
	StateDetail    string        `json:"stateDetail,omitempty"`
	RegistryStatus string        `json:"registryStatus,omitempty"`
	Model          string        `json:"model,omitempty"`
	Effort         string        `json:"effort,omitempty"`
	PermissionMode string        `json:"permissionMode,omitempty"`
	GitBranch      string        `json:"gitBranch,omitempty"`
	LastPrompt     string        `json:"lastPrompt,omitempty"`
	StartedAt      int64         `json:"startedAt,omitempty"`
	LastActivityAt int64         `json:"lastActivityAt,omitempty"`
	ContextTokens  int64         `json:"contextTokens,omitempty"`
	PendingTools   []PendingTool `json:"pendingTools"`
	TranscriptPath string        `json:"transcriptPath,omitempty"`
	// Managed is set when this daemon spawned the session and controls it over stream-json.
	Managed *ManagedInfo `json:"managed,omitempty"`
	// Subagents are the Agent-tool invocations under this session, every depth, in spawn order.
	Subagents []Subagent `json:"subagents,omitempty"`
}

// Subagent is one Agent-tool invocation: its own transcript beside the session's, with a state
// derived the same way. ParentAgentID is empty when the session itself spawned it; otherwise it
// names another Subagent of the same session, so the list is a flat encoding of a tree.
type Subagent struct {
	AgentID        string        `json:"agentId"`
	ParentAgentID  string        `json:"parentAgentId,omitempty"`
	Depth          int           `json:"depth,omitempty"`
	Type           string        `json:"type,omitempty"`        // general-purpose, Explore, claude, …
	Description    string        `json:"description,omitempty"` // the 3–5 word label the parent gave it
	Model          string        `json:"model,omitempty"`
	Background     bool          `json:"background,omitempty"`
	State          AgentState    `json:"state"`
	StateDetail    string        `json:"stateDetail,omitempty"`
	PendingTools   []PendingTool `json:"pendingTools,omitempty"`
	StartedAt      int64         `json:"startedAt,omitempty"`
	LastActivityAt int64         `json:"lastActivityAt,omitempty"`
	ContextTokens  int64         `json:"contextTokens,omitempty"`
	TranscriptPath string        `json:"transcriptPath,omitempty"`
	ToolUseID      string        `json:"toolUseId,omitempty"`
}

// PendingRequest is a control_request the managed session is blocked on: a permission prompt or an
// AskUserQuestion. Input is the tool input as Claude proposed it.
type PendingRequest struct {
	RequestID               string          `json:"requestId"`
	ToolName                string          `json:"toolName"`
	DisplayName             string          `json:"displayName,omitempty"`
	Input                   json.RawMessage `json:"input,omitempty"`
	ToolUseID               string          `json:"toolUseId,omitempty"`
	RequiresUserInteraction bool            `json:"requiresUserInteraction,omitempty"`
	Suggestions             json.RawMessage `json:"suggestions,omitempty"`
	Description             string          `json:"description,omitempty"`
	At                      int64           `json:"at"`
}

// ModelInfo is one row of the model picker Claude Code offers this account, as the harness reports
// it over the control protocol when a managed session starts. Value is what set_model / --model
// accept ("default" means Claude Code's own default); ResolvedModel is the wire id it maps to.
type ModelInfo struct {
	Value                 string   `json:"value"`
	ResolvedModel         string   `json:"resolvedModel,omitempty"`
	DisplayName           string   `json:"displayName"`
	Description           string   `json:"description,omitempty"`
	SupportedEffortLevels []string `json:"supportedEffortLevels,omitempty"` // empty: the model takes no effort setting
}

// CommandInfo is one slash command the session's Claude Code offers (built-in, project, plugin or
// skill), as the initialize handshake lists them. Name has no leading slash.
type CommandInfo struct {
	Name         string `json:"name"`
	Description  string `json:"description,omitempty"`
	ArgumentHint string `json:"argumentHint,omitempty"`
}

// Attachment is a file sent with a prompt. Images (png, jpeg, gif, webp) become image blocks for a
// managed session; anything else is inlined as text. Data is standard base64.
type Attachment struct {
	Name      string `json:"name"`
	MediaType string `json:"mediaType"`
	Data      string `json:"data"`
}

type ManagedInfo struct {
	SessionID      string          `json:"sessionId"`
	PID            int             `json:"pid"`
	Cwd            string          `json:"cwd"`
	Name           string          `json:"name,omitempty"`
	Model          string          `json:"model,omitempty"`
	Effort         string          `json:"effort,omitempty"`
	PermissionMode string          `json:"permissionMode,omitempty"`
	StartedAt      int64           `json:"startedAt"`
	Ready          bool            `json:"ready"`
	Resumed        bool            `json:"resumed,omitempty"`
	Pending        *PendingRequest `json:"pending,omitempty"`
	Turns          int             `json:"turns"`
	CostUSD        float64         `json:"costUsd,omitempty"`
	Exited         bool            `json:"exited"`
	ExitedAt       int64           `json:"exitedAt,omitempty"`
	LastError      string          `json:"lastError,omitempty"`
	// Models is what this session's Claude Code offers in its model picker; empty until the harness
	// has answered the initialize request sent at spawn.
	Models []ModelInfo `json:"models,omitempty"`
	// Commands are the slash commands the session offers, from the same handshake; Account is the
	// e-mail it is signed in as.
	Commands []CommandInfo `json:"commands,omitempty"`
	Account  string        `json:"account,omitempty"`
}

type Workspace struct {
	ID             string  `json:"id"` // machineId::path
	MachineID      string  `json:"machineId"`
	Path           string  `json:"path"`
	Agents         []Agent `json:"agents"`
	HistoryCount   int     `json:"historyCount"`
	LastActivityAt int64   `json:"lastActivityAt,omitempty"`
	OpenInIDE      bool    `json:"openInIde"`
}

type HostInfo struct {
	Hostname  string `json:"hostname,omitempty"`
	OS        string `json:"os,omitempty"`
	Arch      string `json:"arch,omitempty"`
	Home      string `json:"home,omitempty"`
	ClaudeDir string `json:"claudeDir,omitempty"`
	Now       int64  `json:"now,omitempty"`
	// MACs are the hardware addresses of this machine's active interfaces, so peers can send
	// Wake-on-LAN when it sleeps.
	MACs []string `json:"macs,omitempty"`
}

// Snapshot is one machine's complete self-report. It is idempotent: a newer At always supersedes.
type Snapshot struct {
	MachineID     string      `json:"machineId"`
	Name          string      `json:"name"`
	Host          HostInfo    `json:"host"`
	Agents        []Agent     `json:"agents"`
	Workspaces    []Workspace `json:"workspaces"`
	At            int64       `json:"at"` // producer clock, epoch ms
	Seq           uint64      `json:"seq"`
	DaemonVersion string      `json:"daemonVersion,omitempty"`
	Listen        string      `json:"listen,omitempty"` // advertised host:port
	HasClaude     bool        `json:"hasClaude"`
}

// FleetEntry is what viewers see: a snapshot plus how and when this daemon learned about it.
type FleetEntry struct {
	Snapshot   Snapshot `json:"snapshot"`
	Online     bool     `json:"online"`
	Via        string   `json:"via"` // self | direct | gossip
	LastSeen   int64    `json:"lastSeen"`
	ReceivedAt int64    `json:"receivedAt"`
}
