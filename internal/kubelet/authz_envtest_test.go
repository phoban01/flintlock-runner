package kubelet

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/phoban01/flintlock-runner/internal/flintlock"
	hostfake "github.com/phoban01/flintlock-runner/internal/flintlock/fake"
)

// identityCA is a kubelet client certificate authority of the test's own,
// which issues the provider's serving certificate and client certificates
// for whatever identity a scenario needs: the fake Host's WriteTestCerts
// issues only one.
type identityCA struct {
	t    *testing.T
	dir  string
	file string
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	seq  atomic.Int64
}

func newIdentityCA(t *testing.T) *identityCA {
	t.Helper()
	ca := &identityCA{t: t, dir: t.TempDir()}
	ca.key = ca.newKey()
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "kubelet client test CA"},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &ca.key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	if ca.cert, err = x509.ParseCertificate(der); err != nil {
		t.Fatal(err)
	}
	ca.file = filepath.Join(ca.dir, "ca.pem")
	ca.write(ca.file, "CERTIFICATE", der)
	return ca
}

func (ca *identityCA) newKey() *ecdsa.PrivateKey {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		ca.t.Fatal(err)
	}
	return key
}

func (ca *identityCA) write(path, blockType string, der []byte) {
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: blockType, Bytes: der}), 0o600); err != nil {
		ca.t.Fatal(err)
	}
}

// issue signs a certificate for template and returns its files.
func (ca *identityCA) issue(template *x509.Certificate) (certFile, keyFile string) {
	n := ca.seq.Add(1)
	template.SerialNumber = big.NewInt(n + 1)
	template.NotBefore, template.NotAfter = time.Now().Add(-time.Minute), time.Now().Add(time.Hour)
	key := ca.newKey()
	der, err := x509.CreateCertificate(rand.Reader, template, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		ca.t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		ca.t.Fatal(err)
	}
	certFile = filepath.Join(ca.dir, "cert-"+big.NewInt(n).String()+".pem")
	keyFile = filepath.Join(ca.dir, "key-"+big.NewInt(n).String()+".pem")
	ca.write(certFile, "CERTIFICATE", der)
	ca.write(keyFile, "EC PRIVATE KEY", keyDER)
	return certFile, keyFile
}

// serving is the provider's serving material, for a fixture's certs and
// configuration.
func (ca *identityCA) serving() *hostfake.TestCerts {
	certFile, keyFile := ca.issue(&x509.Certificate{
		Subject:     pkix.Name{CommonName: "virtual kubelet"},
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback},
		DNSNames:    []string{"localhost"},
	})
	return &hostfake.TestCerts{Dir: ca.dir, CAFile: ca.file, ServerCertFile: certFile, ServerKeyFile: keyFile}
}

// client is a client certificate whose identity is user in groups, in the
// shape fixture.exec takes.
func (ca *identityCA) client(user string, groups ...string) *hostfake.TestCerts {
	certFile, keyFile := ca.issue(&x509.Certificate{
		Subject:     pkix.Name{CommonName: user, Organization: groups},
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	})
	return &hostfake.TestCerts{Dir: ca.dir, CAFile: ca.file, ClientCertFile: certFile, ClientKeyFile: keyFile}
}

// grant gives group the verbs on nodes/proxy of the named node only.
func grant(t *testing.T, name, group, node string, verbs ...string) {
	t.Helper()
	ctx := context.Background()
	role := &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Rules:      []rbacv1.PolicyRule{{APIGroups: []string{""}, Resources: []string{"nodes/proxy"}, ResourceNames: []string{node}, Verbs: verbs}},
	}
	if _, err := suite.Admin.RbacV1().ClusterRoles().Create(ctx, role, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	binding := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: name},
		Subjects:   []rbacv1.Subject{{APIGroup: rbacv1.GroupName, Kind: rbacv1.GroupKind, Name: group}},
	}
	if _, err := suite.Admin.RbacV1().ClusterRoleBindings().Create(ctx, binding, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
}

//= docs/requirements/12-cluster-fleet.md#cluster-test-doubles
//= type=test
//# The Pod Provider's authorization SHALL be tested against a
//# Kubernetes API server test environment with one client identity that the
//# review allows and one that it refuses, and SHALL be shown to run nothing
//# for the second.
//
//= docs/requirements/12-cluster-fleet.md#cluster-hardening
//= type=test
//# The Pod Provider SHALL authorize every kubelet API request with
//# a SubjectAccessReview of the requesting identity for the `nodes/proxy`
//# resource on its Virtual Node and the verb the request maps to, and SHALL
//# refuse the request unless the review allows it.
//
//= docs/requirements/12-cluster-fleet.md#cluster-hardening
//= type=test
//# The Fleet Manifests SHALL grant the Pod Provider permission to
//# create SubjectAccessReviews in addition to the permissions of KF-111.

