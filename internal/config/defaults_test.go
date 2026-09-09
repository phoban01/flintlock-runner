package config

import (
	"strings"
	"testing"
)

//= docs/requirements/07-configuration.md#gitlab-section
//= type=test
//# The Runner SHALL default the concurrency limit to twice the sum
//# of the declared Pool sizes, because immediate-on-lease replenishment keeps
//# Jobs flowing beyond the idle Pool size.

// TestDefaultConcurrencyIsTwiceThePoolSizes checks the derived concurrency
// limit: with no gitlab.concurrent in the file it is twice the sum of every
// Profile's Pool size, and a value in the file is kept.
func TestDefaultConcurrencyIsTwiceThePoolSizes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		sizes []int
		want  int
	}{
		{"one pool", []int{2}, 4},
		{"two pools are summed", []int{2, 3}, 10},
		{"three pools", []int{1, 1, 5}, 14},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := &Config{}
			sum := 0
			for _, size := range tt.sizes {
				cfg.Profiles = append(cfg.Profiles, Profile{Pool: PoolSettings{Size: size}})
				sum += size
			}
			ApplyDefaults(cfg)
			if cfg.GitLab.Concurrent != tt.want {
				t.Errorf("gitlab.concurrent = %d, want %d (twice the pool sizes %v)",
					cfg.GitLab.Concurrent, tt.want, tt.sizes)
			}
			if cfg.GitLab.Concurrent != DefaultConcurrencyFactor*sum {
				t.Errorf("gitlab.concurrent = %d, want %d", cfg.GitLab.Concurrent, DefaultConcurrencyFactor*sum)
			}
		})
	}

	t.Run("from a file with two profiles", func(t *testing.T) {
		t.Parallel()
		cfg := loadTestdata(t, "with-inventory-file.yaml")
		if len(cfg.Profiles) != 2 || cfg.Profiles[0].Pool.Size != 1 || cfg.Profiles[1].Pool.Size != 1 {
			t.Fatalf("pool sizes = %+v", cfg.Profiles)
		}
		if cfg.GitLab.Concurrent != 4 {
			t.Errorf("gitlab.concurrent = %d, want 4 for two Pools of one", cfg.GitLab.Concurrent)
		}
	})

	t.Run("an explicit limit is kept", func(t *testing.T) {
		t.Parallel()
		data := strings.Replace(minimalConfig(t), "  token: glrt-minimal", "  token: glrt-minimal\n  concurrent: 3", 1)
		cfg, err := Parse([]byte(data), t.TempDir(), WithEnv(noEnv))
		if err != nil {
			t.Fatal(err)
		}
		if cfg.GitLab.Concurrent != 3 {
			t.Errorf("gitlab.concurrent = %d, want the configured 3", cfg.GitLab.Concurrent)
		}
	})
}
