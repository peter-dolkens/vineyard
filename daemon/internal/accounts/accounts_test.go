package accounts

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/peter-dolkens/vineyard/daemon/internal/model"
)

// testStore is a Store over a scratch Claude home that keeps its tokens in .credentials.json.
func testStore(t *testing.T) (*Store, string) {
	t.Helper()
	home := t.TempDir()
	claudeDir := filepath.Join(home, ".claude")
	if err := os.MkdirAll(claudeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := claudePaths{ConfigDir: claudeDir, Global: filepath.Join(home, ".claude.json"), SecureDir: claudeDir}
	s := &Store{file: filepath.Join(home, "vineyard", "claude-accounts.json"), paths: p, now: time.Now}
	_ = os.MkdirAll(filepath.Dir(s.file), 0o700)
	s.live = func() (tokenStore, error) {
		return fileTokens{path: filepath.Join(claudeDir, ".credentials.json")}, nil
	}
	return s, home
}

// signIn writes what `claude auth login` as account n leaves behind, keeping other keys of both files.
func signIn(t *testing.T, s *Store, n, refresh string) {
	t.Helper()
	credPath := filepath.Join(s.paths.SecureDir, ".credentials.json")
	creds := map[string]any{}
	if b, err := os.ReadFile(credPath); err == nil {
		_ = json.Unmarshal(b, &creds)
	}
	creds["claudeAiOauth"] = map[string]any{"accessToken": "at-" + refresh, "refreshToken": refresh, "expiresAt": 1, "refreshTokenExpiresAt": time.Now().Add(720 * time.Hour).UnixMilli(), "subscriptionType": "max"}
	b, _ := json.Marshal(creds)
	if err := os.WriteFile(credPath, b, 0o600); err != nil {
		t.Fatal(err)
	}
	oa, _ := json.Marshal(map[string]any{"accountUuid": "acct-" + n, "emailAddress": n + "@example.com", "organizationUuid": "org-" + n, "organizationName": "Org " + n})
	raw, err := os.ReadFile(s.paths.Global)
	if err != nil {
		raw = []byte("{\n  \"numStartups\": 3\n}\n")
	}
	out, err := spliceTopLevel(raw, "oauthAccount", oa)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.paths.Global, out, 0o600); err != nil {
		t.Fatal(err)
	}
}

func liveRefresh(t *testing.T, s *Store) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(s.paths.SecureDir, ".credentials.json"))
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		O oauthTokens `json:"claudeAiOauth"`
	}
	_ = json.Unmarshal(b, &v)
	return v.O.RefreshToken
}

func activeEmail(list []Account) string {
	for _, a := range list {
		if a.Active {
			return a.Email
		}
	}
	return ""
}

func TestSwitchKeepsRotatedTokensAndOtherKeys(t *testing.T) {
	s, _ := testStore(t)
	// Something besides the login lives in both files and must survive every switch.
	credPath := filepath.Join(s.paths.SecureDir, ".credentials.json")
	_ = os.WriteFile(credPath, []byte(`{"mcpOAuth":{"srv":{"accessToken":"mcp"}}}`), 0o600)

	signIn(t, s, "a", "ra1")
	if list, err := s.List(nil); err != nil || len(list) != 1 || activeEmail(list) != "a@example.com" {
		t.Fatalf("list after first sign-in: %+v %v", list, err)
	}
	signIn(t, s, "b", "rb1") // Add account: sign in as b on top of a
	list, err := s.List(nil)
	if err != nil || len(list) != 2 || activeEmail(list) != "b@example.com" {
		t.Fatalf("list after second sign-in: %+v %v", list, err)
	}
	keyA := list[0].Key

	list, err = s.Switch(keyA, nil)
	if err != nil || activeEmail(list) != "a@example.com" {
		t.Fatalf("switch to a: %+v %v", list, err)
	}
	if r := liveRefresh(t, s); r != "ra1" {
		t.Fatalf("live refresh token after switching to a = %q", r)
	}
	// Claude refreshes a's token while it is signed in; the switch away must keep the rotated one.
	signIn(t, s, "a", "ra2")
	if _, err := s.Switch(list[1].Key, nil); err != nil {
		t.Fatal(err)
	}
	if r := liveRefresh(t, s); r != "rb1" {
		t.Fatalf("live refresh token after switching to b = %q", r)
	}
	if _, err := s.Switch(keyA, nil); err != nil {
		t.Fatal(err)
	}
	if r := liveRefresh(t, s); r != "ra2" {
		t.Fatalf("switching back to a restored %q, want the rotated ra2", r)
	}

	b, _ := os.ReadFile(credPath)
	if !strings.Contains(string(b), `"mcpOAuth"`) {
		t.Fatalf("MCP logins lost: %s", b)
	}
	g, _ := os.ReadFile(s.paths.Global)
	if !strings.HasPrefix(string(g), "{\n  \"numStartups\": 3,") || !strings.Contains(string(g), `"emailAddress": "a@example.com"`) {
		t.Fatalf("global config: %s", g)
	}
	var cfg map[string]json.RawMessage
	if err := json.Unmarshal(g, &cfg); err != nil {
		t.Fatalf("global config no longer parses: %v\n%s", err, g)
	}
}

