package config

import (
	"crypto/x509"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
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

// Writers replacing one file at the same time never leave it partial or empty, nor fail each other.
func TestWriteFileAtomicConcurrent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "machine.crt")
	want := map[string]bool{}
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for i := range 16 {
		data := strings.Repeat(fmt.Sprintf("writer %d\n", i), 4096)
		want[data] = true
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- WriteFileAtomic(path, []byte(data))
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	got, err := os.ReadFile(path)
	if err != nil || !want[string(got)] {
		t.Fatalf("file holds %d bytes that no single writer wrote (err %v)", len(got), err)
	}
	if left, _ := filepath.Glob(path + ".*.tmp"); len(left) > 0 {
		t.Fatalf("temporary files left behind: %v", left)
	}
}
