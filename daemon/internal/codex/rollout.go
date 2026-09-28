// Package codex reads what OpenAI's Codex CLI leaves on disk and drives it over its app-server
// protocol, so Codex threads show up beside Claude Code sessions.
//
// Sources:
//
//	~/.codex/sessions/YYYY/MM/DD/rollout-<ts>-<thread>.jsonl  one transcript ("rollout") per thread
//	~/.codex/thread-writer-locks/<thread>.lock                 flock-held while a Codex process has the thread loaded
//
// A rollout line is {"timestamp":…,"type":…,"payload":{…}}. Types: session_meta (cwd, originator,
// cli_version, git), turn_context (model, effort, approval_policy, sandbox_policy), response_item
// (the model's own items: message, reasoning, function_call, function_call_output, custom_tool_call,
// …), event_msg (task_started, task_complete, token_count, item_completed, …), compacted,
// world_state and token_usage_record. Verified against Codex 0.158.
package codex

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"strings"
	"time"
)

// Line is one parsed rollout line. Offset is its byte position in the file, which doubles as a
// stable id for the entries a viewer sees.
type Line struct {
	Offset  int64
	Time    int64 // epoch ms, 0 when the line has no timestamp
	Type    string
	Payload map[string]any
}

// MaxTailBytes bounds how much of a rollout is read per poll; tool outputs can make lines huge.
const MaxTailBytes = 400_000

func str(v any) string {
	s, _ := v.(string)
	return s
}

func num(v any) (float64, bool) {
	f, ok := v.(float64)
	return f, ok
}

func obj(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func parseTime(s string) int64 {
	if s == "" {
		return 0
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return 0
	}
	return t.UnixMilli()
}

// ParseLine decodes one rollout line; ok is false for blank or malformed lines.
func ParseLine(b []byte, offset int64) (Line, bool) {
	b = bytes.TrimSpace(b)
	if len(b) == 0 {
		return Line{}, false
	}
	var v struct {
		Timestamp string          `json:"timestamp"`
		Type      string          `json:"type"`
		Payload   json.RawMessage `json:"payload"`
	}
	if json.Unmarshal(b, &v) != nil || v.Type == "" {
		return Line{}, false
	}
	l := Line{Offset: offset, Time: parseTime(v.Timestamp), Type: v.Type}
	if len(v.Payload) > 0 {
		_ = json.Unmarshal(v.Payload, &l.Payload)
	}
	return l, true
}

// splitLines cuts a buffer that starts at file offset base into parsed lines with absolute offsets.
// A final fragment without a newline is left out (it is still being written); its start is
// returned as next so a caller can continue from there.
func splitLines(buf []byte, base int64) (lines []Line, next int64) {
	pos := 0
	for {
		i := bytes.IndexByte(buf[pos:], '\n')
		if i < 0 {
			return lines, base + int64(pos)
		}
		if l, ok := ParseLine(buf[pos:pos+i], base+int64(pos)); ok {
			lines = append(lines, l)
		}
		pos += i + 1
	}
}

// ReadTail returns the last n complete lines of a rollout, reading at most maxBytes from its end,
// plus the file size at read time.
func ReadTail(path string, n int, maxBytes int64) ([]Line, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, 0, err
	}
	size := st.Size()
	start := int64(0)
	if size > maxBytes {
		start = size - maxBytes
	}
	buf := make([]byte, size-start)
	if _, err := f.ReadAt(buf, start); err != nil && err != io.EOF {
		return nil, 0, err
	}
	if start > 0 {
		// The first piece is almost certainly a cut line; skip to the next newline.
		if i := bytes.IndexByte(buf, '\n'); i >= 0 {
			buf = buf[i+1:]
			start += int64(i + 1)
		} else {
			return nil, size, nil
		}
	}
	lines, _ := splitLines(buf, start)
	if n > 0 && len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines, size, nil
}

// ReadFrom returns every complete line written after byte offset from, the offset just past the
// last one, and the file size. It reads at most maxBytes.
func ReadFrom(path string, from int64, maxBytes int64) (lines []Line, next int64, size int64, err error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, from, 0, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, from, 0, err
	}
	size = st.Size()
	if from >= size {
		return nil, from, size, nil
	}
	end := size
	if end-from > maxBytes {
		end = from + maxBytes
	}
	buf := make([]byte, end-from)
	if _, err := f.ReadAt(buf, from); err != nil && err != io.EOF {
		return nil, from, size, err
	}
	lines, next = splitLines(buf, from)
	return lines, next, size, nil
}

// ---- payload helpers -----------------------------------------------------------------------------

// eventType is the event_msg's own type ("task_started", …), or "" for other line types.
func (l Line) eventType() string {
	if l.Type != "event_msg" {
		return ""
	}
	return str(l.Payload["type"])
}

// itemType is a response_item's type ("message", "function_call", …), or "" for other line types.
func (l Line) itemType() string {
	if l.Type != "response_item" {
		return ""
	}
	return str(l.Payload["type"])
}

// messageText joins the text parts of a response_item message.
func messageText(p map[string]any) string {
	arr, _ := p["content"].([]any)
	var parts []string
	for _, c := range arr {
		cm := obj(c)
		switch str(cm["type"]) {
		case "input_text", "output_text", "text":
			if t := str(cm["text"]); t != "" {
				parts = append(parts, t)
			}
		}
	}
	return strings.Join(parts, "\n")
}

// isMetaPrompt reports whether a user message is context Codex injects rather than something the
// user typed: <environment_context>, <recommended_plugins>, <user_instructions>, AGENTS.md wrappers…
func isMetaPrompt(text string) bool {
	t := strings.TrimSpace(text)
	if !strings.HasPrefix(t, "<") {
		return false
	}
	end := strings.IndexAny(t, "> \n")
	if end < 0 {
		return false
	}
	tag := strings.TrimPrefix(t[1:end], "/")
	if tag == "" || strings.ContainsAny(tag, "<>\"'=") {
		return false
	}
	// A wrapped block carries its own closing tag; Codex sometimes appends a note after it.
	return strings.Contains(t, "</"+tag+">")
}

// outputText flattens a tool output, which is a string or a list of {type,text} parts.
func outputText(v any) string {
	switch o := v.(type) {
	case string:
		return o
	case []any:
		var parts []string
		for _, c := range o {
			if t := str(obj(c)["text"]); t != "" {
				parts = append(parts, t)
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}
