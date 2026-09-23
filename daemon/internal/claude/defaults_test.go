package claude

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSettingsDefaultsLayersFiles(t *testing.T) {
	home, ws := t.TempDir(), t.TempDir()
	write := func(path, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(home, "settings.json"), `{"model":"claude-fable-5-1[1m]","effortLevel":"high","permissions":{"defaultMode":"acceptEdits"}}`)
	write(filepath.Join(ws, ".claude", "settings.json"), `{"model":"sonnet"}`)
	write(filepath.Join(ws, ".claude", "settings.local.json"), `{"permissions":{"defaultMode":"auto"}}`)

	got := SettingsDefaults(home, ws)
	want := Defaults{Model: "sonnet", Effort: "high", PermissionMode: "auto"}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	if got := SettingsDefaults(home, ""); got.Model != "claude-fable-5-1[1m]" || got.PermissionMode != "acceptEdits" {
		t.Fatalf("user settings only: got %+v", got)
	}
}

func TestSettingsDefaultsToleratesMissingAndBrokenFiles(t *testing.T) {
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "settings.json"), []byte(`{not json`), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := SettingsDefaults(home, filepath.Join(home, "nope")); got != (Defaults{}) {
		t.Fatalf("got %+v, want empty", got)
	}
}
