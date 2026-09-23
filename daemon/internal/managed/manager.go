// Package managed runs Claude Code sessions as children of the daemon over the stream-json control
// protocol, giving Vineyard full duplex control: prompts in, events out, and permission prompts /
// AskUserQuestion answered through control_request / control_response frames.
//
// Managed sessions still write the normal registry and transcript, so the collector sees them like
// any other; this package only adds the control state (pending requests, exit status).
package managed

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/peter-dolkens/vineyard/daemon/internal/model"
)

type SpawnOptions struct {
	Cwd            string   `json:"cwd"`
	Prompt         string   `json:"prompt,omitempty"`
	Model          string   `json:"model,omitempty"`
	Effort         string   `json:"effort,omitempty"`
	PermissionMode string   `json:"permissionMode,omitempty"`
	Resume         string   `json:"resume,omitempty"` // existing session id to continue
	Name           string   `json:"name,omitempty"`
	AllowedTools   []string `json:"allowedTools,omitempty"`
}

type proc struct {
	info   model.ManagedInfo
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	wmu    sync.Mutex
	stderr []string
	done   chan struct{}
	// ctl holds the reply channel for each control_request we sent and are waiting on.
	ctl map[string]chan ctlReply
	// ctxBusy: a get_context_usage request is in flight; ctxUnsupported: this Claude Code refused one.
	ctxBusy        bool
	ctxUnsupported bool
}

// ctlReply is Claude Code's answer to one of our control_requests: the response body on success,
// or why it was refused.
type ctlReply struct {
	body json.RawMessage
	err  error
}

// controlTimeout bounds how long a live setting change waits for Claude Code to acknowledge it.
const controlTimeout = 20 * time.Second

type Manager struct {
	mu       sync.Mutex
	procs    map[string]*proc
	log      *log.Logger
	onChange func()
	// ClaudeBin overrides binary discovery.
	ClaudeBin string
}

func New(logger *log.Logger, onChange func()) *Manager {
	if logger == nil {
		logger = log.Default()
	}
	return &Manager{procs: map[string]*proc{}, log: logger, onChange: onChange}
}

func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b[:])
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

// FindClaude locates the claude binary: PATH, then the usual install locations, then the newest
// VS Code extension bundle.
func FindClaude(override string) (string, error) {
	if override != "" {
		return override, nil
	}
	name := "claude"
	if runtime.GOOS == "windows" {
		name = "claude.exe"
	}
	if p, err := exec.LookPath(name); err == nil {
		return p, nil
	}
	home, _ := os.UserHomeDir()
	candidates := []string{
		filepath.Join(home, ".local", "bin", name),
		"/opt/homebrew/bin/claude", "/usr/local/bin/claude",
		filepath.Join(home, ".claude", "local", "claude"),
		filepath.Join(home, "AppData", "Local", "Programs", "claude", "claude.exe"),
	}
	for _, c := range candidates {
		if st, err := os.Stat(c); err == nil && !st.IsDir() {
			return c, nil
		}
	}
	// VS Code extension bundles a native binary; pick the newest.
	for _, root := range []string{filepath.Join(home, ".vscode", "extensions"), filepath.Join(home, ".vscode-insiders", "extensions"), filepath.Join(home, ".vscode-server", "extensions")} {
		matches, _ := filepath.Glob(filepath.Join(root, "anthropic.claude-code-*", "resources", "native-binary", name))
		if len(matches) > 0 {
			sort.Strings(matches)
			return matches[len(matches)-1], nil
		}
	}
	return "", errors.New("claude binary not found; install Claude Code or set claudeBin in ~/.vineyard/config.json")
}

func loginShell() string {
	if sh := os.Getenv("SHELL"); sh != "" {
		return sh
	}
	if runtime.GOOS == "darwin" {
		return "/bin/zsh"
	}
	return "/bin/bash"
}

