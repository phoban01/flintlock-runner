package scheduler

import (
	"errors"
	"testing"

	"github.com/phoban01/flintlock-runner/internal/poolmgr"
)

func TestRefusalMatchesErrRefused(t *testing.T) {
	t.Parallel()
	var err error = &Refusal{Reason: RefusalNoWarmMicroVM}
	if !errors.Is(err, ErrRefused) {
		t.Error("Refusal does not match ErrRefused")
	}
	var r *Refusal
	if !errors.As(err, &r) || r.Reason != RefusalNoWarmMicroVM {
		t.Error("Refusal reason not recoverable with errors.As")
	}
}

func TestProfileErrorNamesImage(t *testing.T) {
	t.Parallel()
	err := &ProfileError{Image: "golang:1.26"}
	if !errors.Is(err, ErrNoProfile) {
		t.Error("ProfileError does not match ErrNoProfile")
	}
	if got := err.Error(); got != `scheduler: no profile for job: image "golang:1.26" matches no profile` {
		t.Errorf("Error() = %q", got)
	}
}

func TestAllocationErrorUnwraps(t *testing.T) {
	t.Parallel()
	err := &AllocationError{Profile: "p", Pool: poolmgr.PoolRef{Name: "p", Namespace: "ns"}, Host: "h1", Err: ErrHostNotInInventory}
	if !errors.Is(err, ErrHostNotInInventory) {
		t.Error("cause not unwrapped")
	}
	want := `scheduler: allocation for profile "p" from pool ns/p failed on host "h1": scheduler: placed on host not in inventory`
	if got := err.Error(); got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

func TestSnapshotReady(t *testing.T) {
	t.Parallel()
	s := Snapshot{PoolManagerContacted: true, Hosts: []HostHealth{{Name: "h1"}}}
	if s.Ready() {
		t.Error("ready with no healthy host")
	}
	s.Hosts[0].Healthy = true
	if !s.Ready() {
		t.Error("not ready with a healthy host and a contacted pool manager")
	}
	s.PoolManagerContacted = false
	if s.Ready() {
		t.Error("ready before pool manager contact")
	}
}
