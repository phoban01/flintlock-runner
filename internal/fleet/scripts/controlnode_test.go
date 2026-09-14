package scripts

import (
	"strings"
	"testing"

	"github.com/phoban01/flintlock-runner/internal/fleet"
)

//= docs/requirements/06-fleet.md#pool-manager-install
//= type=test
//# The Fleet Controller SHALL install the pinned Pool Manager daemon
//# on the Control Node with a host list generated from the Inventory that
//# names every Host with its `flintlockd` endpoint, token and TLS settings.

func TestControlNodeInstallsPinnedDaemonWithHostList(t *testing.T) {
	t.Parallel()
	s := render(t, fleet.StepControlNode, fullInput())
	mustContain(t, s,
		"version='v0.1.0'",
		`asset="poolmgrd_${version#v}_linux_$FLR_ARCH.tar.gz"`,
		`release="https://github.com/liquidmetal-dev/battery/releases/download/$version"`,
		`sha256sum -c --quiet -`,
		`host_tls="{\"ca_file\": $(json_str '/etc/flintlock-runner/pki/ca.pem')}"`,
		`token=$(secret flintlockd_token '/flr/host-token')`,
		`"$(json_str "$token")" "$host_tls"`,
		`printf '  "api_server": {"addr": %s, "tls": %s}\n' "$(json_str '10.0.0.5:9443')"`,
		`write_file "$etc/config.json" 0600 poolmgrd:poolmgrd`,
		`ExecStart=$bin -config $etc/config.json -db $db_dir/poolmgr.db`,
	)
	for _, h := range fullInput().Inventory.Hosts {
		mustContain(t, s, `"$(json_str '`+h.Name+`')" "$(json_str '`+h.Endpoint+`')"`)
	}
	mustNotContain(t, s, string(fullInput().Fleet.Flintlockd.Token))
}

//= docs/requirements/06-fleet.md#pool-manager-install
//= type=test
//# The Fleet Controller SHALL name each Host identically in the
//# Pool Manager's host list and in the Inventory, because the Runner joins
//# the two by name when resolving Placement.

func TestControlNodeHostListUsesInventoryNames(t *testing.T) {
	t.Parallel()
	in := fullInput()
	in.Inventory.Hosts[1].Name = "renamed-host"
	s := render(t, fleet.StepControlNode, in)
	hosts := section(t, s, `echo '  "hosts": ['`, `echo '  ],'`)
	if n := strings.Count(hosts, "printf '   %s{\"name\": %s"); n != len(in.Inventory.Hosts) {
		t.Errorf("host list has %d entries, want %d", n, len(in.Inventory.Hosts))
	}
	for _, h := range in.Inventory.Hosts {
		mustContain(t, hosts, `"$(json_str '`+h.Name+`')"`)
	}
	mustNotContain(t, hosts, "i-0bbb")
}

//= docs/requirements/06-fleet.md#pool-manager-install
//= type=test
//# When run again after the Inventory changes, the Fleet Controller
//# SHALL regenerate the Pool Manager's host list and reload the daemon without
//# interrupting existing Leases.

func TestControlNodeReloadKeepsLeases(t *testing.T) {
	t.Parallel()
	s := render(t, fleet.StepControlNode, fullInput())
	// The daemon is restarted only when the host list or the unit changed.
	mustContain(t, s,
		"if write_file \"$etc/config.json\" 0600 poolmgrd:poolmgrd <\"$new_config\"; then\n  restart=1\nfi",
		`ensure_service poolmgrd "$restart"`)
	// The Lease database is never removed or moved aside.
	mustNotContain(t, s, "rm -f \"$db_dir", "rm -rf \"$db_dir", "poolmgr.db\"", "systemctl stop poolmgrd", "rm -f /var/lib/poolmgrd")
	mustContain(t, s, "db_dir=/var/lib/poolmgrd")
	// A different Inventory renders a different host list.
	in := fullInput()
	in.Inventory.Hosts = append(in.Inventory.Hosts, in.Inventory.Hosts[0])
	in.Inventory.Hosts[3].Name, in.Inventory.Hosts[3].Endpoint = "i-0ddd", "10.0.3.13:9090"
	if s2 := render(t, fleet.StepControlNode, in); !strings.Contains(s2, "i-0ddd") || strings.Contains(s, "i-0ddd") {
		t.Error("host list not regenerated from the Inventory")
	}
}

