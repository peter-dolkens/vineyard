package accounts

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"runtime"
	"time"

	"github.com/peter-dolkens/vineyard/daemon/internal/config"
)

// Where Claude Code keeps a login, as of Claude Code 2.1.280:
//
//   - The tokens: the macOS keychain item "Claude Code-credentials" (account $USER), falling back to
//     <config dir>/.credentials.json, which is the only store on Linux and Windows. The item holds a
//     JSON object: claudeAiOauth is the subscription login; other keys (mcpOAuth, ...) are not.
//   - The profile: the oauthAccount block of the global config, ~/.claude.json.
//
// With CLAUDE_CONFIG_DIR set, the config dir moves, the global config moves into it, and the keychain
// item gets a "-<sha256(dir)[:8]>" suffix. Claude Code guards each file with a proper-lockfile lock (a
// directory named <file>.lock): <config dir>/.storage-write.lock for the tokens, ~/.claude.json.lock
// for the config. Vineyard takes the same locks, so a Claude refreshing its token cannot interleave.

// accountKeys are the token-store keys `claude auth login` replaces for the incoming account; all
// other keys (MCP server logins and the like) belong to the machine and are left alone on a switch.
var accountKeys = []string{"claudeAiOauth", "organizationUuid", "trustedDeviceToken", "enterpriseGateway", "designOauth"}

const lockStale = 15 * time.Second // older than proper-lockfile's 10 s default: the holder has died

var lockWait = 5 * time.Second

// claudePaths locates this user's Claude Code login.
type claudePaths struct {
	ConfigDir string // ~/.claude or $CLAUDE_CONFIG_DIR
	Global    string // ~/.claude.json
	SecureDir string // where .credentials.json and .storage-write live
	Service   string // keychain service name (macOS)
	Account   string // keychain account name (macOS)
}

func findClaude() claudePaths {
	home, _ := os.UserHomeDir()
	env := os.Getenv("CLAUDE_CONFIG_DIR")
	dir := env
	if dir == "" {
		dir = filepath.Join(home, ".claude")
	}
	global := filepath.Join(dir, ".config.json") // legacy location, used while it exists
	if _, err := os.Stat(global); err != nil {
		base := env
		if base == "" {
			base = home
		}
		global = filepath.Join(base, ".claude.json")
	}
	secure, suffixed := dir, env != ""
	if s, ok := os.LookupEnv("CLAUDE_SECURESTORAGE_CONFIG_DIR"); ok {
		secure, suffixed = s, s != ""
		if s == "" {
			secure = filepath.Join(home, ".claude")
		}
	}
	service := "Claude Code-credentials"
	if suffixed {
		sum := sha256.Sum256([]byte(secure))
		service += "-" + hex.EncodeToString(sum[:])[:8]
	}
	return claudePaths{ConfigDir: dir, Global: global, SecureDir: secure, Service: service, Account: keychainUser()}
}

var userRe = regexp.MustCompile(`^[a-zA-Z0-9._-]+$`)

// keychainUser mirrors Claude Code: $USER, else the OS user name, else a fixed fallback.
func keychainUser() string {
	n := os.Getenv("USER")
	if n == "" {
		if u, err := user.Current(); err == nil {
			n = u.Username
		}
	}
	if !userRe.MatchString(n) {
		return "claude-code-user"
	}
	return n
}

// tokenStore is where Claude Code reads and writes its tokens.
type tokenStore interface {
	// Read returns the stored object, or nil when there is none.
	Read() (map[string]json.RawMessage, error)
	Write(map[string]json.RawMessage) error
}

// fileTokens is <dir>/.credentials.json, mode 0600.
type fileTokens struct{ path string }

func (f fileTokens) Read() (map[string]json.RawMessage, error) {
	b, err := os.ReadFile(f.path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("%s: %w", f.path, err)
	}
	return m, nil
}

func (f fileTokens) Write(m map[string]json.RawMessage) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return writeReplacing(f.path, b)
}

// keychainTokens is the keychain item, with .credentials.json as the fallback Claude Code also reads.
type keychainTokens struct {
	kc   keychain
	file fileTokens
}

func (k keychainTokens) Read() (map[string]json.RawMessage, error) {
	b, err := k.kc.Get()
	if err != nil {
		return nil, err
	}
	if b == nil {
		return k.file.Read()
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("keychain item %q: %w", k.kc.service, err)
	}
	return m, nil
}

// Write goes where the login lives now: the plaintext file when only that exists, else the keychain.
func (k keychainTokens) Write(m map[string]json.RawMessage) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	cur, err := k.kc.Get()
	if err != nil {
		return err
	}
	if cur == nil {
		if _, err := os.Stat(k.file.path); err == nil {
			return k.file.Write(m)
		}
	}
	return k.kc.Put(b)
}

