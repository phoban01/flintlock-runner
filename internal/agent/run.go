package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/phoban01/flintlock-runner/internal/clock"
)

const (
	// startRetry is the wait between attempts at reading the Host's Node
	// at start.
	startRetry = 2 * time.Second
	// annotationResync is how often the annotations are written even when
	// nothing the agent knows of has changed.
	annotationResync = time.Minute
	// guardResync is how often the drain guard is checked with the API
	// server even when nothing has changed.
	guardResync = time.Minute
	// stopGrace is how long in-flight requests are given when the agent
	// stops before their streams are cut. A cut stream is a stream failure
	// for the caller, never a finished command (KF-176).
	stopGrace = 5 * time.Second
	// Keepalive of the exec API: callers keep one connection per Host with
	// pings on (HO-002), which are permitted; the agent pings idle callers
	// in turn so that a Runner that vanished does not hold a stream open.
	serverKeepaliveTime    = 30 * time.Second
	serverKeepaliveTimeout = 10 * time.Second
	clientPingMinTime      = 10 * time.Second
)

// Options are everything Run needs. Config, Kube, Claims and Flintlockd are
// required.
type Options struct {
	Config *Config
	// Kube is the API server client, holding no more than
	// deploy/agent/rbac.yaml grants.
	Kube kubernetes.Interface
	// Claims looks the claims up (KF-174, KF-181).
	Claims ClaimLookup
	// Flintlockd is the local flintlockd (KF-171); DialFlintlockd makes
	// one.
	Flintlockd *Flintlockd
	// Logger defaults to discarding.
	Logger *slog.Logger
	// Clock defaults to the real one.
	Clock clock.Clock
	// Listener, when set, is served instead of listening on the Host's
	// address. It is plain TCP; Run serves TLS on it. Tests use it to learn
	// the port.
	Listener net.Listener
	// Ready, when set, is closed once the exec API is served.
	Ready chan<- struct{}
}

// Run is the Exec Agent: it serves the exec API on the Host's internal
// address and keeps its Host's readiness, Host Service annotations and
// drain guard up to date until ctx ends. Stopping leaves the annotations
// and the guard as they are; the next Run takes them over.
func Run(ctx context.Context, opts Options) error {
	cfg := opts.Config
	if cfg == nil || opts.Kube == nil || opts.Claims == nil || opts.Flintlockd == nil {
		return errors.New("agent: Run needs a configuration, a Kubernetes client, a claim lookup and a flintlockd client")
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	clk := opts.Clock
	if clk == nil {
		clk = clock.Real{}
	}

	host, err := awaitHostNode(ctx, opts.Kube, cfg.HostNode, log, clk)
	if err != nil {
		return err
	}
	address := cfg.Address
	if address == "" {
		if address = hostInternalIP(host); address == "" {
			return fmt.Errorf("agent: the host's node %s has no internal address to serve the exec API on", cfg.HostNode)
		}
	}
	cert, err := newServingCert(cfg.TLS, address, log)
	if err != nil {
		return err
	}
	listener := opts.Listener
	if listener == nil {
		if listener, err = net.Listen("tcp", net.JoinHostPort(address, strconv.Itoa(cfg.Port))); err != nil {
			return fmt.Errorf("agent: listening on %s: %w", address, err)
		}
	}
	defer func() { _ = listener.Close() }()
	_, port, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		return err
	}
	agentAddress := net.JoinHostPort(address, port)

	s := &server{
		hostNode:    cfg.HostNode,
		fl:          opts.Flintlockd,
		authn:       &authenticator{reviews: opts.Kube.AuthenticationV1().TokenReviews(), audiences: cfg.TokenAudiences, timeout: cfg.CallTimeout},
		claims:      opts.Claims,
		clk:         clk,
		log:         log,
		openTimeout: cfg.ExecOpenTimeout,
		callTimeout: cfg.CallTimeout,
	}
	srv := grpc.NewServer(
		grpc.Creds(credentials.NewTLS(cert.tlsConfig())),
		grpc.ChainUnaryInterceptor(s.unaryInterceptor),
		grpc.ChainStreamInterceptor(s.streamInterceptor),
		grpc.KeepaliveParams(keepalive.ServerParameters{Time: serverKeepaliveTime, Timeout: serverKeepaliveTimeout}),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{MinTime: clientPingMinTime, PermitWithoutStream: true}),
	)
	s.register(srv)
	served := make(chan error, 1)
	go func() { served <- srv.Serve(listener) }()
	defer stopServer(srv, served)
	log.Info("serving the exec API", "address", agentAddress, "host_node", cfg.HostNode)
	if opts.Ready != nil {
		close(opts.Ready)
	}

	notes := &annotator{kube: opts.Kube, hostNode: cfg.HostNode, resync: annotationResync}
	guard := &drainGuard{
		cfg: cfg, log: log, clk: clk, kube: opts.Kube, claims: opts.Claims,
		hostNode: cfg.HostNode, resync: guardResync,
	}
	var lastReady *readiness
	for {
		r := checkReadiness(ctx, cfg, opts.Flintlockd)
		if lastReady == nil || *lastReady != r {
			log.Info("host readiness", "ready", r.ready, "reason", r.reason, "message", r.message)
			lastReady = &r
		}
		if err := notes.publish(ctx, hostAnnotations(cfg, r, agentAddress), clk.Now()); err != nil && ctx.Err() == nil {
			log.Warn("could not publish the host's annotations", "error", err)
		}
		if host, err := opts.Kube.CoreV1().Nodes().Get(ctx, cfg.HostNode, metav1.GetOptions{}); err != nil {
			if ctx.Err() == nil {
				log.Warn("could not read the host's node", "host_node", cfg.HostNode, "error", err)
			}
		} else if err := guard.reconcile(ctx, host); err != nil && ctx.Err() == nil {
			log.Warn("could not reconcile the drain guard", "error", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case err := <-served:
			return fmt.Errorf("agent: the exec API stopped: %w", err)
		case <-clk.After(cfg.SyncInterval):
		}
	}
}

// stopServer stops the exec API, giving in-flight requests stopGrace to
// finish and then cutting them.
func stopServer(srv *grpc.Server, served <-chan error) {
	stopped := make(chan struct{})
	go func() {
		srv.GracefulStop()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(stopGrace):
		srv.Stop()
		<-stopped
	}
	select {
	case <-served:
	default:
	}
}

// awaitHostNode reads the Host's Node, waiting for it to exist: the agent
// can start before the Host's kubelet has registered.
func awaitHostNode(ctx context.Context, kube kubernetes.Interface, name string, log *slog.Logger, clk clock.Clock) (*corev1.Node, error) {
	for {
		host, err := kube.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
		if err == nil {
			return host, nil
		}
		log.Warn("could not read the host's node yet", "host_node", name, "error", err)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-clk.After(startRetry):
		}
	}
}
