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
	"strings"
	"sync"
	"time"

	"github.com/peter-dolkens/vineyard/daemon/internal/protocol"
	"github.com/peter-dolkens/vineyard/daemon/internal/service"
)

// Upgrades arrive from the VS Code extension, which carries a vineyardd build for every platform.
// The extension notices a daemon older than its bundled build and streams the matching binary to it
// over the mesh; the receiving daemon stages it, verifies the digest, checks that it actually runs,
// then lets the new binary install itself. There is no polling and no traffic unless a viewer is
// attached and sees an outdated machine.

const (
	minBinarySize  = 1 << 20  // a vineyardd build is several MB; anything smaller is a broken upload
	maxBinarySize  = 64 << 20 // sanity bound on Size
	stagingIdle    = 5 * time.Minute
	installerDelay = 500 * time.Millisecond // let the response reach the viewer before we go down
)

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
	cur *staging
	// installing is set once a verified binary has been handed off; further uploads are refused.
	installing bool
}

func (u *upgrader) abort() {
	if u.cur != nil {
		_ = u.cur.f.Close()
		_ = os.Remove(u.cur.path)
		u.cur = nil
	}
}

func (n *Node) handleUpgrade(a protocol.UpgradeArgs) (json.RawMessage, error) {
	sha := strings.ToLower(a.SHA256)
	if len(sha) != 64 {
		return nil, errors.New("sha256 must be a 64-character hex digest")
	}
	if _, err := hex.DecodeString(sha); err != nil {
		return nil, errors.New("sha256 must be a 64-character hex digest")
	}
	if a.Size <= 0 || a.Size > maxBinarySize {
		return nil, fmt.Errorf("size %d out of range", a.Size)
	}
	if !a.Force && a.Version != "" && a.Version == n.opts.Version {
		return nil, fmt.Errorf("already running %s", a.Version)
	}

	u := &n.upgrade
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.installing {
		return nil, errors.New("an upgrade is already installing; the daemon is about to restart")
	}
	if u.cur != nil && (u.cur.sha != sha || time.Since(u.cur.touched) > stagingIdle) {
		u.abort()
	}
	if u.cur == nil {
		if a.Offset != 0 {
			return nil, errors.New("no upload in progress for this digest; start again at offset 0")
		}
		path := service.StagedBinary(sha[:12])
		if err := os.MkdirAll(service.BinDir(), 0o755); err != nil {
			return nil, err
		}
		f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
		if err != nil {
			return nil, err
		}
		u.cur = &staging{f: f, path: path, sha: sha, size: a.Size, hash: sha256.New()}
		n.logf("upgrade: receiving vineyardd %s (%d bytes) into %s", a.Version, a.Size, path)
	}
	st := u.cur
	if a.Offset != st.n {
		return nil, fmt.Errorf("offset mismatch: have %d bytes, got chunk at %d", st.n, a.Offset)
	}
	if a.Data != "" {
		chunk, err := base64.StdEncoding.DecodeString(a.Data)
		if err != nil {
			return nil, fmt.Errorf("chunk is not valid base64: %w", err)
		}
		if st.n+int64(len(chunk)) > st.size {
			u.abort()
			return nil, errors.New("upload exceeds the declared size")
		}
		if _, err := st.f.Write(chunk); err != nil {
			u.abort()
			return nil, err
		}
		_, _ = st.hash.Write(chunk)
		st.n += int64(len(chunk))
	}
	st.touched = time.Now()
	res := protocol.UpgradeResult{Received: st.n}
	if !a.Done {
		return json.Marshal(res)
	}

	// Final chunk: verify, prove the binary runs on this machine, then hand over.
	if st.n != st.size {
		u.abort()
		return nil, fmt.Errorf("upload incomplete: %d of %d bytes", st.n, st.size)
	}
	if got := hex.EncodeToString(st.hash.Sum(nil)); got != st.sha {
		u.abort()
		return nil, errors.New("sha256 mismatch after upload")
	}
	if st.n < minBinarySize {
		u.abort()
		return nil, fmt.Errorf("staged binary is only %d bytes; refusing to install", st.n)
	}
	if err := st.f.Close(); err != nil {
		u.abort()
		return nil, err
	}
	_ = os.Chmod(st.path, 0o755)
	version, err := probeVersion(st.path)
	if err != nil {
		_ = os.Remove(st.path)
		u.cur = nil
		return nil, fmt.Errorf("staged binary does not run here (%v); wrong platform?", err)
	}
	res.Version = version
	res.Installed = true
	u.installing = true
	path := st.path
	u.cur = nil
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
