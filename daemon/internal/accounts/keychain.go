package accounts

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// keychain is one generic-password item in the login keychain, reached through /usr/bin/security the
// way Claude Code reaches its own (so no prompt: the tool that created the item is the one reading it).
// Secrets go in on stdin (`security -i`), never on a command line other processes can see.
type keychain struct{ service, account string }

const (
	securityBin     = "/usr/bin/security"
	securityTimeout = 5 * time.Second
	errItemNotFound = 44 // security's exit status for errSecItemNotFound
	stdinLineLimit  = 4032
)

func runSecurity(stdin string, args ...string) ([]byte, int, error) {
	cmd := exec.Command(securityBin, args...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Start(); err != nil {
		return nil, -1, err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		code := 0
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		} else if err != nil {
			return nil, -1, err
		}
		if code != 0 && code != errItemNotFound {
			return out.Bytes(), code, fmt.Errorf("security %s: exit %d: %s", args[0], code, strings.TrimSpace(errb.String()))
		}
		return out.Bytes(), code, nil
	case <-time.After(securityTimeout):
		_ = cmd.Process.Kill()
		return nil, -1, fmt.Errorf("security %s timed out (is the keychain locked?)", args[0])
	}
}

// Get returns the item's secret, or nil when there is no such item.
func (k keychain) Get() ([]byte, error) {
	out, code, err := runSecurity("", "find-generic-password", "-a", k.account, "-s", k.service, "-w")
	if err != nil {
		return nil, err
	}
	if code == errItemNotFound {
		return nil, nil
	}
	s := strings.TrimSpace(string(out))
	// security prints a secret that is not plain text as hex.
	if !strings.HasPrefix(s, "{") {
		if b, err := hex.DecodeString(s); err == nil {
			return b, nil
		}
	}
	return []byte(s), nil
}

// Put creates or replaces the item.
func (k keychain) Put(secret []byte) error {
	if strings.ContainsAny(k.account+k.service, "\"\\\n") {
		return fmt.Errorf("unsupported keychain name %q", k.service)
	}
	line := fmt.Sprintf("add-generic-password -U -a \"%s\" -s \"%s\" -X \"%s\"\n", k.account, k.service, hex.EncodeToString(secret))
	var err error
	if len(line) <= stdinLineLimit {
		_, _, err = runSecurity(line, "-i")
	} else {
		// Too long for security's interactive line buffer; Claude Code falls back the same way.
		_, _, err = runSecurity("", "add-generic-password", "-U", "-a", k.account, "-s", k.service, "-X", hex.EncodeToString(secret))
	}
	return err
}

// Delete removes the item; a missing item is not an error.
func (k keychain) Delete() error {
	_, _, err := runSecurity("", "delete-generic-password", "-a", k.account, "-s", k.service)
	return err
}
