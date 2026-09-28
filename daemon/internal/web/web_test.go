package web

import (
	"bufio"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/peter-dolkens/vineyard/daemon/internal/mesh"
	"github.com/peter-dolkens/vineyard/daemon/internal/protocol"
)

// fakeDaemon answers like the local daemon does a viewer: a fleet on connect, an echo for "req".
type fakeDaemon struct {
	dials  atomic.Int32
	closed chan struct{}
}

func (f *fakeDaemon) dial() (*mesh.Conn, error) {
	f.dials.Add(1)
	a, b := net.Pipe()
	d := mesh.NewConn(b)
	go func() {
		defer func() { f.closed <- struct{}{} }()
		_ = d.Send(map[string]any{"t": "fleet", "self": "atelier", "entries": []any{
			map[string]any{"snapshot": map[string]any{"machineId": "atelier", "name": "atelier"}, "online": true},
		}, "peers": []any{}})
		for {
			line, err := d.Recv(5 * time.Second)
			if err != nil {
				return
			}
			var r protocol.Request
			_ = json.Unmarshal(line, &r)
			if r.T == "req" {
				data, _ := json.Marshal(map[string]any{"op": r.Op, "target": r.Target, "args": r.Args})
				_ = d.Send(protocol.Response{T: "res", ID: r.ID, OK: r.Op != "kill", Data: data, Error: "refused"})
			}
		}
	}()
	return mesh.NewConn(a), nil
}

type testEnv struct {
	*httptest.Server
	f       *fakeDaemon
	pairing *Pairing
	devices *Devices
	token   string // a paired device's credential; requests below send it unless told otherwise
}

func newTestServer(t *testing.T) (*testEnv, *fakeDaemon) {
	t.Helper()
	f := &fakeDaemon{closed: make(chan struct{}, 4)}
	env := &testEnv{f: f, pairing: NewPairing(), devices: NewDevices(t.TempDir() + "/web-devices.json")}
	s, err := New(Options{Bridge: NewBridge(f.dial, func(string, ...any) {}) /* timers outlive tests */, Version: "test", Self: "atelier", Name: "atelier", Devices: env.devices, Pairing: env.pairing})
	if err != nil {
		t.Fatal(err)
	}
	env.Server = httptest.NewServer(s.Handler())
	t.Cleanup(env.Close)
	env.token, _, err = env.devices.Add("test phone")
	if err != nil {
		t.Fatal(err)
	}
	return env, f
}

func (e *testEnv) cookie() *http.Cookie { return &http.Cookie{Name: deviceCookie, Value: e.token} }

