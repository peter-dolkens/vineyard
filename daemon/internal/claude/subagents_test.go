package claude

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/peter-dolkens/vineyard/daemon/internal/model"
)

const (
	subPrompt   = `{"type":"user","isSidechain":true,"agentId":"a1","timestamp":"2026-09-16T14:40:00.000Z","message":{"role":"user","content":"Find all callers of foo"}}`
	subToolUse  = `{"type":"assistant","isSidechain":true,"agentId":"a1","timestamp":"2026-09-16T14:40:05.000Z","message":{"model":"claude-sonnet-5","role":"assistant","content":[{"type":"tool_use","id":"toolu_s1","name":"Grep","input":{"pattern":"foo("}}],"stop_reason":"tool_use","usage":{"input_tokens":10,"cache_read_input_tokens":1000}}}`
	subResult   = `{"type":"user","isSidechain":true,"agentId":"a1","timestamp":"2026-09-16T14:40:06.000Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_s1","content":"a.go:1"}]}}`
	subDone     = `{"type":"assistant","isSidechain":true,"agentId":"a1","timestamp":"2026-09-16T14:40:09.000Z","message":{"model":"claude-sonnet-5","role":"assistant","content":[{"type":"text","text":"Two callers: a.go and b.go."}],"stop_reason":"end_turn"}}`
	parentSpawn = `{"type":"assistant","timestamp":"2026-09-16T14:39:59.000Z","message":{"model":"claude-fable-5-1","role":"assistant","content":[{"type":"tool_use","id":"toolu_agent1","name":"Agent","input":{"description":"Find foo callers","prompt":"..."}}],"stop_reason":"tool_use"}}`
	parentGot   = `{"type":"user","timestamp":"2026-09-16T14:40:10.000Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_agent1","content":"Two callers"}]}}`
	parentNotif = `{"type":"queue-operation","operation":"enqueue","timestamp":"2026-09-16T14:40:11.000Z","content":"<task-notification>\n<task-id>x</task-id>\n<tool-use-id>toolu_agent1</tool-use-id>\n<status>completed</status>\n</task-notification>"}`
)

const testNow = int64(1789569700000) // 2026-09-16T14:41:40Z

func sub(t *testing.T, id, parent, shape string, spawned int64, lines ...string) RawSubagent {
	return RawSubagent{
		AgentID: id, Path: "/p/" + id + ".jsonl", Mtime: spawned, Size: 1, SpawnedAt: spawned,
		Meta:    SubMeta{AgentType: "Explore", Description: "Find foo callers", ToolUseID: "toolu_agent1", ParentAgentID: parent, SpawnDepth: 1, RequestShape: shape},
		Entries: parse(t, lines...),
	}
}

func TestBuildSubagentsStates(t *testing.T) {
	recent := testNow - 60_000
	cases := []struct {
		name   string
		parent []string
		sub    RawSubagent
		alive  bool
		state  model.AgentState
		detail string
	}{
		{"finished", nil, sub(t, "a1", "", "foreground", recent, subPrompt, subToolUse, subResult, subDone), true, model.StateDone, "Two callers: a.go and b.go."},
		{"running tool", nil, sub(t, "a1", "", "foreground", recent, subPrompt, subToolUse), true, model.StateTool, "Grep: foo("},
		{"foreground returned", []string{parentSpawn, parentGot}, sub(t, "a1", "", "foreground", recent, subPrompt, subToolUse), true, model.StateDone, "Returned to parent"},
		{"background launch result is not completion", []string{parentSpawn, parentGot}, sub(t, "a1", "", "background", recent, subPrompt, subToolUse), true, model.StateTool, "Grep: foo("},
		{"background notified", []string{parentSpawn, parentGot, parentNotif}, sub(t, "a1", "", "background", recent, subPrompt, subToolUse), true, model.StateDone, "Returned to parent"},
		{"dead parent", nil, sub(t, "a1", "", "foreground", recent, subPrompt, subToolUse), false, model.StateExited, "Session process has exited"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tr := &RawTranscript{Path: "/p.jsonl", Entries: parse(t, c.parent...), Subagents: []RawSubagent{c.sub}}
			// Real tails are newer than the fixture timestamps; pin activity to "recent" via Mtime.
			out := BuildSubagents(tr, c.alive, testNow)
			if len(out) != 1 {
				t.Fatalf("got %d subagents", len(out))
			}
			s := out[0]
			if s.State != c.state || s.StateDetail != c.detail {
				t.Fatalf("state = %s %q, want %s %q", s.State, s.StateDetail, c.state, c.detail)
			}
			if s.Type != "Explore" || s.Description != "Find foo callers" || s.Model != "claude-sonnet-5" || s.ToolUseID != "toolu_agent1" {
				t.Fatalf("metadata = %+v", s)
			}
			if s.Background != (c.sub.Meta.RequestShape == "background") {
				t.Fatalf("background = %v", s.Background)
			}
			if model.IsBusy(s.State) && len(s.PendingTools) != 1 {
				t.Fatalf("pending tools = %+v", s.PendingTools)
			}
			if !model.IsBusy(s.State) && len(s.PendingTools) != 0 {
				t.Fatalf("finished subagent still carries pending tools: %+v", s.PendingTools)
			}
		})
	}
}

