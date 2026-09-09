package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// inlineInventory is the inventory block of testdata/minimal.yaml, so a test
// can replace it with a file reference. minimalConfig checks it is there.
const inlineInventory = `inventory:
  hosts:
    - name: host-a
      endpoint: 10.0.1.10:9090
      arch: arm64
      vcpu: 62
      memory_mb: 253952
`

// minimalConfig is testdata/minimal.yaml as a string: the smallest valid
// configuration (CF-005). Tests append a section to it or replace one line of
// it rather than adding a testdata file per case.
func minimalConfig(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "minimal.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), inlineInventory) {
		t.Fatal("testdata/minimal.yaml no longer contains the inline inventory block the tests replace")
	}
	return string(data)
}

// writeFile writes content to path, failing the test on error.
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
