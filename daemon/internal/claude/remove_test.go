package claude

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// fixture lays out one project's transcripts the way Claude Code does, and returns the claude dir.
func fixture(t *testing.T, cwd string) string {
	t.Helper()
	dir := t.TempDir()
	write := func(path, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	proj := filepath.Join(dir, "projects", EncodeProjectDir(cwd))
	for _, sid := range []string{"aaaaaaaa-0000-0000-0000-000000000000", "bbbbbbbb-0000-0000-0000-000000000000"} {
		write(filepath.Join(proj, sid+".jsonl"), fmt.Sprintf(`{"type":"user","cwd":%q,"sessionId":%q}`+"\n", cwd, sid))
		write(filepath.Join(SubagentsDir(filepath.Join(proj, sid+".jsonl")), "agent-1.jsonl"), "{}\n")
		write(filepath.Join(SubagentsDir(filepath.Join(proj, sid+".jsonl")), "agent-1.meta.json"), "{}\n")
	}
	// Saved tool results share the sidecar folder with the subagent transcripts.
	write(filepath.Join(proj, "aaaaaaaa-0000-0000-0000-000000000000", "tool-results", "bbep7pqd5.txt"), "ok\n")
	// The project's memory notes are the user's, not a transcript: they must survive.
	write(filepath.Join(proj, "memory", "MEMORY.md"), "- a note\n")
	// An exited session's registry entry and the key file beside it: a pid nothing is running under.
	write(filepath.Join(dir, "sessions", "999999.json"), fmt.Sprintf(`{"sessionId":"aaaaaaaa-0000-0000-0000-000000000000","cwd":%q}`, cwd))
	write(filepath.Join(dir, "sessions", "999999.e751d5cb.key"), "k\n")
	for _, sid := range []string{"aaaaaaaa-0000-0000-0000-000000000000", "bbbbbbbb-0000-0000-0000-000000000000"} {
		write(filepath.Join(dir, "session-env", sid, "env.json"), "{}\n")
		write(filepath.Join(dir, "file-history", sid, "1dbe7881927c6f50@v1"), "old\n")
	}
	// Another project that must survive.
	other := filepath.Join(dir, "projects", EncodeProjectDir("/elsewhere"))
	write(filepath.Join(other, "cccccccc-0000-0000-0000-000000000000.jsonl"), `{"type":"user","cwd":"/elsewhere"}`+"\n")
	return dir
}

func TestRemoveSessionTakesSubagentsAndRegistry(t *testing.T) {
	cwd := "/Users/p/Projects/vineyard"
	dir := fixture(t, cwd)
	proj := filepath.Join(dir, "projects", EncodeProjectDir(cwd))
	sid := "aaaaaaaa-0000-0000-0000-000000000000"
	path := filepath.Join(proj, sid+".jsonl")

	got, err := RemoveSession(dir, path, sid)
	if err != nil {
		t.Fatal(err)
	}
	if got.Transcripts != 1 || got.Subagents != 1 || got.Registry != 1 || got.Bytes == 0 {
		t.Fatalf("removed %+v", got)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("transcript still there")
	}
	if _, err := os.Stat(filepath.Join(proj, sid)); !os.IsNotExist(err) {
		t.Fatal("subagent folder still there")
	}
	if _, err := os.Stat(filepath.Join(dir, "sessions", "999999.json")); !os.IsNotExist(err) {
		t.Fatal("registry entry still there")
	}
	for _, p := range []string{
		filepath.Join(dir, "sessions", "999999.e751d5cb.key"),
		filepath.Join(dir, "session-env", sid),
		filepath.Join(dir, "file-history", sid),
	} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("%s still there", p)
		}
	}
	// The session beside it, and the project's memory notes, are untouched.
	if _, err := os.Stat(filepath.Join(proj, "bbbbbbbb-0000-0000-0000-000000000000.jsonl")); err != nil {
		t.Fatal("removed the wrong session")
	}
	for _, p := range []string{
		filepath.Join(proj, "memory", "MEMORY.md"),
		filepath.Join(dir, "session-env", "bbbbbbbb-0000-0000-0000-000000000000", "env.json"),
	} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("%s was removed", p)
		}
	}
}

