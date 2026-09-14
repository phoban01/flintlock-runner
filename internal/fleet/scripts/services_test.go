package scripts

import (
	"regexp"
	"strings"
	"testing"

	"github.com/phoban01/flintlock-runner/internal/config"
	"github.com/phoban01/flintlock-runner/internal/fleet"
)

//= docs/requirements/06-fleet.md#host-services
//= type=test
//# The Fleet Controller SHALL install on every Host a `buildkitd`
//# service, a Go module proxy service, a pull-through container registry
//# mirror service and an HTTP cache service for the configured upstreams,
//# collectively the Host Services.

func TestHostServicesInstallsAllFour(t *testing.T) {
	t.Parallel()
	s := render(t, fleet.StepHostServices, fullInput())
	mustContain(t, s,
		`ensure_service flintlock-runner-buildkitd "$bk_restart"`,
		`ensure_service flintlock-runner-athens "$athens_restart"`,
		`ensure_service flintlock-runner-zot "$zot_restart"`,
		`ensure_service nginx "$nginx_restart"`,
		"location /npm/ {", "location /pypi/ {",
	)
}

//= docs/requirements/06-fleet.md#host-services
//= type=test
//# The Fleet Controller SHALL bind every Host Service only to the
//# guest bridge gateway address so that it is reachable from guests on that
//# Host and from nothing else.

func TestHostServicesBindGatewayOnly(t *testing.T) {
	t.Parallel()
	s := render(t, fleet.StepHostServices, fullInput())
	mustContain(t, s,
		`address = ["tcp://172.31.0.1:1234"]`,
		`"address": "172.31.0.1"`, `"port": "5000"`,
		"listen 172.31.0.1:3128;",
		"listen 172.31.0.1:3000;",
		// Athens sits behind nginx on the loopback address only.
		`Port = "127.0.0.1:3999"`,
		"rm -f /etc/nginx/sites-enabled/default",
	)
	mustNotContain(t, s, "0.0.0.0", "listen 80", "listen 3128", "listen 3000", `Port = ":`)
}

//= docs/requirements/06-fleet.md#host-services
//= type=test
//# The Fleet Controller SHALL run `buildkitd` in rootless mode
//# with the overlayfs snapshotter and SHALL configure its garbage collection
//# with the storage limit from the configuration.

func TestBuildkitdRootlessOverlayfsWithGC(t *testing.T) {
	t.Parallel()
	s := render(t, fleet.StepHostServices, fullInput())
	cfg := section(t, s, "buildkitd.toml\" 0644 <<FLR_EOF", "FLR_EOF\nthen")
	mustContain(t, cfg, "rootless = true", `snapshotter = "overlayfs"`, "gc = true",
		"gckeepstorage = 40960", // 40GiB in MB
		`keepDuration = "48h"`)
	mustContain(t, s, "rootlesskit --net=host", "buildkitd --rootless", "--oci-worker-snapshotter=overlayfs")
}

//= docs/requirements/06-fleet.md#host-services
//= type=test
//# The Fleet Controller SHALL configure `buildkitd` to resolve
//# images through the Host's registry mirror before reaching upstream
//# registries.

func TestBuildkitdResolvesThroughMirror(t *testing.T) {
	t.Parallel()
	s := render(t, fleet.StepHostServices, fullInput())
	cfg := section(t, s, "buildkitd.toml\" 0644 <<FLR_EOF", "FLR_EOF\nthen")
	mustContain(t, cfg,
		"[registry.\"docker.io\"]\n  mirrors = [\"172.31.0.1:5000\"]",
		"[registry.\"ghcr.io\"]\n  mirrors = [\"172.31.0.1:5000\"]",
		"[registry.\"172.31.0.1:5000\"]\n  http = true",
	)
	in := fullInput()
	in.HostServices.RegistryMirror.Enabled = ptr(false)
	off := render(t, fleet.StepHostServices, in)
	mustNotContain(t, section(t, off, "buildkitd.toml\" 0644 <<FLR_EOF", "FLR_EOF\nthen"), "mirrors")
}

//= docs/requirements/06-fleet.md#host-services
//= type=test
//# The Fleet Controller SHALL configure the registry mirror as a
//# pull-through cache for each upstream registry named in the configuration,
//# with credentials for upstreams that need them supplied from Systems
//# Manager parameters.

