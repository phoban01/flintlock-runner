package fake

import (
	"errors"
	"testing"

	poolmgrv1 "github.com/liquidmetal-dev/battery/api/proto/poolmgr/v1alpha1"

	"github.com/phoban01/flintlock-runner/internal/poolmgr"
)

// minSize is the address of a MIN_SIZE_THRESHOLD min_size.
func minSize(n int32) *int32 { return &n }

//= docs/requirements/10-test-doubles.md#fake-pool-manager
//= type=test
//# The fake Pool Manager SHALL implement the immediate-on-lease,
//# minimum-size-threshold and replace-on-delete replenishment strategies as
//# declared in the `PoolSpec`.

// TestReplenishmentStrategies drives each of the three strategies declared
// in a PoolSpec on the fake clock. Each case asserts both what the strategy
// does and what it does not do: the replacement that IMMEDIATE_ON_LEASE
// starts on a claim is one REPLACE_ON_DELETE does not, the replacement
// REPLACE_ON_DELETE starts on a deletion is one MIN_SIZE_THRESHOLD does not,
// and MIN_SIZE_THRESHOLD acts only once the available count falls under
// min_size. Nothing here sleeps: an event-driven replenishment happens with
// the clock standing still, and only the threshold case moves it.
func TestReplenishmentStrategies(t *testing.T) {
	t.Run("immediate on lease", func(t *testing.T) {
		h := newHarness(t, poolmgr.FakeConfig{}, "host-a")
		spec := h.spec("pool", 2, "host-a")
		h.createPool(spec)

		claim := h.claim("pool")
		// The replacement is started by the claim itself, so it arrives
		// without the clock moving at all.
		got := h.collectUntil(poolmgrv1.EventType_VM_AVAILABLE)
		replenish := eventOfType(t, got, poolmgrv1.EventType_POOL_REPLENISHING)
		if reason := payloadOf(t, replenish)["reason"]; reason != "claim" {
			t.Fatalf("POOL_REPLENISHING reason = %v, want %q", reason, "claim")
		}
		if n := count(got, poolmgrv1.EventType_POOL_SIZE_BELOW_TARGET); n != 0 {
			t.Fatalf("%d POOL_SIZE_BELOW_TARGET events, want none: the claim replenishes, not the tick", n)
		}
		if st := h.pool("pool").Status; st.Available != 2 || st.Leased != 1 {
			t.Fatalf("status after the claim = %+v, want 2 available and 1 leased", st)
		}
		if created, _ := h.stubs["host-a"].counts(); created != 3 {
			t.Fatalf("host created %d microvms, want 3 (two warm plus the replacement)", created)
		}
		h.release(claim.LeaseID)
	})

	t.Run("minimum size threshold", func(t *testing.T) {
		h := newHarness(t, poolmgr.FakeConfig{}, "host-a")
		spec := h.spec("pool", 3, "host-a")
		spec.Replenishment = poolmgr.ReplenishmentStrategy{
			Type:    poolmgrv1.ReplenishmentStrategyType_MIN_SIZE_THRESHOLD,
			MinSize: minSize(2),
		}
		h.createPool(spec)

		// A claim does not replenish, and while two MicroVMs are still
		// available the threshold is not crossed, so a tick does not either.
		first := h.claim("pool")
		second := h.claim("pool")
		h.tick()
		h.tick()
		if st := h.pool("pool").Status; st.Available != 1 || st.Leased != 2 {
			t.Fatalf("status after two claims and two ticks = %+v, want 1 available and 2 leased", st)
		}
		if created, _ := h.stubs["host-a"].counts(); created != 3 {
			t.Fatalf("host created %d microvms, want 3: nothing is replenished above min_size", created)
		}

		// Releasing one takes the population to two, one of them available,
		// which is under min_size: the next tick tops the Pool back up.
		h.release(first.LeaseID)
		if st := h.pool("pool").Status; st.Available != 1 || st.Leased != 1 {
			t.Fatalf("status after the release = %+v, want 1 available and 1 leased", st)
		}
		h.tick()
		got := h.collectUntil(poolmgrv1.EventType_VM_AVAILABLE)
		replenish := eventOfType(t, got, poolmgrv1.EventType_POOL_REPLENISHING)
		if reason := payloadOf(t, replenish)["reason"]; reason != "tick" {
			t.Fatalf("POOL_REPLENISHING reason = %v, want %q", reason, "tick")
		}
		if st := h.pool("pool").Status; st.Available != 2 || st.Leased != 1 {
			t.Fatalf("status after the top-up = %+v, want 2 available and 1 leased", st)
		}
		h.release(second.LeaseID)
	})

	t.Run("replace on delete", func(t *testing.T) {
		h := newHarness(t, poolmgr.FakeConfig{}, "host-a")
		spec := h.spec("pool", 2, "host-a")
		spec.Replenishment = poolmgr.ReplenishmentStrategy{
			Type: poolmgrv1.ReplenishmentStrategyType_REPLACE_ON_DELETE,
		}
		h.createPool(spec)

		// A claim leaves the MicroVM in the Pool, so nothing is replaced and
		// a tick finds no deficit.
		claim := h.claim("pool")
		h.tick()
		if created, _ := h.stubs["host-a"].counts(); created != 2 {
			t.Fatalf("host created %d microvms, want 2: a claim is not a deletion", created)
		}

		// The release deletes the MicroVM, and the deletion is what starts
		// the replacement; again the clock stands still.
		h.release(claim.LeaseID)
		got := h.collectUntil(poolmgrv1.EventType_VM_AVAILABLE)
		replenish := eventOfType(t, got, poolmgrv1.EventType_POOL_REPLENISHING)
		if reason := payloadOf(t, replenish)["reason"]; reason != "vm deleted" {
			t.Fatalf("POOL_REPLENISHING reason = %v, want %q", reason, "vm deleted")
		}
		if st := h.pool("pool").Status; st.Available != 2 || st.Leased != 0 {
			t.Fatalf("status after the release = %+v, want 2 available and none leased", st)
		}
		if created, deleted := h.stubs["host-a"].counts(); created != 3 || deleted != 1 {
			t.Fatalf("host created %d and deleted %d microvms, want 3 and 1", created, deleted)
		}
	})
}

