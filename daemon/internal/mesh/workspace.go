package mesh

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/peter-dolkens/vineyard/daemon/internal/model"
	"github.com/peter-dolkens/vineyard/daemon/internal/protocol"
)

// DefaultWorkspacesInside is where new workspaces may go when the setting says nothing: the home
// folder of whoever runs the daemon.
const DefaultWorkspacesInside = "~"

// expandHome turns a leading ~ into home, so one fleet-wide setting means each machine's own home.
func expandHome(p, home string) string {
	if p == "~" {
		return home
	}
	if strings.HasPrefix(p, "~/") || strings.HasPrefix(p, `~\`) {
		return filepath.Join(home, p[2:])
	}
	return p
}

// resolveExisting follows the links in the part of path that exists, so a link inside the allowed
// folder cannot lead a new workspace out of it. What does not exist yet is appended as given.
func resolveExisting(path string) (string, error) {
	var rest []string
	p := path
	for {
		real, err := filepath.EvalSymlinks(p)
		if err == nil {
			for i := len(rest) - 1; i >= 0; i-- {
				real = filepath.Join(real, rest[i])
			}
			return real, nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(p)
		if parent == p {
			return "", fmt.Errorf("nothing of %s exists", path)
		}
		rest = append(rest, filepath.Base(p))
		p = parent
	}
}

// strictlyInside reports whether path is below root (not root itself). filepath.Rel compares
// without regard to case on Windows.
func strictlyInside(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == "." || filepath.IsAbs(rel) {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// workspaceRoot is the folder new workspaces must be inside on this machine, links resolved.
func (n *Node) workspaceRoot() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	n.mu.Lock()
	setting := n.cfg.WorkspacesInside
	n.mu.Unlock()
	if strings.TrimSpace(setting) == "" {
		setting = DefaultWorkspacesInside
	}
	root := filepath.Clean(expandHome(strings.TrimSpace(setting), home))
	if !filepath.IsAbs(root) {
		return "", fmt.Errorf("the allowed folder %q is not an absolute path", setting)
	}
	real, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("the allowed folder %s: %w", root, err)
	}
	return real, nil
}

// makeWorkspace creates a folder for a new workspace, only inside the allowed folder.
func (n *Node) makeWorkspace(a protocol.MakeWorkspaceArgs) (json.RawMessage, error) {
	if a.Origin != nil {
		if err := n.checkOrigin(model.AgentToolNewWorkspaces, "create workspaces", "createWorkspaces", a.Origin); err != nil {
			return nil, err
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	p := strings.TrimSpace(a.Path)
	if p == "" {
		return nil, errors.New("a path is required")
	}
	p = filepath.Clean(expandHome(p, home))
	if !filepath.IsAbs(p) {
		return nil, fmt.Errorf("%s is not an absolute path (a leading ~ means the home folder)", a.Path)
	}
	root, err := n.workspaceRoot()
	if err != nil {
		return nil, err
	}
	real, err := resolveExisting(p)
	if err != nil {
		return nil, err
	}
	if !strictlyInside(root, real) {
		return nil, fmt.Errorf("%s is outside %s, the only folder new workspaces may go in on %s (vineyard.agentTools.workspacesInside)", p, root, n.cfg.Name)
	}
	if st, err := os.Stat(p); err == nil {
		if !st.IsDir() {
			return nil, fmt.Errorf("%s exists and is not a folder", p)
		}
		return json.Marshal(protocol.MakeWorkspaceResult{Path: p, Existed: true})
	}
	if err := os.MkdirAll(p, 0o755); err != nil {
		return nil, err
	}
	// Check again now it exists, in case a link was swapped in meanwhile.
	if after, err := filepath.EvalSymlinks(p); err != nil || !strictlyInside(root, after) {
		return nil, fmt.Errorf("%s did not end up inside %s", p, root)
	}
	n.logf("created workspace %s", p)
	return json.Marshal(protocol.MakeWorkspaceResult{Path: p, Created: true})
}
