// Package fakes3 is the fake object store
// (docs/requirements/10-test-doubles.md#fake-gitlab, TD-034): it accepts the
// pre-signed style PUT, GET and HEAD requests the gitlab-runner cache client
// issues so that `cache:` works end to end against a local endpoint.
//
// The gitlab-runner cache adapter signs URLs of the form
// http://host:port/<bucket>/<key>?X-Amz-Algorithm=...&X-Amz-Signature=...
// and the helper then issues plain HTTP requests against them. The fake is
// path-style, ignores the signature query parameters entirely, and keeps
// every object in memory keyed by "<bucket>/<key>".
package fakes3

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	shutdownTimeout = 5 * time.Second
	// maxObjectBytes bounds a single PUT body.
	maxObjectBytes = 1 << 30
)

// object is one stored blob with the time it was written, which GET and
// HEAD report as Last-Modified since the cache extractor compares it with
// the local archive.
type object struct {
	data    []byte
	modTime time.Time
}

//= docs/requirements/10-test-doubles.md#fake-gitlab
//# The project SHALL provide a fake object store that accepts the
//# pre-signed style requests the gitlab-runner cache client issues, so that
//# the `cache:` keyword works end to end against a local endpoint.

// Store is the fake object store. New builds it, Start serves it on a
// loopback port and Close stops it; Handler exposes it for httptest. Its
// methods are safe for concurrent use.
type Store struct {
	mu      sync.Mutex
	objects map[string]*object
	now     func() time.Time

	listener net.Listener
	server   *http.Server
	serveErr chan error
}

// New builds an empty Store. Start serves it.
func New() *Store { return &Store{objects: map[string]*object{}, now: time.Now} }

// Start begins serving on a free loopback port.
func (s *Store) Start() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listener != nil {
		return errors.New("fakes3: already started")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("fakes3: listen: %w", err)
	}
	s.listener = ln
	s.server = &http.Server{Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second}
	s.serveErr = make(chan error, 1)
	go func(srv *http.Server) {
		s.serveErr <- srv.Serve(ln)
	}(s.server)
	return nil
}

// Close stops the server and waits for the serving goroutine. It is safe to
// call more than once and before Start.
func (s *Store) Close() {
	s.mu.Lock()
	srv, serveErr := s.server, s.serveErr
	s.server, s.listener = nil, nil
	s.mu.Unlock()
	if srv == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		_ = srv.Close()
	}
	<-serveErr
}

// Endpoint is the URL to configure as the S3-compatible endpoint, empty
// before Start.
func (s *Store) Endpoint() string {
	if addr := s.Addr(); addr != "" {
		return "http://" + addr
	}
	return ""
}

// Addr is the host:port of the store, empty before Start. It is the form
// gitlab-runner's cache configuration takes as ServerAddress, together with
// Insecure set, since the store speaks plain HTTP.
func (s *Store) Addr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listener == nil {
		return ""
	}
	return s.listener.Addr().String()
}

// Put stores an object under "<bucket>/<key>" without going through HTTP,
// for tests that need a cache to already exist.
func (s *Store) Put(key string, data []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.objects[strings.TrimPrefix(key, "/")] = &object{data: append([]byte(nil), data...), modTime: s.now()}
}

// Objects returns a copy of every stored object keyed by bucket/key.
func (s *Store) Objects() map[string][]byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string][]byte, len(s.objects))
	for k, v := range s.objects {
		out[k] = append([]byte(nil), v.data...)
	}
	return out
}

// Handler returns the HTTP handler behind Start. The object name is the
// request path without its leading slash; the query string, where the
// signature lives, is ignored. PUT stores, GET and HEAD serve with Range and
// Last-Modified support, DELETE removes, and a missing object is 404 with
// the S3 NoSuchKey error document.
func (s *Store) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimPrefix(r.URL.Path, "/")
		if key == "" || strings.HasSuffix(key, "/") {
			writeS3Error(w, http.StatusBadRequest, "InvalidRequest", "object key is required")
			return
		}
		switch r.Method {
		case http.MethodPut:
			s.put(w, r, key)
		case http.MethodGet, http.MethodHead:
			s.get(w, r, key)
		case http.MethodDelete:
			s.mu.Lock()
			delete(s.objects, key)
			s.mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		default:
			writeS3Error(w, http.StatusMethodNotAllowed, "MethodNotAllowed", "method not allowed")
		}
	})
}

func (s *Store) put(w http.ResponseWriter, r *http.Request, key string) {
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxObjectBytes))
	if err != nil {
		writeS3Error(w, http.StatusBadRequest, "IncompleteBody", err.Error())
		return
	}
	s.mu.Lock()
	s.objects[key] = &object{data: data, modTime: s.now()}
	s.mu.Unlock()
	w.Header().Set("ETag", etag(data))
	w.WriteHeader(http.StatusOK)
}

func (s *Store) get(w http.ResponseWriter, r *http.Request, key string) {
	s.mu.Lock()
	obj := s.objects[key]
	s.mu.Unlock()
	if obj == nil {
		writeS3Error(w, http.StatusNotFound, "NoSuchKey", "The specified key does not exist.")
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Accept-Ranges", "bytes")
	// ServeContent handles HEAD, Range and Last-Modified the way S3 does:
	// a full GET is 200, a satisfiable Range is 206 with Content-Range.
	http.ServeContent(w, r, "", obj.modTime, bytes.NewReader(obj.data))
}

// etag is the entity tag S3 puts on an object stored by a single-part PUT:
// the MD5 digest of the body as lower-case hex, in double quotes. It has to
// be the content digest rather than anything derived from the length, or two
// different objects of the same size would compare equal to a client using
// it for change detection. MD5 is S3's wire format here, not a security
// choice.
func etag(data []byte) string {
	sum := md5.Sum(data)
	return fmt.Sprintf("%q", hex.EncodeToString(sum[:]))
}

// writeS3Error answers with the XML error document S3 uses. The body is
// written for a HEAD too; net/http discards it, as it does for every HEAD.
func writeS3Error(w http.ResponseWriter, code int, s3code, message string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(code)
	_, _ = fmt.Fprintf(w, "<?xml version=\"1.0\" encoding=\"UTF-8\"?><Error><Code>%s</Code><Message>%s</Message></Error>", s3code, message)
}
