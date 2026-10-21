package agenttools

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/json"
	"fmt"
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
// e2e runs a real daemon node on loopback, giving tools to agents, and an MCP server that reaches it
// through its own viewer client, as `vineyardd mcp` does.
func e2e(t *testing.T, tools ...string) (*Server, string) {
	t.Helper()
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
	target := "0b9b6d1e-1111-4222-8333-944455556666"
	ws := t.TempDir()
	cfg := config.New("box", "box", 0, addr)
	cfg.Listen, cfg.Uplink = addr, "off"
	cfg.AgentTools = tools
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
	s := New(Options{Machine: "box", Session: "sender", Dial: dial, Enabled: func() []string { return tools }, TranscriptDir: t.TempDir()})

	return s, target
}

func TestSendFileEndToEnd(t *testing.T) {
	s, target := e2e(t, model.AgentToolFiles)
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

func TestCreateWorkspaceEndToEnd(t *testing.T) {
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	s, _ := e2e(t, model.AgentToolNewWorkspaces)
	call := func(path string) (map[string]any, bool) {
		args, _ := json.Marshal(map[string]any{"machine": "box", "path": path})
		res := s.call("create_workspace", args)
		text := res["content"].([]map[string]any)[0]["text"].(string)
		var out map[string]any
		_ = json.Unmarshal([]byte(text), &out)
		if out == nil {
			out = map[string]any{"error": text}
		}
		return out, res["isError"] == true
	}
	out, failed := call("~/Projects/fresh")
	if failed || out["path"] != filepath.Join(home, "Projects", "fresh") || out["created"] != true {
		t.Fatalf("create: %v", out)
	}
	if out, failed := call(filepath.Join(t.TempDir(), "nope")); !failed || !strings.Contains(fmt.Sprint(out["error"]), "outside") {
		t.Fatalf("outside home: %v", out)
	}
}
