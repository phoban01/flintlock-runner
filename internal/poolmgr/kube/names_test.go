package kube

import (
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/phoban01/flintlock-runner/internal/poolmgr"
)

// TestNamesAreValidAndDistinct checks that whatever a Runner, a Profile or a
// Pool is called, the label value and the ReplicaSet name made from it are
// ones Kubernetes accepts, that a name that is valid already is kept, and
// that two names never collapse into one.
func TestNamesAreValidAndDistinct(t *testing.T) {
	t.Parallel()
	inputs := []string{
		"default", "runner-a", "Runner A", "runner_a", "runner a", "gpu/large", "-edge-",
		"ünïcode", strings.Repeat("x", 80), strings.Repeat("x", 79) + "y", "",
	}
	seenLabels, seenNames := map[string]string{}, map[string]string{}
	for _, in := range inputs {
		label := labelValue(in)
		if errs := validation.IsValidLabelValue(label); len(errs) > 0 || label == "" {
			t.Errorf("labelValue(%q) = %q: %v", in, label, errs)
		}
		if prev, dup := seenLabels[label]; dup {
			t.Errorf("labelValue(%q) and labelValue(%q) are both %q", prev, in, label)
		}
		seenLabels[label] = in

		name := replicaSetName(poolmgr.PoolRef{Namespace: "ci", Name: in})
		if errs := validation.IsDNS1123Label(name); len(errs) > 0 || len(name) > maxReplicaSetName {
			t.Errorf("replicaSetName(%q) = %q: %v", in, name, errs)
		}
		if prev, dup := seenNames[name]; dup {
			t.Errorf("replicaSetName(%q) and replicaSetName(%q) are both %q", prev, in, name)
		}
		seenNames[name] = in
	}
	if got := labelValue("runner_a.1"); got != "runner_a.1" {
		t.Errorf("labelValue of a valid value = %q, want it unchanged", got)
	}
	if got := replicaSetName(poolmgr.PoolRef{Namespace: "ci", Name: "default"}); got != "ci-default" {
		t.Errorf("replicaSetName = %q, want ci-default", got)
	}
}

func TestCmdlineAndDeadline(t *testing.T) {
	t.Parallel()
	if got := cmdline(map[string]string{"quiet": "", "console": "ttyS0", "a": "1"}); got != "a=1 console=ttyS0 quiet" {
		t.Errorf("cmdline = %q", got)
	}
	for d, want := range map[time.Duration]int64{
		0: 1, time.Millisecond: 1, time.Second: 1, 1500 * time.Millisecond: 2, time.Hour: 3600,
	} {
		if got := deadlineSeconds(d); got != want {
			t.Errorf("deadlineSeconds(%s) = %d, want %d", d, got, want)
		}
	}
}
