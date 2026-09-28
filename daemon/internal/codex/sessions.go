package codex

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/peter-dolkens/vineyard/daemon/internal/claude"
)

// ListSessions lists past Codex threads on this machine, newest first, as SessionSummary rows with
// Provider "codex", so a viewer can offer them beside Claude Code's for resuming.
func ListSessions(codexDir, cwd string, limit int) []claude.SessionSummary {
	c := NewCollector(codexDir, 0)
	if limit <= 0 {
		limit = 60
	}
	files := c.walk(maxRollouts)
	sort.Slice(files, func(i, j int) bool { return files[i].mtime > files[j].mtime })
	want := ""
	if cwd != "" {
		want = normalisePath(cwd)
	}
	var out []claude.SessionSummary
	for _, f := range files {
		if len(out) >= limit {
			break
		}
		h := readHead(f.path)
		if h.Cwd == "" || (want != "" && normalisePath(h.Cwd) != want) {
			continue
		}
		s := claude.SessionSummary{Provider: Provider, SessionID: f.id, Path: f.path, Mtime: f.mtime, Size: f.size, Cwd: h.Cwd, FirstPrompt: h.FirstPrompt, Title: clip(h.FirstPrompt, 80)}
		if lines, _, err := ReadTail(f.path, 200, MaxTailBytes); err == nil {
			d := Derive(lines)
			s.LastPrompt, s.Model, s.GitBranch = d.LastPrompt, d.Model, d.GitBranch
			for _, l := range lines {
				if l.itemType() == "message" && str(l.Payload["role"]) == "user" {
					if t := messageText(l.Payload); t != "" && !isMetaPrompt(t) {
						s.Turns++
					}
				}
			}
		}
		out = append(out, s)
	}
	return out
}

// FindCodex locates the codex binary: PATH, then the usual install locations.
func FindCodex(override string) (string, error) {
	if override != "" {
		return override, nil
	}
	name := "codex"
	if runtime.GOOS == "windows" {
		name = "codex.exe"
	}
	if p, err := exec.LookPath(name); err == nil {
		return p, nil
	}
	home, _ := os.UserHomeDir()
	for _, c := range []string{
		filepath.Join(home, ".local", "bin", name), "/opt/homebrew/bin/codex", "/usr/local/bin/codex",
		filepath.Join(home, ".npm-global", "bin", name), filepath.Join(home, "AppData", "Roaming", "npm", "codex.cmd"),
	} {
		if st, err := os.Stat(c); err == nil && !st.IsDir() {
			return c, nil
		}
	}
	return "", os.ErrNotExist
}

// SendToThread queues a message for a Codex thread this daemon does not drive (`codex queue`): the
// process that owns the thread picks it up and starts a turn, like Claude Code's cross-session inbox.
func SendToThread(bin, codexDir, threadID, text string) error {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	exe, err := FindCodex(bin)
	if err != nil {
		return err
	}
	cmd := exec.Command(exe, "queue", "--thread", threadID, "--message", text)
	if codexDir != "" {
		cmd.Env = append(os.Environ(), "CODEX_HOME="+codexDir)
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		return &queueError{msg}
	}
	return nil
}

type queueError struct{ msg string }

func (e *queueError) Error() string { return "codex queue: " + e.msg }
