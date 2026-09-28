package web

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Device is a browser paired with this machine's web app. Only a hash of its credential is kept.
type Device struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Hash     string `json:"hash,omitempty"`
	PairedAt int64  `json:"pairedAt"`
	LastSeen int64  `json:"lastSeen,omitempty"`
}

// Devices is the list of paired browsers, in ~/.vineyard/web-devices.json (0600). The daemon's web
// app and a `vineyardd web` on the same machine share it, so it is re-read when the file changes.
type Devices struct {
	path string

	mu      sync.Mutex
	list    []Device
	modTime time.Time
	saved   map[string]int64 // LastSeen as last written, so a busy phone does not rewrite the file
}

const lastSeenWrite = 10 * time.Minute

// DeviceTTL is how long a paired device may go unseen before it is signed out. Every visit renews it
// (the cookie is set again with the same lifetime), so a phone in use never has to pair again.
const DeviceTTL = 30 * 24 * time.Hour

// expireLocked drops devices unseen for DeviceTTL; it reports whether any went.
func (d *Devices) expireLocked(now time.Time) bool {
	cut := now.Add(-DeviceTTL).UnixMilli()
	kept := d.list[:0:0]
	for _, x := range d.list {
		seen := x.LastSeen
		if seen == 0 {
			seen = x.PairedAt
		}
		if seen >= cut {
			kept = append(kept, x)
		}
	}
	if len(kept) == len(d.list) {
		return false
	}
	d.list = kept
	return true
}

func NewDevices(path string) *Devices {
	return &Devices{path: path, saved: map[string]int64{}}
}

func (d *Devices) loadLocked() {
	st, err := os.Stat(d.path)
	if err != nil {
		d.list = nil
		d.modTime = time.Time{}
		return
	}
	if st.ModTime().Equal(d.modTime) && d.list != nil {
		return
	}
	b, err := os.ReadFile(d.path)
	if err != nil {
		return
	}
	var list []Device
	if json.Unmarshal(b, &list) != nil {
		return
	}
	d.list = list
	d.modTime = st.ModTime()
	for _, x := range list {
		d.saved[x.ID] = x.LastSeen
	}
}

func (d *Devices) saveLocked() error {
	if err := os.MkdirAll(filepath.Dir(d.path), 0o700); err != nil {
		return err
	}
	if d.list == nil {
		d.list = []Device{}
	}
	b, err := json.MarshalIndent(d.list, "", "  ")
	if err != nil {
		return err
	}
	tmp := d.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, d.path); err != nil {
		return err
	}
	if st, err := os.Stat(d.path); err == nil {
		d.modTime = st.ModTime()
	}
	for _, x := range d.list {
		d.saved[x.ID] = x.LastSeen
	}
	return nil
}

func hashToken(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}

// Add pairs a new device and returns the credential it presents from now on.
func (d *Devices) Add(name string) (token string, dev Device, err error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", Device{}, err
	}
	token = base64.RawURLEncoding.EncodeToString(raw[:])
	name = strings.TrimSpace(name)
	if name == "" {
		name = "Browser"
	}
	if len(name) > 60 {
		name = name[:60]
	}
	now := time.Now().UnixMilli()
	dev = Device{ID: newID(), Name: name, Hash: hashToken(token), PairedAt: now, LastSeen: now}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.loadLocked()
	d.list = append(d.list, dev)
	if err := d.saveLocked(); err != nil {
		return "", Device{}, err
	}
	return token, dev, nil
}

// Check finds the device a credential belongs to.
func (d *Devices) Check(token string) (Device, bool) {
	if token == "" {
		return Device{}, false
	}
	want := hashToken(token)
	d.mu.Lock()
	defer d.mu.Unlock()
	d.loadLocked()
	if d.expireLocked(time.Now()) {
		_ = d.saveLocked()
	}
	for i := range d.list {
		if subtle.ConstantTimeCompare([]byte(d.list[i].Hash), []byte(want)) == 1 {
			now := time.Now().UnixMilli()
			d.list[i].LastSeen = now
			if now-d.saved[d.list[i].ID] > lastSeenWrite.Milliseconds() {
				_ = d.saveLocked()
			}
			return d.list[i], true
		}
	}
	return Device{}, false
}

// List returns the paired devices without their credential hashes.
func (d *Devices) List() []Device {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.loadLocked()
	if d.expireLocked(time.Now()) {
		_ = d.saveLocked()
	}
	out := make([]Device, 0, len(d.list))
	for _, x := range d.list {
		x.Hash = ""
		out = append(out, x)
	}
	return out
}

// Revoke signs one device out ("" with all=true signs every device out).
func (d *Devices) Revoke(id string, all bool) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.loadLocked()
	kept := d.list[:0:0]
	found := false
	for _, x := range d.list {
		if all || x.ID == id {
			found = true
			continue
		}
		kept = append(kept, x)
	}
	if !found && !all {
		return errors.New("no such device")
	}
	d.list = kept
	return d.saveLocked()
}

// ---- pairing codes ------------------------------------------------------------------------------

const (
	codeTTL      = 10 * time.Minute
	maxCodes     = 5
	codeAlphabet = "ABCDEFGHJKMNPQRSTUVWXYZ23456789" // no I, L, O, 0, 1
	codeLen      = 8
	// After this many wrong codes every outstanding code is dropped and pairing pauses for a minute,
	// so guessing is not an option (31^8 codes, a handful of tries).
	maxFailures = 10
	failPause   = time.Minute
)

// Pairing holds the single-use codes a new device can pair with, in memory only.
type Pairing struct {
	mu       sync.Mutex
	codes    map[string]time.Time // code → expiry
	failures int
	paused   time.Time
}

func NewPairing() *Pairing { return &Pairing{codes: map[string]time.Time{}} }

// New mints a code, valid for 10 minutes and one use.
func (p *Pairing) New() (string, time.Duration) {
	var b [codeLen]byte
	_, _ = rand.Read(b[:])
	code := make([]byte, codeLen)
	for i, x := range b {
		code[i] = codeAlphabet[int(x)%len(codeAlphabet)]
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	for c, exp := range p.codes {
		if now.After(exp) {
			delete(p.codes, c)
		}
	}
	for len(p.codes) >= maxCodes { // drop the oldest
		var oldest string
		for c, exp := range p.codes {
			if oldest == "" || exp.Before(p.codes[oldest]) {
				oldest = c
			}
		}
		delete(p.codes, oldest)
	}
	p.codes[string(code)] = now.Add(codeTTL)
	return string(code), codeTTL
}

// NormalizeCode upper-cases a typed code and drops spaces and dashes.
func NormalizeCode(s string) string {
	return strings.Map(func(r rune) rune {
		if r == ' ' || r == '-' {
			return -1
		}
		return r
	}, strings.ToUpper(strings.TrimSpace(s)))
}

var errPairPaused = errors.New("too many wrong codes; make a new one in a minute")

// Redeem uses up a code. Wrong codes count towards the pause.
func (p *Pairing) Redeem(code string) error {
	code = NormalizeCode(code)
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	if now.Before(p.paused) {
		return errPairPaused
	}
	exp, ok := p.codes[code]
	if ok && now.Before(exp) {
		delete(p.codes, code)
		p.failures = 0
		return nil
	}
	p.failures++
	if p.failures >= maxFailures {
		p.codes = map[string]time.Time{}
		p.failures = 0
		p.paused = now.Add(failPause)
		return errPairPaused
	}
	return errors.New("that code is wrong or has expired")
}
