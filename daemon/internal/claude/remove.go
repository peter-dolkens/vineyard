package claude

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Removed counts what a removal deleted, so the caller can say what it did.
type Removed struct {
	Transcripts int   `json:"transcripts"`
	Subagents   int   `json:"subagents"`
	Registry    int   `json:"registry"`
	Bytes       int64 `json:"bytes"`
}

func (r *Removed) add(o Removed) {
	r.Transcripts += o.Transcripts
	r.Subagents += o.Subagents
	r.Registry += o.Registry
	r.Bytes += o.Bytes
}

// insideProjects resolves a transcript path and refuses anything that is not a .jsonl under the
// Claude projects directory, so a request from the fleet cannot reach the rest of the disk.
func insideProjects(claudeDir, path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	projects, err := filepath.Abs(filepath.Join(claudeDir, "projects"))
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(projects, abs)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || !strings.HasSuffix(abs, ".jsonl") {
		return "", errors.New("transcript path must be inside the Claude projects directory")
	}
	return abs, nil
}

// RemoveSession deletes everything Claude Code kept for one session: its transcript, the sidecar
// folder of subagent transcripts and tool results beside it, its session-env and file-history
// folders, and the registry entry that keeps an exited session listed in the fleet. A session whose
// process is still running is refused — its next write would only put the transcript back.
func RemoveSession(claudeDir, path, sessionID string) (Removed, error) {
	var out Removed
	abs, err := insideProjects(claudeDir, path)
	if err != nil {
		return out, err
	}
	if sessionID != "" && strings.TrimSuffix(filepath.Base(abs), ".jsonl") != sessionID {
		return out, fmt.Errorf("%s is not the transcript of session %s", filepath.Base(abs), sessionID)
	}
	dead, err := registryFor(claudeDir, func(sid, _ string) bool { return sid != "" && sid == sessionID })
	if err != nil {
		return out, err
	}
	if err := removeSession(claudeDir, abs, &out); err != nil {
		return out, err
	}
	out.Registry = removeRegistry(dead)
	return out, nil
}

// RemoveProject deletes every Claude artifact for one workspace: the sessions under
// projects/<encoded cwd>, those of the session scratchpads that belong to it, and the registry
// entries for its sessions. Anything else Claude Code keeps in the project folder — notably the
// memory/ directory — stays, as does the workspace itself.
func RemoveProject(claudeDir, cwd string) (Removed, error) {
	var out Removed
	target := normalisePath(cwd)
	if target == "" {
		return out, errors.New("workspace path is empty")
	}
	known := map[string]string{EncodeProjectDir(target): target}
	mine := func(c string) bool {
		if c == "" {
			return false
		}
		if parent, ok := ScratchpadParent(c, known); ok {
			c = parent
		}
		return normalisePath(c) == target
	}
	root := filepath.Join(claudeDir, "projects")
	dirs := map[string]bool{}
	if ents, err := os.ReadDir(root); err == nil {
		for _, e := range ents {
			if d := filepath.Join(root, e.Name()); e.IsDir() && mine(dirCwd(d)) {
				dirs[d] = true
			}
		}
	}
	// A project folder whose transcripts no longer name a cwd still belongs here by its name.
	if enc := filepath.Join(root, EncodeProjectDir(target)); !dirs[enc] {
		if st, err := os.Stat(enc); err == nil && st.IsDir() {
			dirs[enc] = true
		}
	}
	var transcripts []string
	for d := range dirs {
		ents, err := os.ReadDir(d)
		if err != nil {
			continue
		}
		for _, e := range ents {
			if !e.IsDir() && strings.HasSuffix(e.Name(), ".jsonl") {
				transcripts = append(transcripts, filepath.Join(d, e.Name()))
			}
		}
	}
	if len(transcripts) == 0 {
		return out, fmt.Errorf("no Claude transcripts on this machine for %s", cwd)
	}
	dead, err := registryFor(claudeDir, func(_, c string) bool { return mine(c) })
	if err != nil {
		return out, err
	}
	for _, t := range transcripts {
		if err := removeSession(claudeDir, t, &out); err != nil {
			return out, err
		}
	}
	for d := range dirs {
		// Only if nothing else lives there: memory/ and anything Claude Code adds later must survive.
		_ = os.Remove(d)
	}
	out.Registry = removeRegistry(dead)
	return out, nil
}

