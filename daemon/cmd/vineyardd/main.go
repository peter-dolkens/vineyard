// vineyardd – the Vineyard agent daemon. One static binary per machine; see README for the design.
package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/peter-dolkens/vineyard/daemon/internal/auth"
	"github.com/peter-dolkens/vineyard/daemon/internal/claude"
	"github.com/peter-dolkens/vineyard/daemon/internal/codex"
	"github.com/peter-dolkens/vineyard/daemon/internal/config"
	"github.com/peter-dolkens/vineyard/daemon/internal/managed"
	"github.com/peter-dolkens/vineyard/daemon/internal/mesh"
	"github.com/peter-dolkens/vineyard/daemon/internal/model"
	"github.com/peter-dolkens/vineyard/daemon/internal/protocol"
	"github.com/peter-dolkens/vineyard/daemon/internal/service"
	"github.com/peter-dolkens/vineyard/daemon/internal/web"
)

// Version is set at build time via -ldflags "-X main.Version=…".
var Version = "dev"

func usage() {
	fmt.Fprintf(os.Stderr, `vineyardd %s – Vineyard agent daemon

Usage:
  vineyardd init [--name N] [--port P] [--machine-id ID] [--advertise HOST:PORT] [--peer ID=HOST:PORT ...]
        Create ~/.vineyard/config.json and, if missing, the fleet certificate.
  vineyardd run          Run in the foreground (this is what the service runs).
  vineyardd install      Copy this binary to ~/.vineyard/bin and register a login service.
  vineyardd uninstall    Remove the service.
  vineyardd restart      Restart the service.
  vineyardd status       Connect to the local daemon and print the fleet.
  vineyardd probe        Print this machine's snapshot as JSON (no daemon needed).
  vineyardd peer add ID HOST:PORT | peer remove ID | peer list
  vineyardd invite       Ask the local daemon for a single-use invite code (valid 15 minutes).
  vineyardd join CODE [--name N] [--port P] [--machine-id ID] [--advertise HOST:PORT]
        Join a fleet using an invite code: fetches the certificate and peer list, writes config.
  vineyardd rotate-key [--exclude ID ...] [--grace 336h]
        Replace the fleet key everywhere: the new key reaches connected members now and others when
        they next connect within the grace period. Excluded machines are removed and never get it.
  vineyardd keygen       Make this machine's own key (if it has none) and print its public half, for a
        member to certify (Add Machine over SSH does this; the private key never leaves).
  vineyardd web [--listen :7735] [--allow-host NAME ...] [--dev DIR]
        Serve the mobile web app in the foreground and print a pairing code. The daemon serves it
        itself when VS Code's vineyard.webApp setting is on. Plain HTTP: securing it is up to you.
  vineyardd version

Environment: VINEYARD_DIR overrides ~/.vineyard.
`, Version)
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "init":
		err = cmdInit(os.Args[2:])
	case "run":
		err = cmdRun()
	case "install":
		err = cmdInstall()
	case "uninstall":
		var msg string
		msg, err = service.Uninstall()
		fmt.Println(msg)
	case "restart":
		err = service.Restart()
	case "status":
		err = cmdStatus(os.Args[2:])
	case "probe":
		err = cmdProbe()
	case "peer":
		err = cmdPeer(os.Args[2:])
	case "invite":
		err = cmdInvite()
	case "rotate-key":
		err = cmdRotateKey(os.Args[2:])
	case "join":
		err = cmdJoin(os.Args[2:])
	case "keygen":
		err = cmdKeygen()
	case "web":
		err = cmdWeb(os.Args[2:])
	case "version", "--version", "-v":
		fmt.Println(Version)
	case "help", "-h", "--help":
		usage()
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

type peerFlags []string

func (p *peerFlags) String() string     { return strings.Join(*p, ",") }
func (p *peerFlags) Set(v string) error { *p = append(*p, v); return nil }

func cmdInit(args []string) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	name := fs.String("name", "", "display name (default: short hostname)")
	port := fs.Int("port", config.DefaultPort, "listen port")
	machineID := fs.String("machine-id", "", "stable machine id (default: lowercase hostname)")
	advertise := fs.String("advertise", "", "host:port peers should dial (default: <machine-id>:<port>)")
	force := fs.Bool("force", false, "overwrite an existing config")
	var peers peerFlags
	fs.Var(&peers, "peer", "peer as ID=HOST:PORT (repeatable)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if _, err := config.Load(); err == nil && !*force {
		return fmt.Errorf("%s already exists (use --force to overwrite)", config.Path(config.ConfigFile))
	}
	c := config.New(*machineID, *name, *port, *advertise)
	for _, p := range peers {
		id, addr, ok := strings.Cut(p, "=")
		if !ok {
			return fmt.Errorf("bad --peer %q, want ID=HOST:PORT", p)
		}
		c.AddPeer(protocol.PeerAddr{MachineID: id, Addr: addr, Added: time.Now().UnixMilli()})
	}
	if err := c.Save(); err != nil {
		return err
	}
	if !config.HasFleetCert() {
		// A new fleet: a root signs this machine's certificate and is thrown away, so no machine holds
		// a key that could mint identities. Later machines join with an invite from any member.
		if err := config.NewFleet("", c.MachineID); err != nil {
			return err
		}
		fmt.Println("started a new fleet; this machine's certificate is", config.Path(config.MachineCertFile))
	}
	fmt.Printf("wrote %s (machine %s, listen %s)\n", config.Path(config.ConfigFile), c.MachineID, c.Listen)
	return nil
}

