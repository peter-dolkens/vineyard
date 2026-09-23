package managed

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"testing"
)

// StopTask sends stop_task with task_id over the control channel and hands back Claude Code's own
// refusal text, so a wrong id or a task that already finished shows up in the UI instead of doing
// nothing. A fake Claude Code on the other end of two pipes refuses the first request and accepts
// the second.
func TestStopTaskRelaysClaudeCodesAnswer(t *testing.T) {
	m := New(nil, nil)
	stdinR, stdinW := io.Pipe()   // the daemon's control_request frames
	stdoutR, stdoutW := io.Pipe() // the fake CLI's control_response frames
	t.Cleanup(func() { stdinW.Close(); stdoutW.Close() })
	p := &proc{stdin: stdinW, done: make(chan struct{}), ctl: map[string]chan ctlReply{}}
	p.info.SessionID = "s1"
	m.procs["s1"] = p
	go m.readStdout(p, stdoutR)

	type request struct {
		RequestID string `json:"request_id"`
		Request   struct {
			Subtype string `json:"subtype"`
			TaskID  string `json:"task_id"`
		} `json:"request"`
	}
	seen := make(chan request, 2)
	go func() {
		sc := bufio.NewScanner(stdinR)
		for i := 0; sc.Scan() && i < 2; i++ {
			var r request
			if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
				t.Errorf("unmarshal %s: %v", sc.Bytes(), err)
				return
			}
			seen <- r
			var resp string
			if i == 0 {
				resp = fmt.Sprintf(`{"subtype":"error","request_id":%q,"error":"StopTask: Task %s is not running (status: completed)"}`, r.RequestID, r.Request.TaskID)
			} else {
				resp = fmt.Sprintf(`{"subtype":"success","request_id":%q,"response":{"taskId":%q}}`, r.RequestID, r.Request.TaskID)
			}
			fmt.Fprintf(stdoutW, `{"type":"control_response","response":%s}%s`, resp, "\n")
		}
	}()

	err := m.StopTask("s1", "t1")
	if err == nil || err.Error() != "StopTask: Task t1 is not running (status: completed)" {
		t.Fatalf("refused stop: got %v", err)
	}
	r := <-seen
	if r.Request.Subtype != "stop_task" || r.Request.TaskID != "t1" || r.RequestID == "" {
		t.Fatalf("sent %+v, want subtype stop_task with task_id t1", r)
	}
	if err := m.StopTask("s1", "agent-2"); err != nil {
		t.Fatalf("accepted stop: %v", err)
	}
	if r := <-seen; r.Request.TaskID != "agent-2" {
		t.Fatalf("second request carried task_id %q", r.Request.TaskID)
	}
	if err := m.StopTask("s1", " "); err == nil {
		t.Fatal("an empty task id must be refused before anything is sent")
	}
	if err := m.StopTask("nope", "t1"); err == nil {
		t.Fatal("an unmanaged session must be refused")
	}
	if len(p.ctl) != 0 {
		t.Fatalf("%d reply channels left behind", len(p.ctl))
	}
}
