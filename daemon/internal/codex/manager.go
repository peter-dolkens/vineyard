package codex

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/peter-dolkens/vineyard/daemon/internal/model"
)

// Manager runs Codex threads as children of the daemon: one `codex app-server` per thread, spoken
// to over newline-delimited JSON-RPC on stdio. Prompts go in as turn/start (or turn/steer while a
// turn runs); approval prompts, questions and MCP elicitations arrive as server requests and are
// answered with the cards the chat already has for Claude Code. The app-server writes the normal
// rollout and holds the thread's writer lock, so the collector sees the thread like any other.
// Verified against Codex 0.158.
type Manager struct {
	mu       sync.Mutex
	procs    map[string]*proc
	log      *log.Logger
	onChange func()
	// CodexBin overrides binary discovery; CodexDir sets CODEX_HOME for the children.
	CodexBin string
	CodexDir string
}

type SpawnOptions struct {
	Cwd            string `json:"cwd"`
	Prompt         string `json:"prompt,omitempty"`
	Model          string `json:"model,omitempty"`
	Effort         string `json:"effort,omitempty"`
	PermissionMode string `json:"permissionMode,omitempty"`
	Resume         string `json:"resume,omitempty"` // thread id to continue
	Name           string `json:"name,omitempty"`
}

type proc struct {
	info   model.ManagedInfo
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	wmu    sync.Mutex
	stderr []string
	done   chan struct{}
	nextID atomic.Int64
	calls  map[int64]chan rpcReply
	// activeTurn is the turn in progress, for turn/steer and turn/interrupt.
	activeTurn string
	// pending is the server request the thread is blocked on, kept whole so the answer can be
	// mapped back to what the method expects.
	pending *serverRequest
	// fileChanges remembers the changes of fileChange items as they start, so an approval prompt
	// for one can show them (the request itself only names the item).
	fileChanges map[string]json.RawMessage
	// overrides are model, effort and mode changes made from the chat, applied on the next turn.
	overrideModel, overrideEffort, overrideMode string
	overrides                                   bool
}

type rpcReply struct {
	result json.RawMessage
	err    error
}

type serverRequest struct {
	ID     json.RawMessage
	Method string
	Params map[string]any
	Raw    json.RawMessage
}

const rpcTimeout = 30 * time.Second

func New(logger *log.Logger, onChange func()) *Manager {
	if logger == nil {
		logger = log.Default()
	}
	return &Manager{procs: map[string]*proc{}, log: logger, onChange: onChange}
}