//= docs/requirements/06-fleet.md#pool-manager-install
//= type=test
//# The Fleet Controller SHALL verify that each installed service is
//# active before reporting an instance as provisioned.

func TestVerifyActiveChecksEveryInstalledUnit(t *testing.T) {
	t.Parallel()
	s := render(t, fleet.StepVerifyActive, fullInput())
	mustContain(t, s,
		"units=(containerd flintlockd flintlock-runner-network dnsmasq flintlock-runner-buildkitd flintlock-runner-athens flintlock-runner-zot nginx)",
		`systemctl is-active --quiet "$u"`,
		`die "some services are not active"`,
	)
	out := ParseOutput("::active:: containerd\n::inactive:: nginx failed\n")
	if len(out.Inactive) != 1 || out.Inactive[0] != "nginx" {
		t.Errorf("inactive = %v", out.Inactive)
	}
}

//= docs/requirements/06-fleet.md#host-services
//= type=test
//# The verification command SHALL, from inside a verification
//# MicroVM on each Host, build a trivial image with `buildctl`, fetch a
//# module through the Go module proxy, pull an image through the registry
//# mirror and fetch one object through the HTTP cache, and SHALL report the
//# outcome per service per Host.

func TestGuestVerifyExercisesEveryService(t *testing.T) {
	t.Parallel()
	s := render(t, fleet.StepGuestVerify, fullInput())
	mustContain(t, s,
		"buildctl --addr 'tcp://172.31.0.1:1234' build --frontend dockerfile.v0",
		"curl -fsS -o /dev/null 'http://172.31.0.1:3000/golang.org/x/tools/@v/v0.30.0.zip'",
		"'http://172.31.0.1:5000/v2/library/alpine/manifests/latest'",
		"curl -fsS -o /dev/null 'http://172.31.0.1:3128/npm/'",
		"curl -fsS -o /dev/null 'http://172.31.0.1:3128/pypi/'",
		"report buildkit ok", "report go_proxy ok", "report registry_mirror ok", "report 'http_cache/npm' ok",
	)
	// It must not need root or anything from the Host prelude.
	mustNotContain(t, s, `die "must run as root"`, "apt-get")

	out := ParseOutput("::service:: buildkit ok\n::service:: go_proxy fail curl: (7) refused\n::service:: http_cache/npm ok\n")
	if len(out.Services) != 3 || out.Services[0].Err != nil || out.Services[1].Err == nil ||
		out.Services[1].Service != "go_proxy" || !strings.Contains(out.Services[1].Err.Error(), "refused") {
		t.Errorf("services = %+v", out.Services)
	}
	// A Host without an Inventory entry reports that rather than nothing.
	in := fullInput()
	in.Instance.ID = "i-unknown"
	mustContain(t, render(t, fleet.StepGuestVerify, in), `report inventory "fail no Inventory entry for this Host"`)
}

func TestUserDataEmbedsHostStepsWithoutSecrets(t *testing.T) {
	t.Parallel()
	in := fullInput()
	in.Instance = fleet.Instance{}
	s := render(t, fleet.StepUserData, in)
	for _, step := range userDataSteps {
		mustContain(t, s, "cat >\"$steps_dir/"+string(step)+".sh\" <<'FLR_STEP_EOF'")
	}
	mustContain(t, s, `FLR_ARCH="$(host_arch)"`, "token=$(secret flintlockd_token '/flr/host-token')")
	mustNotContain(t, s, string(in.Fleet.Flintlockd.Token))
}

func TestDrainAndTeardownGuards(t *testing.T) {
	t.Parallel()
	d := render(t, fleet.StepDrain, fullInput())
	mustNotContain(t, d, "systemctl stop flintlockd")
	in := fullInput()
	in.Options[OptionStopFlintlockd] = "true"
	d = render(t, fleet.StepDrain, in)
	before(t, d, `MicroVMs still run on this Host; flintlockd stays up`, "systemctl stop flintlockd")

	td := render(t, fleet.StepTeardown, fullInput())
	mustNotContain(t, td, "vgremove", "pvremove")
	in.Options[OptionPurge] = "true"
	mustContain(t, render(t, fleet.StepTeardown, in), `vgremove -f -y "$vg"`)
}
