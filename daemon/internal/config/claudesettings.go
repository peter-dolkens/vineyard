package config

import (
	"bytes"
	"os"
)

// ClaudeSettingsFile holds the Claude Code settings Vineyard gives the sessions it starts, in
// ~/.vineyard beside config.json. Sessions get it as --settings, which sits on top of the user's own
// user, project and local settings files.
const ClaudeSettingsFile = "claude-settings.json"

// claudeSettings is that file's contents. Vineyard is the window on the sessions it starts, so they
// are asked for everything there is to show: thinking summaries from the API, verbose tool output
// and the verbose view. Vineyard owns the file and rewrites it whenever this changes, so there is
// one place to add to when a Vineyard session should run differently from a hand-started one.
const claudeSettings = `{
  "showThinkingSummaries": true,
  "verbose": true,
  "viewMode": "verbose"
}
`

// ClaudeSettingsPath is where the file lives on this machine.
func ClaudeSettingsPath() string { return Path(ClaudeSettingsFile) }

// WriteClaudeSettings puts the file on disk if it is missing or out of date and returns its path.
// Claude Code refuses to start on a --settings path that does not exist, so a caller that cannot
// write it should start the session without the flag rather than not at all.
func WriteClaudeSettings() (string, error) {
	path := ClaudeSettingsPath()
	if b, err := os.ReadFile(path); err == nil && bytes.Equal(b, []byte(claudeSettings)) {
		return path, nil
	}
	if err := os.MkdirAll(Dir(), 0o700); err != nil {
		return "", err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(claudeSettings), 0o600); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, path); err != nil {
		return "", err
	}
	return path, nil
}

// EnsureClaudeSettings is WriteClaudeSettings for a caller with nowhere to put an error: the path,
// or "" when the file is not there and cannot be put there. Since it writes what is missing, a
// caller that asks each time it reports the path never advertises a file that has been deleted.
func EnsureClaudeSettings() string {
	path, err := WriteClaudeSettings()
	if err != nil {
		return ""
	}
	return path
}
