package claude

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// AppendCustomTitle renames a session the way Claude Code's /rename does: by appending a custom-title
// line to its transcript. Claude Code (and Vineyard) read the last one. The path must be a transcript
// inside the Claude projects directory.
func AppendCustomTitle(claudeDir, path, sessionID, title string) error {
	title = strings.TrimSpace(title)
	if title == "" {
		return errors.New("title is empty")
	}
	projects := filepath.Join(claudeDir, "projects")
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	if rel, err := filepath.Rel(projects, abs); err != nil || strings.HasPrefix(rel, "..") || !strings.HasSuffix(abs, ".jsonl") {
		return errors.New("transcript path must be inside the Claude projects directory")
	}
	if _, err := os.Stat(abs); err != nil {
		return err
	}
	line, err := json.Marshal(map[string]string{"type": "custom-title", "customTitle": title, "sessionId": sessionID})
	if err != nil {
		return err
	}
	f, err := os.OpenFile(abs, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(line, '\n'))
	return err
}
