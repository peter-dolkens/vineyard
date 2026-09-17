package claude

import (
	"os"
	"path/filepath"
	"testing"
)

func TestScratchpadParentDecodesAgainstFilesystem(t *testing.T) {
	root := t.TempDir()
	proj := filepath.Join(root, "peter-dolkens", "Projects", "vine-yard")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	// A decoy that would win under naive splitting.
	_ = os.MkdirAll(filepath.Join(root, "peter"), 0o755)
	enc := EncodeProjectDir(proj)
	if got := DecodeProjectDir(enc); got != proj {
		t.Fatalf("decode %q: got %q want %q", enc, got, proj)
	}
	scratch := "/private/tmp/claude-501/" + enc + "/efdedde1-1792-4e3e-af62-a76eb0ae1c15/scratchpad/ctltest"
	got, ok := ScratchpadParent(scratch, nil)
	if !ok || got != proj {
		t.Fatalf("ScratchpadParent(%q) = %q,%v", scratch, got, ok)
	}
	// Known projects take precedence and need no filesystem.
	got, ok = ScratchpadParent(`C:\Users\P\AppData\Local\Temp\claude-1\C--Projects-House\efdedde1-1792-4e3e-af62-a76eb0ae1c15\scratchpad`, map[string]string{"C--Projects-House": `C:\Projects\House`})
	if !ok || got != `C:\Projects\House` {
		t.Fatalf("known lookup: %q,%v", got, ok)
	}
	if _, ok := ScratchpadParent("/Users/x/Projects/y", nil); ok {
		t.Fatal("ordinary path flagged as scratchpad")
	}
}