// Spawn starts a managed session and returns its session id immediately.
func (m *Manager) Spawn(o SpawnOptions) (string, error) {
	if o.Cwd == "" {
		return "", errors.New("cwd is required")
	}
	if st, err := os.Stat(o.Cwd); err != nil || !st.IsDir() {
		return "", fmt.Errorf("workspace %s is not a directory", o.Cwd)
	}
	bin, err := FindClaude(m.ClaudeBin)
	if err != nil {
		return "", err
	}
	sid := o.Resume
	args := []string{"--print", "--output-format", "stream-json", "--input-format", "stream-json", "--verbose", "--permission-prompt-tool", "stdio"}
	if sid == "" {
		sid = newUUID()
		args = append(args, "--session-id", sid)
	} else {
		args = append(args, "--resume", sid)
	}
	if o.Model != "" {
		args = append(args, "--model", o.Model)
	}
	if o.Effort != "" {
		args = append(args, "--effort", o.Effort)
	}
	if o.PermissionMode != "" {
		args = append(args, "--permission-mode", o.PermissionMode)
	}
	if o.Name != "" {
		args = append(args, "--name", o.Name)
	}
	for _, t := range o.AllowedTools {
		args = append(args, "--allowedTools", t)
	}

	m.mu.Lock()
	if p, ok := m.procs[sid]; ok && !p.info.Exited {
		m.mu.Unlock()
		return "", fmt.Errorf("session %s is already managed by this daemon", sid)
	}
	m.mu.Unlock()

	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.Command(bin, args...)
	} else {
		// Run through the user's login shell so PATH etc. match an interactive terminal (launchd and
		// systemd give services a minimal environment). "$0" "$@" forwards the binary and args verbatim.
		cmd = exec.Command(loginShell(), append([]string{"-lc", `exec "$0" "$@"`, bin}, args...)...)
	}
	cmd.Dir = o.Cwd
	cmd.Env = append(os.Environ(), "CLAUDE_CODE_ENTRYPOINT=vineyard")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return "", err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return "", err
	}
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("start claude: %w", err)
	}
	p := &proc{
		cmd: cmd, stdin: stdin, done: make(chan struct{}), ctl: map[string]chan ctlReply{},
		info: model.ManagedInfo{SessionID: sid, PID: cmd.Process.Pid, Cwd: o.Cwd, StartedAt: time.Now().UnixMilli(), Model: o.Model, Effort: o.Effort, PermissionMode: o.PermissionMode, Name: o.Name, Resumed: o.Resume != ""},
	}
	m.mu.Lock()
	m.procs[sid] = p
	m.mu.Unlock()
	m.log.Printf("managed: spawned %s (pid %d) in %s", sid, cmd.Process.Pid, o.Cwd)

	go m.readStdout(p, stdout)
	go m.readStderr(p, stderr)
	go m.wait(p)
	go m.initialize(p)

	if strings.TrimSpace(o.Prompt) != "" {
		if err := m.Send(sid, o.Prompt); err != nil {
			return sid, err
		}
	}
	m.changed()
	return sid, nil
}

func (m *Manager) changed() {
	if m.onChange != nil {
		m.onChange()
	}
}

func (m *Manager) write(p *proc, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	p.wmu.Lock()
	defer p.wmu.Unlock()
	_, err = p.stdin.Write(append(b, '\n'))
	return err
}

func (m *Manager) get(sid string) (*proc, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.procs[sid]
	if !ok {
		return nil, fmt.Errorf("session %s is not managed by this daemon", sid)
	}
	if p.info.Exited {
		return nil, fmt.Errorf("managed session %s has exited", sid)
	}
	return p, nil
}

func (m *Manager) Has(sid string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.procs[sid]
	return ok && !p.info.Exited
}

// Send delivers a user prompt; Claude reads it between tool calls or starts a new turn when idle.
func (m *Manager) Send(sid, text string) error {
	p, err := m.get(sid)
	if err != nil {
		return err
	}
	return m.write(p, map[string]any{
		"type":    "user",
		"message": map[string]any{"role": "user", "content": []map[string]any{{"type": "text", "text": text}}},
	})
}

