// Package opstest stands up the fakes the fleet operations are tested
// against: fake flintlock Hosts and the fake Pool Manager, each serving real
// gRPC on loopback, with Pools declared the way the Runner declares them.
// It is test support for internal/fleet/verify, internal/fleet/drain and the
// fleet subcommands; nothing in production imports it.
package opstest

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/phoban01/flintlock-runner/internal/config"
	"github.com/phoban01/flintlock-runner/internal/flintlock"
	hostfake "github.com/phoban01/flintlock-runner/internal/flintlock/fake"
	"github.com/phoban01/flintlock-runner/internal/poolmgr"
	pmfake "github.com/phoban01/flintlock-runner/internal/poolmgr/fake"
)

// Namespace and RunnerName are the Runner namespace and name the Pools are
// declared under.
const (
	Namespace  = "runner-ns"
	RunnerName = "fleet-test"
	// Token is the basic auth token every fake Host requires.
	Token = "fleet-test-token"
)

// Options shape a Stack.
type Options struct {
	// Hosts is the number of fake Hosts, named host-1 to host-N.
	Hosts int
	// ExecDisabled names Hosts whose ServerInfo reports the exec service
	// disabled.
	ExecDisabled map[string]bool
	// ServerInfoUnimplemented names Hosts whose ServerInfo answers
	// UNIMPLEMENTED, as a Host that predates the RPC would (HO-013).
	ServerInfoUnimplemented map[string]bool
	// PoolSize is the size of the one Profile's Pool; zero means one per
	// Host.
	PoolSize int
	// Placement is the fake Pool Manager's placement strategy.
	Placement poolmgr.PlacementStrategy
	// OmitHostOnClaim leaves the host field of claims unset (TD-008).
	OmitHostOnClaim bool
}

// Stack is a running set of fakes.
type Stack struct {
	Hosts       []*hostfake.Host
	PoolManager *pmfake.PoolManager
	// Client is a Pool Manager client over gRPC.
	Client poolmgr.Client
	// Inventory lists every Host as the Fleet Controller would have written
	// it.
	Inventory []config.HostEntry
	// Profiles holds one Profile, with defaults applied, whose Pool may
	// place on every Host.
	Profiles []config.Profile
}