func TestRegistryMirrorPullsThroughEveryUpstream(t *testing.T) {
	t.Parallel()
	s := render(t, fleet.StepHostServices, fullInput())
	cfg := section(t, s, "zot/config.json\" 0644 <<'FLR_EOF'", "FLR_EOF\nthen")
	for _, u := range []string{"https://registry-1.docker.io", "https://ghcr.io"} {
		mustContain(t, cfg, `"`+u+`"`)
	}
	mustContain(t, cfg, `"onDemand": true`, `"credentialsFile": "/etc/flintlock-runner/zot/credentials.json"`)
	if strings.Count(cfg, `"onDemand": true`) != 2 {
		t.Error("want one on-demand registry per upstream")
	}
	// Only ghcr.io has a credential parameter.
	mustContain(t, s, "cred=$(secret 'registry_credential_1' '/flr/ghcr-token')")
	mustNotContain(t, s, "registry_credential_0")
}

//= docs/requirements/06-fleet.md#host-services
//= type=test
//# The Fleet Controller SHALL configure the Go module proxy to
//# store modules on the Host's cache volume and to fetch modules it does not
//# have from the public proxy for public paths and from the configured
//# version control host for the configured private module patterns.

func TestGoProxyStoresOnCacheVolumeAndFetchesByPattern(t *testing.T) {
	t.Parallel()
	s := render(t, fleet.StepHostServices, fullInput())
	cfg := section(t, s, "athens/config.toml\" 0644 <<FLR_EOF", "FLR_EOF\nthen")
	mustContain(t, s, "root='/var/lib/flintlock-runner/cache'")
	mustContain(t, cfg,
		`StorageType = "disk"`,
		`RootPath = "$root/goproxy"`,
		`"GOPROXY=https://proxy.golang.org",`,
		`"GOPRIVATE=gitlab.example.com/platform/*,gitlab.example.com/Tools",`,
	)
	mustContain(t, s, "machine %s login oauth2 password %s\\n' 'gitlab.example.com'")
}

//= docs/requirements/06-fleet.md#host-services
//= type=test
//# The Fleet Controller SHALL supply the Go module proxy with a
//# read-only credential for the private module patterns, read from the
//# configured Systems Manager parameter and held on the Host side only.

//= docs/requirements/09-security.md#host-services-security
//= type=test
//# The Fleet Controller SHALL NOT place upstream registry or proxy
//# credentials where a guest can read them, and SHALL configure the registry
//# mirror and Go module proxy to hold them on the Host side only.

func TestCredentialsStayOnHostSide(t *testing.T) {
	t.Parallel()
	s := render(t, fleet.StepHostServices, fullInput())
	mustContain(t, s,
		"go_cred=$(secret go_proxy_credential '/flr/goproxy-token')",
		"write_file /var/lib/flintlock-runner/athens-home/.netrc 0600 athens:athens",
		`write_file "$FLR_ETC/zot/credentials.json" 0600 zot:zot`,
		// The credential reaches GitLab in a header file, not argv.
		`-H @"$work/auth-header"`,
	)
	// Neither credential is written under the cache volume, which holds
	// what the services serve.
	for _, l := range splitLines(s) {
		if strings.Contains(l, "$root") && (strings.Contains(l, ".netrc") || strings.Contains(l, "credentials")) {
			t.Errorf("credential under the cache volume: %s", l)
		}
	}
	mustNotContain(t, s, "PRIVATE-TOKEN: $go_cred\" ", "curl -H \"PRIVATE-TOKEN")
}

//= docs/requirements/06-fleet.md#host-services
//= type=test
//# The Fleet Controller SHALL configure the Go module proxy to
//# skip checksum database verification for the private module patterns and
//# to verify every other module against the public checksum database.

func TestGoProxyChecksumsExceptPrivate(t *testing.T) {
	t.Parallel()
	s := render(t, fleet.StepHostServices, fullInput())
	cfg := section(t, s, "athens/config.toml\" 0644 <<FLR_EOF", "FLR_EOF\nthen")
	mustContain(t, cfg,
		`"GOSUMDB=sum.golang.org",`,
		`"GONOSUMDB=gitlab.example.com/platform/*,gitlab.example.com/Tools",`,
		`NoSumPatterns = ["gitlab.example.com/platform/*", "gitlab.example.com/Tools"]`,
		`SumDBs = ["https://sum.golang.org"]`,
	)
	mustNotContain(t, cfg, "GOSUMDB=off", "GOINSECURE", "GONOSUMCHECK", "GOFLAGS=-insecure", "GONOSUMDB=*")
	pub := render(t, fleet.StepHostServices, publicGoInput())
	pcfg := section(t, pub, "athens/config.toml\" 0644 <<FLR_EOF", "FLR_EOF\nthen")
	mustContain(t, pcfg, `"GOSUMDB=sum.golang.org",`)
	mustNotContain(t, pcfg, "GONOSUMDB", "NoSumPatterns", "GOPRIVATE")
}

