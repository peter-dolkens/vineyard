// Package accounts switches the Claude subscription account Claude Code is signed in as on this
// machine, keeping every account it has seen signed in so switching back needs no new sign-in.
//
// A switch does what `claude auth login` as the other account would: it replaces the login's keys in
// Claude Code's token store and the oauthAccount block of ~/.claude.json, and nothing else. Projects,
// transcripts, memory, settings and MCP logins stay as they are. Running sessions pick the new account
// up on their own, as they do after a /login in another terminal.
//
// Refresh tokens rotate: whenever a Claude refreshes, the old refresh token dies. So the live store is
// the truth for the signed-in account, and every list and switch first saves it into the cache.
// Nothing here talks to the network; an account left unused past its refresh token's expiry (about a
// month) has to be signed in again.
//
// Cached tokens stay on this machine: in the login keychain on macOS, otherwise in
// ~/.vineyard/claude-accounts.json (mode 0600, as Claude Code keeps its own .credentials.json).
package accounts

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
	"time"

	"github.com/peter-dolkens/vineyard/daemon/internal/config"
	"github.com/peter-dolkens/vineyard/daemon/internal/model"
)

// VaultService names the keychain items holding cached accounts' tokens (macOS).
const VaultService = "Vineyard Claude account"

// Account is what a viewer sees of one cached login; never any token.
type Account struct {
	Key          string `json:"key"` // accountUuid:organizationUuid
	Email        string `json:"email,omitempty"`
	Name         string `json:"name,omitempty"`
	Organization string `json:"organization,omitempty"`
	Plan         string `json:"plan,omitempty"` // subscriptionType: pro, max, team, ...
	Active       bool   `json:"active,omitempty"`
	// SavedAt is when its tokens were last saved; RefreshExpiresAt when its sign-in lapses unless
	// Claude refreshes it before then (epoch ms, 0 = unknown).
	SavedAt          int64 `json:"savedAt,omitempty"`
	RefreshExpiresAt int64 `json:"refreshExpiresAt,omitempty"`
	// Usage is the last limit report seen while it was signed in.
	Usage *model.Usage `json:"usage,omitempty"`
}

type entry struct {
	Key              string          `json:"key"`
	OAuthAccount     json.RawMessage `json:"oauthAccount"`
	Plan             string          `json:"plan,omitempty"`
	SavedAt          int64           `json:"savedAt,omitempty"`
	RefreshExpiresAt int64           `json:"refreshExpiresAt,omitempty"`
	ActiveSince      int64           `json:"activeSince,omitempty"`
	Usage            *model.Usage    `json:"usage,omitempty"`
	// Tokens are the login's token-store keys, kept here only where there is no vault.
	Tokens map[string]json.RawMessage `json:"tokens,omitempty"`
}

type cacheFile struct {
	// Active is the account signed in when the file was last written, to notice sign-ins elsewhere.
	Active   string   `json:"active,omitempty"`
	Accounts []*entry `json:"accounts"`
}

// vault keeps cached accounts' tokens out of the cache file.
type vault interface {
	Get(key string) ([]byte, error) // nil when absent
	Put(key string, b []byte) error
	Delete(key string) error
}

type keychainVault struct{}

func (keychainVault) Get(key string) ([]byte, error) {
	return keychain{service: VaultService, account: key}.Get()
}
func (keychainVault) Put(key string, b []byte) error {
	return keychain{service: VaultService, account: key}.Put(b)
}
func (keychainVault) Delete(key string) error {
	return keychain{service: VaultService, account: key}.Delete()
}

// Store is this machine's set of cached Claude accounts.
type Store struct {
	mu    sync.Mutex
	file  string
	paths claudePaths
	live  func() (tokenStore, error)
	vault vault // nil: tokens are kept in file
	now   func() time.Time
}

// New returns the store kept in dir (the Vineyard config dir) for this user's Claude Code login.
func New(dir string) *Store {
	s := &Store{file: filepath.Join(dir, "claude-accounts.json"), paths: findClaude(), now: time.Now}
	s.live = func() (tokenStore, error) { return liveTokens(s.paths) }
	if runtime.GOOS == "darwin" {
		s.vault = keychainVault{}
	}
	return s
}

