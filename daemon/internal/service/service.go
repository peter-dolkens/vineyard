// Package service installs vineyardd as a per-user background service: launchd on macOS, a systemd
// user unit on Linux, and a logon scheduled task on Windows.
package service

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/peter-dolkens/vineyard/daemon/internal/config"
)

const (
	Label   = "net.dolkens.vineyardd"
	binName = "vineyardd"
)

func BinDir() string { return filepath.Join(config.Dir(), "bin") }

func InstalledBinary() string {
	name := binName
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	return filepath.Join(BinDir(), name)
}

func LogFile() string { return filepath.Join(config.Dir(), "vineyardd.log") }

// UpgradeLogFile collects the output of self-installs started by an upgrade.
func UpgradeLogFile() string { return filepath.Join(config.Dir(), "upgrade.log") }

const stagedPrefix = binName + ".staged-"

// StagedBinary is where an incoming upgrade is written before it installs itself.
func StagedBinary(tag string) string {
	name := stagedPrefix + tag
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	return filepath.Join(BinDir(), name)
}

// CleanStaged removes leftovers from earlier upgrades. Never touches the running executable (which,
// during the install step, is itself a staged file).
func CleanStaged() {
	self, _ := os.Executable()
	self, _ = filepath.EvalSymlinks(self)
	matches, _ := filepath.Glob(filepath.Join(BinDir(), stagedPrefix+"*"))
	for _, m := range matches {
		if same, _ := sameFile(m, self); same {
			continue
		}
		_ = os.Remove(m)
	}
}

// LaunchInstaller runs `<staged> install` detached from this process. The staged binary copies itself
// over the installed one and re-registers/restarts the service, which ends the calling daemon; it must
// therefore outlive us: its own session on Unix, a detached process on Windows, and its own transient
// unit under systemd (whose cgroup kill would otherwise take it down with the service).
func LaunchInstaller(staged string) error {
	logf, err := os.OpenFile(UpgradeLogFile(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer logf.Close()
	fmt.Fprintf(logf, "\n==== %s: installing %s\n", time.Now().Format(time.RFC3339), staged)
	var cmd *exec.Cmd
	if runtime.GOOS == "linux" && os.Getenv("INVOCATION_ID") != "" {
		if sr, err := exec.LookPath("systemd-run"); err == nil {
			cmd = exec.Command(sr, "--user", "--quiet", "--collect", "--setenv=VINEYARD_DIR="+config.Dir(),
				"-p", "StandardOutput=append:"+UpgradeLogFile(), "-p", "StandardError=append:"+UpgradeLogFile(), staged, "install")
		}
	}
	if cmd == nil {
		cmd = exec.Command(staged, "install")
		cmd.Stdout, cmd.Stderr = logf, logf
	}
	cmd.Dir = config.Dir()
	cmd.Env = append(os.Environ(), "VINEYARD_DIR="+config.Dir())
	detach(cmd)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start installer: %w", err)
	}
	return cmd.Process.Release()
}

// EnsureBinary copies the running executable into ~/.vineyard/bin unless it already runs from there.
func EnsureBinary() (string, error) {
	self, err := os.Executable()
	if err != nil {
		return "", err
	}
	self, _ = filepath.EvalSymlinks(self)
	dst := InstalledBinary()
	if same, _ := sameFile(self, dst); same {
		return dst, nil
	}
	if err := os.MkdirAll(BinDir(), 0o755); err != nil {
		return "", err
	}
	// Never reuse a fixed temp name: the bootstrap flows upload the binary next to the destination and
	// run it from there, and truncating our own executable copies zero bytes.
	tmp := fmt.Sprintf("%s.tmp-%d", dst, os.Getpid())
	if same, _ := sameFile(self, tmp); same {
		return "", fmt.Errorf("refusing to overwrite the running executable %s", self)
	}
	in, err := os.Open(self)
	if err != nil {
		return "", err
	}
	defer in.Close()
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return "", err
	}
	n, err := io.Copy(out, in)
	if err != nil {
		out.Close()
		os.Remove(tmp)
		return "", err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return "", err
	}
	if n < 1<<20 {
		os.Remove(tmp)
		return "", fmt.Errorf("copied only %d bytes of %s; refusing to install a truncated binary", n, self)
	}
	if runtime.GOOS == "windows" {
		_ = os.Remove(dst) // rename over a running exe fails; the task is stopped before install
	}
	if err := os.Rename(tmp, dst); err != nil {
		return "", err
	}
	return dst, nil
}

func sameFile(a, b string) (bool, error) {
	sa, err := os.Stat(a)
	if err != nil {
		return false, err
	}
	sb, err := os.Stat(b)
	if err != nil {
		return false, err
	}
	return os.SameFile(sa, sb), nil
}

func Install() (string, error) {
	if runtime.GOOS == "windows" {
		_ = run("schtasks", "/End", "/TN", "Vineyard") // a running exe cannot be replaced
	}
	bin, err := EnsureBinary()
	if err != nil {
		return "", fmt.Errorf("install binary: %w", err)
	}
	switch runtime.GOOS {
	case "darwin":
		return installLaunchd(bin)
	case "linux":
		return installSystemd(bin)
	case "windows":
		return installSchtasks(bin)
	}
	return "", fmt.Errorf("unsupported OS %s", runtime.GOOS)
}

