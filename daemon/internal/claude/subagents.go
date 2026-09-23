package claude

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/peter-dolkens/vineyard/daemon/internal/model"
)

// Claude Code gives every Agent-tool invocation its own transcript beside the session's:
//
//	~/.claude/projects/<enc cwd>/<sid>/subagents/agent-<agentId>.jsonl       the subagent's thread
//	~/.claude/projects/<enc cwd>/<sid>/subagents/agent-<agentId>.meta.json   who spawned it and why
//
// Nested subagents live in the same flat directory; their meta names the parent agent. Every line
// in these files is isSidechain:true, so they are derived with sidechain lines included.

// SubMeta is the .meta.json Claude Code writes when it spawns a subagent.
type SubMeta struct {
	AgentType     string `json:"agentType"`
	Description   string `json:"description"`
	ToolUseID     string `json:"toolUseId"`
	ParentAgentID string `json:"parentAgentId"`
	SpawnDepth    int    `json:"spawnDepth"`
	RequestShape  string `json:"requestShape"` // "background" when run_in_background
	Model         string `json:"model"`
}

type RawSubagent struct {
	AgentID   string
	Path      string
	Mtime     int64
	Size      int64
	SpawnedAt int64 // timestamp of the first transcript line (the prompt it was given)
	Meta      SubMeta
	Entries   []map[string]any
}

type subEntry struct {
	size, mtime int64
	entries     []map[string]any
	meta        SubMeta
	spawnedAt   int64
	seen        bool
}

const (
	subTailLines    = 40
	subMaxTailBytes = 200_000
	// maxSubagents bounds what one session contributes to a snapshot; the oldest finished ones go first.
	maxSubagents = 60
	// staleSubagentMs: a subagent transcript mid-turn but silent this long is no longer trusted to be
	// running (its parent was probably interrupted, or the task was stopped).
	staleSubagentMs = 15 * 60 * 1000
)

// SubagentsDir is where a session's subagent transcripts live, given the session's own transcript.
func SubagentsDir(transcriptPath string) string {
	return filepath.Join(strings.TrimSuffix(transcriptPath, ".jsonl"), "subagents")
}

// collectSubagents fills t.Subagents from disk, re-reading only files whose size or mtime changed.
func (c *Collector) collectSubagents(t *RawTranscript) {
	dir := SubagentsDir(t.Path)
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range ents {
		name := e.Name()
		if e.IsDir() || !strings.HasPrefix(name, "agent-") || !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		st, err := e.Info()
		if err != nil || st.Size() == 0 {
			continue
		}
		path := filepath.Join(dir, name)
		raw := RawSubagent{AgentID: strings.TrimSuffix(strings.TrimPrefix(name, "agent-"), ".jsonl"), Path: path, Mtime: st.ModTime().UnixMilli(), Size: st.Size()}

		c.mu.Lock()
		cached, ok := c.subCache[path]
		c.mu.Unlock()
		if !ok {
			// Read once per transcript: the meta (rewritten later, so its mtime is not the spawn time)
			// and the first line, whose timestamp is.
			metaPath := strings.TrimSuffix(path, ".jsonl") + ".meta.json"
			if b, err := os.ReadFile(metaPath); err == nil {
				_ = json.Unmarshal(b, &cached.meta)
			}
			cached.spawnedAt = firstTimestamp(path)
			if cached.spawnedAt == 0 {
				cached.spawnedAt = raw.Mtime
			}
		}
		if !ok || cached.size != raw.Size || cached.mtime != raw.Mtime {
			lines, err := TailLines(path, subTailLines, subMaxTailBytes)
			if err != nil {
				continue
			}
			cached.entries = nil
			for _, ln := range lines {
				var m map[string]any
				if json.Unmarshal(ln, &m) == nil && m != nil {
					cached.entries = append(cached.entries, m)
				}
			}
			cached.size, cached.mtime = raw.Size, raw.Mtime
		}
		cached.seen = true
		c.mu.Lock()
		c.subCache[path] = cached
		c.mu.Unlock()
		raw.Meta, raw.SpawnedAt, raw.Entries = cached.meta, cached.spawnedAt, cached.entries
		t.Subagents = append(t.Subagents, raw)
	}
}

// firstTimestamp returns the timestamp of a transcript's first complete line, or 0.
func firstTimestamp(path string) int64 {
	f, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer f.Close()
	buf := make([]byte, 256*1024)
	n, _ := io.ReadFull(f, buf)
	line := buf[:n]
	if i := bytes.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	} else if n == len(buf) {
		return 0 // first line longer than the window; no complete line to parse
	}
	var e map[string]any
	if json.Unmarshal(line, &e) != nil {
		return 0
	}
	return entryTime(e)
}

