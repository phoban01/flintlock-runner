package config

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// updateGolden rewrites testdata/*.golden instead of comparing against them:
// `go test ./internal/config -update`.
var updateGolden = flag.Bool("update", false, "rewrite the golden files under testdata")

// secretValues are every plaintext secret testdata/full.yaml carries, plus
// the values the environment overrides them with. None of them may appear in
// `config show` output.
var secretValues = map[string]string{
	EnvGitLabToken:         "glrt-env-token",
	HostTokenEnv("host-a"): "host-a-env-token",
	HostTokenEnv("host-b"): "host-b-env-token",
	EnvFleetHostToken:      "fleet-env-token",
}

// fileSecrets are the plaintext secrets written in testdata/full.yaml.
var fileSecrets = []string{"glrt-file-token", "host-a-file-token", "host-b-file-token", "fleet-file-token"}

//= docs/requirements/07-configuration.md#file-and-precedence
//= type=test
//# The Runner SHALL provide a `config show` command that prints the
//# effective configuration with every secret value redacted.

// TestShowRedactsEverySecret is the guard against a secret leaking into the
// output of `config show`. It renders the complete example, from the file
// and with every secret overridden from the environment, and checks that no
// plaintext secret appears, that every non-empty Secret field of the printed
// copy is the redaction marker, and that the output still matches the golden
// file byte for byte, so that a schema change that added an unredacted
// secret would fail loudly here.
func TestShowRedactsEverySecret(t *testing.T) {
	t.Parallel()

	for _, env := range []struct {
		name   string
		lookup LookupEnv
		values []string
	}{
		{"secrets from the file", noEnv, fileSecrets},
		{"secrets from the environment", mapEnv(secretValues), append(valuesOf(secretValues), fileSecrets...)},
	} {
		t.Run(env.name, func(t *testing.T) {
			t.Parallel()
			cfg := loadTestdata(t, "full.yaml", WithEnv(env.lookup))
			var buf bytes.Buffer
			if err := Show(&buf, cfg); err != nil {
				t.Fatal(err)
			}
			out := buf.String()
			for _, secret := range env.values {
				if secret != "" && strings.Contains(out, secret) {
					t.Errorf("`config show` leaked the secret %q:\n%s", secret, out)
				}
			}
			if got := strings.Count(out, Redacted); got != len(fileSecrets) {
				t.Errorf("%d redaction markers, want %d, one per secret:\n%s", got, len(fileSecrets), out)
			}
			// The output is the effective configuration, so the defaults and
			// the environment overrides are in it, not the raw file.
			for _, want := range []string{"namespace: metal-runner", "name: ubuntu-arm64", "state_dir: /var/lib/flintlock-runner"} {
				if !strings.Contains(out, want) {
					t.Errorf("output lacks %q:\n%s", want, out)
				}
			}
		})
	}

	t.Run("every Secret field of the printed copy is redacted", func(t *testing.T) {
		t.Parallel()
		cfg := loadTestdata(t, "full.yaml", WithEnv(mapEnv(secretValues)))
		var found, redacted int
		walkSecrets(reflect.ValueOf(cfg.Redacted()), func(s Secret) {
			if s == "" {
				return
			}
			found++
			if s == Redacted {
				redacted++
			}
		})
		if found == 0 {
			t.Fatal("no Secret fields were reached; the walk is broken")
		}
		if redacted != found {
			t.Errorf("%d of %d non-empty Secret fields are redacted", redacted, found)
		}
	})

	t.Run("the configuration itself keeps its secrets", func(t *testing.T) {
		t.Parallel()
		cfg := loadTestdata(t, "full.yaml")
		_ = cfg.Redacted()
		var buf bytes.Buffer
		if err := Show(&buf, cfg); err != nil {
			t.Fatal(err)
		}
		if string(cfg.GitLab.Token) != "glrt-file-token" {
			t.Errorf("Show or Redacted modified the configuration: token = %q", string(cfg.GitLab.Token))
		}
		if string(cfg.Inventory.Hosts[0].Token) != "host-a-file-token" {
			t.Errorf("Show or Redacted modified the Inventory: token = %q", string(cfg.Inventory.Hosts[0].Token))
		}
	})

	t.Run("golden", func(t *testing.T) {
		t.Parallel()
		cfg := loadTestdata(t, "full.yaml")
		var buf bytes.Buffer
		if err := Show(&buf, cfg); err != nil {
			t.Fatal(err)
		}
		golden := filepath.Join("testdata", "full.show.golden")
		if *updateGolden {
			writeFile(t, golden, buf.String())
			return
		}
		want, err := os.ReadFile(golden)
		if err != nil {
			t.Fatal(err)
		}
		if buf.String() != string(want) {
			t.Errorf("`config show` output differs from %s; re-run with -update after checking the diff.\ngot:\n%s", golden, buf.String())
		}
	})
}

// TestSecretNeverFormatsItsValue checks the other half of redaction: a
// Secret rendered by fmt, slog or an error message is the marker, so a
// secret cannot reach a log by being interpolated.
func TestSecretNeverFormatsItsValue(t *testing.T) {
	t.Parallel()
	s := Secret("glrt-secret")
	for _, got := range []string{
		s.String(),
		strings.TrimSpace(strings.ReplaceAll(s.LogValue().String(), "\n", "")),
	} {
		if strings.Contains(got, "glrt-secret") {
			t.Errorf("Secret rendered its value: %q", got)
		}
		if got != Redacted {
			t.Errorf("Secret rendered %q, want %q", got, Redacted)
		}
	}
	if Secret("").String() != "" {
		t.Error("an unset Secret renders as the redaction marker")
	}
}

// valuesOf is the values of m, in no particular order.
func valuesOf(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	return out
}

// walkSecrets calls fn for every Secret reachable from v, so the test does
// not have to name each Secret field of the schema.
func walkSecrets(v reflect.Value, fn func(Secret)) {
	switch v.Kind() {
	case reflect.Pointer, reflect.Interface:
		if !v.IsNil() {
			walkSecrets(v.Elem(), fn)
		}
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			walkSecrets(v.Field(i), fn)
		}
	case reflect.Slice, reflect.Array:
		for i := 0; i < v.Len(); i++ {
			walkSecrets(v.Index(i), fn)
		}
	case reflect.Map:
		for _, k := range v.MapKeys() {
			walkSecrets(v.MapIndex(k), fn)
		}
	case reflect.String:
		if v.Type() == secretType {
			fn(Secret(v.String()))
		}
	default:
	}
}
