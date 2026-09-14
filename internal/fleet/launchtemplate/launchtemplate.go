// Package launchtemplate is the Fleet Controller's launch template mode
// (docs/requirements/06-fleet.md#launch-template-mode): `fleet
// emit-userdata` produces a cloud-init user-data script with which instances
// launched by an auto scaling group provision themselves at first boot,
// reading their secrets from Systems Manager parameters (FL-090, FL-091).
// The Runner's side of the mode, refreshing the Inventory by tag discovery
// (FL-092), is discovery.Refresher.
package launchtemplate

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/phoban01/flintlock-runner/internal/config"
	"github.com/phoban01/flintlock-runner/internal/fleet"
)

// DefaultSecretsDir is where the first-boot script writes the secrets it
// reads, each in its own owner-only file. FLINTLOCK_RUNNER_SECRETS_DIR in
// the instance's environment overrides it.
const DefaultSecretsDir = "/run/flintlock-runner/secrets"

// Options passed to Scripts.Render for fleet.StepUserData. Each value is the
// name of the environment variable that holds, when the steps run, the path
// of the file with that secret, for example
// Options["host_token_file_env"] = "FLINTLOCK_RUNNER_HOST_TOKEN_FILE". The
// TLS options are absent when flintlockd is configured insecure.
const (
	OptionHostTokenFile = "host_token_file_env"
	OptionTLSCAFile     = "tls_ca_file_env"
	OptionTLSCertFile   = "tls_cert_file_env"
	OptionTLSKeyFile    = "tls_key_file_env"
)

// parameterName is what a Systems Manager parameter name may contain; the
// names are quoted into the script, so nothing else is accepted.
var parameterName = regexp.MustCompile(`^[A-Za-z0-9_./-]+$`)

// secret is one parameter the first-boot script reads.
type secret struct {
	option string
	env    string
	file   string
	param  string
}

// Emitter is fleet.UserDataEmitter.
type Emitter struct {
	cfg     *config.Config
	scripts fleet.Scripts
	params  fleet.Parameters
}

var _ fleet.UserDataEmitter = (*Emitter)(nil)

// NewEmitter returns the emitter for cfg. scripts renders the provisioning
// steps as fleet.StepUserData. params, when not nil, is used to check that
// every configured parameter exists and that no value read from it ends up
// in the user-data; the values are never written anywhere.
func NewEmitter(cfg *config.Config, scripts fleet.Scripts, params fleet.Parameters) *Emitter {
	return &Emitter{cfg: cfg, scripts: scripts, params: params}
}

func (e *Emitter) secrets() ([]secret, error) {
	f := e.cfg.Fleet
	if f == nil || f.LaunchTemplate == nil {
		return nil, errors.New("launchtemplate: launch template mode is not selected (fleet.launch_template)")
	}
	p := f.LaunchTemplate.Parameters
	all := []secret{
		{option: OptionHostTokenFile, env: "FLINTLOCK_RUNNER_HOST_TOKEN_FILE", file: "host-token", param: p.HostToken},
	}
	if !f.Flintlockd.Insecure {
		all = append(all,
			secret{option: OptionTLSCAFile, env: "FLINTLOCK_RUNNER_TLS_CA_FILE", file: "tls-ca.pem", param: p.TLSCA},
			secret{option: OptionTLSCertFile, env: "FLINTLOCK_RUNNER_TLS_CERT_FILE", file: "tls-cert.pem", param: p.TLSCert},
			secret{option: OptionTLSKeyFile, env: "FLINTLOCK_RUNNER_TLS_KEY_FILE", file: "tls-key.pem", param: p.TLSKey},
		)
	}
	for _, s := range all {
		if s.param == "" {
			return nil, fmt.Errorf("launchtemplate: fleet.launch_template.parameters needs a parameter for %s", s.file)
		}
		if !parameterName.MatchString(s.param) {
			return nil, fmt.Errorf("launchtemplate: %q is not a Systems Manager parameter name", s.param)
		}
	}
	return all, nil
}

//= docs/requirements/06-fleet.md#launch-template-mode
//# Where launch template mode is selected, the Fleet Controller
//# SHALL emit a cloud-init user-data script that performs the Host
//# provisioning steps unattended at first boot so that instances launched by
//# an auto scaling group self-provision.

// Emit implements fleet.UserDataEmitter. The result is a bash script, which
// cloud-init runs once at first boot as root with no terminal. It reads the
// secrets, then runs the provisioning steps rendered for
// fleet.StepUserData with standard input from /dev/null.
func (e *Emitter) Emit(ctx context.Context) ([]byte, error) {
	secrets, err := e.secrets()
	if err != nil {
		return nil, err
	}
	known, err := e.knownSecrets(ctx, secrets)
	if err != nil {
		return nil, err
	}
	f := e.cfg.Fleet
	opts := map[string]string{}
	for _, s := range secrets {
		opts[s.option] = s.env
	}
	body, err := e.scripts.Render(fleet.StepUserData, fleet.RenderInput{
		Fleet:        *f,
		HostServices: e.cfg.HostServices,
		Profiles:     e.cfg.Profiles,
		Options:      opts,
	})
	if err != nil {
		return nil, fmt.Errorf("launchtemplate: render provisioning steps: %w", err)
	}
	out := compose(f.Region, secrets, body.Content)
	if err := refuseEmbedded(out, known); err != nil {
		return nil, err
	}
	return []byte(out), nil
}