func (m *Manager) changed() {
	if m.onChange != nil {
		m.onChange()
	}
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

// Spawn starts an app-server, opens (or resumes) a thread and returns its id once Codex has
// answered, which takes well under a second; MCP servers start on the first turn.
func (m *Manager) Spawn(o SpawnOptions) (string, error) {
	if o.Cwd == "" {
		return "", errors.New("cwd is required")
	}
	if st, err := os.Stat(o.Cwd); err != nil || !st.IsDir() {
		return "", fmt.Errorf("workspace %s is not a directory", o.Cwd)
	}
	bin, err := FindCodex(m.CodexBin)
	if err != nil {
		return "", errors.New("codex binary not found; install the Codex CLI or set codexBin in ~/.vineyard/config.json")
	}
	if o.Resume != "" && m.Has(o.Resume) {
		return "", fmt.Errorf("thread %s is already managed by this daemon", o.Resume)
	}
	args := []string{"app-server"}
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.Command(bin, args...)
	} else {
		cmd = exec.Command(loginShell(), append([]string{"-lc", `exec "$0" "$@"`, bin}, args...)...)
	}
	cmd.Dir = o.Cwd
	cmd.Env = os.Environ()
	if m.CodexDir != "" {
		cmd.Env = append(cmd.Env, "CODEX_HOME="+m.CodexDir)
	}
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
		return "", fmt.Errorf("start codex: %w", err)
	}
	p := &proc{
		cmd: cmd, stdin: stdin, done: make(chan struct{}), calls: map[int64]chan rpcReply{}, fileChanges: map[string]json.RawMessage{},
		info: model.ManagedInfo{PID: cmd.Process.Pid, Cwd: o.Cwd, StartedAt: time.Now().UnixMilli(), Name: o.Name, Resumed: o.Resume != "", Modes: Modes},
	}
	go m.readStdout(p, stdout)
	go m.readStderr(p, stderr)
	go m.wait(p)

	fail := func(err error) (string, error) {
		p.wmu.Lock()
		_ = stdin.Close()
		p.wmu.Unlock()
		go func() {
			select {
			case <-p.done:
			case <-time.After(5 * time.Second):
				_ = cmd.Process.Kill()
			}
		}()
		return "", err
	}
	if _, err := m.call(p, "initialize", map[string]any{"clientInfo": map[string]any{"name": "vineyard", "version": "0.3"}}); err != nil {
		return fail(fmt.Errorf("codex initialize: %w", err))
	}
	var res struct {
		Thread struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"thread"`
		Model           string `json:"model"`
		ReasoningEffort string `json:"reasoningEffort"`
		ApprovalPolicy  any    `json:"approvalPolicy"`
		Sandbox         struct {
			Type string `json:"type"`
		} `json:"sandbox"`
	}
	var body json.RawMessage
	if o.Resume != "" {
		body, err = m.call(p, "thread/resume", map[string]any{"threadId": o.Resume, "cwd": o.Cwd, "excludeTurns": true})
	} else {
		params := map[string]any{"cwd": o.Cwd}
		if o.Model != "" {
			params["model"] = o.Model
		}
		if o.PermissionMode != "" {
			sandbox, approval, _ := ModeSettings(o.PermissionMode)
			params["sandbox"] = sandbox
			params["approvalPolicy"] = approval
		}
		body, err = m.call(p, "thread/start", params)
	}
	if err != nil {
		return fail(fmt.Errorf("codex thread: %w", err))
	}
	if json.Unmarshal(body, &res) != nil || res.Thread.ID == "" {
		return fail(errors.New("codex did not report a thread id"))
	}
	sid := res.Thread.ID
	m.mu.Lock()
	if old, ok := m.procs[sid]; ok && !old.info.Exited {
		m.mu.Unlock()
		return fail(fmt.Errorf("thread %s is already managed by this daemon", sid))
	}
	p.info.SessionID = sid
	p.info.Model = res.Model
	p.info.Effort = res.ReasoningEffort
	if o.Effort != "" && o.Resume == "" {
		p.info.Effort = o.Effort
		p.overrideEffort, p.overrides = o.Effort, true
	}
	p.info.PermissionMode = Mode(sandboxName(res.Sandbox.Type), approvalName(res.ApprovalPolicy))
	if res.Thread.Name != "" {
		p.info.Name = res.Thread.Name
	}
	p.info.Ready = true
	m.procs[sid] = p
	m.mu.Unlock()
	m.log.Printf("codex: spawned thread %s (pid %d) in %s", sid, cmd.Process.Pid, o.Cwd)
	if o.Name != "" && o.Resume == "" {
		go func() { _ = m.Rename(sid, o.Name) }()
	}
	go m.describe(p)
	if strings.TrimSpace(o.Prompt) != "" {
		if err := m.Send(sid, o.Prompt); err != nil {
			return sid, err
		}
	}
	m.changed()
	return sid, nil
}

// sandboxName maps the app-server's SandboxPolicy type back to the mode strings the CLI uses.
func sandboxName(t string) string {
	switch t {
	case "dangerFullAccess":
		return "danger-full-access"
	case "readOnly":
		return "read-only"
	case "workspaceWrite":
		return "workspace-write"
	}
	return t
}

func approvalName(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

// describe asks what the account and model picker look like, after the thread is up.
func (m *Manager) describe(p *proc) {
	if body, err := m.call(p, "model/list", map[string]any{}); err == nil {
		var v struct {
			Data []struct {
				ID                        string `json:"id"`
				Model                     string `json:"model"`
				DisplayName               string `json:"displayName"`
				Description               string `json:"description"`
				Hidden                    bool   `json:"hidden"`
				IsDefault                 bool   `json:"isDefault"`
				SupportedReasoningEfforts []struct {
					ReasoningEffort string `json:"reasoningEffort"`
				} `json:"supportedReasoningEfforts"`
			} `json:"data"`
		}
		if json.Unmarshal(body, &v) == nil {
			var models []model.ModelInfo
			for _, r := range v.Data {
				if r.Hidden || r.ID == "" {
					continue
				}
				mi := model.ModelInfo{Value: r.ID, ResolvedModel: r.Model, DisplayName: r.DisplayName, Description: r.Description}
				for _, e := range r.SupportedReasoningEfforts {
					mi.SupportedEffortLevels = append(mi.SupportedEffortLevels, e.ReasoningEffort)
				}
				models = append(models, mi)
			}
			m.mu.Lock()
			p.info.Models = models
			m.mu.Unlock()
		}
	}
	if body, err := m.call(p, "account/read", map[string]any{}); err == nil {
		var v struct {
			Account struct {
				Type     string `json:"type"`
				Email    string `json:"email"`
				PlanType string `json:"planType"`
			} `json:"account"`
		}
		if json.Unmarshal(body, &v) == nil {
			m.mu.Lock()
			if v.Account.Email != "" {
				p.info.Account = v.Account.Email
			} else if v.Account.Type != "" {
				p.info.Account = v.Account.Type
			}
			p.info.AccountPlan = v.Account.PlanType
			m.mu.Unlock()
		}
	}
	if body, err := m.call(p, "account/rateLimits/read", map[string]any{}); err == nil {
		var v struct {
			RateLimits map[string]any `json:"rateLimits"`
		}
		if json.Unmarshal(body, &v) == nil {
			if u := parseRateLimits(v.RateLimits, time.Now().UnixMilli()); u != nil {
				m.mu.Lock()
				p.info.Usage = u
				m.mu.Unlock()
			}
		}
	}
	m.changed()
}

// ---- JSON-RPC ------------------------------------------------------------------------------------

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

// call sends a request and waits for its response.
func (m *Manager) call(p *proc, method string, params any) (json.RawMessage, error) {
	id := p.nextID.Add(1)
	ch := make(chan rpcReply, 1)
	m.mu.Lock()
	p.calls[id] = ch
	m.mu.Unlock()
	forget := func() {
		m.mu.Lock()
		delete(p.calls, id)
		m.mu.Unlock()
	}
	if err := m.write(p, map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
		forget()
		return nil, err
	}
	select {
	case r := <-ch:
		return r.result, r.err
	case <-p.done:
		forget()
		return nil, errors.New("codex exited before it answered")
	case <-time.After(rpcTimeout):
		forget()
		return nil, fmt.Errorf("codex did not answer %s in time", method)
	}
}

func (m *Manager) reply(p *proc, id json.RawMessage, result any) error {
	return m.write(p, map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

func (m *Manager) replyError(p *proc, id json.RawMessage, msg string) error {
	return m.write(p, map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": -32601, "message": msg}})
}

func (m *Manager) readStdout(p *proc, r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for sc.Scan() {
		line := sc.Bytes()
		var env struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
			Result json.RawMessage `json:"result"`
			Error  *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(line, &env) != nil {
			continue
		}
		switch {
		case env.Method != "" && len(env.ID) > 0 && string(env.ID) != "null":
			m.handleServerRequest(p, env.ID, env.Method, env.Params, line)
		case env.Method != "":
			m.handleNotification(p, env.Method, env.Params)
		case len(env.ID) > 0:
			var id int64
			if json.Unmarshal(env.ID, &id) != nil {
				continue
			}
			m.mu.Lock()
			ch, ok := p.calls[id]
			delete(p.calls, id)
			m.mu.Unlock()
			if !ok {
				continue
			}
			if env.Error != nil {
				msg := env.Error.Message
				if msg == "" {
					msg = "rejected by codex"
				}
				ch <- rpcReply{err: errors.New(msg)}
			} else {
				ch <- rpcReply{result: env.Result}
			}
		}
	}
}

func (m *Manager) readStderr(p *proc, r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
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
	p.pending = nil
	sid := p.info.SessionID
	m.mu.Unlock()
	close(p.done)
	m.log.Printf("codex: thread %s exited (%v)", sid, err)
	m.changed()
	time.AfterFunc(10*time.Minute, func() {
		m.mu.Lock()
		if cur, ok := m.procs[sid]; ok && cur == p {
			delete(m.procs, sid)
		}
		m.mu.Unlock()
	})
}

// ---- notifications and server requests ------------------------------------------------------------

func (m *Manager) handleNotification(p *proc, method string, raw json.RawMessage) {
	var params map[string]any
	_ = json.Unmarshal(raw, &params)
	now := time.Now().UnixMilli()
	changed := false
	m.mu.Lock()
	switch method {
	case "turn/started":
		p.activeTurn = str(obj(params["turn"])["id"])
		changed = true
	case "turn/completed":
		p.activeTurn = ""
		p.info.Turns++
		turn := obj(params["turn"])
		if e := obj(turn["error"]); e != nil {
			p.info.LastError = str(e["message"])
		} else if str(turn["status"]) == "completed" {
			p.info.LastError = ""
		}
		p.info.Pending, p.pending = nil, nil
		changed = true
	case "error":
		if willRetry, _ := params["willRetry"].(bool); !willRetry {
			p.info.LastError = str(obj(params["error"])["message"])
			changed = true
		}
	case "thread/tokenUsage/updated":
		tu := obj(params["tokenUsage"])
		last := obj(tu["last"])
		if total, ok := num(last["totalTokens"]); ok && total > 0 {
			window, _ := num(tu["modelContextWindow"])
			cu := &model.ContextUsage{TotalTokens: int64(total), MaxTokens: int64(window), Model: p.info.Model, At: now}
			if window > 0 {
				cu.Percentage = 100 * total / window
			}
			p.info.Context = cu
			changed = true
		}
	case "account/rateLimits/updated":
		if u := parseRateLimits(obj(params["rateLimits"]), now); u != nil {
			p.info.Usage = u
			changed = true
		}
	case "account/updated":
		if s := str(params["planType"]); s != "" {
			p.info.AccountPlan = s
		}
	case "thread/name/updated":
		if s := str(params["threadName"]); s != "" {
			p.info.Name = s
			changed = true
		}
	case "item/started":
		item := obj(params["item"])
		if str(item["type"]) == "fileChange" {
			if b, err := json.Marshal(item["changes"]); err == nil {
				p.fileChanges[str(item["id"])] = b
			}
		}
	case "serverRequest/resolved":
		// Answered or withdrawn elsewhere (a thread shared with another client, a timeout).
		if p.pending != nil && string(p.pending.ID) == string(rawJSON(params["requestId"])) {
			p.info.Pending, p.pending = nil, nil
			changed = true
		}
	case "thread/settings/updated":
		if s := str(obj(params["settings"])["model"]); s != "" {
			p.info.Model = s
			changed = true
		}
	}
	m.mu.Unlock()
	if changed {
		m.changed()
	}
}

func rawJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

// handleServerRequest turns a request from Codex into the pending card the chat shows, or answers
// it itself when it is nothing a person can decide.
func (m *Manager) handleServerRequest(p *proc, id json.RawMessage, method string, raw json.RawMessage, line []byte) {
	var params map[string]any
	_ = json.Unmarshal(raw, &params)
	now := time.Now().UnixMilli()
	m.mu.Lock()
	pr := pendingFor(method, params, p.fileChanges, now)
	if pr == nil {
		m.mu.Unlock()
		m.log.Printf("codex: %s refused server request %q", p.info.SessionID, method)
		_ = m.replyError(p, id, "not supported by vineyard")
		return
	}
	pr.RequestID = strings.Trim(string(id), `"`)
	p.info.Pending = pr
	p.pending = &serverRequest{ID: append(json.RawMessage(nil), id...), Method: method, Params: params, Raw: append(json.RawMessage(nil), line...)}
	m.mu.Unlock()
	m.changed()
}

