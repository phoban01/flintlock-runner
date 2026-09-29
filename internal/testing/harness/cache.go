package harness

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"time"

	hostfake "github.com/phoban01/flintlock-runner/internal/flintlock/fake"
	"github.com/phoban01/flintlock-runner/internal/testing/fakes3"
)

// The distributed cache's bucket and region, and the secret key of the
// credentials the Runner's instance credentials endpoint hands out. A
// scenario checks that the secret key reaches no Job (CF-082).
const (
	CacheBucket    = "harness-cache"
	CacheRegion    = "us-east-1"
	CacheSecretKey = "harness-cache-secret-key"
	cacheAccessKey = "HARNESSCACHEACCESSKEY"
)

// envContainerCredentials is the variable that points the AWS credential
// chain at a loopback credentials endpoint, as a container's is. The
// Runner signs cache URLs with what it serves (CF-082).
const envContainerCredentials = "AWS_CONTAINER_CREDENTIALS_FULL_URI"

// cacheStore is the distributed cache of a Stack: the fake object store
// (TD-034) served over TLS, since gitlab-runner's instance-credentials
// client connects to the store over TLS only, and a credentials endpoint
// on loopback that stands in for the instance's.
type cacheStore struct {
	// store holds the objects.
	store *fakes3.Store
	// s3 serves store over TLS.
	s3 *httptest.Server
	// creds serves the credentials.
	creds *httptest.Server
	// caFile is the certificate authority of s3's certificate. A Job that
	// uses the cache trusts it through SSL_CERT_FILE, because the fake
	// Hosts run the helper on this machine.
	caFile string
}

// startCache serves the fake object store over TLS with a certificate for
// the loopback address, and the credentials endpoint.
func (s *Stack) startCache() error {
	certs, err := hostfake.WriteTestCerts(filepath.Join(s.Root, "cache-certs"))
	if err != nil {
		return fmt.Errorf("harness: cache certificates: %w", err)
	}
	cert, err := tls.LoadX509KeyPair(certs.ServerCertFile, certs.ServerKeyFile)
	if err != nil {
		return fmt.Errorf("harness: cache certificates: %w", err)
	}
	c := &cacheStore{store: fakes3.New(fakes3.Options{}), caFile: certs.CAFile}
	c.s3 = httptest.NewUnstartedServer(c.store.Handler())
	c.s3.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	c.s3.StartTLS()
	c.creds = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"AccessKeyId":     cacheAccessKey,
			"SecretAccessKey": CacheSecretKey,
			"Token":           "harness-session-token",
			"Expiration":      time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
			"Code":            "Success",
		})
	}))
	s.cache = c
	s.logf("fake object store serving https on %s, credentials on %s", c.s3.Listener.Addr(), c.creds.URL)
	return nil
}

// close stops the object store and the credentials endpoint.
func (c *cacheStore) close() {
	c.s3.Close()
	c.creds.Close()
}

// CacheEndpoint is the https endpoint of the fake object store, empty when
// the Stack has no cache.
func (s *Stack) CacheEndpoint() string {
	if s.cache == nil {
		return ""
	}
	return "https://" + s.cache.s3.Listener.Addr().String()
}

// CacheCA is the certificate authority file of the fake object store,
// empty when the Stack has no cache.
func (s *Stack) CacheCA() string {
	if s.cache == nil {
		return ""
	}
	return s.cache.caFile
}

// CacheObjects returns the objects in the fake object store, by
// "<bucket>/<key>".
func (s *Stack) CacheObjects() map[string][]byte {
	if s.cache == nil {
		return nil
	}
	return s.cache.store.Objects()
}

// runnerExtraEnv is the environment the Stack adds to the Runner's: the
// credentials endpoint when it has a distributed cache.
func (s *Stack) runnerExtraEnv() []string {
	if s.cache == nil {
		return nil
	}
	return []string{envContainerCredentials + "=" + s.cache.creds.URL + "/credentials"}
}
