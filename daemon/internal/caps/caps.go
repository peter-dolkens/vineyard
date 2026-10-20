// Package caps finds which programs a machine has, for the fleet listing an agent sees: the ones in
// Known go in every snapshot, and any other name can be asked about (the "hascap" op).
//
// A name is only ever looked up on PATH, never run, so asking about one cannot execute anything.
// The PATH is the user's login shell's: launchd, systemd and Task Scheduler give the daemon a minimal
// one, which would miss Homebrew's gh and docker.
package caps

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

// Known are the programs every snapshot reports on. "iis" is not a program: on Windows it means IIS
// is installed (inetsrv holds w3wp.exe).
var Known = []string{
	"git", "gh", "docker", "podman", "kubectl", "helm", "terraform",
	"node", "npm", "bun", "deno", "go", "python3", "uv", "dotnet", "java", "cargo", "ruby", "php",
	"make", "cmake", "az", "aws", "gcloud", "brew", "apt-get", "winget", "choco",
	"zsh", "bash", "fish", "pwsh", "powershell", "ssh", "tmux", "claude", "codex", "code", "iis",
}

var nameRe = regexp.MustCompile(`^[A-Za-z0-9._+-]{1,64}$`)

// Valid reports whether name may be looked up: a plain file name, no path, no shell syntax.
func Valid(name string) bool { return nameRe.MatchString(name) && name != "." && name != ".." }

// pathTTL is how long a login shell's PATH is reused before asking the shell again.
const pathTTL = 10 * time.Minute

var (
	mu       sync.Mutex
	pathVal  string
	pathAt   time.Time
	knownVal []string
	knownAt  time.Time
)

// loginPath is PATH as the user's login shell sets it, falling back to the daemon's own.
func loginPath() string {
	mu.Lock()
	defer mu.Unlock()
	if pathVal != "" && time.Since(pathAt) < pathTTL {
		return pathVal
	}
	p := os.Getenv("PATH")
	if runtime.GOOS != "windows" {
		sh := os.Getenv("SHELL")
		if sh == "" {
			sh = "/bin/sh"
			if runtime.GOOS == "darwin" {
				sh = "/bin/zsh"
			}
		}
		// printenv works the same in every shell (fish included), and nothing of ours goes into the
		// command line.
		if out, err := exec.Command(sh, "-lc", "printenv PATH").Output(); err == nil {
			if s := strings.TrimSpace(lastLine(string(out))); s != "" {
				p = s
			}
		}
	}
	pathVal, pathAt = p, time.Now()
	return p
}

func lastLine(s string) string {
	s = strings.TrimRight(s, "\r\n")
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		return s[i+1:]
	}
	return s
}

// Lookup returns where each of names is found, leaving out the ones that are not (and any name that
// is not Valid).
func Lookup(names []string) map[string]string {
	path := loginPath()
	out := map[string]string{}
	for _, n := range names {
		if !Valid(n) {
			continue
		}
		if p := find(n, path); p != "" {
			out[n] = p
		}
	}
	return out
}

// KnownTools is the sorted list of Known programs this machine has, recomputed every pathTTL.
func KnownTools() []string {
	mu.Lock()
	if knownVal != nil && time.Since(knownAt) < pathTTL {
		v := knownVal
		mu.Unlock()
		return v
	}
	mu.Unlock()
	found := Lookup(Known)
	list := make([]string, 0, len(found))
	for n := range found {
		list = append(list, n)
	}
	sort.Strings(list)
	mu.Lock()
	knownVal, knownAt = list, time.Now()
	mu.Unlock()
	return list
}

func find(name, path string) string {
	if name == "iis" {
		if runtime.GOOS != "windows" {
			return ""
		}
		w3wp := filepath.Join(os.Getenv("SystemRoot"), "System32", "inetsrv", "w3wp.exe")
		if fileExists(w3wp) {
			return w3wp
		}
		return ""
	}
	if runtime.GOOS == "windows" {
		exts := strings.Split(strings.ToLower(os.Getenv("PATHEXT")), ";")
		if len(exts) == 1 && exts[0] == "" {
			exts = []string{".com", ".exe", ".bat", ".cmd"}
		}
		for _, dir := range filepath.SplitList(path) {
			for _, ext := range append([]string{""}, exts...) {
				if ext == "" && filepath.Ext(name) == "" {
					continue // a bare name is not runnable on Windows
				}
				if p := filepath.Join(dir, name+ext); fileExists(p) {
					return p
				}
			}
		}
		return ""
	}
	for _, dir := range filepath.SplitList(path) {
		if dir == "" {
			continue
		}
		p := filepath.Join(dir, name)
		if st, err := os.Stat(p); err == nil && !st.IsDir() && st.Mode()&0o111 != 0 {
			return p
		}
	}
	return ""
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}
