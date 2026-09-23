package claude

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/peter-dolkens/vineyard/daemon/internal/model"
)

// ---- tiny helpers over the untyped transcript JSON ----------------------------------------------

func str(v any) string {
	s, _ := v.(string)
	return s
}

func num(v any) (float64, bool) {
	f, ok := v.(float64)
	return f, ok
}

func msg(e map[string]any) map[string]any {
	m, _ := e["message"].(map[string]any)
	return m
}

func blocks(e map[string]any) []map[string]any {
	m := msg(e)
	if m == nil {
		return nil
	}
	arr, _ := m["content"].([]any)
	out := make([]map[string]any, 0, len(arr))
	for _, b := range arr {
		if bm, ok := b.(map[string]any); ok {
			out = append(out, bm)
		}
	}
	return out
}

func entryTime(e map[string]any) int64 {
	t := str(e["timestamp"])
	if t == "" {
		return 0
	}
	ts, err := time.Parse(time.RFC3339Nano, t)
	if err != nil {
		return 0
	}
	return ts.UnixMilli()
}

var spaces = regexp.MustCompile(`\s+`)

func clip(s string, n int) string {
	s = strings.TrimSpace(spaces.ReplaceAllString(s, " "))
	if len([]rune(s)) > n {
		r := []rune(s)
		return string(r[:n-1]) + "…"
	}
	return s
}

// SummarizeToolInput produces a one-liner for a tool call: the Bash description, the file path, etc.
func SummarizeToolInput(name string, input map[string]any) string {
	if input == nil {
		return ""
	}
	first := func(keys ...string) string {
		for _, k := range keys {
			if s := strings.TrimSpace(str(input[k])); s != "" {
				return s
			}
		}
		return ""
	}
	var s string
	switch name {
	case "Bash":
		s = first("description", "command")
	case "Read", "Write", "Edit", "NotebookEdit":
		s = first("file_path", "notebook_path")
	case "Grep", "Glob":
		s = first("pattern")
	case "Agent", "Task":
		s = first("description", "prompt")
	case "WebFetch", "WebSearch":
		s = first("url", "query")
	case "AskUserQuestion":
		if qs, ok := input["questions"].([]any); ok && len(qs) > 0 {
			if q, ok := qs[0].(map[string]any); ok {
				s = str(q["question"])
				if s == "" {
					s = str(q["header"])
				}
				if len(qs) > 1 && s != "" {
					s += " (+" + itoa(len(qs)-1) + " more)"
				}
			}
		}
	default:
		s = first("description", "command", "file_path", "path", "pattern", "query", "url", "prompt")
	}
	if s == "" {
		return ""
	}
	return clip(s, 120)
}

func itoa(i int) string { return strconv.Itoa(i) }

// Derived is everything we learn from a transcript tail.
type Derived struct {
	State          model.AgentState
	StateDetail    string
	PendingTools   []model.PendingTool
	Model          string
	Effort         string
	PermissionMode string
	GitBranch      string
	Title          string
	CustomTitle    string // set by /rename or Vineyard; wins over the AI title
	LastPrompt     string
	Version        string
	LastActivityAt int64
	ContextTokens  int64
}

// DeriveFromTranscript walks the tail once and decides where the conversation is. Only the main
// thread matters: sidechain (subagent) lines never decide the parent's state.
func DeriveFromTranscript(entries []map[string]any) Derived {
	return deriveEntries(entries, false)
}

