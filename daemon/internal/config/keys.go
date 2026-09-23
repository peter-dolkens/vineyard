package config

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"time"
)

// Fleet key rotation. A rotation makes a new key with two certificates: a self-signed one, which is
// the fleet certificate from then on, and a cross certificate for the same key signed by the old
// key. Until the grace period ends, rotated machines present the cross certificate and trust both
// the old and the new certificate, so machines still on the old key (offline at rotation time)
// accept them and are accepted, and get the new key pushed to them when they reconnect. After the
// grace period only the new key is trusted and the cross and previous certificates are deleted.
const (
	PrevCertFile  = "fleet-prev.crt"  // the certificate before the last rotation, trusted during grace
	CrossCertFile = "fleet-cross.crt" // the current key signed by the previous one, presented during grace
)

// PathIn is Path for a directory other than Dir (tests run several daemons in one process).
func PathIn(dir, name string) string {
	if dir == "" {
		dir = Dir()
	}
	return filepath.Join(dir, name)
}

// NewFleetKey makes a new fleet key and its self-signed and cross certificates, the cross one signed
// by prevKeyPEM. at (Unix ms) names the key in the certificate subject, which keeps the two
// generations apart when a machine picks which certificate a peer will accept.
func NewFleetKey(prevCertPEM, prevKeyPEM []byte, at int64) (certPEM, keyPEM, crossPEM []byte, err error) {
	prevCert, err := parseCertPEM(prevCertPEM)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("current fleet certificate: %w", err)
	}
	prevKey, err := parseKeyPEM(prevKeyPEM)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("current fleet key: %w", err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, nil, err
	}
	tmpl := func() (*x509.Certificate, error) {
		serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
		if err != nil {
			return nil, err
		}
		return &x509.Certificate{
			SerialNumber: serial,
			Subject: pkix.Name{CommonName: FleetServerName, Organization: []string{"Vineyard fleet"},
				OrganizationalUnit: []string{fmt.Sprintf("key %d", at)}},
			NotBefore:             time.Now().Add(-time.Hour),
			NotAfter:              time.Now().AddDate(100, 0, 0),
			KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment | x509.KeyUsageCertSign,
			ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
			BasicConstraintsValid: true,
			IsCA:                  true,
			DNSNames:              []string{FleetServerName},
		}, nil
	}
	self, err := tmpl()
	if err != nil {
		return nil, nil, nil, err
	}
	selfDER, err := x509.CreateCertificate(rand.Reader, self, self, &key.PublicKey, key)
	if err != nil {
		return nil, nil, nil, err
	}
	cross, err := tmpl()
	if err != nil {
		return nil, nil, nil, err
	}
	crossDER, err := x509.CreateCertificate(rand.Reader, cross, prevCert, &key.PublicKey, prevKey)
	if err != nil {
		return nil, nil, nil, err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, nil, err
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: selfDER})
	crossPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: crossDER})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM, crossPEM, nil
}

// InstallKeys writes a key set into dir, each file atomically. Empty cross or prev removes that file.
func InstallKeys(dir string, cert, key, cross, prev []byte) error {
	if dir == "" {
		dir = Dir()
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	for _, f := range []struct {
		name string
		data []byte
	}{{CertFile, cert}, {KeyFile, key}, {CrossCertFile, cross}, {PrevCertFile, prev}} {
		p := filepath.Join(dir, f.name)
		if len(f.data) == 0 {
			if f.name == CertFile || f.name == KeyFile {
				return errors.New("a key set needs a certificate and a key")
			}
			_ = os.Remove(p)
			continue
		}
		tmp := p + ".tmp"
		if err := os.WriteFile(tmp, f.data, 0o600); err != nil {
			return err
		}
		if err := os.Rename(tmp, p); err != nil {
			return err
		}
	}
	return nil
}

// RemoveGraceFiles deletes the previous and cross certificates once a grace period is over.
func RemoveGraceFiles(dir string) {
	_ = os.Remove(PathIn(dir, PrevCertFile))
	_ = os.Remove(PathIn(dir, CrossCertFile))
}

func parseCertPEM(b []byte) (*x509.Certificate, error) {
	blk, _ := pem.Decode(b)
	if blk == nil {
		return nil, errors.New("not PEM")
	}
	return x509.ParseCertificate(blk.Bytes)
}

func parseKeyPEM(b []byte) (crypto.Signer, error) {
	blk, _ := pem.Decode(b)
	if blk == nil {
		return nil, errors.New("not PEM")
	}
	if k, err := x509.ParseECPrivateKey(blk.Bytes); err == nil {
		return k, nil
	}
	k, err := x509.ParsePKCS8PrivateKey(blk.Bytes)
	if err != nil {
		return nil, err
	}
	s, ok := k.(crypto.Signer)
	if !ok {
		return nil, errors.New("unsupported key type")
	}
	return s, nil
}
