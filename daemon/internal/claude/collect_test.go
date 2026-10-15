package claude

import (
	"os"
	"path/filepath"
	"testing"
)

// The reported path follows the file: gone means not reported, and a reporter that writes what is
// missing puts it back.
func TestCollectorReportsOnlyALivingSettingsFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "claude-settings.json")
	c := NewCollector(t.TempDir(), 80)
	c.VineyardSettings = func() string {
		if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
			return ""
		}
		return path
	}
	if got := c.Collect().Host.VineyardSettings; got != path {
		t.Fatalf("reported %q, want %q", got, path)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("file not written: %v", err)
	}
	os.Remove(path)
	if got := c.Collect().Host.VineyardSettings; got != path {
		t.Fatalf("a deleted file was not written again: %q", got)
	}

	bare := NewCollector(t.TempDir(), 80)
	if got := bare.Collect().Host.VineyardSettings; got != "" {
		t.Fatalf("a collector with no reporter said %q", got)
	}
}
