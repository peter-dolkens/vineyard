package agenttools

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/peter-dolkens/vineyard/daemon/internal/protocol"
)

// pageLines asks for as many lines as fit in the daemon's 8 MB window per request.
const pageLines = 1 << 20

// entry is the part of a transcript line the tools read.
type entry struct {
	Type        string `json:"type"`
	IsMeta      bool   `json:"isMeta"`
	IsSidechain bool   `json:"isSidechain"`
	Timestamp   string `json:"timestamp"`
	Message     struct {
		Role       string          `json:"role"`
		Content    json.RawMessage `json:"content"`
		StopReason string          `json:"stop_reason"`
	} `json:"message"`
}

type block struct {
	Type    string          `json:"type"`
	Text    string          `json:"text"`
	Name    string          `json:"name"`
	Input   json.RawMessage `json:"input"`
	IsError bool            `json:"is_error"`
	Content json.RawMessage `json:"content"`
}

func parseEntry(raw []byte) (entry, []block, bool) {
	var e entry
	if json.Unmarshal(raw, &e) != nil {
		return e, nil, false
	}
	if len(e.Message.Content) == 0 {
		return e, nil, true
	}
	var s string
	if json.Unmarshal(e.Message.Content, &s) == nil {
		return e, []block{{Type: "text", Text: s}}, true
	}
	var bs []block
	_ = json.Unmarshal(e.Message.Content, &bs)
	return e, bs, true
}

// isPrompt is a user line a person (or another agent) wrote, not a tool result or Claude Code's own.
func isPrompt(e entry, bs []block) bool {
	if e.Type != "user" || e.IsMeta || e.IsSidechain {
		return false
	}
	for _, b := range bs {
		if b.Type == "tool_result" {
			return false
		}
	}
	return len(bs) > 0
}

func isToolResult(e entry, bs []block) bool {
	if e.Type != "user" {
		return false
	}
	for _, b := range bs {
		if b.Type == "tool_result" {
			return true
		}
	}
	return false
}

func texts(bs []block) string {
	var parts []string
	for _, b := range bs {
		if b.Type == "text" && strings.TrimSpace(b.Text) != "" {
			parts = append(parts, strings.TrimSpace(b.Text))
		}
	}
	return strings.Join(parts, "\n\n")
}

var unsafeName = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

func safeName(s string) string {
	s = unsafeName.ReplaceAllString(s, "_")
	if s == "" || s == "." || s == ".." {
		s = "_"
	}
	return s
}

type transcriptMeta struct {
	Machine string `json:"machine"`
	Session string `json:"session"`
	Path    string `json:"path"` // on the machine it came from
	Offset  int64  `json:"offset"`
	Fetched string `json:"fetched"`
}

// locate finds where a session's transcript is on its machine: from the live session if there is
// one, else from the workspace given.
func (s *Server) locate(machine, session, workspace string) (id, name string, args protocol.TranscriptArgs, err error) {
	e, err := s.client.Entry(machine)
	if err != nil {
		return "", "", args, err
	}
	id, name = e.Snapshot.MachineID, e.Snapshot.Name
	args.SessionID = session
	for _, ag := range e.Snapshot.Agents {
		if ag.SessionID == session {
			if ag.TranscriptPath != "" {
				args.Path = ag.TranscriptPath
			}
			args.Cwd = ag.Cwd
			if args.Cwd == "" {
				args.Cwd = ag.WorkspacePath
			}
			if workspace == "" {
				workspace = args.Cwd
			}
		}
	}
	if args.Path == "" {
		if workspace == "" {
			return "", "", args, fmt.Errorf("session %s is not live on %s: give its workspace (list_sessions with include_past shows it)", session, name)
		}
		args.Cwd = workspace
	}
	return id, name, args, nil
}

func (s *Server) readTranscript(target string, a protocol.TranscriptArgs) (protocol.TranscriptData, error) {
	raw, err := s.client.Request(target, "transcript", a, 60*time.Second)
	if err != nil {
		return protocol.TranscriptData{}, err
	}
	var d protocol.TranscriptData
	if err := json.Unmarshal(raw, &d); err != nil {
		return d, err
	}
	return d, nil
}