func TestBuildSubagentsStale(t *testing.T) {
	// Activity is the newest of the last line's timestamp and the file mtime; the clock must be past
	// both by the stale window.
	tr := &RawTranscript{Subagents: []RawSubagent{sub(t, "a1", "", "background", 1, subPrompt, subToolUse)}}
	last := entryTime(parse(t, subToolUse)[0])
	out := BuildSubagents(tr, true, last+staleSubagentMs+60_000)
	if out[0].State != model.StateUnknown || out[0].StateDetail != "No activity for 16m" {
		t.Fatalf("stale = %s %q", out[0].State, out[0].StateDetail)
	}
	out = BuildSubagents(tr, true, last+staleSubagentMs-60_000)
	if out[0].State != model.StateTool {
		t.Fatalf("not yet stale = %s", out[0].State)
	}
}

func TestBuildSubagentsTreeAndOrder(t *testing.T) {
	tr := &RawTranscript{Subagents: []RawSubagent{
		sub(t, "child", "root", "background", 300, subPrompt, subDone),
		sub(t, "root", "", "background", 100, subPrompt, subDone),
		sub(t, "orphan", "gone", "background", 200, subPrompt, subDone),
	}}
	out := BuildSubagents(tr, true, testNow)
	if len(out) != 3 || out[0].AgentID != "root" || out[1].AgentID != "orphan" || out[2].AgentID != "child" {
		t.Fatalf("order = %v", ids(out))
	}
	if out[2].ParentAgentID != "root" {
		t.Fatalf("child parent = %q", out[2].ParentAgentID)
	}
	if out[1].ParentAgentID != "" {
		t.Fatalf("orphan should be re-rooted, got parent %q", out[1].ParentAgentID)
	}
}

func TestBuildSubagentsCap(t *testing.T) {
	var subs []RawSubagent
	for i := 0; i < maxSubagents+5; i++ {
		subs = append(subs, sub(t, "f"+itoa(i), "", "background", int64(i), subPrompt, subDone))
	}
	subs = append(subs, sub(t, "live", "", "background", testNow-1000, subPrompt, subToolUse))
	out := BuildSubagents(&RawTranscript{Subagents: subs}, true, testNow)
	if len(out) != maxSubagents {
		t.Fatalf("len = %d", len(out))
	}
	// 66 candidates, 60 kept: the six oldest finished ones (f0–f5) go; the running one never does.
	if out[0].AgentID != "f6" || out[len(out)-1].AgentID != "live" {
		t.Fatalf("oldest finished should go first, running never: %v", ids(out))
	}
}

func ids(s []model.Subagent) []string {
	out := make([]string, len(s))
	for i, x := range s {
		out[i] = x.AgentID
	}
	return out
}

func TestCollectSubagentsFromDisk(t *testing.T) {
	root := t.TempDir()
	sid := "sess1"
	tpath := filepath.Join(root, sid+".jsonl")
	dir := SubagentsDir(tpath)
	if dir != filepath.Join(root, sid, "subagents") {
		t.Fatalf("SubagentsDir = %q", dir)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("agent-abc.jsonl", subPrompt+"\n"+subToolUse+"\n")
	write("agent-abc.meta.json", `{"agentType":"Explore","description":"Find foo callers","toolUseId":"toolu_agent1","spawnDepth":1,"requestShape":"background","model":"sonnet"}`)
	write("agent-empty.jsonl", "")
	write("notes.txt", "ignored")

	c := NewCollector(root, 80)
	tr := &RawTranscript{SessionID: sid, Path: tpath}
	c.collectSubagents(tr)
	if len(tr.Subagents) != 1 {
		t.Fatalf("got %d subagents: %+v", len(tr.Subagents), tr.Subagents)
	}
	s := tr.Subagents[0]
	if s.AgentID != "abc" || s.Meta.AgentType != "Explore" || s.Meta.ToolUseID != "toolu_agent1" || len(s.Entries) != 2 {
		t.Fatalf("raw = %+v", s)
	}
	if s.SpawnedAt != entryTime(parse(t, subPrompt)[0]) {
		t.Fatalf("spawnedAt = %d", s.SpawnedAt)
	}

	// Unchanged file: served from cache (entries identical, no re-read needed).
	tr2 := &RawTranscript{SessionID: sid, Path: tpath}
	c.collectSubagents(tr2)
	if len(tr2.Subagents) != 1 || len(tr2.Subagents[0].Entries) != 2 {
		t.Fatalf("cached = %+v", tr2.Subagents)
	}
	// Cache entries survive a prune while seen, and go once the file is not.
	c.pruneSubCache()
	if _, ok := c.subCache[s.Path]; !ok {
		t.Fatal("cache dropped a transcript that was seen this poll")
	}
	c.pruneSubCache()
	if _, ok := c.subCache[s.Path]; ok {
		t.Fatal("cache kept a transcript not seen this poll")
	}

	// The full agent carries them, and a dead parent marks them exited.
	reg := map[string]any{"sessionId": sid, "cwd": "/w", "status": "busy"}
	a := BuildAgent("m", RawSession{PID: 1, Alive: true, Registry: reg}, tr, testNow)
	if len(a.Subagents) != 1 || a.Subagents[0].State != model.StateTool || a.Subagents[0].TranscriptPath != s.Path {
		t.Fatalf("agent subagents = %+v", a.Subagents)
	}
	a = BuildAgent("m", RawSession{PID: 1, Alive: false, Registry: reg}, tr, testNow)
	if a.Subagents[0].State != model.StateExited {
		t.Fatalf("dead parent → %s", a.Subagents[0].State)
	}
}

func TestParentResolutionsIgnoresSidechain(t *testing.T) {
	results, notified := parentResolutions(parse(t, parentSpawn, parentGot, parentNotif, subResult))
	if !results["toolu_agent1"] || results["toolu_s1"] {
		t.Fatalf("results = %v", results)
	}
	if notified["toolu_agent1"] != "completed" {
		t.Fatalf("notified = %v", notified)
	}
}
