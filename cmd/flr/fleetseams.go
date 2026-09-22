package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"runtime"
	"sync"
	"time"

	"github.com/phoban01/flintlock-runner/internal/config"
	"github.com/phoban01/flintlock-runner/internal/fleet"
	"github.com/phoban01/flintlock-runner/internal/fleet/awsclient"
	"github.com/phoban01/flintlock-runner/internal/fleet/discovery"
	"github.com/phoban01/flintlock-runner/internal/fleet/drain"
	"github.com/phoban01/flintlock-runner/internal/fleet/provision"
	"github.com/phoban01/flintlock-runner/internal/fleet/remote"
	"github.com/phoban01/flintlock-runner/internal/fleet/scripts"
	"github.com/phoban01/flintlock-runner/internal/flintlock"
	"github.com/phoban01/flintlock-runner/internal/poolmgr"
	"github.com/phoban01/flintlock-runner/internal/transport"
)

// awsClients are the AWS clients of one region, behind the Fleet
// Controller's narrow interfaces (TD-040).
type awsClients struct {
	EC2        fleet.EC2
	SSM        fleet.SSM
	Parameters fleet.Parameters
}

// fleetSeams are the edges of the fleet subcommands: where they reach AWS,
// the Control Node, the Pool Manager and the Hosts. productionSeams wires
// the real ones. Tests put fakes at the edges (AWS, ControlRemote, Dial,
// PoolManager) and keep the wiring in between, or replace a whole
// dependency through one of the overrides.
type fleetSeams struct {
	// AWS returns the AWS clients for a region. It is called only when the
	// fleet uses AWS at all (needsAWS), or for teardown --terminate.
	AWS func(ctx context.Context, region string) (*awsClients, error)
	// ControlRemote runs the control_node script on the Control Node: a
	// provision.LocalRemote, because the Fleet Controller runs there.
	ControlRemote fleet.Remote
	// RemoteOptions are passed to remote.New.
	RemoteOptions []remote.Option
	// Dial is the Provisioner's FL-047 reachability dial; nil uses
	// net.Dialer.
	Dial func(ctx context.Context, network, address string) (net.Conn, error)
	// ControlReadyTimeout bounds the wait for the Pool Manager daemon to
	// answer after an install or reload; zero is the provision default.
	ControlReadyTimeout time.Duration

	// Overrides of whole dependencies; nil builds the real one.
	Discovery   func(ctx context.Context, cfg *config.Config, report func(discovery.Unsupported)) (fleet.Discovery, error)
	Remote      func(ctx context.Context, cfg *config.Config) (fleet.Remote, error)
	Scripts     func(cfg *config.Config) (fleet.Scripts, error)
	Provisioner func(cfg *config.Config, deps fleet.Deps, certs *fleet.CertBundle, inv func() fleet.Inventory) (fleet.Provisioner, error)
	EC2         func(ctx context.Context, cfg *config.Config) (fleet.EC2, error)
	Control     func(ctx context.Context, cfg *config.Config, deps fleet.Deps) (fleet.ControlNode, error)

	// PoolManager, Hosts and Transports are the Runner's own clients.
	PoolManager func(cfg *config.Config) (poolmgr.Client, error)
	Hosts       func() flintlock.Dialer
	Transports  func() transport.Factory
	// ClusterVerifier builds `fleet verify` for a cluster fleet; nil builds
	// the real one with the Runner's Kubernetes identity.
	ClusterVerifier func(cfg *config.Config, timeout time.Duration, out io.Writer) (clusterVerifier, error)
	// Leases counts leased MicroVMs on a drained Host.
	Leases func(pm poolmgr.Client) drain.LeaseReporter
	// Now is the clock for the Inventory's generated_at.
	Now func() time.Time
	// Executable is the flr binary fleet up --install-runner
	// installs as the Runner's service.
	Executable func() (string, error)
	// RunnerInstalled reports whether the Runner's systemd unit is
	// installed on the Control Node, where the Fleet Controller runs, so
	// that fleet teardown stops it there.
	RunnerInstalled func() bool
}

