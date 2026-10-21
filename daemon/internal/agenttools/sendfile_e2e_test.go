package agenttools

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/json"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/peter-dolkens/vineyard/daemon/internal/config"
	"github.com/peter-dolkens/vineyard/daemon/internal/mesh"
	"github.com/peter-dolkens/vineyard/daemon/internal/model"
	"github.com/peter-dolkens/vineyard/daemon/internal/protocol"
)

// A real daemon node on loopback, the MCP server's own viewer client, and send_file through the
// tool-call path: the file crosses the wire in chunks and lands in the session's received folder.
func TestSendFileEndToEnd(t *testing.T) {
	t.Setenv("VINEYARD_DIR", t.TempDir())
	if err := config.GenerateFleetCert(); err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	const target = "0b9b6d1e-1111-4222-8333-944455556666"
	ws := t.TempDir()
	cfg := config.New("box", "box", 0, addr)
	cfg.Listen, cfg.Uplink = addr, "off"
	cfg.AgentTools = []string{model.AgentToolFiles}
	awake := false
	cfg.KeepAwakeWhileWatched = &awake // no caffeinate on the machine running the tests
	node, err := mesh.New(mesh.Options{Config: cfg, Version: "test", Log: log.New(os.Stderr, "box ", 0), Collect: func() model.Snapshot {
		return model.Snapshot{Agents: []model.Agent{{SessionID: target, Alive: true, State: model.StateIdle, WorkspacePath: ws, Cwd: ws}}}
	}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = node.Run(ctx) }()
	time.Sleep(150 * time.Millisecond)

	dial := func() (*mesh.Conn, error) {
		_, cli, err := mesh.FleetTLS()
		if err != nil {
			return nil, err
		}
		raw, err := tls.Dial("tcp", addr, cli)
		if err != nil {
			return nil, err
		}
		c := mesh.NewConn(raw)
		return c, c.Send(protocol.Hello{T: "hello", Role: "viewer", MachineID: "box#mcp-test", Name: "mcp-test", Version: "test", Protocol: protocol.Version})
	}
	s := New(Options{Machine: "box", Session: "sender", Dial: dial, Enabled: func() []string { return []string{model.AgentToolFiles} }, TranscriptDir: t.TempDir()})

	data := make([]byte, 1300<<10) // three chunks
	_, _ = rand.Read(data)
	src := filepath.Join(t.TempDir(), "build output.tar.gz")
	if err := os.WriteFile(src, data, 0o600); err != nil {
		t.Fatal(err)
	}
	args, _ := json.Marshal(map[string]any{"machine": "box", "session_id": target, "path": src, "message": "unpack and run it"})
	res := s.call("send_file", args)
	text := res["content"].([]map[string]any)[0]["text"].(string)
	if res["isError"] == true {
		t.Fatalf("send_file failed: %s", text)
	}
	var out struct {
		RemotePath string `json:"remotePath"`
		Bytes      int64  `json:"bytes"`
		Delivered  bool   `json:"delivered"`
		Note       string `json:"note"`
	}
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatalf("%v: %s", err, text)
	}
	want := filepath.Join(config.Dir(), "received", target, "build output.tar.gz")
	if out.RemotePath != want || out.Bytes != int64(len(data)) {
		t.Fatalf("result = %s", text)
	}
	if got, err := os.ReadFile(want); err != nil || !bytes.Equal(got, data) {
		t.Fatalf("received file differs (%v)", err)
	}
	// No such session process here, so the notice cannot be delivered; the tool says so plainly.
	if out.Delivered || !strings.Contains(out.Note, "not told") {
		t.Fatalf("delivery: %s", text)
	}

	// A machine whose own setting is off refuses an agent's file.
	off, _ := json.Marshal(protocol.AgentToolsArgs{Tools: []string{}, At: time.Now().UnixMilli()})
	if _, err := s.client.Request("", "agenttools", json.RawMessage(off), 5*time.Second); err != nil {
		t.Fatal(err)
	}
	res = s.call("send_file", args)
	if res["isError"] != true || !strings.Contains(res["content"].([]map[string]any)[0]["text"].(string), "sendFiles") {
		t.Fatalf("expected a refusal naming the setting, got %v", res)
	}
}
