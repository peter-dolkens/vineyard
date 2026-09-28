package mesh

import (
	"encoding/json"
	"log"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/peter-dolkens/vineyard/daemon/internal/claude"
	"github.com/peter-dolkens/vineyard/daemon/internal/codex"
	"github.com/peter-dolkens/vineyard/daemon/internal/config"
	"github.com/peter-dolkens/vineyard/daemon/internal/model"
	"github.com/peter-dolkens/vineyard/daemon/internal/protocol"
)

// TestLiveCodexOps drives a real Codex thread through the node's request handlers, the way a viewer
// does: spawn with provider codex, send, transcript, sessions, kill. It costs a few thousand tokens
// on the signed-in account, so it runs only with VINEYARD_CODEX_LIVE=1.
func TestLiveCodexOps(t *testing.T) {
	if os.Getenv("VINEYARD_CODEX_LIVE") == "" {
		t.Skip("set VINEYARD_CODEX_LIVE=1 to drive a real codex app-server")
	}
	if _, err := codex.FindCodex(""); err != nil {
		t.Skip("codex not installed")
	}
	t.Setenv("VINEYARD_DIR", t.TempDir())
	if err := config.GenerateFleetCert(); err != nil {
		t.Fatal(err)
	}
	cfg := config.New("live", "", 0, "")
	collector := codex.NewCollector("", 200)
	mgr := codex.New(log.New(os.Stderr, "", log.Ltime), nil)
	n, err := New(Options{
		Config: cfg, Version: "test", Log: log.New(os.Stderr, "", 0), Codex: mgr, CodexDir: collector.CodexDir,
		Collect: func() model.Snapshot {
			r := collector.Collect()
			agents, workspaces := codex.Interpret("live", r, time.Now().UnixMilli())
			agents = mgr.Merge("live", agents)
			return model.Snapshot{Agents: agents, Workspaces: claude.Regroup("live", agents, workspaces), HasCodex: r.HasCodex}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	call := func(op string, args any) json.RawMessage {
		t.Helper()
		b, _ := json.Marshal(args)
		res, err := n.handleLocal(protocol.Request{Op: op, Args: b})
		if err != nil {
			t.Fatalf("%s: %v", op, err)
		}
		return res
	}
	var spawned struct {
		SessionID string `json:"sessionId"`
		Provider  string `json:"provider"`
	}
	_ = json.Unmarshal(call("spawn", map[string]any{"provider": "codex", "cwd": dir, "name": "vineyard ops test"}), &spawned)
	if spawned.SessionID == "" || spawned.Provider != "codex" {
		t.Fatalf("spawn: %+v", spawned)
	}
	sid := spawned.SessionID
	defer func() { _ = mgr.Stop(sid) }()
	call("send", protocol.SendArgs{SessionID: sid, Text: "Reply with exactly the single word: pong"})
	deadline := time.Now().Add(90 * time.Second)
	var info model.ManagedInfo
	for time.Now().Before(deadline) {
		for _, i := range mgr.All() {
			if i.SessionID == sid {
				info = i
			}
		}
		if info.Turns >= 1 {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if info.Turns < 1 {
		t.Fatal("turn did not complete")
	}
	// The collector sees the thread live, under the temp workspace, with the managed state merged.
	n.collectSelf(true)
	ag := n.localAgent(sid)
	if ag == nil || ag.Provider != "codex" || ag.Managed == nil || ag.WorkspacePath != strings.TrimRight(dir, "/") || ag.Title != "vineyard ops test" {
		t.Fatalf("agent: %+v", ag)
	}
	// Its transcript arrives as Claude-shaped entries, streamable by offset.
	var tr protocol.TranscriptData
	_ = json.Unmarshal(call("transcript", protocol.TranscriptArgs{Path: ag.TranscriptPath}), &tr)
	joined := ""
	for _, e := range tr.Entries {
		joined += string(e) + "\n"
	}
	if !strings.Contains(joined, "Reply with exactly the single word: pong") || !strings.Contains(joined, `"pong"`) || strings.Contains(joined, "environment_context") {
		t.Fatalf("transcript entries: %s", joined)
	}
	var more protocol.TranscriptData
	_ = json.Unmarshal(call("transcript", protocol.TranscriptArgs{Path: ag.TranscriptPath, Offset: tr.Offset}), &more)
	if len(more.Entries) != 0 || more.Offset != tr.Offset {
		t.Fatalf("expected nothing new past offset %d: %+v", tr.Offset, more)
	}
	// It is listed among the past sessions of its workspace, tagged codex.
	var sessions struct {
		Sessions []claude.SessionSummary `json:"sessions"`
	}
	_ = json.Unmarshal(call("sessions", protocol.SessionsArgs{Cwd: dir, Limit: 10}), &sessions)
	found := false
	for _, s := range sessions.Sessions {
		if s.SessionID == sid && s.Provider == "codex" && s.FirstPrompt != "" {
			found = true
		}
	}
	if !found {
		t.Fatalf("sessions: %+v", sessions.Sessions)
	}
	// A mode change goes through the configure op, and kill ends the managed thread cleanly.
	mode := "read-only"
	call("configure", protocol.ConfigureArgs{SessionID: sid, PermissionMode: &mode})
	call("kill", protocol.SendArgs{SessionID: sid})
	deadline = time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) && mgr.Has(sid) {
		time.Sleep(200 * time.Millisecond)
	}
	if mgr.Has(sid) {
		t.Fatal("thread still managed after kill")
	}
}
