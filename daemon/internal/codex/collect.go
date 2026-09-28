package codex

import (
	"bufio"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/peter-dolkens/vineyard/daemon/internal/model"
)

// RawThread is one rollout on disk. Lines is the tail, read only for live threads.
type RawThread struct {
	ID    string
	Path  string
	Mtime int64
	Size  int64
	Head  headInfo
	Live  bool
	Lines []Line
}

type headInfo struct {
	Cwd         string
	FirstPrompt string
	StartedAt   int64
	Originator  string
	Version     string
	GitBranch   string
}

type RawProject struct {
	Cwd   string
	Mtime int64
	Count int
}

type Report struct {
	HasCodex bool
	Threads  []RawThread // live threads, with their tails
	Projects []RawProject
}

// maxRollouts bounds the walk of the sessions tree per poll (newest days first).
const maxRollouts = 3000

var rolloutRe = regexp.MustCompile(`^rollout-.*-([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})\.jsonl$`)

// DefaultCodexDir is $CODEX_HOME or ~/.codex.
func DefaultCodexDir() string {
	if d := os.Getenv("CODEX_HOME"); d != "" {
		return d
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".codex")
}

// Collector caches what does not change between polls: a rollout's head (cwd, first prompt) for
// good, and its tail while size and mtime stand still.
type Collector struct {
	CodexDir  string
	TailLines int

	mu    sync.Mutex
	heads map[string]headInfo
	tails map[string]tailEntry
}

type tailEntry struct {
	size, mtime int64
	lines       []Line
}

func NewCollector(codexDir string, tailLines int) *Collector {
	if codexDir == "" {
		codexDir = DefaultCodexDir()
	}
	if tailLines <= 0 {
		tailLines = 120
	}
	return &Collector{CodexDir: codexDir, TailLines: tailLines, heads: map[string]headInfo{}, tails: map[string]tailEntry{}}
}

// SessionsDir is where rollouts live.
func (c *Collector) SessionsDir() string { return filepath.Join(c.CodexDir, "sessions") }

// ThreadID is the thread uuid in a rollout file name, or "".
func ThreadID(path string) string {
	if m := rolloutRe.FindStringSubmatch(filepath.Base(path)); m != nil {
		return m[1]
	}
	return ""
}

// rolloutFile is one file the walk found.
type rolloutFile struct {
	id    string
	path  string
	mtime int64
	size  int64
}

// walk lists rollouts newest day first, at most maxRollouts of them.
func (c *Collector) walk(limit int) []rolloutFile {
	var out []rolloutFile
	root := c.SessionsDir()
	years, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	descending := func(ents []os.DirEntry) []os.DirEntry {
		dirs := ents[:0:0]
		for _, e := range ents {
			if e.IsDir() {
				dirs = append(dirs, e)
			}
		}
		sort.Slice(dirs, func(i, j int) bool { return dirs[i].Name() > dirs[j].Name() })
		return dirs
	}
	for _, y := range descending(years) {
		months, _ := os.ReadDir(filepath.Join(root, y.Name()))
		for _, m := range descending(months) {
			days, _ := os.ReadDir(filepath.Join(root, y.Name(), m.Name()))
			for _, d := range descending(days) {
				dir := filepath.Join(root, y.Name(), m.Name(), d.Name())
				files, _ := os.ReadDir(dir)
				for _, f := range files {
					if f.IsDir() {
						continue
					}
					id := ThreadID(f.Name())
					if id == "" {
						continue
					}
					st, err := f.Info()
					if err != nil || st.Size() == 0 {
						continue
					}
					out = append(out, rolloutFile{id: id, path: filepath.Join(dir, f.Name()), mtime: st.ModTime().UnixMilli(), size: st.Size()})
					if len(out) >= limit {
						return out
					}
				}
			}
		}
	}
	return out
}

// liveThreads is the set of thread ids whose writer lock is held right now.
func (c *Collector) liveThreads() map[string]bool {
	live := map[string]bool{}
	dir := filepath.Join(c.CodexDir, "thread-writer-locks")
	ents, err := os.ReadDir(dir)
	if err != nil {
		return live
	}
	for _, e := range ents {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".lock") || strings.HasPrefix(name, ".") {
			continue
		}
		id := strings.TrimSuffix(name, ".lock")
		if lockHeld(filepath.Join(dir, name)) {
			live[id] = true
		}
	}
	return live
}

// headBytes is how much of a rollout's start is read for its cwd and first prompt. The developer
// messages Codex injects before the first prompt run to tens of kilobytes.
const headBytes = 512 * 1024

