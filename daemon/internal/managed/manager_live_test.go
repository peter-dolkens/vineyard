package managed

import (
	"encoding/json"
	"os"
	"testing"
	"time"
)

// Against the real Claude Code: two subagents each asking to run a command block on two can_use_tool
// requests at once. Run with VINEYARD_LIVE=1 (it spends tokens and needs a signed-in CLI).
func TestLiveTwoSubagentPermissions(t *testing.T) {
	if os.Getenv("VINEYARD_CLAUDE_LIVE") == "" {
		t.Skip("set VINEYARD_CLAUDE_LIVE=1 to drive a real Claude Code")
	}
	if _, err := FindClaude(""); err != nil {
		t.Skip("claude not installed")
	}
	dir := t.TempDir()
	m := New(nil, func() {})
	sid, err := m.Spawn(SpawnOptions{
		Cwd:   dir,
		Model: "claude-haiku-4-5-20251001",
		Prompt: "Launch two subagents in parallel with the Agent tool in a single message " +
			"(subagent_type general-purpose). Tell agent one to run exactly `echo one > a.txt` with Bash, " +
			"and agent two to run exactly `echo two > b.txt` with Bash. Each must use Bash.",
	})
	if err != nil {
		t.Fatal(err)
	}
	p, err := m.get(sid)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Stop(sid)

	var ids []string
	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		m.mu.Lock()
		n := len(p.queue)
		if n >= 2 {
			for _, pr := range p.queue {
				ids = append(ids, pr.RequestID)
			}
		}
		head, more := p.info.Pending, p.info.PendingMore
		m.mu.Unlock()
		if len(ids) >= 2 {
			t.Logf("two requests queued: head=%s more=%d", head.ToolName, more)
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	if len(ids) < 2 {
		t.Fatalf("never saw two requests queued at once (got %d)", len(ids))
	}
	// Both must be answerable, in either order, and each answer must reach Claude Code.
	for _, id := range []string{ids[1], ids[0]} {
		if err := m.Respond(sid, id, json.RawMessage(`{"behavior":"allow","updatedInput":{}}`)); err != nil {
			t.Fatalf("answering %s: %v", id, err)
		}
	}
	// The turn finishes only if both subagents got their answer.
	done := time.Now().Add(2 * time.Minute)
	for time.Now().Before(done) {
		if _, err := os.Stat(dir + "/a.txt"); err == nil {
			if _, err := os.Stat(dir + "/b.txt"); err == nil {
				return
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatal("both commands should have run once both requests were answered")
}

// Against the real Claude Code: switching a session to bypassPermissions is refused unless it was
// launched allowing it. No prompt is sent, so this spends no tokens. Run with VINEYARD_CLAUDE_LIVE=1.
func TestLiveAllowBypassPermissions(t *testing.T) {
	if os.Getenv("VINEYARD_CLAUDE_LIVE") == "" {
		t.Skip("set VINEYARD_CLAUDE_LIVE=1 to drive a real Claude Code")
	}
	if _, err := FindClaude(""); err != nil {
		t.Skip("claude not installed")
	}
	m := New(nil, func() {})
	for _, allow := range []bool{false, true} {
		sid, err := m.Spawn(SpawnOptions{Cwd: t.TempDir(), AllowBypassPermissions: allow})
		if err != nil {
			t.Fatal(err)
		}
		p, _ := m.get(sid)
		m.mu.Lock()
		avail := p.info.BypassAvailable
		m.mu.Unlock()
		if avail != allow {
			t.Errorf("allow=%v: BypassAvailable = %v", allow, avail)
		}
		err = m.SetPermissionMode(sid, "bypassPermissions")
		t.Logf("allow=%v: set_permission_mode bypassPermissions -> %v", allow, err)
		if allow && err != nil {
			t.Errorf("a session launched allowing bypass should switch to it: %v", err)
		}
		if !allow && err == nil {
			t.Error("a session launched without the flag should refuse bypass")
		}
		if allow {
			if err := m.SetPermissionMode(sid, "default"); err != nil {
				t.Errorf("switching back out of bypass: %v", err)
			}
		}
		_ = m.Stop(sid)
	}
}
