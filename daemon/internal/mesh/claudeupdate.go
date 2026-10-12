package mesh

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/peter-dolkens/vineyard/daemon/internal/managed"
	"github.com/peter-dolkens/vineyard/daemon/internal/protocol"
)

// claudeUpdater runs `claude update` on this machine for a viewer elsewhere. An update can outlast
// the relay's request timeout, so each request waits a while for it and the viewer asks again until
// it is done. A "start" while one is running joins it instead of starting another.
type claudeUpdater struct {
	mu  sync.Mutex
	cur *claudeUpdateRun
}

type claudeUpdateRun struct {
	done   chan struct{}
	output string
	err    error
}

var (
	claudeUpdateWait  = 40 * time.Second // under reqTimeout, so a relayed request is answered
	claudeUpdateLimit = 10 * time.Minute
	ansiEscRe         = regexp.MustCompile(`\x1b\[[0-9;?]*[A-Za-z]`)
)

func (n *Node) handleClaudeUpdate(a protocol.ClaudeUpdateArgs) (json.RawMessage, error) {
	u := &n.ccUpdate
	u.mu.Lock()
	r := u.cur
	switch a.Action {
	case "start":
		if r == nil || r.finished() {
			bin, err := managed.FindClaude(n.cfg.ClaudeBin)
			if err != nil {
				u.mu.Unlock()
				return nil, err
			}
			r = &claudeUpdateRun{done: make(chan struct{})}
			u.cur = r
			n.logf("claude update: started (%s)", bin)
			go n.runClaudeUpdate(r, bin)
		}
	case "wait":
		if r == nil {
			u.mu.Unlock()
			return nil, errors.New("no Claude Code update has run on this machine")
		}
	default:
		u.mu.Unlock()
		return nil, fmt.Errorf("unknown claudeupdate action %q", a.Action)
	}
	u.mu.Unlock()

	select {
	case <-r.done:
	case <-time.After(claudeUpdateWait):
		return json.Marshal(map[string]any{"done": false})
	}
	if r.err != nil {
		return nil, fmt.Errorf("claude update failed: %v: %s", r.err, tailLines(r.output, 6))
	}
	return json.Marshal(map[string]any{"done": true, "message": lastLine(r.output), "output": r.output})
}

func (n *Node) runClaudeUpdate(r *claudeUpdateRun, bin string) {
	ctx, cancel := context.WithTimeout(context.Background(), claudeUpdateLimit)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "update")
	cmd.Env = append(os.Environ(), "CLAUDE_CODE_ENTRYPOINT=vineyard")
	out, err := cmd.CombinedOutput()
	r.output = strings.TrimSpace(ansiEscRe.ReplaceAllString(string(out), ""))
	if len(r.output) > 8000 {
		r.output = "…" + r.output[len(r.output)-8000:]
	}
	r.err = err
	if err != nil {
		n.logf("claude update: failed: %v: %s", err, tailLines(r.output, 6))
	} else {
		n.logf("claude update: %s", lastLine(r.output))
	}
	close(r.done)
}

func (r *claudeUpdateRun) finished() bool {
	select {
	case <-r.done:
		return true
	default:
		return false
	}
}

func lastLine(s string) string {
	return tailLines(s, 1)
}

// tailLines is the last n non-empty lines of s, joined by newlines.
func tailLines(s string, n int) string {
	var keep []string
	lines := strings.Split(s, "\n")
	for i := len(lines) - 1; i >= 0 && len(keep) < n; i-- {
		if l := strings.TrimSpace(lines[i]); l != "" {
			keep = append([]string{l}, keep...)
		}
	}
	return strings.Join(keep, "\n")
}
