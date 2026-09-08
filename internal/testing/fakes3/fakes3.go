// Package fakes3 is the fake object store
// (docs/requirements/10-test-doubles.md#fake-gitlab, TD-034): it accepts the
// pre-signed style PUT and GET requests the gitlab-runner cache client
// issues so that `cache:` works end to end against a local endpoint.
//
// Phase 0 provides the compiling skeleton. The `fakes` work package of
// docs/PLAN.md fills it in.
package fakes3

import (
	"errors"
	"sync"
)

//= docs/requirements/10-test-doubles.md#fake-gitlab
//= type=todo
//= tracking-issue=TBD
//# The project SHALL provide a fake object store that accepts the
//# pre-signed style requests the gitlab-runner cache client issues, so that
//# the `cache:` keyword works end to end against a local endpoint.

// Store is the fake object store.
type Store struct {
	mu      sync.Mutex
	objects map[string][]byte
}

// New builds an empty Store. Start serves it.
func New() *Store { return &Store{objects: map[string][]byte{}} }

// Start begins serving on a free loopback port.
func (s *Store) Start() error { return errors.ErrUnsupported }

// Close stops the server.
func (s *Store) Close() {}

// Endpoint is the URL to configure as the S3-compatible endpoint, empty
// before Start.
func (s *Store) Endpoint() string { return "" }

// Objects returns a copy of every stored object keyed by bucket/key.
func (s *Store) Objects() map[string][]byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string][]byte, len(s.objects))
	for k, v := range s.objects {
		out[k] = append([]byte(nil), v...)
	}
	return out
}
