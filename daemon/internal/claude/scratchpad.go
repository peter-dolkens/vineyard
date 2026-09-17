package claude

import (
	"os"
	"regexp"
	"strings"
)

// Claude Code gives each session a scratchpad directory of the form
//
//	<tmp>/claude-<uid>/<EncodeProjectDir(project cwd)>/<session id>/scratchpad
//
// Agents that run there belong to the project, not to a workspace called "scratchpad".
var scratchRe = regexp.MustCompile(`(?i)[\\/]claude-[^\\/]+[\\/]([^\\/]+)[\\/][0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}[\\/]scratchpad(?:[\\/]|$)`)

// ScratchpadParent reports whether path is inside a session scratchpad and, if so, the project it
// belongs to. known maps EncodeProjectDir(cwd) to cwd for projects this machine already knows; when
// the segment is not among them it is decoded against the filesystem (the encoding is lossy: "-" may
// be a separator or part of a name, so each split is tried and checked with os.Stat).
func ScratchpadParent(path string, known map[string]string) (string, bool) {
	m := scratchRe.FindStringSubmatch(path)
	if m == nil {
		return "", false
	}
	enc := m[1]
	if cwd, ok := known[enc]; ok {
		return cwd, true
	}
	if cwd := DecodeProjectDir(enc); cwd != "" {
		return cwd, true
	}
	return "", false
}

// DecodeProjectDir inverts EncodeProjectDir by walking the filesystem: at each level the directory
// entries whose encoded names prefix the remaining segment are tried, so any character the encoding
// flattened ("-", "_", ".", spaces) is recovered exactly. Returns "" when nothing matches. Handles
// "-Users-me-Projects-x" (POSIX) and "C--Users-me-x" (Windows drive).
func DecodeProjectDir(enc string) string {
	var root, rest, sep string
	switch {
	case strings.HasPrefix(enc, "-"):
		root, rest, sep = "/", enc[1:], "/"
	case len(enc) > 3 && enc[1] == '-' && enc[2] == '-':
		root, rest, sep = enc[:1]+`:\`, enc[3:], `\`
	default:
		return ""
	}
	if rest == "" {
		return ""
	}
	return decodeWalk(root, rest, sep, 0)
}

func decodeWalk(dir, rest, sep string, depth int) string {
	if depth > 48 {
		return ""
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	join := func(name string) string {
		if strings.HasSuffix(dir, sep) {
			return dir + name
		}
		return dir + sep + name
	}
	for _, e := range entries {
		name := e.Name()
		encName := EncodeProjectDir(name)
		if encName == "" || (encName != rest && !strings.HasPrefix(rest, encName+"-")) {
			continue
		}
		// Follow symlinks (/var → /private/var on macOS): DirEntry.IsDir is false for them.
		if st, err := os.Stat(join(name)); err != nil || !st.IsDir() {
			continue
		}
		if rest == encName {
			return join(name)
		}
		if strings.HasPrefix(rest, encName+"-") {
			if r := decodeWalk(join(name), rest[len(encName)+1:], sep, depth+1); r != "" {
				return r
			}
		}
	}
	return ""
}