func TestRemoveSessionRefusesPathsOutsideProjects(t *testing.T) {
	dir := fixture(t, "/Users/p/Projects/vineyard")
	outside := filepath.Join(dir, "secrets.jsonl")
	if err := os.WriteFile(outside, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{outside, filepath.Join(dir, "projects", "..", "secrets.jsonl"), filepath.Join(dir, "projects", "x", "notes.txt")} {
		if _, err := RemoveSession(dir, p, ""); err == nil {
			t.Fatalf("%s was accepted", p)
		}
	}
	if _, err := os.Stat(outside); err != nil {
		t.Fatal("file outside projects was deleted")
	}
}

func TestRemoveSessionRefusesAMismatchedSessionID(t *testing.T) {
	cwd := "/Users/p/Projects/vineyard"
	dir := fixture(t, cwd)
	path := filepath.Join(dir, "projects", EncodeProjectDir(cwd), "aaaaaaaa-0000-0000-0000-000000000000.jsonl")
	if _, err := RemoveSession(dir, path, "bbbbbbbb-0000-0000-0000-000000000000"); err == nil {
		t.Fatal("path and session id were allowed to disagree")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("transcript was deleted anyway")
	}
}

func TestRemoveSessionRefusesALiveSession(t *testing.T) {
	cwd := "/Users/p/Projects/vineyard"
	dir := fixture(t, cwd)
	sid := "aaaaaaaa-0000-0000-0000-000000000000"
	// Our own pid is certainly alive.
	live := filepath.Join(dir, "sessions", fmt.Sprintf("%d.json", os.Getpid()))
	if err := os.WriteFile(live, []byte(fmt.Sprintf(`{"sessionId":%q,"cwd":%q}`, sid, cwd)), 0o644); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "projects", EncodeProjectDir(cwd), sid+".jsonl")
	if _, err := RemoveSession(dir, path, sid); err == nil {
		t.Fatal("a running session was removed")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("transcript of a running session was deleted")
	}
}

func TestRemoveProjectTakesScratchpadsAndLeavesOthers(t *testing.T) {
	cwd := "/Users/p/Projects/vineyard"
	dir := fixture(t, cwd)
	// A session that ran in this project's scratchpad gets its own projects/ folder.
	scratch := "/private/tmp/claude-501/" + EncodeProjectDir(cwd) + "/dddddddd-0000-0000-0000-000000000000/scratchpad"
	sdir := filepath.Join(dir, "projects", EncodeProjectDir(scratch))
	if err := os.MkdirAll(sdir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sdir, "dddddddd-0000-0000-0000-000000000000.jsonl"), []byte(fmt.Sprintf(`{"type":"user","cwd":%q}`+"\n", scratch)), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := RemoveProject(dir, cwd)
	if err != nil {
		t.Fatal(err)
	}
	if got.Transcripts != 3 || got.Subagents != 2 || got.Registry != 1 {
		t.Fatalf("removed %+v", got)
	}
	proj := filepath.Join(dir, "projects", EncodeProjectDir(cwd))
	if ents, err := os.ReadDir(proj); err != nil || len(ents) != 1 || ents[0].Name() != "memory" {
		t.Fatalf("project folder should keep memory/ and nothing else: %v %v", ents, err)
	}
	if _, err := os.Stat(filepath.Join(proj, "memory", "MEMORY.md")); err != nil {
		t.Fatal("memory notes were removed with the transcripts")
	}
	for _, sid := range []string{"aaaaaaaa-0000-0000-0000-000000000000", "bbbbbbbb-0000-0000-0000-000000000000"} {
		if _, err := os.Stat(filepath.Join(dir, "session-env", sid)); !os.IsNotExist(err) {
			t.Fatalf("session-env for %s still there", sid)
		}
	}
	if _, err := os.Stat(sdir); !os.IsNotExist(err) {
		t.Fatal("scratchpad project still there")
	}
	if _, err := os.Stat(filepath.Join(dir, "projects", EncodeProjectDir("/elsewhere"), "cccccccc-0000-0000-0000-000000000000.jsonl")); err != nil {
		t.Fatal("another project was removed")
	}
	if _, err := RemoveProject(dir, cwd); err == nil {
		t.Fatal("removing a project twice should report there is nothing left to remove")
	}
}

func TestRemoveProjectRefusesALiveSession(t *testing.T) {
	cwd := "/Users/p/Projects/vineyard"
	dir := fixture(t, cwd)
	live := filepath.Join(dir, "sessions", fmt.Sprintf("%d.json", os.Getpid()))
	if err := os.WriteFile(live, []byte(fmt.Sprintf(`{"sessionId":"eeeeeeee-0000-0000-0000-000000000000","cwd":%q}`, cwd)), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := RemoveProject(dir, cwd); err == nil {
		t.Fatal("a project with a running session was removed")
	}
	if _, err := os.Stat(filepath.Join(dir, "projects", EncodeProjectDir(cwd))); err != nil {
		t.Fatal("project folder was deleted anyway")
	}
}