// Respond answers a pending control request. response is the raw permission result, e.g.
// {"behavior":"allow","updatedInput":{...}} or {"behavior":"deny","message":"..."}. For AskUserQuestion,
// updatedInput carries the original input plus an "answers" map.
func (m *Manager) Respond(sid, requestID string, response json.RawMessage) error {
	p, err := m.get(sid)
	if err != nil {
		return err
	}
	m.mu.Lock()
	if p.info.Pending == nil || p.info.Pending.RequestID != requestID {
		m.mu.Unlock()
		return fmt.Errorf("request %s is not pending", requestID)
	}
	p.info.Pending = nil
	m.mu.Unlock()
	err = m.write(p, map[string]any{
		"type":     "control_response",
		"response": map[string]any{"subtype": "success", "request_id": requestID, "response": response},
	})
	m.changed()
	return err
}

// Interrupt asks Claude to stop the current turn (like pressing Escape).
func (m *Manager) Interrupt(sid string) error {
	p, err := m.get(sid)
	if err != nil {
		return err
	}
	return m.write(p, map[string]any{"type": "control_request", "request_id": newUUID(), "request": map[string]any{"subtype": "interrupt"}})
}

// StopTask stops one running background task of the session: a backgrounded shell command (taskID is
// the id Claude Code handed back in the Bash tool_result) or a subagent (taskID is its agent id, the
// <id> of subagents/agent-<id>.jsonl; Claude Code registers Agent-tool tasks under that id). It is
// Claude Code's stop_task control request and, unlike Interrupt, waits for the answer: a wrong id or
// a task that already finished is refused ("StopTask: Task x is not running …") and that text is
// returned so the viewer can show it.
func (m *Manager) StopTask(sid, taskID string) error {
	p, err := m.get(sid)
	if err != nil {
		return err
	}
	if strings.TrimSpace(taskID) == "" {
		return errors.New("task id is empty")
	}
	if _, err := m.control(p, map[string]any{"subtype": "stop_task", "task_id": taskID}); err != nil {
		return err
	}
	return nil
}

// control sends a control_request and waits for Claude Code's control_response, so callers learn
// whether a live change was accepted (e.g. bypassPermissions can be refused). The response body is
// returned for requests that answer with data (initialize).
func (m *Manager) control(p *proc, req map[string]any) (json.RawMessage, error) {
	rid := newUUID()
	ch := make(chan ctlReply, 1)
	m.mu.Lock()
	p.ctl[rid] = ch
	m.mu.Unlock()
	forget := func() {
		m.mu.Lock()
		delete(p.ctl, rid)
		m.mu.Unlock()
	}
	if err := m.write(p, map[string]any{"type": "control_request", "request_id": rid, "request": req}); err != nil {
		forget()
		return nil, err
	}
	select {
	case r := <-ch:
		return r.body, r.err
	case <-p.done:
		forget()
		return nil, errors.New("the session exited before it answered")
	case <-time.After(controlTimeout):
		forget()
		return nil, errors.New("the session did not acknowledge the change in time")
	}
}

// initialize asks the freshly spawned session what it offers, the same handshake the Agent SDK
// performs, and records the model picker so viewers list exactly the models this harness and
// account can use rather than a list baked into the extension.
func (m *Manager) initialize(p *proc) {
	body, err := m.control(p, map[string]any{"subtype": "initialize"})
	if err != nil {
		m.log.Printf("managed: %s initialize: %v", p.info.SessionID, err)
		return
	}
	models, commands, account := parseModels(body), parseCommands(body), parseAccountInfo(body)
	if len(models) == 0 && len(commands) == 0 && account.Email == "" {
		return
	}
	m.mu.Lock()
	p.info.Models = models
	p.info.Commands = commands
	p.info.Account, p.info.AccountOrg, p.info.AccountPlan = account.Email, account.Organization, account.Plan
	m.mu.Unlock()
	m.changed()
	// The window's real size (and the baseline: system prompt, tools, memory) comes from the harness.
	m.refreshContext(p)
}

