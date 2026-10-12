package mesh

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/peter-dolkens/vineyard/daemon/internal/protocol"
)

type claudeUpdateReply struct {
	Done    bool   `json:"done"`
	Message string `json:"message"`
	Output  string `json:"output"`
}

func fakeClaude(t *testing.T, script string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell script stand-in for claude")
	}
	bin := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

func claudeUpdate(t *testing.T, n *Node, action string) (claudeUpdateReply, error) {
	t.Helper()
	var r claudeUpdateReply
	data, err := n.handleClaudeUpdate(protocol.ClaudeUpdateArgs{Action: action})
	if err == nil {
		err = json.Unmarshal(data, &r)
	}
	return r, err
}

func TestClaudeUpdateWaitsAcrossRequests(t *testing.T) {
	old := claudeUpdateWait
	claudeUpdateWait = 50 * time.Millisecond
	t.Cleanup(func() { claudeUpdateWait = old })

	n := newTestNode(t, "m1")
	n.cfg.ClaudeBin = fakeClaude(t, `[ "$1" = update ] || exit 9
sleep 0.3
printf 'Current version: 2.1.0\n\033[32mSuccessfully updated from 2.1.0 to version 2.1.5\033[0m\n'
`)
	r, err := claudeUpdate(t, n, "start")
	if err != nil || r.Done {
		t.Fatalf("first reply should be not done yet: %+v %v", r, err)
	}
	// A second start joins the running update rather than starting another.
	first := n.ccUpdate.cur
	if _, err := claudeUpdate(t, n, "start"); err != nil || n.ccUpdate.cur != first {
		t.Fatalf("second start should join the first: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for !r.Done && time.Now().Before(deadline) {
		if r, err = claudeUpdate(t, n, "wait"); err != nil {
			t.Fatal(err)
		}
	}
	if r.Message != "Successfully updated from 2.1.0 to version 2.1.5" {
		t.Fatalf("message = %q", r.Message)
	}
	if !strings.Contains(r.Output, "Current version") {
		t.Fatalf("output = %q", r.Output)
	}
}

func TestClaudeUpdateReportsFailure(t *testing.T) {
	n := newTestNode(t, "m1")
	n.cfg.ClaudeBin = fakeClaude(t, "echo 'Error: permission denied' >&2; exit 1\n")
	_, err := claudeUpdate(t, n, "start")
	if err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("err = %v", err)
	}
}

func TestClaudeUpdateWaitWithoutStart(t *testing.T) {
	n := newTestNode(t, "m1")
	if _, err := claudeUpdate(t, n, "wait"); err == nil {
		t.Fatal("wait with nothing running should fail")
	}
}