func post(t *testing.T, ts *testEnv, body string, hdr map[string]string) (*http.Response, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest("POST", ts.URL+"/api/req", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Vineyard", "1")
	req.AddCookie(ts.cookie())
	for k, v := range hdr {
		if v == "" {
			req.Header.Del(k)
		} else {
			req.Header.Set(k, v)
		}
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(res.Body).Decode(&out)
	return res, out
}

// openEvents starts the event stream and returns a reader of its data lines.
func openEvents(t *testing.T, ts *testEnv) (next func() map[string]any, stop func()) {
	t.Helper()
	req, _ := http.NewRequest("GET", ts.URL+"/api/events", nil)
	req.AddCookie(ts.cookie())
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if ct := res.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content type %q", ct)
	}
	sc := bufio.NewScanner(res.Body)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	next = func() map[string]any {
		for sc.Scan() {
			if data, ok := strings.CutPrefix(sc.Text(), "data: "); ok {
				var m map[string]any
				if err := json.Unmarshal([]byte(data), &m); err != nil {
					t.Fatal(err)
				}
				return m
			}
		}
		t.Fatal("event stream ended")
		return nil
	}
	return next, func() { res.Body.Close() }
}

func TestEventsCarryStateThenFleet(t *testing.T) {
	ts, _ := newTestServer(t)
	next, stop := openEvents(t, ts)
	defer stop()
	sawFleet := false
	for i := 0; i < 4 && !sawFleet; i++ {
		m := next()
		if m["t"] == "fleet" {
			sawFleet = true
			if m["self"] != "atelier" || len(m["entries"].([]any)) != 1 {
				t.Fatalf("fleet = %v", m)
			}
		}
	}
	if !sawFleet {
		t.Fatal("no fleet message")
	}
	// A second browser gets the cached fleet straight away, without a second daemon link.
	next2, stop2 := openEvents(t, ts)
	defer stop2()
	if m := next2(); m["t"] != "state" || m["state"] != "connected" {
		t.Fatalf("second browser first message = %v", m)
	}
	if m := next2(); m["t"] != "fleet" {
		t.Fatalf("second browser second message = %v", m)
	}
}

func TestRequestRelayAndAllowlist(t *testing.T) {
	ts, _ := newTestServer(t)
	next, stop := openEvents(t, ts)
	defer stop()
	for m := next(); m["state"] != "connected"; m = next() {
	}
	res, out := post(t, ts, `{"target":"forge","op":"transcript","args":{"sessionId":"s1"}}`, nil)
	if res.StatusCode != 200 || out["ok"] != true {
		t.Fatalf("transcript: %d %v", res.StatusCode, out)
	}
	data := out["data"].(map[string]any)
	if data["op"] != "transcript" || data["target"] != "forge" || data["args"].(map[string]any)["sessionId"] != "s1" {
		t.Fatalf("relayed %v", data)
	}
	if _, out := post(t, ts, `{"op":"kill","args":{}}`, nil); out["ok"] != false || out["error"] != "refused" {
		t.Fatalf("daemon error not passed on: %v", out)
	}
	for _, op := range []string{"upgrade", "stage", "rotatekey", "addpeer"} {
		if res, _ := post(t, ts, `{"op":"`+op+`"}`, nil); res.StatusCode != http.StatusForbidden {
			t.Fatalf("%s allowed: %d", op, res.StatusCode)
		}
	}
}

func TestGuards(t *testing.T) {
	ts, _ := newTestServer(t)
	if res, _ := post(t, ts, `{"op":"ping"}`, map[string]string{"X-Vineyard": ""}); res.StatusCode != http.StatusForbidden {
		t.Fatalf("no X-Vineyard: %d", res.StatusCode)
	}
	if res, _ := post(t, ts, `{"op":"ping"}`, map[string]string{"Origin": "http://evil.example"}); res.StatusCode != http.StatusForbidden {
		t.Fatalf("cross origin: %d", res.StatusCode)
	}
	host := strings.TrimPrefix(ts.URL, "http://")
	if res, _ := post(t, ts, `{"op":"ping"}`, map[string]string{"Origin": "http://" + host}); res.StatusCode == http.StatusForbidden {
		t.Fatal("same origin refused")
	}
	req, _ := http.NewRequest("GET", ts.URL+"/api/info", nil)
	req.Host = "evil.example"
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusMisdirectedRequest {
		t.Fatalf("rebinding host answered: %d", res.StatusCode)
	}
	s := &Server{hosts: map[string]bool{"localhost": true, "studio": true}}
	for h, want := range map[string]bool{"192.168.1.5:7735": true, "[::1]:7735": true, "localhost:7735": true, "forge.local:7735": true, "forge:7735": true, "studio": true, "evil.example": false, "10.0.0.1.nip.io": false} {
		if got := s.hostAllowed(h); got != want {
			t.Errorf("hostAllowed(%q) = %v", h, got)
		}
	}
}

func TestDetachesWhenLastBrowserLeaves(t *testing.T) {
	old := linger
	linger = 50 * time.Millisecond
	t.Cleanup(func() { linger = old }) // runs after the server has closed (cleanups are LIFO)
	ts, f := newTestServer(t)
	next, stop := openEvents(t, ts)
	for m := next(); m["state"] != "connected"; m = next() {
	}
	stop()
	select {
	case <-f.closed:
	case <-time.After(3 * time.Second):
		t.Fatal("daemon link still open after the last browser left")
	}
	// Coming back attaches again.
	next, stop = openEvents(t, ts)
	defer stop()
	for m := next(); m["state"] != "connected"; m = next() {
	}
	if n := f.dials.Load(); n != 2 {
		t.Fatalf("dials = %d, want 2", n)
	}
}

func TestTail(t *testing.T) {
	p := t.TempDir() + "/log"
	var b strings.Builder
	for i := 0; i < 5000; i++ {
		b.WriteString("line ")
		b.WriteString(strings.Repeat("x", i%50))
		b.WriteString("\n")
	}
	if err := writeFile(p, b.String()); err != nil {
		t.Fatal(err)
	}
	got, err := tail(p, 3)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(got, "\n"), "\n")
	all := strings.Split(strings.TrimRight(b.String(), "\n"), "\n")
	if len(lines) != 3 || lines[2] != all[len(all)-1] || lines[0] != all[len(all)-3] {
		t.Fatalf("tail = %q", lines)
	}
}

func writeFile(p, s string) error { return os.WriteFile(p, []byte(s), 0o600) }