//= docs/requirements/06-fleet.md#host-services
//= type=test
//# The Fleet Controller SHALL configure the Go module proxy to
//# serve private modules from its cache without contacting the version
//# control host again until the configured private module revalidation
//# interval elapses.

func TestGoProxyCachesPrivateListingsForRevalidationInterval(t *testing.T) {
	t.Parallel()
	s := render(t, fleet.StepHostServices, fullInput())
	loc := section(t, s, "    location ~ \"", "    }")
	mustContain(t, loc, "proxy_cache flr_goproxy_private;", "proxy_cache_valid 200 404 410 43200s;")
	mustContain(t, s, "keys_zone=flr_goproxy_private:10m inactive=43200s")

	patterns := append(fullInput().HostServices.GoProxy.PrivatePatterns, "gitlab.example.com/*/internal")
	re := regexp.MustCompile(privateRegex(patterns))
	for path, want := range map[string]bool{
		// A * matches one path element, as in GOPRIVATE.
		"/gitlab.example.com/team/internal/@v/list":       true,
		"/gitlab.example.com/team/sub/internal/@v/list":   false,
		"/gitlab.example.com/platform/svc/@v/list":        true,
		"/gitlab.example.com/platform/svc/sub/@latest":    true,
		"/gitlab.example.com/!tools/@v/list":              true,
		"/gitlab.example.com/platform/svc/@v/v1.0.0.zip":  false, // immutable, served from storage
		"/github.com/example/svc/@v/list":                 false, // public, not cached here
		"/gitlab.example.com/platformx/svc/@v/list":       false,
		"/gitlab.example.com/other/@latest":               false,
		"/gitlab.example.com/platform/svc/@v/v1.0.0.info": false,
	} {
		if got := re.MatchString(path); got != want {
			t.Errorf("private regex on %s = %v, want %v", path, got, want)
		}
	}
}

//= docs/requirements/06-fleet.md#host-services
//= type=test
//# The Fleet Controller SHALL configure the HTTP cache with one
//# cache path per configured upstream, each with its own size limit and
//# time-to-live.

func TestHTTPCachePathPerUpstream(t *testing.T) {
	t.Parallel()
	s := render(t, fleet.StepHostServices, fullInput())
	mustContain(t, s,
		"proxy_cache_path $root/http/npm levels=1:2 keys_zone=flr_http_npm:10m max_size=10737418240 inactive=172800s",
		"proxy_cache_path $root/http/pypi levels=1:2 keys_zone=flr_http_pypi:10m max_size=5368709120 inactive=86400s",
	)
	npm := section(t, s, "location /npm/ {", "}")
	mustContain(t, npm, "proxy_pass https://registry.npmjs.org/;", "proxy_cache flr_http_npm;", "proxy_cache_valid 200 301 302 172800s;")
	pypi := section(t, s, "location /pypi/ {", "}")
	mustContain(t, pypi, "proxy_pass https://pypi.org/simple/;", "proxy_cache flr_http_pypi;", "proxy_cache_valid 200 301 302 86400s;")
}

//= docs/requirements/06-fleet.md#host-services
//= type=test
//# The Fleet Controller SHALL place all Host Service storage on a
//# dedicated cache volume or directory whose total size is capped by the
//# configuration.

