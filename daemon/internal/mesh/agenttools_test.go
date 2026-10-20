package mesh

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/peter-dolkens/vineyard/daemon/internal/model"
	"github.com/peter-dolkens/vineyard/daemon/internal/protocol"
)

func TestAgentToolsOpKeepsTheLaterChoice(t *testing.T) {
	n := newTestNode(t, "m1")
	set := func(at int64, tools ...string) model.AgentToolsStatus {
		t.Helper()
		args, _ := json.Marshal(protocol.AgentToolsArgs{Tools: tools, At: at})
		raw, err := n.handleLocal(protocol.Request{Op: "agenttools", Args: args})
		if err != nil {
			t.Fatal(err)
		}
		var st model.AgentToolsStatus
		_ = json.Unmarshal(raw, &st)
		return st
	}
	st := set(10, model.AgentToolDrive, "bogus", model.AgentToolMachines, model.AgentToolDrive)
	if strings.Join(st.Tools, ",") != "drive,machines" || st.At != 10 {
		t.Fatalf("status = %+v", st)
	}
	if st := set(5); len(st.Tools) != 2 || st.At != 10 {
		t.Fatalf("an older choice applied: %+v", st)
	}
	if st := set(20); len(st.Tools) != 0 || st.At != 20 {
		t.Fatalf("turning everything off: %+v", st)
	}
}

func TestAgentOriginNeedsDriveAndBoundsHops(t *testing.T) {
	n := newTestNode(t, "m1")
	send := func(hops int) error {
		args, _ := json.Marshal(protocol.SendArgs{SessionID: "nope", Text: "hi", Origin: &protocol.AgentOrigin{Machine: "studio", Session: "s0", Hops: hops}})
		_, err := n.handleLocal(protocol.Request{Op: "send", Args: args})
		return err
	}
	if err := send(1); err == nil || !strings.Contains(err.Error(), "does not let agents drive") {
		t.Fatalf("drive off: got %v", err)
	}
	n.cfg.AgentTools = []string{model.AgentToolDrive}
	if err := send(MaxAgentHops + 1); err == nil || !strings.Contains(err.Error(), "hop") {
		t.Fatalf("too many hops: got %v", err)
	}
	// Within bounds it gets as far as delivery (no such session here).
	if err := send(MaxAgentHops); err == nil || strings.Contains(err.Error(), "hop") || strings.Contains(err.Error(), "drive") {
		t.Fatalf("allowed send: got %v", err)
	}
	spawn, _ := json.Marshal(map[string]any{"cwd": t.TempDir(), "resume": "x", "origin": protocol.AgentOrigin{Machine: "studio", Hops: 1}})
	if _, err := n.handleLocal(protocol.Request{Op: "spawn", Args: spawn}); err == nil {
		t.Fatal("spawn without managed sessions should fail")
	}
}

func TestWrapAgentMessage(t *testing.T) {
	got := WrapAgentMessage(&protocol.AgentOrigin{Machine: `bad"<name>`, Session: "s1", Hops: 2}, "please run the tests")
	for _, want := range []string{`<vineyard-agent-message from-machine="bad'(name)" from-session="s1" hops="2">`, "please run the tests", "not from your user"} {
		if !strings.Contains(got, want) {
			t.Errorf("wrapped message lacks %q:\n%s", want, got)
		}
	}
}

func TestAgentHopsOp(t *testing.T) {
	n := newTestNode(t, "m1")
	n.noteAgentHops("s1", 2)
	raw, err := n.handleLocal(protocol.Request{Op: "agenthops", Args: json.RawMessage(`{"sessionId":"s1"}`)})
	if err != nil || string(raw) != `{"hops":2}` {
		t.Fatalf("agenthops = %s, %v", raw, err)
	}
}

func TestHasCapRefusesNonNames(t *testing.T) {
	n := newTestNode(t, "m1")
	if _, err := n.handleLocal(protocol.Request{Op: "hascap", Args: json.RawMessage(`{"names":["$(id)"]}`)}); err == nil {
		t.Fatal("a shell expression was accepted as a program name")
	}
	raw, err := n.handleLocal(protocol.Request{Op: "hascap", Args: json.RawMessage(`{"names":["definitely-not-a-program-xyz"]}`)})
	if err != nil || string(raw) != `{"found":{}}` {
		t.Fatalf("hascap = %s, %v", raw, err)
	}
}
