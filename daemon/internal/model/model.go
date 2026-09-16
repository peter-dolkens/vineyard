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
	StateExited     AgentState = "exited"     // process gone; registry entry is stale
	StateUnknown    AgentState = "unknown"
)

// StatePriority: lower = needs attention sooner.
var StatePriority = map[AgentState]int{
	StateQuestion: 0, StatePermission: 1, StateWorking: 2, StateThinking: 3,
	StateTool: 4, StateShell: 5, StateIdle: 6, StateUnknown: 7, StateExited: 8,
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
