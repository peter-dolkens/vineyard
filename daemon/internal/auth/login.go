// Package auth drives `claude auth login` on this machine on behalf of a viewer elsewhere. Without a
// TTY the CLI prints a sign-in URL whose redirect lands on a page that shows a code to paste, then
// waits for that code on stdin. Vineyard relays the URL to the viewer's browser and the code back,
// so a machine you cannot sit at (a headless box, a Windows desktop without SSH) can be signed in.
package auth

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/peter-dolkens/vineyard/daemon/internal/managed"
)

const (
	urlTimeout  = 45 * time.Second
	codeTimeout = 120 * time.Second
	idleTimeout = 15 * time.Minute // an unfinished login is abandoned after this
)

var (
	ansiRe = regexp.MustCompile(`\x1b\[[0-9;?]*[A-Za-z]`)
	urlRe  = regexp.MustCompile(`https://\S+/oauth/authorize\S+`)
)

type session struct {
	id      string
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	mu      sync.Mutex
	out     bytes.Buffer
	done    chan struct{}
	err     error
	started time.Time
}

func (s *session) output() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return ansiRe.ReplaceAllString(s.out.String(), "")
}

type Manager struct {
	mu        sync.Mutex
	cur       *session
	log       *log.Logger
	ClaudeBin string
}

func New(logger *log.Logger) *Manager {
	if logger == nil {
		logger = log.Default()
	}
	return &Manager{log: logger}
}

func newID() string { return fmt.Sprintf("%d", time.Now().UnixNano()) }

// Start launches the login and returns the sign-in URL once the CLI prints it. Any earlier
// unfinished login is cancelled.
func (m *Manager) Start(console bool) (id, url string, err error) {
	bin, err := managed.FindClaude(m.ClaudeBin)
	if err != nil {
		return "", "", err
	}
	m.mu.Lock()
	if m.cur != nil {
		m.kill(m.cur)
		m.cur = nil
	}
	m.mu.Unlock()

	args := []string{"auth", "login"}
	if console {
		args = append(args, "--console")
	}
	cmd := exec.Command(bin, args...)
	// Do not pop a browser on the machine running the login; the viewer opens the URL on theirs.
	noBrowser := "/usr/bin/true"
	if runtime.GOOS == "windows" {
		noBrowser = "rundll32.exe" // exists everywhere and ignores a URL argument
	}
	cmd.Env = append(os.Environ(), "BROWSER="+noBrowser, "CLAUDE_CODE_ENTRYPOINT=vineyard")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return "", "", err
	}
	s := &session{id: newID(), cmd: cmd, stdin: stdin, done: make(chan struct{}), started: time.Now()}
	cmd.Stdout = &lockedWriter{s: s}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		return "", "", fmt.Errorf("start claude auth login: %w", err)
	}
	go func() {
		s.err = cmd.Wait()
		close(s.done)
	}()
	m.mu.Lock()
	m.cur = s
	m.mu.Unlock()
	m.log.Printf("auth: login started (pid %d, console=%v)", cmd.Process.Pid, console)

	deadline := time.Now().Add(urlTimeout)
	for time.Now().Before(deadline) {
		if u := urlRe.FindString(s.output()); u != "" {
			time.AfterFunc(idleTimeout, func() { m.Cancel(s.id) })
			return s.id, strings.TrimRight(u, ".,)"), nil
		}
		select {
		case <-s.done:
			return "", "", fmt.Errorf("claude auth login exited before printing a sign-in URL: %s", tail(s.output(), 400))
		case <-time.After(150 * time.Millisecond):
		}
	}
	m.Cancel(s.id)
	return "", "", errors.New("claude auth login did not print a sign-in URL in time")
}

// Code hands the pasted authorization code to the waiting CLI and reports how the login ended.
func (m *Manager) Code(id, code string) (string, error) {
	m.mu.Lock()
	s := m.cur
	m.mu.Unlock()
	if s == nil || s.id != id {
		return "", errors.New("no sign-in is waiting for a code on this machine; start again")
	}
	code = strings.TrimSpace(code)
	if code == "" {
		return "", errors.New("empty code")
	}
	before := len(s.output())
	if _, err := io.WriteString(s.stdin, code+"\n"); err != nil {
		return "", fmt.Errorf("send code: %w", err)
	}
	select {
	case <-s.done:
	case <-time.After(codeTimeout):
		m.Cancel(id)
		return "", errors.New("the CLI did not finish signing in within two minutes")
	}
	m.mu.Lock()
	if m.cur == s {
		m.cur = nil
	}
	m.mu.Unlock()
	out := s.output()
	after := strings.TrimSpace(out[min(before, len(out)):])
	if s.err != nil {
		return "", fmt.Errorf("sign-in failed: %s", tail(after, 300))
	}
	m.log.Printf("auth: login finished successfully")
	msg := tail(after, 200)
	if msg == "" {
		msg = "signed in"
	}
	return msg, nil
}

// Cancel abandons a pending login.
func (m *Manager) Cancel(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cur != nil && (id == "" || m.cur.id == id) {
		m.kill(m.cur)
		m.cur = nil
	}
}

func (m *Manager) kill(s *session) {
	select {
	case <-s.done:
		return
	default:
	}
	_ = s.stdin.Close()
	_ = s.cmd.Process.Kill()
	m.log.Printf("auth: login cancelled")
}

type lockedWriter struct{ s *session }

func (w *lockedWriter) Write(p []byte) (int, error) {
	w.s.mu.Lock()
	defer w.s.mu.Unlock()
	if w.s.out.Len() < 256<<10 {
		w.s.out.Write(p)
	}
	return len(p), nil
}

func tail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		s = "…" + s[len(s)-n:]
	}
	return s
}
