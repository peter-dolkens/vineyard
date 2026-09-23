// Package mesh implements the daemon's networking: one TLS listener that accepts peers and viewers,
// on-demand outbound connections to peers, and the aggregated fleet view handed to viewers.
package mesh

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"os"
	"time"

	"github.com/peter-dolkens/vineyard/daemon/internal/config"
)

// FleetTLS returns server and client configurations that present the fleet certificate and accept
// only fleet certificates from the other side. Anyone without the key is rejected in the handshake;
// anyone with it is a fleet member. It honours a rotation grace period recorded in config.json.
func FleetTLS() (server, client *tls.Config, err error) {
	var prevUntil int64
	if cfg, err := config.Load(); err == nil {
		prevUntil = cfg.PrevUntil
	}
	return fleetTLS("", prevUntil)
}

// fleetTLS builds the configurations from the key files in dir. During a rotation grace period
// (prevUntil in the future, see config/keys.go) both sides present the cross certificate, which
// machines still on the previous key accept, and trust the previous certificate as well as the
// current one, so those machines are accepted too. Outside it, only the current key exists.
func fleetTLS(dir string, prevUntil int64) (server, client *tls.Config, err error) {
	certPEM, err := os.ReadFile(config.PathIn(dir, config.CertFile))
	if err != nil {
		return nil, nil, err
	}
	keyPEM, err := os.ReadFile(config.PathIn(dir, config.KeyFile))
	if err != nil {
		return nil, nil, err
	}
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(certPEM) {
		return nil, nil, errors.New("fleet.crt is not a valid PEM certificate")
	}
	if inGrace(dir, prevUntil) {
		prevPEM, err1 := os.ReadFile(config.PathIn(dir, config.PrevCertFile))
		crossPEM, err2 := os.ReadFile(config.PathIn(dir, config.CrossCertFile))
		if err1 == nil && err2 == nil {
			if cross, err := tls.X509KeyPair(crossPEM, keyPEM); err == nil && pool.AppendCertsFromPEM(prevPEM) {
				cert = cross
			}
		}
	}
	server = &tls.Config{
		Certificates: []tls.Certificate{cert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    pool,
		MinVersion:   tls.VersionTLS13,
	}
	client = &tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      pool,
		ServerName:   config.FleetServerName,
		MinVersion:   tls.VersionTLS13,
	}
	return server, client, nil
}

// inGrace reports whether a rotation grace period is running and its files are present.
func inGrace(dir string, prevUntil int64) bool {
	if prevUntil <= time.Now().UnixMilli() {
		return false
	}
	_, e1 := os.Stat(config.PathIn(dir, config.PrevCertFile))
	_, e2 := os.Stat(config.PathIn(dir, config.CrossCertFile))
	return e1 == nil && e2 == nil
}

// lenientFor returns a copy that tolerates a missing client certificate; used only while an invite
// is outstanding so a joiner can present its token. A presented certificate is still verified.
func lenientFor(strict *tls.Config) *tls.Config {
	c := strict.Clone()
	c.ClientAuth = tls.VerifyClientCertIfGiven
	return c
}
