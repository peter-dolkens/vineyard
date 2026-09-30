package mesh

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peter-dolkens/vineyard/daemon/internal/protocol"
)

func TestTranscriptPagesBackAndStreamsOnWithoutOverlap(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "projects", "-w", "s.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for i := 0; i < 23; i++ {
		fmt.Fprintf(&b, "{\"uuid\":\"e%d\"}\n", i)
	}
	b.WriteString(`{"uuid":"par`) // being written
	os.WriteFile(path, []byte(b.String()), 0o600)
	n := &Node{opts: Options{ClaudeDir: dir}}
	read := func(a protocol.TranscriptArgs) protocol.TranscriptData {
		t.Helper()
		a.Path = path
		raw, err := n.readTranscript(a)
		if err != nil {
			t.Fatal(err)
		}
		var d protocol.TranscriptData
		json.Unmarshal(raw, &d)
		return d
	}
	ids := func(d protocol.TranscriptData) []string {
		var out []string
		for _, e := range d.Entries {
			var v struct{ UUID string }
			json.Unmarshal(e, &v)
			out = append(out, v.UUID)
		}
		return out
	}

	tail := read(protocol.TranscriptArgs{Lines: 5})
	got := ids(tail)
	for start := tail.Start; start > 0; {
		page := read(protocol.TranscriptArgs{Lines: 5, Before: start})
		got = append(ids(page), got...)
		start = page.Start
	}
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString("tly\"}\n{\"uuid\":\"e24\"}\n")
	f.Close()
	got = append(got, ids(read(protocol.TranscriptArgs{Lines: 5, Offset: tail.Offset}))...)

	var want []string
	for i := 0; i < 23; i++ {
		want = append(want, fmt.Sprintf("e%d", i))
	}
	want = append(want, "partly", "e24")
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("got  %v\nwant %v", got, want)
	}

	if d := read(protocol.TranscriptArgs{Lines: 5, Before: tail.Start + 3}); !d.Truncated || len(d.Entries) != 0 {
		t.Fatalf("an offset inside a line should say the file changed: %+v", d)
	}
}
