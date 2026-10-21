package agenttools

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/peter-dolkens/vineyard/daemon/internal/caps"
	"github.com/peter-dolkens/vineyard/daemon/internal/claude"
	"github.com/peter-dolkens/vineyard/daemon/internal/model"
)

type tool struct {
	name        string
	setting     string // model.AgentTool*
	title       string
	description string
	schema      map[string]any
	readOnly    bool
	run         func(s *Server, args json.RawMessage) (any, error)
}

func (t *tool) definition() map[string]any {
	return map[string]any{
		"name":        t.name,
		"title":       t.title,
		"description": t.description,
		"inputSchema": t.schema,
		"annotations": map[string]any{"title": t.title, "readOnlyHint": t.readOnly, "destructiveHint": false, "openWorldHint": !t.readOnly},
	}
}

// ReadOnlyTools are the tool names safe to pre-approve: they only read the fleet's state or wait.
func ReadOnlyTools() []string {
	var out []string
	for _, t := range allTools {
		if t.readOnly {
			out = append(out, t.name)
		}
	}
	return out
}

func obj(props map[string]any, required ...string) map[string]any {
	s := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

func str(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }

var allTools = []*tool{
	{
		name: "list_machines", setting: model.AgentToolMachines, title: "List fleet machines", readOnly: true,
		description: "Lists the machines in the Vineyard fleet: whether each is online, its OS and architecture, the common programs it has (git, gh, docker, node, go, python3, dotnet, az, kubectl, pwsh, iis, ...), its live sessions, and whether it accepts work from other agents.",
		schema:      obj(map[string]any{}),
		run:         (*Server).listMachines,
	},
	{
		name: "find_machines", setting: model.AgentToolMachines, title: "Find machines with capabilities", readOnly: true,
		description: "Finds the machines that have every one of the given capabilities. A capability is a program name looked up on the machine's PATH (e.g. \"docker\", \"gh\", \"terraform\", \"xcodebuild\"), \"iis\", or an OS: \"windows\", \"macos\", \"linux\". Online machines are asked about programs not in the fleet listing.",
		schema: obj(map[string]any{
			"capabilities":    map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "minItems": 1, "description": "Program names or OS names, all required."},
			"include_offline": map[string]any{"type": "boolean", "description": "Also match offline machines from what they last reported (default false)."},
		}, "capabilities"),
		run: (*Server).findMachines,
	},
	{
		name: "list_workspaces", setting: model.AgentToolWorkspaces, title: "List workspaces", readOnly: true,
		description: "Lists the workspaces (project folders) with Claude Code sessions on each machine, or on one machine: live session count, past session count, last activity, and whether an editor has it open.",
		schema:      obj(map[string]any{"machine": str("Machine id or name; all machines when left out.")}),
		run:         (*Server).listWorkspaces,
	},
	{
		name: "list_sessions", setting: model.AgentToolSessions, title: "List sessions", readOnly: true,
		description: "Lists Claude Code sessions in the fleet: live ones by default, with what each is doing (state, model, last prompt). With include_past, also past sessions of one machine (optionally one workspace), newest first.",
		schema: obj(map[string]any{
			"machine":      str("Machine id or name; all machines when left out (past sessions need one)."),
			"workspace":    str("Workspace path to narrow to."),
			"include_past": map[string]any{"type": "boolean", "description": "Also list past (finished) sessions; needs machine."},
			"limit":        map[string]any{"type": "integer", "minimum": 1, "maximum": 200, "description": "Most past sessions to list (default 30)."},
		}),
		run: (*Server).listSessions,
	},
	{
		name: "fetch_transcript", setting: model.AgentToolTranscripts, title: "Fetch a session transcript", readOnly: true,
		description: "Downloads a session's transcript from any machine to a local file and returns its path, without putting the transcript in this conversation. Two files: the raw .jsonl and a condensed .md (prompts, replies, one line per tool call); read the .md first, with your file tools. Fetching again only downloads what is new.",
		schema: obj(map[string]any{
			"machine":    str("Machine id or name."),
			"session_id": str("The session id (list_sessions shows them)."),
			"workspace":  str("The session's workspace path; needed only for a past session list_sessions did not show as live."),
		}, "machine", "session_id"),
		run: (*Server).fetchTranscript,
	},
	{
		name: "start_agent", setting: model.AgentToolDrive, title: "Start an agent on a machine",
		description: "Starts a new Claude Code session on a fleet machine, in a workspace there, with a first prompt. It runs as a Vineyard-managed session the user can watch and answer. Its reply (final message of the turn) arrives in this conversation as a <vineyard-agent-reply> message, or use await_agent. Permission prompts on that machine are answered by the user, not by you.",
		schema: obj(map[string]any{
			"machine":         str("Machine id or name (find_machines helps choose)."),
			"workspace":       str("Absolute path of the folder to run in, on that machine."),
			"prompt":          str("The task. Be self-contained: the agent knows nothing of this conversation."),
			"model":           str("Model alias or id (default: that machine's default)."),
			"permission_mode": map[string]any{"type": "string", "enum": []string{"default", "acceptEdits", "plan", "auto"}, "description": "Permission mode (default: that machine's default)."},
			"name":            str("Short name for the session, shown in Vineyard."),
			"notify":          map[string]any{"type": "boolean", "description": "Deliver the reply into this conversation when the turn ends (default true)."},
		}, "machine", "workspace", "prompt"),
		run: (*Server).startAgent,
	},
	{
		name: "message_agent", setting: model.AgentToolDrive, title: "Message an agent",
		description: "Sends a message to a running session on any fleet machine, marked as coming from you (an agent), not the user. Returns once delivered; the reply (its final message when the turn ends) arrives in this conversation as a <vineyard-agent-reply> message, or use await_agent.",
		schema: obj(map[string]any{
			"machine":    str("Machine id or name."),
			"session_id": str("The session to message (list_sessions shows them)."),
			"message":    str("What to say."),
			"notify":     map[string]any{"type": "boolean", "description": "Deliver the reply into this conversation when the turn ends (default true)."},
		}, "machine", "session_id", "message"),
		run: (*Server).messageAgent,
	},
	{
		name: "send_file", setting: model.AgentToolFiles, title: "Send a file to a session",
		description: "Copies one file from this machine to a live session on any fleet machine (up to 256 MB, checksum verified). It lands in ~/.vineyard/received/<session>/ on that machine, never inside its workspace, and the session is told the path, with your message if you give one, marked as coming from you (an agent). Its reply arrives like message_agent's. To send a folder, archive it first.",
		schema: obj(map[string]any{
			"machine":    str("Machine id or name."),
			"session_id": str("The session to send it to (list_sessions shows them)."),
			"path":       str("Absolute path of the file on this machine."),
			"message":    str("What the session should do with it (optional)."),
			"notify":     map[string]any{"type": "boolean", "description": "Deliver its reply into this conversation when the turn ends (default true)."},
		}, "machine", "session_id", "path"),
		run: (*Server).sendFile,
	},
	{
		name: "create_workspace", setting: model.AgentToolNewWorkspaces, title: "Create a workspace on a machine",
		description: "Creates a folder for a new workspace on a fleet machine, making parent folders as needed, so an agent can be started there (start_agent). Each machine only allows this inside one folder, by default its user's home folder; a leading ~ in the path means that home. An existing folder is left as it is. It creates nothing but the folder: clone or set up the project with an agent started there.",
		schema: obj(map[string]any{
			"machine": str("Machine id or name."),
			"path":    str("Absolute path of the new folder on that machine, or one starting with ~ for its home folder (e.g. ~/Projects/new-thing)."),
		}, "machine", "path"),
		run: (*Server).createWorkspace,
	},
	{
		name: "await_agent", setting: model.AgentToolDrive, title: "Wait for an agent's reply", readOnly: true,
		description: "Waits until a session you started or messaged finishes its turn, and returns its reply and status: done, question (it asked something), permission (blocked on a permission prompt the user must answer in Vineyard), exited or error. For a session you have not messaged, returns its latest reply once it is not busy. A reply returned here is not delivered as a message too.",
		schema: obj(map[string]any{
			"machine":         str("Machine id or name."),
			"session_id":      str("The session."),
			"timeout_seconds": map[string]any{"type": "integer", "minimum": 1, "maximum": 1800, "description": "How long to wait (default 300)."},
		}, "machine", "session_id"),
		run: (*Server).awaitAgent,
	},
}