// pendingFor maps a server request onto the request shapes the chat's cards know: a command
// approval looks like a Bash permission, a file change like an Edit, request_user_input like an
// AskUserQuestion, an MCP elicitation like Claude Code's. nil for methods nobody can answer here.
func pendingFor(method string, params map[string]any, fileChanges map[string]json.RawMessage, now int64) *model.PendingRequest {
	input := func(v map[string]any) json.RawMessage {
		b, _ := json.Marshal(v)
		return b
	}
	remember := json.RawMessage(`[{"type":"codex","scope":"session"}]`)
	itemID := str(params["itemId"])
	reason := str(params["reason"])
	switch method {
	case "item/commandExecution/requestApproval", "execCommandApproval":
		cmd := str(params["command"])
		if arr, ok := params["command"].([]any); ok {
			var parts []string
			for _, a := range arr {
				parts = append(parts, str(a))
			}
			cmd = strings.Join(parts, " ")
		}
		in := map[string]any{"command": cmd}
		if c := str(params["cwd"]); c != "" {
			in["cwd"] = c
		}
		if reason != "" {
			in["reason"] = reason
		}
		if str(params["kind"]) == "writeStdin" {
			in["description"] = "Send input to the running command"
		}
		return &model.PendingRequest{ToolName: "Bash", DisplayName: "Codex command", Input: input(in), ToolUseID: itemID, RequiresUserInteraction: true, Suggestions: remember, Description: reason, At: now, Kind: model.PendingPermission}
	case "item/fileChange/requestApproval", "applyPatchApproval":
		in := map[string]any{}
		if reason != "" {
			in["reason"] = reason
		}
		if root := str(params["grantRoot"]); root != "" {
			in["grantRoot"] = root
		}
		if raw, ok := fileChanges[itemID]; ok {
			var changes any
			_ = json.Unmarshal(raw, &changes)
			in["changes"] = changes
		} else if ch, ok := params["changes"]; ok {
			in["changes"] = ch
		}
		return &model.PendingRequest{ToolName: "Edit", DisplayName: "Codex file changes", Input: input(in), ToolUseID: itemID, RequiresUserInteraction: true, Suggestions: remember, Description: reason, At: now, Kind: model.PendingPermission}
	case "item/tool/requestUserInput":
		var questions []map[string]any
		if qs, ok := params["questions"].([]any); ok {
			for _, q := range qs {
				qm := obj(q)
				row := map[string]any{"question": str(qm["question"]), "header": str(qm["header"]), "id": str(qm["id"])}
				var opts []map[string]any
				if os, ok := qm["options"].([]any); ok {
					for _, o := range os {
						om := obj(o)
						opts = append(opts, map[string]any{"label": str(om["label"]), "description": str(om["description"])})
					}
				}
				if opts != nil {
					row["options"] = opts
				}
				questions = append(questions, row)
			}
		}
		return &model.PendingRequest{ToolName: "AskUserQuestion", DisplayName: "AskUserQuestion", Input: input(map[string]any{"questions": questions}), ToolUseID: itemID, RequiresUserInteraction: true, At: now, Kind: model.PendingPermission}
	case "item/permissions/requestApproval":
		in := map[string]any{"permissions": params["permissions"]}
		if reason != "" {
			in["reason"] = reason
		}
		if c := str(params["cwd"]); c != "" {
			in["cwd"] = c
		}
		return &model.PendingRequest{ToolName: "Permissions", DisplayName: "Codex asks for extra permissions", Input: input(in), ToolUseID: itemID, RequiresUserInteraction: true, Suggestions: remember, Description: reason, At: now, Kind: model.PendingPermission}
	case "mcpServer/elicitation/request":
		mode := str(params["mode"])
		url := str(params["url"])
		if mode == "" {
			if url != "" {
				mode = "url"
			} else {
				mode = "form"
			}
		}
		var schema json.RawMessage
		if s, ok := params["requestedSchema"]; ok && s != nil {
			schema, _ = json.Marshal(s)
		}
		e := &model.ElicitationRequest{ServerName: str(params["serverName"]), Message: str(params["message"]), Mode: mode, URL: url, ElicitationID: str(params["elicitationId"]), RequestedSchema: schema, Title: str(params["title"]), Description: str(params["description"])}
		return &model.PendingRequest{ToolName: "elicitation", DisplayName: e.ServerName, Description: e.Description, At: now, Kind: model.PendingElicitation, Elicitation: e}
	}
	return nil
}

