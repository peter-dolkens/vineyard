package mesh

import (
	"github.com/peter-dolkens/vineyard/daemon/internal/claude"
	"github.com/peter-dolkens/vineyard/daemon/internal/managed"
)

// spawnBasis rides along a spawn request next to the remembered model, effort and permission mode:
// for each field, the Claude Code settings default that was in force when the user picked it. A
// field without an entry was remembered by an older extension and is taken as is.
type spawnBasis struct {
	Basis map[string]string `json:"basis,omitempty"`
}

// dropStaleChoices clears each remembered choice whose settings default has changed since it was
// made: the user has since picked a new default for the machine or project, and that should win over
// a choice left behind in one workspace. It returns the names of the fields it cleared.
func dropStaleChoices(o *managed.SpawnOptions, basis map[string]string, now claude.Defaults) []string {
	var stale []string
	for _, f := range []struct {
		name string
		val  *string
	}{{"model", &o.Model}, {"effort", &o.Effort}, {"permissionMode", &o.PermissionMode}} {
		then, recorded := basis[f.name]
		cur, _ := now.Field(f.name)
		if *f.val != "" && recorded && then != cur {
			*f.val = ""
			stale = append(stale, f.name)
		}
	}
	return stale
}