//= docs/requirements/10-test-doubles.md#fake-pool-manager
//= type=test
//# The fake Pool Manager SHALL implement the immediate-on-lease,
//# minimum-size-threshold and replace-on-delete replenishment strategies as
//# declared in the `PoolSpec`.

// TestReplenishmentStrategyValidation checks that a PoolSpec has to declare
// one of the three strategies, and that MIN_SIZE_THRESHOLD without a
// positive min_size is refused rather than silently never replenishing.
func TestReplenishmentStrategyValidation(t *testing.T) {
	h := newHarness(t, poolmgr.FakeConfig{}, "host-a")
	tests := []struct {
		name     string
		strategy poolmgr.ReplenishmentStrategy
		wantErr  error
	}{
		{
			name:     "immediate on lease",
			strategy: poolmgr.ReplenishmentStrategy{Type: poolmgrv1.ReplenishmentStrategyType_IMMEDIATE_ON_LEASE},
		},
		{
			name:     "replace on delete",
			strategy: poolmgr.ReplenishmentStrategy{Type: poolmgrv1.ReplenishmentStrategyType_REPLACE_ON_DELETE},
		},
		{
			name: "min size threshold",
			strategy: poolmgr.ReplenishmentStrategy{
				Type:    poolmgrv1.ReplenishmentStrategyType_MIN_SIZE_THRESHOLD,
				MinSize: minSize(1),
			},
		},
		{
			name: "min size threshold without a min size",
			strategy: poolmgr.ReplenishmentStrategy{
				Type: poolmgrv1.ReplenishmentStrategyType_MIN_SIZE_THRESHOLD,
			},
			wantErr: poolmgr.ErrInvalid,
		},
		{
			name:     "unspecified",
			strategy: poolmgr.ReplenishmentStrategy{Type: poolmgrv1.ReplenishmentStrategyType_REPLENISHMENT_STRATEGY_TYPE_UNSPECIFIED},
			wantErr:  poolmgr.ErrInvalid,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec := h.spec(tt.name, 0, "host-a")
			spec.Replenishment = tt.strategy
			_, err := h.client.CreatePool(h.ctx, spec)
			if tt.wantErr == nil {
				if err != nil {
					t.Fatalf("CreatePool: %v, want it accepted", err)
				}
				return
			}
			if err == nil || !errors.Is(err, tt.wantErr) {
				t.Fatalf("CreatePool = %v, want %v", err, tt.wantErr)
			}
		})
	}
}
