package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Environment variables that override secrets (CF-002). The per-Host
// variable is built by HostTokenEnv.
const (
	// EnvGitLabToken overrides gitlab.token.
	EnvGitLabToken = "FLINTLOCK_RUNNER_GITLAB_TOKEN"
	// EnvHostToken overrides every inventory.hosts[].token that has no
	// per-Host variable set.
	EnvHostToken = "FLINTLOCK_RUNNER_HOST_TOKEN"
	// EnvHostTokenPrefix + the normalised Host name overrides one Host's
	// token.
	EnvHostTokenPrefix = "FLINTLOCK_RUNNER_HOST_TOKEN_"
	// EnvFleetHostToken overrides fleet.flintlockd.token.
	EnvFleetHostToken = "FLINTLOCK_RUNNER_FLEET_HOST_TOKEN"
)

// LookupEnv is the shape of os.LookupEnv, injected so that tests run in
// parallel without touching the process environment.
type LookupEnv func(key string) (string, bool)

// Option configures Load.
type Option func(*loader)

// WithEnv sets the environment lookup used for secret overrides. The
// default is os.LookupEnv.
func WithEnv(lookup LookupEnv) Option {
	return func(l *loader) { l.env = lookup }
}

// WithLogger sets the logger for startup warnings (CF-027, CF-083). The
// default is slog.Default().
func WithLogger(logger *slog.Logger) Option {
	return func(l *loader) { l.logger = logger }
}

// WithReadFile replaces os.ReadFile, for tests that supply files in memory.
func WithReadFile(read func(string) ([]byte, error)) Option {
	return func(l *loader) { l.readFile = read }
}

type loader struct {
	env      LookupEnv
	logger   *slog.Logger
	readFile func(string) ([]byte, error)
}

func newLoader(opts []Option) *loader {
	l := &loader{env: os.LookupEnv, logger: slog.Default(), readFile: os.ReadFile}
	for _, o := range opts {
		o(l)
	}
	return l
}

//= docs/requirements/07-configuration.md#file-and-precedence
//# The Runner SHALL read its configuration from a single YAML file
//# whose path is given by the `--config` flag or the `FLINTLOCK_RUNNER_CONFIG`
//# environment variable.

// Load reads the YAML file at path (the value of --config or
// FLINTLOCK_RUNNER_CONFIG, resolved by the command line), applies the
// environment overrides for secrets, resolves an Inventory file reference,
// applies the documented defaults and validates the result. Unknown keys are
// an error. A validation failure is a *ValidationError listing every invalid
// field; every other failure wraps the underlying error with the path.
//
// Load logs one warning per Profile image pinned by tag (CF-027) and an
// info line when the Distributed cache section is absent (CF-083).
func Load(path string, opts ...Option) (*Config, error) {
	l := newLoader(opts)
	data, err := l.readFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: read %s: %w", path, err)
	}
	cfg, err := l.parse(path, filepath.Dir(path), data)
	if err != nil {
		return nil, err
	}
	l.logStartup(cfg)
	return cfg, nil
}

// Parse is Load for configuration held in memory: the same decoding,
// overrides, defaults and validation, without reading the main file. An
// Inventory file reference is resolved relative to dir. Nothing is logged;
// use Warnings for the tag warnings.
func Parse(data []byte, dir string, opts ...Option) (*Config, error) {
	l := newLoader(opts)
	return l.parse("<memory>", dir, data)
}

// parse runs the pipeline on data; name labels errors and dir anchors a
// relative Inventory file reference.
func (l *loader) parse(name, dir string, data []byte) (*Config, error) {
	var cfg Config
	if err := decodeStrict(data, &cfg); err != nil {
		return nil, fmt.Errorf("config: parse %s: %w", name, err)
	}
	if err := l.resolveInventory(&cfg, dir); err != nil {
		return nil, err
	}
	l.applyEnv(&cfg)
	ApplyDefaults(&cfg)
	if err := Validate(&cfg); err != nil {
		return nil, fmt.Errorf("config: %s: %w", name, err)
	}
	return &cfg, nil
}

