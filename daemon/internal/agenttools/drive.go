package agenttools

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/peter-dolkens/vineyard/daemon/internal/model"
	"github.com/peter-dolkens/vineyard/daemon/internal/protocol"
)

const (
	// watchFor is how long a reply is waited for before the watch is given up.
	watchFor = 2 * time.Hour
	// watchTick re-checks a watched session this often even when no snapshot arrives (a dropped
	// link, a machine that only changes its transcript).
	watchTick = 5 * time.Second
	// replyLimit cuts a reply pushed into the conversation or returned by await_agent.
	replyLimit = 8000
	// tailLines is how much of a transcript's end is read to find a reply.
	tailLines = 400
)

// Reply is what an agent said when its turn ended, or why it stopped.
type Reply struct {
	Machine   string          `json:"machine"`
	SessionID string          `json:"sessionId"`
	Status    string          `json:"status"` // done | question | permission | exited | error | timeout
	Detail    string          `json:"detail,omitempty"`
	Text      string          `json:"reply"`
	Truncated bool            `json:"truncated,omitempty"`
	Question  json.RawMessage `json:"question,omitempty"`
}

// watch waits for one session to finish the turn our message started.
type watch struct {
	machineID, name, session string
	targs                    protocol.TranscriptArgs
	// baseline is the transcript offset when the message was sent, 0 when unknown (a new session, or
	// one whose transcript could not be read); then the tail is read instead, and requirePrompt says
	// whether the turn must follow a prompt seen there (a new session's tail may begin after it).
	baseline      int64
	requirePrompt bool
	started       time.Time

	mu      sync.Mutex
	notify  bool
	awaited bool
	done    chan struct{}
	reply   Reply
}

type watches struct {
	s  *Server
	mu sync.Mutex
	m  map[string]*watch
}

func newWatches(s *Server) *watches { return &watches{s: s, m: map[string]*watch{}} }

func watchKey(machineID, session string) string { return machineID + "::" + session }

// start watches a session for the end of the turn a message just started. A session already watched
// keeps its watch: completion waits for the reply to the latest message either way.
func (ws *watches) start(machineID, name, session string, targs protocol.TranscriptArgs, baseline int64, requirePrompt, notify bool) {
	key := watchKey(machineID, session)
	ws.mu.Lock()
	if w, ok := ws.m[key]; ok {
		select {
		case <-w.done:
		default:
			w.mu.Lock()
			w.notify = w.notify || notify
			w.mu.Unlock()
			ws.mu.Unlock()
			return
		}
	}
	w := &watch{machineID: machineID, name: name, session: session, targs: targs, baseline: baseline, requirePrompt: requirePrompt, started: time.Now(), notify: notify, done: make(chan struct{})}
	ws.m[key] = w
	ws.mu.Unlock()
	go ws.run(w)
}

func (ws *watches) get(machineID, session string) *watch {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	return ws.m[watchKey(machineID, session)]
}

func (ws *watches) run(w *watch) {
	s := ws.s
	release := s.client.Hold()
	defer release()
	deadline := time.After(watchFor)
	for {
		changed := s.client.Changed()
		if r, ok := s.checkTurn(w); ok {
			ws.finish(w, r)
			return
		}
		select {
		case <-changed:
		case <-time.After(watchTick):
		case <-deadline:
			ws.finish(w, Reply{Machine: w.name, SessionID: w.session, Status: "timeout", Detail: "no reply within " + watchFor.String()})
			return
		}
	}
}

func (ws *watches) finish(w *watch, r Reply) {
	w.mu.Lock()
	w.reply = r
	close(w.done)
	push := w.notify && !w.awaited
	w.mu.Unlock()
	if push {
		ws.s.push(r)
	}
}

// push delivers a reply into this server's own session, as a message from Vineyard.
func (s *Server) push(r Reply) {
	if s.o.Session == "" {
		return
	}
	var b strings.Builder
	fmt.Fprintf(&b, "<vineyard-agent-reply machine=%q session=%q status=%q>\n", r.Machine, r.SessionID, r.Status)
	if r.Text != "" {
		b.WriteString(r.Text)
		b.WriteString("\n")
	}
	if r.Detail != "" {
		fmt.Fprintf(&b, "[%s]\n", r.Detail)
	}
	b.WriteString("</vineyard-agent-reply>\n")
	b.WriteString("Delivered by Vineyard: the reply from an agent you started or messaged with its agent tools, not a message from your user.")
	if r.Truncated {
		b.WriteString(" The reply was cut short; fetch_transcript has all of it.")
	}
	switch r.Status {
	case "permission":
		b.WriteString(" That agent is waiting for a permission prompt the user has to answer in Vineyard.")
	case "question":
		b.WriteString(" That agent asked a question; answer with message_agent if it is yours to answer, or leave it to the user.")
	}
	if _, err := s.client.Request("", "send", protocol.SendArgs{SessionID: s.o.Session, Text: b.String()}, 30*time.Second); err != nil {
		s.o.Log.Printf("deliver reply from %s/%s: %v", r.Machine, r.SessionID, err)
	}
}

