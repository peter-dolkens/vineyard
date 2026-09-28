package web

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/peter-dolkens/vineyard/daemon/internal/mesh"
	"github.com/peter-dolkens/vineyard/daemon/internal/model"
)

// RunnerOptions configure the web app a daemon (or `vineyardd web`) serves.
type RunnerOptions struct {
	// Dial opens a viewer connection to the local daemon.
	Dial    func() (*mesh.Conn, error)
	Version string
	Self    string
	Name    string
	LogFile string
	Restart func() error
	// DevicesFile is where paired devices are kept (~/.vineyard/web-devices.json).
	DevicesFile string
	// Static overrides the embedded app (development).
	Static fs.FS
	Logf   func(string, ...any)
}

// Runner starts and stops the web app on request: the daemon turns it on and off as the fleet's
// setting says (the "webapp" op), without restarting.
type Runner struct {
	o       RunnerOptions
	bridge  *Bridge
	devices *Devices
	pairing *Pairing

	mu     sync.Mutex
	srv    *http.Server
	listen string
	hosts  []string
	err    string
	port   string
}

func NewRunner(o RunnerOptions) *Runner {
	if o.Logf == nil {
		o.Logf = func(string, ...any) {}
	}
	return &Runner{o: o, bridge: NewBridge(o.Dial, o.Logf), devices: NewDevices(o.DevicesFile), pairing: NewPairing()}
}

// Set serves the app on listen ("" stops it). extraHosts are Host names to answer to beyond the
// defaults (see Server.hostAllowed), for a reverse proxy or a VPN name.
func (r *Runner) Set(listen string, extraHosts []string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if listen == r.listen && strings.Join(extraHosts, ",") == strings.Join(r.hosts, ",") && (r.srv != nil || listen == "") {
		return nil
	}
	if r.srv != nil {
		_ = r.srv.Close()
		r.srv = nil
		r.o.Logf("web app stopped")
	}
	r.listen, r.hosts, r.err, r.port = listen, extraHosts, "", ""
	if listen == "" {
		return nil
	}
	s, err := New(Options{
		Bridge: r.bridge, Version: r.o.Version, Self: r.o.Self, Name: r.o.Name, Static: r.o.Static,
		AllowHosts: extraHosts, LogFile: r.o.LogFile, Restart: r.o.Restart,
		Devices: r.devices, Pairing: r.pairing, URLs: r.URLs,
	})
	if err != nil {
		r.err = err.Error()
		return err
	}
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		r.err = err.Error()
		r.o.Logf("web app: %v", err)
		return err
	}
	_, r.port, _ = net.SplitHostPort(ln.Addr().String())
	srv := &http.Server{Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second}
	r.srv = srv
	go func() {
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			r.mu.Lock()
			if r.srv == srv {
				r.err = err.Error()
			}
			r.mu.Unlock()
		}
	}()
	r.o.Logf("web app serving on %s (pairing required; plain HTTP: securing this route is up to the user)", ln.Addr())
	return nil
}

// Status is what the machine's snapshot reports about the app.
func (r *Runner) Status() model.WebAppStatus {
	r.mu.Lock()
	st := model.WebAppStatus{Listen: r.listen, Error: r.err}
	running := r.srv != nil
	r.mu.Unlock()
	if running {
		st.URLs = r.URLs()
		st.Devices = len(r.devices.List())
	}
	return st
}

// URLs are the addresses the app answers on: each LAN IPv4 address, then <hostname>.local.
func (r *Runner) URLs() []string {
	r.mu.Lock()
	port, listen := r.port, r.listen
	r.mu.Unlock()
	if port == "" {
		return nil
	}
	host, _, _ := net.SplitHostPort(listen)
	if host != "" && host != "0.0.0.0" && host != "::" {
		return []string{"http://" + net.JoinHostPort(host, port)}
	}
	var out []string
	if h, err := os.Hostname(); err == nil {
		out = append(out, "http://"+net.JoinHostPort(strings.ToLower(strings.SplitN(h, ".", 2)[0])+".local", port))
	}
	for _, ip := range LANIPs() {
		out = append(out, "http://"+net.JoinHostPort(ip, port))
	}
	return out
}

// Pair mints a single-use pairing code ("webpair").
func (r *Runner) Pair() (json.RawMessage, error) {
	r.mu.Lock()
	running := r.srv != nil
	r.mu.Unlock()
	if !running {
		return nil, fmt.Errorf("the web app is not running on this machine")
	}
	return json.Marshal(PairLinks(r.pairing, r.URLs()))
}

// Devices lists paired browsers ("webdevices").
func (r *Runner) Devices() (json.RawMessage, error) {
	return json.Marshal(map[string]any{"devices": r.devices.List()})
}

// Revoke signs one device out, or all of them ("webrevoke").
func (r *Runner) Revoke(id string, all bool) error { return r.devices.Revoke(id, all) }

// LANIPs lists this machine's non-loopback, non-link-local IPv4 addresses.
func LANIPs() []string {
	var out []string
	ifaces, _ := net.Interfaces()
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil && !ipn.IP.IsLinkLocalUnicast() {
				out = append(out, ipn.IP.String())
			}
		}
	}
	return out
}
