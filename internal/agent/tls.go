package agent

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"sync"
	"time"
)

// servingCert serves the exec API's certificate and follows its files: a
// certificate renewed on disk is picked up at the next handshake, as long
// as it still names the agent's address. A renewal that does not, or that
// cannot be read, is logged and the certificate in use is kept.
type servingCert struct {
	files   ServerTLS
	address net.IP
	log     *slog.Logger

	mu       sync.Mutex
	cert     *tls.Certificate
	modified time.Time
}

//= docs/requirements/12-cluster-fleet.md#exec-agent
//# The Exec Agent SHALL serve its exec API over TLS on the Host's
//# internal address, with a serving certificate that names that address.

// newServingCert loads the serving certificate and refuses one that does
// not name address as an IP address: the Runner dials the Exec Agent by
// the Host's internal address and verifies the certificate against it
// (KF-186), so a certificate for any other name would fail every request,
// and failing at start says why.
func newServingCert(files ServerTLS, address string, log *slog.Logger) (*servingCert, error) {
	ip := net.ParseIP(address)
	if ip == nil {
		return nil, fmt.Errorf("agent: the exec API's address %q is not an IP address", address)
	}
	s := &servingCert{files: files, address: ip, log: log}
	cert, modified, err := s.load()
	if err != nil {
		return nil, err
	}
	s.cert, s.modified = cert, modified
	return s, nil
}

// load reads the certificate and key and checks the certificate names the
// address.
func (s *servingCert) load() (*tls.Certificate, time.Time, error) {
	info, err := os.Stat(s.files.CertFile)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("agent: reading the serving certificate: %w", err)
	}
	cert, err := tls.LoadX509KeyPair(s.files.CertFile, s.files.KeyFile)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("agent: loading the serving certificate: %w", err)
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("agent: parsing the serving certificate: %w", err)
	}
	if err := leaf.VerifyHostname(s.address.String()); err != nil {
		return nil, time.Time{}, fmt.Errorf("agent: the serving certificate does not name the exec API's address %s: %w", s.address, err)
	}
	cert.Leaf = leaf
	return &cert, info.ModTime(), nil
}

// get is tls.Config.GetCertificate.
func (s *servingCert) get(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if info, err := os.Stat(s.files.CertFile); err == nil && !info.ModTime().Equal(s.modified) {
		cert, modified, err := s.load()
		if err != nil {
			s.log.Warn("the renewed serving certificate cannot be used; keeping the one in use", "error", err)
			s.modified = info.ModTime()
		} else {
			s.cert, s.modified = cert, modified
		}
	}
	if s.cert == nil {
		return nil, errors.New("agent: no serving certificate")
	}
	return s.cert, nil
}

// tlsConfig is the TLS configuration of the exec API. Clients are not
// asked for certificates: they authenticate with a bearer token (KF-173).
func (s *servingCert) tlsConfig() *tls.Config {
	return &tls.Config{
		MinVersion:     tls.VersionTLS12,
		GetCertificate: s.get,
		NextProtos:     []string{"h2"},
	}
}
