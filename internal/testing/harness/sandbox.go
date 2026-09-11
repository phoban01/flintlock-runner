package harness

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/liquidmetal-dev/flintlock/api/types"

	"github.com/phoban01/flintlock-runner/internal/flintlock"
	hostfake "github.com/phoban01/flintlock-runner/internal/flintlock/fake"
)

// sandboxDialer wraps the fake Pool Manager's Host dialer so that every
// MicroVM a fake Host creates has the Profile's builds and cache
// directories inside its sandbox.
//
// This works around a split in the fake Host's view of the filesystem. It
// resolves an exec request's cwd inside the MicroVM's sandbox (TD-023), but
// runs the command as an unconfined local process, so every absolute path
// the command itself uses is a path on this machine (TD-021). A Stage is
// run with its cwd set to the builds directory (EX-025), and the Executor
// creates that directory with a command (EX-016), which lands on this
// machine's filesystem, not in the sandbox. Without the directory in the
// sandbox too, the fake Host would refuse the Stage's cwd. DeleteMicroVM
// removes the whole sandbox, so nothing here outlives the MicroVM.
type sandboxDialer struct {
	inner flintlock.AdminDialer
	hosts map[string]*hostfake.Host
	dirs  []string
}

// DialAdmin implements flintlock.AdminDialer.
func (d sandboxDialer) DialAdmin(ctx context.Context, ep flintlock.Endpoint) (flintlock.PoolHostClient, error) {
	c, err := d.inner.DialAdmin(ctx, ep)
	if err != nil {
		return nil, err
	}
	h, ok := d.hosts[ep.Name]
	if !ok {
		return c, nil
	}
	return &sandboxClient{PoolHostClient: c, host: h, dirs: d.dirs}, nil
}

// sandboxClient is a PoolHostClient whose CreateMicroVM prepares the new
// MicroVM's sandbox.
type sandboxClient struct {
	flintlock.PoolHostClient
	host *hostfake.Host
	dirs []string
}

// CreateMicroVM implements flintlock.HostAdminClient.
func (c *sandboxClient) CreateMicroVM(ctx context.Context, spec *types.MicroVMSpec) (*types.MicroVM, error) {
	vm, err := c.PoolHostClient.CreateMicroVM(ctx, spec)
	if err != nil {
		return vm, err
	}
	if err := prepareSandbox(c.host, vm.GetSpec().GetUid(), c.dirs); err != nil {
		return nil, err
	}
	return vm, nil
}

// prepareSandbox creates dirs, which are guest paths, inside the sandbox of
// MicroVM uid on h.
func prepareSandbox(h *hostfake.Host, uid string, dirs []string) error {
	sandbox, ok := h.SandboxPath(uid)
	if !ok {
		return fmt.Errorf("harness: host %s has no sandbox for microvm %s", h.Config().Name, uid)
	}
	for _, d := range dirs {
		if err := os.MkdirAll(filepath.Join(sandbox, d), 0o755); err != nil {
			return fmt.Errorf("harness: preparing sandbox of microvm %s: %w", uid, err)
		}
	}
	return nil
}
