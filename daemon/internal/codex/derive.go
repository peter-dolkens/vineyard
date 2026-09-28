package codex

import (
	"encoding/json"
	"regexp"
	"sort"
	"strings"

	"github.com/peter-dolkens/vineyard/daemon/internal/claude"
	"github.com/peter-dolkens/vineyard/daemon/internal/model"
)

// Provider is the Agent.Provider value for Codex threads.
const Provider = "codex"

// Derived is everything a rollout tail tells us about a thread.
type Derived struct {
	Cwd            string
	Originator     string // codex_vscode, codex_exec, codex_cli, …
	Version        string // cli_version
	GitBranch      string
	Model          string
	Effort         string
	ApprovalPolicy string // untrusted | on-request | never
	Sandbox        string // read-only | workspace-write | danger-full-access
	FirstPrompt    string
	LastPrompt     string
	StartedAt      int64
	LastActivityAt int64
	State          model.AgentState
	StateDetail    string
	PendingTools   []model.PendingTool
	// ActiveTurn is the turn_id of a turn that started and has not completed in the tail.
	ActiveTurn string
	// ContextTokens is the size of the last request (input + output of the last response);
	// ContextWindow the model's window as Codex reported it; ContextAt the line's timestamp.
	ContextTokens int64
	ContextWindow int64
	ContextAt     int64
	// Usage is the account's rate-limit picture as the last token_count line carried it.
	Usage *model.Usage
}

// Mode folds Codex's sandbox and approval policy into one permission-mode value, the values the
// picker offers (see Modes): full access, or ask-for-everything, or the sandbox level.
func Mode(sandbox, approval string) string {
	switch {
	case sandbox == "danger-full-access":
		return "danger-full-access"
	case approval == "untrusted":
		return "untrusted"
	case sandbox == "read-only":
		return "read-only"
	default:
		return "workspace-write"
	}
}

// ModeSettings is the inverse of Mode: the thread/start (or turn/start) parameters for a mode.
// Sandbox is the SandboxMode string thread/start takes; Policy the SandboxPolicy object for turn/start.
func ModeSettings(mode string) (sandbox, approval string, policy map[string]any) {
	switch mode {
	case "danger-full-access":
		return "danger-full-access", "never", map[string]any{"type": "dangerFullAccess"}
	case "untrusted":
		return "workspace-write", "untrusted", map[string]any{"type": "workspaceWrite"}
	case "read-only":
		return "read-only", "on-request", map[string]any{"type": "readOnly"}
	default:
		return "workspace-write", "on-request", map[string]any{"type": "workspaceWrite"}
	}
}

// Modes is the permission-mode picker for a managed Codex thread, in place of Claude Code's modes.
var Modes = []model.ModeInfo{
	{Value: "workspace-write", Label: "Workspace write", Description: "Codex edits files and runs commands inside the workspace; anything outside it asks.", Icon: "edit"},
	{Value: "read-only", Label: "Read only", Description: "Codex can read the workspace; every edit and command asks.", Icon: "eye"},
	{Value: "untrusted", Label: "Ask for everything", Description: "Every command asks before it runs.", Icon: "hand"},
	{Value: "danger-full-access", Label: "Full access", Description: "No sandbox and nothing is asked. Every command runs.", Icon: "unlock"},
}

// ---- tool calls -----------------------------------------------------------------------------------

var cmdRe = regexp.MustCompile(`\bcmd\s*:\s*"((?:[^"\\]|\\.)*)"`)

// shellCommand pulls the command out of a code-mode exec call
// (`text(await tools.exec_command({cmd:"ls -la", …}))`); "" when it does not look like one.
func shellCommand(code string) string {
	m := cmdRe.FindStringSubmatch(code)
	if m == nil {
		return ""
	}
	var s string
	if json.Unmarshal([]byte(`"`+m[1]+`"`), &s) != nil {
		return ""
	}
	return s
}

var patchFileRe = regexp.MustCompile(`\*\*\* (?:Add|Update|Delete) File: (.+)`)

// call is a tool call as the transcript shows it: Claude-style name and input so the viewer's
// existing tool rows (Bash gets IN/OUT blocks, Edit a file path) apply.
type call struct {
	ID      string
	Name    string
	Input   map[string]any
	Summary string
}