// ---- control -----------------------------------------------------------------------------------

func (m *Manager) get(sid string) (*proc, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.procs[sid]
	if !ok {
		return nil, fmt.Errorf("thread %s is not managed by this daemon", sid)
	}
	if p.info.Exited {
		return nil, fmt.Errorf("managed thread %s has exited", sid)
	}
	return p, nil
}

func (m *Manager) Has(sid string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.procs[sid]
	return ok && !p.info.Exited
}

// Cwd is the directory a managed thread runs in, or "" when this daemon does not manage it.
func (m *Manager) Cwd(sid string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if p, ok := m.procs[sid]; ok {
		return p.info.Cwd
	}
	return ""
}

// Send delivers a prompt: steered into the running turn, or as a new turn with any model, effort
// or mode change made since the last one. "/compact" asks Codex to compact the thread instead.
func (m *Manager) Send(sid, text string) error {
	return m.SendInput(sid, []map[string]any{{"type": "text", "text": text}})
}

// SendInput is Send for a prepared input list (text and image parts).
func (m *Manager) SendInput(sid string, input []map[string]any) error {
	p, err := m.get(sid)
	if err != nil {
		return err
	}
	if len(input) == 1 && str(input[0]["type"]) == "text" && strings.TrimSpace(str(input[0]["text"])) == "/compact" {
		_, err := m.call(p, "thread/compact/start", map[string]any{"threadId": sid})
		return err
	}
	m.mu.Lock()
	turn := p.activeTurn
	m.mu.Unlock()
	if turn != "" {
		if _, err := m.call(p, "turn/steer", map[string]any{"threadId": sid, "expectedTurnId": turn, "input": input}); err == nil {
			return nil
		}
		// The turn ended in the meantime: start a new one.
	}
	params := map[string]any{"threadId": sid, "input": input}
	m.mu.Lock()
	if p.overrides {
		if p.overrideModel != "" {
			params["model"] = p.overrideModel
		}
		if p.overrideEffort != "" {
			params["effort"] = p.overrideEffort
		}
		if p.overrideMode != "" {
			_, approval, policy := ModeSettings(p.overrideMode)
			params["approvalPolicy"] = approval
			params["sandboxPolicy"] = policy
		}
		p.overrides = false
	}
	m.mu.Unlock()
	body, err := m.call(p, "turn/start", params)
	if err != nil {
		return err
	}
	var res struct {
		Turn struct {
			ID string `json:"id"`
		} `json:"turn"`
	}
	if json.Unmarshal(body, &res) == nil && res.Turn.ID != "" {
		m.mu.Lock()
		p.activeTurn = res.Turn.ID
		m.mu.Unlock()
	}
	m.changed()
	return nil
}

