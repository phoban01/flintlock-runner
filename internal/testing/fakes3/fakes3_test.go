package fakes3_test

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"gitlab.com/gitlab-org/gitlab-runner/cache"
	"gitlab.com/gitlab-org/gitlab-runner/cache/cacheconfig"
	_ "gitlab.com/gitlab-org/gitlab-runner/cache/s3" // registers the s3 adapter the Runner's cache config selects
	cmdhelpers "gitlab.com/gitlab-org/gitlab-runner/commands/helpers"
	runnerhelpers "gitlab.com/gitlab-org/gitlab-runner/helpers"

	"github.com/phoban01/flintlock-runner/internal/testing/fakes3"
)

func startStore(t *testing.T) *fakes3.Store {
	t.Helper()
	s := fakes3.New()
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(s.Close)
	return s
}

// cacheAdapter builds the real gitlab-runner S3 cache adapter against the
// store, the way the Runner does from its [runners.cache] configuration, so
// the pre-signed URLs under test are the ones the helper would receive.
func cacheAdapter(t *testing.T, s *fakes3.Store, key string) cache.Adapter {
	t.Helper()
	cfg := &cacheconfig.Config{
		Type: "s3",
		Path: "prefix",
		S3: &cacheconfig.CacheS3Config{
			ServerAddress:  s.Addr(),
			AccessKey:      "fake-access-key",
			SecretKey:      "fake-secret-key",
			BucketName:     "runner-cache",
			BucketLocation: "us-east-1",
			Insecure:       true,
		},
	}
	return cache.GetAdapter(cfg, time.Hour, "abcdefgh", "42", key, false)
}

func doRequest(t *testing.T, method, url string, header http.Header, body []byte) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), method, url, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	for k, v := range header {
		req.Header[k] = v
	}
	if method == http.MethodPut {
		// The cache archiver sets these on every upload.
		if req.Header.Get("Content-Type") == "" {
			req.Header.Set("Content-Type", "application/octet-stream")
		}
		req.Header.Set("Last-Modified", time.Now().UTC().Format(http.TimeFormat))
		req.ContentLength = int64(len(body))
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	t.Cleanup(func() { _ = res.Body.Close() })
	return res
}

//= docs/requirements/10-test-doubles.md#fake-gitlab
//= type=test
//# The project SHALL provide a fake object store that accepts the
//# pre-signed style requests the gitlab-runner cache client issues, so that
//# the `cache:` keyword works end to end against a local endpoint.