// parseModels extracts the picker rows from an initialize response. Rows Claude Code marks disabled
// (not usable on this account) are dropped; order is Claude Code's own.
func parseModels(body json.RawMessage) []model.ModelInfo {
	var v struct {
		Models []struct {
			model.ModelInfo
			Disabled bool `json:"disabled"`
		} `json:"models"`
	}
	if json.Unmarshal(body, &v) != nil {
		return nil
	}
	out := make([]model.ModelInfo, 0, len(v.Models))
	for _, r := range v.Models {
		if r.Disabled || r.Value == "" {
			continue
		}
		out = append(out, r.ModelInfo)
	}
	return out
}

// SetModel switches the model for the rest of the session; "" returns to Claude Code's default.
func (m *Manager) SetModel(sid, modelID string) error {
	p, err := m.get(sid)
	if err != nil {
		return err
	}
	req := map[string]any{"subtype": "set_model"}
	if modelID != "" {
		req["model"] = modelID
	}
	if _, err := m.control(p, req); err != nil {
		return fmt.Errorf("set model: %w", err)
	}
	m.mu.Lock()
	p.info.Model = modelID
	m.mu.Unlock()
	m.changed()
	go m.refreshContext(p) // the window may differ ("[1m]" models)
	return nil
}

// SetEffort changes the reasoning effort level (low | medium | high | xhigh | max; "" = default).
func (m *Manager) SetEffort(sid, effort string) error {
	p, err := m.get(sid)
	if err != nil {
		return err
	}
	var level any
	if effort != "" {
		level = effort
	}
	if _, err := m.control(p, map[string]any{"subtype": "apply_flag_settings", "settings": map[string]any{"effortLevel": level}}); err != nil {
		return fmt.Errorf("set effort: %w", err)
	}
	m.mu.Lock()
	p.info.Effort = effort
	m.mu.Unlock()
	m.changed()
	return nil
}

// Rename gives the session a custom title; Claude Code records it in the transcript itself.
func (m *Manager) Rename(sid, title string) error {
	p, err := m.get(sid)
	if err != nil {
		return err
	}
	if _, err := m.control(p, map[string]any{"subtype": "rename_session", "title": title}); err != nil {
		return fmt.Errorf("rename: %w", err)
	}
	m.changed()
	return nil
}

// SetPermissionMode switches how the session asks before acting (default | acceptEdits | plan | auto | bypassPermissions).
func (m *Manager) SetPermissionMode(sid, mode string) error {
	p, err := m.get(sid)
	if err != nil {
		return err
	}
	if mode == "" {
		mode = "default"
	}
	if _, err := m.control(p, map[string]any{"subtype": "set_permission_mode", "mode": mode}); err != nil {
		return fmt.Errorf("set permission mode: %w", err)
	}
	m.mu.Lock()
	p.info.PermissionMode = mode
	m.mu.Unlock()
	m.changed()
	return nil
}

// Stop ends the session: close stdin (Claude exits cleanly) and kill after a grace period.
func (m *Manager) Stop(sid string) error {
	p, err := m.get(sid)
	if err != nil {
		return err
	}
	p.wmu.Lock()
	_ = p.stdin.Close()
	p.wmu.Unlock()
	go func() {
		select {
		case <-p.done:
		case <-time.After(10 * time.Second):
			_ = p.cmd.Process.Kill()
		}
	}()
	return nil
}