func cmdRun() error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config (run `vineyardd init` first): %w", err)
	}
	var logOut io.Writer = os.Stderr
	if runtime.GOOS == "windows" {
		// The scheduled task / Run key give the daemon no useful stderr; keep a file as well.
		if f, err := os.OpenFile(service.LogFile(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); err == nil {
			logOut = io.MultiWriter(os.Stderr, f)
		}
	}
	logger := log.New(logOut, "", log.LstdFlags)
	service.CleanStaged()      // leftovers from a previous self-upgrade
	service.CleanDist(Version) // stored builds of versions we no longer distribute
	collector := claude.NewCollector(cfg.ClaudeDir, cfg.TailLines)
	var node *mesh.Node
	var mgr *managed.Manager
	if !cfg.DisableManaged {
		mgr = managed.New(logger, func() {
			if node != nil {
				node.Kick()
			}
		})
		mgr.ClaudeBin = cfg.ClaudeBin
	}
	authMgr := auth.New(logger)
	authMgr.ClaudeBin = cfg.ClaudeBin
	var codexCollector *codex.Collector
	var codexMgr *codex.Manager
	if !cfg.DisableCodex {
		codexCollector = codex.NewCollector(cfg.CodexDir, cfg.TailLines)
		if !cfg.DisableManaged {
			codexMgr = codex.New(logger, func() {
				if node != nil {
					node.Kick()
				}
			})
			codexMgr.CodexBin = cfg.CodexBin
			codexMgr.CodexDir = cfg.CodexDir
		}
	}
	// The web app is off unless the fleet's vineyard.webApp setting turned it on (config webApp).
	webApp := web.NewRunner(web.RunnerOptions{
		Dial:        func() (*mesh.Conn, error) { return dialLocalViewer(cfg, "web") },
		Version:     Version,
		Self:        cfg.MachineID,
		Name:        cfg.Name,
		LogFile:     service.LogFile(),
		Restart:     service.Restart,
		DevicesFile: config.Path("web-devices.json"),
		Logf:        logger.Printf,
	})
	if cfg.WebApp != "" {
		_ = webApp.Set(cfg.WebApp, cfg.WebAppHosts) // a failure shows in the snapshot
	}
	node, err = mesh.New(mesh.Options{
		Config:    cfg,
		Version:   Version,
		Log:       logger,
		ClaudeDir: collector.ClaudeDir,
		Managed:   mgr,
		Codex:     codexMgr,
		CodexDir:  codexDirOf(codexCollector),
		Auth:      authMgr,
		WebApp:    webApp,
		Collect: func() model.Snapshot {
			r := collector.Collect()
			agents, workspaces := claude.Interpret(cfg.MachineID, r, time.Now().UnixMilli())
			var usage *model.Usage
			if mgr != nil {
				agents = mgr.Merge(cfg.MachineID, agents)
				workspaces = claude.Regroup(cfg.MachineID, agents, workspaces)
				usage = mgr.LatestUsage()
			}
			snap := model.Snapshot{Host: r.Host, Agents: agents, Workspaces: workspaces, HasClaude: r.HasClaude, Usage: usage}
			addCodex(&snap, cfg.MachineID, codexCollector, codexMgr)
			return snap
		},
	})
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	logger.Printf("vineyardd %s starting (config %s)", Version, config.Dir())
	return node.Run(ctx)
}

