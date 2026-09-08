package config

import "testing"

// TestConfigLiteral checks that the schema can be built as a literal with
// nothing imported but this package, which is what every other package's
// tests rely on.
func TestConfigLiteral(t *testing.T) {
	t.Parallel()
	enabled := true
	cfg := Config{
		GitLab: GitLab{URL: "https://gitlab.example.com", Token: "glrt-x", Name: "r1"},
		Profiles: []Profile{{
			Name: "default", Arch: ArchARM64, VCPU: 2, MemoryMB: 2048,
			Kernel: Kernel{Image: "ghcr.io/x/kernel:6.1"}, RootFS: "ghcr.io/x/rootfs:1",
			Default: true, Transport: Transport{Kind: TransportExec},
			Pool: PoolSettings{Size: 2, Strategy: ReplenishImmediateOnLease},
		}},
		Inventory:   Inventory{Hosts: []HostEntry{{Name: "h1", Endpoint: "10.0.0.1:9090", Arch: ArchARM64}}},
		PoolManager: PoolManager{Endpoint: "10.0.0.9:9091"},
		HostServices: HostServices{
			Buildkit:    Buildkit{Service: Service{Enabled: &enabled, Port: 1234}, StorageLimit: 50 << 30},
			CacheVolume: CacheVolume{Directory: "/var/cache/flintlock", SizeCap: 200 << 30},
		},
	}
	if cfg.Profiles[0].Pool.Name != "" {
		t.Fatal("pool name defaults are applied by validation, not by the literal")
	}
	if got := cfg.HostServices.Buildkit.StorageLimit.String(); got != "50GiB" {
		t.Errorf("StorageLimit = %s, want 50GiB", got)
	}
	if string(cfg.GitLab.Token) != "glrt-x" {
		t.Error("Secret lost its value")
	}
}
