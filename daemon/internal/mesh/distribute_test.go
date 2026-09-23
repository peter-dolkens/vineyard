package mesh

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/peter-dolkens/vineyard/daemon/internal/model"
	"github.com/peter-dolkens/vineyard/daemon/internal/protocol"
	"github.com/peter-dolkens/vineyard/daemon/internal/service"
)

// fakePeer answers upgrade requests on the far end of a pipe like a daemon would, collecting the bytes.
func fakePeer(t *testing.T, c *Conn, version string) (<-chan []byte, <-chan protocol.UpgradeArgs) {
	t.Helper()
	got := make(chan []byte, 1)
	first := make(chan protocol.UpgradeArgs, 1)
	go func() {
		var buf bytes.Buffer
		sent := false
		for {
			b, err := c.Recv(5 * time.Second)
			if err != nil {
				return
			}
			var r protocol.Request
			if json.Unmarshal(b, &r) != nil || r.T != "req" {
				continue
			}
			var a protocol.UpgradeArgs
			_ = json.Unmarshal(r.Args, &a)
			if !sent {
				first <- a
				sent = true
			}
			if a.Version == version {
				_ = c.Send(protocol.Response{T: "res", ID: r.ID, OK: false, Error: "already running " + version})
				return
			}
			chunk, _ := base64.StdEncoding.DecodeString(a.Data)
			buf.Write(chunk)
			res := protocol.UpgradeResult{Received: int64(buf.Len()), Installed: a.Done, Version: a.Version}
			data, _ := json.Marshal(res)
			_ = c.Send(protocol.Response{T: "res", ID: r.ID, OK: true, Data: data})
			if a.Done {
				got <- buf.Bytes()
				return
			}
		}
	}()
	return got, first
}

func connectFakePeer(t *testing.T, n *Node, id string) (*link, *Conn) {
	t.Helper()
	ours, theirs := net.Pipe()
	l := &link{conn: NewConn(ours), outbound: true, peerID: id, role: "peer"}
	n.mu.Lock()
	n.links[l] = struct{}{}
	n.peers[id] = &peerState{id: id, link: l}
	n.mu.Unlock()
	// Our side of the link needs a reader, as serve would provide, so the peer's answers reach dispatch.
	go func() {
		for {
			b, err := l.conn.Recv(10 * time.Second)
			if err != nil {
				n.unregister(l)
				return
			}
			n.dispatch(l, b)
		}
	}()
	return l, NewConn(theirs)
}

// A peer that reports an older version than ours, on a platform we hold a binary for, receives that
// binary over the peer link in order and is told which version it is getting.
func TestDistributesStoredBinaryToOlderPeer(t *testing.T) {
	n := newTestNode(t, "zebra")
	n.opts.Version = "9.9.9"
	bin := fakeBinary()
	dest := service.DistBinary("9.9.9", "plan9", "mips")
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dest, bin, 0o755); err != nil {
		t.Fatal(err)
	}
	_, far := connectFakePeer(t, n, "apple")
	got, first := fakePeer(t, far, "9.9.8")

	// The peer's snapshot arrives (as dispatch would deliver it).
	n.considerPeer("apple", "9.9.8", "plan9", "mips")
	select {
	case a := <-first:
		sum := sha256.Sum256(bin)
		if a.Version != "9.9.9" || a.SHA256 != hex.EncodeToString(sum[:]) || a.Size != int64(len(bin)) || a.Offset != 0 {
			t.Fatalf("first chunk: %+v", a)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no upgrade request reached the peer")
	}
	select {
	case b := <-got:
		if !bytes.Equal(b, bin) {
			t.Fatal("peer received different bytes")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("upload did not complete")
	}
	// One attempt per peer and version: once the push has wound down, asking again does nothing.
	inflight := func() bool {
		n.dist.mu.Lock()
		defer n.dist.mu.Unlock()
		tr := n.dist.tried["apple"]
		return tr == nil || tr.inflight
	}
	deadline := time.Now().Add(2 * time.Second)
	for inflight() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if inflight() {
		t.Fatal("push never marked finished")
	}
	n.considerPeer("apple", "9.9.8", "plan9", "mips")
	if inflight() {
		t.Fatal("a second attempt started inside the retry window")
	}
}

// No push when the peer is current, newer, on an unknown platform, or when we have no binary for it.
func TestDistributionStaysQuiet(t *testing.T) {
	n := newTestNode(t, "zebra")
	n.opts.Version = "9.9.9"
	_, far := connectFakePeer(t, n, "apple")
	_, first := fakePeer(t, far, "9.9.9")

	n.considerPeer("apple", "9.9.9", "plan9", "mips")  // same version
	n.considerPeer("apple", "10.0.0", "plan9", "mips") // newer
	n.considerPeer("apple", "dev", "plan9", "mips")    // unparseable
	n.considerPeer("apple", "9.9.8", "", "")           // platform unknown (no snapshot cached)
	n.considerPeer("apple", "9.9.8", "plan9", "mips")  // no binary in the store
	select {
	case a := <-first:
		t.Fatalf("unexpected upgrade request: %+v", a)
	case <-time.After(300 * time.Millisecond):
	}

	// With a cached snapshot naming the platform, a version-only hint (from a hello) is enough.
	n.store["apple"] = model.FleetEntry{Snapshot: model.Snapshot{MachineID: "apple", Host: model.HostInfo{OS: "plan9", Arch: "mips"}}}
	dest := service.DistBinary("9.9.9", "plan9", "mips")
	_ = os.MkdirAll(filepath.Dir(dest), 0o755)
	_ = os.WriteFile(dest, fakeBinary(), 0o755)
	n.considerPeer("apple", "9.9.8", "", "")
	select {
	case a := <-first:
		if a.Version != "9.9.9" {
			t.Fatalf("first chunk: %+v", a)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no upgrade request after the platform became known")
	}
}

// A request this daemon originates is answered through its channel, and fails cleanly when the link
// drops or the peer never answers.
func TestOriginatedRequestLifecycle(t *testing.T) {
	n := newTestNode(t, "zebra")
	l, far := connectFakePeer(t, n, "apple")
	go func() {
		b, err := far.Recv(2 * time.Second)
		if err != nil {
			return
		}
		var r protocol.Request
		_ = json.Unmarshal(b, &r)
		_ = far.Send(protocol.Response{T: "res", ID: r.ID, OK: true, Data: json.RawMessage(`{"pong":true}`)})
		// Second request: never answered; the link is closed instead.
		if _, err := far.Recv(2 * time.Second); err == nil {
			far.Close()
		}
	}()
	data, err := n.request(l, "ping", struct{}{}, 2*time.Second)
	if err != nil || string(data) != `{"pong":true}` {
		t.Fatalf("request: %v %s", err, data)
	}
	if _, err := n.request(l, "ping", struct{}{}, 5*time.Second); err == nil {
		t.Fatal("request on a dropped link should fail")
	}
	n.mu.Lock()
	left := len(n.pending)
	n.mu.Unlock()
	if left != 0 {
		t.Fatalf("%d pending requests leaked", left)
	}
}
