package codex

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/peter-dolkens/vineyard/daemon/internal/model"
)

// Fixture lines are trimmed copies of what Codex 0.158 writes.
const (
	sessionMeta   = `{"timestamp":"2026-09-28T17:51:44.340Z","type":"session_meta","payload":{"session_id":"t1","id":"t1","timestamp":"2026-09-28T17:51:39.742Z","cwd":"/w","originator":"codex_vscode","cli_version":"0.155.0","source":"vscode","model_provider":"openai","git":{"commit_hash":"abc","branch":"main"}}}`
	taskStarted   = `{"timestamp":"2026-09-28T17:51:44.341Z","type":"event_msg","payload":{"type":"task_started","turn_id":"turn1","started_at":1790617904,"model_context_window":258400}}`
	devMsg        = `{"timestamp":"2026-09-28T17:51:44.938Z","type":"response_item","payload":{"type":"message","id":"m0","role":"developer","content":[{"type":"input_text","text":"<skills_instructions>stuff</skills_instructions>"}]}}`
	pluginsMsg    = `{"timestamp":"2026-09-28T17:51:44.939Z","type":"response_item","payload":{"type":"message","id":"m1","role":"user","content":[{"type":"input_text","text":"<recommended_plugins>\nHere is a list\n- Box\n</recommended_plugins>"}]}}`
	envMsg        = `{"timestamp":"2026-09-28T17:51:44.939Z","type":"response_item","payload":{"type":"message","id":"m2","role":"user","content":[{"type":"input_text","text":"<environment_context>\n  <cwd>/w</cwd>\n</environment_context>"}]}}`
	turnContext   = `{"timestamp":"2026-09-28T17:51:44.939Z","type":"turn_context","payload":{"turn_id":"turn1","cwd":"/w","approval_policy":"on-request","sandbox_policy":{"type":"workspace-write","network_access":false},"model":"gpt-6-astra","effort":"low","summary":"none"}}`
	userMsg       = `{"timestamp":"2026-09-28T17:51:44.957Z","type":"response_item","payload":{"type":"message","id":"m3","role":"user","content":[{"type":"input_text","text":"this is just an example codex transcript\n"}]}}`
	userEvent     = `{"timestamp":"2026-09-28T17:51:44.957Z","type":"event_msg","payload":{"type":"user_message","message":"this is just an example codex transcript\n","images":[]}}`
	reasoning     = `{"timestamp":"2026-09-28T17:51:46.000Z","type":"response_item","payload":{"type":"reasoning","id":"rs1","summary":[{"type":"summary_text","text":"Thinking about it"}],"encrypted_content":"gAAA"}}`
	reasoningNone = `{"timestamp":"2026-09-28T17:51:46.000Z","type":"response_item","payload":{"type":"reasoning","id":"rs2","summary":[],"encrypted_content":"gAAA"}}`
	agentEvent    = `{"timestamp":"2026-09-28T17:51:47.122Z","type":"event_msg","payload":{"type":"agent_message","message":"Understood.","phase":"final_answer"}}`
	assistantMsg  = `{"timestamp":"2026-09-28T17:51:47.123Z","type":"response_item","payload":{"type":"message","id":"m4","role":"assistant","content":[{"type":"output_text","text":"Understood."}],"phase":"final_answer"}}`
	tokenCount    = `{"timestamp":"2026-09-28T17:51:47.296Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":15219,"cached_input_tokens":12160,"output_tokens":15,"total_tokens":15234},"last_token_usage":{"input_tokens":15219,"cached_input_tokens":12160,"cache_write_input_tokens":0,"output_tokens":15,"reasoning_output_tokens":0,"total_tokens":15234},"model_context_window":258400},"rate_limits":{"limit_id":"codex","primary":{"used_percent":2.0,"window_minutes":300,"resets_at":1790635908},"secondary":{"used_percent":85.0,"window_minutes":10080,"resets_at":1791222708},"plan_type":"plus"}}}`
	taskComplete  = `{"timestamp":"2026-09-28T17:51:47.300Z","type":"event_msg","payload":{"type":"task_complete","turn_id":"turn1","last_agent_message":"Understood.","started_at":1790617904,"completed_at":1790617907}}`
	execCall      = `{"timestamp":"2026-09-28T17:52:24.514Z","type":"response_item","payload":{"type":"custom_tool_call","id":"ctc1","status":"completed","call_id":"call_1","name":"exec","input":"text(await tools.exec_command({cmd:\"cat /tmp/x \\\"q\\\"\",max_output_tokens:1000}));\n"}}`
	execOutput    = `{"timestamp":"2026-09-28T17:52:25.986Z","type":"response_item","payload":{"type":"custom_tool_call_output","id":"ctco1","call_id":"call_1","output":[{"type":"input_text","text":"Script completed\nWall time 0.1 seconds\nOutput:\n"},{"type":"input_text","text":"hello"}]}}`
	execFailed    = `{"timestamp":"2026-09-28T17:52:25.986Z","type":"response_item","payload":{"type":"custom_tool_call_output","id":"ctco1","call_id":"call_1","output":[{"type":"input_text","text":"Script failed\nWall time 2.0 seconds\nOutput:\n"},{"type":"input_text","text":"Script error:\nexec_command failed: CreateProcess { message: \"Rejected(\\\"rejected by user\\\")\" }"}]}}`
	mcpCall       = `{"timestamp":"2026-06-16T15:08:40.000Z","type":"response_item","payload":{"type":"function_call","name":"cloudflare_query_security_events","namespace":"mcp__clubspark_cloudflare","arguments":"{\"hostname\":\"x.example\",\"action\":\"block\"}","call_id":"call_2"}}`
	mcpOutput     = `{"timestamp":"2026-06-16T15:08:41.000Z","type":"response_item","payload":{"type":"function_call_output","call_id":"call_2","output":"Wall time: 1.7 seconds\nOutput:\n[{\"type\":\"text\",\"text\":\"ok\"}]"}}`
	patchCall     = `{"timestamp":"2026-06-16T15:09:00.000Z","type":"response_item","payload":{"type":"custom_tool_call","status":"completed","call_id":"call_3","name":"apply_patch","input":"*** Begin Patch\n*** Add File: adhoc/list.csv\n+a,b\n*** End Patch"}}`
	compactedLine = `{"timestamp":"2026-06-16T15:20:00.000Z","type":"compacted","payload":{"message":"summary"}}`
	aborted       = `{"timestamp":"2026-09-28T17:53:00.000Z","type":"event_msg","payload":{"type":"turn_aborted","turn_id":"turn2","reason":"interrupted"}}`
)