func (m *Manager) readStdout(p *proc, r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for sc.Scan() {
		line := sc.Bytes()
		var env struct {
			Type      string `json:"type"`
			Subtype   string `json:"subtype"`
			RequestID string `json:"request_id"`
			Request   struct {
				Subtype                 string          `json:"subtype"`
				ToolName                string          `json:"tool_name"`
				DisplayName             string          `json:"display_name"`
				Input                   json.RawMessage `json:"input"`
				ToolUseID               string          `json:"tool_use_id"`
				RequiresUserInteraction bool            `json:"requires_user_interaction"`
				PermissionSuggestions   json.RawMessage `json:"permission_suggestions"`
				Description             string          `json:"description"`
			} `json:"request"`
			Response struct {
				Subtype   string          `json:"subtype"`
				RequestID string          `json:"request_id"`
				Error     string          `json:"error"`
				Response  json.RawMessage `json:"response"`
			} `json:"response"`
			RateLimitInfo json.RawMessage `json:"rate_limit_info"`
			// Status is "compacting", "requesting" or null on system/status lines.
			Status          *string         `json:"status"`
			CompactMetadata json.RawMessage `json:"compact_metadata"`
			SessionID       string          `json:"session_id"`
			Model           string          `json:"model"`
			PermissionMode  string          `json:"permissionMode"`
			TotalCostUSD    float64         `json:"total_cost_usd"`
			IsError         bool            `json:"is_error"`
			Result          string          `json:"result"`
		}
		if json.Unmarshal(line, &env) != nil {
			continue
		}
		switch env.Type {
		case "control_response":
			m.mu.Lock()
			ch, ok := p.ctl[env.Response.RequestID]
			delete(p.ctl, env.Response.RequestID)
			m.mu.Unlock()
			if ok {
				if env.Response.Subtype == "error" {
					msg := env.Response.Error
					if msg == "" {
						msg = "rejected by the session"
					}
					ch <- ctlReply{err: errors.New(msg)}
				} else {
					ch <- ctlReply{body: env.Response.Response}
				}
			}
		case "control_request":
			if env.Request.Subtype == "can_use_tool" {
				m.mu.Lock()
				p.info.Pending = &model.PendingRequest{
					RequestID: env.RequestID, ToolName: env.Request.ToolName, DisplayName: env.Request.DisplayName,
					Input: env.Request.Input, ToolUseID: env.Request.ToolUseID, RequiresUserInteraction: env.Request.RequiresUserInteraction,
					Suggestions: env.Request.PermissionSuggestions, Description: env.Request.Description, At: time.Now().UnixMilli(),
				}
				m.mu.Unlock()
				m.changed()
			} else {
				// Anything else (hook callbacks, mcp messages) we acknowledge so the session never hangs.
				_ = m.write(p, map[string]any{"type": "control_response", "response": map[string]any{"subtype": "success", "request_id": env.RequestID, "response": map[string]any{}}})
			}
		case "system":
			switch env.Subtype {
			case "init":
				m.mu.Lock()
				if env.Model != "" {
					p.info.Model = env.Model
				}
				if env.PermissionMode != "" {
					p.info.PermissionMode = env.PermissionMode
				}
				p.info.Ready = true
				m.mu.Unlock()
				m.changed()
			case "status":
				// Emitted when the session's mode changes (from us or from inside the session), when a
				// compaction starts (status "compacting", repeated every 30 s while it runs) and when the
				// next request starts ("requesting") or the status clears (null).
				changed := false
				m.mu.Lock()
				if env.PermissionMode != "" {
					p.info.PermissionMode = env.PermissionMode
					changed = true
				}
				compacting := env.Status != nil && *env.Status == "compacting"
				if p.setCompacting(compacting, time.Now().UnixMilli()) {
					changed = true
				}
				m.mu.Unlock()
				if changed {
					m.changed()
				}
			case "compact_boundary":
				// The summary is in place; the transcript shows the new size only at the next API call,
				// so this is one of the few moments worth asking the harness.
				m.mu.Lock()
				p.setCompacting(false, 0)
				m.mu.Unlock()
				m.changed()
				go m.refreshContext(p)
			}
		case "rate_limit_event":
			// The account's limit picture changed (read from the API's rate-limit headers).
			if u := parseRateLimit(env.RateLimitInfo, time.Now().UnixMilli()); u != nil {
				m.mu.Lock()
				p.info.Usage = u
				m.mu.Unlock()
				m.changed()
			}
		case "result":
			m.mu.Lock()
			p.info.Turns++
			p.info.CostUSD = env.TotalCostUSD
			if env.IsError {
				p.info.LastError = env.Result
			}
			m.mu.Unlock()
			m.changed()
		}
	}
}

