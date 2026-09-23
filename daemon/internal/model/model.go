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
	// Input is the full tool input for an AskUserQuestion (questions, options), so a viewer can show
	// the question and, after a takeover, ask it again; omitted for every other tool.
	Input json.RawMessage `json:"input,omitempty"`
}

type Agent struct {
	ID            string `json:"id"` // machineId::sessionId
	Provider      string `json:"provider"`
	MachineID     string `json:"machineId"`
	WorkspacePath string `json:"workspacePath"`
	// Cwd is the directory the process actually runs in. It differs from WorkspacePath only for a
	// session inside a Claude Code scratchpad, which is shown under the project that spawned it; a
	// resume must run where the session ran.
	Cwd            string     `json:"cwd,omitempty"`
	SessionID      string     `json:"sessionId"`
	PID            int        `json:"pid,omitempty"`
	Alive          bool       `json:"alive"`
	Name           string     `json:"name,omitempty"`
	Title          string     `json:"title,omitempty"`
	Kind           string     `json:"kind,omitempty"`
	Entrypoint     string     `json:"entrypoint,omitempty"`
	Version        string     `json:"version,omitempty"`
	State          AgentState `json:"state"`
	StateDetail    string     `json:"stateDetail,omitempty"`
	RegistryStatus string     `json:"registryStatus,omitempty"`
	Model          string     `json:"model,omitempty"`
	Effort         string     `json:"effort,omitempty"`
	PermissionMode string     `json:"permissionMode,omitempty"`
	GitBranch      string     `json:"gitBranch,omitempty"`
	LastPrompt     string     `json:"lastPrompt,omitempty"`
	StartedAt      int64      `json:"startedAt,omitempty"`
	LastActivityAt int64      `json:"lastActivityAt,omitempty"`
	ContextTokens  int64      `json:"contextTokens,omitempty"`
	// ContextWindow is the model's context size in tokens as Claude Code reported it (managed sessions
	// only); 0 means "unknown, infer from the model id". ContextAt is when ContextTokens was measured
	// (epoch ms), so a fresher figure from the control channel can replace a transcript-derived one.
	ContextWindow  int64         `json:"contextWindow,omitempty"`
	ContextAt      int64         `json:"contextAt,omitempty"`
	PendingTools   []PendingTool `json:"pendingTools"`
	TranscriptPath string        `json:"transcriptPath,omitempty"`
	// Managed is set when this daemon spawned the session and controls it over stream-json.
	Managed *ManagedInfo `json:"managed,omitempty"`
	// Subagents are the Agent-tool invocations under this session, every depth, in spawn order.
	Subagents []Subagent `json:"subagents,omitempty"`
	// Tasks are the shell commands the session left running in the background, plus ones that
	// finished in the last half hour, in start order.
	Tasks []BackgroundTask `json:"tasks,omitempty"`
}

// BackgroundTask is a Bash call made with run_in_background, or a foreground one that outlived its
// timeout and was moved to the background, tracked until its <task-notification> arrives.
type BackgroundTask struct {
	ToolUseID   string `json:"toolUseId"`
	TaskID      string `json:"taskId,omitempty"`
	Description string `json:"description,omitempty"`
	Kind        string `json:"kind,omitempty"` // "shell"
	State       string `json:"state"`          // running | completed | failed | …
	StartedAt   int64  `json:"startedAt,omitempty"`
	EndedAt     int64  `json:"endedAt,omitempty"`
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
	// Kind says what is being asked: "" or PendingPermission for a can_use_tool prompt (including
	// AskUserQuestion), PendingElicitation for an MCP server's question, carried in Elicitation, or
	// PendingRecovered for an AskUserQuestion the session was blocked on when Vineyard took it over:
	// there is no control_request behind it, so the answer is sent as a user message instead.
	Kind        string              `json:"kind,omitempty"`
	Elicitation *ElicitationRequest `json:"elicitation,omitempty"`
}

const (
	PendingPermission  = "permission"
	PendingElicitation = "elicitation"
	PendingRecovered   = "recovered"
)

