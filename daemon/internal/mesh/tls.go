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

// fleetTLS builds the configurations from the files in dir. This machine presents its own
// certificate chain (machine.crt) when it has one, and the shared fleet certificate otherwise (a
// machine that could not migrate). Every root in fleet.crt is trusted, plus, during a pre-0.3.23 key
// rotation's grace period (config/keys.go), the previous certificate. Who the peer is, and whether
// its key is revoked, is checked after the handshake (identity.go).
func fleetTLS(dir string, prevUntil int64) (server, client *tls.Config, err error) {
	rootsPEM, err := os.ReadFile(config.PathIn(dir, config.CertFile))
	if err != nil {
		return nil, nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(rootsPEM) {
		return nil, nil, errors.New("fleet.crt is not a valid PEM certificate")
	}
	var cert tls.Certificate
	if config.HasMachine(dir) {
		m, err := config.LoadMachine(dir)
		if err != nil {
			return nil, nil, err
		}
		cert = m.TLS
	} else {
		keyPEM, err := os.ReadFile(config.PathIn(dir, config.KeyFile))
		if err != nil {
			return nil, nil, err
		}
		if cert, err = tls.X509KeyPair(rootsPEM, keyPEM); err != nil {
			return nil, nil, err
		}
		if inGrace(dir, prevUntil) {
			crossPEM, err := os.ReadFile(config.PathIn(dir, config.CrossCertFile))
			if err == nil {
				if cross, err := tls.X509KeyPair(crossPEM, keyPEM); err == nil {
					cert = cross
				}
			}
		}
	}
	if inGrace(dir, prevUntil) {
		if prevPEM, err := os.ReadFile(config.PathIn(dir, config.PrevCertFile)); err == nil {
			pool.AppendCertsFromPEM(prevPEM)
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
