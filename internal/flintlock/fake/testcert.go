package fake

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/phoban01/flintlock-runner/internal/flintlock"
)

// TestCerts are the files WriteTestCerts produces: a CA, a server
// certificate signed by it for the loopback addresses, and a client
// certificate signed by it for mutual TLS. Each path is under Dir. They are
// what a test or the harness feeds to flintlock.ServerTLS on the Host side
// and flintlock.TLSOptions on the Runner side (TD-024, HO-005, HO-006).
type TestCerts struct {
	Dir            string
	CAFile         string
	ServerCertFile string
	ServerKeyFile  string
	ClientCertFile string
	ClientKeyFile  string
}

// ServerTLS returns the Host-side material: the server certificate, and
// client certificate verification when mutual is set.
func (c *TestCerts) ServerTLS(mutual bool) *flintlock.ServerTLS {
	t := &flintlock.ServerTLS{CertFile: c.ServerCertFile, KeyFile: c.ServerKeyFile}
	if mutual {
		t.ClientCAFile = c.CAFile
	}
	return t
}

// certValidity is how long the generated certificates are valid. Tests do
// not run for a day; anything longer only invites reuse outside them.
const certValidity = 24 * time.Hour

// WriteTestCerts generates a throwaway certificate authority under dir and
// writes a server certificate valid for 127.0.0.1, ::1, localhost and every
// name in hosts, plus a client certificate, all PEM-encoded. The keys are
// ECDSA P-256. It takes no testing.T so that the end-to-end harness can use
// it as well as unit tests; callers pass t.TempDir() or clean up dir.
func WriteTestCerts(dir string, hosts ...string) (*TestCerts, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("creating cert dir: %w", err)
	}
	notBefore := time.Now().Add(-time.Minute)
	notAfter := notBefore.Add(certValidity)

	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generating CA key: %w", err)
	}
	caTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "flintlock-runner fake host test CA"},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		return nil, fmt.Errorf("creating CA certificate: %w", err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		return nil, fmt.Errorf("parsing CA certificate: %w", err)
	}

	serverTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "fake flintlock host"},
		NotBefore:    notBefore,
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback},
		DNSNames:     append([]string{"localhost"}, hosts...),
	}
	clientTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(3),
		Subject:      pkix.Name{CommonName: "flintlock-runner"},
		NotBefore:    notBefore,
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}

	certs := &TestCerts{
		Dir:            dir,
		CAFile:         filepath.Join(dir, "ca.pem"),
		ServerCertFile: filepath.Join(dir, "server.pem"),
		ServerKeyFile:  filepath.Join(dir, "server-key.pem"),
		ClientCertFile: filepath.Join(dir, "client.pem"),
		ClientKeyFile:  filepath.Join(dir, "client-key.pem"),
	}
	if err := writePEM(certs.CAFile, "CERTIFICATE", caDER); err != nil {
		return nil, err
	}
	if err := issue(serverTemplate, caCert, caKey, certs.ServerCertFile, certs.ServerKeyFile); err != nil {
		return nil, fmt.Errorf("server certificate: %w", err)
	}
	if err := issue(clientTemplate, caCert, caKey, certs.ClientCertFile, certs.ClientKeyFile); err != nil {
		return nil, fmt.Errorf("client certificate: %w", err)
	}
	return certs, nil
}

// issue generates a key, signs template with the CA and writes both files.
func issue(template, ca *x509.Certificate, caKey *ecdsa.PrivateKey, certFile, keyFile string) error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return fmt.Errorf("generating key: %w", err)
	}
	der, err := x509.CreateCertificate(rand.Reader, template, ca, &key.PublicKey, caKey)
	if err != nil {
		return fmt.Errorf("signing: %w", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return fmt.Errorf("marshalling key: %w", err)
	}
	if err := writePEM(certFile, "CERTIFICATE", der); err != nil {
		return err
	}
	return writePEM(keyFile, "EC PRIVATE KEY", keyDER)
}

// writePEM writes one PEM block to path, readable by the owner only.
func writePEM(path, blockType string, der []byte) error {
	data := pem.EncodeToMemory(&pem.Block{Type: blockType, Bytes: der})
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}