// removeSession deletes one transcript and the per-session folders that belong with it, adding what
// went to out. The transcript path is trusted: callers resolve it through insideProjects.
func removeSession(claudeDir, transcript string, out *Removed) error {
	sid := strings.TrimSuffix(filepath.Base(transcript), ".jsonl")
	if st, err := os.Stat(transcript); err == nil {
		out.Transcripts, out.Bytes = out.Transcripts+1, out.Bytes+st.Size()
	}
	// Subagent transcripts and saved tool results sit in <sid>/ next to the file; the folder is ours.
	sidecar := strings.TrimSuffix(transcript, ".jsonl")
	out.add(countUnder(sidecar))
	if err := os.Remove(transcript); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.RemoveAll(sidecar); err != nil {
		return err
	}
	// Claude Code also files a session's captured environment and edit history under its id.
	for _, dir := range []string{"session-env", "file-history"} {
		p := filepath.Join(claudeDir, dir, sid)
		out.Bytes += countUnder(p).Bytes
		if err := os.RemoveAll(p); err != nil {
			return err
		}
	}
	return nil
}

// countUnder totals the transcripts under a directory and the bytes everything in it takes up,
// before it is removed.
func countUnder(dir string) Removed {
	var out Removed
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if strings.HasSuffix(p, ".jsonl") && filepath.Base(filepath.Dir(p)) == "subagents" {
			out.Subagents++
		}
		if st, err := d.Info(); err == nil {
			out.Bytes += st.Size()
		}
		return nil
	})
	return out
}

// dirCwd reads the working directory a project folder belongs to out of its newest transcript, the
// same way the collector does.
func dirCwd(dir string) string {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	var latest string
	var latestM int64
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		if st, err := e.Info(); err == nil && st.ModTime().UnixMilli() >= latestM {
			latestM, latest = st.ModTime().UnixMilli(), filepath.Join(dir, e.Name())
		}
	}
	if latest == "" {
		return ""
	}
	return findCwd(latest)
}

// registryFor returns the pid prefixes under sessions/ whose entry matches, and refuses the lot if
// one of them is still running: removing a live session's artifacts would only have them written
// again.
func registryFor(claudeDir string, match func(sessionID, cwd string) bool) ([]string, error) {
	dir := filepath.Join(claudeDir, "sessions")
	ents, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var dead []string
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		var reg struct {
			SessionID string `json:"sessionId"`
			Cwd       string `json:"cwd"`
		}
		if json.Unmarshal(b, &reg) != nil || !match(reg.SessionID, reg.Cwd) {
			continue
		}
		pid := strings.TrimSuffix(e.Name(), ".json")
		if n, err := strconv.Atoi(pid); err == nil && processAlive(n) {
			return nil, fmt.Errorf("session %s is still running (pid %d); stop it first", shortID(reg.SessionID), n)
		}
		dead = append(dead, filepath.Join(dir, pid))
	}
	return dead, nil
}

// removeRegistry deletes each entry and the key file Claude Code writes beside it (<pid>.<hash>.key).
func removeRegistry(prefixes []string) int {
	n := 0
	for _, p := range prefixes {
		if os.Remove(p+".json") == nil {
			n++
		}
		ents, err := os.ReadDir(filepath.Dir(p))
		if err != nil {
			continue
		}
		for _, e := range ents {
			if strings.HasPrefix(e.Name(), filepath.Base(p)+".") && strings.HasSuffix(e.Name(), ".key") {
				_ = os.Remove(filepath.Join(filepath.Dir(p), e.Name()))
			}
		}
	}
	return n
}

func shortID(sessionID string) string {
	if len(sessionID) > 8 {
		return sessionID[:8]
	}
	return sessionID
}
