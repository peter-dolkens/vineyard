// Package agenttools is `vineyardd mcp`: the MCP server that gives the sessions Vineyard starts tools
// for the fleet (machines, workspaces, sessions, transcripts, other agents), each turned on by a
// fleet setting. Claude Code runs it over stdio, one per session; it talks to the local daemon as a
// viewer. See "Agent tools" in docs/DESIGN.md.
package agenttools

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"slices"
	"sync"
	"time"

	"github.com/peter-dolkens/vineyard/daemon/internal/mesh"
)

// ServerName is the name sessions know the server by: its tools are mcp__vineyard__<tool>.
const ServerName = "vineyard"

// pollSettings is how often the server looks for a changed setting, to tell the session its tool
// list changed.
var pollSettings = 3 * time.Second

const instructions = `This session runs inside a Vineyard fleet: Vineyard watches and runs Claude Code sessions across the user's machines. ` +
	`Use these tools when the work involves other machines or other agents: to find a machine that has some program (find_machines), ` +
	`see what other sessions are doing, read another session's transcript (fetch_transcript saves it to a file and tells you the path), ` +
	`or hand work to an agent on another machine and get its reply (start_agent / message_agent; the reply arrives as a message in this conversation, or use await_agent). ` +
	`Messages you send to other agents are labelled as coming from you, not from the user.`

// Options configures a server.
type Options struct {
	// Machine is this machine's id; Session the Claude Code session the server runs for ("" when
	// started by hand, which turns off pushed replies).
	Machine string
	Session string
	Version string
	// Dial opens a viewer link to the local daemon.
	Dial func() (*mesh.Conn, error)
	// Enabled reads which tools the fleet setting turns on (model.AgentTool*), from config.json.
	Enabled func() []string
	// TranscriptDir is where fetch_transcript saves files.
	TranscriptDir string
	Log           *log.Logger
}

type Server struct {
	o       Options
	client  *Client
	watches *watches

	wmu sync.Mutex
	out io.Writer

	mu          sync.Mutex
	initialized bool
	listed      []string // tool names last sent in tools/list
}

func New(o Options) *Server {
	if o.Log == nil {
		o.Log = log.New(io.Discard, "", 0)
	}
	s := &Server{o: o, client: NewClient(o.Dial)}
	s.watches = newWatches(s)
	return s
}

type rpcMsg struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// Serve answers MCP requests from in, one JSON message per line, until in ends.
func (s *Server) Serve(in io.Reader, out io.Writer) error {
	s.out = out
	go s.watchSettings()
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	for sc.Scan() {
		line := append([]byte(nil), sc.Bytes()...)
		var m rpcMsg
		if err := json.Unmarshal(line, &m); err != nil {
			s.send(map[string]any{"jsonrpc": "2.0", "id": nil, "error": rpcError{-32700, "parse error"}})
			continue
		}
		if m.Method == "" {
			continue // a response to something we never ask
		}
		// Tool calls can block (await_agent), so each request is answered on its own goroutine.
		go s.dispatch(m)
	}
	return sc.Err()
}

func (s *Server) send(v any) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	s.wmu.Lock()
	defer s.wmu.Unlock()
	_, _ = s.out.Write(append(b, '\n'))
}

func (s *Server) reply(id json.RawMessage, result any) {
	s.send(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

func (s *Server) fail(id json.RawMessage, code int, msg string) {
	s.send(map[string]any{"jsonrpc": "2.0", "id": id, "error": rpcError{code, msg}})
}

func (s *Server) dispatch(m rpcMsg) {
	notification := len(m.ID) == 0
	switch m.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(m.Params, &p)
		version := p.ProtocolVersion
		if version == "" {
			version = "2025-06-18"
		}
		s.reply(m.ID, map[string]any{
			"protocolVersion": version,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": true}},
			"serverInfo":      map[string]any{"name": ServerName, "title": "Vineyard", "version": s.o.Version},
			"instructions":    instructions,
		})
	case "notifications/initialized":
		s.mu.Lock()
		s.initialized = true
		s.mu.Unlock()
	case "ping":
		s.reply(m.ID, map[string]any{})
	case "tools/list":
		tools := s.enabledTools()
		names := make([]string, 0, len(tools))
		defs := make([]map[string]any, 0, len(tools))
		for _, t := range tools {
			names = append(names, t.name)
			defs = append(defs, t.definition())
		}
		s.mu.Lock()
		s.listed = names
		s.mu.Unlock()
		s.reply(m.ID, map[string]any{"tools": defs})
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(m.Params, &p); err != nil {
			s.fail(m.ID, -32602, "bad params")
			return
		}
		s.reply(m.ID, s.call(p.Name, p.Arguments))
	default:
		if !notification {
			s.fail(m.ID, -32601, "method not found: "+m.Method)
		}
	}
}

// call runs one tool and shapes its answer as an MCP tool result.
func (s *Server) call(name string, args json.RawMessage) map[string]any {
	var t *tool
	for _, x := range s.enabledTools() {
		if x.name == name {
			t = x
			break
		}
	}
	if t == nil {
		return toolError(fmt.Sprintf("%s is not available: the fleet setting that turns it on is off (vineyard.agentTools in VS Code)", name))
	}
	if len(args) == 0 || string(args) == "null" {
		args = json.RawMessage("{}")
	}
	out, err := t.run(s, args)
	if err != nil {
		return toolError(err.Error())
	}
	text, ok := out.(string)
	if !ok {
		b, err := json.MarshalIndent(out, "", " ")
		if err != nil {
			return toolError(err.Error())
		}
		text = string(b)
	}
	return map[string]any{"content": []map[string]any{{"type": "text", "text": text}}}
}

func toolError(msg string) map[string]any {
	return map[string]any{"content": []map[string]any{{"type": "text", "text": msg}}, "isError": true}
}

// enabledTools are the tools whose setting is on.
func (s *Server) enabledTools() []*tool {
	on := s.o.Enabled()
	var out []*tool
	for _, t := range allTools {
		if slices.Contains(on, t.setting) {
			out = append(out, t)
		}
	}
	return out
}

// watchSettings tells the session when the fleet setting changed which tools there are.
func (s *Server) watchSettings() {
	for {
		time.Sleep(pollSettings)
		var names []string
		for _, t := range s.enabledTools() {
			names = append(names, t.name)
		}
		s.mu.Lock()
		changed := s.initialized && s.listed != nil && !slices.Equal(names, s.listed)
		if changed {
			s.listed = names // asked again after the notification; do not repeat it meanwhile
		}
		s.mu.Unlock()
		if changed {
			s.o.Log.Printf("tools changed: %v", names)
			s.send(map[string]any{"jsonrpc": "2.0", "method": "notifications/tools/list_changed"})
		}
	}
}
