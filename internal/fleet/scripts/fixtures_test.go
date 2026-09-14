package scripts

import (
	"strings"
	"testing"
	"time"

	"github.com/phoban01/flintlock-runner/internal/config"
	"github.com/phoban01/flintlock-runner/internal/fleet"
)

func ptr[T any](v T) *T { return &v }

// fullInput is a representative input with every Host Service enabled,
// private modules, credentials and three Hosts in the Inventory.
func fullInput() fleet.RenderInput {
	f := config.Fleet{
		Region:         "eu-west-1",
		Versions:       config.PinnedVersions{Flintlock: "v0.14.0", Firecracker: "v1.10.1", CloudHypervisor: "v41.0", Containerd: "v1.7.22", PoolManager: "v0.1.0"},
		ThinPoolDevice: "/dev/nvme1n1",
		GuestSubnet:    "172.31.0.0/16",
		Flintlockd:     config.Flintlockd{Port: 9090, Token: "fleet-token"},
		LaunchTemplate: &config.LaunchTemplate{Parameters: config.LaunchTemplateParameters{
			HostToken: "/flr/host-token", TLSCA: "/flr/tls-ca", TLSCert: "/flr/tls-cert", TLSKey: "/flr/tls-key",
		}},
	}
	hs := config.HostServices{
		Buildkit: config.Buildkit{Service: config.Service{Enabled: ptr(true), Port: 1234}, StorageLimit: 40 << 30, GCPolicy: "keepDuration=48h,filters=type==source.local"},
		GoProxy: config.GoProxy{
			Service: config.Service{Enabled: ptr(true), Port: 3000}, Upstream: "https://proxy.golang.org",
			PrivatePatterns: []string{"gitlab.example.com/platform/*", "gitlab.example.com/Tools"}, PrivateVCSHost: "gitlab.example.com",
			CredentialParameter: "/flr/goproxy-token", PrivateRevalidateInterval: 12 * time.Hour,
			Prewarm: []string{"golang.org/x/tools@v0.30.0", "github.com/BurntSushi/toml@v1.4.0"},
		},
		RegistryMirror: config.RegistryMirror{
			Service: config.Service{Enabled: ptr(true), Port: 5000},
			Upstreams: []config.RegistryUpstream{
				{URL: "https://registry-1.docker.io"},
				{URL: "https://ghcr.io", CredentialParameter: "/flr/ghcr-token"},
			},
			Prewarm: []string{"docker.io/library/alpine:3.20"},
		},
		HTTPCache: config.HTTPCache{Service: config.Service{Enabled: ptr(true), Port: 3128}, Upstreams: []config.HTTPCacheUpstream{
			{Name: "npm", URL: "https://registry.npmjs.org", SizeLimit: 10 << 30, TTL: 48 * time.Hour, EnvVar: "NPM_CONFIG_REGISTRY"},
			{Name: "pypi", URL: "https://pypi.org/simple/", SizeLimit: 5 << 30, TTL: 24 * time.Hour, EnvVar: "PIP_INDEX_URL"},
		}},
		CacheVolume: config.CacheVolume{Device: "/dev/nvme2n1", SizeCap: 200 << 30},
	}
	profiles := []config.Profile{
		{
			Name: "go-arm", Arch: config.ArchARM64,
			Kernel:            config.Kernel{Image: "ghcr.io/example/kernel:6.1-arm64"},
			RootFS:            "ghcr.io/example/rootfs-go:1.25@sha256:0000000000000000000000000000000000000000000000000000000000000000",
			AdditionalVolumes: []config.Volume{{ID: "tools", Image: "ghcr.io/example/tools:1"}},
			Transport:         config.Transport{Kind: config.TransportExec},
		},
		{
			Name: "go-amd", Arch: config.ArchAMD64,
			Kernel: config.Kernel{Image: "ghcr.io/example/kernel:6.1-amd64"},
			RootFS: "ghcr.io/example/rootfs-go:1.25-amd64",
		},
	}
	inst := fleet.Instance{ID: "i-0aaa", Type: "m7g.metal", Arch: config.ArchARM64, PrivateIP: "10.0.1.10", State: "running", VCPU: 64, MemoryMB: 262144}
	inv := fleet.Inventory{Hosts: []config.HostEntry{
		{Name: "i-0aaa", Endpoint: "10.0.1.10:9090", Arch: config.ArchARM64, Services: config.HostServiceAddresses{
			Buildkit: "tcp://172.31.0.1:1234", GoProxy: "http://172.31.0.1:3000", RegistryMirror: "http://172.31.0.1:5000",
			HTTPCache: map[string]string{"npm": "http://172.31.0.1:3128/npm", "pypi": "http://172.31.0.1:3128/pypi"},
		}},
		{Name: "i-0bbb", Endpoint: "10.0.1.11:9090", Arch: config.ArchARM64},
		{Name: "i-0ccc", Endpoint: "10.0.2.12:9090", Arch: config.ArchAMD64},
	}}
	return fleet.RenderInput{
		Fleet: f, HostServices: hs, Profiles: profiles, Instance: inst, Inventory: inv,
		Options: map[string]string{
			OptionPoolManagerListen: "10.0.0.5:9443", OptionPoolManagerCert: "/etc/flintlock-runner/pki/poolmgr.pem",
			OptionPoolManagerKey: "/etc/flintlock-runner/pki/poolmgr.key", OptionHostCAFile: "/etc/flintlock-runner/pki/ca.pem",
		},
	}
}

