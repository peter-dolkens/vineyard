package release

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"testing"
)

func keys(t *testing.T) (ed25519.PrivateKey, ed25519.PrivateKey, []Key) {
	t.Helper()
	pa, a, _ := ed25519.GenerateKey(rand.Reader)
	pb, b, _ := ed25519.GenerateKey(rand.Reader)
	return a, b, []Key{{ID: KeyID(pa), Pub: pa}, {ID: KeyID(pb), Pub: pb}}
}

const sha = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestSignedBuildVerifies(t *testing.T) {
	a, b, trusted := keys(t)
	raw, _ := json.Marshal(Sign("darwin-arm64", "v0.4.0", sha, []ed25519.PrivateKey{a, b}))
	if err := verifyWith(trusted, raw, "darwin-arm64", "v0.4.0", sha); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ p, v, s string }{{"linux-arm64", "v0.4.0", sha}, {"darwin-arm64", "v0.4.1", sha}, {"darwin-arm64", "v0.4.0", "ff" + sha[2:]}} {
		if err := verifyWith(trusted, raw, c.p, c.v, c.s); err == nil {
			t.Errorf("signature accepted for %v", c)
		}
	}
}

// Rotation: a release trusting {A', B}, signed with A' and B, is accepted by a daemon that still
// trusts only {A, B}, through B.
func TestOneKeyAtATimeRotation(t *testing.T) {
	a, b, oldTrust := keys(t)
	_ = a
	pa2, a2, _ := ed25519.GenerateKey(rand.Reader)
	raw, _ := json.Marshal(Sign("linux-amd64", "v0.5.0", sha, []ed25519.PrivateKey{a2, b}))
	if err := verifyWith(oldTrust, raw, "linux-amd64", "v0.5.0", sha); err != nil {
		t.Fatalf("old daemon refused the rotation release: %v", err)
	}
	newTrust := []Key{{ID: KeyID(pa2), Pub: pa2}, oldTrust[1]}
	if err := verifyWith(newTrust, raw, "linux-amd64", "v0.5.0", sha); err != nil {
		t.Fatal(err)
	}
	// A retired key alone no longer passes.
	onlyA, _ := json.Marshal(Sign("linux-amd64", "v0.5.0", sha, []ed25519.PrivateKey{a}))
	if err := verifyWith(newTrust, onlyA, "linux-amd64", "v0.5.0", sha); err == nil {
		t.Fatal("a retired key was accepted")
	}
}

func TestForgedAndMissing(t *testing.T) {
	_, _, trusted := keys(t)
	_, evil, _ := ed25519.GenerateKey(rand.Reader)
	raw, _ := json.Marshal(Sign("darwin-arm64", "v1", sha, []ed25519.PrivateKey{evil}))
	var s Signature
	_ = json.Unmarshal(raw, &s)
	s.Sigs[0].Key = trusted[0].ID // claim a trusted key id
	raw, _ = json.Marshal(s)
	if err := verifyWith(trusted, raw, "darwin-arm64", "v1", sha); err == nil {
		t.Fatal("forged signature accepted")
	}
	if err := verifyWith(trusted, nil, "darwin-arm64", "v1", sha); err != ErrUnsigned {
		t.Fatalf("missing signature: %v", err)
	}
}

func TestCompiledInKeysParse(t *testing.T) {
	if len(Trusted) != 2 || Trusted[0].ID == Trusted[1].ID {
		t.Fatalf("trusted keys: %+v", Trusted)
	}
}
