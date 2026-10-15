package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestWriteClaudeSettings(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VINEYARD_DIR", dir)

	path, err := WriteClaudeSettings()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dir, ClaudeSettingsFile); path != want {
		t.Fatalf("path = %s, want %s", path, want)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Claude Code parses this file, so it has to be JSON, with the settings Vineyard means to set.
	var s struct {
		ShowThinkingSummaries bool   `json:"showThinkingSummaries"`
		Verbose               bool   `json:"verbose"`
		ViewMode              string `json:"viewMode"`
	}
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	if !s.ShowThinkingSummaries || !s.Verbose || s.ViewMode != "verbose" {
		t.Fatalf("settings lost a field: %+v", s)
	}

	// A stale file is brought back up to date; an up-to-date one is left alone.
	if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := WriteClaudeSettings(); err != nil {
		t.Fatal(err)
	}
	now, _ := os.ReadFile(path)
	if string(now) != string(b) {
		t.Fatalf("stale file not rewritten: %s", now)
	}
	st, _ := os.Stat(path)
	if _, err := WriteClaudeSettings(); err != nil {
		t.Fatal(err)
	}
	st2, _ := os.Stat(path)
	if !st.ModTime().Equal(st2.ModTime()) {
		t.Errorf("an up-to-date file was rewritten")
	}
}
