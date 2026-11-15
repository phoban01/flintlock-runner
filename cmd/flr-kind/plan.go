package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/phoban01/flintlock-runner/internal/kindspec"
)

// planUsage is the usage of `flr-kind plan`, which flr-kind-cluster --spec
// runs (docs/requirements/13-guest-image.md#guest-kind-spec-hook).
const planUsage = `usage:
  flr-kind plan [-root DIR] [-offline] -base FILE -kind-config OUT [-node-image REF] SPEC
`

// dockerSocket is the mount that the kind image's configuration gives the
// control-plane node for CAPD (GI-080). It goes on control-plane nodes only.
const dockerSocket = "/var/run/docker.sock"

// plan reads a spec, writes the kind configuration to make its cluster
// from, and prints the steps that flr-kind-cluster takes, one to a line,
// with the fields of a line separated by a tab. An empty field is "-".
//
//	name NAME
//	volumes VOLUMES
//	nodes N
//	apply FILE                                    a manifest by path
//	apply-k DIR                                   a kustomize directory
//	url URL SHA256                                a manifest to download
//	helm RELEASE NAMESPACE CHART VERSION [VALUES...]
//	wait RESOURCE NAMESPACE                       after the entry before it
//
// The apply, apply-k, url and helm lines are in the order of the spec's
// manifests, each followed by its wait lines. Paths are absolute. A chart
// that is a file in the root is a local archive, with VERSION "-"; any
// other chart is a reference for helm to fetch. With -offline, a url entry
// and a chart that is not a local archive are refused, because the create
// hook does not use the network.
func plan(args []string, stdout, stderr io.Writer) int {
	fl := flag.NewFlagSet("flr-kind plan", flag.ContinueOnError)
	fl.SetOutput(stderr)
	root := fl.String("root", "", "the repository root (default: the parent of the spec's directory)")
	offline := fl.Bool("offline", false, "refuse anything that needs the network")
	base := fl.String("base", "", "the kind image's kind configuration, /etc/kind/kind-config.yaml")
	out := fl.String("kind-config", "", "where to write the kind configuration of the cluster")
	nodeImage := fl.String("node-image", "", "the node image for a node that names none")
	if err := fl.Parse(args); err != nil {
		return 2
	}
	if fl.NArg() != 1 || *base == "" || *out == "" {
		fmt.Fprint(stderr, planUsage)
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
	steps, err := planSteps(spec, *offline)
	if err != nil {
		fmt.Fprintf(stderr, "flr-kind: %s: %v\n", fl.Arg(0), err)
		return 1
	}
	baseData, err := os.ReadFile(*base)
	if err != nil {
		fmt.Fprintln(stderr, "flr-kind: "+err.Error())
		return 1
	}
	own, err := spec.KindConfigData()
	if err != nil {
		fmt.Fprintf(stderr, "flr-kind: %s: kindConfig: %v\n", fl.Arg(0), err)
		return 1
	}
	config, nodes, err := mergeKindConfig(baseData, own, *nodeImage)
	if err != nil {
		fmt.Fprintf(stderr, "flr-kind: %s: %v\n", fl.Arg(0), err)
		return 1
	}
	if err := writeFile(*out, config); err != nil {
		fmt.Fprintln(stderr, "flr-kind: "+err.Error())
		return 1
	}
	lines := append([][]string{
		{"name", spec.Name},
		{"volumes", spec.Volumes},
		{"nodes", fmt.Sprint(nodes)},
	}, steps...)
	for _, l := range lines {
		fmt.Fprintln(stdout, strings.Join(l, "\t"))
	}
	return 0
}

//= docs/requirements/13-guest-image.md#guest-kind-spec-hook
//# If `flr-kind-cluster --spec FILE` takes the spec's digest from a `.digest`
//# file and the spec has an entry under `manifests` with a `url`, or a
//# `helm` entry whose `chart` is not a file in the guest, then
//# `flr-kind-cluster` SHALL fail before it makes the cluster.

// planSteps turns the spec's manifests into plan lines.
func planSteps(spec *kindspec.Spec, offline bool) ([][]string, error) {
	var lines [][]string
	for i := range spec.Manifests {
		m := &spec.Manifests[i]
		field := fmt.Sprintf("manifests[%d]", i)
		switch m.Source() {
		case "url":
			if offline {
				return nil, fmt.Errorf("%s: a url needs the network; a baked spec names the file by path", field)
			}
			lines = append(lines, []string{"url", m.URL, m.SHA256})
		case "path", "kustomize":
			p := m.Path
			kind := "apply"
			if m.Source() == "kustomize" {
				p, kind = m.Kustomize, "apply-k"
			}
			abs, err := spec.Abs(p)
			if err != nil {
				return nil, fmt.Errorf("%s.%s: %w", field, m.Source(), err)
			}
			lines = append(lines, []string{kind, abs})
		case "helm":
			l, err := helmLine(spec, field, m.Helm, offline)
			if err != nil {
				return nil, err
			}
			lines = append(lines, l)
		}
		for j, w := range m.Wait {
			pw, err := kindspec.ParseWait(w)
			if err != nil {
				return nil, fmt.Errorf("%s.wait[%d]: %w", field, j, err)
			}
			lines = append(lines, []string{"wait", pw.Resource, dash(pw.Namespace)})
		}
	}
	for _, l := range lines {
		for _, f := range l {
			if f == "" || strings.ContainsAny(f, "\t\n") {
				return nil, fmt.Errorf("%q is empty or has a tab or a line break", f)
			}
		}
	}
	return lines, nil
}

// helmLine is the plan line of a chart. A chart that names a regular file
// inside the root is a local archive, such as a baked one; anything else,
// such as oci://..., is a reference for helm to fetch.
func helmLine(spec *kindspec.Spec, field string, h *kindspec.Helm, offline bool) ([]string, error) {
	chart, version := h.Chart, h.Version
	if !strings.Contains(chart, "://") {
		if abs, err := spec.Abs(chart); err == nil {
			if fi, err := os.Stat(abs); err == nil && fi.Mode().IsRegular() {
				chart, version = abs, "-"
			}
		}
	}
	if offline && version != "-" {
		return nil, fmt.Errorf("%s.helm.chart: %q is not a chart archive in the guest, and fetching it needs the network", field, h.Chart)
	}
	l := []string{"helm", h.Release, dash(h.Namespace), chart, version}
	for j, v := range h.Values {
		abs, err := spec.Abs(v)
		if err != nil {
			return nil, fmt.Errorf("%s.helm.values[%d]: %w", field, j, err)
		}
		l = append(l, abs)
	}
	return l, nil
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

//= docs/requirements/13-guest-image.md#guest-kind-spec-hook
//# When `flr-kind-cluster --spec` makes the cluster from a spec with a
//# `kindConfig`, `flr-kind-cluster` SHALL add to it the
//# `containerdConfigPatches` of `/etc/kind/kind-config.yaml`, and add to
//# every node each mount of the control-plane node of
//# `/etc/kind/kind-config.yaml` other than the Docker socket.
//
//= docs/requirements/13-guest-image.md#guest-kind-spec-hook
//# When `flr-kind-cluster --spec` makes the cluster from a spec with a
//# `kindConfig`, `flr-kind-cluster` SHALL mount the guest's Docker socket,
//# `/var/run/docker.sock`, at the same path in each control-plane node and
//# in no other node.
//
//= docs/requirements/13-guest-image.md#guest-kind-spec-hook
//# When `flr-kind-cluster --spec` makes the cluster, `flr-kind-cluster`
//# SHALL give each node that names no `image` the node image of
//# `KIND_NODE_IMAGE`.

// mergeKindConfig returns the kind configuration to make a spec's cluster
// from, and its number of nodes. Without own, it is base, the kind image's
// configuration. With own, it is own with base's additions: base's
// containerdConfigPatches, which point the nodes' containerd at the
// Host's registry mirror, and the mounts of base's control-plane node.
// The Docker socket goes on each control-plane node, and every other
// mount, such as the mirror's hosts directory, on every node. A patch or
// a mount that own has already is not added twice. Each node that names
// no image gets nodeImage, when it is set, so that kind uses the node
// image that Docker Engine has.
func mergeKindConfig(base, own []byte, nodeImage string) ([]byte, int, error) {
	var b map[string]any
	if err := yaml.Unmarshal(base, &b); err != nil {
		return nil, 0, fmt.Errorf("the kind image's kind configuration: %w", err)
	}
	if b == nil {
		return nil, 0, errors.New("the kind image's kind configuration is empty")
	}
	var c map[string]any
	if own == nil {
		c = b
	} else if err := yaml.Unmarshal(own, &c); err != nil {
		return nil, 0, fmt.Errorf("kindConfig: %w", err)
	} else if c == nil {
		return nil, 0, errors.New("kindConfig is empty")
	}

	patches, err := list(c, "containerdConfigPatches")
	if err != nil {
		return nil, 0, err
	}
	basePatches, err := list(b, "containerdConfigPatches")
	if err != nil {
		return nil, 0, err
	}
	for _, p := range basePatches {
		if !containsValue(patches, p) {
			patches = append(patches, p)
		}
	}
	if len(patches) > 0 {
		c["containerdConfigPatches"] = patches
	}

	var baseMounts []any
	baseNodes, err := nodeList(b)
	if err != nil {
		return nil, 0, err
	}
	for _, n := range baseNodes {
		if role(n) == "control-plane" {
			if baseMounts, err = list(n, "extraMounts"); err != nil {
				return nil, 0, err
			}
			break
		}
	}

	nodes, err := nodeList(c)
	if err != nil {
		return nil, 0, err
	}
	if len(nodes) == 0 {
		nodes = []map[string]any{{"role": "control-plane"}}
	}
	out := make([]any, 0, len(nodes))
	for _, n := range nodes {
		mounts, err := list(n, "extraMounts")
		if err != nil {
			return nil, 0, err
		}
		for _, m := range baseMounts {
			path := containerPath(m)
			if path == dockerSocket && role(n) != "control-plane" {
				continue
			}
			if !hasMount(mounts, path) {
				mounts = append(mounts, m)
			}
		}
		if len(mounts) > 0 {
			n["extraMounts"] = mounts
		}
		if _, ok := n["image"]; !ok && nodeImage != "" {
			n["image"] = nodeImage
		}
		out = append(out, n)
	}
	c["nodes"] = out

	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(c); err != nil {
		return nil, 0, err
	}
	return buf.Bytes(), len(nodes), nil
}

// list returns m[key] as a list; a missing key is an empty list.
func list(m map[string]any, key string) ([]any, error) {
	v, ok := m[key]
	if !ok || v == nil {
		return nil, nil
	}
	l, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("%s is not a list", key)
	}
	return l, nil
}

// nodeList returns the nodes of a kind configuration.
func nodeList(c map[string]any) ([]map[string]any, error) {
	l, err := list(c, "nodes")
	if err != nil {
		return nil, err
	}
	nodes := make([]map[string]any, 0, len(l))
	for i, v := range l {
		n, ok := v.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("nodes[%d] is not a mapping", i)
		}
		nodes = append(nodes, n)
	}
	return nodes, nil
}

// role is a node's role; kind takes a node with none as a control plane.
func role(n map[string]any) string {
	if r, ok := n["role"].(string); ok && r != "" {
		return r
	}
	return "control-plane"
}

func containerPath(m any) string {
	if mm, ok := m.(map[string]any); ok {
		if p, ok := mm["containerPath"].(string); ok {
			return p
		}
	}
	return ""
}

func hasMount(mounts []any, path string) bool {
	for _, m := range mounts {
		if containerPath(m) == path {
			return true
		}
	}
	return false
}

func containsValue(l []any, v any) bool {
	s, ok := v.(string)
	if !ok {
		return false
	}
	for _, x := range l {
		if xs, ok := x.(string); ok && xs == s {
			return true
		}
	}
	return false
}

// writeFile writes data to path through a temporary file beside it.
func writeFile(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