// ElicitationRequest is an MCP server's question to the user, relayed by Claude Code as a
// control_request of subtype "elicitation". Mode is "form" (answer the fields RequestedSchema
// describes, a JSON Schema object with properties and required) or "url" (open URL, then confirm).
// The answer is {"action":"accept","content":{...}}, {"action":"decline"} or {"action":"cancel"}.
type ElicitationRequest struct {
	ServerName      string          `json:"serverName"`
	DisplayName     string          `json:"displayName,omitempty"`
	Message         string          `json:"message,omitempty"`
	Mode            string          `json:"mode,omitempty"`
	URL             string          `json:"url,omitempty"`
	ElicitationID   string          `json:"elicitationId,omitempty"`
	RequestedSchema json.RawMessage `json:"requestedSchema,omitempty"`
	Title           string          `json:"title,omitempty"`
	Description     string          `json:"description,omitempty"`
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
	// RecoveredDone is the tool_use id of a recovered question that has been answered (or waved off
	// with a prompt). The transcript keeps that AskUserQuestion dangling for a moment after the
	// answer is sent, until Claude Code appends the prompt; Merge hides it meanwhile.
	RecoveredDone string  `json:"recoveredDone,omitempty"`
	Turns         int     `json:"turns"`
	CostUSD       float64 `json:"costUsd,omitempty"`
	Exited        bool    `json:"exited"`
	ExitedAt      int64   `json:"exitedAt,omitempty"`
	LastError     string  `json:"lastError,omitempty"`
	// Models is what this session's Claude Code offers in its model picker; empty until the harness
	// has answered the initialize request sent at spawn.
	Models []ModelInfo `json:"models,omitempty"`
	// Commands are the slash commands the session offers, from the same handshake; Account is the
	// e-mail it is signed in as.
	Commands    []CommandInfo `json:"commands,omitempty"`
	Account     string        `json:"account,omitempty"`
	AccountOrg  string        `json:"accountOrg,omitempty"`
	AccountPlan string        `json:"accountPlan,omitempty"`
	// Usage is the account's limit report as this session last saw it (rate_limit_event).
	Usage *Usage `json:"usage,omitempty"`
	// Context is Claude Code's own measurement of the context window (get_context_usage), taken only
	// when the transcript cannot tell: after the handshake, a model switch and a compaction.
	Context *ContextUsage `json:"context,omitempty"`
	// Compacting is true from Claude Code's "compacting" status until the compact_boundary that ends it.
	Compacting      bool  `json:"compacting,omitempty"`
	CompactingSince int64 `json:"compactingSince,omitempty"`
}

// ContextUsage is the answer to a get_context_usage control request, trimmed to what the ring needs:
// tokens in the window, the window's size, and Claude Code's own rounded percentage.
type ContextUsage struct {
	TotalTokens int64   `json:"totalTokens"`
	MaxTokens   int64   `json:"maxTokens"`
	Percentage  float64 `json:"percentage"`
	Model       string  `json:"model,omitempty"`
	At          int64   `json:"at"` // when the daemon received it, epoch ms
}

// UsageWindow is one rate-limit window: share used (0..1) and when it resets (epoch ms).
type UsageWindow struct {
	Utilization float64 `json:"utilization"`
	ResetsAt    int64   `json:"resetsAt,omitempty"`
}

// Usage is what Claude Code reports about the account's limits, from the API's rate-limit headers:
// a status for the request that triggered it, the window that status refers to, and every window.
// Window keys: five_hour, seven_day, seven_day_opus, seven_day_sonnet, seven_day_overage_included.
type Usage struct {
	Status         string                 `json:"status"` // allowed | allowed_warning | rejected
	RateLimitType  string                 `json:"rateLimitType,omitempty"`
	Utilization    float64                `json:"utilization,omitempty"`
	ResetsAt       int64                  `json:"resetsAt,omitempty"`
	Windows        map[string]UsageWindow `json:"windows,omitempty"`
	IsUsingOverage bool                   `json:"isUsingOverage,omitempty"`
	OverageStatus  string                 `json:"overageStatus,omitempty"`
	At             int64                  `json:"at"` // when the daemon saw it, epoch ms
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
	// Usage is the newest account-limit report from any session this daemon manages. Limits are
	// per account, so it applies to every session on the machine signed in as that account.
	Usage *Usage `json:"usage,omitempty"`
}

// FleetEntry is what viewers see: a snapshot plus how and when this daemon learned about it.
type FleetEntry struct {
	Snapshot   Snapshot `json:"snapshot"`
	Online     bool     `json:"online"`
	Via        string   `json:"via"` // self | direct | gossip
	LastSeen   int64    `json:"lastSeen"`
	ReceivedAt int64    `json:"receivedAt"`
}
