package managed

import (
	"strings"
	"testing"
)

// The task events Claude Code 2.1.301 emits around a foreground Bash call that is moved to the
// background, as captured by probe: the call becomes backgroundable at task_started and stops being
// so when it is backgrounded or ends.
func TestForegroundTasksFollowTaskEvents(t *testing.T) {
	m := New(nil, nil)
	p := &proc{ctl: map[string]chan ctlReply{}}
	feed := func(lines ...string) {
		m.readStdout(p, strings.NewReader(strings.Join(lines, "\n")+"\n"))
	}
	feed(`{"type":"system","subtype":"task_started","task_id":"be836cno1","tool_use_id":"toolu_1","is_backgrounded":false,"task_type":"local_bash","description":"Ping localhost"}`,
		`{"type":"system","subtype":"task_started","task_id":"bg2","tool_use_id":"toolu_2","is_backgrounded":true,"task_type":"local_bash"}`)
	if len(p.info.Foreground) != 1 || p.info.Foreground[0].ToolUseID != "toolu_1" || p.info.Foreground[0].TaskID != "be836cno1" {
		t.Fatalf("after task_started: %+v", p.info.Foreground)
	}
	feed(`{"type":"system","subtype":"task_updated","task_id":"be836cno1","patch":{"is_backgrounded":true}}`)
	if len(p.info.Foreground) != 0 {
		t.Fatalf("a backgrounded task is still offered: %+v", p.info.Foreground)
	}
	feed(`{"type":"system","subtype":"task_started","task_id":"t3","tool_use_id":"toolu_3","is_backgrounded":false,"task_type":"local_agent"}`,
		`{"type":"system","subtype":"task_updated","task_id":"t3","patch":{"status":"completed"}}`)
	if len(p.info.Foreground) != 0 {
		t.Fatalf("a finished task is still offered: %+v", p.info.Foreground)
	}
	feed(`{"type":"system","subtype":"task_started","task_id":"t4","tool_use_id":"toolu_4","is_backgrounded":false}`, `{"type":"result","total_cost_usd":0.01}`)
	if len(p.info.Foreground) != 0 {
		t.Fatalf("the end of the turn should clear it: %+v", p.info.Foreground)
	}
}