// callOf decodes a response_item that starts a tool call; ok is false for other items.
func callOf(l Line) (call, bool) {
	p := l.Payload
	switch l.itemType() {
	case "function_call":
		name := str(p["name"])
		c := call{ID: str(p["call_id"]), Name: name}
		if ns := str(p["namespace"]); ns != "" {
			c.Name = strings.TrimPrefix(ns, "mcp__") + ": " + name
		}
		var args map[string]any
		if json.Unmarshal([]byte(str(p["arguments"])), &args) == nil && args != nil {
			c.Input = args
		} else {
			c.Input = map[string]any{"arguments": str(p["arguments"])}
		}
		if name == "shell" || name == "exec_command" || name == "container.exec" {
			if cmd, ok := c.Input["command"]; ok {
				c.Name = "Bash"
				if arr, ok := cmd.([]any); ok {
					var parts []string
					for _, a := range arr {
						parts = append(parts, str(a))
					}
					c.Input = map[string]any{"command": strings.Join(parts, " ")}
				}
			} else if cmd := str(c.Input["cmd"]); cmd != "" {
				c.Name = "Bash"
				c.Input = map[string]any{"command": cmd}
			}
		}
		c.Summary = claude.SummarizeToolInput(c.Name, c.Input)
		if c.Summary == "" {
			c.Summary = firstStringArg(c.Input)
		}
		return c, true
	case "custom_tool_call":
		name := str(p["name"])
		input := str(p["input"])
		c := call{ID: str(p["call_id"]), Name: name}
		switch name {
		case "exec", "shell", "exec_command":
			if cmd := shellCommand(input); cmd != "" {
				c.Name = "Bash"
				c.Input = map[string]any{"command": cmd}
			} else {
				c.Input = map[string]any{"code": input}
			}
		case "apply_patch":
			c.Name = "Edit"
			c.Input = map[string]any{"patch": input}
			if m := patchFileRe.FindStringSubmatch(input); m != nil {
				c.Input["file_path"] = strings.TrimSpace(m[1])
			}
		default:
			c.Input = map[string]any{"input": input}
		}
		c.Summary = claude.SummarizeToolInput(c.Name, c.Input)
		if c.Summary == "" {
			c.Summary = clip(firstLine(input), 120)
		}
		return c, true
	case "local_shell_call":
		action := obj(p["action"])
		var parts []string
		if arr, ok := action["command"].([]any); ok {
			for _, a := range arr {
				parts = append(parts, str(a))
			}
		}
		cmd := strings.Join(parts, " ")
		c := call{ID: str(p["call_id"]), Name: "Bash", Input: map[string]any{"command": cmd}, Summary: clip(cmd, 120)}
		if c.ID == "" {
			c.ID = str(p["id"])
		}
		return c, true
	case "web_search_call":
		q := str(obj(p["action"])["query"])
		return call{ID: str(p["id"]), Name: "WebSearch", Input: map[string]any{"query": q}, Summary: clip(q, 120)}, true
	case "tool_search_call":
		return call{ID: str(p["id"]), Name: "ToolSearch", Input: map[string]any{"query": str(p["query"])}, Summary: clip(str(p["query"]), 120)}, true
	}
	return call{}, false
}

// outputOf decodes a response_item that ends a tool call.
func outputOf(l Line) (id, text string, isError bool, ok bool) {
	p := l.Payload
	switch l.itemType() {
	case "function_call_output", "custom_tool_call_output", "local_shell_call_output":
		id = str(p["call_id"])
		text = outputText(p["output"])
		lower := strings.ToLower(text)
		isError = strings.HasPrefix(text, "Script failed") || strings.Contains(lower, "exec_command failed") || strings.HasPrefix(lower, "error:")
		return id, text, isError, true
	case "tool_search_output":
		return str(p["call_id"]), outputText(p["output"]), false, true
	case "web_search_call":
		// A web search has no separate output line; its call completes on its own.
		return "", "", false, false
	}
	return "", "", false, false
}

// firstStringArg is the first (by key) short string argument of a tool call, for a one-line summary.
func firstStringArg(input map[string]any) string {
	keys := make([]string, 0, len(input))
	for k := range input {
		if s, ok := input[k].(string); ok && strings.TrimSpace(s) != "" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		return clip(input[k].(string), 120)
	}
	return ""
}

