package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// base is the kind image's kind configuration with a registry mirror, as
// guest/rootfs-kind/mirror-kind.sh writes it.
const base = `kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
kubeadmConfigPatches:
  - |
    kind: ClusterConfiguration
containerdConfigPatches:
  - |-
    [plugins."io.containerd.grpc.v1.cri".registry]
      config_path = "/etc/containerd/certs.d"
nodes:
  - role: control-plane
    extraMounts:
      - hostPath: /etc/kind/certs.d
        containerPath: /etc/containerd/certs.d
        readOnly: true
      - hostPath: /var/run/docker.sock
        containerPath: /var/run/docker.sock
`

const nodeImage = "kindest/node:v1.37.0@sha256:a1ed56cfb0e7b93589bdf97c8cd566405a265939e3620fc4f5de89adff580ae5"

// files writes each file under root and returns root.
func files(t *testing.T, fs map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, content := range fs {
		p := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

type kindConfig struct {
	ContainerdConfigPatches []string `yaml:"containerdConfigPatches"`
	KubeadmConfigPatches    []string `yaml:"kubeadmConfigPatches"`
	Nodes                   []struct {
		Role        string `yaml:"role"`
		Image       string `yaml:"image"`
		Labels      map[string]string
		ExtraMounts []struct {
			HostPath      string `yaml:"hostPath"`
			ContainerPath string `yaml:"containerPath"`
		} `yaml:"extraMounts"`
	} `yaml:"nodes"`
}

func merge(t *testing.T, own string) (kindConfig, int) {
	t.Helper()
	var ownData []byte
	if own != "" {
		ownData = []byte(own)
	}
	out, n, err := mergeKindConfig([]byte(base), ownData, nodeImage)
	if err != nil {
		t.Fatal(err)
	}
	var c kindConfig
	if err := yaml.Unmarshal(out, &c); err != nil {
		t.Fatalf("%v in\n%s", err, out)
	}
	return c, n
}

func mounts(c kindConfig, i int) []string {
	var paths []string
	for _, m := range c.Nodes[i].ExtraMounts {
		paths = append(paths, m.ContainerPath)
	}
	return paths
}

//= docs/requirements/13-guest-image.md#guest-kind-spec-hook
//= type=test
//# When `flr-kind-cluster --spec` makes the cluster from a spec with a
//# `kindConfig`, `flr-kind-cluster` SHALL add to it the
//# `containerdConfigPatches` of `/etc/kind/kind-config.yaml`, and add to
//# every node each mount of the control-plane node of
//# `/etc/kind/kind-config.yaml` other than the Docker socket.
//
//= docs/requirements/13-guest-image.md#guest-kind-spec-hook
//= type=test
//# When `flr-kind-cluster --spec` makes the cluster from a spec with a
//# `kindConfig`, `flr-kind-cluster` SHALL mount the guest's Docker socket,
//# `/var/run/docker.sock`, at the same path in each control-plane node and
//# in no other node.
//
//= docs/requirements/13-guest-image.md#guest-kind-spec-hook
//= type=test
//# When `flr-kind-cluster --spec` makes the cluster, `flr-kind-cluster`
//# SHALL give each node that names no `image` the node image of
//# `KIND_NODE_IMAGE`.

func TestMergeKindConfig(t *testing.T) {
	own := `kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
containerdConfigPatches:
  - |-
    [plugins."io.containerd.grpc.v1.cri".containerd]
      snapshotter = "overlayfs"
nodes:
  - role: control-plane
    image: kindest/node:v1.36.0@sha256:0000000000000000000000000000000000000000000000000000000000000000
  - role: worker
    labels: {host: "true"}
    extraMounts:
      - hostPath: /tmp/own
        containerPath: /etc/containerd/certs.d
  - role: worker
`
	c, n := merge(t, own)
	if n != 3 || len(c.Nodes) != 3 {
		t.Fatalf("nodes: %d, %d; want 3", n, len(c.Nodes))
	}
	if len(c.ContainerdConfigPatches) != 2 || !strings.Contains(c.ContainerdConfigPatches[0], "snapshotter") ||
		!strings.Contains(c.ContainerdConfigPatches[1], `config_path = "/etc/containerd/certs.d"`) {
		t.Errorf("containerdConfigPatches: %q; want the spec's, then the mirror's", c.ContainerdConfigPatches)
	}
	if len(c.KubeadmConfigPatches) != 0 {
		t.Errorf("kubeadmConfigPatches: %q; want only the spec's, none", c.KubeadmConfigPatches)
	}
	if got := strings.Join(mounts(c, 0), " "); got != "/etc/containerd/certs.d /var/run/docker.sock" {
		t.Errorf("the control plane's mounts: %s; want the mirror's hosts directory and the Docker socket", got)
	}
	if got := strings.Join(mounts(c, 1), " "); got != "/etc/containerd/certs.d" || c.Nodes[1].ExtraMounts[0].HostPath != "/tmp/own" {
		t.Errorf("the first worker's mounts: %s from %s; want its own mount only", got, c.Nodes[1].ExtraMounts[0].HostPath)
	}
	if got := strings.Join(mounts(c, 2), " "); got != "/etc/containerd/certs.d" {
		t.Errorf("the second worker's mounts: %s; want the mirror's hosts directory and no Docker socket", got)
	}
	if c.Nodes[0].Image == nodeImage || c.Nodes[1].Image != nodeImage || c.Nodes[2].Image != nodeImage {
		t.Errorf("node images: %q, %q, %q; want the spec's, then KIND_NODE_IMAGE twice", c.Nodes[0].Image, c.Nodes[1].Image, c.Nodes[2].Image)
	}
	if c.Nodes[1].Labels["host"] != "true" {
		t.Errorf("the worker lost its labels: %v", c.Nodes[1].Labels)
	}

	// No nodes: kind's one control plane, with the additions.
	c, n = merge(t, "kind: Cluster\napiVersion: kind.x-k8s.io/v1alpha4\n")
	if n != 1 || c.Nodes[0].Role != "control-plane" || strings.Join(mounts(c, 0), " ") != "/etc/containerd/certs.d /var/run/docker.sock" {
		t.Errorf("a kindConfig with no nodes gave %d nodes: %+v", n, c.Nodes)
	}

	// No kindConfig: the kind image's own, with the node image.
	c, n = merge(t, "")
	if n != 1 || len(c.KubeadmConfigPatches) != 1 || len(c.ContainerdConfigPatches) != 1 || len(mounts(c, 0)) != 2 || c.Nodes[0].Image != nodeImage {
		t.Errorf("no kindConfig gave %d nodes: %+v", n, c)
	}
}

// bakedSpec is a spec as flr-bake writes it, at etc/flr/kind-cluster.yaml
// under the root, with each file under var/lib/flr.
const bakedSpec = `apiVersion: flintlock-runner.dev/v1alpha1
kind: KindCluster
name: my-e2e
volumes: disk
kindConfig: |
  kind: Cluster
  apiVersion: kind.x-k8s.io/v1alpha4
  nodes:
    - role: control-plane
    - role: worker
manifests:
  - path: ../../var/lib/flr/manifests/0.yaml
    wait:
      - deployment/cert-manager-webhook -n cert-manager
      - crd/certificates.cert-manager.io
  - helm:
      chart: ../../var/lib/flr/charts/1.tgz
      version: 1.4.2
      digest: sha256:1111111111111111111111111111111111111111111111111111111111111111
      release: thing
      namespace: thing
      values: [../../var/lib/flr/charts/1-values-0.yaml]
    wait:
      - deployment/thing -n thing
`

func TestPlan(t *testing.T) {
	root := files(t, map[string]string{
		"etc/flr/kind-cluster.yaml":          bakedSpec,
		"etc/kind/kind-config.yaml":          base,
		"var/lib/flr/manifests/0.yaml":       thing,
		"var/lib/flr/charts/1.tgz":           "a chart",
		"var/lib/flr/charts/1-values-0.yaml": "a: b\n",
	})
	out := filepath.Join(root, "run/kind-config.yaml")
	code, stdout, stderr := run("plan", "-root", root, "-offline", "-base", filepath.Join(root, "etc/kind/kind-config.yaml"),
		"-kind-config", out, "-node-image", nodeImage, filepath.Join(root, "etc/flr/kind-cluster.yaml"))
	if code != 0 {
		t.Fatalf("plan: %d, %s", code, stderr)
	}
	want := strings.Join([]string{
		"name\tmy-e2e",
		"volumes\tdisk",
		"nodes\t2",
		"apply\t" + root + "/var/lib/flr/manifests/0.yaml",
		"wait\tdeployment/cert-manager-webhook\tcert-manager",
		"wait\tcrd/certificates.cert-manager.io\t-",
		"helm\tthing\tthing\t" + root + "/var/lib/flr/charts/1.tgz\t-\t" + root + "/var/lib/flr/charts/1-values-0.yaml",
		"wait\tdeployment/thing\tthing",
	}, "\n") + "\n"
	if stdout != want {
		t.Errorf("plan printed\n%s\nwant\n%s", stdout, want)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "containerPath: /var/run/docker.sock") || !strings.Contains(string(data), "config_path") {
		t.Errorf("the kind configuration has no additions:\n%s", data)
	}
}

//= docs/requirements/13-guest-image.md#guest-kind-spec-hook
//= type=test
//# If `flr-kind-cluster --spec FILE` takes the spec's digest from a `.digest`
//# file and the spec has an entry under `manifests` with a `url`, or a
//# `helm` entry whose `chart` is not a file in the guest, then
//# `flr-kind-cluster` SHALL fail before it makes the cluster.

func TestPlanOffline(t *testing.T) {
	sum := strings.Repeat("a", 64)
	for name, entry := range map[string]string{
		"url":  "  - url: https://example.com/a.yaml\n    sha256: " + sum + "\n",
		"helm": "  - helm:\n      chart: oci://ghcr.io/example/thing\n      version: 1.0.0\n      digest: sha256:" + sum + "\n      release: thing\n",
	} {
		t.Run(name, func(t *testing.T) {
			spec := "apiVersion: flintlock-runner.dev/v1alpha1\nkind: KindCluster\nname: e2e\nmanifests:\n" + entry
			root := files(t, map[string]string{".flr/kind-cluster.yaml": spec, "base.yaml": base})
			args := []string{"-base", filepath.Join(root, "base.yaml"), "-kind-config", filepath.Join(root, "out.yaml"),
				filepath.Join(root, ".flr/kind-cluster.yaml")}
			if code, _, stderr := run(append([]string{"plan", "-offline"}, args...)...); code != 1 || !strings.Contains(stderr, "network") {
				t.Errorf("plan -offline: %d, %q; want 1 and a message about the network", code, stderr)
			}
			if _, err := os.Stat(filepath.Join(root, "out.yaml")); err == nil {
				t.Error("plan -offline wrote a kind configuration for a spec it refused")
			}
			code, stdout, stderr := run(append([]string{"plan"}, args...)...)
			if code != 0 || !strings.Contains(stdout, "\n"+name+"\t") {
				t.Errorf("plan without -offline: %d, %q, %q; want a %s line", code, stdout, stderr, name)
			}
		})
	}
}

func TestPlanUsage(t *testing.T) {
	if code, _, _ := run("plan", "spec.yaml"); code != 2 {
		t.Errorf("plan without -base and -kind-config: %d; want 2", code)
	}
}
