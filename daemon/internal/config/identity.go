package config

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Per-machine identity. Every machine has its own key (machine.key) and a certificate naming it
// (machine.crt, leaf first, then the certificates of the machines that vouched for it, up to but not
// including a fleet root in fleet.crt). A machine's certificate is signed by the member that invited
// it, with that member's own key: nobody needs a key that can mint arbitrary identities, and removing
// a machine revokes its key and, with it, every certificate it signed.
//
// fleet.crt holds the trusted roots (public). fleet.key, the old shared key that was also the root's
// private key, is kept only while a fleet migrates to machine certificates (see Migrate) and deleted
// once every member presents its own; a fleet created from 0.3.23 on never keeps a root key at all.
const (
	MachineKeyFile  = "machine.key"
	MachineCertFile = "machine.crt"
	uriPrefix       = "vineyard://machine/"
)

// MachineURI is the SAN that names a machine in its certificate.
func MachineURI(id string) string { return uriPrefix + url.PathEscape(id) }

// CertMachineID is the machine a certificate names, or "" when it names none (a fleet root, or the
// shared certificate every member presented before 0.3.23).
func CertMachineID(c *x509.Certificate) string {
	for _, u := range c.URIs {
		s := u.String()
		if rest, ok := strings.CutPrefix(s, uriPrefix); ok {
			if id, err := url.PathUnescape(rest); err == nil {
				return id
			}
		}
	}
	return ""
}

// KeyFingerprint identifies a certificate's key: hex SHA-256 of its SubjectPublicKeyInfo, 128 bits.
func KeyFingerprint(c *x509.Certificate) string {
	h := sha256.Sum256(c.RawSubjectPublicKeyInfo)
	return hex.EncodeToString(h[:16])
}

func serial() (*big.Int, error) {
	return rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
}

// machineTemplate is a machine's certificate: it names the machine, authenticates both ends of a
// fleet connection (and keeps the "vineyard" DNS name older daemons check for), and may sign the
// certificates of machines it invites.
func machineTemplate(id string) (*x509.Certificate, error) {
	sn, err := serial()
	if err != nil {
		return nil, err
	}
	u, err := url.Parse(MachineURI(id))
	if err != nil {
		return nil, err
	}
	return &x509.Certificate{
		SerialNumber:          sn,
		Subject:               pkix.Name{CommonName: id, Organization: []string{"Vineyard fleet"}},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(50, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		DNSNames:              []string{FleetServerName},
		URIs:                  []*url.URL{u},
	}, nil
}

// NewRoot makes a self-signed fleet root. Its key signs the first certificates and is then thrown
// away (NewFleet) or kept only as long as a migration needs it.
func NewRoot(label string) (certPEM []byte, key *ecdsa.PrivateKey, err error) {
	key, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	sn, err := serial()
	if err != nil {
		return nil, nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          sn,
		Subject:               pkix.Name{CommonName: FleetServerName, Organization: []string{"Vineyard fleet"}, OrganizationalUnit: []string{"root " + label}},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(100, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		DNSNames:              []string{FleetServerName},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), key, nil
}

// IssueMachineCert signs a certificate naming id for pub, with the issuer's certificate and key.
func IssueMachineCert(issuer *x509.Certificate, issuerKey crypto.Signer, id string, pub crypto.PublicKey) ([]byte, error) {
	if id == "" {
		return nil, errors.New("a machine certificate needs a machine id")
	}
	tmpl, err := machineTemplate(id)
	if err != nil {
		return nil, err
	}
	return x509.CreateCertificate(rand.Reader, tmpl, issuer, pub, issuerKey)
}

// Bridge certifies a new fleet root's public key with this machine's key, so machines that still
// trust only the old root can verify certificates under the new one while a re-issue's grace period
// runs. Returns the bridge followed by this machine's chain (PEM): the intermediates to present.
func (m *Machine) Bridge(root *x509.Certificate) ([]byte, error) {
	sn, err := serial()
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          sn,
		Subject:               root.Subject,
		SubjectKeyId:          root.SubjectKeyId,
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(1, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		DNSNames:              []string{FleetServerName},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, m.Leaf, root.PublicKey, m.Key)
	if err != nil {
		return nil, err
	}
	return append(certsPEM(der), m.ChainPEM()...), nil
}

// NewMachineKey makes a machine's private key.
func NewMachineKey() (*ecdsa.PrivateKey, []byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	return key, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}), nil
}

func certsPEM(ders ...[]byte) []byte {
	var out []byte
	for _, d := range ders {
		out = append(out, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: d})...)
	}
	return out
}