func TestPresignedCacheRoundTrip(t *testing.T) {
	t.Parallel()
	s := startStore(t)
	ctx := context.Background()
	adapter := cacheAdapter(t, s, "main-protected")
	archive := []byte("cache archive bytes")

	// A missing cache is 404 on HEAD and GET, which the extractor reads as
	// "no cache yet" rather than a failure.
	head := adapter.GetHeadURL(ctx)
	if head.URL == nil || !strings.Contains(head.URL.RawQuery, "X-Amz-Signature") {
		t.Fatalf("GetHeadURL = %+v, want a pre-signed URL", head)
	}
	if res := doRequest(t, http.MethodHead, head.URL.String(), nil, nil); res.StatusCode != http.StatusNotFound {
		t.Fatalf("HEAD missing = %d, want 404", res.StatusCode)
	}
	if res := doRequest(t, http.MethodGet, adapter.GetDownloadURL(ctx).URL.String(), nil, nil); res.StatusCode != http.StatusNotFound {
		t.Fatalf("GET missing = %d, want 404", res.StatusCode)
	}

	// The archiver uploads with PUT and the headers the adapter signed.
	up := adapter.GetUploadURL(ctx)
	if res := doRequest(t, http.MethodPut, up.URL.String(), up.Headers, archive); res.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(res.Body)
		t.Fatalf("PUT = %d %s, want 200", res.StatusCode, body)
	}
	wantKey := "runner-cache/prefix/runner/abcdefgh/project/42/main-protected"
	objects := s.Objects()
	if got, ok := objects[wantKey]; !ok || !bytes.Equal(got, archive) {
		t.Fatalf("Objects = %v, want %q under %q", keys(objects), archive, wantKey)
	}

	// HEAD reports the object with Last-Modified, which the extractor uses
	// to skip an up-to-date download.
	res := doRequest(t, http.MethodHead, head.URL.String(), nil, nil)
	if res.StatusCode != http.StatusOK || res.ContentLength != int64(len(archive)) {
		t.Fatalf("HEAD = %d length %d, want 200 with %d", res.StatusCode, res.ContentLength, len(archive))
	}
	if _, err := http.ParseTime(res.Header.Get("Last-Modified")); err != nil {
		t.Errorf("HEAD Last-Modified = %q: %v", res.Header.Get("Last-Modified"), err)
	}

	// GET returns the archive; a Range GET, which the extractor uses to
	// probe for parallel download, is a 206 with Content-Range.
	res = doRequest(t, http.MethodGet, adapter.GetDownloadURL(ctx).URL.String(), nil, nil)
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusOK || !bytes.Equal(body, archive) {
		t.Fatalf("GET = %d %q, want 200 with the archive", res.StatusCode, body)
	}
	res = doRequest(t, http.MethodGet, adapter.GetDownloadURL(ctx).URL.String(), http.Header{"Range": {"bytes=0-0"}}, nil)
	body, _ = io.ReadAll(res.Body)
	if res.StatusCode != http.StatusPartialContent || string(body) != "c" || !strings.HasSuffix(res.Header.Get("Content-Range"), "/"+itoa(len(archive))) {
		t.Fatalf("Range GET = %d %q Content-Range %q, want 206 with the first byte", res.StatusCode, body, res.Header.Get("Content-Range"))
	}

	// A second upload replaces the object.
	if res := doRequest(t, http.MethodPut, up.URL.String(), up.Headers, []byte("v2")); res.StatusCode != http.StatusOK {
		t.Fatalf("second PUT = %d, want 200", res.StatusCode)
	}
	if got := s.Objects()[wantKey]; string(got) != "v2" {
		t.Errorf("object after second PUT = %q, want v2", got)
	}
}

//= docs/requirements/10-test-doubles.md#fake-gitlab
//= type=test
//# The project SHALL provide a fake object store that accepts the
//# pre-signed style requests the gitlab-runner cache client issues, so that
//# the `cache:` keyword works end to end against a local endpoint.

func TestStoreRequests(t *testing.T) {
	t.Parallel()
	s := startStore(t)
	s.Put("bucket/seeded/key", []byte("seeded"))
	base := s.Endpoint()
	if !strings.HasPrefix(base, "http://127.0.0.1:") || s.Addr() == "" {
		t.Fatalf("Endpoint = %q Addr = %q, want a loopback endpoint", base, s.Addr())
	}

	tests := []struct {
		name     string
		method   string
		path     string
		body     []byte
		wantCode int
		wantBody string
	}{
		{"get seeded ignoring signature", http.MethodGet, "/bucket/seeded/key?X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Signature=abc", nil, http.StatusOK, "seeded"},
		{"head seeded", http.MethodHead, "/bucket/seeded/key", nil, http.StatusOK, ""},
		{"get missing", http.MethodGet, "/bucket/missing", nil, http.StatusNotFound, "NoSuchKey"},
		{"head missing", http.MethodHead, "/bucket/missing", nil, http.StatusNotFound, ""},
		{"put", http.MethodPut, "/bucket/new?X-Amz-Signature=abc", []byte("new"), http.StatusOK, ""},
		{"get after put", http.MethodGet, "/bucket/new", nil, http.StatusOK, "new"},
		{"delete", http.MethodDelete, "/bucket/new", nil, http.StatusNoContent, ""},
		{"get after delete", http.MethodGet, "/bucket/new", nil, http.StatusNotFound, "NoSuchKey"},
		{"bucket without key", http.MethodGet, "/bucket/", nil, http.StatusBadRequest, "InvalidRequest"},
		{"root", http.MethodGet, "/", nil, http.StatusBadRequest, "InvalidRequest"},
		{"unsupported method", http.MethodPost, "/bucket/seeded/key", nil, http.StatusMethodNotAllowed, "MethodNotAllowed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := doRequest(t, tt.method, base+tt.path, nil, tt.body)
			body, _ := io.ReadAll(res.Body)
			if res.StatusCode != tt.wantCode {
				t.Fatalf("code = %d (%s), want %d", res.StatusCode, body, tt.wantCode)
			}
			if !strings.Contains(string(body), tt.wantBody) {
				t.Errorf("body = %q, want it to contain %q", body, tt.wantBody)
			}
		})
	}
	if got := s.Objects(); len(got) != 1 || string(got["bucket/seeded/key"]) != "seeded" {
		t.Errorf("Objects = %v, want only the seeded object", keys(got))
	}
	s.Close()
	s.Close() // idempotent
	if s.Endpoint() != "" {
		t.Error("Endpoint after Close is not empty")
	}
}