func endsTurn(stop string) bool {
	return stop == "end_turn" || stop == "stop_sequence" || stop == "max_tokens"
}

// checkTurn reports the reply once the watched session has finished the turn our message started.
func (s *Server) checkTurn(w *watch) (Reply, bool) {
	e, err := s.client.Entry(w.machineID)
	if err != nil {
		return Reply{}, false
	}
	var ag *model.Agent
	for i := range e.Snapshot.Agents {
		if e.Snapshot.Agents[i].SessionID == w.session {
			ag = &e.Snapshot.Agents[i]
		}
	}
	if ag == nil {
		return Reply{}, false // a new session not reported yet
	}
	if model.IsBusy(ag.State) || ag.State == model.StateUnknown || ag.State == model.StateShell {
		return Reply{}, false
	}
	if w.targs.Path == "" && ag.TranscriptPath != "" {
		w.targs.Path = ag.TranscriptPath
	}
	entries, err := s.entriesSince(w.machineID, w.targs, w.baseline)
	if err != nil {
		return Reply{}, false
	}
	r := Reply{Machine: w.name, SessionID: w.session}
	switch {
	case !ag.Alive || ag.State == model.StateExited:
		r.Status, r.Detail = "exited", "the session's process has ended"
	case ag.APIError != "":
		r.Status, r.Detail = "error", ag.APIError
	case ag.State == model.StateQuestion:
		r.Status, r.Detail = "question", ag.StateDetail
		for _, p := range ag.PendingTools {
			if len(p.Input) > 0 {
				r.Question = p.Input
			}
		}
	case ag.State == model.StatePermission:
		r.Status, r.Detail = "permission", ag.StateDetail
	default:
		if !turnEnded(entries, w.baseline > 0 || w.requirePrompt) {
			return Reply{}, false
		}
		r.Status = "done"
	}
	r.Text, r.Truncated = replyText(entries)
	return r, true
}

// entriesSince is the transcript after byte offset since, at most the last tailLines entries; with
// since 0, just the tail.
func (s *Server) entriesSince(target string, a protocol.TranscriptArgs, since int64) ([]json.RawMessage, error) {
	a.Lines, a.Before = tailLines, 0
	if since <= 0 {
		a.Offset = 0
		d, err := s.readTranscript(target, a)
		return d.Entries, err
	}
	var out []json.RawMessage
	a.Offset = since
	for {
		d, err := s.readTranscript(target, a)
		if err != nil {
			return nil, err
		}
		if d.Truncated {
			return nil, errors.New("transcript replaced")
		}
		out = append(out, d.Entries...)
		if len(out) > tailLines {
			out = out[len(out)-tailLines:]
		}
		if d.Offset <= a.Offset || d.Offset >= d.Size {
			return out, nil
		}
		a.Offset = d.Offset
	}
}

// turnEnded: after the latest prompt there is an assistant message that ended the turn. Without
// needPrompt (a tail that may begin after the prompt) a finished turn with no prompt in sight counts.
func turnEnded(entries []json.RawMessage, needPrompt bool) bool {
	prompt := -1
	parsed := make([]entry, len(entries))
	for i, raw := range entries {
		e, bs, ok := parseEntry(raw)
		if !ok {
			continue
		}
		parsed[i] = e
		if isPrompt(e, bs) {
			prompt = i
		}
	}
	if prompt < 0 && needPrompt {
		return false
	}
	for i := prompt + 1; i < len(parsed); i++ {
		if parsed[i].Type == "assistant" && !parsed[i].IsSidechain && endsTurn(parsed[i].Message.StopReason) {
			return true
		}
	}
	return false
}

