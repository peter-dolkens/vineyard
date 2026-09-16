// Package mesh implements the daemon's networking: one TLS listener that accepts peers and viewers,
// on-demand outbound connections to peers, and the aggregated fleet view handed to viewers.
package mesh

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"os"

	"github.com/peter-dolkens/vineyard/daemon/internal/config"
)

// FleetTLS returns server and client configurations that both present the shared fleet certificate
// and accept only that same certificate from the other side. Anyone without the key is rejected in
// the handshake; anyone with it is a fleet member.
func FleetTLS() (server, client *tls.Config, err error) {
	certPEM, err := os.ReadFile(config.Path(config.CertFile))
	if err != nil {
		return nil, nil, err
	}
	keyPEM, err := os.ReadFile(config.Path(config.KeyFile))
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
	server = &tls.Config{
		Certificates: []tls.Certificate{cert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    pool,
		MinVersion:   tls.VersionTLS13,
	}
	// lenientFor returns a copy that tolerates a missing client certificate; used only while an
	// invite is outstanding so a joiner can present its token. A presented certificate is still verified.
	_ = lenientFor
	client = &tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      pool,
		ServerName:   config.FleetServerName,
		MinVersion:   tls.VersionTLS13,
	}
	return server, client, nil
}

func lenientFor(strict *tls.Config) *tls.Config {
	c := strict.Clone()
	c.ClientAuth = tls.VerifyClientCertIfGiven
	return c
}
