// Package web serves the mobile web app (`vineyardd web`): a static bundle plus a small HTTP bridge
// to the local daemon, so a phone on the same network can see and drive the whole fleet.
//
// The browser talks to this process only: an event stream (/api/events) carries the fleet exactly as
// the daemon tells a viewer, and POST /api/req relays one operation (transcript, send, respond, …)
// to any machine through the local daemon. Every API call but pairing needs a paired device: a
// browser that redeemed a single-use pairing code and holds the device credential it got back in an
// HttpOnly cookie (devices.go). The guards in `guard` stop other web pages (cross-site requests, DNS
// rebinding) from acting through a paired browser. Traffic is plain HTTP unless the user puts a TLS
// proxy or a VPN in front: securing the route is up to them, and the setting that turns this on says so.
package web

import (
	"bufio"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

//go:embed static
var embedded embed.FS

// Ops the web app may relay. Pushing daemon binaries (upgrade, stage), rotating the fleet key and
// minting invite codes (which admit a new machine to the fleet) stay with VS Code and the CLI.
var allowedOps = map[string]bool{
	"transcript": true, "send": true, "spawn": true, "takeover": true, "respond": true,
	"interrupt": true, "stop": true, "stoptask": true, "configure": true, "login": true, "accounts": true,
	"rename": true, "wake": true, "sessions": true, "kill": true, "probe": true,
	"removepeer": true, "descendants": true, "version": true, "ping": true,
}

const (
	maxBody        = 48 << 20 // attachments travel base64 in the request: a few photos
	maxReqTimeout  = 180 * time.Second
	defReqTimeout  = 30 * time.Second
	sseKeepalive   = 20 * time.Second
	logTailDefault = 300
)

type Options struct {
	Bridge  *Bridge
	Version string
	// Self and Name identify the machine this server runs on (the local daemon's machine).
	Self, Name string
	// Static overrides the embedded app, e.g. a directory esbuild writes to while developing.
	Static fs.FS
	// AllowHosts are extra Host names to answer to, beyond IP literals, localhost, single-label and
	// .local names and this machine's hostname.
	AllowHosts []string
	// LogFile is the local daemon's log, offered read-only at /api/log.
	LogFile string
	// Restart restarts the local daemon's service.
	Restart func() error
	// Devices are the paired browsers; Pairing mints and redeems the codes that pair new ones.
	Devices *Devices
	Pairing *Pairing
	// URLs are the addresses this app is reachable at, for pairing links.
	URLs func() []string
}

const deviceCookie = "vineyard_device"

type ctxKey struct{}

func deviceOf(r *http.Request) (Device, bool) {
	d, ok := r.Context().Value(ctxKey{}).(Device)
	return d, ok
}

type Server struct {
	o     Options
	files fs.FS
	hosts map[string]bool
}

func New(o Options) (*Server, error) {
	files := o.Static
	if files == nil {
		sub, err := fs.Sub(embedded, "static")
		if err != nil {
			return nil, err
		}
		files = sub
	}
	s := &Server{o: o, files: files, hosts: map[string]bool{"localhost": true}}
	for _, h := range o.AllowHosts {
		s.hosts[strings.ToLower(strings.TrimSuffix(h, "."))] = true
	}
	if h, err := os.Hostname(); err == nil {
		h = strings.ToLower(h)
		s.hosts[h] = true
		s.hosts[strings.SplitN(h, ".", 2)[0]] = true
	}
	return s, nil
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/info", s.info)
	mux.HandleFunc("POST /api/pair", s.pair)
	mux.Handle("GET /api/events", s.paired(s.events))
	mux.Handle("POST /api/req", s.paired(s.request))
	mux.Handle("GET /api/log", s.paired(s.logTail))
	mux.Handle("POST /api/restart", s.paired(s.restart))
	mux.Handle("POST /api/pair/new", s.paired(s.pairNew))
	mux.Handle("GET /api/devices", s.paired(s.devices))
	mux.Handle("POST /api/devices/revoke", s.paired(s.revoke))
	static := http.FileServer(http.FS(s.files))
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		// Small files on a local network: always revalidate so a new build shows on the next load.
		w.Header().Set("Cache-Control", "no-cache")
		static.ServeHTTP(w, r)
	})
	return s.guard(mux)
}