func cmdInstall() error {
	if _, err := config.Load(); err != nil {
		return fmt.Errorf("load config (run `vineyardd init` first): %w", err)
	}
	if !config.HasFleetCert() {
		return fmt.Errorf("missing fleet certificate in %s", config.Dir())
	}
	msg, err := service.Install()
	if err != nil {
		return err
	}
	fmt.Println(msg)
	return nil
}

func cmdProbe() error {
	cfg, err := config.Load()
	machineID := config.DefaultMachineID()
	claudeDir := ""
	if err == nil {
		machineID = cfg.MachineID
		claudeDir = cfg.ClaudeDir
	}
	c := claude.NewCollector(claudeDir, 80)
	r := c.Collect()
	agents, workspaces := claude.Interpret(machineID, r, time.Now().UnixMilli())
	snap := model.Snapshot{MachineID: machineID, Host: r.Host, Agents: agents, Workspaces: workspaces, HasClaude: r.HasClaude, At: time.Now().UnixMilli(), DaemonVersion: Version}
	if err == nil && cfg.DisableCodex {
		// Codex hidden by config.
	} else {
		codexDir := ""
		if err == nil {
			codexDir = cfg.CodexDir
		}
		addCodex(&snap, machineID, codex.NewCollector(codexDir, 120), nil)
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(snap)
}

func cmdPeer(args []string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if len(args) == 0 {
		args = []string{"list"}
	}
	switch args[0] {
	case "add":
		if len(args) != 3 {
			return fmt.Errorf("usage: peer add ID HOST:PORT")
		}
		cfg.AddPeer(protocol.PeerAddr{MachineID: args[1], Addr: args[2], Added: time.Now().UnixMilli()})
		return cfg.Save()
	case "remove":
		if len(args) != 2 {
			return fmt.Errorf("usage: peer remove ID")
		}
		cfg.RemovePeer(args[1])
		return cfg.Save()
	case "list":
		for _, p := range cfg.Peers {
			fmt.Printf("%s\t%s\n", p.MachineID, p.Addr)
		}
		return nil
	}
	return fmt.Errorf("unknown peer subcommand %q", args[0])
}

// cmdStatus attaches as a viewer, prints the fleet once, and detaches.
func cmdStatus(args []string) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "print the raw fleet message")
	wait := fs.Duration("wait", 2*time.Second, "how long to wait for peers to report before printing")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	conn, err := dialLocalViewer(cfg, "cli")
	if err != nil {
		return err
	}
	defer conn.Close()
	entries := map[string]model.FleetEntry{}
	var peers []protocol.PeerStatus
	deadline := time.Now().Add(*wait)
	for time.Now().Before(deadline) {
		b, err := conn.Recv(time.Until(deadline) + 50*time.Millisecond)
		if err != nil {
			break
		}
		var env protocol.Envelope
		_ = json.Unmarshal(b, &env)
		switch env.T {
		case "hello":
		case "fleet":
			var f protocol.Fleet
			_ = json.Unmarshal(b, &f)
			for _, e := range f.Entries {
				entries[e.Snapshot.MachineID] = e
			}
			peers = f.Peers
		case "update":
			var u protocol.Update
			_ = json.Unmarshal(b, &u)
			entries[u.Entry.Snapshot.MachineID] = u.Entry
		case "peerstatus":
			var p protocol.PeersUpdate
			_ = json.Unmarshal(b, &p)
			peers = p.Peers
		}
	}
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(map[string]any{"self": cfg.MachineID, "entries": entries, "peers": peers})
	}
	ids := make([]string, 0, len(entries))
	for id := range entries {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	names := map[string]string{}
	for id, e := range entries {
		names[id] = e.Snapshot.Name
	}
	via := func(v string) string {
		for _, kind := range []string{"relay", "reported"} {
			if id, ok := strings.CutPrefix(v, kind+":"); ok {
				if names[id] != "" {
					id = names[id]
				}
				return kind + " " + id
			}
		}
		return v
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "MACHINE\tSTATUS\tVIA\tAGENT\tSTATE\tMODEL\tWORKSPACE\tDETAIL")
	for _, id := range ids {
		e := entries[id]
		status := "offline"
		if e.Online {
			status = "online"
		}
		live := 0
		for _, a := range e.Snapshot.Agents {
			if !a.Alive {
				continue
			}
			live++
			title := a.Title
			if title == "" {
				title = a.Name
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", e.Snapshot.Name, status, via(e.Via), title, a.State, shortModel(a.Model), a.WorkspacePath, a.StateDetail)
		}
		if live == 0 {
			fmt.Fprintf(w, "%s\t%s\t%s\t-\t-\t-\t-\t%s\n", e.Snapshot.Name, status, via(e.Via), "no live agents")
		}
	}
	w.Flush()
	for _, id := range ids {
		if up := entries[id].Snapshot.Uplink; up != "" {
			if names[up] != "" {
				up = names[up]
			}
			fmt.Printf("%s: nobody can connect to it; keeps an uplink to %s\n", entries[id].Snapshot.Name, up)
		}
	}
	if m, err := config.LoadMachine(""); err == nil {
		by := "the fleet root"
		if len(m.Chain) > 1 {
			by = config.CertMachineID(m.Chain[1])
		}
		fmt.Printf("identity: this machine's own certificate (key %s), vouched for by %s\n", config.KeyFingerprint(m.Leaf), by)
	} else {
		fmt.Println("identity: the shared fleet certificate from before 0.3.23 (restart the daemon to migrate)")
	}
	if cfg.LegacyUntil > time.Now().UnixMilli() {
		fmt.Printf("migration: peers may still present the shared certificate until %s; fleet.key is deleted once every member has its own\n", time.UnixMilli(cfg.LegacyUntil).Format(time.DateTime))
	}
	if r := cfg.Reissue; r != nil && r.Until > time.Now().UnixMilli() {
		fmt.Printf("re-issue: certificates re-issued %s; the previous root is accepted until %s\n", time.UnixMilli(r.At).Format(time.DateTime), time.UnixMilli(r.Until).Format(time.DateTime))
	}
	if len(cfg.RevokedKeys) > 0 {
		fmt.Printf("revoked keys: %d\n", len(cfg.RevokedKeys))
	}
	if len(peers) > 0 {
		fmt.Println()
		sort.Slice(peers, func(i, j int) bool { return peers[i].MachineID < peers[j].MachineID })
		for _, p := range peers {
			state := "disconnected"
			if p.Connected {
				state = "connected"
				if strings.HasPrefix(p.Via, "relay:") {
					state += " via " + via(p.Via)
				}
			}
			if p.LastError != "" {
				state += " (" + p.LastError + ")"
			}
			fmt.Printf("peer %-28s %-24s %s\n", p.MachineID, p.Addr, state)
		}
	}
	return nil
}

func shortModel(m string) string {
	m = strings.TrimPrefix(m, "claude-")
	if i := strings.Index(m, "["); i > 0 {
		m = m[:i]
	}
	return m
}

func dialLocalViewer(cfg *config.Config, name string) (*mesh.Conn, error) {
	_, cli, err := mesh.FleetTLS()
	if err != nil {
		return nil, err
	}
	_, port, _ := net.SplitHostPort(cfg.Listen)
	raw, err := tls.DialWithDialer(&net.Dialer{Timeout: 3 * time.Second}, "tcp", "127.0.0.1:"+port, cli)
	if err != nil {
		return nil, fmt.Errorf("connect to local daemon: %w (is it running? try `vineyardd install`)", err)
	}
	conn := mesh.NewConn(raw)
	if err := conn.Send(protocol.Hello{T: "hello", Role: "viewer", MachineID: cfg.MachineID + "#" + name, Name: name, Version: Version, Protocol: protocol.Version}); err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}

func cmdInvite() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	conn, err := dialLocalViewer(cfg, "cli")
	if err != nil {
		return err
	}
	defer conn.Close()
	if err := conn.Send(protocol.Request{T: "req", ID: "inv", Op: "invite"}); err != nil {
		return err
	}
	for {
		b, err := conn.Recv(5 * time.Second)
		if err != nil {
			return err
		}
		var res protocol.Response
		if json.Unmarshal(b, &res) != nil || res.T != "res" {
			continue
		}
		if !res.OK {
			return fmt.Errorf("%s", res.Error)
		}
		var d struct {
			Code string `json:"code"`
		}
		_ = json.Unmarshal(res.Data, &d)
		fmt.Println(d.Code)
		return nil
	}
}

