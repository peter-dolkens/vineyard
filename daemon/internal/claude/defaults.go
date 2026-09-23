package claude

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// Defaults is what a new session in a workspace starts with when given no flags: the model, effort
// level and permission mode from Claude Code's settings files. An empty field means the settings
// leave it to Claude Code's built-in default.
type Defaults struct {
	Model          string `json:"model"`
	Effort         string `json:"effort"`
	PermissionMode string `json:"permissionMode"`
}

// Field returns the named field ("model", "effort" or "permissionMode"); ok is false for any other name.
func (d Defaults) Field(name string) (value string, ok bool) {
	switch name {
	case "model":
		return d.Model, true
	case "effort":
		return d.Effort, true
	case "permissionMode":
		return d.PermissionMode, true
	}
	return "", false
}

// SettingsDefaults reads the user, project and local settings files in Claude Code's order (a later
// file overrides an earlier one field by field). Enterprise-managed settings are not read. Missing or
// unreadable files count as empty.
func SettingsDefaults(claudeDir, cwd string) Defaults {
	var d Defaults
	files := []string{filepath.Join(claudeDir, "settings.json")}
	if cwd != "" {
		files = append(files, filepath.Join(cwd, ".claude", "settings.json"), filepath.Join(cwd, ".claude", "settings.local.json"))
	}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var s struct {
			Model       string `json:"model"`
			EffortLevel string `json:"effortLevel"`
			Permissions struct {
				DefaultMode string `json:"defaultMode"`
			} `json:"permissions"`
		}
		if json.Unmarshal(b, &s) != nil {
			continue
		}
		if s.Model != "" {
			d.Model = s.Model
		}
		if s.EffortLevel != "" {
			d.Effort = s.EffortLevel
		}
		if s.Permissions.DefaultMode != "" {
			d.PermissionMode = s.Permissions.DefaultMode
		}
	}
	return d
}
