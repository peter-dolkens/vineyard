package managed

import (
	"slices"
	"testing"
)

// Vineyard's settings file goes to every session it spawns, and a session still starts when there
// is none: Claude Code refuses a --settings path that is not there.
func TestSpawnArgsSettings(t *testing.T) {
	args := spawnArgs(SpawnOptions{Cwd: "/w"}, "sid", "/home/u/.vineyard/claude-settings.json")
	i := slices.Index(args, "--settings")
	if i < 0 || i+1 >= len(args) || args[i+1] != "/home/u/.vineyard/claude-settings.json" {
		t.Fatalf("--settings missing from %v", args)
	}
	if bare := spawnArgs(SpawnOptions{Cwd: "/w"}, "sid", ""); slices.Contains(bare, "--settings") {
		t.Fatalf("--settings should be left off without a file: %v", bare)
	}
}

func TestSpawnArgsNewAndResume(t *testing.T) {
	fresh := spawnArgs(SpawnOptions{Cwd: "/w", Model: "opus", Effort: "high", PermissionMode: "auto", Name: "n", AllowedTools: []string{"Bash", "Read"}}, "sid", "")
	for _, want := range [][2]string{{"--session-id", "sid"}, {"--model", "opus"}, {"--effort", "high"}, {"--permission-mode", "auto"}, {"--name", "n"}, {"--allowedTools", "Bash"}} {
		if i := slices.Index(fresh, want[0]); i < 0 || fresh[i+1] != want[1] {
			t.Errorf("%s %s missing from %v", want[0], want[1], fresh)
		}
	}
	if slices.Contains(fresh, "--resume") {
		t.Errorf("a new session should not resume: %v", fresh)
	}

	resumed := spawnArgs(SpawnOptions{Cwd: "/w", Resume: "old"}, "old", "")
	if i := slices.Index(resumed, "--resume"); i < 0 || resumed[i+1] != "old" {
		t.Errorf("--resume old missing from %v", resumed)
	}
	if slices.Contains(resumed, "--session-id") {
		t.Errorf("a resumed session should not set --session-id: %v", resumed)
	}
}

func TestSpawnArgsAllowBypass(t *testing.T) {
	if args := spawnArgs(SpawnOptions{Cwd: "/w"}, "sid", ""); slices.Contains(args, "--allow-dangerously-skip-permissions") {
		t.Errorf("bypass should not be allowed unless asked for: %v", args)
	}
	args := spawnArgs(SpawnOptions{Cwd: "/w", AllowBypassPermissions: true}, "sid", "")
	if !slices.Contains(args, "--allow-dangerously-skip-permissions") {
		t.Errorf("--allow-dangerously-skip-permissions missing from %v", args)
	}
	if slices.Contains(args, "--dangerously-skip-permissions") || slices.Contains(args, "--permission-mode") {
		t.Errorf("allowing bypass should not switch it on: %v", args)
	}
}

func TestBypassAvailable(t *testing.T) {
	for _, c := range []struct {
		o    SpawnOptions
		want bool
	}{
		{SpawnOptions{}, false},
		{SpawnOptions{PermissionMode: "acceptEdits"}, false},
		{SpawnOptions{AllowBypassPermissions: true}, true},
		{SpawnOptions{PermissionMode: "bypassPermissions"}, true},
	} {
		if got := bypassAvailable(c.o); got != c.want {
			t.Errorf("bypassAvailable(%+v) = %v, want %v", c.o, got, c.want)
		}
	}
}
