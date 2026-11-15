// Command flr-kind checks a project's kind cluster spec, prints its digest
// (docs/requirements/13-guest-image.md#guest-kind-spec) and bakes what a
// guest image needs for it
// (docs/requirements/13-guest-image.md#guest-kind-bake).
//
// Usage:
//
//	flr-kind check [-root DIR] SPEC
//	flr-kind digest [-root DIR] SPEC
//	flr-kind bake [-root DIR] [-base DIR] [-arch ARCH] [-docker-store FILE] SPEC OUT
//
// check prints nothing and exits 0 when the spec is good. digest prints the
// spec's digest, sha256:<64 hexadecimal digits>, on one line. Both exit 1
// with one line for each fault when the spec is bad, and 2 on a usage
// error.
//
// bake writes under OUT, an empty directory, the files that a project's
// guest image adds to the kind image, at their paths in the guest
// (internal/kindbake lists them), and prints their total size. -base is
// the kind image's root filesystem, for its node image and Docker Engine's
// repositories.json; it is needed only when the kind configuration names
// a node image. -arch is the target architecture, by default flr-kind's
// own. The guest-baker image runs it as flr-bake.
//
// Paths in the spec are relative to the spec's directory and stay inside
// the repository root. The root is the parent of the spec's directory, the
// repository for `.flr/kind-cluster.yaml`, unless -root names another.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/phoban01/flintlock-runner/internal/kindbake"
	"github.com/phoban01/flintlock-runner/internal/kindspec"
)

const usage = `usage:
  flr-kind check [-root DIR] SPEC
  flr-kind digest [-root DIR] SPEC
  flr-kind bake [-root DIR] [-base DIR] [-arch ARCH] [-docker-store FILE] SPEC OUT
`

// newTools gives bake its programs; tests replace it with fakes.
var newTools = kindbake.ExecTools

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := command(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

// command runs one subcommand and returns the exit status.
func command(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	sub := args[0]
	if sub != "check" && sub != "digest" && sub != "bake" {
		fmt.Fprintf(stderr, "flr-kind: unknown command %q\n%s", sub, usage)
		return 2
	}
	fl := flag.NewFlagSet("flr-kind "+sub, flag.ContinueOnError)
	fl.SetOutput(stderr)
	root := fl.String("root", "", "the repository root (default: the parent of the spec's directory)")
	var opts kindbake.Options
	dockerStore := kindbake.DefaultDockerStore
	nargs := 1
	if sub == "bake" {
		fl.StringVar(&opts.Base, "base", "", "the kind image's root filesystem, needed when the kind configuration names a node image")
		fl.StringVar(&opts.Arch, "arch", "", "the target architecture (default: flr-kind's own)")
		fl.StringVar(&dockerStore, "docker-store", dockerStore, "the path of docker-store.sh")
		nargs = 2
	}
	if err := fl.Parse(args[1:]); err != nil {
		return 2
	}
	if fl.NArg() != nargs {
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
	switch sub {
	case "check":
		return 0
	case "bake":
		return bake(ctx, spec, fl.Arg(1), newTools(dockerStore), opts, stdout, stderr)
	}
	digest, err := spec.Digest()
	if err != nil {
		printError(stderr, fl.Arg(0), err)
		return 1
	}
	fmt.Fprintln(stdout, digest)
	return 0
}

//= docs/requirements/13-guest-image.md#guest-kind-bake
//# `flr-kind bake` SHALL print the total size of the files that it wrote
//# under OUT.

// bake runs the bake, logging its steps to stderr, and prints what it
// wrote to stdout.
func bake(ctx context.Context, spec *kindspec.Spec, out string, tools kindbake.Tools, opts kindbake.Options, stdout, stderr io.Writer) int {
	opts.Log = stderr
	res, err := kindbake.Bake(ctx, spec, out, tools, opts)
	if err != nil {
		fmt.Fprintln(stderr, "flr-kind bake: "+err.Error())
		return 1
	}
	fmt.Fprintf(stdout, "flr-kind bake: wrote %d bytes (%.2f GiB) in %d files under %s: %d node images, %d image archives, %d images found in the manifests\n",
		res.Bytes, float64(res.Bytes)/(1<<30), res.Files, out, len(res.NodeImages), len(res.Images), len(res.Lock))
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