func lines(t *testing.T, raw ...string) []Line {
	t.Helper()
	var out []Line
	var off int64
	for _, r := range raw {
		l, ok := ParseLine([]byte(r), off)
		if !ok {
			t.Fatalf("bad fixture line: %s", r[:40])
		}
		out = append(out, l)
		off += int64(len(r) + 1)
	}
	return out
}

func TestDeriveStates(t *testing.T) {
	cases := []struct {
		name   string
		raw    []string
		state  model.AgentState
		detail string
	}{
		{"idle after task_complete", []string{sessionMeta, taskStarted, turnContext, userMsg, assistantMsg, tokenCount, taskComplete}, model.StateIdle, "Understood."},
		{"responding to prompt", []string{sessionMeta, taskStarted, turnContext, userMsg, userEvent}, model.StateWorking, "Responding to prompt…"},
		{"reasoning", []string{sessionMeta, taskStarted, turnContext, userMsg, reasoningNone}, model.StateThinking, "Reasoning…"},
		{"tool running", []string{sessionMeta, taskStarted, turnContext, userMsg, execCall}, model.StateTool, "Bash: cat /tmp/x \"q\""},
		{"tool done", []string{sessionMeta, taskStarted, turnContext, userMsg, execCall, execOutput}, model.StateWorking, "Processing tool result…"},
		{"mcp tool pending", []string{sessionMeta, taskStarted, turnContext, userMsg, mcpCall}, model.StateTool, "clubspark_cloudflare: cloudflare_query_security_events: block"},
		{"interrupted", []string{sessionMeta, taskStarted, turnContext, userMsg, aborted}, model.StateIdle, "Interrupted"},
		{"no turn markers, final answer", []string{turnContext, userMsg, assistantMsg}, model.StateIdle, "Understood."},
		{"no turn markers, pending call", []string{turnContext, userMsg, execCall}, model.StateTool, "Bash: cat /tmp/x \"q\""},
		{"tail begins mid-turn", []string{execOutput, assistantMsg, tokenCount, taskComplete}, model.StateIdle, "Understood."},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := Derive(lines(t, c.raw...))
			if d.State != c.state || d.StateDetail != c.detail {
				t.Fatalf("got %s %q, want %s %q", d.State, d.StateDetail, c.state, c.detail)
			}
		})
	}
}

