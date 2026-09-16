package claude

import (
	"encoding/json"
	"testing"

	"github.com/peter-dolkens/vineyard/daemon/internal/model"
)

func parse(t *testing.T, lines ...string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, l := range lines {
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("bad fixture line %q: %v", l, err)
		}
		out = append(out, m)
	}
	return out
}

const (
	userPrompt   = `{"type":"user","timestamp":"2026-09-16T14:32:05.796Z","permissionMode":"auto","cwd":"/w","sessionId":"s1","version":"2.1.273","gitBranch":"main","message":{"role":"user","content":[{"type":"text","text":"do the thing"}]}}`
	thinking     = `{"type":"assistant","timestamp":"2026-09-16T14:32:15.745Z","effort":"high","message":{"model":"claude-fable-5-1","role":"assistant","content":[{"type":"thinking","thinking":"hmm"}],"stop_reason":"tool_use","usage":{"input_tokens":2,"cache_read_input_tokens":28336,"cache_creation_input_tokens":21658,"output_tokens":716}}}`
	toolUse      = `{"type":"assistant","timestamp":"2026-09-16T14:32:16.000Z","effort":"high","message":{"model":"claude-fable-5-1","role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"command":"ls -la","description":"List files"}}],"stop_reason":"tool_use"}}`
	toolResult   = `{"type":"user","timestamp":"2026-09-16T14:32:17.000Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"ok"}]}}`
	finalText    = `{"type":"assistant","timestamp":"2026-09-16T14:32:20.000Z","effort":"high","message":{"model":"claude-fable-5-1","role":"assistant","content":[{"type":"text","text":"# Done\nAll good."}],"stop_reason":"end_turn"}}`
	question     = `{"type":"assistant","timestamp":"2026-09-16T14:32:21.000Z","message":{"model":"claude-fable-5-1","role":"assistant","content":[{"type":"tool_use","id":"toolu_q","name":"AskUserQuestion","input":{"questions":[{"question":"Which database?","header":"DB"}]}}],"stop_reason":"tool_use"}}`
	title        = `{"type":"ai-title","aiTitle":"Do the thing","sessionId":"s1"}`
	lastPrompt   = `{"type":"last-prompt","lastPrompt":"do the thing","sessionId":"s1"}`
	sidechainUse = `{"type":"assistant","isSidechain":true,"timestamp":"2026-09-16T14:32:22.000Z","message":{"model":"claude-sonnet-5","role":"assistant","content":[{"type":"tool_use","id":"toolu_side","name":"Read","input":{"file_path":"/x"}}],"stop_reason":"tool_use"}}`
)

func TestDeriveStates(t *testing.T) {
	cases := []struct {
		name   string
		lines  []string
		state  model.AgentState
		detail string
	}{
		{"prompt just sent", []string{userPrompt}, model.StateWorking, "Responding to prompt…"},
		{"thinking", []string{userPrompt, thinking}, model.StateThinking, "Reasoning…"},
		{"tool pending", []string{userPrompt, thinking, toolUse}, model.StateTool, "Bash: List files"},
		{"tool result", []string{userPrompt, thinking, toolUse, toolResult}, model.StateWorking, "Processing tool result…"},
		{"finished", []string{userPrompt, thinking, toolUse, toolResult, finalText, title, lastPrompt}, model.StateIdle, "Done"},
		{"question", []string{userPrompt, toolUse, toolResult, question}, model.StateQuestion, "Which database?"},
		{"sidechain ignored", []string{userPrompt, toolUse, toolResult, finalText, sidechainUse}, model.StateIdle, "Done"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := DeriveFromTranscript(parse(t, c.lines...))
			if d.State != c.state {
				t.Fatalf("state = %s, want %s (detail %q)", d.State, c.state, d.StateDetail)
			}
			if d.StateDetail != c.detail {
				t.Fatalf("detail = %q, want %q", d.StateDetail, c.detail)
			}
		})
	}
}

func TestDeriveMetadata(t *testing.T) {
	d := DeriveFromTranscript(parse(t, userPrompt, thinking, toolUse, toolResult, finalText, title, lastPrompt))
	if d.Model != "claude-fable-5-1" || d.Effort != "high" || d.Title != "Do the thing" || d.LastPrompt != "do the thing" {
		t.Fatalf("metadata = %+v", d)
	}
	if d.ContextTokens != 2+28336+21658 {
		t.Fatalf("context tokens = %d", d.ContextTokens)
	}
	if d.GitBranch != "main" || d.PermissionMode != "auto" || d.Version != "2.1.273" {
		t.Fatalf("metadata = %+v", d)
	}
}

func TestBuildAgentRegistryInteraction(t *testing.T) {
	reg := map[string]any{"sessionId": "s1", "cwd": "/w", "name": "w-1a", "status": "waiting", "statusUpdatedAt": float64(1789569125748)}
	tr := &RawTranscript{SessionID: "s1", Path: "/p", Entries: parse(t, userPrompt, thinking, toolUse)}
	a := BuildAgent("m", RawSession{PID: 1, Alive: true, Registry: reg}, tr, 1789569200000)
	if a.State != model.StatePermission || a.StateDetail != "Permission: Bash: List files" {
		t.Fatalf("waiting+pending tool → %s %q", a.State, a.StateDetail)
	}
	reg["status"] = "shell"
	a = BuildAgent("m", RawSession{PID: 1, Alive: true, Registry: reg}, tr, 1789569200000)
	if a.State != model.StateShell {
		t.Fatalf("shell → %s", a.State)
	}
	a = BuildAgent("m", RawSession{PID: 1, Alive: false, Registry: reg}, tr, 1789569200000)
	if a.State != model.StateExited {
		t.Fatalf("dead pid → %s", a.State)
	}
	if a.ID != "m::s1" || a.WorkspacePath != "/w" {
		t.Fatalf("identity = %+v", a)
	}
}

func TestEncodeProjectDir(t *testing.T) {
	if got := EncodeProjectDir("/Users/peter.dolkens/Projects/down-under"); got != "-Users-peter-dolkens-Projects-down-under" {
		t.Fatalf("got %q", got)
	}
}
