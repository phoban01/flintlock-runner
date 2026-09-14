package inventory

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"

	"gopkg.in/yaml.v3"

	"github.com/phoban01/flintlock-runner/internal/config"
	"github.com/phoban01/flintlock-runner/internal/fleet"
)

// fileMode is the mode of every file the Store writes. The Inventory and the
// Runner configuration can carry Host tokens and the runner token (SE-010),
// so they are readable by the owner only.
const fileMode fs.FileMode = 0o600

// Store is the file-backed fleet.InventoryStore.
type Store struct{}

var _ fleet.InventoryStore = Store{}

// Load reads the Inventory file at path. A missing file is an empty
// Inventory, because the first `fleet provision` starts from nothing; any
// other read or parse failure is returned.
func (Store) Load(_ context.Context, path string) (*fleet.Inventory, error) {
	file, err := config.LoadInventoryFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return &fleet.Inventory{}, nil
	}
	if err != nil {
		return nil, err
	}
	return &fleet.Inventory{Hosts: file.Hosts, GeneratedAt: file.GeneratedAt}, nil
}

//= docs/requirements/06-fleet.md#inventory-and-runner-configuration
//# When provisioning completes, the Fleet Controller SHALL write an
//# Inventory file listing every Host with its name, `flintlockd` endpoint,
//# architecture, vCPU and memory capacity, labels, Host Service addresses and
//# installed versions.

// Save writes inv to path in the schema the Runner reads (config.InventoryFile,
// CF-003): every Host entry whole, so the name, endpoint, architecture,
// capacity, labels, Host Service addresses and installed versions all reach
// the file. The write is atomic: a Runner reloading the file never reads half
// of it.
func (Store) Save(_ context.Context, path string, inv *fleet.Inventory) error {
	if inv == nil {
		return errors.New("inventory: nil Inventory")
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(config.InventoryFile{Hosts: inv.Hosts, GeneratedAt: inv.GeneratedAt}); err != nil {
		return fmt.Errorf("inventory: encode: %w", err)
	}
	if err := enc.Close(); err != nil {
		return fmt.Errorf("inventory: encode: %w", err)
	}
	return writeAtomic(path, buf.Bytes())
}

//= docs/requirements/06-fleet.md#inventory-and-runner-configuration
//# The Fleet Controller SHALL generate a Runner configuration file
//# that references the Inventory, names the Pool Manager endpoint and carries
//# the Profiles from its input.

// SaveRunnerConfig writes cfg to path after proving that the Runner will
// accept it: the encoded bytes are parsed back with config.Parse, which
// resolves the Inventory file cfg references (relative to path's directory)
// and runs config.Validate. A configuration that would not load is never
// written. cfg is usually built by RunnerConfig.
func (Store) SaveRunnerConfig(_ context.Context, path string, cfg *config.Config) error {
	if cfg == nil {
		return errors.New("inventory: nil Runner configuration")
	}
	data, err := encodeConfig(cfg)
	if err != nil {
		return err
	}
	// No environment: what is checked is the file as written, not the file
	// plus whatever secrets the Fleet Controller's own environment holds.
	noEnv := config.WithEnv(func(string) (string, bool) { return "", false })
	if _, err := config.Parse(data, filepath.Dir(path), noEnv); err != nil {
		return fmt.Errorf("inventory: generated Runner configuration is invalid: %w", err)
	}
	return writeAtomic(path, data)
}

// encodeConfig renders cfg as YAML with two-space indentation.
func encodeConfig(cfg *config.Config) ([]byte, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(cfg); err != nil {
		return nil, fmt.Errorf("inventory: encode Runner configuration: %w", err)
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("inventory: encode Runner configuration: %w", err)
	}
	return buf.Bytes(), nil
}

// writeAtomic writes data to a temporary file beside path with owner-only
// permissions and renames it over path. The directory is created owner-only
// when missing. A file it replaces that its group may read keeps that
// group and that read permission, and nothing wider: that is how the
// Runner's service user, which `fleet up --install-runner` gives read
// access to these files, keeps it when fleet provision rewrites them.
func writeAtomic(path string, data []byte) (err error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("inventory: create %s: %w", dir, err)
	}
	mode, gid := fileMode, -1
	if fi, err := os.Stat(path); err == nil && fi.Mode().Perm()&0o040 != 0 {
		if st, ok := fi.Sys().(*syscall.Stat_t); ok {
			mode, gid = fileMode|0o040, int(st.Gid)
		}
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*")
	if err != nil {
		return fmt.Errorf("inventory: write %s: %w", path, err)
	}
	defer func() {
		if err != nil {
			_ = os.Remove(tmp.Name())
		}
	}()
	if gid >= 0 {
		if err := tmp.Chown(-1, gid); err != nil {
			_ = tmp.Close()
			return fmt.Errorf("inventory: write %s: keep its group: %w", path, err)
		}
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("inventory: write %s: %w", path, err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("inventory: write %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("inventory: write %s: %w", path, err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("inventory: write %s: %w", path, err)
	}
	return nil
}

// Remove deletes the Inventory file at path; a missing file is not an error
// (FL-081).
func Remove(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("inventory: remove %s: %w", path, err)
	}
	return nil
}