// liveTokens picks Claude Code's store for this platform.
func liveTokens(p claudePaths) (tokenStore, error) {
	file := fileTokens{path: filepath.Join(p.SecureDir, ".credentials.json")}
	switch runtime.GOOS {
	case "darwin":
		return keychainTokens{kc: keychain{service: p.Service, account: p.Account}, file: file}, nil
	case "windows":
		if windowsCredman(p) {
			return nil, errors.New("Claude Code on this machine keeps its login in Windows Credential Manager, which Vineyard cannot switch yet")
		}
	}
	return file, nil
}

// windowsCredman mirrors Claude Code's choice of Windows Credential Manager over the plaintext file.
func windowsCredman(p claudePaths) bool {
	if os.Getenv("CLAUDE_CODE_FORCE_WINDOWS_CREDMAN") == "1" {
		return true
	}
	b, err := os.ReadFile(p.Global)
	if err != nil {
		return false
	}
	var v struct {
		Features map[string]json.RawMessage `json:"cachedGrowthBookFeatures"`
	}
	_ = json.Unmarshal(b, &v)
	return string(v.Features["tengu_windows_credman"]) == "true"
}

// withLock holds the proper-lockfile lock on target (the directory target+".lock") while fn runs.
func withLock(target string, fn func() error) error {
	lock := target + ".lock"
	deadline := time.Now().Add(lockWait)
	for {
		err := os.Mkdir(lock, 0o755)
		if err == nil {
			break
		}
		if !errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("lock %s: %w", lock, err)
		}
		if st, serr := os.Stat(lock); serr == nil && time.Since(st.ModTime()) > lockStale {
			_ = os.Remove(lock) // its holder died without cleaning up
			continue
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("Claude Code is busy writing %s; try again in a moment", filepath.Base(target))
		}
		time.Sleep(50 * time.Millisecond)
	}
	defer os.Remove(lock)
	return fn()
}

// writeReplacing atomically replaces path (through a symlink, if it is one) with data, mode 0600.
func writeReplacing(path string, data []byte) error {
	if real, err := filepath.EvalSymlinks(path); err == nil {
		path = real
	}
	return config.WriteFileAtomic(path, data)
}

// readOAuthAccount returns the global config's oauthAccount block, or nil if it has none.
func readOAuthAccount(global string) (json.RawMessage, error) {
	b, err := os.ReadFile(global)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var v struct {
		OAuthAccount json.RawMessage `json:"oauthAccount"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return nil, fmt.Errorf("%s: %w", global, err)
	}
	if len(v.OAuthAccount) == 0 || string(v.OAuthAccount) == "null" {
		return nil, nil
	}
	return v.OAuthAccount, nil
}

// writeOAuthAccount replaces the global config's oauthAccount in place, leaving every other byte of
// the file (key order, formatting) as Claude Code wrote it.
func writeOAuthAccount(global string, val json.RawMessage) error {
	b, err := os.ReadFile(global)
	if err != nil {
		return err
	}
	var ind bytes.Buffer
	if err := json.Indent(&ind, val, "  ", "  "); err != nil {
		return err
	}
	out, err := spliceTopLevel(b, "oauthAccount", ind.Bytes())
	if err != nil {
		return fmt.Errorf("%s: %w", global, err)
	}
	return writeReplacing(global, out)
}

// spliceTopLevel sets key to val (raw JSON) in the top-level object raw, replacing the old value's
// bytes or appending the key when it is missing.
func spliceTopLevel(raw []byte, key string, val []byte) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return nil, errors.New("not a JSON object")
	}
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return nil, err
		}
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, err
		}
		if t == key {
			end := int(dec.InputOffset())
			start := end - len(v)
			return append(append(append([]byte{}, raw[:start]...), val...), raw[end:]...), nil
		}
	}
	if _, err := dec.Token(); err != nil { // the closing brace
		return nil, err
	}
	closeAt := int(dec.InputOffset()) - 1
	body := bytes.TrimRight(raw[:closeAt], " \t\r\n")
	sep := ","
	if bytes.HasSuffix(body, []byte("{")) {
		sep = ""
	}
	name, _ := json.Marshal(key)
	out := append([]byte{}, body...)
	out = append(out, sep+"\n  "...)
	out = append(out, name...)
	out = append(out, ": "...)
	out = append(out, val...)
	out = append(out, "\n"...)
	return append(out, raw[closeAt:]...), nil
}
