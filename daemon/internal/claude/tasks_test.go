package claude

import (
	"testing"

	"github.com/peter-dolkens/vineyard/daemon/internal/model"
)

func bashUse(id, desc, ts string, background bool) map[string]any {
	input := map[string]any{"command": "sleep 1", "description": desc}
	if background {
		input["run_in_background"] = true
	}
	return map[string]any{"type": "assistant", "timestamp": ts, "message": map[string]any{"role": "assistant", "content": []any{
		map[string]any{"type": "tool_use", "id": id, "name": "Bash", "input": input},
	}}}
}

func bgResult(id, text, ts string) map[string]any {
	return map[string]any{"type": "user", "timestamp": ts, "message": map[string]any{"role": "user", "content": []any{
		map[string]any{"type": "tool_result", "tool_use_id": id, "content": text},
	}}}
}

func notification(taskID, toolUseID, status, ts string) map[string]any {
	return map[string]any{"type": "queue-operation", "operation": "enqueue", "timestamp": ts,
		"content": "<task-notification>\n<task-id>" + taskID + "</task-id>\n<tool-use-id>" + toolUseID + "</tool-use-id>\n<output-file>/tmp/x</output-file>\n<status>" + status + "</status>\n<summary>done</summary>\n</task-notification>"}
}

func TestBuildTasks(t *testing.T) {
	entries := []map[string]any{
		bashUse("tu1", "Watch the release run", "2026-09-23T04:00:00Z", true),
		bgResult("tu1", "Command running in background with ID: abc123. Output is being written to: /tmp/abc123.output.", "2026-09-23T04:00:01Z"),
		bashUse("tu2", "Rebuild and re-run the smoke test", "2026-09-23T04:05:00Z", false),
		bgResult("tu2", "Command did not complete within its 120s timeout and was moved to the background (ID: def456). Output is being written to: /tmp/def456.output.", "2026-09-23T04:07:00Z"),
		bashUse("tu3", "Plain foreground command", "2026-09-23T04:08:00Z", false),
		bgResult("tu3", "ok", "2026-09-23T04:08:01Z"),
		// A sidechain line never counts.
		func() map[string]any {
			e := bashUse("side", "subagent bash", "2026-09-23T04:08:30Z", true)
			e["isSidechain"] = true
			return e
		}(),
		notification("def456", "tu2", "failed", "2026-09-23T04:09:00Z"),
		// The attachment form of the same notification adds nothing new.
		{"type": "attachment", "timestamp": "2026-09-23T04:09:00Z", "attachment": map[string]any{"type": "queued_command", "prompt": "<task-notification><task-id>def456</task-id><tool-use-id>tu2</tool-use-id><status>failed</status></task-notification>"}},
		// An ending for something that started before the tail.
		notification("old1", "tu0", "completed", "2026-09-23T04:09:30Z"),
	}
	got := BuildTasks(entries)
	if len(got) != 3 {
		t.Fatalf("want 3 tasks (2 started, 1 orphan ending), got %d: %+v", len(got), got)
	}
	a, b, orphan := got[0], got[1], got[2]
	if a.ToolUseID != "tu1" || a.TaskID != "abc123" || a.State != "running" || a.Description != "Watch the release run" || a.StartedAt == 0 || a.EndedAt != 0 || a.Kind != "shell" {
		t.Errorf("background call: %+v", a)
	}
	if b.ToolUseID != "tu2" || b.TaskID != "def456" || b.State != "failed" || b.Description != "Rebuild and re-run the smoke test" {
		t.Errorf("timed-out call: %+v", b)
	}
	if want := entryTime(entries[2]); b.StartedAt != want {
		t.Errorf("a moved command started with its call: got %d want %d", b.StartedAt, want)
	}
	if b.EndedAt != entryTime(entries[7]) {
		t.Errorf("ended at the notification: %+v", b)
	}
	if orphan.ToolUseID != "tu0" || orphan.StartedAt != 0 || orphan.State != "completed" || orphan.TaskID != "old1" {
		t.Errorf("orphan ending: %+v", orphan)
	}
	if len(BuildTasks(nil)) != 0 {
		t.Error("no entries, no tasks")
	}
}

func TestMergeTasks(t *testing.T) {
	now := int64(10_000_000)
	prev := []model.BackgroundTask{
		{ToolUseID: "tu0", TaskID: "old1", Kind: "shell", State: "running", Description: "Started long ago", StartedAt: now - 3_600_000},
		{ToolUseID: "tuX", Kind: "shell", State: "completed", StartedAt: now - 7_200_000, EndedAt: now - finishedTaskTTL - 1},
		{ToolUseID: "tuY", Kind: "shell", State: "completed", StartedAt: now - 7_200_000, EndedAt: now - 60_000},
	}
	found := []model.BackgroundTask{
		{ToolUseID: "tu0", TaskID: "old1", State: "completed", EndedAt: now - 1000}, // orphan ending lands on the remembered start
		{ToolUseID: "tu9", State: "failed", EndedAt: now - 500},                     // orphan with nothing to land on
		{ToolUseID: "tu5", Kind: "shell", State: "running", Description: "New", StartedAt: now - 2000},
	}
	got := MergeTasks(prev, found, now)
	ids := make([]string, 0, len(got))
	for _, g := range got {
		ids = append(ids, g.ToolUseID)
	}
	if len(got) != 3 || ids[0] != "tuY" || ids[1] != "tu0" || ids[2] != "tu5" {
		t.Fatalf("want [tuY tu0 tu5] (expired tuX and orphan tu9 dropped, start order), got %v", ids)
	}
	if got[1].State != "completed" || got[1].EndedAt != now-1000 || got[1].Description != "Started long ago" {
		t.Errorf("remembered task should take the ending and keep its description: %+v", got[1])
	}
	if same := MergeTasks(nil, nil, now); len(same) != 0 {
		t.Errorf("nothing in, nothing out: %v", same)
	}
}
