package flintlock_test

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/phoban01/flintlock-runner/internal/flintlock"
)

//= docs/requirements/05-hosts.md#flintlock-client
//= type=test
//# The Runner SHALL NOT call `CreateMicroVM` or `DeleteMicroVM` on
//# any Host.

// TestRunnerSideCodeCannotCreateOrDeleteMicroVMs checks the requirement in
// the two ways it can fail. A dialled Host client must not carry the admin
// methods, so that no Runner-side value can be type-asserted into one; and
// no Runner-side package may contain a call to either RPC, which is what a
// helper reaching for the generated MicroVM client directly would look
// like. The Pool Manager side is deliberately not scanned: creating and
// deleting is its job.
func TestRunnerSideCodeCannotCreateOrDeleteMicroVMs(t *testing.T) {
	t.Parallel()

	t.Run("a dialled client has no admin methods", func(t *testing.T) {
		host := startHost(t, flintlock.FakeHostConfig{Name: "h1"})
		client, err := flintlock.NewDialer().Dial(context.Background(), flintlock.Endpoint{
			Name:    "h1",
			Address: host.Addr(),
			TLS:     flintlock.TLSOptions{Insecure: true},
		})
		if err != nil {
			t.Fatalf("Dial: %v", err)
		}
		t.Cleanup(func() { _ = client.Close() })
		if _, ok := client.(flintlock.HostAdminClient); ok {
			t.Error("the dialled host client satisfies HostAdminClient; HO-007 needs create and delete out of reach")
		}
		if _, ok := client.(flintlock.PoolHostClient); ok {
			t.Error("the dialled host client satisfies PoolHostClient; HO-007 needs create and delete out of reach")
		}
	})

	t.Run("no runner-side package calls them", func(t *testing.T) {
		root := repoRoot(t)
		// The Runner side: this package, the Inventory translation and the
		// Guest Transports. Not the fake Host, which serves both RPCs, and
		// not the fake Pool Manager, which calls them.
		dirs := []string{
			filepath.Join(root, "internal", "flintlock"),
			filepath.Join(root, "internal", "flintlock", "inventory"),
			filepath.Join(root, "internal", "transport"),
		}
		for _, dir := range dirs {
			for _, file := range goFiles(t, dir) {
				for _, call := range calledMethods(t, file) {
					if call == "CreateMicroVM" || call == "DeleteMicroVM" {
						t.Errorf("%s calls %s; the Runner never creates or deletes a MicroVM (HO-007)",
							mustRel(t, root, file), call)
					}
				}
			}
		}
	})
}

// repoRoot walks up from the test's directory to the module root.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above the test's working directory")
		}
		dir = parent
	}
}

// goFiles lists the Go source files of the package in dir. Test files are
// left out: a test that seeds a fake Host with a MicroVM is standing in for
// the Pool Manager, which is the component that may create and delete.
func goFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}
	var files []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		files = append(files, filepath.Join(dir, e.Name()))
	}
	return files
}

// calledMethods returns the method names called in a file. Parsing rather
// than searching the text is what keeps the comments that name the two RPCs,
// including the citations above, from being mistaken for calls.
func calledMethods(t *testing.T, file string) []string {
	t.Helper()
	fset := token.NewFileSet()
	parsed, err := parser.ParseFile(fset, file, nil, 0)
	if err != nil {
		t.Fatalf("parsing %s: %v", file, err)
	}
	var names []string
	ast.Inspect(parsed, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
			names = append(names, sel.Sel.Name)
		}
		return true
	})
	return names
}

// mustRel makes a path relative to the repository root for an error message.
func mustRel(t *testing.T, root, path string) string {
	t.Helper()
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return path
	}
	return rel
}
