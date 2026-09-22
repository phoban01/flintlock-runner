package transport

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"sync"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
	utilexec "k8s.io/client-go/util/exec"
	"k8s.io/streaming/pkg/httpstream"
)

// KindKubeExec is the Guest Transport of a cluster fleet: every operation is
// one exec session on the claimed pod's exec subresource, which the API
// server relays to the Pod Provider of the pod's Virtual Node (KF-060).
const KindKubeExec Kind = "kube-exec"

// kubeShell is the guest shell kube-exec runs its wrapper and its readiness
// probe with. Every root filesystem image has one there.
const kubeShell = "/bin/sh"

// kubeExecWrapper is the shell program every kube-exec Run goes through,
// because a pod exec carries a command line and nothing else: no working
// directory, no environment and no user, all of which a Stage needs
// (EX-025, EX-026). Its positional parameters are the directory, the user
// and then the command to run, with its environment already in front of it
// as an `env` invocation where there is one.
//
// The directory is entered relative to where the exec starts, which is the
// guest's root: the Pod Provider opens every session without a working
// directory of its own, so the guest agent's, and on the fake Host that is
// the MicroVM's sandbox, which is the guest's root as the fake plays it.
// This is the same rule the Executor follows when it creates the builds
// directory. A directory that cannot be entered is the command's failure,
// as the exec transport's is. A user other than root is taken on with
// runuser, which is what a guest agent running as root would do for it.
const kubeExecWrapper = `dir=$1 user=$2
shift 2
if [ -n "$dir" ]; then
	case $dir in /*) dir=.$dir ;; esac
	cd -- "$dir" || exit 126
fi
if [ -n "$user" ] && [ "$user" != root ]; then
	exec runuser -u "$user" -- "$@"
fi
exec "$@"`

// kubeExecName is $0 of the wrapper, which is what the guest's shell names
// in its own error messages.
const kubeExecName = "flr-kube-exec"

// WithKubeExec configures the kube-exec Guest Transport: config is the
// Runner's client configuration of the Kubernetes API, the same one its
// Kubernetes pool backend uses, and namespace the namespace its Pool pods
// live in. Without it, a Target of KindKubeExec is refused.
func WithKubeExec(config *rest.Config, namespace string) FactoryOption {
	return func(f *factory) {
		f.kube = &kubeExec{config: config, namespace: namespace}
	}
}

// kubeExec is what every kube-exec Transport of one Factory shares: the API
// server's client configuration and the Pool pods' namespace.
type kubeExec struct {
	config    *rest.Config
	namespace string
	// spdyOnly skips the WebSocket executor; tests use it to run the same
	// behaviour over the protocol the fallback would take.
	spdyOnly bool

	once   sync.Once
	client kubernetes.Interface
	err    error
}

// build makes the client the exec URLs are built with, once for every Job.
func (k *kubeExec) build() error {
	k.once.Do(func() {
		switch {
		case k.config == nil:
			k.err = errors.New("transport: kube-exec has no kubernetes client configuration")
		case k.namespace == "":
			k.err = errors.New("transport: kube-exec has no namespace")
		default:
			k.client, k.err = kubernetes.NewForConfig(k.config)
			if k.err != nil {
				k.err = fmt.Errorf("transport: kube-exec: %w", k.err)
			}
		}
	})
	return k.err
}

// newKubeExecTransport builds the kube-exec Transport for a target. The
// target's VMUID names the claimed pod: it is the Lease id the Kubernetes
// pool backend returns, which is the pod's name (KF-046). No Host is
// contacted, or even looked at (KF-063).
func (f *factory) newKubeExecTransport(target Target) (Transport, error) {
	if f.kube == nil {
		return nil, fmt.Errorf("transport: the %q guest transport is not configured on this runner", KindKubeExec)
	}
	if err := f.kube.build(); err != nil {
		return nil, err
	}
	if target.VMUID == "" {
		return nil, errors.New("transport: kube-exec target names no pod")
	}
	return &kubeExecTransport{kube: f.kube, pod: target.VMUID}, nil
}

// kubeExecTransport is the kube-exec Guest Transport bound to one claimed
// pod.
type kubeExecTransport struct {
	kube *kubeExec
	pod  string
}

//= docs/requirements/02-executor.md#guest-transport
//# The Guest Transport SHALL provide a readiness operation that
//# succeeds only when a trivial command can be executed in the guest.

// Ready implements Transport: it runs the trivial command through the
// guest's shell over the pod's exec subresource and succeeds only when it
// ran and exited zero. A pod that is not running yet, a Pod Provider that
// is not answering and a guest that cannot run the command are all not
// ready, with the cause wrapped.
func (t *kubeExecTransport) Ready(ctx context.Context) error {
	status, err := t.exec(ctx, []string{kubeShell, "-c", readyCommand}, nil, nil, nil)
	if err != nil {
		return fmt.Errorf("pod %s/%s is not ready: %w", t.kube.namespace, t.pod, errors.Join(ErrNotReady, err))
	}
	if status != 0 {
		return fmt.Errorf("pod %s/%s: readiness command exited %d: %w", t.kube.namespace, t.pod, status, ErrNotReady)
	}
	return nil
}

//= docs/requirements/12-cluster-fleet.md#kube-exec-transport
//# Where the `kube-exec` Guest Transport is configured, the
//# Executor SHALL run each Stage by opening the exec subresource of the
//# claimed pod through the Kubernetes API, with the Stage script on standard
//# input.