// replyText is the assistant's last words after the latest prompt: the text after its final tool
// result, or all its text since the prompt when that is empty.
func replyText(entries []json.RawMessage) (string, bool) {
	type item struct {
		e  entry
		bs []block
	}
	var items []item
	prompt := -1
	for _, raw := range entries {
		e, bs, ok := parseEntry(raw)
		if !ok || e.IsSidechain {
			continue
		}
		items = append(items, item{e, bs})
		if isPrompt(e, bs) {
			prompt = len(items) - 1
		}
	}
	after := items[prompt+1:]
	last := -1
	for i, it := range after {
		if isToolResult(it.e, it.bs) {
			last = i
		}
	}
	collect := func(from []item) string {
		var parts []string
		for _, it := range from {
			if it.e.Type == "assistant" {
				if t := texts(it.bs); t != "" {
					parts = append(parts, t)
				}
			}
		}
		return strings.Join(parts, "\n\n")
	}
	text := collect(after[last+1:])
	if text == "" {
		text = collect(after)
	}
	r := []rune(text)
	if len(r) > replyLimit {
		return string(r[:replyLimit]) + "…", true
	}
	return text, false
}

// origin is this session as the sender of an agent message, one hop further than whatever brought
// this session its own latest agent message.
func (s *Server) origin() *protocol.AgentOrigin {
	o := &protocol.AgentOrigin{Machine: s.o.Machine, Session: s.o.Session, Hops: 1}
	if _, all, err := s.client.Fleet(); err == nil {
		if e, ok := matchMachine(all, s.o.Machine); ok && e.Snapshot.Name != "" {
			o.Machine = e.Snapshot.Name
		}
	}
	if s.o.Session != "" {
		if raw, err := s.client.Request("", "agenthops", protocol.AgentHopsArgs{SessionID: s.o.Session}, 10*time.Second); err == nil {
			var d struct {
				Hops int `json:"hops"`
			}
			if json.Unmarshal(raw, &d) == nil {
				o.Hops = d.Hops + 1
			}
		}
	}
	return o
}

func boolOr(p *bool, def bool) bool {
	if p == nil {
		return def
	}
	return *p
}

func (s *Server) startAgent(args json.RawMessage) (any, error) {
	var a struct {
		Machine        string `json:"machine"`
		Workspace      string `json:"workspace"`
		Prompt         string `json:"prompt"`
		Model          string `json:"model"`
		PermissionMode string `json:"permission_mode"`
		Name           string `json:"name"`
		Notify         *bool  `json:"notify"`
	}
	if err := decode(args, &a); err != nil {
		return nil, err
	}
	if a.Machine == "" || a.Workspace == "" || strings.TrimSpace(a.Prompt) == "" {
		return nil, errors.New("machine, workspace and prompt are required")
	}
	e, err := s.client.Entry(a.Machine)
	if err != nil {
		return nil, err
	}
	if !e.Online {
		return nil, fmt.Errorf("%s is offline", e.Snapshot.Name)
	}
	spawn := map[string]any{"cwd": a.Workspace, "prompt": a.Prompt, "origin": s.origin()}
	if a.Model != "" {
		spawn["model"] = a.Model
	}
	if a.PermissionMode != "" {
		spawn["permissionMode"] = a.PermissionMode
	}
	if a.Name != "" {
		spawn["name"] = a.Name
	}
	raw, err := s.client.Request(e.Snapshot.MachineID, "spawn", spawn, 60*time.Second)
	if err != nil {
		return nil, fmt.Errorf("start an agent on %s: %w", e.Snapshot.Name, err)
	}
	var d struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal(raw, &d); err != nil || d.SessionID == "" {
		return nil, errors.New("the daemon did not say which session it started")
	}
	notify := boolOr(a.Notify, true)
	s.watches.start(e.Snapshot.MachineID, e.Snapshot.Name, d.SessionID, protocol.TranscriptArgs{SessionID: d.SessionID, Cwd: a.Workspace}, 0, false, notify)
	return map[string]any{"machine": e.Snapshot.Name, "sessionId": d.SessionID, "workspace": a.Workspace, "reply": replyNote(notify, s.o.Session != "")}, nil
}

func replyNote(notify, canPush bool) string {
	if notify && canPush {
		return "Started. Its reply will arrive in this conversation as a <vineyard-agent-reply> message when its turn ends; carry on meanwhile, or call await_agent to wait for it."
	}
	return "Started. Call await_agent to get its reply."
}

