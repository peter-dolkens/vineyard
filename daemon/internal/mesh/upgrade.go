package mesh

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/peter-dolkens/vineyard/daemon/internal/protocol"
	"github.com/peter-dolkens/vineyard/daemon/internal/service"
)

// Upgrades arrive as chunked binaries over the mesh. The VS Code extension, which carries a vineyardd
// build for every platform, only ever upgrades the daemon next to it ("upgrade" over loopback): that
// daemon stages the binary, verifies the digest, checks that it runs, and lets the new binary install
// itself. Once running, the new daemon brings the rest of the fleet up to its version itself (see
// distribute.go): peers of its own platform get its own executable; other platforms come from a
// distribution store the extension fills with "stage". There is no polling and no traffic unless a
// peer actually reports an older version.

const (
	minBinarySize  = 1 << 20                // a vineyardd build is several MB; anything smaller is a broken upload
	maxBinarySize  = 64 << 20               // sanity bound on Size
	staleAfter     = 60 * time.Second       // an upload with no chunk for this long has lost its sender
	maxStagings    = 4                      // concurrent uploads (different digests) a daemon will hold
	installerDelay = 500 * time.Millisecond // let the response reach the viewer before we go down
)

var platformRe = regexp.MustCompile(`^([a-z0-9]+)-([a-z0-9]+)$`)

type staging struct {
	f       *os.File
	path    string
	sha     string
	size    int64
	n       int64
	hash    hash.Hash
	touched time.Time
}

type upgrader struct {
	mu  sync.Mutex
	cur map[string]*staging // by digest
	// installing is set once a verified binary has been handed off; further uploads are refused.
	installing bool
}

func (u *upgrader) drop(st *staging) {
	_ = st.f.Close()
	_ = os.Remove(st.path)
	delete(u.cur, st.sha)
}

func (u *upgrader) dropAll() {
	for _, st := range u.cur {
		u.drop(st)
	}
}

// receive appends one chunk to the upload for a.SHA256, opening it on the first chunk. It returns the
// staging entry and, on the final chunk, true: the file is then complete, closed and verified, and it
// is the caller's job to consume it and remove it from u.cur. Callers hold u.mu.
func (u *upgrader) receive(a protocol.UpgradeArgs, sha string, opened func(*staging)) (*staging, bool, error) {
	if u.cur == nil {
		u.cur = map[string]*staging{}
	}
	st := u.cur[sha]
	if st != nil && time.Since(st.touched) > staleAfter {
		u.drop(st) // whoever was sending this has gone away; let the newcomer start over
		st = nil
	}
	if st == nil {
		if a.Offset != 0 {
			return nil, false, errors.New("no upload in progress for this digest; start again at offset 0")
		}
		if len(u.cur) >= maxStagings {
			return nil, false, fmt.Errorf("%d uploads already in progress; try again later", len(u.cur))
		}
		path := service.StagedBinary(sha[:12])
		if err := os.MkdirAll(service.BinDir(), 0o755); err != nil {
			return nil, false, err
		}
		f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
		if err != nil {
			return nil, false, err
		}
		st = &staging{f: f, path: path, sha: sha, size: a.Size, hash: sha256.New(), touched: time.Now()}
		u.cur[sha] = st
		if opened != nil {
			opened(st)
		}
	} else if a.Offset == 0 && st.n > 0 {
		// Another sender (a second VS Code window, or a peer distributing the same build) is already
		// streaming this exact binary. Tell the newcomer so it can wait rather than fail.
		return nil, false, fmt.Errorf("upload of this build is already in progress (%d of %d bytes); wait for it", st.n, st.size)
	}
	if a.Offset != st.n {
		return nil, false, fmt.Errorf("offset mismatch: have %d bytes, got chunk at %d", st.n, a.Offset)
	}
	if a.Data != "" {
		chunk, err := base64.StdEncoding.DecodeString(a.Data)
		if err != nil {
			return nil, false, fmt.Errorf("chunk is not valid base64: %w", err)
		}
		if st.n+int64(len(chunk)) > st.size {
			u.drop(st)
			return nil, false, errors.New("upload exceeds the declared size")
		}
		if _, err := st.f.Write(chunk); err != nil {
			u.drop(st)
			return nil, false, err
		}
		_, _ = st.hash.Write(chunk)
		st.n += int64(len(chunk))
	}
	st.touched = time.Now()
	if !a.Done {
		return st, false, nil
	}
	if st.n != st.size {
		u.drop(st)
		return nil, false, fmt.Errorf("upload incomplete: %d of %d bytes", st.n, st.size)
	}
	if got := hex.EncodeToString(st.hash.Sum(nil)); got != st.sha {
		u.drop(st)
		return nil, false, errors.New("sha256 mismatch after upload")
	}
	if st.n < minBinarySize {
		u.drop(st)
		return nil, false, fmt.Errorf("staged binary is only %d bytes; refusing to install", st.n)
	}
	if err := st.f.Close(); err != nil {
		u.drop(st)
		return nil, false, err
	}
	_ = os.Chmod(st.path, 0o755)
	return st, true, nil
}

func checkUpgradeArgs(a protocol.UpgradeArgs) (string, error) {
	sha := strings.ToLower(a.SHA256)
	if len(sha) != 64 {
		return "", errors.New("sha256 must be a 64-character hex digest")
	}
	if _, err := hex.DecodeString(sha); err != nil {
		return "", errors.New("sha256 must be a 64-character hex digest")
	}
	if a.Size <= 0 || a.Size > maxBinarySize {
		return "", fmt.Errorf("size %d out of range", a.Size)
	}
	return sha, nil
}