// TestKubeletAPIRunsNothingForAnIdentityTheReviewRefuses runs the provider as
// the Host Agent's ServiceAccount under the shipped RBAC, so its reviews are
// real SubjectAccessReviews answered by the API server's RBAC authorizer. All
// the client certificates are valid and from the configured authority: only
// the review tells them apart.
func TestKubeletAPIRunsNothingForAnIdentityTheReviewRefuses(t *testing.T) {
	t.Parallel()
	f := newFixture(t, flintlock.FakeHostConfig{})
	ca := newIdentityCA(t)
	f.certs = ca.serving()
	f.cfg.TLS = ServerTLS{CertFile: f.certs.ServerCertFile, KeyFile: f.certs.ServerKeyFile, ClientCAFile: ca.file}

	execers, readers, elsewhere := "execers-"+f.hostNode, "readers-"+f.hostNode, "elsewhere-"+f.hostNode
	grant(t, execers, execers, f.vnode, "create", "get")
	grant(t, readers, readers, f.vnode, "get")
	grant(t, elsewhere, elsewhere, VirtualNodeName("another-"+f.hostNode), "create", "get")

	f.start()
	pod := f.createPod("job")
	f.waitRunning(pod.Name)
	sandbox, _ := f.fake.SandboxPath(f.microVMOf(pod).GetSpec().GetUid())
	ran := func(marker string) bool {
		_, err := os.Stat(filepath.Join(sandbox, marker))
		return err == nil
	}

	allowed := ca.client("apiserver-"+f.hostNode, execers)
	refused := ca.client("stranger-"+f.hostNode, "system:nodes")
	reader := ca.client("reader-"+f.hostNode, readers)
	other := ca.client("other-host-client-"+f.hostNode, elsewhere)

	// The identity the review refuses: its exec is refused and nothing runs
	// in the guest.
	if _, _, err := f.exec(pod.Name, refused, nil, "touch", "refused"); err == nil || !strings.Contains(err.Error(), "Forbidden") {
		t.Errorf("exec by an identity with no grant: %v, want forbidden", err)
	}
	// An identity that may only read the node may not open a shell, over
	// POST or over the GET a websocket upgrade uses.
	if _, _, err := f.exec(pod.Name, reader, nil, "touch", "reader"); err == nil {
		t.Error("exec by an identity that may only get nodes/proxy succeeded")
	}
	// An identity granted another node's proxy is not granted this one's.
	if _, _, err := f.exec(pod.Name, other, nil, "touch", "other"); err == nil {
		t.Error("exec by an identity granted another node succeeded")
	}
	for _, marker := range []string{"refused", "reader", "other"} {
		if ran(marker) {
			t.Errorf("the refused exec %q ran its command in the guest", marker)
		}
	}

	get := func(certs *hostfake.TestCerts, path string) int {
		client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{TLSClientConfig: clientTLS(t, f.certs, certs)}}
		resp, err := client.Get("https://" + f.addr + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	execPath := "/exec/" + f.namespace + "/" + pod.Name + "/microvm?command=touch&command=websocket&output=1"
	for name, tc := range map[string]struct {
		certs *hostfake.TestCerts
		path  string
		want  int
	}{
		"GET /pods by the refused identity":           {refused, "/pods", http.StatusForbidden},
		"GET /pods by the reader":                     {reader, "/pods", http.StatusOK},
		"GET /pods by the identity allowed to exec":   {allowed, "/pods", http.StatusOK},
		"GET /exec by the reader (websocket upgrade)": {reader, execPath, http.StatusForbidden},
		"GET /exec by the refused identity":           {refused, execPath, http.StatusForbidden},
	} {
		if got := get(tc.certs, tc.path); got != tc.want {
			t.Errorf("%s: status %d, want %d", name, got, tc.want)
		}
	}
	if ran("websocket") {
		t.Error("a refused GET exec ran its command in the guest")
	}

	// The identity the review allows runs its command.
	if code, _, err := f.exec(pod.Name, allowed, nil, "touch", "allowed"); err != nil || code != 0 || !ran("allowed") {
		t.Errorf("exec by the allowed identity: exit %d, %v, ran %v", code, err, ran("allowed"))
	}
	if !strings.Contains(f.logs.String(), "refused a kubelet API request") {
		t.Error("the provider did not log its refusals")
	}
}