// Respond answers the pending server request with what the chat's card sent: the Claude-shaped
// {"behavior":"allow"|"deny", "updatedInput", "updatedPermissions", "message"} for permissions and
// questions, {"action":…, "content":…} for an elicitation.
func (m *Manager) Respond(sid, requestID string, response json.RawMessage) error {
	p, err := m.get(sid)
	if err != nil {
		return err
	}
	m.mu.Lock()
	req := p.pending
	if req == nil || p.info.Pending == nil || p.info.Pending.RequestID != requestID {
		m.mu.Unlock()
		return fmt.Errorf("request %s is not pending", requestID)
	}
	p.info.Pending, p.pending = nil, nil
	m.mu.Unlock()

	var answer struct {
		Behavior           string          `json:"behavior"`
		Message            string          `json:"message"`
		UpdatedInput       map[string]any  `json:"updatedInput"`
		UpdatedPermissions json.RawMessage `json:"updatedPermissions"`
		Action             string          `json:"action"`
		Content            json.RawMessage `json:"content"`
	}
	_ = json.Unmarshal(response, &answer)
	allow := answer.Behavior == "allow"
	remember := len(answer.UpdatedPermissions) > 0 && string(answer.UpdatedPermissions) != "null"
	var result any
	switch req.Method {
	case "item/commandExecution/requestApproval", "item/fileChange/requestApproval":
		switch {
		case allow && remember:
			result = map[string]any{"decision": "acceptForSession"}
		case allow:
			result = map[string]any{"decision": "accept"}
		default:
			result = map[string]any{"decision": "decline"}
		}
	case "execCommandApproval", "applyPatchApproval":
		switch {
		case allow && remember:
			result = map[string]any{"decision": "approved_for_session"}
		case allow:
			result = map[string]any{"decision": "approved"}
		default:
			result = map[string]any{"decision": "denied"}
		}
	case "item/tool/requestUserInput":
		answers := map[string]any{}
		given, _ := answer.UpdatedInput["answers"].(map[string]any)
		if qs, ok := req.Params["questions"].([]any); ok {
			for _, q := range qs {
				qm := obj(q)
				id, question := str(qm["id"]), str(qm["question"])
				if a := strings.TrimSpace(str(given[question])); a != "" && id != "" {
					answers[id] = map[string]any{"answers": []string{a}}
				}
			}
		}
		result = map[string]any{"answers": answers}
	case "item/permissions/requestApproval":
		if allow {
			scope := "turn"
			if remember {
				scope = "session"
			}
			result = map[string]any{"permissions": req.Params["permissions"], "scope": scope}
		} else {
			result = map[string]any{"permissions": map[string]any{}}
		}
	case "mcpServer/elicitation/request":
		action := answer.Action
		if action == "" {
			action = "cancel"
		}
		r := map[string]any{"action": action}
		if action == "accept" && len(answer.Content) > 0 {
			r["content"] = answer.Content
		}
		result = r
	default:
		return m.replyError(p, req.ID, "not supported by vineyard")
	}
	if err := m.reply(p, req.ID, result); err != nil {
		return err
	}
	m.changed()
	// A deny with a reason of the user's own: Codex has no field for it, so steer the turn with it.
	if !allow && req.Method != "mcpServer/elicitation/request" && req.Method != "item/tool/requestUserInput" {
		if msg := strings.TrimSpace(answer.Message); msg != "" && !strings.HasPrefix(msg, "The user denied this action") {
			m.mu.Lock()
			turn := p.activeTurn
			m.mu.Unlock()
			if turn != "" {
				input := []map[string]any{{"type": "text", "text": "The user declined that action and says: " + msg}}
				_, _ = m.call(p, "turn/steer", map[string]any{"threadId": sid, "expectedTurnId": turn, "input": input})
			}
		}
	}
	return nil
}

