// Package retention keeps the folders the agent tools fill from growing for ever: transcripts
// fetched from other machines (~/.vineyard/transcripts) and files sent to this machine's sessions
// (~/.vineyard/received).
//
// Nothing here runs on a timer, since an unwatched daemon runs none: the daemon sweeps when it
// starts, when a viewer attaches (at most hourly), and after it receives a file, and the agent
// tools' server sweeps transcripts after each fetch.
package retention

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/peter-dolkens/vineyard/daemon/internal/config"
)

// Defaults, each overridable in config.json (see Policy).
const (
	DefaultTranscriptDays = 7
	DefaultTranscriptMB   = 1024
	DefaultReceivedDays   = 14
	// partialAge drops a half-received file: a live transfer adds to it far more often than this.
	partialAge = time.Hour
	// minInterval spaces out sweeps asked for by viewers attaching.
	minInterval = time.Hour
)

// Policy is how long things are kept. Zero fields take the defaults; a negative one keeps for ever.
type Policy struct {
	TranscriptDays int
	TranscriptMB   int
	ReceivedDays   int
}

// PolicyOf reads the limits from a machine's config.
func PolicyOf(c *config.Config) Policy {
	if c == nil {
		return Policy{}
	}
	return Policy{TranscriptDays: c.TranscriptRetentionDays, TranscriptMB: c.TranscriptCacheMB, ReceivedDays: c.ReceivedRetentionDays}
}

func days(n, def int) time.Duration {
	if n == 0 {
		n = def
	}
	if n < 0 {
		return 0
	}
	return time.Duration(n) * 24 * time.Hour
}

// TranscriptDir and ReceivedDir are the two folders, under ~/.vineyard (or VINEYARD_DIR).
func TranscriptDir() string { return config.Path("transcripts") }
func ReceivedDir() string   { return config.Path("received") }

// Result says what a sweep removed.
type Result struct {
	Removed int   `json:"removed"`
	Bytes   int64 `json:"bytes"`
}

func (r *Result) add(o Result) { r.Removed += o.Removed; r.Bytes += o.Bytes }

// unit is what is kept or removed as one: a fetched transcript's files, or one received file.
type unit struct {
	files []string
	size  int64
	at    time.Time // the newest of its files
}

// SweepTranscripts removes fetched transcripts not fetched again within the policy's days, then the
// least recently fetched until the folder fits its size. A transcript's .jsonl, .md and .meta.json
// go together; fetching it again just downloads it afresh.
func SweepTranscripts(p Policy, now time.Time) (Result, error) {
	units, err := collect(TranscriptDir(), func(rel string) string {
		// <machine>/<session>.<ext>: the session's files share a key.
		dir, file := filepath.Split(rel)
		for _, ext := range []string{".meta.json", ".jsonl", ".md"} {
			if strings.HasSuffix(file, ext) {
				return dir + strings.TrimSuffix(file, ext)
			}
		}
		return rel
	})
	if err != nil {
		return Result{}, err
	}
	res := removeOlder(units, days(p.TranscriptDays, DefaultTranscriptDays), now)
	mb := p.TranscriptMB
	if mb == 0 {
		mb = DefaultTranscriptMB
	}
	if mb > 0 {
		res.add(removeOverSize(units, int64(mb)<<20))
	}
	pruneEmpty(TranscriptDir())
	return res, nil
}

// SweepReceived removes files sent to this machine's sessions once they are older than the policy's
// days, except a session's that live(session) says is still running, and half-received files left by
// a transfer that stopped.
func SweepReceived(p Policy, now time.Time, live func(session string) bool) (Result, error) {
	root := ReceivedDir()
	var res Result
	partial, err := collect(filepath.Join(root, ".partial"), func(rel string) string { return rel })
	if err != nil {
		return res, err
	}
	res.add(removeOlder(partial, partialAge, now))

	keep := days(p.ReceivedDays, DefaultReceivedDays)
	if keep > 0 {
		sessions, err := os.ReadDir(root)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return res, err
		}
		for _, s := range sessions {
			if !s.IsDir() || s.Name() == ".partial" || (live != nil && live(s.Name())) {
				continue
			}
			units, err := collect(filepath.Join(root, s.Name()), func(rel string) string { return rel })
			if err != nil {
				return res, err
			}
			res.add(removeOlder(units, keep, now))
		}
	}
	pruneEmpty(root)
	return res, nil
}

// collect groups the regular files under root by key(path relative to root).
func collect(root string, key func(rel string) string) (map[string]*unit, error) {
	units := map[string]*unit{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if d.Type()&fs.ModeSymlink != 0 || !d.Type().IsRegular() {
			return nil // never follow a link out of the folder
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return nil
		}
		k := key(rel)
		u := units[k]
		if u == nil {
			u = &unit{}
			units[k] = u
		}
		u.files = append(u.files, path)
		u.size += info.Size()
		if info.ModTime().After(u.at) {
			u.at = info.ModTime()
		}
		return nil
	})
	return units, err
}

func remove(u *unit) Result {
	var r Result
	for _, f := range u.files {
		if st, err := os.Lstat(f); err == nil && os.Remove(f) == nil {
			r.Bytes += st.Size()
		}
	}
	r.Removed++
	return r
}

func removeOlder(units map[string]*unit, maxAge time.Duration, now time.Time) Result {
	var r Result
	if maxAge <= 0 {
		return r
	}
	for k, u := range units {
		if now.Sub(u.at) > maxAge {
			r.add(remove(u))
			delete(units, k)
		}
	}
	return r
}

func removeOverSize(units map[string]*unit, max int64) Result {
	var r Result
	var total int64
	list := make([]*unit, 0, len(units))
	for _, u := range units {
		total += u.size
		list = append(list, u)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].at.Before(list[j].at) })
	for i, u := range list {
		if total <= max || i == len(list)-1 {
			break // the newest stays even alone over the limit: it was just fetched
		}
		r.add(remove(u))
		total -= u.size
	}
	return r
}

// pruneEmpty removes empty folders under root, never root itself.
func pruneEmpty(root string) {
	var dirs []string
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() && path != root {
			dirs = append(dirs, path)
		}
		return nil
	})
	for i := len(dirs) - 1; i >= 0; i-- { // deepest first
		_ = os.Remove(dirs[i]) // fails, harmlessly, on a folder that is not empty
	}
}

// Sweeper runs both sweeps for the daemon, at most once per minInterval unless forced.
type Sweeper struct {
	Policy func() Policy
	Live   func(session string) bool
	Logf   func(format string, args ...any)

	mu   sync.Mutex
	last time.Time
}

// Run sweeps now if forced or if the last sweep was long enough ago.
func (s *Sweeper) Run(force bool) {
	s.mu.Lock()
	if !force && time.Since(s.last) < minInterval {
		s.mu.Unlock()
		return
	}
	s.last = time.Now()
	s.mu.Unlock()
	p := Policy{}
	if s.Policy != nil {
		p = s.Policy()
	}
	now := time.Now()
	t, err1 := SweepTranscripts(p, now)
	r, err2 := SweepReceived(p, now, s.Live)
	if s.Logf == nil {
		return
	}
	if err1 != nil || err2 != nil {
		s.Logf("retention: %v %v", err1, err2)
	}
	if t.Removed > 0 || r.Removed > 0 {
		s.Logf("retention: removed %d fetched transcripts (%d KB) and %d received files (%d KB)", t.Removed, t.Bytes>>10, r.Removed, r.Bytes>>10)
	}
}
