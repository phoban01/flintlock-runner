package fakes3_test

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"gitlab.com/gitlab-org/gitlab-runner/cache"
	"gitlab.com/gitlab-org/gitlab-runner/cache/cacheconfig"
	_ "gitlab.com/gitlab-org/gitlab-runner/cache/s3" // registers the s3 adapter the Runner's cache config selects

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

func keys(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func itoa(n int) string { return strconv.Itoa(n) }