// productionSeams are the seams of the released binary.
func productionSeams() fleetSeams {
	return fleetSeams{
		AWS:           cachedAWS(loadAWS),
		ControlRemote: provision.LocalRemote{},
		PoolManager: func(cfg *config.Config) (poolmgr.Client, error) {
			return poolmgr.NewClient(poolmgr.ClientConfig{
				Endpoint: cfg.PoolManager.Endpoint,
				TLS:      cfg.PoolManager.TLS,
				Deadline: cfg.PoolManager.Deadline,
			})
		},
		Hosts:      func() flintlock.Dialer { return flintlock.NewDialer() },
		Transports: func() transport.Factory { return transport.NewFactory() },
		Leases:     func(pm poolmgr.Client) drain.LeaseReporter { return drain.PoolLeases{Pools: pm} },
		Now:        time.Now,
		Executable: os.Executable,
		RunnerInstalled: func() bool {
			_, err := os.Stat(scripts.RunnerUnitPath)
			return err == nil
		},
	}
}

// loadAWS builds the real AWS clients for region, with credentials from
// the SDK's default chain (FL-006).
func loadAWS(ctx context.Context, region string) (*awsClients, error) {
	cfg, err := awsclient.LoadConfig(ctx, region)
	if err != nil {
		return nil, err
	}
	return &awsClients{
		EC2:        awsclient.NewEC2(cfg),
		SSM:        awsclient.NewSSM(cfg),
		Parameters: awsclient.NewParameters(cfg),
	}, nil
}

// cachedAWS loads the clients of a region once.
func cachedAWS(load func(ctx context.Context, region string) (*awsClients, error)) func(ctx context.Context, region string) (*awsClients, error) {
	var mu sync.Mutex
	byRegion := map[string]*awsClients{}
	return func(ctx context.Context, region string) (*awsClients, error) {
		mu.Lock()
		defer mu.Unlock()
		if c, ok := byRegion[region]; ok {
			return c, nil
		}
		c, err := load(ctx, region)
		if err != nil {
			return nil, err
		}
		byRegion[region] = c
		return c, nil
	}
}

// needsAWS reports whether a fleet reaches AWS: every fleet but a static
// list provisioned over SSH, which needs no AWS credentials at all.
func needsAWS(f *config.Fleet) bool {
	return len(f.Discovery.Static) == 0 || f.Remote.Mode != config.RemoteSSH
}

// aws returns the fleet's AWS clients, or nil when it needs none.
func (s fleetSeams) aws(ctx context.Context, cfg *config.Config) (*awsClients, error) {
	if !needsAWS(cfg.Fleet) {
		return nil, nil
	}
	if s.AWS == nil {
		return nil, errors.New("fleet: no AWS clients are configured")
	}
	c, err := s.AWS(ctx, cfg.Fleet.Region)
	if err != nil {
		return nil, fmt.Errorf("fleet: %w", err)
	}
	return c, nil
}

// discovery is the configured instance discovery (FL-001, FL-002,
// TD-044): the static list, explicit instance ids or the tag, as
// discovery.New chooses. Instances it will not provision go to report.
func (s fleetSeams) discovery(ctx context.Context, cfg *config.Config, report func(discovery.Unsupported)) (fleet.Discovery, error) {
	if s.Discovery != nil {
		return s.Discovery(ctx, cfg, report)
	}
	var ec2 fleet.EC2
	if len(cfg.Fleet.Discovery.Static) == 0 {
		c, err := s.aws(ctx, cfg)
		if err != nil {
			return nil, err
		}
		ec2 = c.EC2
	}
	return discovery.New(cfg.Fleet, ec2, discovery.WithUnsupported(report))
}

// remote is the configured remote execution (FL-010, FL-011): Systems
// Manager Run Command unless SSH is configured, which needs no SSM client.
func (s fleetSeams) remote(ctx context.Context, cfg *config.Config) (fleet.Remote, error) {
	if s.Remote != nil {
		return s.Remote(ctx, cfg)
	}
	var ssm fleet.SSM
	if cfg.Fleet.Remote.Mode != config.RemoteSSH {
		c, err := s.aws(ctx, cfg)
		if err != nil {
			return nil, err
		}
		ssm = c.SSM
	}
	return remote.New(cfg.Fleet, ssm, s.RemoteOptions...)
}