// ParseCerts reads every certificate in a PEM bundle, in order.
func ParseCerts(b []byte) ([]*x509.Certificate, error) {
	var out []*x509.Certificate
	for {
		var blk *pem.Block
		blk, b = pem.Decode(b)
		if blk == nil {
			break
		}
		if blk.Type != "CERTIFICATE" {
			continue
		}
		c, err := x509.ParseCertificate(blk.Bytes)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	if len(out) == 0 {
		return nil, errors.New("no certificates")
	}
	return out, nil
}

// Machine is this machine's identity as loaded from disk.
type Machine struct {
	TLS   tls.Certificate     // the chain and key, for handshakes
	Leaf  *x509.Certificate   // this machine's certificate
	Chain []*x509.Certificate // Leaf, then the vouching certificates up to a root (exclusive)
	Key   crypto.Signer
	ID    string
}

// HasMachine reports whether dir holds a machine identity.
func HasMachine(dir string) bool {
	_, e1 := os.Stat(PathIn(dir, MachineKeyFile))
	_, e2 := os.Stat(PathIn(dir, MachineCertFile))
	return e1 == nil && e2 == nil
}

// LoadMachine reads machine.key and machine.crt.
func LoadMachine(dir string) (*Machine, error) {
	chainPEM, err := os.ReadFile(PathIn(dir, MachineCertFile))
	if err != nil {
		return nil, err
	}
	keyPEM, err := os.ReadFile(PathIn(dir, MachineKeyFile))
	if err != nil {
		return nil, err
	}
	pair, err := tls.X509KeyPair(chainPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("machine certificate: %w", err)
	}
	chain, err := ParseCerts(chainPEM)
	if err != nil {
		return nil, err
	}
	key, err := parseKeyPEM(keyPEM)
	if err != nil {
		return nil, err
	}
	id := CertMachineID(chain[0])
	if id == "" {
		return nil, errors.New("machine certificate names no machine")
	}
	return &Machine{TLS: pair, Leaf: chain[0], Chain: chain, Key: key, ID: id}, nil
}

// ChainPEM is the machine's chain as written to machine.crt.
func (m *Machine) ChainPEM() []byte {
	ders := make([][]byte, len(m.Chain))
	for i, c := range m.Chain {
		ders[i] = c.Raw
	}
	return certsPEM(ders...)
}

// Issue signs a certificate for another machine's key with this machine's: an invite, or a vouch that
// keeps a machine in the fleet after the one that invited it was removed. Returns the new machine's
// chain PEM (its certificate, then this machine's chain).
func (m *Machine) Issue(id string, pub crypto.PublicKey) ([]byte, error) {
	der, err := IssueMachineCert(m.Leaf, m.Key, id, pub)
	if err != nil {
		return nil, err
	}
	ders := [][]byte{der}
	for _, c := range m.Chain {
		ders = append(ders, c.Raw)
	}
	return certsPEM(ders...), nil
}

// WriteMachine stores a machine identity atomically: the key only when given (a chain replaced by a
// vouch or re-issue keeps the key it already has).
func WriteMachine(dir string, keyPEM, chainPEM []byte) error {
	if dir == "" {
		dir = Dir()
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	for _, f := range []struct {
		name string
		data []byte
	}{{MachineKeyFile, keyPEM}, {MachineCertFile, chainPEM}} {
		if len(f.data) == 0 {
			continue
		}
		if err := WriteFileAtomic(PathIn(dir, f.name), f.data); err != nil {
			return err
		}
	}
	return nil
}

// WriteFileAtomic replaces path with data (mode 0600) through a temporary file of its own, so a reader
// never sees a partial file and two writers cannot truncate each other's.
func WriteFileAtomic(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, werr := f.Write(data)
	cerr := f.Close()
	if werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Chmod(tmp, 0o600)
	}
	if werr == nil {
		werr = os.Rename(tmp, path)
	}
	if werr != nil {
		_ = os.Remove(tmp)
	}
	return werr
}

// Roots are the trusted fleet roots in fleet.crt (more than one while a re-issue is under way).
func Roots(dir string) ([]*x509.Certificate, error) {
	b, err := os.ReadFile(PathIn(dir, CertFile))
	if err != nil {
		return nil, err
	}
	return ParseCerts(b)
}

// NewFleet starts a fleet on this machine: a root that signs this machine's certificate and is then
// discarded, so no machine ever holds a key that can mint identities.
func NewFleet(dir, machineID string) error {
	rootPEM, rootKey, err := NewRoot(time.Now().UTC().Format("2006-01-02"))
	if err != nil {
		return err
	}
	root, err := ParseCerts(rootPEM)
	if err != nil {
		return err
	}
	key, keyPEM, err := NewMachineKey()
	if err != nil {
		return err
	}
	der, err := IssueMachineCert(root[0], rootKey, machineID, &key.PublicKey)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(PathIn(dir, ""), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(PathIn(dir, CertFile), rootPEM, 0o600); err != nil {
		return err
	}
	return WriteMachine(dir, keyPEM, certsPEM(der))
}

// Migrate gives a machine of a fleet from before 0.3.23 its own identity, offline: a new key with a
// certificate signed by the old shared key, which is the root every member already trusts. Older
// daemons accept the result as they did the shared certificate. It does nothing when the machine
// already has an identity, and reports whether it made one.
func Migrate(dir, machineID string) (bool, error) {
	if HasMachine(dir) {
		return false, nil
	}
	rootPEM, err := os.ReadFile(PathIn(dir, CertFile))
	if err != nil {
		return false, err
	}
	rootKeyPEM, err := os.ReadFile(PathIn(dir, KeyFile))
	if err != nil {
		return false, fmt.Errorf("no machine certificate and no fleet key to make one with: %w", err)
	}
	roots, err := ParseCerts(rootPEM)
	if err != nil {
		return false, err
	}
	rootKey, err := parseKeyPEM(rootKeyPEM)
	if err != nil {
		return false, err
	}
	key, keyPEM, err := NewMachineKey()
	if err != nil {
		return false, err
	}
	der, err := IssueMachineCert(roots[0], rootKey, machineID, &key.PublicKey)
	if err != nil {
		return false, err
	}
	return true, WriteMachine(dir, keyPEM, certsPEM(der))
}

// RetireFleetKey deletes the old shared key once every member has its own identity.
func RetireFleetKey(dir string) error {
	err := os.Remove(PathIn(dir, KeyFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// PublicKeyPEM and ParsePublicKeyPEM carry a joiner's public key in a join request.
func PublicKeyPEM(pub crypto.PublicKey) ([]byte, error) {
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}), nil
}

func ParsePublicKeyPEM(b []byte) (crypto.PublicKey, error) {
	blk, _ := pem.Decode(b)
	if blk == nil || blk.Type != "PUBLIC KEY" {
		return nil, errors.New("not a PEM public key")
	}
	return x509.ParsePKIXPublicKey(blk.Bytes)
}
