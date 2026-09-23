package mesh

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/peter-dolkens/vineyard/daemon/internal/protocol"
	"github.com/peter-dolkens/vineyard/daemon/internal/service"
)

// A daemon brings its peers up to its own version. It is the one place that can do so safely: it is
// connected to every peer directly, so nothing is relayed through a process that is about to restart,
// and there is exactly one of it per machine, so two VS Code windows cannot race each other. It acts
// only on evidence a peer volunteers anyway (the version in its hello or snapshot), never polls, and
// tries a given peer and version once, with a long backoff after a failure.

const (
	distChunk      = 512 << 10
	distReqTimeout = 60 * time.Second
	distRetryAfter = 15 * time.Minute
)

type distTry struct {
	version  string
	at       time.Time
	inflight bool
}

type distributor struct {
	mu    sync.Mutex
	tried map[string]*distTry // by peer machine id
}

// binaryFor returns the path of this daemon's own version built for a platform, or "" if it has none.
func (n *Node) binaryFor(goos, goarch string) string {
	if goos == runtime.GOOS && goarch == runtime.GOARCH {
		if self, err := os.Executable(); err == nil {
			if real, err := filepath.EvalSymlinks(self); err == nil {
				return real
			}
			return self
		}
	}
	p := service.DistBinary(n.opts.Version, goos, goarch)
	if _, err := os.Stat(p); err == nil {
		return p
	}
	return ""
}

// considerPeer starts an upgrade of peerID if it reports an older version than ours and we hold a
// binary for its platform. goos/goarch may be empty, in which case the last snapshot we have decides.
func (n *Node) considerPeer(peerID, version, goos, goarch string) {
	if peerID == "" || peerID == n.cfg.MachineID || !isNewer(n.opts.Version, version) {
		return
	}
	if goos == "" || goarch == "" {
		n.mu.Lock()
		if e, ok := n.store[peerID]; ok {
			goos, goarch = e.Snapshot.Host.OS, e.Snapshot.Host.Arch
		}
		n.mu.Unlock()
		if goos == "" || goarch == "" {
			return
		}
	}
	bin := n.binaryFor(goos, goarch)
	if bin == "" {
		return
	}
	d := &n.dist
	d.mu.Lock()
	if d.tried == nil {
		d.tried = map[string]*distTry{}
	}
	t := d.tried[peerID]
	if t != nil && (t.inflight || (t.version == n.opts.Version && time.Since(t.at) < distRetryAfter)) {
		d.mu.Unlock()
		return
	}
	d.tried[peerID] = &distTry{version: n.opts.Version, at: time.Now(), inflight: true}
	d.mu.Unlock()
	go n.pushUpgrade(peerID, version, goos+"-"+goarch, bin)
}

// distributeAll re-examines every peer we currently have a snapshot for (after the store gained a new
// platform binary).
func (n *Node) distributeAll() {
	type cand struct{ id, version, goos, goarch string }
	var cands []cand
	n.mu.Lock()
	for id, e := range n.store {
		if id != n.cfg.MachineID && e.Online {
			cands = append(cands, cand{id, e.Snapshot.DaemonVersion, e.Snapshot.Host.OS, e.Snapshot.Host.Arch})
		}
	}
	n.mu.Unlock()
	for _, c := range cands {
		n.considerPeer(c.id, c.version, c.goos, c.goarch)
	}
}

func (n *Node) pushUpgrade(peerID, theirVersion, platform, bin string) {
	ok := false
	defer func() {
		n.dist.mu.Lock()
		if t := n.dist.tried[peerID]; t != nil {
			t.inflight = false
			if ok {
				t.at = time.Now()
			}
		}
		n.dist.mu.Unlock()
	}()

	n.mu.Lock()
	p := n.peers[peerID]
	var l *link
	if p != nil {
		l = p.link
	}
	n.mu.Unlock()
	if l == nil {
		return
	}
	data, err := os.ReadFile(bin)
	if err != nil {
		n.logf("dist: cannot read %s: %v", bin, err)
		return
	}
	if len(data) < minBinarySize {
		n.logf("dist: %s is only %d bytes; not pushing it", bin, len(data))
		return
	}
	sum := sha256.Sum256(data)
	sha := hex.EncodeToString(sum[:])
	n.logf("dist: %s runs %s; pushing vineyardd %s for %s (%d bytes)", peerID, theirVersion, n.opts.Version, platform, len(data))

	args := protocol.UpgradeArgs{Version: n.opts.Version, SHA256: sha, Size: int64(len(data))}
	for off := 0; off < len(data); {
		end := min(off+distChunk, len(data))
		a := args
		a.Offset = int64(off)
		a.Data = base64.StdEncoding.EncodeToString(data[off:end])
		a.Done = end == len(data)
		raw, err := n.request(l, "upgrade", a, distReqTimeout)
		if err != nil {
			msg := err.Error()
			switch {
			case strings.Contains(msg, "already in progress"):
				n.logf("dist: %s is already receiving this build from someone else; leaving it to them", peerID)
				ok = true
			case strings.Contains(msg, "already running"):
				n.logf("dist: %s already runs %s", peerID, n.opts.Version)
				ok = true
			default:
				n.logf("dist: upgrade of %s failed at %d bytes: %v", peerID, off, err)
			}
			return
		}
		if a.Done {
			var res protocol.UpgradeResult
			_ = json.Unmarshal(raw, &res)
			if !res.Installed {
				n.logf("dist: %s took the whole binary but did not confirm the install", peerID)
				return
			}
			n.logf("dist: %s verified vineyardd %s and is restarting", peerID, res.Version)
			ok = true
		}
		off = end
	}
}

// request sends one operation to the daemon at the far end of l and waits for its answer.
func (n *Node) request(l *link, op string, args any, timeout time.Duration) (json.RawMessage, error) {
	raw, err := json.Marshal(args)
	if err != nil {
		return nil, err
	}
	id := newID()
	ch := make(chan protocol.Response, 1)
	n.mu.Lock()
	n.pending[id] = pendingReq{origin: l, origID: id, expires: time.Now().Add(timeout), ch: ch}
	n.mu.Unlock()
	if err := l.conn.Send(protocol.Request{T: "req", ID: id, Op: op, Args: raw}); err != nil {
		n.mu.Lock()
		delete(n.pending, id)
		n.mu.Unlock()
		return nil, err
	}
	select {
	case res := <-ch:
		if !res.OK {
			return nil, errors.New(res.Error)
		}
		return res.Data, nil
	case <-time.After(timeout + 5*time.Second):
		// The maintenance loop should have expired this already; belt and braces.
		n.mu.Lock()
		delete(n.pending, id)
		n.mu.Unlock()
		return nil, fmt.Errorf("%s: no answer from %s", op, l.peerID)
	}
}
