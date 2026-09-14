package ca

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/phoban01/flintlock-runner/internal/config"
	"github.com/phoban01/flintlock-runner/internal/fleet"
)

// File modes. Private keys and the directory holding them are owner-only
// (SE-023); certificates are public.
const (
	dirMode  fs.FileMode = 0o700
	keyMode  fs.FileMode = 0o600
	certMode fs.FileMode = 0o644
)

// Validity periods of generated material, and how close to expiry a Host
// certificate may be before Ensure replaces it.
const (
	caValidity    = 10 * 365 * 24 * time.Hour
	hostValidity  = 2 * 365 * 24 * time.Hour
	renewalWindow = 30 * 24 * time.Hour
)

// File names under the directory given to Ensure.
const (
	caCertName = "ca.pem"
	caKeyName  = "ca-key.pem"
	hostsDir   = "hosts"
)

// Authority is the fleet.CertificateAuthority.
type Authority struct {
	// Supplied is fleet.flintlockd.tls. When its three files are set they
	// are used as given and nothing is generated.
	Supplied config.ServerTLSFiles
	// Overrides is fleet.endpoint_overrides: an instance whose Host address
	// is overridden gets that name or address in its certificate too, since
	// the Runner dials it by that address (FL-005).
	Overrides map[string]string
	// Now is the clock for validity periods; nil means time.Now.
	Now func() time.Time
}

var _ fleet.CertificateAuthority = Authority{}

func (a Authority) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

func (a Authority) supplied() bool {
	return a.Supplied.CAFile != "" && a.Supplied.CertFile != "" && a.Supplied.KeyFile != ""
}

//= docs/requirements/09-security.md#transport-security
//# The Fleet Controller SHALL generate a certificate authority and
//# per-Host certificates when none are supplied and SHALL store the private
//# keys with owner-only permissions on the Control Node.

// Ensure returns the TLS material for hosts. With supplied files every Host
// gets the supplied certificate and key and the supplied CA is trusted.
// Otherwise a CA is kept under dir, created on first use and reused after so
// that Hosts provisioned earlier stay trusted, and every Host gets its own
// certificate signed by it, valid for its private address, its endpoint
// override and its instance id. A Host certificate is reused while it is
// signed by the CA, names the same addresses and is not about to expire.
// dir is owner-only and every private key is written with mode 0600.
func (a Authority) Ensure(ctx context.Context, dir string, hosts []fleet.Instance) (*fleet.CertBundle, error) {
	if a.supplied() {
		for _, f := range []string{a.Supplied.CAFile, a.Supplied.CertFile, a.Supplied.KeyFile} {
			if _, err := os.Stat(f); err != nil {
				return nil, fmt.Errorf("ca: supplied TLS material: %w", err)
			}
		}
		b := &fleet.CertBundle{CAFile: a.Supplied.CAFile, Hosts: map[string]fleet.HostCert{}}
		for _, h := range hosts {
			b.Hosts[h.ID] = fleet.HostCert{CertFile: a.Supplied.CertFile, KeyFile: a.Supplied.KeyFile}
		}
		return b, nil
	}
	if err := ownerOnlyDir(dir); err != nil {
		return nil, err
	}
	if err := ownerOnlyDir(filepath.Join(dir, hostsDir)); err != nil {
		return nil, err
	}
	caCert, caKey, err := a.loadOrCreateCA(dir)
	if err != nil {
		return nil, err
	}
	b := &fleet.CertBundle{CAFile: filepath.Join(dir, caCertName), Hosts: map[string]fleet.HostCert{}}
	for _, h := range hosts {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		hc, err := a.ensureHost(dir, h, caCert, caKey)
		if err != nil {
			return nil, err
		}
		b.Hosts[h.ID] = hc
	}
	return b, nil
}

// ownerOnlyDir creates dir, or tightens an existing one, to mode 0700.
func ownerOnlyDir(dir string) error {
	if err := os.MkdirAll(dir, dirMode); err != nil {
		return fmt.Errorf("ca: create %s: %w", dir, err)
	}
	if err := os.Chmod(dir, dirMode); err != nil {
		return fmt.Errorf("ca: chmod %s: %w", dir, err)
	}
	return nil
}

// loadOrCreateCA reads the CA under dir or generates one.
func (a Authority) loadOrCreateCA(dir string) (*x509.Certificate, crypto.Signer, error) {
	certPath, keyPath := filepath.Join(dir, caCertName), filepath.Join(dir, caKeyName)
	cert, key, err := loadPair(certPath, keyPath)
	if err == nil {
		// A key left readable by an earlier version or by hand is tightened.
		if err := os.Chmod(keyPath, keyMode); err != nil {
			return nil, nil, fmt.Errorf("ca: chmod %s: %w", keyPath, err)
		}
		return cert, key, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return nil, nil, fmt.Errorf("ca: existing CA under %s: %w", dir, err)
	}
	newKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("ca: generate CA key: %w", err)
	}
	now := a.now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial(),
		Subject:               pkix.Name{CommonName: "flintlock-runner fleet CA"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(caValidity),
		IsCA:                  true,
		BasicConstraintsValid: true,
		MaxPathLenZero:        true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, newKey.Public(), newKey)
	if err != nil {
		return nil, nil, fmt.Errorf("ca: create CA certificate: %w", err)
	}
	if err := writePair(certPath, keyPath, der, newKey); err != nil {
		return nil, nil, err
	}
	cert, err = x509.ParseCertificate(der)
	if err != nil {
		return nil, nil, fmt.Errorf("ca: parse CA certificate: %w", err)
	}
	return cert, newKey, nil
}

