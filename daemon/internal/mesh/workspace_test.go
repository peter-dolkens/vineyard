package mesh

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/peter-dolkens/vineyard/daemon/internal/model"
	"github.com/peter-dolkens/vineyard/daemon/internal/protocol"
)

func mkws(t *testing.T, n *Node, path string, o *protocol.AgentOrigin) (protocol.MakeWorkspaceResult, error) {
	t.Helper()
	raw, err := n.makeWorkspace(protocol.MakeWorkspaceArgs{Path: path, Origin: o})
	var res protocol.MakeWorkspaceResult
	if err == nil {
		_ = json.Unmarshal(raw, &res)
	}
	return res, err
}

// A temporary home, with the node limited to it by default.
func homeNode(t *testing.T) (*Node, string) {
	t.Helper()
	n := newTestNode(t, "m1")
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	return n, home
}

func TestWorkspacesOnlyInsideTheAllowedFolder(t *testing.T) {
	n, home := homeNode(t)
	res, err := mkws(t, n, "~/Projects/new-thing", nil)
	want := filepath.Join(home, "Projects", "new-thing")
	if err != nil || !res.Created || res.Path != want {
		t.Fatalf("inside home: %+v, %v", res, err)
	}
	if st, err := os.Stat(want); err != nil || !st.IsDir() {
		t.Fatal("folder not created")
	}
	if res, err := mkws(t, n, want, nil); err != nil || !res.Existed || res.Created {
		t.Fatalf("existing folder: %+v, %v", res, err)
	}
	outside := filepath.Join(t.TempDir(), "elsewhere")
	for _, p := range []string{outside, "~", home, "~/../escape", filepath.Join(home, "a", "..", "..", "escape"), "relative/path", ""} {
		if _, err := mkws(t, n, p, nil); err == nil {
			t.Errorf("%q was allowed", p)
		}
	}
	if _, err := os.Stat(outside); err == nil {
		t.Error("a folder outside was created")
	}
	// A file where the folder should be.
	if err := os.WriteFile(filepath.Join(home, "file"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := mkws(t, n, "~/file", nil); err == nil || !strings.Contains(err.Error(), "not a folder") {
		t.Errorf("a file was taken for a folder: %v", err)
	}
}

func TestWorkspaceLinksCannotLeadOut(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}
	n, home := homeNode(t)
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(home, "door")); err != nil {
		t.Fatal(err)
	}
	if _, err := mkws(t, n, "~/door/new", nil); err == nil || !strings.Contains(err.Error(), "outside") {
		t.Fatalf("a link out of home was followed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outside, "new")); err == nil {
		t.Fatal("a folder was created through the link")
	}
}

func TestWorkspacesInsideSettingAndAgentGate(t *testing.T) {
	n, home := homeNode(t)
	projects := filepath.Join(home, "Projects")
	if err := os.MkdirAll(projects, 0o755); err != nil {
		t.Fatal(err)
	}
	args, _ := json.Marshal(protocol.AgentToolsArgs{Tools: []string{model.AgentToolNewWorkspaces}, At: 10, WorkspacesInside: "~/Projects"})
	raw, err := n.handleLocal(protocol.Request{Op: "agenttools", Args: args})
	if err != nil {
		t.Fatal(err)
	}
	var st model.AgentToolsStatus
	_ = json.Unmarshal(raw, &st)
	if st.WorkspacesInside != "~/Projects" {
		t.Fatalf("status = %+v", st)
	}
	if _, err := mkws(t, n, "~/notes", nil); err == nil {
		t.Fatal("outside ~/Projects was allowed")
	}
	o := &protocol.AgentOrigin{Machine: "studio", Session: "s0", Hops: 1}
	if res, err := mkws(t, n, "~/Projects/app", o); err != nil || !res.Created {
		t.Fatalf("inside ~/Projects for an agent: %+v, %v", res, err)
	}
	// Off for agents once the setting no longer includes it.
	off, _ := json.Marshal(protocol.AgentToolsArgs{Tools: []string{}, At: 20})
	if _, err := n.handleLocal(protocol.Request{Op: "agenttools", Args: off}); err != nil {
		t.Fatal(err)
	}
	if _, err := mkws(t, n, "~/Projects/app2", o); err == nil || !strings.Contains(err.Error(), "createWorkspaces") {
		t.Fatalf("setting off: %v", err)
	}
}

func TestStrictlyInside(t *testing.T) {
	root := filepath.FromSlash("/home/u")
	for p, want := range map[string]bool{"/home/u/a": true, "/home/u/a/b": true, "/home/u": false, "/home/u2": false, "/home": false, "/home/u/../x": false, "/home/u/..x": true} {
		if got := strictlyInside(root, filepath.Clean(filepath.FromSlash(p))); got != want {
			t.Errorf("strictlyInside(%s) = %v, want %v", p, got, want)
		}
	}
}