// downloadAll reads a whole transcript, newest page first, paging back until the start.
func (s *Server) downloadAll(target string, a protocol.TranscriptArgs) ([]json.RawMessage, int64, string, error) {
	a.Lines, a.Offset, a.Before = pageLines, 0, 0
	d, err := s.readTranscript(target, a)
	if err != nil {
		return nil, 0, "", err
	}
	pages := [][]json.RawMessage{d.Entries}
	end, path := d.Offset, d.Path
	start := d.Start
	for start > 0 {
		a.Before = start
		p, err := s.readTranscript(target, a)
		if err != nil {
			return nil, 0, "", err
		}
		if p.Truncated {
			return nil, 0, "", errors.New("the transcript was replaced while downloading; try again")
		}
		if p.Start >= start {
			break // nothing further back could be read
		}
		pages = append(pages, p.Entries)
		start = p.Start
	}
	var all []json.RawMessage
	for i := len(pages) - 1; i >= 0; i-- {
		all = append(all, pages[i]...)
	}
	return all, end, path, nil
}

func (s *Server) fetchTranscript(args json.RawMessage) (any, error) {
	var a struct {
		Machine   string `json:"machine"`
		SessionID string `json:"session_id"`
		Workspace string `json:"workspace"`
	}
	if err := decode(args, &a); err != nil {
		return nil, err
	}
	if a.Machine == "" || a.SessionID == "" {
		return nil, errors.New("machine and session_id are required")
	}
	id, name, targs, err := s.locate(a.Machine, a.SessionID, a.Workspace)
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(s.o.TranscriptDir, safeName(name))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	base := filepath.Join(dir, safeName(a.SessionID))
	jsonlPath, mdPath, metaPath := base+".jsonl", base+".md", base+".meta.json"

	var meta transcriptMeta
	if b, err := os.ReadFile(metaPath); err == nil {
		_ = json.Unmarshal(b, &meta)
	}
	_, statErr := os.Stat(jsonlPath)
	added := 0
	mode := "downloaded"
	if meta.Offset > 0 && statErr == nil {
		targs.Offset, targs.Lines = meta.Offset, pageLines
		if meta.Path != "" {
			targs.Path = meta.Path
		}
		var fresh []json.RawMessage
		truncated := false
		for {
			d, err := s.readTranscript(id, targs)
			if err != nil {
				return nil, err
			}
			if d.Truncated {
				truncated = true
				break
			}
			fresh = append(fresh, d.Entries...)
			if d.Offset <= targs.Offset || d.Offset >= d.Size {
				targs.Offset = max(d.Offset, targs.Offset)
				break
			}
			targs.Offset = d.Offset // more than one window was written since: keep going
		}
		if truncated {
			meta.Offset = 0 // replaced on the machine: start over below
		} else {
			f, err := os.OpenFile(jsonlPath, os.O_APPEND|os.O_WRONLY, 0o600)
			if err != nil {
				return nil, err
			}
			for _, e := range fresh {
				_, _ = f.Write(append([]byte(e), '\n'))
			}
			if err := f.Close(); err != nil {
				return nil, err
			}
			added, meta.Offset, mode = len(fresh), targs.Offset, "updated"
		}
	}
	if mode == "downloaded" {
		targs.Offset = 0
		entries, end, path, err := s.downloadAll(id, targs)
		if err != nil {
			return nil, err
		}
		var buf bytes.Buffer
		for _, e := range entries {
			buf.Write(e)
			buf.WriteByte('\n')
		}
		if err := os.WriteFile(jsonlPath, buf.Bytes(), 0o600); err != nil {
			return nil, err
		}
		added, meta.Offset, meta.Path = len(entries), end, path
	}
	meta.Machine, meta.Session, meta.Fetched = name, a.SessionID, time.Now().UTC().Format(time.RFC3339)
	if b, err := json.MarshalIndent(meta, "", "  "); err == nil {
		_ = os.WriteFile(metaPath, b, 0o600)
	}
	sum, err := renderMarkdown(jsonlPath, mdPath, meta)
	if err != nil {
		return nil, err
	}
	st, _ := os.Stat(jsonlPath)
	var size int64
	if st != nil {
		size = st.Size()
	}
	return map[string]any{
		"markdown":     mdPath,
		"jsonl":        jsonlPath,
		"mode":         mode,
		"newEntries":   added,
		"entries":      sum.entries,
		"prompts":      sum.prompts,
		"jsonlBytes":   size,
		"firstPrompt":  clip(sum.first, 200),
		"lastActivity": sum.last,
		"hint":         "Read the .md file (prompts, replies and one line per tool call); the .jsonl has every detail. Call fetch_transcript again later to pull only what is new.",
	}, nil
}

