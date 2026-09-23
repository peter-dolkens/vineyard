package claude

import (
	"regexp"
	"sort"
	"strings"

	"github.com/peter-dolkens/vineyard/daemon/internal/model"
)

// Background tasks are shell commands a session left running: a Bash call made with
// run_in_background, or a foreground one that outlived its timeout and was moved to the background.
// Either way the tool_result arrives at once naming a task id, and the command's end is announced
// later by a <task-notification> (a queue-operation line and a queued_command attachment) naming the
// tool-use id and a status. The chat's Agent map and the tree show them beside the subagents.

var (
	// The two tool_result phrasings that hand back a background task id.
	bgTaskIDRe = regexp.MustCompile(`(?:moved to the background \(ID: |running in background with ID: )([A-Za-z0-9_-]+)`)
	// One notification: task id, the tool-use id it belongs to, then how it ended.
	bgTaskNoteRe = regexp.MustCompile(`<task-id>\s*([^<\s]+)\s*</task-id>[\s\S]*?<tool-use-id>\s*([^<\s]+)\s*</tool-use-id>[\s\S]*?<status>\s*(\w+)\s*</status>`)
)

// finishedTaskTTL is how long a finished task stays listed after its notification.
const finishedTaskTTL = 30 * 60 * 1000

func resultText(b map[string]any) string {
	switch c := b["content"].(type) {
	case string:
		return c
	case []any:
		var parts []string
		for _, x := range c {
			if m, ok := x.(map[string]any); ok {
				parts = append(parts, str(m["text"]))
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

// notificationText returns the <task-notification> text an entry carries, if any: the content of a
// queue-operation line, or the prompt of a queued_command attachment.
func notificationText(e map[string]any) string {
	if s := str(e["content"]); strings.Contains(s, "<task-notification>") {
		return s
	}
	if att, ok := e["attachment"].(map[string]any); ok {
		if s := str(att["prompt"]); strings.Contains(s, "<task-notification>") {
			return s
		}
	}
	return ""
}

// BuildTasks finds the background shell commands in a transcript tail, in start order. A notification
// whose start is not in the tail comes back as a task with no StartedAt, for MergeTasks to apply to
// one remembered from an earlier poll (and to drop otherwise).
func BuildTasks(entries []map[string]any) []model.BackgroundTask {
	type bashCall struct {
		desc string
		at   int64
	}
	byID := map[string]*model.BackgroundTask{}
	var order []string
	get := func(id string) *model.BackgroundTask {
		if t, ok := byID[id]; ok {
			return t
		}
		t := &model.BackgroundTask{ToolUseID: id, Kind: "shell", State: "running"}
		byID[id] = t
		order = append(order, id)
		return t
	}
	foreground := map[string]bashCall{} // Bash calls not asked to run in the background, by tool-use id

	for _, e := range entries {
		if side, _ := e["isSidechain"].(bool); side {
			continue
		}
		ts := entryTime(e)
		switch str(e["type"]) {
		case "assistant":
			for _, b := range blocks(e) {
				if str(b["type"]) != "tool_use" || str(b["name"]) != "Bash" {
					continue
				}
				id := str(b["id"])
				if id == "" {
					continue
				}
				input, _ := b["input"].(map[string]any)
				desc := SummarizeToolInput("Bash", input)
				if bg, _ := input["run_in_background"].(bool); bg {
					t := get(id)
					t.Description, t.StartedAt = desc, ts
				} else {
					foreground[id] = bashCall{desc, ts}
				}
			}
		case "user":
			for _, b := range blocks(e) {
				if str(b["type"]) != "tool_result" {
					continue
				}
				m := bgTaskIDRe.FindStringSubmatch(resultText(b))
				if m == nil {
					continue
				}
				t := get(str(b["tool_use_id"]))
				t.TaskID = m[1]
				if t.StartedAt == 0 {
					if fg, ok := foreground[t.ToolUseID]; ok {
						t.Description, t.StartedAt = fg.desc, fg.at // timed out and moved: it started with the call
					} else {
						t.StartedAt = ts
					}
				}
			}
		}
		for _, m := range bgTaskNoteRe.FindAllStringSubmatch(notificationText(e), -1) {
			t := get(m[2]) // may be an orphan (start scrolled out of the tail)
			t.TaskID = m[1]
			t.State = m[3]
			if t.EndedAt == 0 || ts > t.EndedAt {
				t.EndedAt = ts
			}
		}
	}

	out := make([]model.BackgroundTask, 0, len(order))
	for _, id := range order {
		out = append(out, *byID[id])
	}
	return out
}

// MergeTasks folds one poll's findings into what earlier polls knew about the session, so a task
// whose start has scrolled out of the tail is still listed while it runs, and its notification still
// lands on it. Finished tasks are forgotten finishedTaskTTL after they ended; orphans (no start known
// anywhere) are dropped.
func MergeTasks(prev, found []model.BackgroundTask, now int64) []model.BackgroundTask {
	byID := map[string]*model.BackgroundTask{}
	merged := make([]model.BackgroundTask, 0, len(prev)+len(found))
	for _, t := range prev {
		merged = append(merged, t)
		byID[t.ToolUseID] = &merged[len(merged)-1]
	}
	for _, f := range found {
		if cur, ok := byID[f.ToolUseID]; ok {
			if f.TaskID != "" {
				cur.TaskID = f.TaskID
			}
			if f.Description != "" {
				cur.Description = f.Description
			}
			if f.StartedAt != 0 && (cur.StartedAt == 0 || f.StartedAt < cur.StartedAt) {
				cur.StartedAt = f.StartedAt
			}
			if f.EndedAt != 0 {
				cur.State, cur.EndedAt = f.State, f.EndedAt
			}
			continue
		}
		if f.StartedAt == 0 {
			continue // an ending for a task we never saw start
		}
		merged = append(merged, f)
		byID[f.ToolUseID] = &merged[len(merged)-1]
	}
	out := merged[:0]
	for _, t := range merged {
		if t.State != "running" && t.EndedAt != 0 && now-t.EndedAt > finishedTaskTTL {
			continue
		}
		out = append(out, t)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].StartedAt < out[j].StartedAt })
	return out
}

// mergeTasks runs MergeTasks against the collector's memory of this session.
func (c *Collector) mergeTasks(sid string, found []model.BackgroundTask, now int64) []model.BackgroundTask {
	c.mu.Lock()
	defer c.mu.Unlock()
	merged := MergeTasks(c.taskCache[sid], found, now)
	if len(merged) == 0 {
		delete(c.taskCache, sid)
	} else {
		c.taskCache[sid] = merged
	}
	return merged
}

// pruneTaskCache forgets sessions that are no longer live.
func (c *Collector) pruneTaskCache(live map[string]bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for sid := range c.taskCache {
		if !live[sid] {
			delete(c.taskCache, sid)
		}
	}
}