// sans is the addresses a Host certificate has to name.
func (a Authority) sans(h fleet.Instance) (ips []net.IP, names []string) {
	add := func(s string) {
		if s == "" {
			return
		}
		if ip := net.ParseIP(s); ip != nil {
			if !slices.ContainsFunc(ips, ip.Equal) {
				ips = append(ips, ip)
			}
		} else if !slices.Contains(names, s) {
			names = append(names, s)
		}
	}
	add(h.PrivateIP)
	add(a.Overrides[h.ID])
	add(h.ID)
	return ips, names
}

// ensureHost returns the Host's certificate, reusing the one on disk when it
// is still good.
func (a Authority) ensureHost(dir string, h fleet.Instance, caCert *x509.Certificate, caKey crypto.Signer) (fleet.HostCert, error) {
	base := fileName(h.ID)
	certPath := filepath.Join(dir, hostsDir, base+".pem")
	keyPath := filepath.Join(dir, hostsDir, base+"-key.pem")
	out := fleet.HostCert{CertFile: certPath, KeyFile: keyPath}
	ips, names := a.sans(h)

	if cert, _, err := loadPair(certPath, keyPath); err == nil && a.stillGood(cert, caCert, ips, names) {
		if err := os.Chmod(keyPath, keyMode); err != nil {
			return out, fmt.Errorf("ca: chmod %s: %w", keyPath, err)
		}
		return out, nil
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return out, fmt.Errorf("ca: generate key for %s: %w", h.ID, err)
	}
	now := a.now()
	tmpl := &x509.Certificate{
		SerialNumber: serial(),
		Subject:      pkix.Name{CommonName: h.ID},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(hostValidity),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  ips,
		DNSNames:     names,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, key.Public(), caKey)
	if err != nil {
		return out, fmt.Errorf("ca: create certificate for %s: %w", h.ID, err)
	}
	if err := writePair(certPath, keyPath, der, key); err != nil {
		return out, err
	}
	return out, nil
}

// stillGood reports whether an existing Host certificate can be kept.
func (a Authority) stillGood(cert, caCert *x509.Certificate, ips []net.IP, names []string) bool {
	if cert.CheckSignatureFrom(caCert) != nil {
		return false
	}
	if a.now().Add(renewalWindow).After(cert.NotAfter) {
		return false
	}
	if len(cert.IPAddresses) != len(ips) || !slices.Equal(cert.DNSNames, names) {
		return false
	}
	for i := range ips {
		if !cert.IPAddresses[i].Equal(ips[i]) {
			return false
		}
	}
	return true
}

// writePair writes a certificate and its private key; the key is created
// with mode 0600 before any byte of it is written.
func writePair(certPath, keyPath string, der []byte, key *ecdsa.PrivateKey) error {
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return fmt.Errorf("ca: marshal key: %w", err)
	}
	if err := writeFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), keyMode); err != nil {
		return err
	}
	return writeFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), certMode)
}

// writeFile writes data through a temporary file created with mode and
// renamed over path, so a reader never sees a partial file and a key never
// exists with wider permissions.
func writeFile(path string, data []byte, mode fs.FileMode) (err error) {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return fmt.Errorf("ca: write %s: %w", path, err)
	}
	defer func() {
		if err != nil {
			_ = os.Remove(tmp.Name())
		}
	}()
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("ca: write %s: %w", path, err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("ca: write %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("ca: write %s: %w", path, err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("ca: write %s: %w", path, err)
	}
	return nil
}

// loadPair reads a PEM certificate and PKCS#8 key. A missing file is
// reported with fs.ErrNotExist.
func loadPair(certPath, keyPath string) (*x509.Certificate, crypto.Signer, error) {
	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		return nil, nil, err
	}
	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, nil, err
	}
	cb, _ := pem.Decode(certPEM)
	if cb == nil || cb.Type != "CERTIFICATE" {
		return nil, nil, fmt.Errorf("%s: no PEM certificate", certPath)
	}
	cert, err := x509.ParseCertificate(cb.Bytes)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %w", certPath, err)
	}
	kb, _ := pem.Decode(keyPEM)
	if kb == nil {
		return nil, nil, fmt.Errorf("%s: no PEM key", keyPath)
	}
	parsed, err := x509.ParsePKCS8PrivateKey(kb.Bytes)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %w", keyPath, err)
	}
	signer, ok := parsed.(crypto.Signer)
	if !ok {
		return nil, nil, fmt.Errorf("%s: unsupported key type %T", keyPath, parsed)
	}
	if pub, ok := signer.Public().(interface{ Equal(crypto.PublicKey) bool }); !ok || !pub.Equal(cert.PublicKey) {
		return nil, nil, fmt.Errorf("%s does not match %s", keyPath, certPath)
	}
	return cert, signer, nil
}

// serial is a random 128-bit certificate serial number.
func serial() *big.Int {
	n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		// crypto/rand does not fail on supported platforms.
		panic(err)
	}
	return n
}

// fileName makes an instance id safe as a file name.
func fileName(id string) string {
	var b bytes.Buffer
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	s := strings.TrimLeft(b.String(), ".")
	if s == "" {
		s = "host"
	}
	return s
}
