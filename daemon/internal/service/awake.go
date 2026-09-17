package service

import (
	"os/exec"
	"runtime"
	"sync"
)

// Awake holds off system idle sleep while something is watching this machine. Without it a Mac that
// dozes (idle sleep + Wake on Demand) only surfaces for ~45 s at a time, so its daemon link flaps and
// its agents stall. macOS only: `caffeinate -s -i` is a child that lives exactly as long as the hold.
type Awake struct {
	mu  sync.Mutex
	cmd *exec.Cmd
	// Disabled turns the feature off (config keepAwakeWhileWatched=false).
	Disabled bool
}

// Set starts or stops the hold. Idempotent; a no-op off macOS.
func (a *Awake) Set(hold bool) {
	if runtime.GOOS != "darwin" || a.Disabled {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if hold {
		if a.cmd != nil {
			return
		}
		cmd := exec.Command("caffeinate", "-s", "-i")
		if err := cmd.Start(); err != nil {
			return
		}
		a.cmd = cmd
		go func() { _ = cmd.Wait() }()
		return
	}
	if a.cmd != nil {
		_ = a.cmd.Process.Kill()
		a.cmd = nil
	}
}

// Held reports whether a hold is active.
func (a *Awake) Held() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cmd != nil
}