func decode(args json.RawMessage, v any) error {
	if err := json.Unmarshal(args, v); err != nil {
		return fmt.Errorf("bad arguments: %v", err)
	}
	return nil
}

func when(ms int64) string {
	if ms <= 0 {
		return ""
	}
	return time.UnixMilli(ms).UTC().Format(time.RFC3339)
}

func matchMachine(all []model.FleetEntry, machine string) (model.FleetEntry, bool) {
	for _, e := range all {
		if e.Snapshot.MachineID == machine {
			return e, true
		}
	}
	for _, e := range all {
		if strings.EqualFold(e.Snapshot.Name, machine) || strings.EqualFold(e.Snapshot.Host.Hostname, machine) {
			return e, true
		}
	}
	return model.FleetEntry{}, false
}

func liveAgents(e model.FleetEntry) []model.Agent {
	var out []model.Agent
	for _, a := range e.Snapshot.Agents {
		if a.Alive {
			out = append(out, a)
		}
	}
	return out
}

func osName(goos string) string {
	if goos == "darwin" {
		return "macos"
	}
	return goos
}

func (s *Server) listMachines(json.RawMessage) (any, error) {
	self, all, err := s.client.Fleet()
	if err != nil {
		return nil, err
	}
	type machine struct {
		ID             string   `json:"id"`
		Name           string   `json:"name"`
		ThisMachine    bool     `json:"thisMachine,omitempty"`
		Online         bool     `json:"online"`
		LastSeen       string   `json:"lastSeen,omitempty"`
		OS             string   `json:"os,omitempty"`
		Arch           string   `json:"arch,omitempty"`
		Hostname       string   `json:"hostname,omitempty"`
		Tools          []string `json:"tools,omitempty"`
		HasClaude      bool     `json:"hasClaude"`
		LiveSessions   int      `json:"liveSessions"`
		AcceptsAgents  bool     `json:"acceptsAgents"`
		DaemonVersion  string   `json:"daemonVersion,omitempty"`
		ToolsNotListed bool     `json:"toolsNotListed,omitempty"`
	}
	out := []machine{}
	for _, e := range all {
		sn := e.Snapshot
		m := machine{
			ID: sn.MachineID, Name: sn.Name, ThisMachine: sn.MachineID == self, Online: e.Online, OS: osName(sn.Host.OS), Arch: sn.Host.Arch,
			Hostname: sn.Host.Hostname, Tools: sn.Host.Tools, HasClaude: sn.HasClaude, LiveSessions: len(liveAgents(e)),
			AcceptsAgents: sn.AgentTools != nil && slices.Contains(sn.AgentTools.Tools, model.AgentToolDrive), DaemonVersion: sn.DaemonVersion,
			ToolsNotListed: sn.Host.Tools == nil,
		}
		if !e.Online {
			m.LastSeen = when(e.LastSeen)
		}
		out = append(out, m)
	}
	return map[string]any{"machines": out}, nil
}

