//go:build !windows

package mesh

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/peter-dolkens/vineyard/daemon/internal/protocol"
	"github.com/peter-dolkens/vineyard/daemon/internal/service"
)

// fakeBinary is an executable shell script large enough to pass the size floor; `version` prints a
// recognisable string and `install` is a no-op, so the hand-off can run harmlessly inside the test.
func fakeBinary() []byte {
	var b strings.Builder
	b.WriteString("#!/bin/sh\n[ \"$1\" = version ] && echo 9.9.9-fake\nexit 0\n")
	pad := "# " + strings.Repeat("x", 78) + "\n"
	for b.Len() < minBinarySize+1024 {
		b.WriteString(pad)
	}
	return []byte(b.String())
}

func sendChunk(t *testing.T, n *Node, a protocol.UpgradeArgs) (protocol.UpgradeResult, error) {
	t.Helper()
	raw, err := n.handleUpgrade(a)
	if err != nil {
		return protocol.UpgradeResult{}, err
	}
	var res protocol.UpgradeResult
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatal(err)
	}
	return res, nil
}

func TestUpgradeStagesVerifiesAndHandsOver(t *testing.T) {
	n := newTestNode(t, "zebra")
	bin := fakeBinary()
	sum := sha256.Sum256(bin)
	sha := hex.EncodeToString(sum[:])
	base := protocol.UpgradeArgs{Version: "9.9.9-fake", SHA256: sha, Size: int64(len(bin))}

	chunk := 300 << 10
	var off int
	for off < len(bin) {
		end := min(off+chunk, len(bin))
		a := base
		a.Offset = int64(off)
		a.Data = base64.StdEncoding.EncodeToString(bin[off:end])
		a.Done = end == len(bin)
		res, err := sendChunk(t, n, a)
		if err != nil {
			t.Fatalf("chunk at %d: %v", off, err)
		}
		if res.Received != int64(end) {
			t.Fatalf("received %d, want %d", res.Received, end)
		}
		if a.Done {
			if !res.Installed {
				t.Fatal("final chunk did not report installed")
			}
			if res.Version != "9.9.9-fake" {
				t.Fatalf("probe version %q", res.Version)
			}
		} else if res.Installed {
			t.Fatal("installed reported before the last chunk")
		}
		off = end
	}
	if _, err := os.Stat(service.StagedBinary(sha[:12])); err != nil {
		t.Fatalf("staged binary missing: %v", err)
	}
	// While the hand-off is pending, further uploads are refused.
	if _, err := sendChunk(t, n, protocol.UpgradeArgs{Version: "x", SHA256: sha, Size: 1, Offset: 0}); err == nil {
		t.Fatal("expected refusal while installing")
	}
	// Let the (no-op) installer run so the temp dir can be cleaned up.
	time.Sleep(installerDelay + 300*time.Millisecond)
}

func TestUpgradeRejectsBadInput(t *testing.T) {
	n := newTestNode(t, "zebra")
	bin := fakeBinary()
	sum := sha256.Sum256(bin)
	sha := hex.EncodeToString(sum[:])
	whole := base64.StdEncoding.EncodeToString(bin)

	if _, err := sendChunk(t, n, protocol.UpgradeArgs{Version: "test", SHA256: sha, Size: int64(len(bin))}); err == nil || !strings.Contains(err.Error(), "already running") {
		t.Fatalf("same version should be refused without force, got %v", err)
	}
	if _, err := sendChunk(t, n, protocol.UpgradeArgs{Version: "v", SHA256: "nothex", Size: 1}); err == nil {
		t.Fatal("bad digest format accepted")
	}
	if _, err := sendChunk(t, n, protocol.UpgradeArgs{Version: "v", SHA256: sha, Size: int64(len(bin)), Offset: 10, Data: whole}); err == nil || !strings.Contains(err.Error(), "offset 0") {
		t.Fatalf("non-zero first offset accepted: %v", err)
	}
	wrong := strings.Repeat("ab", 32)
	if _, err := sendChunk(t, n, protocol.UpgradeArgs{Version: "v", SHA256: wrong, Size: int64(len(bin)), Data: whole, Done: true}); err == nil || !strings.Contains(err.Error(), "mismatch") {
		t.Fatalf("digest mismatch accepted: %v", err)
	}
	if _, err := os.Stat(service.StagedBinary(wrong[:12])); !os.IsNotExist(err) {
		t.Fatal("staging file not removed after a failed upload")
	}
	// A mid-stream gap is refused and the upload can be restarted from zero.
	half := len(bin) / 2
	if _, err := sendChunk(t, n, protocol.UpgradeArgs{Version: "v", SHA256: sha, Size: int64(len(bin)), Data: base64.StdEncoding.EncodeToString(bin[:half])}); err != nil {
		t.Fatal(err)
	}
	if _, err := sendChunk(t, n, protocol.UpgradeArgs{Version: "v", SHA256: sha, Size: int64(len(bin)), Offset: int64(half + 1), Data: "AA=="}); err == nil || !strings.Contains(err.Error(), "offset mismatch") {
		t.Fatalf("gap accepted: %v", err)
	}
	if res, err := sendChunk(t, n, protocol.UpgradeArgs{Version: "v", SHA256: sha, Size: int64(len(bin)), Offset: int64(half), Data: base64.StdEncoding.EncodeToString(bin[half:]), Done: true}); err != nil || !res.Installed {
		t.Fatalf("resume after gap failed: %v %+v", err, res)
	}
	time.Sleep(installerDelay + 300*time.Millisecond)
}