//= docs/requirements/10-test-doubles.md#fake-gitlab
//= type=test
//# The project SHALL provide a fake object store that accepts the
//# pre-signed style requests the gitlab-runner cache client issues, so that
//# the `cache:` keyword works end to end against a local endpoint.

// TestCacheClientRoundTrip drives the store with the gitlab-runner cache
// client itself: the cache-archiver and cache-extractor commands the
// generated shell scripts invoke inside the Job, on the pre-signed URLs the
// real S3 adapter produces. Nothing here builds a request by hand, so the
// method, the path, the headers and the body are exactly the ones the
// helper binary sends and the store has to accept for `cache:` to work.
func TestCacheClientRoundTrip(t *testing.T) {
	// Not parallel: the helper commands read and write the process working
	// directory and MakeFatalToPanic replaces the global logrus hooks.
	defer runnerhelpers.MakeFatalToPanic()()

	work := t.TempDir()
	t.Chdir(work)
	if err := os.MkdirAll(filepath.Join(work, "src"), 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	const content = "cached file\n"
	if err := os.WriteFile(filepath.Join(work, "src", "hello.txt"), []byte(content), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}

	s := startStore(t)
	adapter := cacheAdapter(t, s, "main-cache")
	ctx := context.Background()
	upload := adapter.GetUploadURL(ctx)

	archiver := &cmdhelpers.CacheArchiverCommand{
		File:    filepath.Join(t.TempDir(), "cache.zip"),
		URL:     upload.URL.String(),
		Timeout: 1,
	}
	archiver.Paths = []string{"src"}
	for name, values := range upload.Headers {
		for _, value := range values {
			archiver.Headers = append(archiver.Headers, name+":"+value)
		}
	}
	archiver.Execute(nil)

	const wantKey = "runner-cache/prefix/runner/abcdefgh/project/42/main-cache"
	stored, ok := s.Objects()[wantKey]
	if !ok {
		t.Fatalf("cache-archiver stored nothing under %q, objects: %v", wantKey, keys(s.Objects()))
	}
	if !bytes.HasPrefix(stored, []byte("PK")) {
		t.Errorf("stored object is not the zip archive the archiver uploaded: %q", firstBytes(stored))
	}

	// The extractor downloads from the pre-signed GET URL and unpacks into
	// the working directory, so a clean one proves the round trip.
	restore := t.TempDir()
	t.Chdir(restore)
	extractor := &cmdhelpers.CacheExtractorCommand{
		File:    filepath.Join(t.TempDir(), "cache.zip"),
		URL:     adapter.GetDownloadURL(ctx).URL.String(),
		Timeout: 1,
	}
	extractor.Execute(nil)

	got, err := os.ReadFile(filepath.Join(restore, "src", "hello.txt"))
	if err != nil {
		t.Fatalf("read extracted file: %v", err)
	}
	if string(got) != content {
		t.Errorf("extracted file = %q, want %q", got, content)
	}
}

// firstBytes is a short, printable prefix of b for a failure message.
func firstBytes(b []byte) []byte {
	if len(b) > 16 {
		return b[:16]
	}
	return b
}

func keys(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func itoa(n int) string { return strconv.Itoa(n) }
