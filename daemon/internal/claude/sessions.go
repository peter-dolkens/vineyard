package claude

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// SessionSummary describes one transcript on disk, for "resume a past session" pickers.
type SessionSummary struct {
	SessionID   string `json:"sessionId"`
	Cwd         string `json:"cwd"`
	Path        string `json:"path"`
	Mtime       int64  `json:"mtime"`
	Size        int64  `json:"size"`
	Title       string `json:"title,omitempty"`
	FirstPrompt string `json:"firstPrompt,omitempty"`
	LastPrompt  string `json:"lastPrompt,omitempty"`
	Model       string `json:"model,omitempty"`
	GitBranch   string `json:"gitBranch,omitempty"`
	Turns       int    `json:"turns,omitempty"` // user prompts seen in the head+tail sample (lower bound)

	aiTitle, customTitle string
}

const (
	sessionsHeadBytes = 96 * 1024
	sessionsTailLines = 40
)

// ListSessions returns transcripts newest first. cwd narrows to one workspace; "" lists every project.
// Each file is sampled (head for the title and first prompt, tail for the last prompt and model), so
// the call stays cheap even with hundreds of sessions.
func ListSessions(claudeDir, cwd string, limit int) ([]SessionSummary, error) {
	root := filepath.Join(claudeDir, "projects")
	var dirs []string
	if cwd != "" {
		dirs = []string{filepath.Join(root, EncodeProjectDir(cwd))}
	} else {
		ents, err := os.ReadDir(root)
		if err != nil {
			return nil, err
		}
		for _, e := range ents {
			if e.IsDir() {
				dirs = append(dirs, filepath.Join(root, e.Name()))
			}
		}
	}
	type file struct {
		path  string
		mtime int64
		size  int64
	}
	var files []file
	for _, d := range dirs {
		ents, err := os.ReadDir(d)
		if err != nil {
			continue
		}
		for _, e := range ents {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
				continue
			}
			if st, err := e.Info(); err == nil && st.Size() > 0 {
				files = append(files, file{filepath.Join(d, e.Name()), st.ModTime().UnixMilli(), st.Size()})
			}
		}
	}
	sort.Slice(files, func(i, j int) bool { return files[i].mtime > files[j].mtime })
	if limit <= 0 {
		limit = 60
	}
	if len(files) > limit {
		files = files[:limit]
	}
	out := make([]SessionSummary, 0, len(files))
	for _, f := range files {
		s := SessionSummary{SessionID: strings.TrimSuffix(filepath.Base(f.path), ".jsonl"), Path: f.path, Mtime: f.mtime, Size: f.size, Cwd: cwd}
		summarize(&s)
		if s.Cwd == "" {
			continue // could not tell where it ran; cannot resume it sensibly
		}
		out = append(out, s)
	}
	return out, nil
}

func summarize(s *SessionSummary) {
	head, _ := os.Open(s.Path)
	if head != nil {
		sc := bufio.NewScanner(head)
		sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
		read := 0
		for sc.Scan() && read < sessionsHeadBytes {
			read += len(sc.Bytes()) + 1
			var e map[string]any
			if json.Unmarshal(sc.Bytes(), &e) != nil {
				continue
			}
			absorb(s, e, true)
		}
		head.Close()
	}
	tail, err := TailLines(s.Path, sessionsTailLines, MaxTailBytes)
	if err != nil {
		return
	}
	for _, ln := range tail {
		var e map[string]any
		if json.Unmarshal(ln, &e) != nil {
			continue
		}
		absorb(s, e, false)
	}
}

// absorb folds one transcript line into the summary. fromHead lines may set the first prompt.
func absorb(s *SessionSummary, e map[string]any, fromHead bool) {
	if s.Cwd == "" {
		if c := str(e["cwd"]); c != "" {
			s.Cwd = c
		}
	}
	switch str(e["type"]) {
	case "ai-title":
		if t := str(e["aiTitle"]); t != "" {
			s.aiTitle = t
		}
		s.Title = PreferredTitle(s.customTitle, s.aiTitle)
	case "custom-title":
		if t := str(e["customTitle"]); t != "" {
			s.customTitle = t
		}
		s.Title = PreferredTitle(s.customTitle, s.aiTitle)
	case "last-prompt":
		if p := str(e["lastPrompt"]); p != "" {
			s.LastPrompt = clip(p, 160)
		}
	case "assistant":
		if m := str(msg(e)["model"]); m != "" {
			s.Model = m
		}
		if b := str(e["gitBranch"]); b != "" {
			s.GitBranch = b
		}
	case "user":
		if side, _ := e["isSidechain"].(bool); side {
			return
		}
		if meta, _ := e["isMeta"].(bool); meta {
			return
		}
		text := promptText(msg(e))
		if text == "" || strings.HasPrefix(text, "<") {
			return // tool results, cross-session wrappers, local command output
		}
		s.Turns++
		if fromHead && s.FirstPrompt == "" {
			s.FirstPrompt = clip(text, 160)
		}
		s.LastPrompt = clip(text, 160)
		if b := str(e["gitBranch"]); b != "" {
			s.GitBranch = b
		}
	}
}

func promptText(m map[string]any) string {
	switch c := m["content"].(type) {
	case string:
		return strings.TrimSpace(c)
	case []any:
		for _, b := range c {
			if bm, ok := b.(map[string]any); ok && str(bm["type"]) == "text" {
				if t := strings.TrimSpace(str(bm["text"])); t != "" {
					return t
				}
			}
		}
	}
	return ""
}
