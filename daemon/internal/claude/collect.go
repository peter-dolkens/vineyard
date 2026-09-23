// Package claude reads what Claude Code leaves on disk and turns it into agents with derived states.
//
// Sources:
//
//	~/.claude/sessions/<pid>.json          live session registry (status busy|shell|idle|waiting)
//	~/.claude/projects/<enc cwd>/<sid>.jsonl transcript – model, effort, title, pending tool calls
//	~/.claude/projects/<enc cwd>/<sid>/subagents/ one transcript + meta per Agent-tool call (see subagents.go)
//	~/.claude/ide/<port>.lock              which folders are open in VS Code
package claude

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/peter-dolkens/vineyard/daemon/internal/model"
)

type RawSession struct {
	PID       int
	Alive     bool
	FileMtime int64
	Registry  map[string]any
}

type RawTranscript struct {
	SessionID string
	Mtime     int64
	Size      int64
	Path      string
	Entries   []map[string]any
	Subagents []RawSubagent // transcripts under <sid>/subagents/, unordered
	Tasks     []model.BackgroundTask
}

type RawProject struct {
	Mtime int64
	Count int
	Cwd   string
}

type RawIDELock struct {
	Name             string
	PID              int
	WorkspaceFolders []string
	IDEName          string
}

type Report struct {
	Host        model.HostInfo
	HasClaude   bool
	Sessions    []RawSession
	Transcripts map[string]RawTranscript
	Projects    []RawProject
	IDELocks    []RawIDELock
}

// MaxTailBytes bounds how much of a transcript we read per poll; tool results can make single lines huge.
const MaxTailBytes = 400_000

var nonPathChars = regexp.MustCompile(`[^A-Za-z0-9-]`)

// EncodeProjectDir mirrors Claude Code's mapping from a working directory to its projects/ folder name.
func EncodeProjectDir(cwd string) string {
	return nonPathChars.ReplaceAllString(cwd, "-")
}

func DefaultClaudeDir() string {
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		return d
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude")
}

// Collector caches expensive lookups between polls.
type Collector struct {
	ClaudeDir string
	TailLines int

	mu       sync.Mutex
	cwdCache map[string]cwdEntry // project dir → cwd (keyed by latest transcript path+mtime)
	// tailCache avoids re-reading a transcript whose size and mtime have not changed since last poll.
	tailCache map[string]tailEntry
	// subCache does the same for subagent transcripts, keyed by path.
	subCache map[string]subEntry
	// taskCache remembers each live session's background tasks between polls, so one whose start
	// has scrolled out of the tail is still listed while it runs (see tasks.go).
	taskCache map[string][]model.BackgroundTask
}

type tailEntry struct {
	size    int64
	mtime   int64
	entries []map[string]any
}

type cwdEntry struct {
	key string
	cwd string
}

func NewCollector(claudeDir string, tailLines int) *Collector {
	if claudeDir == "" {
		claudeDir = DefaultClaudeDir()
	}
	if tailLines <= 0 {
		tailLines = 80
	}
	return &Collector{ClaudeDir: claudeDir, TailLines: tailLines, cwdCache: map[string]cwdEntry{}, tailCache: map[string]tailEntry{}, subCache: map[string]subEntry{}, taskCache: map[string][]model.BackgroundTask{}}
}

func (c *Collector) Collect() *Report {
	home, _ := os.UserHomeDir()
	hostname, _ := os.Hostname()
	r := &Report{
		Host: model.HostInfo{
			Hostname:  hostname,
			OS:        runtime.GOOS,
			Arch:      runtime.GOARCH,
			Home:      home,
			ClaudeDir: c.ClaudeDir,
			Now:       time.Now().UnixMilli(),
			MACs:      hardwareAddrs(),
		},
		Transcripts: map[string]RawTranscript{},
	}
	if st, err := os.Stat(c.ClaudeDir); err != nil || !st.IsDir() {
		r.HasClaude = false
		return r
	}
	r.HasClaude = true
	c.collectIDELocks(r)
	c.collectSessions(r)
	c.collectProjects(r)
	return r
}

