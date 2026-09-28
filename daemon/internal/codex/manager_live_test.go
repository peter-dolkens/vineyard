package codex

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/peter-dolkens/vineyard/daemon/internal/model"
)

// TestLiveManager drives a real `codex app-server` and costs a few thousand tokens on the signed-in
// account, so it runs only with VINEYARD_CODEX_LIVE=1. It covers spawn, a turn, an approval prompt
// answered with a decline, a rename, model list and account, then a clean stop.
func TestLiveManager(t *testing.T) {
	if os.Getenv("VINEYARD_CODEX_LIVE") == "" {
		t.Skip("set VINEYARD_CODEX_LIVE=1 to drive a real codex app-server")
	}
	if _, err := FindCodex(""); err != nil {
		t.Skip("codex not installed")
	}
	dir := t.TempDir()
	changes := make(chan struct{}, 100)
	m := New(log.New(os.Stderr, "", log.Ltime), func() {
		select {
		case changes <- struct{}{}:
		default:
		}
	})
	sid, err := m.Spawn(SpawnOptions{Cwd: dir, PermissionMode: "untrusted", Name: "vineyard live test"})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("thread %s", sid)
	defer func() { _ = m.Stop(sid) }()
	if !m.Has(sid) || m.Cwd(sid) != dir {
		t.Fatal("not registered")
	}
	waitFor := func(what string, timeout time.Duration, ok func(model.ManagedInfo) bool) model.ManagedInfo {
		deadline := time.Now().Add(timeout)
		for time.Now().Before(deadline) {
			for _, info := range m.All() {
				if info.SessionID == sid && ok(info) {
					return info
				}
			}
			select {
			case <-changes:
			case <-time.After(500 * time.Millisecond):
			}
		}
		t.Fatalf("timed out waiting for %s", what)
		return model.ManagedInfo{}
	}
	info := waitFor("models and account", 30*time.Second, func(i model.ManagedInfo) bool { return len(i.Models) > 0 && i.Account != "" })
	t.Logf("account %s (%s), %d models, mode %s, model %s, effort %s, name %q", info.Account, info.AccountPlan, len(info.Models), info.PermissionMode, info.Model, info.Effort, info.Name)
	if info.PermissionMode != "untrusted" || len(info.Modes) == 0 {
		t.Fatalf("mode %q, modes %d", info.PermissionMode, len(info.Modes))
	}
	if err := m.Send(sid, "Run the shell command `touch probe.txt` in the working directory. If it is not allowed, just say so."); err != nil {
		t.Fatal(err)
	}
	info = waitFor("approval prompt", 90*time.Second, func(i model.ManagedInfo) bool { return i.Pending != nil })
	if info.Pending.ToolName != "Bash" || !strings.Contains(string(info.Pending.Input), "touch probe.txt") {
		t.Fatalf("pending: %+v", info.Pending)
	}
	t.Logf("pending %s %s", info.Pending.ToolName, string(info.Pending.Input))
	if err := m.Respond(sid, info.Pending.RequestID, json.RawMessage(`{"behavior":"deny","message":"The user denied this action in Vineyard."}`)); err != nil {
		t.Fatal(err)
	}
	info = waitFor("turn to complete", 90*time.Second, func(i model.ManagedInfo) bool { return i.Turns >= 1 && i.Pending == nil })
	if _, err := os.Stat(filepath.Join(dir, "probe.txt")); err == nil {
		t.Fatal("probe.txt was created despite the decline")
	}
	if info.Context == nil || info.Context.TotalTokens == 0 || info.Context.MaxTokens == 0 {
		t.Fatalf("context: %+v", info.Context)
	}
	if info.Usage == nil || len(info.Usage.Windows) == 0 {
		t.Fatalf("usage: %+v", info.Usage)
	}
	if info.Name != "vineyard live test" {
		t.Fatalf("name %q", info.Name)
	}
	if err := m.Rename(sid, "vineyard live test 2"); err != nil {
		t.Fatal(err)
	}
	if err := m.SetPermissionMode(sid, "read-only"); err != nil {
		t.Fatal(err)
	}
	if err := m.SetEffort(sid, "low"); err != nil {
		t.Fatal(err)
	}
	if err := m.Send(sid, "Reply with exactly the single word: pong"); err != nil {
		t.Fatal(err)
	}
	info = waitFor("second turn", 90*time.Second, func(i model.ManagedInfo) bool { return i.Turns >= 2 })
	if info.Name != "vineyard live test 2" || info.PermissionMode != "read-only" {
		t.Fatalf("after second turn: name %q mode %q", info.Name, info.PermissionMode)
	}
	// The rollout on disk shows the same thread, live, with the second answer.
	c := NewCollector("", 200)
	r := c.Collect()
	var found *RawThread
	for i := range r.Threads {
		if r.Threads[i].ID == sid {
			found = &r.Threads[i]
		}
	}
	if found == nil {
		t.Fatal("collector did not see the managed thread as live")
	}
	a := BuildAgent("m", *found, time.Now().UnixMilli())
	t.Logf("collector: state %s %q model %s mode %s", a.State, a.StateDetail, a.Model, a.PermissionMode)
	if a.State != model.StateIdle || a.StateDetail != "pong" {
		t.Fatalf("collector state %s %q", a.State, a.StateDetail)
	}
	agents := m.Merge("m", []model.Agent{*a})
	if agents[0].Managed == nil || agents[0].Title != "vineyard live test 2" {
		t.Fatalf("merge: %+v", agents[0])
	}
	if err := m.Stop(sid); err != nil {
		t.Fatal(err)
	}
	waitFor("exit", 20*time.Second, func(i model.ManagedInfo) bool { return i.Exited })
	if lockHeld(filepath.Join(DefaultCodexDir(), "thread-writer-locks", sid+".lock")) {
		t.Fatal("lock still held after stop")
	}
}
