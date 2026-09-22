package kubelet

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/virtual-kubelet/virtual-kubelet/errdefs"
	"github.com/virtual-kubelet/virtual-kubelet/node/api"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	corev1listers "k8s.io/client-go/listers/core/v1"
	statsv1alpha1 "k8s.io/kubelet/pkg/apis/stats/v1alpha1"
	utilexec "k8s.io/utils/exec"

	"github.com/phoban01/flintlock-runner/internal/transport"
)

// Stream timeouts of the kubelet API, the kubelet's own defaults.
const (
	streamIdleTimeout     = 4 * time.Hour
	streamCreationTimeout = 30 * time.Second
	readHeaderTimeout     = 30 * time.Second
)

//= docs/requirements/12-cluster-fleet.md#microvm-pods
//# The Pod Provider SHALL serve the pod exec endpoint of the
//# kubelet API by relaying the streams to `MicroVMExec.ExecCommand` on the
//# local `flintlockd` and SHALL return the command's exit status.

// RunInContainer is the pod exec endpoint: it runs cmd in the pod's MicroVM
// through the exec Guest Transport, which is one MicroVMExec.ExecCommand
// exchange on the local flintlockd, with the caller's streams. A non-zero
// exit status is returned as an exit error, which the kubelet API's
// streaming layer reports to the client as the command's exit code; a
// stream that fails before the exit status stays an error, so that the two
// are never confused (EX-023).
//
// There is one container and it is the machine, so the container name is
// not looked at. A terminal is refused: MicroVMExec has none to offer.
func (p *Provider) RunInContainer(ctx context.Context, namespace, podName, _ string, cmd []string, attach api.AttachIO) error {
	key := podKey(namespace, podName)
	if len(cmd) == 0 {
		return errdefs.InvalidInput("exec needs a command")
	}
	if attach.TTY() {
		return errdefs.InvalidInput("a microvm pod has no terminal to attach; run the command without a tty")
	}
	p.mu.Lock()
	rec, ok := p.pods[key]
	var uid string
	var ready bool
	if ok {
		uid, ready = rec.vmUID, rec.ready
	}
	p.mu.Unlock()
	if !ok || uid == "" {
		return errdefs.NotFoundf("pod %s has no microvm on this host", key)
	}
	if !ready {
		return errdefs.InvalidInputf("pod %s is not running", key)
	}

	guest, err := p.transports.New(ctx, transport.Target{Kind: transport.KindExec, Host: p.host, VMUID: uid, Deadline: probeTimeout})
	if err != nil {
		return err
	}
	defer func() { _ = guest.Close() }()

	command := transport.Command{Path: cmd[0], Args: cmd[1:]}
	// Typed nils would make the transport believe it has streams.
	if in := attach.Stdin(); in != nil {
		command.Stdin = in
	}
	if out := attach.Stdout(); out != nil {
		command.Stdout = out
		defer func() { _ = out.Close() }()
	}
	if errOut := attach.Stderr(); errOut != nil {
		command.Stderr = errOut
		defer func() { _ = errOut.Close() }()
	}

	status, err := guest.Run(ctx, command)
	if err != nil {
		return fmt.Errorf("exec in pod %s: %w", key, err)
	}
	if status != 0 {
		return utilexec.CodeExitError{Err: fmt.Errorf("command terminated with exit code %d", status), Code: status}
	}
	return nil
}

// The rest of the kubelet API is not offered. A MicroVM has no container
// log, nothing to attach to and no pod network to forward into.

// GetContainerLogs is not supported.
func (p *Provider) GetContainerLogs(context.Context, string, string, string, api.ContainerLogOpts) (io.ReadCloser, error) {
	return nil, errdefs.InvalidInput("a microvm pod has no container log; the job log is in GitLab")
}

// AttachToContainer is not supported.
func (p *Provider) AttachToContainer(context.Context, string, string, string, api.AttachIO) error {
	return errdefs.InvalidInput("a microvm pod has no process to attach to")
}

// PortForward is not supported.
func (p *Provider) PortForward(context.Context, string, string, int32, io.ReadWriteCloser) error {
	return errdefs.InvalidInput("a microvm pod has no cluster networking to forward into")
}

// GetStatsSummary reports the node with no pod statistics.
func (p *Provider) GetStatsSummary(context.Context) (*statsv1alpha1.Summary, error) {
	return &statsv1alpha1.Summary{Node: statsv1alpha1.NodeStats{NodeName: p.nodeName}}, nil
}

// GetMetricsResource reports no resource metrics.
func (p *Provider) GetMetricsResource(context.Context) ([]*dto.MetricFamily, error) {
	return nil, nil
}

//= docs/requirements/12-cluster-fleet.md#microvm-pods
//# The Pod Provider SHALL serve its kubelet API over TLS and SHALL
//# reject every request that does not authenticate with a client certificate
//# issued by the cluster's kubelet client certificate authority.

// kubeletTLSConfig is the TLS configuration of the kubelet API. The client
// certificate is required and verified in the handshake against the one
// configured certificate authority, and nothing else: the pool starts empty,
// so the system roots are never trusted, and there is no mode without it.
// A connection with no certificate, or one from another authority, does not
// get as far as a request.
func kubeletTLSConfig(files ServerTLS) (*tls.Config, error) {
	if files.CertFile == "" || files.KeyFile == "" || files.ClientCAFile == "" {
		return nil, errors.New("kubelet: the serving certificate, its key and the client certificate authority are all required")
	}
	cert, err := tls.LoadX509KeyPair(files.CertFile, files.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("kubelet: loading the serving certificate: %w", err)
	}
	pem, err := os.ReadFile(files.ClientCAFile)
	if err != nil {
		return nil, fmt.Errorf("kubelet: reading the client certificate authority: %w", err)
	}
	clientCAs := x509.NewCertPool()
	if !clientCAs.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("kubelet: %s holds no certificate", files.ClientCAFile)
	}
	return &tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{cert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    clientCAs,
	}, nil
}

// requireClientCertificate is the second lock on the same door: it refuses
// any request that did not arrive with a verified client certificate chain.
// With kubeletTLSConfig in place none can, and this handler is what keeps
// that true if the listener is ever built another way.
func requireClientCertificate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS == nil || len(r.TLS.VerifiedChains) == 0 {
			http.Error(w, "a client certificate issued by the kubelet client certificate authority is required", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// kubeletHandler is the kubelet API: virtual kubelet's pod routes over the
// provider, behind the client certificate check.
func (p *Provider) kubeletHandler(pods corev1listers.PodLister) http.Handler {
	return requireClientCertificate(api.PodHandler(api.PodHandlerConfig{
		RunInContainer:    p.RunInContainer,
		AttachToContainer: p.AttachToContainer,
		GetContainerLogs:  p.GetContainerLogs,
		PortForward:       p.PortForward,
		GetPods:           p.GetPods,
		GetPodsFromKubernetes: func(context.Context) ([]*corev1.Pod, error) {
			return pods.List(labels.Everything())
		},
		GetStatsSummary:       p.GetStatsSummary,
		GetMetricsResource:    p.GetMetricsResource,
		StreamIdleTimeout:     streamIdleTimeout,
		StreamCreationTimeout: streamCreationTimeout,
	}, false))
}