func TestRemoveAndExpiry(t *testing.T) {
	s, _ := testStore(t)
	signIn(t, s, "a", "ra1")
	_ = s.SaveCurrent(nil) // what Add does before signing in on top
	signIn(t, s, "b", "rb1")
	list, err := s.List(nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Remove(list[1].Key); err == nil {
		t.Fatal("removing the signed-in account should fail")
	}
	// Age a's sign-in past its refresh token's expiry.
	c, _ := s.load()
	c.find(list[0].Key).RefreshExpiresAt = time.Now().Add(-time.Hour).UnixMilli()
	_ = s.save(c)
	if _, err := s.Switch(list[0].Key, nil); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("switch to an expired account: %v", err)
	}
	list, err = s.Remove(list[0].Key)
	if err != nil || len(list) != 1 || !list[0].Active {
		t.Fatalf("after remove: %+v %v", list, err)
	}
}

func TestUsageCreditedToTheAccountSignedIn(t *testing.T) {
	s, _ := testStore(t)
	signIn(t, s, "a", "ra1")
	old := &model.Usage{Status: "allowed", At: time.Now().Add(-time.Minute).UnixMilli()}
	list, _ := s.List(old)
	if list[0].Usage != nil {
		t.Fatal("a report older than the sign-in was credited to it")
	}
	fresh := &model.Usage{Status: "allowed", At: time.Now().Add(time.Second).UnixMilli()}
	list, _ = s.List(fresh)
	if list[0].Usage == nil || list[0].Usage.At != fresh.At {
		t.Fatalf("usage not credited: %+v", list[0].Usage)
	}
}

func TestSwitchWaitsForClaudesLock(t *testing.T) {
	s, _ := testStore(t)
	signIn(t, s, "a", "ra1")
	_ = s.SaveCurrent(nil)
	signIn(t, s, "b", "rb1")
	list, _ := s.List(nil)
	lock := filepath.Join(s.paths.SecureDir, ".storage-write.lock")
	if err := os.Mkdir(lock, 0o755); err != nil {
		t.Fatal(err)
	}
	lockWait = 200 * time.Millisecond
	defer func() { lockWait = 5 * time.Second }()
	if _, err := s.Switch(list[0].Key, nil); err == nil {
		t.Fatal("switch went ahead while Claude held its lock")
	}
	if r := liveRefresh(t, s); r != "rb1" {
		t.Fatalf("tokens changed under the lock: %q", r)
	}
	// A lock left behind by a dead process goes stale.
	old := time.Now().Add(-time.Minute)
	_ = os.Chtimes(lock, old, old)
	if _, err := s.Switch(list[0].Key, nil); err != nil {
		t.Fatal(err)
	}
}

func TestSpliceTopLevel(t *testing.T) {
	cases := []struct{ in, want string }{
		{"{\n  \"a\": 1,\n  \"oauthAccount\": {\"x\": 1},\n  \"b\": [1]\n}\n", "{\n  \"a\": 1,\n  \"oauthAccount\": NEW,\n  \"b\": [1]\n}\n"},
		{"{\n  \"a\": 1\n}\n", "{\n  \"a\": 1,\n  \"oauthAccount\": NEW\n}\n"},
		{"{}", "{\n  \"oauthAccount\": NEW\n}"},
		{"{\"oauthAccount\":null}", "{\"oauthAccount\":NEW}"},
		{"{\"nested\":{\"oauthAccount\":1}}", "{\"nested\":{\"oauthAccount\":1},\n  \"oauthAccount\": NEW\n}"},
	}
	for _, c := range cases {
		got, err := spliceTopLevel([]byte(c.in), "oauthAccount", []byte("NEW"))
		if err != nil || string(got) != c.want {
			t.Errorf("splice(%q) = %q, %v; want %q", c.in, got, err, c.want)
		}
	}
}

func TestServiceName(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	if p := findClaude(); p.Service != "Claude Code-credentials" {
		t.Fatalf("default service %q", p.Service)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", "/tmp/cc")
	p := findClaude()
	// sha256("/tmp/cc")[:8], as Claude Code computes it
	if p.Service != "Claude Code-credentials-aa3d8c96" {
		t.Fatalf("suffixed service %q", p.Service)
	}
	if p.Global != "/tmp/cc/.claude.json" || p.SecureDir != "/tmp/cc" {
		t.Fatalf("paths %+v", p)
	}
}
