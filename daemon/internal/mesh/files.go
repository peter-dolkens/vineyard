package mesh

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/peter-dolkens/vineyard/daemon/internal/claude"
	"github.com/peter-dolkens/vineyard/daemon/internal/model"
	"github.com/peter-dolkens/vineyard/daemon/internal/protocol"
	"github.com/peter-dolkens/vineyard/daemon/internal/retention"
)

const (
	// MaxPutFile bounds one file sent to a session.
	MaxPutFile = 256 << 20
	// maxPutChunk bounds one chunk, decoded.
	maxPutChunk = 1 << 20
	// putIdle drops a transfer nobody has added to for this long.
	putIdle = 10 * time.Minute
	// maxPuts bounds transfers in progress at once.
	maxPuts = 8
)

// ReceivedDir is where files sent to this machine's sessions land: one folder per session. Package
// retention clears it out.
func ReceivedDir() string { return retention.ReceivedDir() }

type putFile struct {
	args    protocol.PutFileArgs
	f       *os.File
	path    string
	n       int64
	hash    hash.Hash
	touched time.Time
}

type putFiles struct {
	mu  sync.Mutex
	cur map[string]*putFile
}

func (p *putFiles) drop(t *putFile, id string) {
	_ = t.f.Close()
	_ = os.Remove(t.path)
	delete(p.cur, id)
}

var transferIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]{8,64}$`)

// safeFileName is name as a plain file name: no directories, no control characters, nothing that
// means "here" or "up".
func safeFileName(name string) (string, error) {
	name = strings.TrimSpace(filepath.Base(strings.ReplaceAll(name, `\`, "/")))
	if name == "/" || name == "." || name == ".." {
		return "", errors.New("not a usable file name")
	}
	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || strings.ContainsRune(`<>:"/\|?*`, r) {
			return '_'
		}
		return r
	}, name)
	if name == "" || name == "." || name == ".." || len(name) > 200 {
		return "", errors.New("not a usable file name")
	}
	return name, nil
}

// freePath is dir/name, or "name (2).ext" and so on when that is taken.
func freePath(dir, name string) string {
	p := filepath.Join(dir, name)
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	for i := 2; ; i++ {
		if _, err := os.Lstat(p); errors.Is(err, os.ErrNotExist) {
			return p
		}
		p = filepath.Join(dir, fmt.Sprintf("%s (%d)%s", stem, i, ext))
	}
}

