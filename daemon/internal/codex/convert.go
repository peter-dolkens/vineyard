package codex

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Convert turns rollout lines into the transcript entries the chat views already render for
// Claude Code: user and assistant messages with text, thinking, tool_use and tool_result blocks,
// plus a compact_boundary system line. Each entry's uuid is "codex:<offset>", stable across reads,
// so a viewer that re-reads a tail shows nothing twice.
//
// Lines that would only add noise are dropped: developer messages, the context blocks Codex
// injects as user messages, event duplicates of items (user_message, agent_message, item_completed)
// and reasoning whose summary is empty (Codex keeps the content encrypted).
func Convert(lines []Line) []json.RawMessage {
	out := make([]json.RawMessage, 0, len(lines))
	model := ""
	emit := func(v map[string]any) {
		if b, err := json.Marshal(v); err == nil {
			out = append(out, b)
		}
	}
	stamp := func(l Line) string {
		if l.Time == 0 {
			return ""
		}
		return time.UnixMilli(l.Time).UTC().Format(time.RFC3339Nano)
	}
	entry := func(l Line, typ string, message map[string]any) map[string]any {
		return map[string]any{"type": typ, "uuid": fmt.Sprintf("codex:%d", l.Offset), "timestamp": stamp(l), "message": message, "provider": Provider}
	}
	for _, l := range lines {
		p := l.Payload
		switch l.Type {
		case "turn_context":
			if s := str(p["model"]); s != "" {
				model = s
			}
		case "compacted":
			emit(map[string]any{"type": "system", "subtype": "compact_boundary", "uuid": fmt.Sprintf("codex:%d", l.Offset), "timestamp": stamp(l), "content": "Conversation compacted", "level": "info", "provider": Provider})
		case "event_msg":
			switch l.eventType() {
			case "thread_settings_applied":
				if s := str(obj(p["thread_settings"])["model"]); s != "" {
					model = s
				}
			case "token_count":
				// Usage on an otherwise empty assistant entry feeds the viewer's token counters. Codex
				// reports what was cached; the cache clock stays off because there is no cache_creation.
				last := obj(obj(p["info"])["last_token_usage"])
				if last == nil {
					continue
				}
				input, _ := num(last["input_tokens"])
				cached, _ := num(last["cached_input_tokens"])
				output, _ := num(last["output_tokens"])
				usage := map[string]any{"input_tokens": input - cached, "cache_read_input_tokens": cached, "cache_creation_input_tokens": 0, "output_tokens": output}
				emit(entry(l, "assistant", map[string]any{"role": "assistant", "model": model, "content": []any{}, "usage": usage}))
			case "turn_aborted":
				emit(map[string]any{"type": "system", "subtype": "turn_aborted", "uuid": fmt.Sprintf("codex:%d", l.Offset), "timestamp": stamp(l), "content": "Turn interrupted", "level": "error", "provider": Provider})
			}
		case "response_item":
			switch l.itemType() {
			case "message":
				text := messageText(p)
				switch str(p["role"]) {
				case "user":
					if text == "" || isMetaPrompt(text) {
						continue
					}
					content := []any{map[string]any{"type": "text", "text": text}}
					for _, c := range imageBlocks(p) {
						content = append([]any{c}, content...)
					}
					emit(entry(l, "user", map[string]any{"role": "user", "content": content}))
				case "assistant":
					if strings.TrimSpace(text) == "" {
						continue
					}
					emit(entry(l, "assistant", map[string]any{"role": "assistant", "model": model, "content": []any{map[string]any{"type": "text", "text": text}}}))
				}
			case "reasoning":
				var parts []string
				if arr, ok := p["summary"].([]any); ok {
					for _, s := range arr {
						if t := str(obj(s)["text"]); t != "" {
							parts = append(parts, t)
						} else if t := str(s); t != "" {
							parts = append(parts, t)
						}
					}
				}
				if arr, ok := p["content"].([]any); ok && len(parts) == 0 {
					for _, s := range arr {
						if t := str(obj(s)["text"]); t != "" {
							parts = append(parts, t)
						}
					}
				}
				if len(parts) == 0 {
					continue
				}
				emit(entry(l, "assistant", map[string]any{"role": "assistant", "model": model, "content": []any{map[string]any{"type": "thinking", "thinking": strings.Join(parts, "\n\n")}}}))
			case "compaction":
				emit(map[string]any{"type": "system", "subtype": "compact_boundary", "uuid": fmt.Sprintf("codex:%d", l.Offset), "timestamp": stamp(l), "content": "Conversation compacted", "level": "info", "provider": Provider})
			default:
				if c, ok := callOf(l); ok {
					emit(entry(l, "assistant", map[string]any{"role": "assistant", "model": model, "content": []any{map[string]any{"type": "tool_use", "id": c.ID, "name": c.Name, "input": c.Input}}}))
				} else if id, text, isErr, ok := outputOf(l); ok {
					emit(entry(l, "user", map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": id, "content": text, "is_error": isErr}}}))
				}
			}
		}
	}
	return out
}

// imageBlocks turns the input_image parts of a user message that carry a data URL into image blocks.
func imageBlocks(p map[string]any) []map[string]any {
	arr, _ := p["content"].([]any)
	var out []map[string]any
	for _, c := range arr {
		cm := obj(c)
		if str(cm["type"]) != "input_image" {
			continue
		}
		url := str(cm["image_url"])
		if !strings.HasPrefix(url, "data:") {
			continue
		}
		semi, comma := strings.Index(url, ";"), strings.Index(url, ",")
		if semi < 0 || comma < 0 || comma < semi {
			continue
		}
		out = append(out, map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": url[5:semi], "data": url[comma+1:]}})
	}
	return out
}