//= docs/requirements/02-executor.md#guest-transport
//# The Guest Transport SHALL provide an operation that runs a
//# command in a MicroVM with a working directory, environment, user, standard
//# input stream, standard output stream, standard error stream and returns the
//# command's exit status.

// Run implements Transport: one exec session on the claimed pod, with the
// Command's standard input, the Stage script, as the session's standard
// input and its output streamed back as it arrives. The working directory,
// the environment and the user travel in the command line, through
// kubeExecWrapper, because the exec subresource has no field for any of
// them.
func (t *kubeExecTransport) Run(ctx context.Context, cmd Command) (int, error) {
	if cmd.Path == "" {
		return -1, fmt.Errorf("pod %s/%s: command has no path", t.kube.namespace, t.pod)
	}
	return t.exec(ctx, wrapCommand(cmd), cmd.Stdin, cmd.Stdout, cmd.Stderr)
}

// Close implements Transport. Every operation is its own session and the
// client configuration belongs to the Factory, so there is nothing to
// release.
func (t *kubeExecTransport) Close() error { return nil }

// wrapCommand is the command line of an exec session that runs cmd in its
// working directory, as its user and with its environment.
func wrapCommand(cmd Command) []string {
	argv := []string{kubeShell, "-c", kubeExecWrapper, kubeExecName, cmd.Dir, cmd.User}
	if len(cmd.Env) > 0 {
		keys := make([]string, 0, len(cmd.Env))
		for k := range cmd.Env {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		argv = append(argv, "env")
		for _, k := range keys {
			argv = append(argv, k+"="+cmd.Env[k])
		}
	}
	argv = append(argv, cmd.Path)
	return append(argv, cmd.Args...)
}

//= docs/requirements/12-cluster-fleet.md#kube-exec-transport
//# The `kube-exec` Guest Transport SHALL meet every requirement
//# that `02-executor.md` places on the `exec` Guest Transport for output
//# streaming, exit status, cancellation and timeouts.

// exec runs one exec session to its end and returns the exit status, as
// kubectl does: over WebSocket, falling back to SPDY where the API server,
// or a proxy in front of it, refuses the upgrade.
//
// The four behaviours the exec transport has map onto the session like
// this. Output is written as the session delivers it (EX-021). A command
// that ran reports its exit status in the session's status message, which
// client-go returns as an exit error, and that is the status here, zero or
// not (EX-022, EX-045); a session that ends any other way leaves the status
// unknown, which is a stream failure (EX-023). Cancelling ctx, or its
// deadline passing, closes the session; the Pod Provider cancels its
// MicroVMExec exchange when its side of the session closes, and that
// terminates the process in the guest, as the exec transport's cancelled
// stream does (EX-024). The remaining time on ctx is the Stage's timeout
// for the same reason: there is no timeout field to carry it (EX-046), so
// the deadline ends the session, and with it the process.
func (t *kubeExecTransport) exec(ctx context.Context, command []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	if stdout == nil {
		stdout = io.Discard
	}
	if stderr == nil {
		stderr = io.Discard
	}
	req := t.kube.client.CoreV1().RESTClient().Post().
		Namespace(t.kube.namespace).
		Resource("pods").
		Name(t.pod).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Command: command,
			Stdin:   stdin != nil,
			Stdout:  true,
			Stderr:  true,
		}, scheme.ParameterCodec)

	executor, err := t.executor(req)
	if err != nil {
		return -1, t.streamFailure("building the exec session", err)
	}
	err = executor.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdin:  stdin,
		Stdout: stdout,
		Stderr: stderr,
	})
	var exitErr utilexec.ExitError
	switch {
	case err == nil:
		return 0, nil
	case errors.As(err, &exitErr) && exitErr.Exited():
		return exitErr.ExitStatus(), nil
	case ctx.Err() != nil:
		return -1, t.streamFailure("exec session ended with its context", errors.Join(err, context.Cause(ctx)))
	default:
		return -1, t.streamFailure("exec session failed", err)
	}
}

// executor is the session's client: WebSocket first and SPDY where the
// upgrade is refused, the choice kubectl makes. The WebSocket session is a
// GET of the exec subresource and the SPDY one a POST, which is why the
// Runner's Role grants both verbs on pods/exec.
func (t *kubeExecTransport) executor(req *rest.Request) (remotecommand.Executor, error) {
	spdy, err := remotecommand.NewSPDYExecutor(t.kube.config, http.MethodPost, req.URL())
	if err != nil {
		return nil, err
	}
	if t.kube.spdyOnly {
		return spdy, nil
	}
	websocket, err := remotecommand.NewWebSocketExecutor(t.kube.config, http.MethodGet, req.URL().String())
	if err != nil {
		return nil, err
	}
	return remotecommand.NewFallbackExecutor(websocket, spdy, func(err error) bool {
		return httpstream.IsUpgradeFailure(err) || httpstream.IsHTTPSProxyError(err)
	})
}

// streamFailure wraps a session failure so that it names the pod and
// satisfies errors.Is(err, ErrStreamFailed).
func (t *kubeExecTransport) streamFailure(what string, cause error) error {
	return fmt.Errorf("pod %s/%s: %s: %w", t.kube.namespace, t.pod, what, errors.Join(cause, ErrStreamFailed))
}

// Compile-time check.
var _ Transport = (*kubeExecTransport)(nil)
