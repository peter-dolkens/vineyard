package mesh

import (
	"reflect"
	"testing"

	"github.com/peter-dolkens/vineyard/daemon/internal/claude"
	"github.com/peter-dolkens/vineyard/daemon/internal/managed"
)

func TestDropStaleChoices(t *testing.T) {
	now := claude.Defaults{Model: "claude-fable-5-1[1m]", Effort: "high"}
	o := managed.SpawnOptions{Model: "opus[1m]", Effort: "max", PermissionMode: "auto"}
	// The model was picked while the default was Opus and the default has since moved to Fable: stale.
	// The effort was picked under today's default: kept. The mode has no basis (older extension): kept.
	stale := dropStaleChoices(&o, map[string]string{"model": "opus", "effort": "high"}, now)
	if !reflect.DeepEqual(stale, []string{"model"}) {
		t.Fatalf("stale = %v", stale)
	}
	if o.Model != "" || o.Effort != "max" || o.PermissionMode != "auto" {
		t.Fatalf("options after drop: %+v", o)
	}
}

func TestDropStaleChoicesEmptyBasisMatchesUnsetDefault(t *testing.T) {
	o := managed.SpawnOptions{PermissionMode: "auto"}
	if stale := dropStaleChoices(&o, map[string]string{"permissionMode": ""}, claude.Defaults{}); stale != nil || o.PermissionMode != "auto" {
		t.Fatalf("stale = %v, options %+v", stale, o)
	}
	if stale := dropStaleChoices(&o, map[string]string{"permissionMode": ""}, claude.Defaults{PermissionMode: "plan"}); len(stale) != 1 || o.PermissionMode != "" {
		t.Fatalf("stale = %v, options %+v", stale, o)
	}
}
