//go:build !windows

package mesh

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/peter-dolkens/vineyard/daemon/internal/model"
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

// A second sender starting the same build at offset 0 while the first is still streaming is told to
// wait; once the first sender has been silent for staleAfter, a fresh start at 0 is accepted.
func TestUpgradeSecondSenderIsToldToWait(t *testing.T) {
	n := newTestNode(t, "zebra")
	bin := fakeBinary()
	sum := sha256.Sum256(bin)
	sha := hex.EncodeToString(sum[:])
	base := protocol.UpgradeArgs{Version: "9.9.9-fake", SHA256: sha, Size: int64(len(bin))}
	half := len(bin) / 2

	first := base
	first.Data = base64.StdEncoding.EncodeToString(bin[:half])
	if _, err := sendChunk(t, n, first); err != nil {
		t.Fatal(err)
	}
	if _, err := sendChunk(t, n, first); err == nil || !strings.Contains(err.Error(), "already in progress") {
		t.Fatalf("second sender at offset 0 should be told to wait, got %v", err)
	}
	// The first sender is still fine.
	rest := base
	rest.Offset = int64(half)
	rest.Data = base64.StdEncoding.EncodeToString(bin[half:])
	if res, err := sendChunk(t, n, rest); err != nil || res.Received != int64(len(bin)) {
		t.Fatalf("first sender interrupted: %v %+v", err, res)
	}

	// Abandoned upload: after staleAfter a newcomer may start over.
	n.upgrade.mu.Lock()
	n.upgrade.cur[sha].touched = time.Now().Add(-staleAfter - time.Second)
	n.upgrade.mu.Unlock()
	if res, err := sendChunk(t, n, first); err != nil || res.Received != int64(half) {
		t.Fatalf("restart after a stale upload refused: %v %+v", err, res)
	}
}

func TestUpgradeRefusesDowngrade(t *testing.T) {
	n := newTestNode(t, "zebra")
	n.opts.Version = "0.4.0"
	bin := fakeBinary()
	sum := sha256.Sum256(bin)
	sha := hex.EncodeToString(sum[:])
	a := protocol.UpgradeArgs{Version: "0.3.9", SHA256: sha, Size: int64(len(bin)), Data: base64.StdEncoding.EncodeToString(bin[:1024])}
	if _, err := sendChunk(t, n, a); err == nil || !strings.Contains(err.Error(), "downgrade") {
		t.Fatalf("older version accepted without force: %v", err)
	}
	a.Force = true
	if _, err := sendChunk(t, n, a); err != nil {
		t.Fatalf("forced downgrade refused: %v", err)
	}
}

// "stage" stores a binary for another platform under the daemon's own version, and "dist" then lists
// it as available and no longer wanted.
func TestStageStoresForDistribution(t *testing.T) {
	n := newTestNode(t, "zebra")
	n.opts.Version = "9.9.9"
	n.store["apple"] = model.FleetEntry{Snapshot: model.Snapshot{MachineID: "apple", DaemonVersion: "9.9.8", Host: model.HostInfo{OS: "plan9", Arch: "mips"}}, Online: true}
	n.store["pear"] = model.FleetEntry{Snapshot: model.Snapshot{MachineID: "pear", DaemonVersion: "9.9.8", Host: model.HostInfo{OS: runtime.GOOS, Arch: runtime.GOARCH}}, Online: true}

	raw, err := n.handleDist()
	if err != nil {
		t.Fatal(err)
	}
	var d protocol.DistResult
	_ = json.Unmarshal(raw, &d)
	if d.Version != "9.9.9" || !slices.Contains(d.Have, runtime.GOOS+"-"+runtime.GOARCH) || !slices.Equal(d.Want, []string{"plan9-mips"}) {
		t.Fatalf("dist before staging: %+v", d)
	}

	bin := fakeBinary()
	sum := sha256.Sum256(bin)
	sha := hex.EncodeToString(sum[:])
	a := protocol.UpgradeArgs{Version: "9.9.8", Platform: "plan9-mips", SHA256: sha, Size: int64(len(bin)), Data: base64.StdEncoding.EncodeToString(bin), Done: true}
	if _, err := n.handleStage(a); err == nil || !strings.Contains(err.Error(), "only distributes") {
		t.Fatalf("staging a foreign version accepted: %v", err)
	}
	a.Version = "9.9.9"
	raw, err = n.handleStage(a)
	if err != nil {
		t.Fatal(err)
	}
	var res protocol.UpgradeResult
	_ = json.Unmarshal(raw, &res)
	if !res.Stored {
		t.Fatalf("not stored: %+v", res)
	}
	got, err := os.ReadFile(service.DistBinary("9.9.9", "plan9", "mips"))
	if err != nil || !bytes.Equal(got, bin) {
		t.Fatalf("stored binary wrong: %v", err)
	}
	raw, _ = n.handleDist()
	var after protocol.DistResult
	_ = json.Unmarshal(raw, &after)
	if !slices.Contains(after.Have, "plan9-mips") || len(after.Want) != 0 {
		t.Fatalf("dist after staging: %+v", after)
	}
	if n.binaryFor("plan9", "mips") == "" || n.binaryFor("plan9", "arm") != "" {
		t.Fatal("binaryFor does not reflect the store")
	}
	service.CleanDist("other")
	if n.binaryFor("plan9", "mips") != "" {
		t.Fatal("CleanDist kept a version it should have dropped")
	}
}