// deriveEntries is DeriveFromTranscript with a choice about sidechain lines: a subagent's own
// transcript is nothing but sidechain lines, so for those they are the thread.
func deriveEntries(entries []map[string]any, sidechain bool) Derived {
	d := Derived{State: model.StateUnknown, PendingTools: []model.PendingTool{}}
	var last map[string]any
	skip := func(e map[string]any) bool {
		if sidechain {
			return false
		}
		side, _ := e["isSidechain"].(bool)
		return side
	}

	for _, e := range entries {
		typ := str(e["type"])
		if t := entryTime(e); t > d.LastActivityAt {
			d.LastActivityAt = t
		}
		switch typ {
		case "ai-title":
			if s := str(e["aiTitle"]); s != "" {
				d.Title = s
			}
		case "custom-title":
			if s := str(e["customTitle"]); s != "" {
				d.CustomTitle = s
			}
		case "last-prompt":
			if s := str(e["lastPrompt"]); s != "" {
				d.LastPrompt = s
			}
		case "mode":
			if s := str(e["mode"]); s != "" {
				d.PermissionMode = s
			}
		}
		if skip(e) {
			continue
		}
		if typ == "user" || typ == "assistant" {
			if s := str(e["version"]); s != "" {
				d.Version = s
			}
			if s := str(e["gitBranch"]); s != "" {
				d.GitBranch = s
			}
			if s := str(e["permissionMode"]); s != "" {
				d.PermissionMode = s
			}
			last = e
		}
		if typ == "assistant" {
			m := msg(e)
			if s := str(m["model"]); s != "" {
				d.Model = s
			}
			if s := str(e["effort"]); s != "" {
				d.Effort = s
			}
			if usage, ok := m["usage"].(map[string]any); ok {
				total := 0.0
				for _, k := range []string{"input_tokens", "cache_read_input_tokens", "cache_creation_input_tokens"} {
					if f, ok := num(usage[k]); ok {
						total += f
					}
				}
				if total > 0 {
					d.ContextTokens = int64(total)
				}
			}
		}
	}

	// Pending tool calls: tool_use blocks with no later tool_result.
	seen := map[string]bool{}
	var pending []model.PendingTool
	for i := len(entries) - 1; i >= 0; i-- {
		e := entries[i]
		if skip(e) {
			continue
		}
		switch str(e["type"]) {
		case "user":
			for _, b := range blocks(e) {
				if str(b["type"]) == "tool_result" {
					seen[str(b["tool_use_id"])] = true
				}
			}
		case "assistant":
			for _, b := range blocks(e) {
				if str(b["type"]) == "tool_use" {
					id := str(b["id"])
					if id != "" && !seen[id] {
						input, _ := b["input"].(map[string]any)
						name := str(b["name"])
						if name == "" {
							name = "tool"
						}
						pending = append([]model.PendingTool{{ID: id, Name: name, Summary: SummarizeToolInput(name, input)}}, pending...)
					}
				}
			}
		}
	}
	if pending != nil {
		d.PendingTools = pending
	}

	if last == nil {
		return d
	}

	for _, p := range pending {
		if p.Name == "AskUserQuestion" {
			d.State = model.StateQuestion
			d.StateDetail = p.Summary
			if d.StateDetail == "" {
				d.StateDetail = "Waiting for your answer"
			}
			return d
		}
	}
	for _, p := range pending {
		if p.Name == "ExitPlanMode" {
			d.State = model.StatePermission
			d.StateDetail = "Plan ready for review"
			return d
		}
	}
	if len(pending) > 0 {
		p := pending[len(pending)-1]
		d.State = model.StateTool
		if p.Summary != "" {
			d.StateDetail = p.Name + ": " + p.Summary
		} else {
			d.StateDetail = p.Name
		}
		return d
	}

	typ := str(last["type"])
	stop := str(msg(last)["stop_reason"])
	kinds := map[string]bool{}
	var firstText string
	for _, b := range blocks(last) {
		k := str(b["type"])
		kinds[k] = true
		if k == "text" && firstText == "" {
			firstText = str(b["text"])
		}
	}

	if typ == "assistant" {
		switch stop {
		case "end_turn", "stop_sequence", "max_tokens":
			d.State = model.StateIdle
			d.StateDetail = firstLine(firstText)
			if d.StateDetail == "" {
				d.StateDetail = "Finished turn"
			}
			return d
		}
		// Blocks are appended as they complete; a trailing thinking block with stop_reason still
		// tool_use means the model is between blocks – still generating.
		if kinds["thinking"] && !kinds["text"] && !kinds["tool_use"] {
			d.State = model.StateThinking
			d.StateDetail = "Reasoning…"
			return d
		}
		d.State = model.StateWorking
		d.StateDetail = "Generating…"
		return d
	}
	if typ == "user" {
		d.State = model.StateWorking
		if kinds["tool_result"] {
			d.StateDetail = "Processing tool result…"
		} else {
			d.StateDetail = "Responding to prompt…"
		}
		return d
	}
	return d
}

var mdHeading = regexp.MustCompile(`^#+\s*`)

func firstLine(text string) string {
	for _, l := range strings.Split(strings.TrimSpace(text), "\n") {
		l = strings.TrimSpace(l)
		if l == "" {
			continue
		}
		l = mdHeading.ReplaceAllString(l, "")
		l = strings.ReplaceAll(l, "**", "")
		return clip(l, 120)
	}
	return ""
}