// knownSecrets returns the secret values the emitter can see, so that it can
// refuse user-data carrying one: the configured host token and, when params
// is set, every parameter's value, which also proves each exists.
func (e *Emitter) knownSecrets(ctx context.Context, secrets []secret) ([]string, error) {
	var known []string
	if t := string(e.cfg.Fleet.Flintlockd.Token); t != "" {
		known = append(known, t)
	}
	if e.params == nil {
		return known, nil
	}
	for _, s := range secrets {
		v, err := e.params.GetParameter(ctx, s.param)
		if err != nil {
			return nil, fmt.Errorf("launchtemplate: parameter %s for %s: %w", s.param, s.file, err)
		}
		if strings.TrimSpace(v) != "" {
			known = append(known, strings.TrimSpace(v))
		}
	}
	return known, nil
}

//= docs/requirements/06-fleet.md#launch-template-mode
//# Where launch template mode is selected, the Fleet Controller
//# SHALL read secrets for the emitted script from the configured Systems
//# Manager parameters rather than embedding them in the user-data.

// refuseEmbedded fails when out contains any known secret value (SE-014).
func refuseEmbedded(out string, known []string) error {
	for _, v := range known {
		if strings.Contains(out, v) {
			return errors.New("launchtemplate: the user-data would embed a secret; secrets have to come from the Systems Manager parameters")
		}
	}
	return nil
}

// compose builds the user-data: a preamble that reads each secret from its
// parameter into an owner-only file, then the provisioning steps, written to
// a file through a quoted here-document and run by bash.
func compose(region string, secrets []secret, body string) string {
	var b strings.Builder
	b.WriteString("#!/bin/bash\n")
	b.WriteString("# flintlock-runner launch template user-data, generated by `flintlock-runner fleet emit-userdata`.\n")
	b.WriteString("# cloud-init runs it once at first boot. It holds no secret: they are read\n")
	b.WriteString("# from Systems Manager parameters with the instance's role.\n")
	b.WriteString("set -euo pipefail\n")
	b.WriteString("umask 077\n")
	b.WriteString("secrets=\"${FLINTLOCK_RUNNER_SECRETS_DIR:-" + DefaultSecretsDir + "}\"\n")
	b.WriteString("mkdir -p \"$secrets\"\n")
	b.WriteString("chmod 700 \"$secrets\"\n")
	b.WriteString("region=" + shellQuote(region) + "\n")
	b.WriteString("fetch_parameter() {\n")
	b.WriteString("  aws ssm get-parameter --region \"$region\" --name \"$1\" --with-decryption --query Parameter.Value --output text >\"$secrets/$2\"\n")
	b.WriteString("}\n")
	for _, s := range secrets {
		b.WriteString("fetch_parameter " + shellQuote(s.param) + " " + shellQuote(s.file) + "\n")
		b.WriteString("export " + s.env + "=\"$secrets/" + s.file + "\"\n")
	}
	delim := "FLINTLOCK_RUNNER_USERDATA_EOF"
	for n := 0; containsLine(body, delim); n++ {
		delim = "FLINTLOCK_RUNNER_USERDATA_EOF_" + strconv.Itoa(n)
	}
	if !strings.HasSuffix(body, "\n") {
		body += "\n"
	}
	b.WriteString("steps=$(mktemp)\n")
	b.WriteString("trap 'rm -f \"$steps\"' EXIT\n")
	b.WriteString("cat >\"$steps\" <<'" + delim + "'\n")
	b.WriteString(body)
	b.WriteString(delim + "\n")
	b.WriteString("bash \"$steps\" </dev/null\n")
	return b.String()
}

// MaxUserDataBytes is EC2's limit on an instance's user-data, counted
// before base64 encoding. A launch template whose user-data is longer is
// rejected, and so is every instance an auto scaling group launches from
// it.
const MaxUserDataBytes = 16384

// Compress returns user-data gzip-compressed, which cloud-init detects and
// decompresses before running the script. The script embeds every Host
// provisioning step and is several times EC2's limit uncompressed, so this
// is the form a launch template carries. The output is deterministic, and
// a result over MaxUserDataBytes is an error rather than a launch template
// EC2 would refuse.
func Compress(userData []byte) ([]byte, error) {
	var b bytes.Buffer
	zw, err := gzip.NewWriterLevel(&b, gzip.BestCompression)
	if err != nil {
		return nil, err
	}
	if _, err := zw.Write(userData); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	if b.Len() > MaxUserDataBytes {
		return nil, fmt.Errorf("launchtemplate: the user-data is %d bytes gzip-compressed (%d uncompressed), over EC2's limit of %d", b.Len(), len(userData), MaxUserDataBytes)
	}
	return b.Bytes(), nil
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func containsLine(s, line string) bool {
	for l := range strings.Lines(s) {
		if strings.TrimSuffix(l, "\n") == line {
			return true
		}
	}
	return false
}
