package agenttools

import (
	"encoding/json"
	"path/filepath"
	"runtime"
	"strings"
)

// SpawnArgs is what a session's command line gains when its machine gives it any agent tools: the
// MCP server (this binary, `mcp --session <id>`) as --mcp-config, and --allowedTools for the tools
// that only read, plus Read on the folder fetch_transcript saves to. Nothing when tools is empty.
func SpawnArgs(exe, vineyardDir, sessionID string, tools []string) []string {
	if len(tools) == 0 || exe == "" {
		return nil
	}
	cfg := map[string]any{"mcpServers": map[string]any{ServerName: map[string]any{
		"type":    "stdio",
		"command": exe,
		"args":    []string{"mcp", "--session", sessionID},
		"env":     map[string]string{"VINEYARD_DIR": vineyardDir},
	}}}
	b, _ := json.Marshal(cfg)
	args := []string{"--mcp-config", string(b)}
	for _, t := range ReadOnlyTools() {
		args = append(args, "--allowedTools", "mcp__"+ServerName+"__"+t)
	}
	if rule := readRule(TranscriptDir(vineyardDir)); rule != "" {
		args = append(args, "--allowedTools", rule)
	}
	return args
}

// TranscriptDir is where fetch_transcript saves transcripts.
func TranscriptDir(vineyardDir string) string { return filepath.Join(vineyardDir, "transcripts") }

// readRule is a permission rule allowing Read under dir. Claude Code writes an absolute path with a
// leading "//"; on Windows the drive's form is not settled, so none is given there and each read asks.
func readRule(dir string) string {
	if runtime.GOOS == "windows" || !filepath.IsAbs(dir) {
		return ""
	}
	return "Read(/" + filepath.ToSlash(strings.TrimRight(dir, "/")) + "/**)"
}