var osCaps = map[string]string{"windows": "windows", "macos": "darwin", "darwin": "darwin", "mac": "darwin", "linux": "linux"}

func (s *Server) findMachines(args json.RawMessage) (any, error) {
	var a struct {
		Capabilities   []string `json:"capabilities"`
		IncludeOffline bool     `json:"include_offline"`
	}
	if err := decode(args, &a); err != nil {
		return nil, err
	}
	if len(a.Capabilities) == 0 {
		return nil, errors.New("name at least one capability")
	}
	for _, c := range a.Capabilities {
		if _, isOS := osCaps[strings.ToLower(c)]; !isOS && !caps.Valid(c) {
			return nil, fmt.Errorf("%q is not a program name", c)
		}
	}
	_, all, err := s.client.Fleet()
	if err != nil {
		return nil, err
	}
	type result struct {
		ID      string   `json:"id"`
		Name    string   `json:"name"`
		Online  bool     `json:"online"`
		OS      string   `json:"os,omitempty"`
		Has     []string `json:"has"`
		Missing []string `json:"missing,omitempty"`
		Unknown []string `json:"unknown,omitempty"`
	}
	results := make([]result, len(all))
	var wg sync.WaitGroup
	for i, e := range all {
		sn := e.Snapshot
		r := result{ID: sn.MachineID, Name: sn.Name, Online: e.Online, OS: osName(sn.Host.OS), Has: []string{}}
		var ask []string
		for _, c := range a.Capabilities {
			if goos, isOS := osCaps[strings.ToLower(c)]; isOS {
				if sn.Host.OS == goos {
					r.Has = append(r.Has, c)
				} else {
					r.Missing = append(r.Missing, c)
				}
				continue
			}
			switch {
			case slices.Contains(sn.Host.Tools, c):
				r.Has = append(r.Has, c)
			case slices.Contains(caps.Known, c) && sn.Host.Tools != nil:
				r.Missing = append(r.Missing, c) // a known program the machine looked for and lacks
			default:
				ask = append(ask, c)
			}
		}
		results[i] = r
		if len(ask) == 0 {
			continue
		}
		if !e.Online || len(r.Missing) > 0 {
			results[i].Unknown = ask
			continue
		}
		wg.Add(1)
		go func(i int, id string, ask []string) {
			defer wg.Done()
			raw, err := s.client.Request(id, "hascap", map[string]any{"names": ask}, 15*time.Second)
			var d struct {
				Found map[string]string `json:"found"`
			}
			if err != nil || json.Unmarshal(raw, &d) != nil {
				results[i].Unknown = ask // an older daemon, or unreachable
				return
			}
			for _, c := range ask {
				if _, ok := d.Found[c]; ok {
					results[i].Has = append(results[i].Has, c)
				} else {
					results[i].Missing = append(results[i].Missing, c)
				}
			}
		}(i, sn.MachineID, ask)
	}
	wg.Wait()
	var matching []string
	var out []result
	for _, r := range results {
		if !r.Online && !a.IncludeOffline {
			continue
		}
		if len(r.Missing) == 0 && len(r.Unknown) == 0 {
			matching = append(matching, r.Name)
		}
		out = append(out, r)
	}
	sort.SliceStable(out, func(i, j int) bool {
		return len(out[i].Missing)+len(out[i].Unknown) < len(out[j].Missing)+len(out[j].Unknown)
	})
	return map[string]any{"matching": nonNilStrings(matching), "machines": out}, nil
}