func Uninstall() (string, error) {
	switch runtime.GOOS {
	case "darwin":
		plist := launchdPlistPath()
		_ = run("launchctl", "bootout", fmt.Sprintf("gui/%d/%s", os.Getuid(), Label))
		_ = os.Remove(plist)
		return "launchd agent removed", nil
	case "linux":
		_ = run("systemctl", "--user", "disable", "--now", "vineyardd.service")
		_ = os.Remove(systemdUnitPath())
		_ = run("systemctl", "--user", "daemon-reload")
		return "systemd user unit removed", nil
	case "windows":
		_ = run("schtasks", "/End", "/TN", "Vineyard")
		_ = run("schtasks", "/Delete", "/TN", "Vineyard", "/F")
		return "scheduled task removed", nil
	}
	return "", fmt.Errorf("unsupported OS %s", runtime.GOOS)
}

// Restart bounces the service so a new binary or config takes effect.
func Restart() error {
	switch runtime.GOOS {
	case "darwin":
		return run("launchctl", "kickstart", "-k", fmt.Sprintf("gui/%d/%s", os.Getuid(), Label))
	case "linux":
		return run("systemctl", "--user", "restart", "vineyardd.service")
	case "windows":
		_ = run("schtasks", "/End", "/TN", "Vineyard")
		return run("schtasks", "/Run", "/TN", "Vineyard")
	}
	return fmt.Errorf("unsupported OS %s", runtime.GOOS)
}

func run(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// ---- macOS ---------------------------------------------------------------------------------------

func launchdPlistPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library", "LaunchAgents", Label+".plist")
}

func installLaunchd(bin string) (string, error) {
	plist := launchdPlistPath()
	content := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>%s</string>
  <key>ProgramArguments</key>
  <array><string>%s</string><string>run</string></array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>ProcessType</key><string>Background</string>
  <key>LowPriorityIO</key><true/>
  <key>StandardOutPath</key><string>%s</string>
  <key>StandardErrorPath</key><string>%s</string>
  <key>EnvironmentVariables</key>
  <dict><key>VINEYARD_DIR</key><string>%s</string></dict>
</dict>
</plist>
`, Label, bin, LogFile(), LogFile(), config.Dir())
	if err := os.MkdirAll(filepath.Dir(plist), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(plist, []byte(content), 0o644); err != nil {
		return "", err
	}
	target := fmt.Sprintf("gui/%d", os.Getuid())
	_ = run("launchctl", "bootout", target+"/"+Label) // ignore: may not be loaded
	if err := run("launchctl", "bootstrap", target, plist); err != nil {
		// Over SSH without a GUI session `bootstrap gui/…` can fail; the legacy verb usually still works.
		if err2 := run("launchctl", "load", "-w", plist); err2 != nil {
			return "", fmt.Errorf("%v (fallback: %v)", err, err2)
		}
	}
	_ = run("launchctl", "kickstart", "-k", target+"/"+Label)
	return "launchd agent " + Label + " installed and started", nil
}

// ---- Linux ---------------------------------------------------------------------------------------

func systemdUnitPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "systemd", "user", "vineyardd.service")
}

func installSystemd(bin string) (string, error) {
	unit := systemdUnitPath()
	content := fmt.Sprintf(`[Unit]
Description=Vineyard agent daemon
After=network-online.target

[Service]
ExecStart=%s run
Restart=always
RestartSec=3
Nice=10
Environment=VINEYARD_DIR=%s

[Install]
WantedBy=default.target
`, bin, config.Dir())
	if err := os.MkdirAll(filepath.Dir(unit), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(unit, []byte(content), 0o644); err != nil {
		return "", err
	}
	if err := run("systemctl", "--user", "daemon-reload"); err != nil {
		return "", err
	}
	if err := run("systemctl", "--user", "enable", "--now", "vineyardd.service"); err != nil {
		return "", err
	}
	_ = run("systemctl", "--user", "restart", "vineyardd.service")
	note := "systemd user unit installed and started"
	if out, err := exec.Command("loginctl", "show-user", os.Getenv("USER"), "-p", "Linger", "--value").Output(); err == nil && strings.TrimSpace(string(out)) != "yes" {
		note += "\nNOTE: run `loginctl enable-linger` so the daemon starts at boot without a login session."
	}
	return note, nil
}

// ---- Windows -------------------------------------------------------------------------------------

func installSchtasks(bin string) (string, error) {
	_ = run("schtasks", "/End", "/TN", "Vineyard")
	_ = run("schtasks", "/Delete", "/TN", "Vineyard", "/F")
	tr := fmt.Sprintf(`"%s" run`, bin)
	if err := run("schtasks", "/Create", "/F", "/SC", "ONLOGON", "/RL", "LIMITED", "/TN", "Vineyard", "/TR", tr); err != nil {
		return "", err
	}
	if err := run("schtasks", "/Run", "/TN", "Vineyard"); err != nil {
		return "", err
	}
	return "scheduled task 'Vineyard' installed and started (logs: " + LogFile() + ")", nil
}
