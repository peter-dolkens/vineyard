// Package service installs vineyardd as a per-user background service: launchd on macOS, a systemd
// user unit on Linux, and a logon scheduled task on Windows.
package service

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"time"
	"unicode/utf16"

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
		// A running exe cannot be replaced. Only kill by image name when we are not that image
		// ourselves (an in-place `vineyardd.exe install` would otherwise terminate here).
		_ = run("schtasks", "/End", "/TN", "Vineyard")
		if self, err := os.Executable(); err == nil {
			if same, _ := sameFile(self, InstalledBinary()); !same {
				_ = run("taskkill", "/IM", binName+".exe", "/F")
			}
		}
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
		uninstallWindows()
		return "scheduled task / Run entry removed", nil
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
		return restartWindows()
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
//
// A logon task is the Windows equivalent of a launchd agent, but `schtasks /Create /SC ONLOGON` is
// refused for non-elevated users ("Access is denied"). Creating the task from XML with a logon trigger
// scoped to the current user works without elevation, so that is tried first; the plain command is
// kept for elevated shells, and the per-user Run registry key is the last resort.

const runKey = `HKCU\Software\Microsoft\Windows\CurrentVersion\Run`

func installSchtasks(bin string) (string, error) {
	_ = run("schtasks", "/End", "/TN", "Vineyard")
	_ = run("schtasks", "/Delete", "/TN", "Vineyard", "/F")
	_ = run("reg", "delete", runKey, "/v", "Vineyard", "/f")
	tr := fmt.Sprintf(`"%s" run`, bin)

	var errs []string
	if xmlPath, err := writeTaskXML(bin); err == nil {
		defer os.Remove(xmlPath)
		if err := run("schtasks", "/Create", "/F", "/TN", "Vineyard", "/XML", xmlPath); err == nil {
			if err := run("schtasks", "/Run", "/TN", "Vineyard"); err != nil {
				return "", err
			}
			return "scheduled task 'Vineyard' installed and started (logs: " + LogFile() + ")", nil
		} else {
			errs = append(errs, err.Error())
		}
	} else {
		errs = append(errs, err.Error())
	}
	if err := run("schtasks", "/Create", "/F", "/SC", "ONLOGON", "/RL", "LIMITED", "/TN", "Vineyard", "/TR", tr); err == nil {
		if err := run("schtasks", "/Run", "/TN", "Vineyard"); err != nil {
			return "", err
		}
		return "scheduled task 'Vineyard' installed and started (logs: " + LogFile() + ")", nil
	} else {
		errs = append(errs, err.Error())
	}
	// No task scheduler access at all: start at logon from the Run key and launch it now.
	if err := run("reg", "add", runKey, "/v", "Vineyard", "/t", "REG_SZ", "/d", tr, "/f"); err != nil {
		errs = append(errs, err.Error())
		return "", fmt.Errorf("could not register a logon task or Run key:\n  %s", strings.Join(errs, "\n  "))
	}
	if err := startHidden(bin); err != nil {
		return "", err
	}
	return "registered in HKCU Run (task scheduler refused: " + errs[0] + ") and started (logs: " + LogFile() + ")", nil
}

// startHidden launches the daemon detached, without a console window.
func startHidden(bin string) error {
	return run("powershell", "-NoProfile", "-Command", fmt.Sprintf(`Start-Process -WindowStyle Hidden -FilePath '%s' -ArgumentList 'run'`, strings.ReplaceAll(bin, "'", "''")))
}

func hasTask() bool { return run("schtasks", "/Query", "/TN", "Vineyard") == nil }

func restartWindows() error {
	if hasTask() {
		_ = run("schtasks", "/End", "/TN", "Vineyard")
		return run("schtasks", "/Run", "/TN", "Vineyard")
	}
	_ = run("taskkill", "/IM", binName+".exe", "/F")
	return startHidden(InstalledBinary())
}

func uninstallWindows() {
	_ = run("schtasks", "/End", "/TN", "Vineyard")
	_ = run("schtasks", "/Delete", "/TN", "Vineyard", "/F")
	_ = run("reg", "delete", runKey, "/v", "Vineyard", "/f")
	_ = run("taskkill", "/IM", binName+".exe", "/F")
}

// writeTaskXML writes a Task Scheduler definition (UTF-16LE, as schtasks expects) for a logon task
// that runs as the current user with least privilege, never times out, and restarts if it dies.
func writeTaskXML(bin string) (string, error) {
	u, err := user.Current()
	if err != nil {
		return "", err
	}
	xml := taskXML(bin, u.Username)
	path := filepath.Join(config.Dir(), "vineyard-task.xml")
	if err := os.WriteFile(path, xml, 0o600); err != nil {
		return "", err
	}
	return path, nil
}

func taskXML(bin, username string) []byte {
	esc := func(s string) string {
		return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;").Replace(s)
	}
	body := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-16"?>
<Task version="1.2" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">
  <RegistrationInfo><Description>Vineyard agent daemon</Description></RegistrationInfo>
  <Triggers>
    <LogonTrigger><Enabled>true</Enabled><UserId>%s</UserId></LogonTrigger>
  </Triggers>
  <Principals>
    <Principal id="Author"><UserId>%s</UserId><LogonType>InteractiveToken</LogonType><RunLevel>LeastPrivilege</RunLevel></Principal>
  </Principals>
  <Settings>
    <MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>
    <DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>
    <StopIfGoingOnBatteries>false</StopIfGoingOnBatteries>
    <AllowHardTerminate>true</AllowHardTerminate>
    <StartWhenAvailable>true</StartWhenAvailable>
    <RunOnlyIfNetworkAvailable>false</RunOnlyIfNetworkAvailable>
    <AllowStartOnDemand>true</AllowStartOnDemand>
    <Enabled>true</Enabled>
    <Hidden>true</Hidden>
    <ExecutionTimeLimit>PT0S</ExecutionTimeLimit>
    <Priority>7</Priority>
    <RestartOnFailure><Interval>PT1M</Interval><Count>10</Count></RestartOnFailure>
  </Settings>
  <Actions Context="Author">
    <Exec><Command>%s</Command><Arguments>run</Arguments></Exec>
  </Actions>
</Task>
`, esc(username), esc(username), esc(bin))
	// UTF-16LE with BOM.
	runes := utf16.Encode([]rune(body))
	out := make([]byte, 0, 2*len(runes)+2)
	out = append(out, 0xFF, 0xFE)
	for _, r := range runes {
		out = append(out, byte(r), byte(r>>8))
	}
	return out
}