// readHead pulls the working directory, start time and first prompt from a rollout's beginning.
func readHead(path string) headInfo {
	var h headInfo
	f, err := os.Open(path)
	if err != nil {
		return h
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	read := 0
	var offset int64
	for sc.Scan() && read < headBytes {
		b := sc.Bytes()
		read += len(b) + 1
		l, ok := ParseLine(b, offset)
		offset += int64(len(b) + 1)
		if !ok {
			continue
		}
		p := l.Payload
		switch l.Type {
		case "session_meta":
			if h.Cwd == "" {
				h.Cwd = str(p["cwd"])
			}
			if h.Originator == "" {
				h.Originator = str(p["originator"])
			}
			if h.Version == "" {
				h.Version = str(p["cli_version"])
			}
			if h.GitBranch == "" {
				h.GitBranch = str(obj(p["git"])["branch"])
			}
			if h.StartedAt == 0 {
				h.StartedAt = parseTime(str(p["timestamp"]))
				if h.StartedAt == 0 {
					h.StartedAt = l.Time
				}
			}
		case "turn_context":
			if h.Cwd == "" {
				h.Cwd = str(p["cwd"])
			}
		case "response_item":
			if l.itemType() == "message" && str(p["role"]) == "user" {
				if t := messageText(p); t != "" && !isMetaPrompt(t) {
					h.FirstPrompt = clip(t, 160)
					return h
				}
			}
		}
	}
	return h
}

func (c *Collector) head(f rolloutFile) headInfo {
	c.mu.Lock()
	h, ok := c.heads[f.path]
	c.mu.Unlock()
	if ok && (h.FirstPrompt != "" || f.size > headBytes) {
		return h
	}
	h = readHead(f.path)
	c.mu.Lock()
	c.heads[f.path] = h
	c.mu.Unlock()
	return h
}

func (c *Collector) tail(f rolloutFile) []Line {
	c.mu.Lock()
	t, ok := c.tails[f.path]
	c.mu.Unlock()
	if ok && t.size == f.size && t.mtime == f.mtime {
		return t.lines
	}
	lines, _, err := ReadTail(f.path, c.TailLines, MaxTailBytes)
	if err != nil {
		return nil
	}
	c.mu.Lock()
	c.tails[f.path] = tailEntry{size: f.size, mtime: f.mtime, lines: lines}
	c.mu.Unlock()
	return lines
}

// Collect reads the Codex directory once. Only live threads (writer lock held) get their tails
// read; every rollout counts towards its workspace's history.
func (c *Collector) Collect() *Report {
	r := &Report{}
	if st, err := os.Stat(c.CodexDir); err != nil || !st.IsDir() {
		return r
	}
	r.HasCodex = true
	live := c.liveThreads()
	files := c.walk(maxRollouts)
	seen := map[string]bool{}
	projects := map[string]*RawProject{}
	for _, f := range files {
		seen[f.path] = true
		h := c.head(f)
		if h.Cwd == "" {
			continue
		}
		p := projects[h.Cwd]
		if p == nil {
			p = &RawProject{Cwd: h.Cwd}
			projects[h.Cwd] = p
		}
		p.Count++
		if f.mtime > p.Mtime {
			p.Mtime = f.mtime
		}
		if live[f.id] {
			r.Threads = append(r.Threads, RawThread{ID: f.id, Path: f.path, Mtime: f.mtime, Size: f.size, Head: h, Live: true, Lines: c.tail(f)})
		}
	}
	for _, p := range projects {
		r.Projects = append(r.Projects, *p)
	}
	sort.Slice(r.Projects, func(i, j int) bool { return r.Projects[i].Mtime > r.Projects[j].Mtime })
	c.mu.Lock()
	for path := range c.heads {
		if !seen[path] {
			delete(c.heads, path)
		}
	}
	for path := range c.tails {
		if !seen[path] {
			delete(c.tails, path)
		}
	}
	c.mu.Unlock()
	return r
}

func normalisePath(p string) string {
	if t := strings.TrimRight(p, `/\`); t != "" {
		return t
	}
	return p
}

// BuildAgent turns one live thread into an Agent.
func BuildAgent(machineID string, t RawThread, now int64) *model.Agent {
	d := Derive(t.Lines)
	cwd := d.Cwd
	if cwd == "" {
		cwd = t.Head.Cwd
	}
	if cwd == "" {
		return nil
	}
	cwd = normalisePath(cwd)
	a := &model.Agent{
		ID: machineID + "::" + t.ID, Provider: Provider, MachineID: machineID, WorkspacePath: cwd, Cwd: cwd,
		SessionID: t.ID, Alive: t.Live, Entrypoint: d.Originator, Version: d.Version,
		State: d.State, StateDetail: d.StateDetail, Model: d.Model, Effort: d.Effort,
		PermissionMode: Mode(d.Sandbox, d.ApprovalPolicy), GitBranch: d.GitBranch, LastPrompt: d.LastPrompt,
		StartedAt: t.Head.StartedAt, LastActivityAt: d.LastActivityAt, ContextTokens: d.ContextTokens,
		ContextWindow: d.ContextWindow, ContextAt: d.ContextAt, PendingTools: d.PendingTools, TranscriptPath: t.Path,
	}
	if a.StartedAt == 0 {
		a.StartedAt = d.StartedAt
	}
	if a.Entrypoint == "" {
		a.Entrypoint = t.Head.Originator
	}
	if a.Version == "" {
		a.Version = t.Head.Version
	}
	if a.GitBranch == "" {
		a.GitBranch = t.Head.GitBranch
	}
	if a.PendingTools == nil {
		a.PendingTools = []model.PendingTool{}
	}
	if t.Mtime > a.LastActivityAt {
		a.LastActivityAt = t.Mtime
	}
	if title := t.Head.FirstPrompt; title != "" {
		a.Title = clip(title, 80)
	} else if d.FirstPrompt != "" {
		a.Title = clip(d.FirstPrompt, 80)
	}
	if !t.Live {
		a.State, a.StateDetail = model.StateExited, "Process has exited"
	} else if a.State == model.StateUnknown {
		a.State, a.StateDetail = model.StateIdle, "Idle"
	}
	return a
}

// Interpret turns a Report into the agents and workspaces of one machine.
func Interpret(machineID string, r *Report, now int64) ([]model.Agent, []model.Workspace) {
	wsMap := map[string]*model.Workspace{}
	ws := func(path string) *model.Workspace {
		key := normalisePath(path)
		if w, ok := wsMap[key]; ok {
			return w
		}
		w := &model.Workspace{ID: machineID + "::" + key, MachineID: machineID, Path: key, Agents: []model.Agent{}}
		wsMap[key] = w
		return w
	}
	for _, p := range r.Projects {
		w := ws(p.Cwd)
		w.HistoryCount += p.Count
		if p.Mtime > w.LastActivityAt {
			w.LastActivityAt = p.Mtime
		}
	}
	var agents []model.Agent
	for _, t := range r.Threads {
		a := BuildAgent(machineID, t, now)
		if a == nil {
			continue
		}
		agents = append(agents, *a)
		w := ws(a.WorkspacePath)
		w.Agents = append(w.Agents, *a)
		if a.LastActivityAt > w.LastActivityAt {
			w.LastActivityAt = a.LastActivityAt
		}
	}
	sort.Slice(agents, func(i, j int) bool { return agents[i].ID < agents[j].ID })
	workspaces := make([]model.Workspace, 0, len(wsMap))
	for _, w := range wsMap {
		sort.Slice(w.Agents, func(i, j int) bool { return w.Agents[i].ID < w.Agents[j].ID })
		workspaces = append(workspaces, *w)
	}
	sort.Slice(workspaces, func(i, j int) bool { return workspaces[i].Path < workspaces[j].Path })
	return agents, workspaces
}

// MergeWorkspaces folds two machines' workspace lists (Claude's and Codex's) into one by path:
// agents are concatenated, history counts added, the newest activity and any open-in-IDE flag kept.
func MergeWorkspaces(a, b []model.Workspace) []model.Workspace {
	byPath := map[string]*model.Workspace{}
	out := make([]model.Workspace, 0, len(a)+len(b))
	for _, w := range a {
		out = append(out, w)
		byPath[w.Path] = &out[len(out)-1]
	}
	for _, w := range b {
		if cur, ok := byPath[w.Path]; ok {
			cur.Agents = append(cur.Agents, w.Agents...)
			cur.HistoryCount += w.HistoryCount
			if w.LastActivityAt > cur.LastActivityAt {
				cur.LastActivityAt = w.LastActivityAt
			}
			cur.OpenInIDE = cur.OpenInIDE || w.OpenInIDE
			sort.Slice(cur.Agents, func(i, j int) bool { return cur.Agents[i].ID < cur.Agents[j].ID })
			continue
		}
		out = append(out, w)
		byPath[w.Path] = &out[len(out)-1]
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}