// decodeStrict decodes YAML into out, rejecting unknown keys and duplicate
// documents. An empty file decodes to the zero value.
func decodeStrict(data []byte, out any) error {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(out); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	var extra any
	if err := dec.Decode(&extra); err == nil {
		return errors.New("more than one YAML document")
	} else if !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

// InventoryFile is the schema of a separate Inventory file (CF-003), as the
// Fleet Controller writes it (FL-060): the Host list plus when it was
// generated.
type InventoryFile struct {
	Hosts       []HostEntry `yaml:"hosts"`
	GeneratedAt time.Time   `yaml:"generated_at,omitempty"`
}

//= docs/requirements/07-configuration.md#file-and-precedence
//# The Runner SHALL allow an Inventory to be supplied inline in the
//# configuration file or by reference to a separate Inventory file.

// resolveInventory reads inventory.file, when set, into inventory.hosts. A
// relative path is resolved against the configuration file's directory.
// Setting both is an error because it is ambiguous which one is meant.
func (l *loader) resolveInventory(cfg *Config, dir string) error {
	inv := &cfg.Inventory
	if inv.File == "" {
		return nil
	}
	if len(inv.Hosts) > 0 {
		return fmt.Errorf("config: %w", &ValidationError{Errors: []FieldError{{
			Field:  "inventory",
			Reason: "file and hosts are mutually exclusive; supply the Inventory inline or by reference, not both",
		}}})
	}
	path := inv.File
	if !filepath.IsAbs(path) {
		path = filepath.Join(dir, path)
	}
	data, err := l.readFile(path)
	if err != nil {
		return fmt.Errorf("config: read inventory file %s: %w", path, err)
	}
	var file InventoryFile
	if err := decodeStrict(data, &file); err != nil {
		return fmt.Errorf("config: parse inventory file %s: %w", path, err)
	}
	inv.Hosts = file.Hosts
	return nil
}

// LoadInventoryFile reads a standalone Inventory file. It does not validate
// the entries; Validate on the whole Config does.
func LoadInventoryFile(path string) (*InventoryFile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: read inventory file %s: %w", path, err)
	}
	var file InventoryFile
	if err := decodeStrict(data, &file); err != nil {
		return nil, fmt.Errorf("config: parse inventory file %s: %w", path, err)
	}
	return &file, nil
}

//= docs/requirements/07-configuration.md#file-and-precedence
//# The Runner SHALL allow every secret value in the configuration
//# to be supplied by an environment variable named in the field's
//# documentation, with the environment variable taking precedence over the
//# file.

// applyEnv overrides every Secret field from its documented environment
// variable. A variable that is set, even to the empty string, wins over the
// file so that an operator can blank a token from the environment.
func (l *loader) applyEnv(cfg *Config) {
	if v, ok := l.env(EnvGitLabToken); ok {
		cfg.GitLab.Token = Secret(v)
	}
	fleetWide, hasFleetWide := l.env(EnvHostToken)
	for i := range cfg.Inventory.Hosts {
		h := &cfg.Inventory.Hosts[i]
		if v, ok := l.env(HostTokenEnv(h.Name)); ok {
			h.Token = Secret(v)
		} else if hasFleetWide {
			h.Token = Secret(fleetWide)
		}
	}
	if cfg.Fleet != nil {
		if v, ok := l.env(EnvFleetHostToken); ok {
			cfg.Fleet.Flintlockd.Token = Secret(v)
		}
	}
}

// HostTokenEnv is the per-Host token variable: EnvHostTokenPrefix followed
// by the Host name upper-cased with every non-alphanumeric character
// replaced by an underscore ("host-1" becomes
// FLINTLOCK_RUNNER_HOST_TOKEN_HOST_1).
func HostTokenEnv(hostName string) string {
	var b strings.Builder
	b.WriteString(EnvHostTokenPrefix)
	for _, r := range strings.ToUpper(hostName) {
		if r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	return b.String()
}

// logStartup writes the startup lines Load owes: tag warnings (CF-027) and
// the Distributed cache notice (CF-083).
func (l *loader) logStartup(cfg *Config) {
	for _, w := range Warnings(cfg) {
		l.logger.Warn("profile image is specified by tag rather than digest; pin a digest for reproducible MicroVMs",
			"profile", w.Profile, "field", w.Field, "image", w.Image)
	}
	//= docs/requirements/07-configuration.md#distributed-cache-section
	//# If the Distributed cache section is absent, then the Runner
	//# SHALL log at startup that the `cache:` keyword is unavailable and SHALL
	//# fail Jobs that use it with the failure reason `runner_unsupported`.
	if !cfg.CacheConfigured() {
		l.logger.Info("no distributed_cache section: the cache: keyword is unavailable and jobs that use it fail with runner_unsupported")
	}
}

// CacheConfigured reports whether the Distributed cache section is present.
// The Executor takes it as Deps.CacheConfigured and fails Jobs that use
// `cache:` with runner_unsupported when it is false (CF-083).
func (c *Config) CacheConfigured() bool { return c.DistributedCache != nil }

// IsNotExist reports whether err is a Load failure caused by a missing
// configuration or Inventory file.
func IsNotExist(err error) bool { return errors.Is(err, fs.ErrNotExist) }