func TestPairingGatesEverything(t *testing.T) {
	ts, _ := newTestServer(t)
	anon := func(method, path, body string) (int, map[string]any, *http.Response) {
		req, _ := http.NewRequest(method, ts.URL+path, strings.NewReader(body))
		req.Header.Set("X-Vineyard", "1")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(res.Body).Decode(&out)
		return res.StatusCode, out, res
	}
	for _, p := range []struct{ m, path string }{{"GET", "/api/events"}, {"POST", "/api/req"}, {"GET", "/api/log"}, {"POST", "/api/restart"}, {"POST", "/api/pair/new"}, {"GET", "/api/devices"}, {"POST", "/api/devices/revoke"}} {
		if code, _, _ := anon(p.m, p.path, "{}"); code != http.StatusUnauthorized {
			t.Errorf("%s %s unpaired: %d", p.m, p.path, code)
		}
	}
	if _, out, _ := anon("GET", "/api/info", ""); out["paired"] != false || out["version"] != nil {
		t.Fatalf("unpaired info says too much: %v", out)
	}
	if code, _, _ := anon("POST", "/api/pair", `{"code":"WRONGONE"}`); code != http.StatusForbidden {
		t.Fatalf("wrong code: %d", code)
	}
	code, _ := ts.pairing.New()
	status, out, res := anon("POST", "/api/pair", `{"code":"`+strings.ToLower(code[:4])+"-"+code[4:]+`","name":"iPhone"}`)
	if status != 200 || out["ok"] != true {
		t.Fatalf("pair: %d %v", status, out)
	}
	var cookie *http.Cookie
	for _, c := range res.Cookies() {
		if c.Name == deviceCookie {
			cookie = c
		}
	}
	if cookie == nil || !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode {
		t.Fatalf("device cookie: %+v", cookie)
	}
	if status, _, _ := anon("POST", "/api/pair", `{"code":"`+code+`"}`); status != http.StatusForbidden {
		t.Fatal("a pairing code worked twice")
	}
	// The new device's credential works, and revoking it signs it out.
	ts.token = cookie.Value
	if res, out := post(t, ts, `{"op":"ping"}`, nil); res.StatusCode == http.StatusUnauthorized || out["ok"] == false && out["unpaired"] == true {
		t.Fatalf("paired request refused: %d %v", res.StatusCode, out)
	}
	id := out["device"].(map[string]any)["id"].(string)
	if err := ts.devices.Revoke(id, false); err != nil {
		t.Fatal(err)
	}
	if res, _ := post(t, ts, `{"op":"ping"}`, nil); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("revoked device still accepted: %d", res.StatusCode)
	}
}

func TestPairingStopsGuessing(t *testing.T) {
	p := NewPairing()
	code, _ := p.New()
	for i := 0; i < maxFailures; i++ {
		_ = p.Redeem("AAAAAAAA")
	}
	if err := p.Redeem(code); err != errPairPaused {
		t.Fatalf("real code after %d wrong ones: %v", maxFailures, err)
	}
	p.paused = time.Time{}
	if err := p.Redeem(code); err == nil {
		t.Fatal("outstanding codes survived the pause")
	}
}

func TestDevicesPersistAndAreShared(t *testing.T) {
	path := t.TempDir() + "/web-devices.json"
	a, b := NewDevices(path), NewDevices(path) // the daemon and a vineyardd web on one machine
	tok, dev, err := a.Add("iPad")
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := b.Check(tok); !ok || got.ID != dev.ID {
		t.Fatal("second reader does not see the device")
	}
	if st, _ := os.Stat(path); st.Mode().Perm() != 0o600 {
		t.Fatalf("devices file mode %v", st.Mode().Perm())
	}
	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), tok) {
		t.Fatal("credential stored in the clear")
	}
	if l := b.List(); len(l) != 1 || l[0].Hash != "" {
		t.Fatalf("list leaks hashes: %+v", l)
	}
	if err := b.Revoke("", true); err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	if _, ok := a.Check(tok); ok {
		t.Fatal("revoked in one reader, accepted by the other")
	}
}

func TestRunnerStartsAndStops(t *testing.T) {
	f := &fakeDaemon{closed: make(chan struct{}, 4)}
	r := NewRunner(RunnerOptions{Dial: f.dial, Name: "atelier", DevicesFile: t.TempDir() + "/d.json"})
	if _, err := r.Pair(); err == nil {
		t.Fatal("pairing code minted while the app is off")
	}
	if err := r.Set("127.0.0.1:0", nil); err != nil {
		t.Fatal(err)
	}
	st := r.Status()
	if len(st.URLs) != 1 || !strings.HasPrefix(st.URLs[0], "http://127.0.0.1:") {
		t.Fatalf("status %+v", st)
	}
	res, err := http.Get(st.URLs[0] + "/api/info")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	raw, err := r.Pair()
	if err != nil || !strings.Contains(string(raw), st.URLs[0]+"/#pair=") {
		t.Fatalf("pair: %s %v", raw, err)
	}
	if err := r.Set("", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := http.Get(st.URLs[0] + "/api/info"); err == nil {
		t.Fatal("still serving after Set(\"\")")
	}
	if st := r.Status(); st.Listen != "" || len(st.URLs) != 0 {
		t.Fatalf("status after stop %+v", st)
	}
}