// identity is the part of oauthAccount (and of the tokens) that names the login.
type identity struct {
	AccountUUID      string `json:"accountUuid"`
	Email            string `json:"emailAddress"`
	OrganizationUUID string `json:"organizationUuid"`
	OrganizationName string `json:"organizationName"`
	DisplayName      string `json:"displayName"`
}

type oauthTokens struct {
	RefreshToken          string `json:"refreshToken"`
	RefreshTokenExpiresAt int64  `json:"refreshTokenExpiresAt"`
	SubscriptionType      string `json:"subscriptionType"`
}

func parseIdentity(raw json.RawMessage) identity {
	var id identity
	_ = json.Unmarshal(raw, &id)
	return id
}

func (id identity) key() string { return id.AccountUUID + ":" + id.OrganizationUUID }

// current reads the signed-in login from Claude Code's own files; ok is false when it is not signed
// in to a subscription.
func (s *Store) current() (e *entry, tokens map[string]json.RawMessage, ok bool, err error) {
	store, err := s.live()
	if err != nil {
		return nil, nil, false, err
	}
	all, err := store.Read()
	if err != nil {
		return nil, nil, false, err
	}
	oa, err := readOAuthAccount(s.paths.Global)
	if err != nil {
		return nil, nil, false, err
	}
	var t oauthTokens
	if raw := all["claudeAiOauth"]; len(raw) > 0 {
		_ = json.Unmarshal(raw, &t)
	}
	id := parseIdentity(oa)
	if t.RefreshToken == "" || id.AccountUUID == "" {
		return nil, nil, false, nil
	}
	tokens = map[string]json.RawMessage{}
	for _, k := range accountKeys {
		if v, has := all[k]; has {
			tokens[k] = v
		}
	}
	return &entry{Key: id.key(), OAuthAccount: oa, Plan: t.SubscriptionType, RefreshExpiresAt: t.RefreshTokenExpiresAt}, tokens, true, nil
}

func (s *Store) load() (*cacheFile, error) {
	b, err := os.ReadFile(s.file)
	if errors.Is(err, fs.ErrNotExist) {
		return &cacheFile{}, nil
	}
	if err != nil {
		return nil, err
	}
	var c cacheFile
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("%s: %w", s.file, err)
	}
	return &c, nil
}

func (s *Store) save(c *cacheFile) error {
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return config.WriteFileAtomic(s.file, b)
}

func (c *cacheFile) find(key string) *entry {
	for _, e := range c.Accounts {
		if e.Key == key {
			return e
		}
	}
	return nil
}

// saveCurrent caches the signed-in login's latest tokens, and gives it latest if that report was seen
// while it was signed in. It returns the signed-in key ("" when signed out).
func (s *Store) saveCurrent(c *cacheFile, latest *model.Usage) (string, error) {
	cur, tokens, ok, err := s.current()
	if err != nil || !ok {
		return "", err
	}
	now := s.now().UnixMilli()
	e := c.find(cur.Key)
	if e == nil {
		e = &entry{Key: cur.Key, ActiveSince: now}
		c.Accounts = append(c.Accounts, e)
	}
	e.OAuthAccount, e.Plan, e.RefreshExpiresAt, e.SavedAt = cur.OAuthAccount, cur.Plan, cur.RefreshExpiresAt, now
	if c.Active != cur.Key || e.ActiveSince == 0 {
		e.ActiveSince, c.Active = now, cur.Key // signed in since we last looked
	}
	if latest != nil && latest.At >= e.ActiveSince && (e.Usage == nil || latest.At > e.Usage.At) {
		u := *latest
		e.Usage = &u
	}
	if s.vault != nil {
		b, err := json.Marshal(tokens)
		if err != nil {
			return "", err
		}
		if err := s.vault.Put(cur.Key, b); err != nil {
			return "", fmt.Errorf("save the signed-in account: %w", err)
		}
		e.Tokens = nil
	} else {
		e.Tokens = tokens
	}
	return cur.Key, nil
}

