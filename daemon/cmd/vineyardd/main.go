// vineyardd – the Vineyard agent daemon. One static binary per machine; see README for the design.
package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/peter-dolkens/vineyard/daemon/internal/claude"
	"github.com/peter-dolkens/vineyard/daemon/internal/config"
	"github.com/peter-dolkens/vineyard/daemon/internal/mesh"
	"github.com/peter-dolkens/vineyard/daemon/internal/model"
	"github.com/peter-dolkens/vineyard/daemon/internal/protocol"
	"github.com/peter-dolkens/vineyard/daemon/internal/service"
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
	case "join":
		err = cmdJoin(os.Args[2:])
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
		c.AddPeer(protocol.PeerAddr{MachineID: id, Addr: addr})
	}
	if err := c.Save(); err != nil {
		return err
	}
	if !config.HasFleetCert() {
		if err := config.GenerateFleetCert(); err != nil {
			return err
		}
		fmt.Println("generated fleet certificate", config.Path(config.CertFile))
	}
	fmt.Printf("wrote %s (machine %s, listen %s)\n", config.Path(config.ConfigFile), c.MachineID, c.Listen)
	return nil
}

func cmdRun() error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config (run `vineyardd init` first): %w", err)
	}
	logger := log.New(os.Stderr, "", log.LstdFlags)
	collector := claude.NewCollector(cfg.ClaudeDir, cfg.TailLines)
	node, err := mesh.New(mesh.Options{
		Config:    cfg,
		Version:   Version,
		Log:       logger,
		ClaudeDir: collector.ClaudeDir,
		Collect: func() model.Snapshot {
			r := collector.Collect()
			agents, workspaces := claude.Interpret(cfg.MachineID, r, time.Now().UnixMilli())
			return model.Snapshot{Host: r.Host, Agents: agents, Workspaces: workspaces, HasClaude: r.HasClaude}
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
		cfg.AddPeer(protocol.PeerAddr{MachineID: args[1], Addr: args[2]})
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
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", e.Snapshot.Name, status, e.Via, title, a.State, shortModel(a.Model), a.WorkspacePath, a.StateDetail)
		}
		if live == 0 {
			fmt.Fprintf(w, "%s\t%s\t%s\t-\t-\t-\t-\t%s\n", e.Snapshot.Name, status, e.Via, "no live agents")
		}
	}
	w.Flush()
	if len(peers) > 0 {
		fmt.Println()
		for _, p := range peers {
			state := "disconnected"
			if p.Connected {
				state = "connected"
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
	joined, via, err := mesh.JoinFleet(inv, c.MachineID, c.Name, c.Advertise)
	if err != nil {
		return fmt.Errorf("join via %s failed: %w", inv.Name, err)
	}
	if err := os.MkdirAll(config.Dir(), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(config.Path(config.CertFile), []byte(joined.Cert), 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(config.Path(config.KeyFile), []byte(joined.Key), 0o600); err != nil {
		return err
	}
	for _, p := range joined.Peers {
		c.AddPeer(p)
	}
	if err := c.Save(); err != nil {
		return err
	}
	fmt.Printf("joined fleet via %s (%s); %d peers known; wrote %s\n", inv.Name, via, len(c.Peers), config.Path(config.ConfigFile))
	return nil
}