// Start serves the fakes until the test ends.
func Start(t testing.TB, opts Options) *Stack {
	t.Helper()
	if opts.Hosts == 0 {
		opts.Hosts = 1
	}
	if opts.PoolSize == 0 {
		opts.PoolSize = opts.Hosts
	}
	// The Pool Manager stops first, deleting its MicroVMs while it can still
	// reach the Hosts; then its Host connections close; then the Hosts stop.
	hostCtx, hostCancel := context.WithCancel(context.Background())
	ctx, cancel := context.WithCancel(context.Background())
	s := &Stack{}
	var hostDone []chan error
	var pmDone chan error
	var pmHosts *pmfake.Hosts
	t.Cleanup(func() {
		if s.Client != nil {
			_ = s.Client.Close()
		}
		cancel()
		if pmDone != nil {
			<-pmDone
		}
		if pmHosts != nil {
			_ = pmHosts.Close()
		}
		hostCancel()
		for _, d := range hostDone {
			<-d
		}
	})

	root := t.TempDir()
	for i := 1; i <= opts.Hosts; i++ {
		name := fmt.Sprintf("host-%d", i)
		h := hostfake.New(flintlock.FakeHostConfig{
			Name:                    name,
			Listen:                  "127.0.0.1:0",
			SandboxRoot:             filepath.Join(root, name),
			Version:                 "v0.0.0-fleet-test",
			ExecEnabled:             !opts.ExecDisabled[name],
			ServerInfoUnimplemented: opts.ServerInfoUnimplemented[name],
			Token:                   Token,
		})
		d := make(chan error, 1)
		hostDone = append(hostDone, d)
		go func() { d <- h.Serve(hostCtx) }()
		waitReady(t, name, h.Ready(), d)
		s.Hosts = append(s.Hosts, h)
		s.Inventory = append(s.Inventory, config.HostEntry{
			Name:     name,
			Endpoint: h.Addr(),
			Arch:     config.Architecture(runtime.GOARCH),
			VCPU:     8,
			MemoryMB: 16384,
			Token:    Token,
			TLS:      config.ClientTLS{Insecure: true},
		})
	}

	var eps []flintlock.Endpoint
	for i := range s.Inventory {
		h := &s.Inventory[i]
		eps = append(eps, flintlock.Endpoint{Name: h.Name, Address: h.Endpoint, Token: string(h.Token), TLS: flintlock.TLSOptions{Insecure: true}})
	}
	hosts, err := pmfake.HostsFromDialer(ctx, pmfake.AdminDialer{}, eps)
	if err != nil {
		t.Fatalf("opstest: dialing hosts: %v", err)
	}
	pmHosts = hosts
	pm := pmfake.New(poolmgr.FakeConfig{
		Listen:            "127.0.0.1:0",
		Hosts:             hosts,
		Placement:         opts.Placement,
		OmitHostOnClaim:   opts.OmitHostOnClaim,
		ReconcileInterval: 100 * time.Millisecond,
		ReadyTimeout:      10 * time.Second,
	})
	pmDone = make(chan error, 1)
	go func() { pmDone <- pm.Serve(ctx) }()
	waitReady(t, "pool manager", pm.Ready(), pmDone)
	s.PoolManager = pm

	client, err := poolmgr.NewClient(poolmgr.ClientConfig{Endpoint: pm.Addr(), TLS: config.ClientTLS{Insecure: true}})
	if err != nil {
		t.Fatalf("opstest: pool manager client: %v", err)
	}
	s.Client = client

	shell, err := exec.LookPath("sh")
	if err != nil {
		t.Fatalf("opstest: no sh on PATH: %v", err)
	}
	cfg := config.Config{
		Scheduler: config.Scheduler{Namespace: Namespace},
		Inventory: config.Inventory{Hosts: s.Inventory},
		Profiles: []config.Profile{{
			Name:   "small",
			Arch:   config.Architecture(runtime.GOARCH),
			Kernel: config.Kernel{Image: "ghcr.io/example/kernel:test"},
			RootFS: "ghcr.io/example/rootfs:test",
			Shell:  shell,
			Pool:   config.PoolSettings{Size: opts.PoolSize},
		}},
	}
	config.ApplyDefaults(&cfg)
	s.Profiles = cfg.Profiles
	return s
}

func waitReady(t testing.TB, what string, ready <-chan struct{}, done chan error) {
	t.Helper()
	select {
	case <-ready:
	case err := <-done:
		done <- err
		if err == nil {
			err = errors.New("stopped before it was listening")
		}
		t.Fatalf("opstest: %s: %v", what, err)
	case <-time.After(10 * time.Second):
		t.Fatalf("opstest: %s did not start listening", what)
	}
}

// Ref is the Pool of the Stack's Profile.
func (s *Stack) Ref() poolmgr.PoolRef {
	return poolmgr.PoolRef{Name: s.Profiles[0].Pool.Name, Namespace: s.Profiles[0].Pool.Namespace}
}

// Declare declares every Profile's Pool as the Runner does, on the Hosts its
// selector picks from the Inventory.
func (s *Stack) Declare(t testing.TB) {
	t.Helper()
	d := poolmgr.NewDeclarer(s.Client)
	for _, p := range s.Profiles {
		spec, err := poolmgr.NewSpecBuilder().Build(p, poolmgr.SpecInput{
			RunnerName: RunnerName,
			Namespace:  Namespace,
			Hosts:      config.SelectHosts(&p, s.Inventory),
		})
		if err != nil {
			t.Fatalf("opstest: building pool spec: %v", err)
		}
		if _, err := d.Declare(context.Background(), spec); err != nil {
			t.Fatalf("opstest: declaring pool: %v", err)
		}
	}
}

// WaitAvailable waits until the Pool has n AVAILABLE MicroVMs.
func (s *Stack) WaitAvailable(t testing.TB, n int32) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		pool, err := s.Client.GetPool(context.Background(), s.Ref())
		if err == nil && pool.Status.Available >= n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("opstest: pool %s never reached %d available (last %v, %v)", s.Ref(), n, pool, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Dialer is a Runner-side dialer for the Stack's Hosts.
func Dialer() flintlock.Dialer { return flintlock.NewDialer() }