func cmdRotateKey(args []string) error {
	fs := flag.NewFlagSet("rotate-key", flag.ContinueOnError)
	var exclude peerFlags
	fs.Var(&exclude, "exclude", "machine id to remove from the fleet and leave out of the new key (repeatable)")
	grace := fs.Duration("grace", mesh.DefaultRotationGrace, "how long machines still on the old key may reconnect and catch up")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	conn, err := dialLocalViewer(cfg, "cli")
	if err != nil {
		return err
	}
	defer conn.Close()
	a, _ := json.Marshal(protocol.RotateArgs{Exclude: exclude, GraceHours: int(grace.Hours())})
	if err := conn.Send(protocol.Request{T: "req", ID: "rot", Op: "rotatekey", Args: a}); err != nil {
		return err
	}
	for {
		b, err := conn.Recv(30 * time.Second)
		if err != nil {
			return err
		}
		var res protocol.Response
		if json.Unmarshal(b, &res) != nil || res.T != "res" || res.ID != "rot" {
			continue
		}
		if !res.OK {
			return fmt.Errorf("%s", res.Error)
		}
		fmt.Printf("fleet key rotated; machines on the old key can catch up until %s\n", time.Now().Add(*grace).Format(time.DateTime))
		return nil
	}
}

func cmdJoin(args []string) error {
	fs := flag.NewFlagSet("join", flag.ContinueOnError)
	name := fs.String("name", "", "display name (default: short hostname)")
	port := fs.Int("port", config.DefaultPort, "listen port")
	machineID := fs.String("machine-id", "", "stable machine id (default: lowercase hostname)")
	advertise := fs.String("advertise", "", "host:port peers should dial (default: <machine-id>:<port>)")
	force := fs.Bool("force", false, "overwrite an existing config and certificate")
	// Accept the code before or after the flags: Go's flag package stops at the first positional.
	var code string
	var flagArgs []string
	for _, a := range args {
		if code == "" && !strings.HasPrefix(a, "-") {
			code = a
			continue
		}
		flagArgs = append(flagArgs, a)
	}
	if err := fs.Parse(flagArgs); err != nil {
		return err
	}
	if code == "" {
		return fmt.Errorf("usage: vineyardd join CODE")
	}
	if config.HasFleetCert() && !*force {
		return fmt.Errorf("this machine already has a fleet certificate in %s (use --force to replace it)", config.Dir())
	}
	inv, err := mesh.DecodeInvite(code)
	if err != nil {
		return err
	}
	c := config.New(*machineID, *name, *port, *advertise)
	// This machine's own key: only its public half travels, and the inviter certifies it.
	key, keyPEM, err := config.NewMachineKey()
	if err != nil {
		return err
	}
	pubPEM, err := config.PublicKeyPEM(&key.PublicKey)
	if err != nil {
		return err
	}
	joined, via, err := mesh.JoinFleet(inv, c.MachineID, c.Name, c.Advertise, string(pubPEM))
	if err != nil {
		return fmt.Errorf("join via %s failed: %w", inv.Name, err)
	}
	if joined.Chain == "" {
		return fmt.Errorf("%s runs a vineyardd older than this one, which would hand out the shared fleet key; update it first", inv.Name)
	}
	if err := os.MkdirAll(config.Dir(), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(config.Path(config.CertFile), []byte(joined.Cert), 0o600); err != nil {
		return err
	}
	if err := config.WriteMachine("", keyPEM, []byte(joined.Chain)); err != nil {
		return err
	}
	_ = config.RetireFleetKey("") // a --force rejoin must not keep an old shared key around
	c.Added = joined.Added
	for _, p := range joined.Peers {
		c.AddPeer(p)
	}
	if err := c.Save(); err != nil {
		return err
	}
	fmt.Printf("joined fleet via %s (%s); %d peers known; wrote %s\n", inv.Name, via, len(c.Peers), config.Path(config.ConfigFile))
	return nil
}

// cmdWeb serves the mobile web app from the foreground, for development or a machine whose daemon
// does not run it; it prints a pairing code to pair the first device with.
func cmdWeb(args []string) error {
	fs := flag.NewFlagSet("web", flag.ContinueOnError)
	listen := fs.String("listen", ":7735", "address to serve the web app on")
	dev := fs.String("dev", "", "serve the app from this directory instead of the built-in copy")
	var hosts peerFlags
	fs.Var(&hosts, "allow-host", "extra host name the app is reached by (repeatable)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	o := web.RunnerOptions{
		Dial:        func() (*mesh.Conn, error) { return dialLocalViewer(cfg, "web") },
		Version:     Version,
		Self:        cfg.MachineID,
		Name:        cfg.Name,
		LogFile:     service.LogFile(),
		Restart:     service.Restart,
		DevicesFile: config.Path("web-devices.json"),
		Logf:        log.Printf,
	}
	if *dev != "" {
		o.Static = os.DirFS(*dev)
	}
	r := web.NewRunner(o)
	if err := r.Set(*listen, hosts); err != nil {
		return err
	}
	var pair struct {
		Code  string   `json:"code"`
		Links []string `json:"links"`
	}
	if raw, err := r.Pair(); err == nil {
		_ = json.Unmarshal(raw, &pair)
	}
	fmt.Printf("Vineyard web app for %s. Pair a device with code %s-%s (single use, 10 minutes):\n", cfg.Name, pair.Code[:4], pair.Code[4:])
	for _, l := range pair.Links {
		fmt.Println("  " + l)
	}
	fmt.Println("Plain HTTP: anyone on this network can read the traffic. Securing this route is up to you.")
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	<-ctx.Done()
	return r.Set("", nil)
}

// cmdKeygen makes this machine's key if it has none and prints the public key, for Add Machine over
// SSH: the setting-up machine certifies it and copies back only certificates.
func cmdKeygen() error {
	keyPath := config.Path(config.MachineKeyFile)
	var pub any
	if b, err := os.ReadFile(keyPath); err == nil {
		k, err := parseECKey(b)
		if err != nil {
			return fmt.Errorf("%s: %w", keyPath, err)
		}
		pub = &k.PublicKey
	} else {
		key, keyPEM, err := config.NewMachineKey()
		if err != nil {
			return err
		}
		if err := os.MkdirAll(config.Dir(), 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
			return err
		}
		pub = &key.PublicKey
	}
	out, err := config.PublicKeyPEM(pub)
	if err != nil {
		return err
	}
	fmt.Print(string(out))
	return nil
}

func parseECKey(b []byte) (*ecdsa.PrivateKey, error) {
	blk, _ := pem.Decode(b)
	if blk == nil {
		return nil, errors.New("not PEM")
	}
	return x509.ParseECPrivateKey(blk.Bytes)
}

func codexDirOf(c *codex.Collector) string {
	if c == nil {
		return ""
	}
	return c.CodexDir
}

// addCodex folds this machine's Codex threads into a snapshot beside the Claude Code sessions:
// live threads become agents, every rollout counts towards its workspace, and a managed thread's
// control state (pending approvals, usage) is merged in. Usage windows from both providers share
// the map, so the fullest limit of either account shows.
func addCodex(snap *model.Snapshot, machineID string, c *codex.Collector, mgr *codex.Manager) {
	if c == nil {
		return
	}
	r := c.Collect()
	snap.HasCodex = r.HasCodex
	if !r.HasCodex && mgr == nil {
		return
	}
	agents, workspaces := codex.Interpret(machineID, r, time.Now().UnixMilli())
	if mgr != nil {
		agents = mgr.Merge(machineID, agents)
		workspaces = claude.Regroup(machineID, agents, workspaces)
		if u := mgr.LatestUsage(); u != nil {
			if snap.Usage == nil {
				snap.Usage = u
			} else {
				merged := *snap.Usage
				merged.Windows = map[string]model.UsageWindow{}
				for k, w := range snap.Usage.Windows {
					merged.Windows[k] = w
				}
				for k, w := range u.Windows {
					merged.Windows[k] = w
				}
				if u.At > merged.At {
					merged.At = u.At
				}
				if u.Status == "rejected" {
					merged.Status = u.Status
				}
				snap.Usage = &merged
			}
		}
	}
	if len(agents) == 0 && len(workspaces) == 0 {
		return
	}
	snap.Agents = append(snap.Agents, agents...)
	sort.Slice(snap.Agents, func(i, j int) bool { return snap.Agents[i].ID < snap.Agents[j].ID })
	snap.Workspaces = codex.MergeWorkspaces(snap.Workspaces, workspaces)
}