func (c *Collector) collectIDELocks(r *Report) {
	entries, err := os.ReadDir(filepath.Join(c.ClaudeDir, "ide"))
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".lock") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(c.ClaudeDir, "ide", e.Name()))
		if err != nil {
			continue
		}
		var v struct {
			PID              int      `json:"pid"`
			WorkspaceFolders []string `json:"workspaceFolders"`
			IDEName          string   `json:"ideName"`
		}
		_ = json.Unmarshal(b, &v)
		r.IDELocks = append(r.IDELocks, RawIDELock{
			Name: strings.TrimSuffix(e.Name(), ".lock"), PID: v.PID, WorkspaceFolders: v.WorkspaceFolders, IDEName: v.IDEName,
		})
	}
}

func (c *Collector) collectSessions(r *Report) {
	dir := filepath.Join(c.ClaudeDir, "sessions")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	defer func() {
		c.mu.Lock()
		for sid := range c.tailCache {
			if _, live := r.Transcripts[sid]; !live {
				delete(c.tailCache, sid)
			}
		}
		c.mu.Unlock()
		c.pruneSubCache()
		live := make(map[string]bool, len(r.Transcripts))
		for sid := range r.Transcripts {
			live[sid] = true
		}
		c.pruneTaskCache(live)
	}()
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		pid, err := strconv.Atoi(strings.TrimSuffix(e.Name(), ".json"))
		if err != nil {
			continue
		}
		full := filepath.Join(dir, e.Name())
		b, err := os.ReadFile(full)
		if err != nil {
			continue
		}
		var reg map[string]any
		if err := json.Unmarshal(b, &reg); err != nil {
			reg = nil
		}
		var mtime int64
		if st, err := e.Info(); err == nil {
			mtime = st.ModTime().UnixMilli()
		}
		s := RawSession{PID: pid, Alive: processAlive(pid), FileMtime: mtime, Registry: reg}
		r.Sessions = append(r.Sessions, s)

		if !s.Alive || reg == nil {
			continue
		}
		sid, _ := reg["sessionId"].(string)
		cwd, _ := reg["cwd"].(string)
		if sid == "" || cwd == "" {
			continue
		}
		tpath := filepath.Join(c.ClaudeDir, "projects", EncodeProjectDir(cwd), sid+".jsonl")
		st, err := os.Stat(tpath)
		if err != nil {
			continue
		}
		t := RawTranscript{SessionID: sid, Mtime: st.ModTime().UnixMilli(), Size: st.Size(), Path: tpath}
		c.mu.Lock()
		cached, ok := c.tailCache[sid]
		c.mu.Unlock()
		if ok && cached.size == t.Size && cached.mtime == t.Mtime {
			t.Entries = cached.entries // unchanged since last poll: one stat, no read
		} else {
			lines, err := TailLines(tpath, c.TailLines, MaxTailBytes)
			if err != nil {
				continue
			}
			for _, ln := range lines {
				var m map[string]any
				if json.Unmarshal(ln, &m) == nil && m != nil {
					t.Entries = append(t.Entries, m)
				}
			}
			c.mu.Lock()
			c.tailCache[sid] = tailEntry{size: t.Size, mtime: t.Mtime, entries: t.Entries}
			c.mu.Unlock()
		}
		c.collectSubagents(&t)
		t.Tasks = c.mergeTasks(sid, BuildTasks(t.Entries), time.Now().UnixMilli())
		r.Transcripts[sid] = t
	}
}

var cwdRe = regexp.MustCompile(`"cwd":"((?:[^"\\]|\\.)*)"`)

