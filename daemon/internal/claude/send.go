package claude

import (
	"bufio"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Cross-session messaging: every Claude Code session binds an inbox socket (Unix socket, or a named
// pipe on Windows) recorded in ~/.claude/sessions/<pid>.json. A client authenticates with the
// session's own token, read from ~/.claude/sessions/<pid>.<sha256(socketPath)>.key, then writes one
// JSON line per message. This is the same path Claude Code's hooks use to post back into their own
// session, so a message delivered this way is treated as an own-child post and not held for approval.
// Format captured from Claude Code 2.1.273 (see docs: code.claude.com/docs/en/cross-session-messaging).

type inboxTarget struct {
	PID        int
	SocketPath string
	Token      string
	Name       string
}

func newMsgID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b[:])
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

// findInbox locates the live session with this id and its inbox credentials.
func findInbox(claudeDir, sessionID string) (*inboxTarget, error) {
	dir := filepath.Join(claudeDir, "sessions")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		var reg struct {
			PID                 int    `json:"pid"`
			SessionID           string `json:"sessionId"`
			Name                string `json:"name"`
			MessagingSocketPath string `json:"messagingSocketPath"`
		}
		if json.Unmarshal(b, &reg) != nil || reg.SessionID != sessionID {
			continue
		}
		if !processAlive(reg.PID) {
			return nil, fmt.Errorf("session %s (pid %d) is no longer running", sessionID, reg.PID)
		}
		if reg.MessagingSocketPath == "" {
			return nil, errors.New("session has no inbox socket (messaging unavailable or Claude Code too old)")
		}
		sock := strings.TrimPrefix(reg.MessagingSocketPath, "uds:")
		sum := sha256.Sum256([]byte(sock))
		keyFile := filepath.Join(dir, strconv.Itoa(reg.PID)+"."+hex.EncodeToString(sum[:])+".key")
		kb, err := os.ReadFile(keyFile)
		if err != nil {
			return nil, fmt.Errorf("read inbox key: %w", err)
		}
		var key struct {
			PeerToken string `json:"peerToken"`
		}
		if json.Unmarshal(kb, &key) != nil || key.PeerToken == "" {
			return nil, errors.New("inbox key file is malformed")
		}
		return &inboxTarget{PID: reg.PID, SocketPath: sock, Token: key.PeerToken, Name: reg.Name}, nil
	}
	return nil, fmt.Errorf("no live session with id %s on this machine", sessionID)
}

// SendToSession posts a user message into a running Claude Code session on this machine.
// The text arrives as if typed at that session's prompt (read between tool calls if busy, or starting
// a new turn if idle). It cannot answer permission prompts or AskUserQuestion; only the session's own
// UI can do that.
func SendToSession(claudeDir, sessionID, text string) (msgID string, err error) {
	if strings.TrimSpace(text) == "" {
		return "", errors.New("message is empty")
	}
	t, err := findInbox(claudeDir, sessionID)
	if err != nil {
		return "", err
	}
	conn, err := dialInbox(t.SocketPath, 5*time.Second)
	if err != nil {
		return "", fmt.Errorf("connect to session inbox: %w", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))

	msgID = newMsgID()
	frames := []any{
		map[string]any{"type": "auth", "token": t.Token},
		map[string]any{
			"msgV":     1,
			"msg_id":   msgID,
			"type":     "user",
			"message":  map[string]any{"role": "user", "content": text},
			"priority": "next",
		},
	}
	w := bufio.NewWriter(conn)
	for _, f := range frames {
		b, err := json.Marshal(f)
		if err != nil {
			return "", err
		}
		if _, err := w.Write(append(b, '\n')); err != nil {
			return "", err
		}
	}
	if err := w.Flush(); err != nil {
		return "", err
	}
	return msgID, nil
}

func dialInbox(path string, timeout time.Duration) (net.Conn, error) {
	if strings.HasPrefix(path, `\\`) {
		return dialPipe(path, timeout)
	}
	return net.DialTimeout("unix", path, timeout)
}