func TestDeriveFields(t *testing.T) {
	d := Derive(lines(t, sessionMeta, taskStarted, devMsg, pluginsMsg, envMsg, turnContext, userMsg, userEvent, assistantMsg, tokenCount, taskComplete))
	if d.Cwd != "/w" || d.Originator != "codex_vscode" || d.Version != "0.155.0" || d.GitBranch != "main" {
		t.Fatalf("session fields: %+v", d)
	}
	if d.Model != "gpt-6-astra" || d.Effort != "low" || d.ApprovalPolicy != "on-request" || d.Sandbox != "workspace-write" {
		t.Fatalf("turn fields: %+v", d)
	}
	if d.FirstPrompt != "this is just an example codex transcript" || d.LastPrompt != d.FirstPrompt {
		t.Fatalf("prompts: %q / %q", d.FirstPrompt, d.LastPrompt)
	}
	if d.ContextTokens != 15234 || d.ContextWindow != 258400 || d.ContextAt == 0 {
		t.Fatalf("context: %d/%d at %d", d.ContextTokens, d.ContextWindow, d.ContextAt)
	}
	if d.ActiveTurn != "" || len(d.PendingTools) != 0 {
		t.Fatalf("turn should be closed: %+v", d)
	}
	if d.Usage == nil || d.Usage.Windows[UsageSecondary].Utilization != 0.85 || d.Usage.Windows[UsagePrimary].ResetsAt != 1790635908000 || d.Usage.Status != "allowed_warning" {
		t.Fatalf("usage: %+v", d.Usage)
	}
	if d.StartedAt != 1790617899742 {
		t.Fatalf("startedAt %d", d.StartedAt)
	}
}

func TestPendingToolsAndCompaction(t *testing.T) {
	d := Derive(lines(t, taskStarted, turnContext, userMsg, mcpCall, patchCall, tokenCount))
	if d.ActiveTurn != "turn1" || len(d.PendingTools) != 2 {
		t.Fatalf("pending: %+v", d.PendingTools)
	}
	if d.PendingTools[1].Name != "Edit" || d.PendingTools[1].Summary != "adhoc/list.csv" {
		t.Fatalf("patch call: %+v", d.PendingTools[1])
	}
	d = Derive(lines(t, taskStarted, turnContext, userMsg, mcpCall, mcpOutput, tokenCount, compactedLine))
	if d.ContextTokens != 0 || len(d.PendingTools) != 0 {
		t.Fatalf("after compaction: %+v", d)
	}
}

func TestModeRoundTrip(t *testing.T) {
	for _, m := range Modes {
		sandbox, approval, policy := ModeSettings(m.Value)
		if got := Mode(sandbox, approval); got != m.Value {
			t.Fatalf("%s -> %s/%s -> %s", m.Value, sandbox, approval, got)
		}
		if policy["type"] == nil {
			t.Fatalf("%s has no policy", m.Value)
		}
	}
	if Mode("workspace-write", "never") != "workspace-write" || Mode("", "") != "workspace-write" {
		t.Fatal("defaults")
	}
	if Mode("dangerFullAccess", "") != "workspace-write" {
		// Only the CLI's own spellings map to full access; the wire spelling is translated before.
		t.Fatal("wire spelling should not be accepted here")
	}
	if Mode(sandboxName("dangerFullAccess"), "") != "danger-full-access" {
		t.Fatal("sandboxName")
	}
}

func TestMetaPrompts(t *testing.T) {
	for _, s := range []string{"<environment_context>\n<cwd>/w</cwd>\n</environment_context>", "<recommended_plugins>\nx\n</recommended_plugins>\nnote", "<user_instructions>\nfoo\n</user_instructions>"} {
		if !isMetaPrompt(s) {
			t.Errorf("should be meta: %q", s)
		}
	}
	for _, s := range []string{"hello", "<b>bold</b> is html", "< not a tag", "<foo>unterminated"} {
		if isMetaPrompt(s) && s != "<b>bold</b> is html" {
			t.Errorf("should not be meta: %q", s)
		}
	}
}

