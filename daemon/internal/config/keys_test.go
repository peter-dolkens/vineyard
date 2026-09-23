package config

import (
	"crypto/x509"
	"os"
	"testing"
)

// The cross certificate chains to the previous certificate; the self-signed one stands alone; and a
// verifier trusting both accepts the cross certificate, which is what grace periods rely on.
func TestNewFleetKeyBridgesToThePreviousKey(t *testing.T) {
	t.Setenv("VINEYARD_DIR", t.TempDir())
	if err := GenerateFleetCert(); err != nil {
		t.Fatal(err)
	}
	prevCert, _ := os.ReadFile(Path(CertFile))
	prevKey, _ := os.ReadFile(Path(KeyFile))
	cert, key, cross, err := NewFleetKey(prevCert, prevKey, 1700000000000)
	if err != nil || len(key) == 0 {
		t.Fatal(err)
	}
	prev, _ := parseCertPEM(prevCert)
	self, _ := parseCertPEM(cert)
	crossCert, _ := parseCertPEM(cross)
	verify := func(c *x509.Certificate, roots ...*x509.Certificate) error {
		pool := x509.NewCertPool()
		for _, r := range roots {
			pool.AddCert(r)
		}
		_, err := c.Verify(x509.VerifyOptions{Roots: pool, DNSName: FleetServerName, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny}})
		return err
	}
	if err := verify(crossCert, prev); err != nil {
		t.Fatalf("old members must accept the cross certificate: %v", err)
	}
	if err := verify(crossCert, self, prev); err != nil {
		t.Fatalf("rotated members in grace must accept it: %v", err)
	}
	if err := verify(self, self); err != nil {
		t.Fatalf("the new certificate must stand alone after grace: %v", err)
	}
	if err := verify(prev, self); err == nil {
		t.Fatal("after grace the previous certificate must not verify")
	}
	if self.Subject.String() == prev.Subject.String() {
		t.Fatal("generations need distinct subjects")
	}
}