// handleUpgrade installs the streamed binary on this machine.
func (n *Node) handleUpgrade(a protocol.UpgradeArgs) (json.RawMessage, error) {
	sha, err := checkUpgradeArgs(a)
	if err != nil {
		return nil, err
	}
	if !a.Force && a.Version != "" && a.Version == n.opts.Version {
		return nil, fmt.Errorf("already running %s", a.Version)
	}
	if !a.Force && isNewer(n.opts.Version, a.Version) {
		return nil, fmt.Errorf("refusing to downgrade from %s to %s", n.opts.Version, a.Version)
	}

	u := &n.upgrade
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.installing {
		return nil, errors.New("an upgrade is already installing; the daemon is about to restart")
	}
	st, done, err := u.receive(a, sha, func(st *staging) {
		n.logf("upgrade: receiving vineyardd %s (%d bytes) into %s", a.Version, a.Size, st.path)
	})
	if err != nil {
		return nil, err
	}
	res := protocol.UpgradeResult{Received: st.n}
	if !done {
		return json.Marshal(res)
	}

	// Final chunk: prove the binary runs on this machine, then hand over.
	delete(u.cur, sha)
	version, err := probeVersion(st.path)
	if err != nil {
		_ = os.Remove(st.path)
		return nil, fmt.Errorf("staged binary does not run here (%v); wrong platform?", err)
	}
	res.Version = version
	res.Installed = true
	u.installing = true
	u.dropAll()
	path := st.path
	n.logf("upgrade: %s verified (reports %s); installing and restarting", path, version)
	time.AfterFunc(installerDelay, func() {
		if err := service.LaunchInstaller(path); err != nil {
			n.logf("upgrade: %v", err)
			u.mu.Lock()
			u.installing = false
			u.mu.Unlock()
		}
	})
	return json.Marshal(res)
}

// handleStage stores a binary of this daemon's own version for another platform, so the daemon can
// upgrade peers of that platform itself.
func (n *Node) handleStage(a protocol.UpgradeArgs) (json.RawMessage, error) {
	sha, err := checkUpgradeArgs(a)
	if err != nil {
		return nil, err
	}
	m := platformRe.FindStringSubmatch(a.Platform)
	if m == nil {
		return nil, errors.New(`platform must be "<os>-<arch>"`)
	}
	goos, goarch := m[1], m[2]
	if a.Version != n.opts.Version {
		return nil, fmt.Errorf("this daemon runs %s and only distributes that version; %s cannot be staged here", n.opts.Version, a.Version)
	}

	u := &n.upgrade
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.installing {
		return nil, errors.New("an upgrade is installing; the daemon is about to restart")
	}
	st, done, err := u.receive(a, sha, func(st *staging) {
		n.logf("dist: receiving vineyardd %s for %s (%d bytes)", a.Version, a.Platform, a.Size)
	})
	if err != nil {
		return nil, err
	}
	res := protocol.UpgradeResult{Received: st.n}
	if !done {
		return json.Marshal(res)
	}
	delete(u.cur, sha)
	dest := service.DistBinary(a.Version, goos, goarch)
	if err := os.MkdirAll(filepath.Join(service.DistDir(), a.Version), 0o755); err != nil {
		_ = os.Remove(st.path)
		return nil, err
	}
	_ = os.Remove(dest) // Windows will not rename over an existing file
	if err := os.Rename(st.path, dest); err != nil {
		_ = os.Remove(st.path)
		return nil, err
	}
	res.Stored = true
	res.Version = a.Version
	n.logf("dist: stored vineyardd %s for %s", a.Version, a.Platform)
	go n.distributeAll()
	return json.Marshal(res)
}

// handleDist reports which platforms this daemon can upgrade and which its peers need.
func (n *Node) handleDist() (json.RawMessage, error) {
	have := map[string]bool{}
	self := runtime.GOOS + "-" + runtime.GOARCH
	if _, err := os.Executable(); err == nil {
		have[self] = true
	}
	if entries, err := os.ReadDir(filepath.Join(service.DistDir(), n.opts.Version)); err == nil {
		for _, e := range entries {
			name := strings.TrimSuffix(strings.TrimPrefix(e.Name(), "vineyardd-"), ".exe")
			if platformRe.MatchString(name) {
				have[name] = true
			}
		}
	}
	want := map[string]bool{}
	n.mu.Lock()
	for id, e := range n.store {
		if id == n.cfg.MachineID || e.Snapshot.Host.OS == "" || e.Snapshot.Host.Arch == "" {
			continue
		}
		p := e.Snapshot.Host.OS + "-" + e.Snapshot.Host.Arch
		if !have[p] {
			want[p] = true
		}
	}
	n.mu.Unlock()
	res := protocol.DistResult{Version: n.opts.Version, Have: sortedKeys(have), Want: sortedKeys(want)}
	return json.Marshal(res)
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// probeVersion runs `<bin> version` and returns its trimmed output.
func probeVersion(bin string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "version").Output()
	if err != nil {
		return "", err
	}
	v := strings.TrimSpace(string(out))
	if v == "" {
		return "", errors.New("empty version output")
	}
	return v, nil
}