// putFile takes one chunk of a file sent to a session on this machine.
func (n *Node) putFile(a protocol.PutFileArgs) (json.RawMessage, error) {
	if !transferIDRe.MatchString(a.ID) {
		return nil, errors.New("bad transfer id")
	}
	if a.SessionID == "" || !transferIDRe.MatchString(strings.ReplaceAll(a.SessionID, "-", "")) {
		return nil, errors.New("a session id is required")
	}
	if a.Size < 0 || a.Size > MaxPutFile {
		return nil, fmt.Errorf("files up to %d MB can be sent", MaxPutFile>>20)
	}
	if len(a.SHA256) != 64 {
		return nil, errors.New("sha256 is required")
	}
	name, err := safeFileName(a.Name)
	if err != nil {
		return nil, err
	}
	if a.Origin != nil {
		if err := n.checkOrigin(model.AgentToolFiles, "send files to its sessions", "sendFiles", a.Origin); err != nil {
			return nil, err
		}
	}
	data, err := base64.StdEncoding.DecodeString(a.Data)
	if err != nil {
		return nil, errors.New("data is not base64")
	}
	if len(data) > maxPutChunk {
		return nil, fmt.Errorf("chunks are at most %d KB", maxPutChunk>>10)
	}

	p := &n.puts
	p.mu.Lock()
	if p.cur == nil {
		p.cur = map[string]*putFile{}
	}
	for id, t := range p.cur {
		if time.Since(t.touched) > putIdle {
			p.drop(t, id)
		}
	}
	t := p.cur[a.ID]
	if t == nil {
		if a.Offset != 0 {
			p.mu.Unlock()
			return nil, errors.New("no transfer in progress with this id; start again at offset 0")
		}
		if len(p.cur) >= maxPuts {
			p.mu.Unlock()
			return nil, fmt.Errorf("%d transfers already in progress; try again later", len(p.cur))
		}
		partial := filepath.Join(ReceivedDir(), ".partial")
		if err := os.MkdirAll(partial, 0o700); err != nil {
			p.mu.Unlock()
			return nil, err
		}
		path := filepath.Join(partial, a.ID)
		f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
		if err != nil {
			p.mu.Unlock()
			return nil, err
		}
		t = &putFile{args: a, f: f, path: path, hash: sha256.New()}
		p.cur[a.ID] = t
	} else if t.args.SHA256 != a.SHA256 || t.args.Size != a.Size || t.args.SessionID != a.SessionID {
		p.mu.Unlock()
		return nil, errors.New("a chunk does not belong to this transfer")
	}
	if a.Offset != t.n {
		p.mu.Unlock()
		return nil, fmt.Errorf("expected offset %d, got %d", t.n, a.Offset)
	}
	if t.n+int64(len(data)) > t.args.Size {
		p.drop(t, a.ID)
		p.mu.Unlock()
		return nil, errors.New("more data than the stated size")
	}
	if _, err := t.f.Write(data); err != nil {
		p.drop(t, a.ID)
		p.mu.Unlock()
		return nil, err
	}
	t.hash.Write(data)
	t.n += int64(len(data))
	t.touched = time.Now()
	res := protocol.PutFileResult{Received: t.n}
	if !a.Done {
		p.mu.Unlock()
		return json.Marshal(res)
	}
	delete(p.cur, a.ID)
	p.mu.Unlock()

	if err := t.f.Close(); err != nil {
		_ = os.Remove(t.path)
		return nil, err
	}
	if t.n != t.args.Size || hex.EncodeToString(t.hash.Sum(nil)) != strings.ToLower(t.args.SHA256) {
		_ = os.Remove(t.path)
		return nil, errors.New("the file arrived damaged (size or checksum differs); send it again")
	}
	dir := filepath.Join(ReceivedDir(), t.args.SessionID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		_ = os.Remove(t.path)
		return nil, err
	}
	final := freePath(dir, name)
	if err := os.Rename(t.path, final); err != nil {
		_ = os.Remove(t.path)
		return nil, err
	}
	res.Path = final
	n.logf("received %s (%d bytes) for session %s", final, t.n, t.args.SessionID)
	go n.sweeper.Run(false)
	if err := n.deliverText(t.args.SessionID, fileNotice(a, final, t.n), a.Origin); err != nil {
		res.DeliverError = err.Error()
	} else {
		res.Delivered = true
	}
	return json.Marshal(res)
}

// fileNotice tells a session a file has arrived for it.
func fileNotice(a protocol.PutFileArgs, path string, size int64) string {
	var b strings.Builder
	fmt.Fprintf(&b, "A file was sent to this session through Vineyard and saved at:\n%s\n(%d bytes, checksum verified). It is outside your workspace; copy it where you need it.", path, size)
	if note := strings.TrimSpace(a.Note); note != "" {
		b.WriteString("\n\n")
		b.WriteString(note)
	}
	return b.String()
}

// deliverText puts text into a session on this machine as a prompt: on stdin for one this daemon
// runs, through the cross-session inbox otherwise. With an origin it is marked as an agent's.
func (n *Node) deliverText(sessionID, text string, o *protocol.AgentOrigin) error {
	if o != nil {
		text = WrapAgentMessage(o, text)
	}
	if n.opts.Managed != nil && n.opts.Managed.Has(sessionID) {
		if err := n.opts.Managed.Send(sessionID, text); err != nil {
			return err
		}
	} else if _, err := claude.SendToSession(n.opts.ClaudeDir, sessionID, text); err != nil {
		return err
	}
	if o != nil {
		n.noteAgentHops(sessionID, o.Hops)
	}
	n.kickCollector()
	return nil
}