// pruneSubCache drops cache entries for transcripts not seen this poll and clears the seen marks.
func (c *Collector) pruneSubCache() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for p, e := range c.subCache {
		if !e.seen {
			delete(c.subCache, p)
			continue
		}
		e.seen = false
		c.subCache[p] = e
	}
}

var taskNotificationRe = regexp.MustCompile(`<tool-use-id>\s*([^<\s]+)\s*</tool-use-id>[\s\S]*?<status>\s*(\w+)\s*</status>`)

// parentResolutions reads the parent's tail for evidence that a subagent has returned: a tool_result
// for its Agent call (foreground agents), or a <task-notification> naming its tool-use id (background
// agents, whose tool_result arrives at launch and says nothing about completion).
func parentResolutions(parent []map[string]any) (results map[string]bool, notified map[string]string) {
	results, notified = map[string]bool{}, map[string]string{}
	for _, e := range parent {
		if side, _ := e["isSidechain"].(bool); side {
			continue
		}
		switch str(e["type"]) {
		case "user":
			for _, b := range blocks(e) {
				if str(b["type"]) == "tool_result" {
					results[str(b["tool_use_id"])] = true
				}
			}
		}
		var text string
		if s := str(e["content"]); strings.Contains(s, "<task-notification>") {
			text = s
		} else if att, ok := e["attachment"].(map[string]any); ok {
			if s := str(att["prompt"]); strings.Contains(s, "<task-notification>") {
				text = s
			}
		}
		if text != "" {
			for _, m := range taskNotificationRe.FindAllStringSubmatch(text, -1) {
				notified[m[1]] = m[2]
			}
		}
	}
	return results, notified
}

// BuildSubagents derives one model.Subagent per transcript under the session, in spawn order. alive is
// the parent process; nothing under a dead session is running.
func BuildSubagents(t *RawTranscript, alive bool, now int64) []model.Subagent {
	if t == nil || len(t.Subagents) == 0 {
		return nil
	}
	results, notified := parentResolutions(t.Entries)
	out := make([]model.Subagent, 0, len(t.Subagents))
	for _, raw := range t.Subagents {
		d := deriveEntries(raw.Entries, true)
		s := model.Subagent{
			AgentID:        raw.AgentID,
			ParentAgentID:  raw.Meta.ParentAgentID,
			Depth:          raw.Meta.SpawnDepth,
			Type:           raw.Meta.AgentType,
			Description:    clip(raw.Meta.Description, 120),
			Model:          d.Model,
			Background:     raw.Meta.RequestShape == "background",
			StartedAt:      raw.SpawnedAt,
			LastActivityAt: max64(d.LastActivityAt, raw.Mtime),
			ContextTokens:  d.ContextTokens,
			TranscriptPath: raw.Path,
			ToolUseID:      raw.Meta.ToolUseID,
		}
		if s.Model == "" {
			s.Model = raw.Meta.Model
		}
		if s.Depth == 0 {
			s.Depth = 1
		}
		state, detail := d.State, d.StateDetail
		returned := false
		if s.ToolUseID != "" {
			if st, ok := notified[s.ToolUseID]; ok && st != "running" {
				returned = true
			} else if !s.Background && results[s.ToolUseID] {
				returned = true
			}
		}
		switch {
		case !alive:
			state, detail = model.StateExited, "Session process has exited"
		case state == model.StateIdle:
			state = model.StateDone
			if detail == "" || detail == "Finished turn" {
				detail = "Finished"
			}
		case returned:
			state, detail = model.StateDone, "Returned to parent"
		case model.IsBusy(state) && now-s.LastActivityAt > staleSubagentMs:
			state, detail = model.StateUnknown, "No activity for "+duration(now-s.LastActivityAt)
		}
		s.State, s.StateDetail = state, detail
		if model.IsBusy(state) || model.NeedsAttention(state) {
			s.PendingTools = d.PendingTools
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].StartedAt != out[j].StartedAt {
			return out[i].StartedAt < out[j].StartedAt
		}
		return out[i].AgentID < out[j].AgentID
	})
	if len(out) > maxSubagents {
		kept := make([]model.Subagent, 0, maxSubagents)
		drop := len(out) - maxSubagents
		for _, s := range out {
			if drop > 0 && !model.IsBusy(s.State) && !model.NeedsAttention(s.State) {
				drop--
				continue
			}
			kept = append(kept, s)
		}
		out = kept
	}
	// A parent that was pruned (or never written) leaves its children at the top level.
	ids := map[string]bool{}
	for _, s := range out {
		ids[s.AgentID] = true
	}
	for i := range out {
		if out[i].ParentAgentID != "" && !ids[out[i].ParentAgentID] {
			out[i].ParentAgentID = ""
		}
	}
	return out
}

func duration(ms int64) string {
	m := ms / 60000
	if m < 60 {
		return itoa(int(m)) + "m"
	}
	return itoa(int(m/60)) + "h " + itoa(int(m%60)) + "m"
}
