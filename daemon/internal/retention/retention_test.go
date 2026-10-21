package retention

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

var now = time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)

func put(t *testing.T, path string, size int, age time.Duration) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, make([]byte, size), 0o600); err != nil {
		t.Fatal(err)
	}
	at := now.Add(-age)
	if err := os.Chtimes(path, at, at); err != nil {
		t.Fatal(err)
	}
}

func exists(path string) bool { _, err := os.Lstat(path); return err == nil }

func transcript(t *testing.T, machine, session string, size int, age time.Duration) string {
	base := filepath.Join(TranscriptDir(), machine, session)
	put(t, base+".jsonl", size, age)
	put(t, base+".md", 10, age)
	put(t, base+".meta.json", 10, age)
	return base
}

func TestTranscriptsGoByAgeAsAWholeAndEmptyFoldersWithThem(t *testing.T) {
	t.Setenv("VINEYARD_DIR", t.TempDir())
	old := transcript(t, "hawk", "s-old", 100, 8*24*time.Hour)
	fresh := transcript(t, "hawk", "s-fresh", 100, 2*24*time.Hour)
	gone := transcript(t, "falcon", "s-gone", 100, 30*24*time.Hour)
	// A fetch again rewrites only meta.json: the whole transcript counts as fetched then.
	refetched := transcript(t, "hawk", "s-refetched", 100, 20*24*time.Hour)
	put(t, refetched+".meta.json", 10, time.Hour)

	r, err := SweepTranscripts(Policy{}, now)
	if err != nil {
		t.Fatal(err)
	}
	if r.Removed != 2 {
		t.Fatalf("removed %d, want 2", r.Removed)
	}
	for _, ext := range []string{".jsonl", ".md", ".meta.json"} {
		if exists(old+ext) || exists(gone+ext) {
			t.Errorf("an old transcript's %s was kept", ext)
		}
		if !exists(fresh+ext) || !exists(refetched+ext) {
			t.Errorf("a recent transcript's %s was removed", ext)
		}
	}
	if exists(filepath.Join(TranscriptDir(), "falcon")) {
		t.Error("an emptied machine folder was left behind")
	}
	if !exists(TranscriptDir()) {
		t.Error("the folder itself was removed")
	}
}

func TestTranscriptsOverTheSizeLimitLoseTheLeastRecentlyFetched(t *testing.T) {
	t.Setenv("VINEYARD_DIR", t.TempDir())
	const mb = 1 << 20
	a := transcript(t, "hawk", "a", 600<<10, 3*time.Hour)
	b := transcript(t, "hawk", "b", 600<<10, 2*time.Hour)
	c := transcript(t, "hawk", "c", 600<<10, time.Hour)
	if _, err := SweepTranscripts(Policy{TranscriptMB: 1}, now); err != nil {
		t.Fatal(err)
	}
	if exists(a+".jsonl") || exists(b+".jsonl") || !exists(c+".jsonl") {
		t.Fatalf("kept a=%v b=%v c=%v, want only c", exists(a+".jsonl"), exists(b+".jsonl"), exists(c+".jsonl"))
	}
	// The newest stays even when it alone is over the limit: it is the one just fetched.
	big := transcript(t, "hawk", "big", 2*mb, 0)
	if _, err := SweepTranscripts(Policy{TranscriptMB: 1}, now); err != nil {
		t.Fatal(err)
	}
	if !exists(big + ".jsonl") {
		t.Fatal("the transcript just fetched was evicted")
	}
}

func TestReceivedFilesKeepLiveSessionsAndClearStalePartials(t *testing.T) {
	t.Setenv("VINEYARD_DIR", t.TempDir())
	root := ReceivedDir()
	oldDone := filepath.Join(root, "ended", "report.pdf")
	newDone := filepath.Join(root, "ended", "today.txt")
	oldLive := filepath.Join(root, "running", "data.csv")
	stale := filepath.Join(root, ".partial", "transfer-a")
	active := filepath.Join(root, ".partial", "transfer-b")
	put(t, oldDone, 10, 20*24*time.Hour)
	put(t, newDone, 10, time.Hour)
	put(t, oldLive, 10, 20*24*time.Hour)
	put(t, stale, 10, 2*time.Hour)
	put(t, active, 10, time.Minute)
	if runtime.GOOS != "windows" {
		// A link out of the folder is neither followed nor its target touched.
		outside := filepath.Join(t.TempDir(), "keep.txt")
		put(t, outside, 10, 90*24*time.Hour)
		if err := os.Symlink(outside, filepath.Join(root, "ended", "link")); err != nil {
			t.Fatal(err)
		}
		defer func() {
			if !exists(outside) {
				t.Error("a sweep removed a file outside its folder through a link")
			}
		}()
	}
	r, err := SweepReceived(Policy{}, now, func(s string) bool { return s == "running" })
	if err != nil {
		t.Fatal(err)
	}
	if exists(oldDone) || !exists(newDone) || !exists(oldLive) || exists(stale) || !exists(active) {
		t.Fatalf("after sweep: old=%v new=%v live=%v stale=%v active=%v (removed %d)", exists(oldDone), exists(newDone), exists(oldLive), exists(stale), exists(active), r.Removed)
	}
}

func TestNegativeKeepsForEver(t *testing.T) {
	t.Setenv("VINEYARD_DIR", t.TempDir())
	tr := transcript(t, "hawk", "ancient", 100, 400*24*time.Hour)
	f := filepath.Join(ReceivedDir(), "ended", "ancient.bin")
	put(t, f, 10, 400*24*time.Hour)
	p := Policy{TranscriptDays: -1, TranscriptMB: -1, ReceivedDays: -1}
	if _, err := SweepTranscripts(p, now); err != nil {
		t.Fatal(err)
	}
	if _, err := SweepReceived(p, now, nil); err != nil {
		t.Fatal(err)
	}
	if !exists(tr+".jsonl") || !exists(f) {
		t.Fatal("a negative limit should keep everything")
	}
}

func TestSweeperRateLimits(t *testing.T) {
	t.Setenv("VINEYARD_DIR", t.TempDir())
	runs := 0
	s := &Sweeper{Policy: func() Policy { runs++; return Policy{} }}
	s.Run(true)
	s.Run(false)
	s.Run(false)
	s.Run(true)
	if runs != 2 {
		t.Fatalf("swept %d times, want 2 (forced runs only, within the hour)", runs)
	}
}