// Interrupt stops the turn in progress.
func (m *Manager) Interrupt(sid string) error {
	p, err := m.get(sid)
	if err != nil {
		return err
	}
	m.mu.Lock()
	turn := p.activeTurn
	m.mu.Unlock()
	if turn == "" {
		return errors.New("no turn is in progress")
	}
	_, err = m.call(p, "turn/interrupt", map[string]any{"threadId": sid, "turnId": turn})
	return err
}

// Stop ends the app-server: closing its stdin makes it exit; it is killed after a grace period.
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

// SetModel picks the model for the next turn on; "" returns to Codex's configured default.
func (m *Manager) SetModel(sid, modelID string) error {
	p, err := m.get(sid)
	if err != nil {
		return err
	}
	m.mu.Lock()
	if modelID != "" && len(p.info.Models) > 0 {
		known := false
		for _, mi := range p.info.Models {
			if mi.Value == modelID {
				known = true
				break
			}
		}
		if !known {
			m.mu.Unlock()
			return fmt.Errorf("codex does not offer model %q", modelID)
		}
	}
	p.overrideModel, p.overrides = modelID, true
	if modelID != "" {
		p.info.Model = modelID
	}
	m.mu.Unlock()
	m.changed()
	return nil
}

// SetEffort picks the reasoning effort for the next turn on.
func (m *Manager) SetEffort(sid, effort string) error {
	p, err := m.get(sid)
	if err != nil {
		return err
	}
	m.mu.Lock()
	p.overrideEffort, p.overrides = effort, true
	p.info.Effort = effort
	m.mu.Unlock()
	m.changed()
	return nil
}

