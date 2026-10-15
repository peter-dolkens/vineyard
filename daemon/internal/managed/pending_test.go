package managed

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"testing"
	"time"
)

// Two subagents asking to run a command at the same time are two can_use_tool requests with no
// answer between them (Claude Code 2.1.273 sends them that way; the terminal UI queues them). Both
// must survive: the one the user does not see first is still the one an agent is blocked on, and
// nothing ever asks again.
func TestTwoPendingRequestsAreBothKept(t *testing.T) {
	m := New(nil, nil)
	stdinR, stdinW := io.Pipe()
	stdoutR, stdoutW := io.Pipe()
	t.Cleanup(func() { stdinW.Close(); stdoutW.Close() })
	p := &proc{stdin: stdinW, done: make(chan struct{}), ctl: map[string]chan ctlReply{}}
	p.info.SessionID = "s1"
	m.procs["s1"] = p
	go m.readStdout(p, stdoutR)

	answered := make(chan string, 2)
	go func() {
		sc := bufio.NewScanner(stdinR)
		for sc.Scan() {
			var env struct {
				Type     string `json:"type"`
				Response struct {
					RequestID string `json:"request_id"`
				} `json:"response"`
			}
			if json.Unmarshal(sc.Bytes(), &env) == nil && env.Type == "control_response" {
				answered <- env.Response.RequestID
			}
		}
	}()

	for _, id := range []string{"req-a", "req-b"} {
		fmt.Fprintf(stdoutW, `{"type":"control_request","request_id":%q,"request":{"subtype":"can_use_tool","tool_name":"Bash","input":{"command":"echo %s"},"tool_use_id":"tu-%s"}}%s`, id, id, id, "\n")
	}
	waitFor(t, func() bool { m.mu.Lock(); defer m.mu.Unlock(); return len(p.queue) == 2 })

	m.mu.Lock()
	head, more := p.info.Pending, p.info.PendingMore
	m.mu.Unlock()
	if head == nil || head.RequestID != "req-a" || more != 1 {
		t.Fatalf("the first request must stay in front with one behind it: %+v more=%d", head, more)
	}

	// The user answers the second card first; the first is still pending and still answerable.
	if err := m.Respond("s1", "req-b", json.RawMessage(`{"behavior":"allow"}`)); err != nil {
		t.Fatalf("answering the queued request: %v", err)
	}
	if got := <-answered; got != "req-b" {
		t.Fatalf("answered %s, want req-b", got)
	}
	m.mu.Lock()
	head, more = p.info.Pending, p.info.PendingMore
	m.mu.Unlock()
	if head == nil || head.RequestID != "req-a" || more != 0 {
		t.Fatalf("req-a must be the card now, alone: %+v more=%d", head, more)
	}
	if err := m.Respond("s1", "req-a", json.RawMessage(`{"behavior":"deny"}`)); err != nil {
		t.Fatalf("answering the first request: %v", err)
	}
	if got := <-answered; got != "req-a" {
		t.Fatalf("answered %s, want req-a", got)
	}
	m.mu.Lock()
	head, more = p.info.Pending, p.info.PendingMore
	m.mu.Unlock()
	if head != nil || more != 0 {
		t.Fatalf("nothing should be pending: %+v more=%d", head, more)
	}
	if err := m.Respond("s1", "req-a", json.RawMessage(`{"behavior":"deny"}`)); err == nil {
		t.Error("answering the same request twice must be refused")
	}
}

// Claude Code answers the tool calls itself when the turn is interrupted and never withdraws the
// control_requests, so Interrupt drops them: a card left behind would answer a dead request.
func TestInterruptClearsPendingRequests(t *testing.T) {
	m := New(nil, nil)
	stdinR, stdinW := io.Pipe()
	stdoutR, stdoutW := io.Pipe()
	t.Cleanup(func() { stdinW.Close(); stdoutW.Close() })
	p := &proc{stdin: stdinW, done: make(chan struct{}), ctl: map[string]chan ctlReply{}}
	p.info.SessionID = "s1"
	m.procs["s1"] = p
	go m.readStdout(p, stdoutR)
	go io.Copy(io.Discard, stdinR) // the interrupt frame has to go somewhere

	fmt.Fprint(stdoutW, `{"type":"control_request","request_id":"req-a","request":{"subtype":"can_use_tool","tool_name":"Bash","input":{"command":"ls"},"tool_use_id":"tu"}}`+"\n")
	waitFor(t, func() bool { m.mu.Lock(); defer m.mu.Unlock(); return len(p.queue) == 1 })

	if err := m.Interrupt("s1"); err != nil {
		t.Fatalf("interrupt: %v", err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(p.queue) != 0 || p.info.Pending != nil {
		t.Fatalf("the interrupt must take the pending requests with it: %+v", p.info.Pending)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not reached in time")
}