type mdSummary struct {
	entries, prompts int
	first, last      string
}

// renderMarkdown writes a condensed, readable transcript: prompts and replies in full, one line per
// tool call, tool errors noted, thinking and tool output left out.
func renderMarkdown(jsonlPath, mdPath string, meta transcriptMeta) (mdSummary, error) {
	var sum mdSummary
	f, err := os.Open(jsonlPath)
	if err != nil {
		return sum, err
	}
	defer f.Close()
	var out bytes.Buffer
	fmt.Fprintf(&out, "# Transcript %s\n\nMachine: %s · fetched %s · source %s\n", meta.Session, meta.Machine, meta.Fetched, meta.Path)
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	lastRole := ""
	for sc.Scan() {
		e, bs, ok := parseEntry(sc.Bytes())
		if !ok {
			continue
		}
		sum.entries++
		if e.Timestamp != "" {
			sum.last = e.Timestamp
		}
		if e.IsSidechain {
			continue
		}
		switch {
		case isPrompt(e, bs):
			t := texts(bs)
			if t == "" {
				continue
			}
			sum.prompts++
			if sum.first == "" {
				sum.first = t
			}
			fmt.Fprintf(&out, "\n## User%s\n\n%s\n", stamp(e.Timestamp), t)
			lastRole = "user"
		case isToolResult(e, bs):
			for _, b := range bs {
				if b.Type == "tool_result" && b.IsError {
					fmt.Fprintf(&out, "  - ↳ error: %s\n", clip(firstLine(resultText(b.Content)), 200))
				}
			}
		case e.Type == "assistant":
			for _, b := range bs {
				switch b.Type {
				case "text":
					if strings.TrimSpace(b.Text) == "" {
						continue
					}
					if lastRole != "assistant" {
						fmt.Fprintf(&out, "\n## Assistant%s\n\n", stamp(e.Timestamp))
					}
					fmt.Fprintf(&out, "%s\n\n", strings.TrimSpace(b.Text))
					lastRole = "assistant"
				case "tool_use":
					if lastRole != "assistant" {
						fmt.Fprintf(&out, "\n## Assistant%s\n\n", stamp(e.Timestamp))
						lastRole = "assistant"
					}
					fmt.Fprintf(&out, "- `%s` %s\n", b.Name, clip(inputSummary(b.Input), 200))
				}
			}
		}
	}
	if err := sc.Err(); err != nil {
		return sum, err
	}
	return sum, os.WriteFile(mdPath, out.Bytes(), 0o600)
}

func stamp(ts string) string {
	if ts == "" {
		return ""
	}
	return " · " + ts
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func resultText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var bs []block
	_ = json.Unmarshal(raw, &bs)
	return texts(bs)
}

// inputSummary is a tool call's input on one line: the field that says the most, or compact JSON.
func inputSummary(raw json.RawMessage) string {
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return ""
	}
	for _, k := range []string{"command", "file_path", "path", "pattern", "url", "query", "description", "prompt"} {
		if v, ok := m[k].(string); ok && v != "" {
			return strings.ReplaceAll(v, "\n", " ")
		}
	}
	b, _ := json.Marshal(m)
	return string(b)
}