func clip(s string, n int) string {
	s = strings.TrimSpace(strings.Join(strings.Fields(s), " "))
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

var mdHeading = regexp.MustCompile(`^#+\s*`)

func firstLine(text string) string {
	for _, l := range strings.Split(strings.TrimSpace(text), "\n") {
		l = strings.TrimSpace(l)
		if l == "" {
			continue
		}
		l = mdHeading.ReplaceAllString(l, "")
		return clip(strings.ReplaceAll(l, "**", ""), 120)
	}
	return ""
}

// ---- state --------------------------------------------------------------------------------------

// Derive walks a rollout tail once and decides where the thread is. Approval prompts are not
// written to the rollout (they travel over the app-server protocol only), so an observed thread
// waiting for approval looks like one running that tool.
func Derive(lines []Line) Derived {
	d := Derived{State: model.StateUnknown, PendingTools: []model.PendingTool{}}
	var (
		pending    []call
		turnSeen   bool // a task_started or task_complete was in the tail
		turnOpen   bool
		aborted    bool
		lastKind   string // user | assistant | reasoning | call | output
		lastAgent  string
		lastPhase  string
		webSearchN int
	)
	drop := func(id string) {
		for i, c := range pending {
			if c.ID == id {
				pending = append(pending[:i], pending[i+1:]...)
				return
			}
		}
	}
	for _, l := range lines {
		if l.Time > d.LastActivityAt {
			d.LastActivityAt = l.Time
		}
		p := l.Payload
		switch l.Type {
		case "session_meta":
			if d.StartedAt == 0 {
				d.StartedAt = parseTime(str(p["timestamp"]))
			}
			if s := str(p["cwd"]); s != "" {
				d.Cwd = s
			}
			if s := str(p["originator"]); s != "" {
				d.Originator = s
			}
			if s := str(p["cli_version"]); s != "" {
				d.Version = s
			}
			if s := str(obj(p["git"])["branch"]); s != "" {
				d.GitBranch = s
			}
		case "turn_context":
			if s := str(p["cwd"]); s != "" {
				d.Cwd = s
			}
			if s := str(p["model"]); s != "" {
				d.Model = s
			}
			if s := str(p["effort"]); s != "" {
				d.Effort = s
			}
			if s := str(p["approval_policy"]); s != "" {
				d.ApprovalPolicy = s
			}
			if s := str(obj(p["sandbox_policy"])["type"]); s != "" {
				d.Sandbox = s
			}
		case "compacted":
			d.ContextTokens, d.ContextAt = 0, l.Time
		case "response_item":
			switch l.itemType() {
			case "message":
				text := messageText(p)
				switch str(p["role"]) {
				case "user":
					if text == "" || isMetaPrompt(text) {
						continue
					}
					if d.FirstPrompt == "" {
						d.FirstPrompt = clip(text, 160)
					}
					d.LastPrompt = clip(text, 160)
					lastKind = "user"
				case "assistant":
					lastKind = "assistant"
					lastAgent = text
					lastPhase = str(p["phase"])
				}
			case "reasoning":
				lastKind = "reasoning"
			case "compaction":
				d.ContextTokens, d.ContextAt = 0, l.Time
			default:
				if c, ok := callOf(l); ok {
					if c.Name == "WebSearch" {
						webSearchN++ // completes on its own; never pending
						lastKind = "output"
						continue
					}
					pending = append(pending, c)
					lastKind = "call"
				} else if id, _, _, ok := outputOf(l); ok {
					drop(id)
					lastKind = "output"
				}
			}
		case "event_msg":
			switch l.eventType() {
			case "task_started":
				turnSeen, turnOpen, aborted = true, true, false
				d.ActiveTurn = str(p["turn_id"])
				pending = pending[:0]
				if f, ok := num(p["model_context_window"]); ok && f > 0 {
					d.ContextWindow = int64(f)
				}
			case "task_complete":
				turnSeen, turnOpen = true, false
				d.ActiveTurn = ""
				pending = pending[:0]
				if s := str(p["last_agent_message"]); s != "" {
					lastAgent = s
				}
			case "turn_aborted":
				turnSeen, turnOpen, aborted = true, false, true
				d.ActiveTurn = ""
				pending = pending[:0]
			case "thread_settings_applied":
				ts := obj(p["thread_settings"])
				if s := str(ts["model"]); s != "" {
					d.Model = s
				}
				if s := str(ts["reasoning_effort"]); s != "" {
					d.Effort = s
				}
				if s := str(ts["approval_policy"]); s != "" {
					d.ApprovalPolicy = s
				}
				if s := str(ts["cwd"]); s != "" {
					d.Cwd = s
				}
			case "token_count":
				info := obj(p["info"])
				if last := obj(info["last_token_usage"]); last != nil {
					if f, ok := num(last["total_tokens"]); ok && f > 0 {
						d.ContextTokens, d.ContextAt = int64(f), l.Time
					}
				}
				if f, ok := num(info["model_context_window"]); ok && f > 0 {
					d.ContextWindow = int64(f)
				}
				if u := parseRateLimits(obj(p["rate_limits"]), l.Time); u != nil {
					d.Usage = u
				}
			}
		}
	}
	_ = webSearchN

	for _, c := range pending {
		d.PendingTools = append(d.PendingTools, model.PendingTool{ID: c.ID, Name: c.Name, Summary: c.Summary})
	}
	if len(lines) == 0 {
		return d
	}
	// Turn not in the tail at all (a long turn scrolled it out): go by the last item.
	if !turnSeen {
		turnOpen = len(pending) > 0 || (lastKind != "assistant" || lastPhase != "final_answer") && lastKind != ""
	}
	switch {
	case !turnOpen:
		d.State = model.StateIdle
		if aborted {
			d.StateDetail = "Interrupted"
		} else if s := firstLine(lastAgent); s != "" {
			d.StateDetail = s
		} else {
			d.StateDetail = "Finished turn"
		}
	case len(pending) > 0:
		c := pending[len(pending)-1]
		d.State = model.StateTool
		d.StateDetail = c.Name
		if c.Summary != "" {
			d.StateDetail += ": " + c.Summary
		}
	case lastKind == "reasoning":
		d.State, d.StateDetail = model.StateThinking, "Reasoning…"
	case lastKind == "user":
		d.State, d.StateDetail = model.StateWorking, "Responding to prompt…"
	case lastKind == "output":
		d.State, d.StateDetail = model.StateWorking, "Processing tool result…"
	default:
		d.State, d.StateDetail = model.StateWorking, "Generating…"
	}
	return d
}

// parseRateLimits turns a token_count's rate_limits block (or account/rateLimits/updated's) into a
// Usage. Codex has two windows per limit: primary (a few hours) and secondary (a week). Keys accept
// both the rollout's snake_case and the app-server's camelCase.
func parseRateLimits(rl map[string]any, now int64) *model.Usage {
	if rl == nil {
		return nil
	}
	window := func(w map[string]any) (model.UsageWindow, bool) {
		if w == nil {
			return model.UsageWindow{}, false
		}
		pct, ok := num(w["used_percent"])
		if !ok {
			pct, ok = num(w["usedPercent"])
		}
		if !ok {
			return model.UsageWindow{}, false
		}
		reset, ok2 := num(w["resets_at"])
		if !ok2 {
			reset, _ = num(w["resetsAt"])
		}
		uw := model.UsageWindow{Utilization: pct / 100}
		if reset > 0 {
			if reset < 1e11 {
				reset *= 1000
			}
			uw.ResetsAt = int64(reset)
		}
		return uw, true
	}
	u := &model.Usage{Status: "allowed", At: now, Windows: map[string]model.UsageWindow{}}
	if w, ok := window(obj(rl["primary"])); ok {
		u.Windows[UsagePrimary] = w
	}
	if w, ok := window(obj(rl["secondary"])); ok {
		u.Windows[UsageSecondary] = w
	}
	if len(u.Windows) == 0 {
		return nil
	}
	for _, w := range u.Windows {
		if w.Utilization >= 1 {
			u.Status = "rejected"
		} else if w.Utilization >= 0.8 && u.Status == "allowed" {
			u.Status = "allowed_warning"
		}
	}
	return u
}

// Usage window keys for Codex, distinct from Claude's so both accounts can share one map.
const (
	UsagePrimary   = "codex_primary"
	UsageSecondary = "codex_secondary"
)
