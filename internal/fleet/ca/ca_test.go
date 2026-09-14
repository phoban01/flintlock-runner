package ca

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/phoban01/flintlock-runner/internal/config"
	"github.com/phoban01/flintlock-runner/internal/fleet"
)

var hosts = []fleet.Instance{
	{ID: "i-0aaa", PrivateIP: "10.0.1.10"},
	{ID: "i-0bbb", PrivateIP: "10.0.1.11"},
}

func mode(t *testing.T, path string) os.FileMode {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Mode().Perm()
}

func readCert(t *testing.T, path string) *x509.Certificate {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := pem.Decode(data)
	if b == nil {
		t.Fatalf("%s: no PEM", path)
	}
	c, err := x509.ParseCertificate(b.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

//= docs/requirements/09-security.md#transport-security
//= type=test
//# The Fleet Controller SHALL generate a certificate authority and
//# per-Host certificates when none are supplied and SHALL store the private
//# keys with owner-only permissions on the Control Node.

func TestEnsureGeneratesCAAndHostCertsWithOwnerOnlyKeys(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "tls")
	b, err := Authority{}.Ensure(context.Background(), dir, hosts)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if b.CAFile != filepath.Join(dir, caCertName) {
		t.Errorf("CAFile = %q", b.CAFile)
	}
	if got := mode(t, filepath.Join(dir, caKeyName)); got != 0o600 {
		t.Errorf("CA key mode = %v, want 0600", got)
	}
	if got := mode(t, dir); got != 0o700 {
		t.Errorf("directory mode = %v, want 0700", got)
	}
	caCert := readCert(t, b.CAFile)
	if !caCert.IsCA {
		t.Error("the CA certificate is not a CA")
	}
	roots := x509.NewCertPool()
	roots.AddCert(caCert)
	if len(b.Hosts) != len(hosts) {
		t.Fatalf("host certificates = %d, want %d", len(b.Hosts), len(hosts))
	}
	for _, h := range hosts {
		hc, ok := b.Hosts[h.ID]
		if !ok {
			t.Fatalf("no certificate for %s", h.ID)
		}
		if got := mode(t, hc.KeyFile); got != 0o600 {
			t.Errorf("%s key mode = %v, want 0600", h.ID, got)
		}
		cert := readCert(t, hc.CertFile)
		if _, err := cert.Verify(x509.VerifyOptions{Roots: roots, DNSName: h.PrivateIP}); err != nil {
			t.Errorf("%s certificate does not verify for %s against the CA: %v", h.ID, h.PrivateIP, err)
		}
	}
	if b.Hosts[hosts[0].ID].CertFile == b.Hosts[hosts[1].ID].CertFile {
		t.Error("two Hosts share one certificate; SE-023 asks for per-Host certificates")
	}
}

func TestEnsureTightensLooseKeyPermissions(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	b, err := Authority{}.Ensure(context.Background(), dir, hosts[:1])
	if err != nil {
		t.Fatal(err)
	}
	key := b.Hosts[hosts[0].ID].KeyFile
	if err := os.Chmod(key, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(dir, caKeyName), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := (Authority{}).Ensure(context.Background(), dir, hosts[:1]); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{key, filepath.Join(dir, caKeyName)} {
		if got := mode(t, p); got != 0o600 {
			t.Errorf("%s mode = %v after Ensure, want 0600", p, got)
		}
	}
}

func TestEnsureReusesCAAndHostCerts(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	first, err := Authority{}.Ensure(context.Background(), dir, hosts[:1])
	if err != nil {
		t.Fatal(err)
	}
	caBefore, _ := os.ReadFile(first.CAFile)
	hostBefore, _ := os.ReadFile(first.Hosts[hosts[0].ID].CertFile)

	// A later run with a new Host keeps the CA every earlier Host trusts
	// and the earlier Host's certificate.
	second, err := Authority{}.Ensure(context.Background(), dir, hosts)
	if err != nil {
		t.Fatal(err)
	}
	caAfter, _ := os.ReadFile(second.CAFile)
	hostAfter, _ := os.ReadFile(second.Hosts[hosts[0].ID].CertFile)
	if !bytes.Equal(caBefore, caAfter) {
		t.Error("the CA was regenerated; every Host provisioned earlier would stop being trusted")
	}
	if !bytes.Equal(hostBefore, hostAfter) {
		t.Error("an unchanged Host's certificate was regenerated")
	}

	// A Host whose address changed gets a new certificate.
	moved := []fleet.Instance{{ID: hosts[0].ID, PrivateIP: "10.0.9.9"}}
	third, err := Authority{}.Ensure(context.Background(), dir, moved)
	if err != nil {
		t.Fatal(err)
	}
	cert := readCert(t, third.Hosts[hosts[0].ID].CertFile)
	if len(cert.IPAddresses) != 1 || cert.IPAddresses[0].String() != "10.0.9.9" {
		t.Errorf("moved Host certificate names %v, want 10.0.9.9", cert.IPAddresses)
	}
}

func TestEnsureRenewsNearExpiry(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	first, err := Authority{Now: func() time.Time { return start }}.Ensure(context.Background(), dir, hosts[:1])
	if err != nil {
		t.Fatal(err)
	}
	before := readCert(t, first.Hosts[hosts[0].ID].CertFile)
	later := before.NotAfter.Add(-renewalWindow / 2)
	second, err := Authority{Now: func() time.Time { return later }}.Ensure(context.Background(), dir, hosts[:1])
	if err != nil {
		t.Fatal(err)
	}
	after := readCert(t, second.Hosts[hosts[0].ID].CertFile)
	if !after.NotAfter.After(before.NotAfter) {
		t.Error("a certificate inside the renewal window was not renewed")
	}
}

func TestEnsureUsesSuppliedFilesAndGeneratesNothing(t *testing.T) {
	t.Parallel()
	src := t.TempDir()
	supplied := config.ServerTLSFiles{
		CAFile:   filepath.Join(src, "ca.pem"),
		CertFile: filepath.Join(src, "host.pem"),
		KeyFile:  filepath.Join(src, "host-key.pem"),
	}
	for _, f := range []string{supplied.CAFile, supplied.CertFile, supplied.KeyFile} {
		if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	dir := filepath.Join(t.TempDir(), "tls")
	b, err := Authority{Supplied: supplied}.Ensure(context.Background(), dir, hosts)
	if err != nil {
		t.Fatal(err)
	}
	if b.CAFile != supplied.CAFile {
		t.Errorf("CAFile = %q, want the supplied %q", b.CAFile, supplied.CAFile)
	}
	for _, h := range hosts {
		if got := b.Hosts[h.ID]; got.CertFile != supplied.CertFile || got.KeyFile != supplied.KeyFile {
			t.Errorf("%s = %+v, want the supplied files", h.ID, got)
		}
	}
	if _, err := os.Stat(dir); err == nil {
		t.Error("material was generated although it was supplied")
	}

	missing := supplied
	missing.KeyFile = filepath.Join(src, "absent.pem")
	if _, err := (Authority{Supplied: missing}).Ensure(context.Background(), dir, hosts); err == nil {
		t.Error("a supplied file that does not exist was accepted")
	}
}

func TestGeneratedCertificatesServeTLS(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	host := fleet.Instance{ID: "i-loop", PrivateIP: "127.0.0.1"}
	a := Authority{Overrides: map[string]string{"i-loop": "flintlockd.internal"}}
	b, err := a.Ensure(context.Background(), dir, []fleet.Instance{host})
	if err != nil {
		t.Fatal(err)
	}
	pair, err := tls.LoadX509KeyPair(b.Hosts[host.ID].CertFile, b.Hosts[host.ID].KeyFile)
	if err != nil {
		t.Fatalf("the generated pair does not load: %v", err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(readCert(t, b.CAFile))
	for _, name := range []string{"127.0.0.1", "flintlockd.internal"} {
		leaf, err := x509.ParseCertificate(pair.Certificate[0])
		if err != nil {
			t.Fatal(err)
		}
		if _, err := leaf.Verify(x509.VerifyOptions{Roots: roots, DNSName: name, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err != nil {
			t.Errorf("certificate does not serve %s: %v", name, err)
		}
	}
}

func TestFileNameSanitises(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{"i-0abc": "i-0abc", "../etc/passwd": "_etc_passwd", "": "host", "a/b c": "a_b_c"} {
		if got := fileName(in); got != want {
			t.Errorf("fileName(%q) = %q, want %q", in, got, want)
		}
	}
}