// SetPermissionMode picks the sandbox and approval level for the next turn on (see Modes).
func (m *Manager) SetPermissionMode(sid, mode string) error {
	p, err := m.get(sid)
	if err != nil {
		return err
	}
	if mode == "" {
		mode = "workspace-write"
	}
	known := false
	for _, mi := range Modes {
		if mi.Value == mode {
			known = true
		}
	}
	if !known {
		return fmt.Errorf("unknown codex mode %q", mode)
	}
	m.mu.Lock()
	p.overrideMode, p.overrides = mode, true
	p.info.PermissionMode = mode
	m.mu.Unlock()
	m.changed()
	return nil
}

// Rename sets the thread's name; Codex's own UIs show it too.
func (m *Manager) Rename(sid, name string) error {
	p, err := m.get(sid)
	if err != nil {
		return err
	}
	if _, err := m.call(p, "thread/name/set", map[string]any{"threadId": sid, "name": name}); err != nil {
		return fmt.Errorf("rename: %w", err)
	}
	m.mu.Lock()
	p.info.Name = name
	m.mu.Unlock()
	m.changed()
	return nil
}

// All returns a copy of every managed thread's info.
func (m *Manager) All() []model.ManagedInfo {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]model.ManagedInfo, 0, len(m.procs))
	for _, p := range m.procs {
		out = append(out, p.info)
	}
	return out
}