// parameters reads Systems Manager parameters on the Control Node, or is
// nil for a fleet without AWS, whose scripts then read them on the Host.
func (s fleetSeams) parameters(ctx context.Context, cfg *config.Config) (fleet.Parameters, error) {
	c, err := s.aws(ctx, cfg)
	if err != nil || c == nil {
		return nil, err
	}
	return c.Parameters, nil
}

// scripts renders the provisioning, drain and teardown scripts.
func (s fleetSeams) scripts(cfg *config.Config) (fleet.Scripts, error) {
	if s.Scripts != nil {
		return s.Scripts(cfg)
	}
	return scripts.New()
}

// ec2 is only needed by teardown --terminate.
func (s fleetSeams) ec2(ctx context.Context, cfg *config.Config) (fleet.EC2, error) {
	if s.EC2 != nil {
		return s.EC2(ctx, cfg)
	}
	if s.AWS == nil {
		return nil, errors.New("fleet: no AWS clients are configured")
	}
	c, err := s.AWS(ctx, cfg.Fleet.Region)
	if err != nil {
		return nil, fmt.Errorf("fleet: %w", err)
	}
	return c.EC2, nil
}

// provisioner provisions one instance end to end with deps.Scripts,
// deps.Remote and, when set, deps.Parameters. certs is the TLS material,
// nil for an insecure fleet; inv is the Inventory the guest firewall's peer
// addresses come from (FL-046).
func (s fleetSeams) provisioner(cfg *config.Config, deps fleet.Deps, certs *fleet.CertBundle, inv func() fleet.Inventory) (fleet.Provisioner, error) {
	if s.Provisioner != nil {
		return s.Provisioner(cfg, deps, certs, inv)
	}
	o := provision.OptionsFrom(cfg, deps, certs, inv)
	if s.Dial != nil {
		o.Dial = s.Dial
	}
	return provision.New(o)
}

// controlNodeName is how the Control Node is named in its own script's
// output and in the Pool Manager daemon's certificate.
const controlNodeName = "control-node"

// control installs and reloads the Pool Manager daemon on the Control Node
// (FL-051, FL-055) with deps.Scripts, deps.PoolManager and deps.Certs. The
// daemon listens where the Runner dials it, verifies flintlockd with the
// fleet's CA, and, unless the Pool Manager is configured insecure, serves
// TLS with a certificate from the same authority as the Hosts'.
func (s fleetSeams) control(ctx context.Context, cfg *config.Config, deps fleet.Deps) (fleet.ControlNode, error) {
	if s.Control != nil {
		return s.Control(ctx, cfg, deps)
	}
	if s.ControlRemote == nil {
		return nil, errors.New("fleet: no way to run scripts on the Control Node")
	}
	host, _, err := net.SplitHostPort(cfg.PoolManager.Endpoint)
	if err != nil {
		return nil, fmt.Errorf("fleet: pool_manager.endpoint %q: %w", cfg.PoolManager.Endpoint, err)
	}
	self := fleet.Instance{ID: controlNodeName, Arch: config.Architecture(runtime.GOARCH), PrivateIP: host, State: "running"}
	opts := map[string]string{scripts.OptionPoolManagerListen: cfg.PoolManager.Endpoint}
	if !cfg.Fleet.Flintlockd.Insecure || !cfg.PoolManager.TLS.Insecure {
		var certHosts []fleet.Instance
		if !cfg.PoolManager.TLS.Insecure {
			certHosts = append(certHosts, self)
		}
		b, err := deps.Certs.Ensure(ctx, tlsDir(cfg), certHosts)
		if err != nil {
			return nil, fmt.Errorf("fleet: TLS material for the Pool Manager: %w", err)
		}
		if !cfg.Fleet.Flintlockd.Insecure {
			opts[scripts.OptionHostCAFile] = b.CAFile
		}
		if hc, ok := b.Hosts[self.ID]; ok {
			opts[scripts.OptionPoolManagerCert] = hc.CertFile
			opts[scripts.OptionPoolManagerKey] = hc.KeyFile
		}
	}
	return provision.NewControlNode(provision.ControlNodeOptions{
		Scripts:      deps.Scripts,
		Remote:       s.ControlRemote,
		Self:         self,
		Fleet:        *cfg.Fleet,
		Options:      opts,
		PoolManager:  deps.PoolManager,
		ReadyTimeout: s.ControlReadyTimeout,
		Out:          deps.Out,
	})
}
