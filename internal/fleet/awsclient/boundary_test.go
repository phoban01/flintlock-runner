package awsclient

import (
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/phoban01/flintlock-runner/internal/fleet"
	"github.com/phoban01/flintlock-runner/internal/fleet/awsfake"
)

// sdkServices are the SDK packages that talk to EC2 and Systems Manager.
var sdkServices = []string{
	"github.com/aws/aws-sdk-go-v2/service/ec2",
	"github.com/aws/aws-sdk-go-v2/service/ssm",
}

// repoRoot finds the directory holding go.mod above the package.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found")
		}
		dir = parent
	}
}

//= docs/requirements/10-test-doubles.md#fake-aws
//= type=test
//# The Fleet Controller SHALL access EC2 and Systems Manager
//# through narrow interfaces of its own so that fakes can stand in for the
//# AWS SDK.

// TestOnlyAWSClientTalksToTheSDK parses every Go file in the module and
// fails if any package but this one imports the EC2 or Systems Manager SDK,
// which would bypass the narrow interfaces the fakes implement. It also
// checks the real clients and the fakes satisfy the same interfaces.
func TestOnlyAWSClientTalksToTheSDK(t *testing.T) {
	t.Parallel()
	root := repoRoot(t)
	self := filepath.Join(root, "internal", "fleet", "awsclient")
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if path != root && (strings.HasPrefix(name, ".") || name == "vendor" || name == "testdata" || name == "bin") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || filepath.Dir(path) == self {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, imp := range f.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			for _, svc := range sdkServices {
				if p == svc || strings.HasPrefix(p, svc+"/") {
					rel, _ := filepath.Rel(root, path)
					t.Errorf("%s imports %s; go through fleet.EC2, fleet.SSM or fleet.Parameters instead", rel, p)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	pairs := []struct {
		real, fake any
		iface      string
	}{
		{(*EC2)(nil), awsfake.NewEC2(), "EC2"},
		{(*SSM)(nil), awsfake.NewSSM(), "SSM"},
		{(*Parameters)(nil), awsfake.NewParameters(nil), "Parameters"},
	}
	for _, p := range pairs {
		for _, v := range []any{p.real, p.fake} {
			var ok bool
			switch p.iface {
			case "EC2":
				_, ok = v.(fleet.EC2)
			case "SSM":
				_, ok = v.(fleet.SSM)
			case "Parameters":
				_, ok = v.(fleet.Parameters)
			}
			if !ok {
				t.Errorf("%T does not implement fleet.%s", v, p.iface)
			}
		}
	}
}