// sparseInput turns the other branches of every template: Host Services
// disabled, flintlockd insecure, an ssh Profile, an egress allow-list, a
// cache directory and a lone Host.
func sparseInput() fleet.RenderInput {
	in := fullInput()
	in.Fleet.Flintlockd.Insecure = true
	in.Fleet.LaunchTemplate = nil
	in.Fleet.EgressAllowList = []string{"10.20.0.0/16", "192.0.2.10"}
	in.HostServices.Buildkit.Enabled = ptr(false)
	in.HostServices.GoProxy.Enabled = ptr(false)
	in.HostServices.RegistryMirror.Enabled = ptr(false)
	in.HostServices.HTTPCache.Enabled = ptr(false)
	in.HostServices.CacheVolume = config.CacheVolume{Directory: "/var/lib/flintlock-runner/cache", SizeCap: 50 << 30}
	in.Profiles[0].Transport.Kind = config.TransportSSH
	in.Inventory.Hosts = in.Inventory.Hosts[:1]
	in.Options = nil
	return in
}

// publicGoInput has the Go module proxy without private modules and the
// mirror without credentials.
func publicGoInput() fleet.RenderInput {
	in := fullInput()
	in.HostServices.GoProxy.PrivatePatterns = nil
	in.HostServices.GoProxy.PrivateVCSHost = ""
	in.HostServices.GoProxy.CredentialParameter = ""
	in.HostServices.RegistryMirror.Upstreams = nil
	in.HostServices.HTTPCache.Enabled = ptr(false)
	in.HostServices.Buildkit.GCPolicy = ""
	return in
}

func render(t *testing.T, step fleet.Step, in fleet.RenderInput) string {
	t.Helper()
	s, err := New()
	if err != nil {
		t.Fatal(err)
	}
	sc, err := s.Render(step, in)
	if err != nil {
		t.Fatalf("render %s: %v", step, err)
	}
	if sc.Name != string(step) || sc.Timeout <= 0 {
		t.Fatalf("script = %q timeout %v", sc.Name, sc.Timeout)
	}
	return sc.Content
}

func mustContain(t *testing.T, script string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(script, w) {
			t.Errorf("script lacks %q", w)
		}
	}
}

func mustNotContain(t *testing.T, script string, bad ...string) {
	t.Helper()
	for _, b := range bad {
		if strings.Contains(script, b) {
			t.Errorf("script contains %q", b)
		}
	}
}