func (s *Server) messageAgent(args json.RawMessage) (any, error) {
	var a struct {
		Machine   string `json:"machine"`
		SessionID string `json:"session_id"`
		Message   string `json:"message"`
		Notify    *bool  `json:"notify"`
	}
	if err := decode(args, &a); err != nil {
		return nil, err
	}
	if a.Machine == "" || a.SessionID == "" || strings.TrimSpace(a.Message) == "" {
		return nil, errors.New("machine, session_id and message are required")
	}
	e, err := s.client.Entry(a.Machine)
	if err != nil {
		return nil, err
	}
	if e.Snapshot.MachineID == s.o.Machine && a.SessionID == s.o.Session {
		return nil, errors.New("that is this session")
	}
	var live bool
	for _, ag := range e.Snapshot.Agents {
		if ag.SessionID == a.SessionID && ag.Alive {
			live = true
		}
	}
	if !live {
		return nil, fmt.Errorf("no live session %s on %s (list_sessions shows them; start_agent starts a new one)", a.SessionID, e.Snapshot.Name)
	}
	id, name, targs, err := s.locate(e.Snapshot.MachineID, a.SessionID, "")
	if err != nil {
		return nil, err
	}
	var baseline int64
	tail := targs
	tail.Lines = 1
	if d, err := s.readTranscript(id, tail); err == nil {
		baseline = d.Offset
	}
	if _, err := s.client.Request(id, "send", protocol.SendArgs{SessionID: a.SessionID, Text: a.Message, Origin: s.origin()}, 30*time.Second); err != nil {
		return nil, fmt.Errorf("message %s on %s: %w", a.SessionID, name, err)
	}
	notify := boolOr(a.Notify, true)
	s.watches.start(id, name, a.SessionID, targs, baseline, true, notify)
	note := replyNote(notify, s.o.Session != "")
	return map[string]any{"delivered": true, "machine": name, "sessionId": a.SessionID, "reply": strings.Replace(note, "Started.", "Delivered.", 1)}, nil
}

func (s *Server) awaitAgent(args json.RawMessage) (any, error) {
	var a struct {
		Machine        string `json:"machine"`
		SessionID      string `json:"session_id"`
		TimeoutSeconds int    `json:"timeout_seconds"`
	}
	if err := decode(args, &a); err != nil {
		return nil, err
	}
	timeout := time.Duration(a.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	timeout = min(timeout, 30*time.Minute)
	e, err := s.client.Entry(a.Machine)
	if err != nil {
		return nil, err
	}
	id, name := e.Snapshot.MachineID, e.Snapshot.Name
	if w := s.watches.get(id, a.SessionID); w != nil {
		w.mu.Lock()
		w.awaited = true
		w.mu.Unlock()
		select {
		case <-w.done:
			w.mu.Lock()
			defer w.mu.Unlock()
			return w.reply, nil
		case <-time.After(timeout):
			w.mu.Lock()
			w.awaited = false // still pushed when it comes, if asked for
			w.mu.Unlock()
			return Reply{Machine: name, SessionID: a.SessionID, Status: "timeout", Detail: "still working; its reply is delivered later if notify was on, or call await_agent again"}, nil
		}
	}
	// Not messaged from here: wait for it to stop being busy and report its latest reply.
	release := s.client.Hold()
	defer release()
	deadline := time.After(timeout)
	for {
		changed := s.client.Changed()
		e, err := s.client.Entry(id)
		if err != nil {
			return nil, err
		}
		var ag *model.Agent
		for i := range e.Snapshot.Agents {
			if e.Snapshot.Agents[i].SessionID == a.SessionID {
				ag = &e.Snapshot.Agents[i]
			}
		}
		if ag == nil {
			return nil, fmt.Errorf("no session %s on %s", a.SessionID, name)
		}
		if !model.IsBusy(ag.State) {
			_, _, targs, err := s.locate(id, a.SessionID, "")
			if err != nil {
				return nil, err
			}
			entries, err := s.entriesSince(id, targs, 0)
			if err != nil {
				return nil, err
			}
			r := Reply{Machine: name, SessionID: a.SessionID, Status: "done"}
			switch {
			case !ag.Alive:
				r.Status = "exited"
			case ag.State == model.StateQuestion:
				r.Status, r.Detail = "question", ag.StateDetail
			case ag.State == model.StatePermission:
				r.Status, r.Detail = "permission", ag.StateDetail
			case ag.APIError != "":
				r.Status, r.Detail = "error", ag.APIError
			}
			r.Text, r.Truncated = replyText(entries)
			return r, nil
		}
		select {
		case <-changed:
		case <-time.After(watchTick):
		case <-deadline:
			return Reply{Machine: name, SessionID: a.SessionID, Status: "timeout", Detail: "still working"}, nil
		}
	}
}