// guard refuses requests that did not come from the app itself: a Host we do not answer to (DNS
// rebinding: evil.example resolving to this machine) and, for anything that acts, a cross-origin
// caller. Acting requests must also carry X-Vineyard, which a cross-site page cannot add without a
// CORS preflight this server never approves.
func (s *Server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.hostAllowed(r.Host) {
			http.Error(w, "unknown host name; start vineyardd web with --allow-host "+hostOnly(r.Host), http.StatusMisdirectedRequest)
			return
		}
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if r.Header.Get("X-Vineyard") == "" {
				http.Error(w, "missing X-Vineyard header", http.StatusForbidden)
				return
			}
			if o := r.Header.Get("Origin"); o != "" && !sameOrigin(o, r.Host) {
				http.Error(w, "cross-origin request refused", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// paired lets a request through only from a paired device.
func (s *Server) paired(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(deviceCookie)
		if err != nil || s.o.Devices == nil {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"ok": false, "error": "not paired", "unpaired": true})
			return
		}
		dev, ok := s.o.Devices.Check(c.Value)
		if !ok {
			clearCookie(w, r)
			writeJSON(w, http.StatusUnauthorized, map[string]any{"ok": false, "error": "this device is no longer paired", "unpaired": true})
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, dev)))
	})
}

func secure(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

func setCookie(w http.ResponseWriter, r *http.Request, token string) {
	http.SetCookie(w, &http.Cookie{Name: deviceCookie, Value: token, Path: "/", MaxAge: int(DeviceTTL.Seconds()), HttpOnly: true, Secure: secure(r), SameSite: http.SameSiteStrictMode})
}

func clearCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: deviceCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: secure(r), SameSite: http.SameSiteStrictMode})
}

func (s *Server) hostAllowed(host string) bool {
	h := strings.ToLower(strings.TrimSuffix(hostOnly(host), "."))
	if h == "" {
		return false
	}
	if net.ParseIP(strings.Trim(h, "[]")) != nil {
		return true
	}
	if !strings.Contains(h, ".") || strings.HasSuffix(h, ".local") {
		return true
	}
	return s.hosts[h]
}

func hostOnly(hostport string) string {
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		return h
	}
	return hostport
}

func sameOrigin(origin, host string) bool {
	rest, ok := strings.CutPrefix(origin, "http://")
	if !ok {
		rest, ok = strings.CutPrefix(origin, "https://")
	}
	return ok && strings.EqualFold(rest, host)
}

// events streams the fleet as server-sent events: a state message, the fleet, then every update.
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no")
	c := s.o.Bridge.subscribe()
	defer s.o.Bridge.unsubscribe(c)
	bw := bufio.NewWriter(w)
	_, _ = fmt.Fprint(bw, "retry: 2000\n\n")
	_ = bw.Flush()
	flusher.Flush()
	keep := time.NewTicker(sseKeepalive)
	defer keep.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case msg, ok := <-c.ch:
			if !ok {
				return // fell behind; the page reconnects and gets the whole fleet again
			}
			_, _ = bw.WriteString("data: ")
			_, _ = bw.Write(msg)
			_, _ = bw.WriteString("\n\n")
			// Drain whatever else is queued before flushing, so a burst goes out as one write.
			for drained := false; !drained; {
				select {
				case more, ok := <-c.ch:
					if !ok {
						_ = bw.Flush()
						return
					}
					_, _ = bw.WriteString("data: ")
					_, _ = bw.Write(more)
					_, _ = bw.WriteString("\n\n")
				default:
					drained = true
				}
			}
			if bw.Flush() != nil {
				return
			}
			flusher.Flush()
		case <-keep.C:
			_, _ = bw.WriteString(": keepalive\n\n")
			if bw.Flush() != nil {
				return
			}
			flusher.Flush()
		}
	}
}

type reqBody struct {
	Target    string          `json:"target"`
	Op        string          `json:"op"`
	Args      json.RawMessage `json:"args"`
	TimeoutMs int             `json:"timeoutMs"`
}

func (s *Server) request(w http.ResponseWriter, r *http.Request) {
	var body reqBody
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "bad request: " + err.Error()})
		return
	}
	if !allowedOps[body.Op] {
		writeJSON(w, http.StatusForbidden, map[string]any{"ok": false, "error": fmt.Sprintf("%q is not available from the web app", body.Op)})
		return
	}
	timeout := defReqTimeout
	if body.TimeoutMs > 0 {
		timeout = min(time.Duration(body.TimeoutMs)*time.Millisecond, maxReqTimeout)
	}
	data, err := s.o.Bridge.Request(body.Target, body.Op, body.Args, timeout)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	if len(data) == 0 {
		data = json.RawMessage("null")
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "data": data})
}