func (m *Manager) readStderr(p *proc, r io.Reader) {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		m.mu.Lock()
		p.stderr = append(p.stderr, sc.Text())
		if len(p.stderr) > 50 {
			p.stderr = p.stderr[len(p.stderr)-50:]
		}
		m.mu.Unlock()
	}
}

func (m *Manager) wait(p *proc) {
	err := p.cmd.Wait()
	m.mu.Lock()
	p.info.Exited = true
	p.info.ExitedAt = time.Now().UnixMilli()
	if err != nil {
		p.info.LastError = err.Error()
		if len(p.stderr) > 0 {
			p.info.LastError += ": " + strings.Join(p.stderr[max(0, len(p.stderr)-5):], " | ")
		}
	}
	p.info.Pending = nil
	m.mu.Unlock()
	close(p.done)
	m.log.Printf("managed: %s exited (%v)", p.info.SessionID, err)
	m.changed()
	// Keep the record briefly so the UI can show why it ended, then forget it.
	time.AfterFunc(10*time.Minute, func() {
		m.mu.Lock()
		if cur, ok := m.procs[p.info.SessionID]; ok && cur == p {
			delete(m.procs, p.info.SessionID)
		}
		m.mu.Unlock()
	})
}

// All returns a copy of every managed session's info.
func (m *Manager) All() []model.ManagedInfo {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]model.ManagedInfo, 0, len(m.procs))
	for _, p := range m.procs {
		out = append(out, p.info)
	}
	return out
}

// Merge annotates collector-derived agents with managed state and synthesises entries for managed
// sessions the registry has not caught up with yet (the first second or two after spawn).
func (m *Manager) Merge(machineID string, agents []model.Agent) []model.Agent {
	infos := m.All()
	if len(infos) == 0 {
		return agents
	}
	byID := map[string]*model.Agent{}
	for i := range agents {
		byID[agents[i].SessionID] = &agents[i]
	}
	for _, info := range infos {
		a, ok := byID[info.SessionID]
		if !ok {
			if info.Exited {
				continue
			}
			agents = append(agents, model.Agent{
				ID: machineID + "::" + info.SessionID, Provider: "claude", MachineID: machineID, WorkspacePath: info.Cwd,
				SessionID: info.SessionID, PID: info.PID, Alive: true, Name: info.Name, Kind: "managed", Entrypoint: "vineyard",
				State: model.StateWorking, StateDetail: "Starting…", Model: info.Model, PermissionMode: info.PermissionMode,
				StartedAt: info.StartedAt, LastActivityAt: info.StartedAt, PendingTools: []model.PendingTool{},
			})
			a = &agents[len(agents)-1]
		}
		copyInfo := info
		a.Managed = &copyInfo
		if info.Exited {
			continue
		}
		// What the control channel told us is more current than what the transcript shows: after a
		// live switch the transcript only catches up on the next assistant message.
		if info.Model != "" {
			a.Model = info.Model
		}
		if info.Effort != "" {
			a.Effort = info.Effort
		}
		if info.PermissionMode != "" {
			a.PermissionMode = info.PermissionMode
		}
		if info.Context != nil {
			a.ContextWindow = info.Context.MaxTokens
			// The harness's own measurement wins over an older transcript figure (right after a
			// compaction the transcript still shows the pre-compaction size).
			if info.Context.At >= a.ContextAt {
				a.ContextTokens = info.Context.TotalTokens
				a.ContextAt = info.Context.At
			}
		}
		if info.Pending != nil {
			if info.Pending.ToolName == "AskUserQuestion" {
				a.State = model.StateQuestion
				a.StateDetail = questionSummary(info.Pending.Input)
			} else {
				a.State = model.StatePermission
				a.StateDetail = "Permission: " + info.Pending.ToolName
			}
		}
	}
	return agents
}

func questionSummary(input json.RawMessage) string {
	var v struct {
		Questions []struct {
			Question string `json:"question"`
		} `json:"questions"`
	}
	if json.Unmarshal(input, &v) == nil && len(v.Questions) > 0 {
		return v.Questions[0].Question
	}
	return "Waiting for your answer"
}
