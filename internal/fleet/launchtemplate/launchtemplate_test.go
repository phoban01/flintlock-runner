package launchtemplate

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/phoban01/flintlock-runner/internal/config"
	"github.com/phoban01/flintlock-runner/internal/fleet"
	"github.com/phoban01/flintlock-runner/internal/fleet/awsfake"
)

// stubScripts renders fleet.StepUserData as body and records the input.
type stubScripts struct {
	body string
	got  []fleet.RenderInput
}

func (s *stubScripts) Render(step fleet.Step, in fleet.RenderInput) (fleet.Script, error) {
	if step != fleet.StepUserData {
		return fleet.Script{}, errors.New("unexpected step " + string(step))
	}
	s.got = append(s.got, in)
	return fleet.Script{Name: string(step), Content: s.body}, nil
}

func (s *stubScripts) All() []fleet.Step { return []fleet.Step{fleet.StepUserData} }

var params = config.LaunchTemplateParameters{
	HostToken: "/flintlock-runner/ci/host-token",
	TLSCA:     "/flintlock-runner/ci/tls-ca",
	TLSCert:   "/flintlock-runner/ci/tls-cert",
	TLSKey:    "/flintlock-runner/ci/tls-key",
}

func launchTemplateConfig() *config.Config {
	return &config.Config{Fleet: &config.Fleet{
		Region:         "eu-west-1",
		Discovery:      config.Discovery{TagKey: "flintlock-runner", TagValue: "ci"},
		Flintlockd:     config.Flintlockd{Port: 9090, Token: "configured-token-value"},
		LaunchTemplate: &config.LaunchTemplate{Parameters: params},
	}}
}

func secretValues() map[string]string {
	return map[string]string{
		params.HostToken: "host-token-from-ssm",
		params.TLSCA:     "-----BEGIN CERTIFICATE-----ca",
		params.TLSCert:   "-----BEGIN CERTIFICATE-----cert",
		params.TLSKey:    "-----BEGIN PRIVATE KEY-----key",
	}
}

// provisioningSteps stands in for the rendered provisioning steps: it
// proves it runs without a terminal and can read each secret through the
// environment variable its option names.
const provisioningSteps = `#!/bin/bash
set -euo pipefail
if read -r _; then echo "steps got stdin"; exit 1; fi
echo "provisioning with token $(cat "$FLINTLOCK_RUNNER_HOST_TOKEN_FILE")"
echo "key $(cat "$FLINTLOCK_RUNNER_TLS_KEY_FILE")"
`

//= docs/requirements/06-fleet.md#launch-template-mode
//= type=test
//# Where launch template mode is selected, the Fleet Controller
//# SHALL emit a cloud-init user-data script that performs the Host
//# provisioning steps unattended at first boot so that instances launched by
//# an auto scaling group self-provision.

// TestEmittedUserDataRunsTheProvisioningStepsUnattended emits the user-data
// and runs it as cloud-init would, as a script with no standard input, with
// a stand-in aws CLI on PATH, and checks the provisioning steps ran with the
// secrets in owner-only files.
func TestEmittedUserDataRunsTheProvisioningStepsUnattended(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not installed")
	}
	cfg := launchTemplateConfig()
	scripts := &stubScripts{body: provisioningSteps}
	out, err := NewEmitter(cfg, scripts, awsfake.NewParameters(secretValues())).Emit(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(out, []byte("#!/bin/bash\n")) {
		t.Fatalf("user-data does not start with a shebang, so cloud-init would not run it:\n%s", out)
	}
	if len(scripts.got) != 1 || scripts.got[0].Fleet.Region != "eu-west-1" {
		t.Fatalf("provisioning steps rendered with %+v", scripts.got)
	}
	wantOpts := map[string]string{
		OptionHostTokenFile: "FLINTLOCK_RUNNER_HOST_TOKEN_FILE",
		OptionTLSCAFile:     "FLINTLOCK_RUNNER_TLS_CA_FILE",
		OptionTLSCertFile:   "FLINTLOCK_RUNNER_TLS_CERT_FILE",
		OptionTLSKeyFile:    "FLINTLOCK_RUNNER_TLS_KEY_FILE",
	}
	if !reflect.DeepEqual(scripts.got[0].Options, wantOpts) {
		t.Errorf("render options = %v, want %v", scripts.got[0].Options, wantOpts)
	}

	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	// The stand-in aws CLI answers get-parameter from a fixed table and
	// fails on anything else.
	awsCLI := "#!/usr/bin/env bash\nset -eu\nname=''\nwhile [ $# -gt 0 ]; do if [ \"$1\" = --name ]; then name=$2; fi; shift; done\n" +
		"case \"$name\" in\n"
	for k, v := range secretValues() {
		awsCLI += "  '" + k + "') printf '%s\\n' '" + v + "' ;;\n"
	}
	awsCLI += "  *) echo \"unknown parameter $name\" >&2; exit 1 ;;\nesac\n"
	if err := os.WriteFile(filepath.Join(bin, "aws"), []byte(awsCLI), 0o755); err != nil {
		t.Fatal(err)
	}
	userData := filepath.Join(dir, "user-data")
	if err := os.WriteFile(userData, out, 0o755); err != nil {
		t.Fatal(err)
	}
	secrets := filepath.Join(dir, "secrets")
	cmd := exec.Command("bash", userData)
	cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "FLINTLOCK_RUNNER_SECRETS_DIR="+secrets)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("user-data failed: %v\nstdout:\n%s\nstderr:\n%s", err, stdout.String(), stderr.String())
	}
	want := "provisioning with token host-token-from-ssm\nkey -----BEGIN PRIVATE KEY-----key\n"
	if stdout.String() != want {
		t.Errorf("steps printed %q, want %q (stderr %q)", stdout.String(), want, stderr.String())
	}
	if fi, err := os.Stat(secrets); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("secrets dir: %v, %v", fi, err)
	}
	for _, f := range []string{"host-token", "tls-ca.pem", "tls-cert.pem", "tls-key.pem"} {
		if fi, err := os.Stat(filepath.Join(secrets, f)); err != nil || fi.Mode().Perm() != 0o600 {
			t.Errorf("secret file %s: %v, %v; want mode 0600", f, fi, err)
		}
	}
}

