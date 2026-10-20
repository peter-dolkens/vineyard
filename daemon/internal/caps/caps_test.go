package caps

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestValid(t *testing.T) {
	for _, ok := range []string{"gh", "python3.12", "apt-get", "g++", "a_b"} {
		if !Valid(ok) {
			t.Errorf("Valid(%q) = false", ok)
		}
	}
	for _, bad := range []string{"", ".", "..", "a/b", `a\b`, "a b", "$(id)", "a;b", "`x`", "x\n"} {
		if Valid(bad) {
			t.Errorf("Valid(%q) = true", bad)
		}
	}
}

func TestLookupUsesPathAndSkipsInvalid(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix PATH semantics")
	}
	dir := t.TempDir()
	write := func(name string, mode os.FileMode) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"), mode); err != nil {
			t.Fatal(err)
		}
	}
	write("fakecap", 0o755)
	write("notexec", 0o644)
	mu.Lock()
	pathVal, pathAt = dir, time.Now()
	mu.Unlock()
	t.Cleanup(func() {
		mu.Lock()
		pathVal = ""
		mu.Unlock()
	})
	got := Lookup([]string{"fakecap", "notexec", "missing", "../fakecap"})
	if len(got) != 1 || got["fakecap"] != filepath.Join(dir, "fakecap") {
		t.Fatalf("Lookup = %v", got)
	}
}