func TestCacheVolumeCapped(t *testing.T) {
	t.Parallel()
	dev := render(t, fleet.StepHostServices, fullInput())
	mustContain(t, dev, "cap_mb=204800", "backing='/dev/nvme2n1'",
		`mkfs.ext4 -q -L flr-cache "$backing" "${cap_mb}M"`, `mount "$root"`)
	// Every service's storage is under the mounted volume.
	for _, want := range []string{`root = "$root/buildkit"`, `RootPath = "$root/goproxy"`, `"rootDirectory": "/var/lib/flintlock-runner/cache/registry"`, "proxy_cache_path $root/http/npm"} {
		mustContain(t, dev, want)
	}
	in := fullInput()
	in.HostServices.CacheVolume = config.CacheVolume{Directory: "/srv/flr-cache", SizeCap: 50 << 30}
	d := render(t, fleet.StepHostServices, in)
	mustContain(t, d, "root='/srv/flr-cache'", "cap_mb=51200", `backing="${root%/}.img"`,
		`fallocate -l "${cap_mb}M" "$backing"`, "mount_opts=defaults,nofail,loop", `"rootDirectory": "/srv/flr-cache/registry"`)
}

//= docs/requirements/06-fleet.md#host-services
//= type=test
//# Where a Host Service is disabled in the configuration, the Fleet
//# Controller SHALL NOT install it and SHALL NOT open its port.

func TestDisabledHostServicesNotInstalledNorOpened(t *testing.T) {
	t.Parallel()
	in := fullInput()
	in.HostServices.GoProxy.Enabled = ptr(false)
	in.HostServices.RegistryMirror.Enabled = ptr(false)
	s := render(t, fleet.StepHostServices, in)
	mustContain(t, s, "retire flintlock-runner-athens", "retire flintlock-runner-zot", `ensure_service flintlock-runner-buildkitd`)
	mustNotContain(t, s, "athens/config.toml", "zot/config.json", "listen 172.31.0.1:3000", "zot-linux")
	n := render(t, fleet.StepNetworking, in)
	mustContain(t, n, "tcp dport { 1234, 3128 } accept")
	v := render(t, fleet.StepVerifyActive, in)
	mustNotContain(t, v, "flintlock-runner-athens", "flintlock-runner-zot")

	none := render(t, fleet.StepNetworking, sparseInput())
	mustNotContain(t, none, "1234", "3000", "5000", "3128")
	hs := render(t, fleet.StepHostServices, sparseInput())
	mustContain(t, hs, "retire flintlock-runner-buildkitd", "retire flintlock-runner-athens", "retire flintlock-runner-zot")
	mustNotContain(t, hs, "ensure_packages nginx", "ensure_service flintlock-runner")

	addr, err := ServiceAddresses(in.HostServices, "172.31.0.0/16")
	if err != nil {
		t.Fatal(err)
	}
	if addr.GoProxy != "" || addr.RegistryMirror != "" || addr.Buildkit == "" {
		t.Errorf("addresses = %+v", addr)
	}
}

//= docs/requirements/06-fleet.md#host-services
//= type=test
//# The Fleet Controller SHALL pre-warm the Go module proxy and the
//# registry mirror on each Host with the modules and images listed in the
//# configuration after provisioning.

func TestPrewarmFetchesThroughTheServices(t *testing.T) {
	t.Parallel()
	s := render(t, fleet.StepPrewarm, fullInput())
	mustContain(t, s,
		`proxy="http://172.31.0.1:3000"`,
		"modules=('golang.org/x/tools/@v/v0.30.0' 'github.com/!burnt!sushi/toml/@v/v1.4.0' )",
		`"$proxy/$path.$ext"`, "for ext in info mod zip",
		"mirror='172.31.0.1:5000'",
		"images=('docker.io/library/alpine:3.20')",
		`images pull --plain-http --platform "linux/$FLR_ARCH" "$mirror/$repo"`,
	)
	off := render(t, fleet.StepPrewarm, sparseInput())
	mustNotContain(t, off, `"$proxy/`, "images pull")
}

//= docs/requirements/09-security.md#host-services-security
//= type=test
//# The Fleet Controller SHALL run `buildkitd` as an unprivileged
//# user so that a build escaping its sandbox does not gain root on the Host.

func TestBuildkitdRunsUnprivileged(t *testing.T) {
	t.Parallel()
	s := render(t, fleet.StepHostServices, fullInput())
	unit := section(t, s, "flintlock-runner-buildkitd.service 0644 <<FLR_EOF", "FLR_EOF\nthen")
	mustContain(t, unit, "User=buildkit", "Group=buildkit", "$rk_dir/rootlesskit")
	mustNotContain(t, unit, "User=root", "Group=root")
	// Only the firewall load is privileged.
	if strings.Count(unit, "=+") != 1 || !strings.Contains(unit, "ExecStartPre=+/usr/sbin/nft") {
		t.Error("something other than the firewall load runs privileged")
	}
	mustContain(t, s, "ensure_user buildkit")
}