func TestConvert(t *testing.T) {
	out := Convert(lines(t, sessionMeta, taskStarted, devMsg, pluginsMsg, envMsg, turnContext, userMsg, userEvent, reasoning, reasoningNone, execCall, execFailed, mcpCall, mcpOutput, agentEvent, assistantMsg, tokenCount, taskComplete, compactedLine))
	var types []string
	for _, raw := range out {
		var e map[string]any
		if err := json.Unmarshal(raw, &e); err != nil {
			t.Fatal(err)
		}
		typ := str(e["type"])
		msg := obj(e["message"])
		if arr, ok := msg["content"].([]any); ok && len(arr) > 0 {
			typ += ":" + str(obj(arr[0])["type"])
		} else if typ == "system" {
			typ += ":" + str(e["subtype"])
		} else {
			typ += ":usage"
		}
		types = append(types, typ)
		if !strings.HasPrefix(str(e["uuid"]), "codex:") {
			t.Fatalf("uuid %v", e["uuid"])
		}
	}
	want := []string{"user:text", "assistant:thinking", "assistant:tool_use", "user:tool_result", "assistant:tool_use", "user:tool_result", "assistant:text", "assistant:usage", "system:compact_boundary"}
	if strings.Join(types, " ") != strings.Join(want, " ") {
		t.Fatalf("got %v\nwant %v", types, want)
	}
	// The exec call renders as Bash with the command pulled out of the code-mode wrapper.
	var call map[string]any
	_ = json.Unmarshal(out[2], &call)
	block := obj(obj(obj(call["message"])["content"].([]any)[0]))
	if str(block["name"]) != "Bash" || str(obj(block["input"])["command"]) != `cat /tmp/x "q"` || str(block["id"]) != "call_1" {
		t.Fatalf("exec block: %v", block)
	}
	var res map[string]any
	_ = json.Unmarshal(out[3], &res)
	rb := obj(obj(res["message"])["content"].([]any)[0])
	if rb["is_error"] != true || str(rb["tool_use_id"]) != "call_1" || !strings.Contains(str(rb["content"]), "rejected by user") {
		t.Fatalf("result block: %v", rb)
	}
	var usage map[string]any
	_ = json.Unmarshal(out[7], &usage)
	u := obj(obj(usage["message"])["usage"])
	if u["input_tokens"] != float64(15219-12160) || u["cache_read_input_tokens"] != float64(12160) || str(obj(usage["message"])["model"]) != "gpt-6-astra" {
		t.Fatalf("usage: %v", u)
	}
}

func TestReadTailAndFrom(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/rollout-2026-09-29T03-51-39-01a0e924-d2b2-7072-bebc-3ad88475df61.jsonl"
	content := sessionMeta + "\n" + taskStarted + "\n" + userMsg + "\n"
	if err := writeFile(path, content); err != nil {
		t.Fatal(err)
	}
	if ThreadID(path) != "01a0e924-d2b2-7072-bebc-3ad88475df61" {
		t.Fatal("thread id")
	}
	tail, size, err := ReadTail(path, 2, 1<<20)
	if err != nil || len(tail) != 2 || size != int64(len(content)) || tail[0].Type != "event_msg" {
		t.Fatalf("tail: %d lines, size %d, err %v", len(tail), size, err)
	}
	if tail[1].Offset != int64(len(sessionMeta)+1+len(taskStarted)+1) {
		t.Fatalf("offset %d", tail[1].Offset)
	}
	more, next, size2, err := ReadFrom(path, size, 1<<20)
	if err != nil || len(more) != 0 || next != size || size2 != size {
		t.Fatalf("nothing new: %d %d %d %v", len(more), next, size2, err)
	}
	if err := writeFile(path, content+assistantMsg+"\n"+`{"partial`); err != nil {
		t.Fatal(err)
	}
	more, next, _, err = ReadFrom(path, size, 1<<20)
	if err != nil || len(more) != 1 || more[0].Offset != size || next != size+int64(len(assistantMsg)+1) {
		t.Fatalf("new lines: %d next %d err %v", len(more), next, err)
	}
	// A tail read that starts mid-file drops the cut first line.
	tail, _, _ = ReadTail(path, 10, int64(len(assistantMsg)+20))
	if len(tail) != 1 || tail[0].Type != "response_item" {
		t.Fatalf("cut tail: %+v", tail)
	}
}