//= docs/requirements/06-fleet.md#launch-template-mode
//= type=test
//# Where launch template mode is selected, the Fleet Controller
//# SHALL read secrets for the emitted script from the configured Systems
//# Manager parameters rather than embedding them in the user-data.

// TestUserDataReadsSecretsFromParametersAndEmbedsNone checks the script
// names each configured parameter, that no secret value is in it, that each
// parameter was checked to exist, and that rendered steps embedding a secret
// are refused.
func TestUserDataReadsSecretsFromParametersAndEmbedsNone(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cfg := launchTemplateConfig()
	p := awsfake.NewParameters(secretValues())
	out, err := NewEmitter(cfg, &stubScripts{body: "true\n"}, p).Emit(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{params.HostToken, params.TLSCA, params.TLSCert, params.TLSKey} {
		if !bytes.Contains(out, []byte("fetch_parameter '"+name+"'")) {
			t.Errorf("user-data does not read %s", name)
		}
	}
	if !bytes.Contains(out, []byte("--with-decryption")) {
		t.Error("user-data does not decrypt parameters")
	}
	for _, v := range append([]string{string(cfg.Fleet.Flintlockd.Token)}, valuesOf(secretValues())...) {
		if bytes.Contains(out, []byte(v)) {
			t.Errorf("user-data embeds secret %q", v)
		}
	}
	if got := p.ParameterCalls(); len(got) != 4 {
		t.Errorf("parameters checked: %v", got)
	}

	// Steps that embed a secret, from the configuration or from a
	// parameter, are refused.
	for _, leaked := range []string{string(cfg.Fleet.Flintlockd.Token), secretValues()[params.TLSKey]} {
		_, err := NewEmitter(cfg, &stubScripts{body: "echo '" + leaked + "' > /etc/flintlockd/token\n"}, p).Emit(ctx)
		if err == nil || !strings.Contains(err.Error(), "embed a secret") {
			t.Errorf("steps embedding %q: err = %v", leaked, err)
		}
	}

	// A configured parameter that does not exist fails the emit.
	missing := awsfake.NewParameters(secretValues())
	cfg2 := launchTemplateConfig()
	cfg2.Fleet.LaunchTemplate.Parameters.TLSKey = "/flintlock-runner/ci/absent"
	if _, err := NewEmitter(cfg2, &stubScripts{body: "true\n"}, missing).Emit(ctx); !errors.Is(err, awsfake.ErrParameterNotFound) {
		t.Errorf("missing parameter: err = %v", err)
	}
}

func TestEmitRequiresLaunchTemplateMode(t *testing.T) {
	t.Parallel()
	cfg := launchTemplateConfig()
	cfg.Fleet.LaunchTemplate = nil
	if _, err := NewEmitter(cfg, &stubScripts{}, nil).Emit(context.Background()); err == nil {
		t.Error("emitted user-data without launch template mode")
	}
	cfg = launchTemplateConfig()
	cfg.Fleet.LaunchTemplate.Parameters.HostToken = "bad name; rm -rf /"
	if _, err := NewEmitter(cfg, &stubScripts{}, nil).Emit(context.Background()); err == nil {
		t.Error("accepted a parameter name that is not one")
	}
	cfg = launchTemplateConfig()
	cfg.Fleet.Flintlockd.Insecure = true
	cfg.Fleet.LaunchTemplate.Parameters = config.LaunchTemplateParameters{HostToken: params.HostToken}
	s := &stubScripts{body: "true\n"}
	if _, err := NewEmitter(cfg, s, nil).Emit(context.Background()); err != nil {
		t.Errorf("insecure flintlockd needs only the token: %v", err)
	}
}

func valuesOf(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	return out
}