// LatestUsage is the newest limit report from any thread this daemon manages, or nil.
func (m *Manager) LatestUsage() *model.Usage {
	m.mu.Lock()
	defer m.mu.Unlock()
	var best *model.Usage
	for _, p := range m.procs {
		if p.info.Usage != nil && (best == nil || p.info.Usage.At > best.At) {
			best = p.info.Usage
		}
	}
	if best == nil {
		return nil
	}
	cp := *best
	return &cp
}

// Merge annotates collector-derived agents with managed state and synthesises entries for threads
// whose rollout the collector has not seen yet.
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
				ID: machineID + "::" + info.SessionID, Provider: Provider, MachineID: machineID, WorkspacePath: normalisePath(info.Cwd), Cwd: normalisePath(info.Cwd),
				SessionID: info.SessionID, PID: info.PID, Alive: true, Name: info.Name, Kind: "managed", Entrypoint: "vineyard",
				State: model.StateIdle, StateDetail: "Ready", Model: info.Model, Effort: info.Effort, PermissionMode: info.PermissionMode,
				StartedAt: info.StartedAt, LastActivityAt: info.StartedAt, PendingTools: []model.PendingTool{},
			})
			a = &agents[len(agents)-1]
		}
		copyInfo := info
		a.Managed = &copyInfo
		if info.Exited {
			continue
		}
		a.Kind = "managed"
		a.Entrypoint = "vineyard"
		a.PID = info.PID
		if info.Name != "" {
			a.Title = info.Name
		}
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
			if info.Context.MaxTokens > 0 {
				a.ContextWindow = info.Context.MaxTokens
			}
			if info.Context.At >= a.ContextAt {
				a.ContextTokens, a.ContextAt = info.Context.TotalTokens, info.Context.At
			}
		}
		if info.Pending != nil {
			switch {
			case info.Pending.Kind == model.PendingElicitation:
				a.State, a.StateDetail = model.StateQuestion, elicitationSummary(info.Pending.Elicitation)
			case info.Pending.ToolName == "AskUserQuestion":
				a.State, a.StateDetail = model.StateQuestion, questionSummary(info.Pending.Input)
			default:
				a.State = model.StatePermission
				a.StateDetail = "Permission: " + info.Pending.DisplayName
				if info.Pending.ToolName == "Bash" {
					var in struct {
						Command string `json:"command"`
					}
					if json.Unmarshal(info.Pending.Input, &in) == nil && in.Command != "" {
						a.StateDetail = "Permission: " + clip(in.Command, 100)
					}
				}
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

func elicitationSummary(e *model.ElicitationRequest) string {
	if e == nil {
		return "Waiting for your answer"
	}
	who := e.DisplayName
	if who == "" {
		who = e.ServerName
	}
	if who == "" {
		who = "An MCP server"
	}
	what := e.Title
	if what == "" {
		what = e.Message
	}
	if what = clip(what, 80); what == "" {
		return who + " asks a question"
	}
	return who + " asks: " + what
}
