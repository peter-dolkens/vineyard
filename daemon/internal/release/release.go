// Package release signs and verifies vineyardd builds. A daemon installs a binary sent over the mesh
// only if it carries a signature from one of the release keys compiled in below, over its platform,
// version and SHA-256; the fleet key alone is no longer enough to put code on every machine.
//
// There are two keys, and every release is signed with both. Rotation replaces one at a time: a
// release that trusts the new key and the untouched old one, signed with both, is accepted by daemons
// that still trust only the old pair, through the untouched key. See SECURITY.md.
package release

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
)

// Key is a trusted release public key.
type Key struct {
	ID  string // first 8 bytes of SHA-256 of the public key, hex
	Pub ed25519.PublicKey
}

func mustKey(b64 string) Key {
	pub, err := base64.StdEncoding.DecodeString(b64)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		panic("bad release key " + b64)
	}
	return Key{ID: KeyID(pub), Pub: pub}
}

// Trusted are the release keys this build accepts. Replace one entry at a time (see SECURITY.md).
var Trusted = []Key{
	mustKey("iiy8+zkGEN1k0XXwfF5lJN40dASMQnXwlRudb2oEGPU="), // A: a2dfba40b643fe94
	mustKey("tcHuuKrAq9Q3intg1sGwZtmAVYQcX8oZYXwYWI05QlI="), // B: 0b30102a3a753b4f
}

func KeyID(pub ed25519.PublicKey) string {
	h := sha256.Sum256(pub)
	return hex.EncodeToString(h[:8])
}

// Signature is what travels with a binary (the <binary>.sig file, the upgrade's Signature field).
type Signature struct {
	V        int      `json:"v"`
	Platform string   `json:"platform"` // <os>-<arch>
	Version  string   `json:"version"`
	SHA256   string   `json:"sha256"`
	Sigs     []KeySig `json:"sigs"`
}

type KeySig struct {
	Key string `json:"key"`
	Sig string `json:"sig"` // base64 Ed25519 signature of Message(...)
}

// Message is the exact byte string a release key signs.
func Message(platform, version, sha string) []byte {
	return []byte(fmt.Sprintf("vineyardd release v1\nplatform=%s\nversion=%s\nsha256=%s\n", platform, version, strings.ToLower(sha)))
}

// Sign signs a build with every given private key.
func Sign(platform, version, sha string, keys []ed25519.PrivateKey) Signature {
	s := Signature{V: 1, Platform: platform, Version: version, SHA256: strings.ToLower(sha)}
	for _, k := range keys {
		pub := k.Public().(ed25519.PublicKey)
		s.Sigs = append(s.Sigs, KeySig{Key: KeyID(pub), Sig: base64.StdEncoding.EncodeToString(ed25519.Sign(k, Message(platform, version, sha)))})
	}
	return s
}

var ErrUnsigned = errors.New("the binary is not signed by a Vineyard release key")

// Verify checks that raw (a Signature as JSON) covers exactly this platform, version and digest and
// carries a valid signature from at least one trusted key.
func Verify(raw []byte, platform, version, sha string) error {
	return verifyWith(Trusted, raw, platform, version, sha)
}

func verifyWith(trusted []Key, raw []byte, platform, version, sha string) error {
	if len(raw) == 0 {
		return ErrUnsigned
	}
	var s Signature
	if err := json.Unmarshal(raw, &s); err != nil {
		return fmt.Errorf("unreadable signature: %w", err)
	}
	if s.V != 1 {
		return fmt.Errorf("unknown signature version %d", s.V)
	}
	if s.Platform != platform || s.Version != version || !strings.EqualFold(s.SHA256, sha) {
		return fmt.Errorf("the signature is for %s %s (%.12s), not %s %s (%.12s)", s.Platform, s.Version, s.SHA256, platform, version, sha)
	}
	msg := Message(platform, version, sha)
	for _, ks := range s.Sigs {
		sig, err := base64.StdEncoding.DecodeString(ks.Sig)
		if err != nil {
			continue
		}
		for _, k := range trusted {
			if k.ID == ks.Key && ed25519.Verify(k.Pub, msg, sig) {
				return nil
			}
		}
	}
	return ErrUnsigned
}

// SigPath is where a binary's signature lives: next to it, with ".sig" appended.
func SigPath(bin string) string { return bin + ".sig" }

// ReadSig returns the signature file next to bin, or nil.
func ReadSig(bin string) []byte {
	b, err := os.ReadFile(SigPath(bin))
	if err != nil {
		return nil
	}
	return b
}

// KeysFromEnv reads VINEYARD_SIGNING_KEY_A / _B (base64 Ed25519 seeds) for signing builds.
func KeysFromEnv() ([]ed25519.PrivateKey, error) {
	var out []ed25519.PrivateKey
	for _, name := range []string{"VINEYARD_SIGNING_KEY_A", "VINEYARD_SIGNING_KEY_B"} {
		v := strings.TrimSpace(os.Getenv(name))
		if v == "" {
			continue
		}
		seed, err := base64.StdEncoding.DecodeString(v)
		if err != nil || len(seed) != ed25519.SeedSize {
			return nil, fmt.Errorf("%s is not a base64 Ed25519 seed", name)
		}
		out = append(out, ed25519.NewKeyFromSeed(seed))
	}
	if len(out) == 0 {
		return nil, errors.New("no signing keys: set VINEYARD_SIGNING_KEY_A and VINEYARD_SIGNING_KEY_B")
	}
	return out, nil
}