func (s *Store) tokensOf(e *entry) (map[string]json.RawMessage, error) {
	if s.vault == nil {
		return e.Tokens, nil
	}
	b, err := s.vault.Get(e.Key)
	if err != nil || b == nil {
		return nil, err
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return m, nil
}

func (s *Store) list(c *cacheFile, active string) []Account {
	out := make([]Account, 0, len(c.Accounts))
	for _, e := range c.Accounts {
		id := parseIdentity(e.OAuthAccount)
		out = append(out, Account{
			Key: e.Key, Email: id.Email, Name: id.DisplayName, Organization: id.OrganizationName, Plan: e.Plan,
			Active: e.Key == active, SavedAt: e.SavedAt, RefreshExpiresAt: e.RefreshExpiresAt, Usage: e.Usage,
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Email != out[j].Email {
			return out[i].Email < out[j].Email
		}
		return out[i].Organization < out[j].Organization
	})
	return out
}

// List saves the signed-in login and returns every cached account. latest is the machine's newest
// limit report, credited to the signed-in account.
func (s *Store) List(latest *model.Usage) ([]Account, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, err := s.load()
	if err != nil {
		return nil, err
	}
	active, err := s.saveCurrent(c, latest)
	if err != nil {
		return nil, err
	}
	if err := s.save(c); err != nil {
		return nil, err
	}
	return s.list(c, active), nil
}

// SaveCurrent caches the signed-in login (before a sign-in replaces it, and after one).
func (s *Store) SaveCurrent(latest *model.Usage) error {
	_, err := s.List(latest)
	return err
}

// Switch signs Claude Code in as the cached account key.
func (s *Store) Switch(key string, latest *model.Usage) ([]Account, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, err := s.load()
	if err != nil {
		return nil, err
	}
	active, err := s.saveCurrent(c, latest)
	if err != nil {
		return nil, err
	}
	if err := s.save(c); err != nil {
		return nil, err
	}
	if key == active {
		return s.list(c, active), nil
	}
	target := c.find(key)
	if target == nil {
		return nil, errors.New("that account is not saved on this machine")
	}
	now := s.now().UnixMilli()
	if target.RefreshExpiresAt > 0 && target.RefreshExpiresAt < now {
		return nil, fmt.Errorf("the sign-in for %s has expired; add the account again", parseIdentity(target.OAuthAccount).Email)
	}
	want, err := s.tokensOf(target)
	if err != nil {
		return nil, err
	}
	if len(want["claudeAiOauth"]) == 0 {
		return nil, errors.New("this machine no longer has that account's sign-in; add the account again")
	}
	store, err := s.live()
	if err != nil {
		return nil, err
	}

	var before map[string]json.RawMessage
	err = withLock(filepath.Join(s.paths.SecureDir, ".storage-write"), func() error {
		cur, err := store.Read()
		if err != nil {
			return err
		}
		before = cur
		next := map[string]json.RawMessage{}
		for k, v := range cur {
			next[k] = v
		}
		for _, k := range accountKeys {
			delete(next, k)
		}
		for k, v := range want {
			next[k] = v
		}
		return store.Write(next)
	})
	if err != nil {
		return nil, fmt.Errorf("install the account's sign-in: %w", err)
	}
	if err := withLock(s.paths.Global, func() error { return writeOAuthAccount(s.paths.Global, target.OAuthAccount) }); err != nil {
		// Put the tokens back so the tokens and the profile keep naming the same account.
		if before != nil {
			_ = withLock(filepath.Join(s.paths.SecureDir, ".storage-write"), func() error { return store.Write(before) })
		}
		return nil, fmt.Errorf("update %s: %w", filepath.Base(s.paths.Global), err)
	}

	cur, _, ok, err := s.current()
	if err != nil {
		return nil, err
	}
	if !ok || cur.Key != key {
		return nil, errors.New("the switch did not stick: another Claude process rewrote the sign-in; try again")
	}
	target.ActiveSince, c.Active = now, key
	if err := s.save(c); err != nil {
		return nil, err
	}
	return s.list(c, key), nil
}

// Remove forgets a cached account. It does not sign anything out; the signed-in account stays.
func (s *Store) Remove(key string) ([]Account, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, err := s.load()
	if err != nil {
		return nil, err
	}
	active, err := s.saveCurrent(c, nil)
	if err != nil {
		return nil, err
	}
	if key == active {
		return nil, errors.New("that account is signed in now; switch to another before removing it")
	}
	kept := c.Accounts[:0]
	for _, e := range c.Accounts {
		if e.Key != key {
			kept = append(kept, e)
		}
	}
	c.Accounts = kept
	if s.vault != nil {
		if err := s.vault.Delete(key); err != nil {
			return nil, err
		}
	}
	if err := s.save(c); err != nil {
		return nil, err
	}
	return s.list(c, active), nil
}
