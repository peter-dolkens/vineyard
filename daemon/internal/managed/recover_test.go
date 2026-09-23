package managed

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/peter-dolkens/vineyard/daemon/internal/model"
)

// A question carried over a takeover has no control_request behind it. The webview answers it like
// any other (answers keyed by question text); the daemon turns that into a prompt worded like the
// tool_result Claude Code writes for an answered AskUserQuestion, questions in the order asked.
func TestRecoveredAnswerText(t *testing.T) {
	input := json.RawMessage(`{"questions":[{"question":"Which colour?","header":"Colour","options":[{"label":"Red"},{"label":"Blue"}]},{"question":"Which size?","header":"Size"}]}`)
	resp := json.RawMessage(`{"behavior":"allow","updatedInput":{"questions":[],"answers":{"Which size?":"Large","Which colour?":"Blue"}}}`)
	got := recoveredAnswerText(input, resp)
	want := `Your questions have been answered: "Which colour?"="Blue", "Which size?"="Large". You can now continue with these answers in mind.`
	if !strings.HasSuffix(got, want) || !strings.HasPrefix(got, "[Vineyard resumed this session") {
		t.Fatalf("text = %q", got)
	}
	if recoveredAnswerText(input, json.RawMessage(`{"behavior":"deny","message":"no"}`)) != "" {
		t.Fatal("a deny should send nothing")
	}
	if recoveredAnswerText(input, json.RawMessage(`{"behavior":"allow","updatedInput":{"answers":{}}}`)) != "" {
		t.Fatal("no answers should send nothing")
	}
}

// Spawning with Recover shows the question as a pending request of kind recovered, so the chat
// renders the usual question card and the tree says "question".
func TestRecoveredPendingShape(t *testing.T) {
	r := &RecoveredQuestion{ToolUseID: "toolu_q", Input: json.RawMessage(`{"questions":[{"question":"Which?"}]}`)}
	p := recoveredPending(r, 42)
	if p.Kind != model.PendingRecovered || p.RequestID != "recovered:toolu_q" || p.ToolName != "AskUserQuestion" || p.At != 42 || !p.RequiresUserInteraction {
		t.Fatalf("pending = %+v", p)
	}
	if questionSummary(p.Input) != "Which?" {
		t.Fatalf("summary = %q", questionSummary(p.Input))
	}
}

// Once a recovered question is answered, the transcript still shows its tool_use as pending until
// Claude Code appends the prompt; Merge hides it so the tree does not flip back to "question".
func TestMergeHidesAnsweredRecoveredQuestion(t *testing.T) {
	m := New(nil, nil)
	p := &proc{done: make(chan struct{}), ctl: map[string]chan ctlReply{}}
	p.info.SessionID = "s1"
	p.info.RecoveredDone = "toolu_q"
	m.procs["s1"] = p
	agents := []model.Agent{{SessionID: "s1", State: model.StateQuestion, StateDetail: "Which?", PendingTools: []model.PendingTool{{ID: "toolu_q", Name: "AskUserQuestion", Summary: "Which?"}}}}
	out := m.Merge("m", agents)
	if len(out) != 1 || len(out[0].PendingTools) != 0 || out[0].State != model.StateWorking {
		t.Fatalf("merged = %+v", out[0])
	}
}