// info says whether this browser is paired, and for a paired one what this server offers.
func (s *Server) info(w http.ResponseWriter, r *http.Request) {
	out := map[string]any{"name": s.o.Name, "paired": false}
	if c, err := r.Cookie(deviceCookie); err == nil && s.o.Devices != nil {
		if dev, ok := s.o.Devices.Check(c.Value); ok {
			setCookie(w, r, c.Value) // every app start renews the 30 days
			out = map[string]any{"version": s.o.Version, "self": s.o.Self, "name": s.o.Name, "restart": s.o.Restart != nil, "log": s.o.LogFile != "", "paired": true, "device": map[string]string{"id": dev.ID, "name": dev.Name}}
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// pair redeems a pairing code and hands the browser its device credential.
func (s *Server) pair(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Code string `json:"code"`
		Name string `json:"name"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "bad request"})
		return
	}
	if s.o.Pairing == nil || s.o.Devices == nil {
		writeJSON(w, http.StatusForbidden, map[string]any{"ok": false, "error": "pairing is not available"})
		return
	}
	if err := s.o.Pairing.Redeem(body.Code); err != nil {
		writeJSON(w, http.StatusForbidden, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	token, dev, err := s.o.Devices.Add(body.Name)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	setCookie(w, r, token)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "device": map[string]string{"id": dev.ID, "name": dev.Name}})
}

// PairLinks mints a pairing code and the links that carry it.
func PairLinks(p *Pairing, urls []string) map[string]any {
	code, ttl := p.New()
	links := make([]string, 0, len(urls))
	for _, u := range urls {
		links = append(links, u+"/#pair="+code)
	}
	return map[string]any{"code": code, "expiresInSeconds": int(ttl.Seconds()), "links": links}
}

func (s *Server) pairNew(w http.ResponseWriter, r *http.Request) {
	var urls []string
	if s.o.URLs != nil {
		urls = s.o.URLs()
	}
	// The address this browser is using works for the next device too; offer it first.
	scheme := "http"
	if secure(r) {
		scheme = "https"
	}
	here := scheme + "://" + r.Host
	out := PairLinks(s.o.Pairing, append([]string{here}, without(urls, here)...))
	out["ok"] = true
	writeJSON(w, http.StatusOK, out)
}

func without(xs []string, x string) []string {
	out := make([]string, 0, len(xs))
	for _, v := range xs {
		if v != x {
			out = append(out, v)
		}
	}
	return out
}

func (s *Server) devices(w http.ResponseWriter, r *http.Request) {
	me, _ := deviceOf(r)
	list := s.o.Devices.List()
	out := make([]map[string]any, 0, len(list))
	for _, d := range list {
		out = append(out, map[string]any{"id": d.ID, "name": d.Name, "pairedAt": d.PairedAt, "lastSeen": d.LastSeen, "current": d.ID == me.ID})
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "devices": out})
}

func (s *Server) revoke(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID  string `json:"id"`
		All bool   `json:"all"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "bad request"})
		return
	}
	if err := s.o.Devices.Revoke(body.ID, body.All); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	if me, _ := deviceOf(r); body.All || body.ID == me.ID {
		clearCookie(w, r)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// logTail returns the last lines of the local daemon's log.
func (s *Server) logTail(w http.ResponseWriter, r *http.Request) {
	if s.o.LogFile == "" {
		http.Error(w, "no log", http.StatusNotFound)
		return
	}
	n := logTailDefault
	if v := r.URL.Query().Get("lines"); v != "" {
		_, _ = fmt.Sscan(v, &n)
		n = max(1, min(n, 5000))
	}
	text, err := tail(s.o.LogFile, n)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = io.WriteString(w, text)
}

func (s *Server) restart(w http.ResponseWriter, _ *http.Request) {
	if s.o.Restart == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": "restart is not available"})
		return
	}
	if err := s.o.Restart(); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// tail reads the last n lines of a file without loading all of it.
func tail(path string, n int) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "", err
	}
	const chunk = 64 << 10
	size := st.Size()
	var buf []byte
	for off := size; off > 0 && strings.Count(string(buf), "\n") <= n; {
		step := int64(chunk)
		if off < step {
			step = off
		}
		off -= step
		part := make([]byte, step)
		if _, err := f.ReadAt(part, off); err != nil && !errors.Is(err, io.EOF) {
			return "", err
		}
		buf = append(part, buf...)
	}
	lines := strings.Split(strings.TrimRight(string(buf), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n") + "\n", nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
