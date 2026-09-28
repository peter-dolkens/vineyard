package mesh

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"runtime"

	"github.com/peter-dolkens/vineyard/daemon/internal/release"
)

// Tests sign builds with their own key; the compiled-in release keys stay out of reach.
var testSigningKey ed25519.PrivateKey

func init() {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		panic(err)
	}
	testSigningKey = priv
	release.Trusted = []release.Key{{ID: release.KeyID(pub), Pub: pub}}
}

func testSig(platform, version, sha string) json.RawMessage {
	b, _ := json.Marshal(release.Sign(platform, version, sha, []ed25519.PrivateKey{testSigningKey}))
	return b
}

func hostPlatform() string { return runtime.GOOS + "-" + runtime.GOARCH }

func x509Pool(certs []*x509.Certificate) *x509.CertPool {
	p := x509.NewCertPool()
	for _, c := range certs {
		p.AddCert(c)
	}
	return p
}