func (c *Collector) collectProjects(r *Report) {
	dir := filepath.Join(c.ClaudeDir, "projects")
	dirs, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		pdir := filepath.Join(dir, d.Name())
		files, err := os.ReadDir(pdir)
		if err != nil {
			continue
		}
		var latest string
		var latestM int64
		count := 0
		for _, f := range files {
			if f.IsDir() || !strings.HasSuffix(f.Name(), ".jsonl") {
				continue
			}
			count++
			if st, err := f.Info(); err == nil {
				if m := st.ModTime().UnixMilli(); m > latestM {
					latestM = m
					latest = filepath.Join(pdir, f.Name())
				}
			}
		}
		if count == 0 {
			continue
		}
		key := latest + "@" + strconv.FormatInt(latestM, 10)
		ce, ok := c.cwdCache[d.Name()]
		if !ok || ce.key != key {
			ce = cwdEntry{key: key, cwd: findCwd(latest)}
			c.cwdCache[d.Name()] = ce
		}
		if ce.cwd == "" {
			continue
		}
		r.Projects = append(r.Projects, RawProject{Mtime: latestM, Count: count, Cwd: ce.cwd})
	}
	sort.Slice(r.Projects, func(i, j int) bool { return r.Projects[i].Mtime > r.Projects[j].Mtime })
}

// findCwd scans the head of a transcript for its working directory.
func findCwd(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	buf := make([]byte, 256*1024)
	n, _ := io.ReadFull(f, buf)
	if m := cwdRe.FindSubmatch(buf[:n]); m != nil {
		var s string
		if json.Unmarshal(append(append([]byte{'"'}, m[1]...), '"'), &s) == nil {
			return s
		}
	}
	return ""
}

// TailLines returns the last n complete lines of a file, reading at most maxBytes from its end.
func TailLines(path string, n int, maxBytes int64) ([][]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	size := st.Size()
	start := int64(0)
	if size > maxBytes {
		start = size - maxBytes
	}
	buf := make([]byte, size-start)
	if _, err := f.ReadAt(buf, start); err != nil && err != io.EOF {
		return nil, err
	}
	parts := bytes.Split(buf, []byte{'\n'})
	if start > 0 && len(parts) > 0 {
		parts = parts[1:] // first line is almost certainly truncated
	}
	var out [][]byte
	for _, p := range parts {
		p = bytes.TrimRight(p, "\r")
		if len(bytes.TrimSpace(p)) > 0 {
			out = append(out, p)
		}
	}
	if len(out) > n {
		out = out[len(out)-n:]
	}
	return out, nil
}

// ReadFrom returns every complete line starting at byte offset `from`, plus the offset just past the
// last complete line. It reads at most maxBytes; if more is pending the caller loops.
func ReadFrom(path string, from int64, maxBytes int64) (lines [][]byte, next int64, size int64, err error) {
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
	last := bytes.LastIndexByte(buf, '\n')
	if last < 0 {
		return nil, from, size, nil // no complete line yet
	}
	next = from + int64(last) + 1
	for _, p := range bytes.Split(buf[:last], []byte{'\n'}) {
		p = bytes.TrimRight(p, "\r")
		if len(bytes.TrimSpace(p)) > 0 {
			lines = append(lines, p)
		}
	}
	return lines, next, size, nil
}

// TailWithOffset is TailLines plus the end-of-file offset so callers can continue with ReadFrom.
func TailWithOffset(path string, n int, maxBytes int64) ([][]byte, int64, error) {
	lines, err := TailLines(path, n, maxBytes)
	if err != nil {
		return nil, 0, err
	}
	st, err := os.Stat(path)
	if err != nil {
		return nil, 0, err
	}
	return lines, st.Size(), nil
}

// hardwareAddrs lists the MAC addresses of up, non-loopback interfaces that carry an IPv4 address
// (so virtual and idle interfaces do not get magic packets they can never act on).
func hardwareAddrs() []string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var out []string
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 || len(ifc.HardwareAddr) != 6 {
			continue
		}
		addrs, _ := ifc.Addrs()
		has4 := false
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil && !ipn.IP.IsLinkLocalUnicast() {
				has4 = true
			}
		}
		if has4 {
			out = append(out, ifc.HardwareAddr.String())
		}
	}
	return out
}
