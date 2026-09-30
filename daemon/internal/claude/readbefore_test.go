package claude

import (
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// pageBack reads the tail, then pages back with ReadBefore until the start, returning the lines in
// file order and the tail's next offset.
func pageBack(t *testing.T, path string, n int, maxBytes int64) ([]string, int64) {
	t.Helper()
	lines, start, next, _, err := ReadBefore(path, -1, n, maxBytes)
	if err != nil {
		t.Fatal(err)
	}
	got := toStrings(lines)
	for guard := 0; start > 0; guard++ {
		if guard > 100000 {
			t.Fatal("paging back did not reach the start")
		}
		older, s, _, _, err := ReadBefore(path, start, n, maxBytes)
		if err != nil {
			t.Fatalf("ReadBefore(%d): %v", start, err)
		}
		if s >= start {
			t.Fatalf("ReadBefore(%d) made no progress (start %d)", start, s)
		}
		got = append(toStrings(older), got...)
		start = s
	}
	return got, next
}

func toStrings(b [][]byte) []string {
	out := make([]string, len(b))
	for i, l := range b {
		out[i] = string(l)
	}
	return out
}

func write(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "t.jsonl")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestReadBeforeTailAndPages(t *testing.T) {
	path := write(t, "a\nb\n\nc\nd\ne\n")
	lines, start, next, size, err := ReadBefore(path, -1, 2, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(toStrings(lines), ","); got != "d,e" || next != size || start != 7 {
		t.Fatalf("tail = %q start %d next %d size %d", got, start, next, size)
	}
	lines, start, _, _, _ = ReadBefore(path, start, 2, 1<<20)
	if got := strings.Join(toStrings(lines), ","); got != "b,c" || start != 2 {
		t.Fatalf("page = %q start %d", got, start)
	}
	lines, start, _, _, _ = ReadBefore(path, start, 2, 1<<20)
	if got := strings.Join(toStrings(lines), ","); got != "a" || start != 0 {
		t.Fatalf("last page = %q start %d", got, start)
	}
}

func TestReadBeforeLeavesUnfinishedLineForReadFrom(t *testing.T) {
	path := write(t, "one\ntwo\nthr")
	lines, _, next, _, err := ReadBefore(path, -1, 10, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(toStrings(lines), ","); got != "one,two" || next != 8 {
		t.Fatalf("tail = %q next %d", got, next)
	}
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString("ee\nfour\n")
	f.Close()
	more, _, _, err := ReadFrom(path, next, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(toStrings(more), ","); got != "three,four" {
		t.Fatalf("ReadFrom = %q", got)
	}
}

func TestReadBeforeRefusesAnOffsetInsideALine(t *testing.T) {
	path := write(t, "one\ntwo\n")
	if _, _, _, _, err := ReadBefore(path, 5, 10, 1<<20); !errors.Is(err, ErrNotLineStart) {
		t.Fatalf("mid-line offset: %v", err)
	}
	if _, _, _, _, err := ReadBefore(path, 99, 10, 1<<20); !errors.Is(err, ErrNotLineStart) {
		t.Fatalf("offset past the end: %v", err)
	}
}

func TestReadBeforeSkipsALineLongerThanTheWindow(t *testing.T) {
	path := write(t, "a\n"+strings.Repeat("x", 100)+"\nb\n")
	got, _ := pageBack(t, path, 10, 16)
	if strings.Join(got, ",") != "a,b" {
		t.Fatalf("got %q", got)
	}
}

// Every line exactly once, in order, whatever the page size, window and line lengths, and a live
// ReadFrom from the tail's offset carries on with no overlap.
func TestReadBeforePagesCoverTheFileExactlyOnce(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	for round := 0; round < 300; round++ {
		var want []string
		var b strings.Builder
		for i, count := 0, rng.Intn(60); i < count; i++ {
			switch rng.Intn(8) {
			case 0:
				b.WriteString("\n") // blank lines are skipped, not returned
				continue
			case 1:
				b.WriteString("  \r\n")
				continue
			}
			line := fmt.Sprintf(`{"i":%d,"pad":"%s"}`, i, strings.Repeat("p", rng.Intn(40)))
			want = append(want, line)
			if rng.Intn(5) == 0 {
				b.WriteString(line + "\r\n")
			} else {
				b.WriteString(line + "\n")
			}
		}
		path := write(t, b.String())
		n := 1 + rng.Intn(7)
		maxBytes := int64(80 + rng.Intn(400)) // wider than any line: none is skipped
		got, next := pageBack(t, path, n, maxBytes)
		if strings.Join(got, "|") != strings.Join(want, "|") {
			t.Fatalf("round %d (n=%d, max=%d):\n got %q\nwant %q", round, n, maxBytes, got, want)
		}
		f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
		f.WriteString("{\"live\":1}\n")
		f.Close()
		live, _, _, err := ReadFrom(path, next, 1<<20)
		if err != nil || strings.Join(toStrings(live), "|") != `{"live":1}` {
			t.Fatalf("round %d: live read after the tail = %q, %v", round, toStrings(live), err)
		}
	}
}