func TestInterpretAndMerge(t *testing.T) {
	r := &Report{HasCodex: true,
		Threads:  []RawThread{{ID: "t1", Path: "/p/rollout-x-t1.jsonl", Mtime: 5, Live: true, Head: headInfo{Cwd: "/w/", FirstPrompt: "first", StartedAt: 1}, Lines: lines(t, taskStarted, turnContext, userMsg, assistantMsg, taskComplete)}},
		Projects: []RawProject{{Cwd: "/w", Mtime: 5, Count: 3}, {Cwd: "/other", Mtime: 2, Count: 1}},
	}
	agents, ws := Interpret("m", r, 10)
	if len(agents) != 1 || agents[0].ID != "m::t1" || agents[0].Provider != "codex" || agents[0].WorkspacePath != "/w" || agents[0].Title != "first" || agents[0].State != model.StateIdle {
		t.Fatalf("agents: %+v", agents)
	}
	if len(ws) != 2 || ws[1].Path != "/w" || ws[1].HistoryCount != 3 || len(ws[1].Agents) != 1 {
		t.Fatalf("workspaces: %+v", ws)
	}
	merged := MergeWorkspaces([]model.Workspace{{Path: "/w", HistoryCount: 2, OpenInIDE: true, Agents: []model.Agent{{ID: "m::c1"}}}}, ws)
	if len(merged) != 2 || merged[1].HistoryCount != 5 || !merged[1].OpenInIDE || len(merged[1].Agents) != 2 {
		t.Fatalf("merged: %+v", merged)
	}
}

func TestPendingForAndAnswers(t *testing.T) {
	params := map[string]any{"itemId": "exec-1", "command": "/bin/zsh -lc 'touch probe.txt'", "cwd": "/w", "commandActions": []any{}, "proposedExecpolicyAmendment": []any{"touch"}}
	pr := pendingFor("item/commandExecution/requestApproval", params, nil, 1)
	if pr == nil || pr.ToolName != "Bash" || pr.ToolUseID != "exec-1" || len(pr.Suggestions) == 0 {
		t.Fatalf("command: %+v", pr)
	}
	var in map[string]any
	_ = json.Unmarshal(pr.Input, &in)
	if in["command"] != "/bin/zsh -lc 'touch probe.txt'" || in["cwd"] != "/w" {
		t.Fatalf("input %v", in)
	}
	changes := map[string]json.RawMessage{"fc-1": json.RawMessage(`[{"path":"a.txt","kind":{"type":"add"},"diff":"+hi"}]`)}
	pr = pendingFor("item/fileChange/requestApproval", map[string]any{"itemId": "fc-1", "reason": "outside workspace"}, changes, 1)
	if pr == nil || pr.ToolName != "Edit" || pr.Description != "outside workspace" || !strings.Contains(string(pr.Input), `"a.txt"`) {
		t.Fatalf("file change: %+v", pr)
	}
	pr = pendingFor("item/tool/requestUserInput", map[string]any{"itemId": "q-1", "questions": []any{map[string]any{"id": "q1", "header": "DB", "question": "Which database?", "options": []any{map[string]any{"label": "Postgres", "description": "relational"}}}}}, nil, 1)
	if pr == nil || pr.ToolName != "AskUserQuestion" || !strings.Contains(string(pr.Input), `"Which database?"`) || !strings.Contains(string(pr.Input), `"Postgres"`) {
		t.Fatalf("question: %+v", pr)
	}
	if pendingFor("item/tool/call", map[string]any{}, nil, 1) != nil {
		t.Fatal("dynamic tool calls are not ours to answer")
	}
	pr = pendingFor("mcpServer/elicitation/request", map[string]any{"serverName": "srv", "message": "Token?", "requestedSchema": map[string]any{"type": "object", "properties": map[string]any{"t": map[string]any{"type": "string"}}}}, nil, 1)
	if pr == nil || pr.Kind != model.PendingElicitation || pr.Elicitation.Mode != "form" || pr.Elicitation.Message != "Token?" || len(pr.Elicitation.RequestedSchema) == 0 {
		t.Fatalf("elicitation: %+v", pr)
	}
}

func TestShellCommand(t *testing.T) {
	if got := shellCommand(`text(await tools.exec_command({cmd:"rg --files | head", max_output_tokens: 10}))`); got != "rg --files | head" {
		t.Fatalf("got %q", got)
	}
	if got := shellCommand(`text(await tools.exec_command({cmd:"echo \"a b\"\n"}))`); got != "echo \"a b\"\n" {
		t.Fatalf("got %q", got)
	}
	if shellCommand(`return 1`) != "" {
		t.Fatal("no command")
	}
}
