package harness

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	hostfake "github.com/phoban01/flintlock-runner/internal/flintlock/fake"
)

// LeaseHosts names the Host of every Lease held, one entry per Lease, in no
// particular order: on the battery stack from the fake Pool Manager's
// records, on the Kubernetes stack from the Virtual Nodes of the claimed
// pods. It is how a scenario finds the Host its Job runs on, to fault it.
func (s *Stack) LeaseHosts(ctx context.Context) ([]string, error) {
	if s.kube != nil {
		pods, err := s.kubeLeases(ctx)
		if err != nil {
			return nil, err
		}
		var out []string
		for _, p := range pods {
			for _, h := range s.kube.hosts {
				if h.VirtualNode == p.Spec.NodeName {
					out = append(out, h.Node)
				}
			}
		}
		return out, nil
	}
	if s.PoolManager == nil {
		return nil, fmt.Errorf("harness: no fake pool manager to ask for leases")
	}
	vms := map[string]string{}
	for _, vm := range s.PoolManager.VMs() {
		vms[vm.UID] = vm.Host
	}
	var out []string
	for _, l := range s.PoolManager.Leases() {
		out = append(out, vms[l.VMUID])
	}
	sort.Strings(out)
	return out, nil
}

// Host is the fake Host of that name, nil if there is none.
func (s *Stack) Host(name string) *hostfake.Host {
	for _, h := range s.Hosts {
		if h.Config().Name == name {
			return h
		}
	}
	return nil
}

// KubeHost is the Kubernetes stack's Host of that Node name, nil if there
// is none.
func (s *Stack) KubeHost(name string) *KubeHost {
	if s.kube == nil {
		return nil
	}
	for _, h := range s.kube.hosts {
		if h.Node == name {
			return h
		}
	}
	return nil
}

// Gate is a condition a Job's script can wait on and the test opens. The
// fake Hosts run a Job's commands on this machine, so a file under the
// Stack's root is visible to both.
type Gate struct {
	path string
}

// NewGate makes a closed gate.
func (s *Stack) NewGate(name string) *Gate {
	return &Gate{path: filepath.Join(s.Root, "gate-"+name)}
}

// Wait is a shell line that blocks until the gate is open, for up to two
// minutes, after which it fails the script.
func (g *Gate) Wait() string {
	return fmt.Sprintf(`for i in $(seq 1 1200); do [ -e %q ] && break; sleep 0.1; done; [ -e %q ]`, g.path, g.path)
}

// Open opens the gate.
func (g *Gate) Open() error {
	return os.WriteFile(g.path, nil, 0o600)
}

// Trace is what the fake GitLab has recorded of Job id's log so far.
func (s *Stack) Trace(id int64) string {
	if rec := s.GitLab.Record(id); rec != nil {
		return rec.Trace
	}
	return ""
}

// WaitTrace blocks until Job id's log contains want, and fails if the Job
// ends or the Runner exits first.
func (s *Stack) WaitTrace(ctx context.Context, id int64, want string) error {
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	exited := s.runnerExited()
	for {
		rec := s.GitLab.Record(id)
		if rec != nil && strings.Contains(rec.Trace, want) {
			return nil
		}
		if rec != nil && Final(rec.Status) {
			return fmt.Errorf("harness: job %d ended %s without logging %q:\n%s", id, rec.Status, want, rec.Trace)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("harness: waiting for job %d to log %q: %w; it logged so far:\n%s", id, want, context.Cause(ctx), s.Trace(id))
		case <-exited:
			return errors.Join(fmt.Errorf("harness: job %d did not log %q", id, want), s.runnerExitError())
		case <-ticker.C:
		}
	}
}