func mapRegistryStatus(status string) (model.AgentState, string) {
	switch status {
	case "busy":
		return model.StateWorking, "Busy"
	case "shell":
		return model.StateShell, "In a shell"
	case "waiting":
		return model.StatePermission, "Waiting for input"
	case "idle":
		return model.StateIdle, "Idle"
	}
	return model.StateUnknown, ""
}

const staleRegistryMs = 6 * 60 * 60 * 1000

// BuildAgent combines a registry entry, liveness and the transcript tail into one Agent.
func BuildAgent(machineID string, s RawSession, t *RawTranscript, now int64) *model.Agent {
	r := s.Registry
	if r == nil {
		return nil
	}
	sid := str(r["sessionId"])
	cwd := str(r["cwd"])
	if sid == "" || cwd == "" {
		return nil
	}
	regStatus := str(r["status"])
	regUpdated := s.FileMtime
	if f, ok := num(r["statusUpdatedAt"]); ok {
		regUpdated = int64(f)
	} else if f, ok := num(r["updatedAt"]); ok {
		regUpdated = int64(f)
	}

	a := &model.Agent{
		ID:             machineID + "::" + sid,
		Provider:       "claude",
		MachineID:      machineID,
		WorkspacePath:  cwd,
		SessionID:      sid,
		PID:            s.PID,
		Alive:          s.Alive,
		Name:           str(r["name"]),
		Kind:           str(r["kind"]),
		Entrypoint:     str(r["entrypoint"]),
		Version:        str(r["version"]),
		State:          model.StateUnknown,
		RegistryStatus: regStatus,
		LastActivityAt: regUpdated,
		PendingTools:   []model.PendingTool{},
	}
	if f, ok := num(r["startedAt"]); ok {
		a.StartedAt = int64(f)
	}
	if t != nil {
		a.TranscriptPath = t.Path
	}

	a.Subagents = BuildSubagents(t, s.Alive, now)

	if !s.Alive {
		a.State = model.StateExited
		a.StateDetail = "Process has exited"
		return a
	}

	regState, regDetail := mapRegistryStatus(regStatus)

	if t == nil || len(t.Entries) == 0 {
		a.State, a.StateDetail = regState, regDetail
		if a.State == model.StateIdle && now-regUpdated > staleRegistryMs {
			a.StateDetail = "Idle (stale registry entry)"
		}
		return a
	}

	d := DeriveFromTranscript(t.Entries)
	a.Model = d.Model
	a.Effort = d.Effort
	a.PermissionMode = d.PermissionMode
	a.GitBranch = d.GitBranch
	a.Title = PreferredTitle(d.CustomTitle, d.Title)
	a.LastPrompt = d.LastPrompt
	if d.Version != "" {
		a.Version = d.Version
	}
	a.ContextTokens = d.ContextTokens
	a.PendingTools = d.PendingTools
	a.LastActivityAt = max64(d.LastActivityAt, t.Mtime, regUpdated)

	state, detail := d.State, d.StateDetail
	switch {
	case regStatus == "shell":
		state, detail = model.StateShell, "In a shell"
	case regStatus == "waiting":
		// A pending tool while waiting is a permission prompt for that tool.
		if state == model.StateTool {
			state = model.StatePermission
			if detail != "" {
				detail = "Permission: " + detail
			} else {
				detail = "Waiting for permission"
			}
		} else if state != model.StateQuestion {
			state = model.StatePermission
			if detail == "" {
				detail = "Waiting for input"
			}
		}
	case regStatus == "busy" && state == model.StateIdle && regUpdated > d.LastActivityAt+1500:
		// The transcript's turn ended but Claude flipped to busy afterwards: a new turn is starting.
		state, detail = model.StateWorking, "Starting next turn…"
	case regStatus == "idle" && model.IsBusy(state):
		// Transcript says mid-turn but Claude says idle: interrupted or errored out.
		if now-d.LastActivityAt > 30_000 {
			state, detail = model.StateIdle, "Interrupted"
		}
	case state == model.StateUnknown:
		state, detail = regState, regDetail
	}
	a.State, a.StateDetail = state, detail
	return a
}

func max64(vals ...int64) int64 {
	var m int64
	for _, v := range vals {
		if v > m {
			m = v
		}
	}
	return m
}

