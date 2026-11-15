// Command flr-kind checks a project's kind cluster spec and prints its
// digest (docs/requirements/13-guest-image.md#guest-kind-spec), so that a
// shell script can do both.
//
// Usage:
//
//	flr-kind check [-root DIR] SPEC
//	flr-kind digest [-root DIR] SPEC
//
// check prints nothing and exits 0 when the spec is good. digest prints the
// spec's digest, sha256:<64 hexadecimal digits>, on one line. Both exit 1
// with one line for each fault when the spec is bad, and 2 on a usage
// error.
//
// Paths in the spec are relative to the spec's directory and stay inside
// the repository root. The root is the parent of the spec's directory, the
// repository for `.flr/kind-cluster.yaml`, unless -root names another.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/phoban01/flintlock-runner/internal/kindspec"
)

const usage = `usage:
  flr-kind check [-root DIR] SPEC
  flr-kind digest [-root DIR] SPEC
`

func main() { os.Exit(command(os.Args[1:], os.Stdout, os.Stderr)) }

// command runs one subcommand and returns the exit status.
func command(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	sub := args[0]
	if sub != "check" && sub != "digest" {
		fmt.Fprintf(stderr, "flr-kind: unknown command %q\n%s", sub, usage)
		return 2
	}
	fl := flag.NewFlagSet("flr-kind "+sub, flag.ContinueOnError)
	fl.SetOutput(stderr)
	root := fl.String("root", "", "the repository root (default: the parent of the spec's directory)")
	if err := fl.Parse(args[1:]); err != nil {
		return 2
	}
	if fl.NArg() != 1 {
		fmt.Fprint(stderr, usage)
		return 2
	}

	var spec *kindspec.Spec
	var err error
	if *root == "" {
		spec, err = kindspec.Load(fl.Arg(0))
	} else {
		spec, err = kindspec.LoadFromRoot(fl.Arg(0), *root)
	}
	if err != nil {
		printError(stderr, fl.Arg(0), err)
		return 1
	}
	if sub == "check" {
		return 0
	}
	digest, err := spec.Digest()
	if err != nil {
		printError(stderr, fl.Arg(0), err)
		return 1
	}
	fmt.Fprintln(stdout, digest)
	return 0
}

// printError prints each fault of a refused spec on a line of its own.
func printError(w io.Writer, path string, err error) {
	var joined interface{ Unwrap() []error }
	if errors.As(err, &joined) {
		fmt.Fprintf(w, "flr-kind: %s is not a valid kind cluster spec:\n", path)
		for _, e := range joined.Unwrap() {
			fmt.Fprintln(w, "  "+e.Error())
		}
		return
	}
	fmt.Fprintln(w, "flr-kind: "+err.Error())
}
