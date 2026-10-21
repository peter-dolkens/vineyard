package mesh

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peter-dolkens/vineyard/daemon/internal/model"
	"github.com/peter-dolkens/vineyard/daemon/internal/protocol"
)

const testSession = "0b9b6d1e-1111-4222-8333-944455556666"

// sendFile sends data in chunks of size chunk, as the agent tool does, and returns the last result.
func sendFile(t *testing.T, n *Node, id, name string, data []byte, chunk int, o *protocol.AgentOrigin) (protocol.PutFileResult, error) {
	t.Helper()
	sum := sha256.Sum256(data)
	a := protocol.PutFileArgs{ID: id, SessionID: testSession, Name: name, Size: int64(len(data)), SHA256: hex.EncodeToString(sum[:]), Note: "please read it", Origin: o}
	var res protocol.PutFileResult
	for {
		end := min(int(a.Offset)+chunk, len(data))
		a.Data = base64.StdEncoding.EncodeToString(data[a.Offset:end])
		a.Done = end == len(data)
		raw, err := n.putFile(a)
		if err != nil {
			return res, err
		}
		_ = json.Unmarshal(raw, &res)
		a.Offset = int64(end)
		if a.Done {
			return res, nil
		}
	}
}

func TestPutFileLandsInTheSessionsFolder(t *testing.T) {
	n := newTestNode(t, "m1")
	data := []byte(strings.Repeat("vineyard ", 5000))
	res, err := sendFile(t, n, "transfer-0001", "../../etc/notes.txt", data, 4096, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(ReceivedDir(), testSession, "notes.txt")
	if res.Path != want || res.Received != int64(len(data)) {
		t.Fatalf("result = %+v, want path %s", res, want)
	}
	if got, _ := os.ReadFile(want); string(got) != string(data) {
		t.Fatal("file contents differ")
	}
	if res.Delivered || res.DeliverError == "" {
		t.Fatalf("no such session here, so delivery should fail and say why: %+v", res)
	}
	// The same name again gets a new one rather than overwriting.
	res2, err := sendFile(t, n, "transfer-0002", "notes.txt", []byte("second"), 4096, nil)
	if err != nil || filepath.Base(res2.Path) != "notes (2).txt" {
		t.Fatalf("second file: %+v, %v", res2, err)
	}
	if entries, _ := os.ReadDir(filepath.Join(ReceivedDir(), ".partial")); len(entries) != 0 {
		t.Fatalf("staging left behind: %v", entries)
	}
}

func TestPutFileRefusesDamageBadNamesAndUnapprovedAgents(t *testing.T) {
	n := newTestNode(t, "m1")
	// A checksum that does not match what arrives.
	bad := protocol.PutFileArgs{ID: "transfer-0003", SessionID: testSession, Name: "x.bin", Size: 3, SHA256: strings.Repeat("0", 64), Data: base64.StdEncoding.EncodeToString([]byte("abc")), Done: true}
	if _, err := n.putFile(bad); err == nil || !strings.Contains(err.Error(), "damaged") {
		t.Fatalf("damaged file: %v", err)
	}
	for _, name := range []string{"", ".", "..", "/"} {
		if _, err := sendFile(t, n, "transfer-0004", name, []byte("a"), 10, nil); err == nil {
			t.Errorf("name %q accepted", name)
		}
	}
	traversal := protocol.PutFileArgs{ID: "transfer-0005", SessionID: "../../../tmp", Name: "a", Size: 1, SHA256: strings.Repeat("0", 64)}
	if _, err := n.putFile(traversal); err == nil {
		t.Fatal("a session id that walks out of the folder was accepted")
	}
	// An agent's file needs this machine's own sendFiles setting.
	o := &protocol.AgentOrigin{Machine: "studio", Session: "s0", Hops: 1}
	if _, err := sendFile(t, n, "transfer-0006", "a.txt", []byte("hi"), 10, o); err == nil || !strings.Contains(err.Error(), "sendFiles") {
		t.Fatalf("files off: %v", err)
	}
	n.cfg.AgentTools = []string{model.AgentToolFiles}
	if _, err := sendFile(t, n, "transfer-0007", "a.txt", []byte("hi"), 10, o); err != nil {
		t.Fatalf("files on: %v", err)
	}
	// A chunk out of order is refused.
	sum := sha256.Sum256([]byte("abcdef"))
	gap := protocol.PutFileArgs{ID: "transfer-0008", SessionID: testSession, Name: "g", Size: 6, SHA256: hex.EncodeToString(sum[:]), Data: base64.StdEncoding.EncodeToString([]byte("abc"))}
	if _, err := n.putFile(gap); err != nil {
		t.Fatal(err)
	}
	gap.Offset = 4
	if _, err := n.putFile(gap); err == nil {
		t.Fatal("a gap in the file was accepted")
	}
}