func normalisePath(p string) string {
	t := strings.TrimRight(p, `/\`)
	if t == "" {
		return p
	}
	return t
}

// Interpret turns a Report into the agents and workspaces of one machine.
func Interpret(machineID string, r *Report, now int64) ([]model.Agent, []model.Workspace) {
	byID := map[string]*model.Agent{}
	for _, s := range r.Sessions {
		var t *RawTranscript
		if s.Registry != nil {
			if sid := str(s.Registry["sessionId"]); sid != "" {
				if tt, ok := r.Transcripts[sid]; ok {
					t = &tt
				}
			}
		}
		a := BuildAgent(machineID, s, t, now)
		if a == nil {
			continue
		}
		if prev, ok := byID[a.ID]; !ok || (a.Alive && !prev.Alive) || a.LastActivityAt > prev.LastActivityAt {
			byID[a.ID] = a
		}
	}

	open := map[string]bool{}
	for _, l := range r.IDELocks {
		for _, f := range l.WorkspaceFolders {
			open[normalisePath(f)] = true
		}
	}

	// Sessions running in a scratchpad belong to the project that spawned them.
	known := map[string]string{}
	for _, p := range r.Projects {
		if p.Cwd != "" && !scratchRe.MatchString(p.Cwd) {
			known[EncodeProjectDir(p.Cwd)] = p.Cwd
		}
	}
	unscratch := func(path string) string {
		if parent, ok := ScratchpadParent(path, known); ok {
			return parent
		}
		return path
	}

	wsMap := map[string]*model.Workspace{}
	ws := func(path string) *model.Workspace {
		key := normalisePath(unscratch(path))
		if w, ok := wsMap[key]; ok {
			return w
		}
		w := &model.Workspace{ID: machineID + "::" + key, MachineID: machineID, Path: key, Agents: []model.Agent{}, OpenInIDE: open[key]}
		wsMap[key] = w
		return w
	}
	for _, p := range r.Projects {
		if p.Cwd == "" {
			continue
		}
		w := ws(p.Cwd)
		w.HistoryCount += p.Count
		if p.Mtime > w.LastActivityAt {
			w.LastActivityAt = p.Mtime
		}
	}
	agents := make([]model.Agent, 0, len(byID))
	for _, a := range byID {
		a.WorkspacePath = normalisePath(unscratch(a.WorkspacePath))
		agents = append(agents, *a)
		w := ws(a.WorkspacePath)
		w.Agents = append(w.Agents, *a)
		if a.LastActivityAt > w.LastActivityAt {
			w.LastActivityAt = a.LastActivityAt
		}
	}
	sort.Slice(agents, func(i, j int) bool { return agents[i].ID < agents[j].ID })
	workspaces := make([]model.Workspace, 0, len(wsMap))
	for _, w := range wsMap {
		sort.Slice(w.Agents, func(i, j int) bool { return w.Agents[i].ID < w.Agents[j].ID })
		workspaces = append(workspaces, *w)
	}
	sort.Slice(workspaces, func(i, j int) bool { return workspaces[i].Path < workspaces[j].Path })
	return agents, workspaces
}

// Regroup rebuilds workspace agent lists from an (annotated or extended) agent slice, keeping the
// history counts the collector found.
func Regroup(machineID string, agents []model.Agent, workspaces []model.Workspace) []model.Workspace {
	byPath := map[string]*model.Workspace{}
	for i := range workspaces {
		workspaces[i].Agents = []model.Agent{}
		byPath[workspaces[i].Path] = &workspaces[i]
	}
	var extra []model.Workspace
	for _, a := range agents {
		key := normalisePath(a.WorkspacePath)
		w, ok := byPath[key]
		if !ok {
			extra = append(extra, model.Workspace{ID: machineID + "::" + key, MachineID: machineID, Path: key, Agents: []model.Agent{}})
			w = &extra[len(extra)-1]
			byPath[key] = w
		}
		w.Agents = append(w.Agents, a)
		if a.LastActivityAt > w.LastActivityAt {
			w.LastActivityAt = a.LastActivityAt
		}
	}
	out := append(workspaces, extra...)
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// PreferredTitle picks what to show: a custom title (from /rename or Vineyard) beats the AI one, except
// for the "vineyard-<workspace>" names Vineyard gives sessions it spawns, which are only a fallback.
func PreferredTitle(custom, ai string) string {
	if custom != "" && (ai == "" || !strings.HasPrefix(custom, "vineyard-")) {
		return custom
	}
	return ai
}
