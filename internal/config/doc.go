// Package config defines the configuration surface of flintlock-runner as
// specified in docs/requirements/07-configuration.md: the YAML schema, its
// validation, defaulting and the translation into the gitlab-runner
// RunnerConfig.
//
// types.go holds only the schema as Go structs. Loading (CF-001),
// environment overrides (CF-002), inventory file references (CF-003),
// validation (CF-004), defaults (CF-005), redaction for `config show`
// (CF-006) and reload (CF-007, CF-008) are the `config` work package of
// docs/PLAN.md and land in this package next to it. The structs carry no
// behaviour and import nothing but the standard library so that every other
// package can depend on them without pulling in YAML or gitlab-runner, and
// so that tests can build a Config literal directly.
//
// Every time.Duration field is written in YAML as a Go duration string
// ("30s", "5m"), which gopkg.in/yaml.v3 decodes natively. Every size field is
// a ByteSize, written as a human-readable quantity ("20GiB", "512MB").
//
// Secrets (CF-002, CF-006, SE-010) are fields of type Secret. Each one
// documents the environment variable that overrides it. Per-Host tokens use
// the convention FLINTLOCK_RUNNER_HOST_TOKEN_<NAME>, with the Host name
// upper-cased and every non-alphanumeric character replaced by an
// underscore, and fall back to the fleet-wide FLINTLOCK_RUNNER_HOST_TOKEN
// when the per-Host variable is unset.
package config