//= docs/requirements/09-security.md#host-services-security
//= type=test
//# Every Host Service SHALL be reachable only from the guest
//# subnet of its own Host and from the Host itself, and SHALL NOT be
//# reachable from other Hosts or from outside the Host.

func TestHostServicesReachableOnlyLocally(t *testing.T) {
	t.Parallel()
	n := render(t, fleet.StepNetworking, fullInput())
	input, _ := chains(t, n)
	mustContain(t, input, `iifname != { "flbr0", "lo" } ip daddr 172.31.0.1 tcp dport { 1234, 3000, 5000, 3128 } drop`)
	s := render(t, fleet.StepHostServices, fullInput())
	mustNotContain(t, s, "0.0.0.0", "listen 3000", "listen 3128")
}

//= docs/requirements/09-security.md#host-services-security
//= type=test
//# The credential the Go module proxy uses for private modules
//# SHALL be read-only and SHALL grant access to no groups or projects beyond
//# those covered by the configured private module patterns.

func TestGoProxyCredentialScopeChecked(t *testing.T) {
	t.Parallel()
	s := render(t, fleet.StepHostServices, fullInput())
	scopes := section(t, s, "for scope in", "done")
	mustContain(t, scopes, "read_repository | read_api) ;;", `*) die "the Go module proxy credential has scope $scope`)
	projects := section(t, s, "page=1", "\ndone\n")
	mustContain(t, projects,
		`"$api/projects?membership=true&simple=true&per_page=100&page=$page"`,
		`if [[ $m == gitlab.example.com/platform/* || $m == gitlab.example.com/platform/*/* ]]; then ok=1; fi`,
		`if [[ $m == gitlab.example.com/Tools || $m == gitlab.example.com/Tools/* ]]; then ok=1; fi`,
		`[ "$ok" = 1 ] || die "the Go module proxy credential reaches $m, outside the private module patterns"`,
	)
	// The checks run before the credential is installed.
	before(t, s, "for scope in", "write_file /var/lib/flintlock-runner/athens-home/.netrc")
	before(t, s, "outside the private module patterns", "write_file /var/lib/flintlock-runner/athens-home/.netrc")
}

//= docs/requirements/09-security.md#host-services-security
//= type=test
//# The Go module proxy SHALL NOT use a Job's token to fetch
//# modules.

func TestGoProxyDropsJobCredentials(t *testing.T) {
	t.Parallel()
	s := render(t, fleet.StepHostServices, fullInput())
	front := section(t, s, "listen 172.31.0.1:3000;", "location / {")
	mustContain(t, front, `proxy_set_header Authorization "";`, `proxy_set_header Cookie "";`)
	mustNotContain(t, s, "CI_JOB_TOKEN", "JOB-TOKEN", "GOAUTH")
	// The only credential Athens has is the fleet's .netrc.
	cfg := section(t, s, "athens/config.toml\" 0644 <<FLR_EOF", "FLR_EOF\nthen")
	mustContain(t, cfg, `"NETRC=/var/lib/flintlock-runner/athens-home/.netrc",`)
}

//= docs/requirements/06-fleet.md#host-services
//= type=test
//# The Fleet Controller SHALL record the address and port of each
//# Host Service in the Host's Inventory entry.

func TestServiceAddresses(t *testing.T) {
	t.Parallel()
	in := fullInput()
	in.HostServices.Buildkit.Port = 1300
	a, err := ServiceAddresses(in.HostServices, "10.9.0.0/24")
	if err != nil {
		t.Fatal(err)
	}
	want := config.HostServiceAddresses{
		Buildkit: "tcp://10.9.0.1:1300", GoProxy: "http://10.9.0.1:3000", RegistryMirror: "http://10.9.0.1:5000",
		HTTPCache: map[string]string{"npm": "http://10.9.0.1:3128/npm", "pypi": "http://10.9.0.1:3128/pypi"},
	}
	if a.Buildkit != want.Buildkit || a.GoProxy != want.GoProxy || a.RegistryMirror != want.RegistryMirror ||
		a.HTTPCache["npm"] != want.HTTPCache["npm"] || a.HTTPCache["pypi"] != want.HTTPCache["pypi"] || len(a.HTTPCache) != 2 {
		t.Errorf("addresses = %+v, want %+v", a, want)
	}
}