func nonNilStrings(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func (s *Server) listWorkspaces(args json.RawMessage) (any, error) {
	var a struct {
		Machine string `json:"machine"`
	}
	if err := decode(args, &a); err != nil {
		return nil, err
	}
	_, all, err := s.client.Fleet()
	if err != nil {
		return nil, err
	}
	if a.Machine != "" {
		e, ok := matchMachine(all, a.Machine)
		if !ok {
			return nil, fmt.Errorf("no machine called %s in the fleet", a.Machine)
		}
		all = []model.FleetEntry{e}
	}
	type ws struct {
		Machine      string `json:"machine"`
		Path         string `json:"path"`
		LiveSessions int    `json:"liveSessions"`
		PastSessions int    `json:"pastSessions"`
		LastActivity string `json:"lastActivity,omitempty"`
		OpenInEditor bool   `json:"openInEditor,omitempty"`
	}
	out := []ws{}
	for _, e := range all {
		for _, w := range e.Snapshot.Workspaces {
			live := 0
			for _, ag := range w.Agents {
				if ag.Alive {
					live++
				}
			}
			out = append(out, ws{Machine: e.Snapshot.Name, Path: w.Path, LiveSessions: live, PastSessions: w.HistoryCount, LastActivity: when(w.LastActivityAt), OpenInEditor: w.OpenInIDE})
		}
	}
	return map[string]any{"workspaces": out}, nil
}

func clip(s string, n int) string {
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

func (s *Server) listSessions(args json.RawMessage) (any, error) {
	var a struct {
		Machine     string `json:"machine"`
		Workspace   string `json:"workspace"`
		IncludePast bool   `json:"include_past"`
		Limit       int    `json:"limit"`
	}
	if err := decode(args, &a); err != nil {
		return nil, err
	}
	if a.IncludePast && a.Machine == "" {
		return nil, errors.New("include_past needs a machine")
	}
	_, all, err := s.client.Fleet()
	if err != nil {
		return nil, err
	}
	if a.Machine != "" {
		e, ok := matchMachine(all, a.Machine)
		if !ok {
			return nil, fmt.Errorf("no machine called %s in the fleet", a.Machine)
		}
		all = []model.FleetEntry{e}
	}
	type live struct {
		Machine      string `json:"machine"`
		SessionID    string `json:"sessionId"`
		ThisSession  bool   `json:"thisSession,omitempty"`
		Workspace    string `json:"workspace"`
		Title        string `json:"title,omitempty"`
		State        string `json:"state"`
		Detail       string `json:"detail,omitempty"`
		Model        string `json:"model,omitempty"`
		Managed      bool   `json:"managed"`
		Subagents    int    `json:"subagents,omitempty"`
		LastPrompt   string `json:"lastPrompt,omitempty"`
		LastActivity string `json:"lastActivity,omitempty"`
	}
	lives := []live{}
	for _, e := range all {
		for _, ag := range liveAgents(e) {
			if a.Workspace != "" && ag.WorkspacePath != a.Workspace {
				continue
			}
			title := ag.Title
			if title == "" {
				title = ag.Name
			}
			lives = append(lives, live{
				Machine: e.Snapshot.Name, SessionID: ag.SessionID, ThisSession: ag.SessionID == s.o.Session && e.Snapshot.MachineID == s.o.Machine,
				Workspace: ag.WorkspacePath, Title: title, State: string(ag.State), Detail: ag.StateDetail, Model: ag.Model, Managed: ag.Managed != nil,
				Subagents: len(ag.Subagents), LastPrompt: clip(ag.LastPrompt, 200), LastActivity: when(ag.LastActivityAt),
			})
		}
	}
	out := map[string]any{"live": lives}
	if a.IncludePast {
		limit := a.Limit
		if limit <= 0 {
			limit = 30
		}
		raw, err := s.client.Request(all[0].Snapshot.MachineID, "sessions", map[string]any{"cwd": a.Workspace, "limit": limit}, 30*time.Second)
		if err != nil {
			return nil, fmt.Errorf("past sessions on %s: %w", all[0].Snapshot.Name, err)
		}
		var d struct {
			Sessions []claude.SessionSummary `json:"sessions"`
		}
		if err := json.Unmarshal(raw, &d); err != nil {
			return nil, err
		}
		type past struct {
			SessionID   string `json:"sessionId"`
			Workspace   string `json:"workspace"`
			Title       string `json:"title,omitempty"`
			FirstPrompt string `json:"firstPrompt,omitempty"`
			LastPrompt  string `json:"lastPrompt,omitempty"`
			Model       string `json:"model,omitempty"`
			Modified    string `json:"modified"`
			Bytes       int64  `json:"bytes"`
		}
		pasts := []past{}
		for _, p := range d.Sessions {
			pasts = append(pasts, past{SessionID: p.SessionID, Workspace: p.Cwd, Title: p.Title, FirstPrompt: clip(p.FirstPrompt, 200), LastPrompt: clip(p.LastPrompt, 200), Model: p.Model, Modified: when(p.Mtime), Bytes: p.Size})
		}
		out["past"] = pasts
	}
	return out, nil
}
