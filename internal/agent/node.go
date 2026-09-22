package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"strconv"
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/emptypb"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"

	"github.com/phoban01/flintlock-runner/internal/hostcheck"
	"github.com/phoban01/flintlock-runner/internal/kubelabels"
)

// Reasons of the readiness the agent reports.
const (
	reasonReady              = "Ready"
	reasonHostImageNotReady  = "HostImageNotReady"
	reasonFlintlockdNotReady = "FlintlockdNotReady"
	reasonExecDisabled       = "ExecDisabled"
	reasonHostServiceDown    = "HostServiceUnreachable"
)

// readiness is the outcome of one readiness check.
type readiness struct {
	ready   bool
	reason  string
	message string
}

// maxMessageLength caps the readiness message on the Node, which lists
// everything that is wrong and could otherwise grow without bound.
const maxMessageLength = 1024

//= docs/requirements/12-cluster-fleet.md#exec-agent
//# The Exec Agent SHALL report its Host not ready while the local
//# `flintlockd` does not answer `ServerInfo` with the exec service enabled,
//# while an enabled Host Service does not accept connections on the bridge
//# gateway address, or while a unit of the Host Image reports a not ready
//# reason.

// checkReadiness decides whether the Host is ready. Every check runs, so
// that the message lists everything that is wrong at once; the reason is
// that of the first failing check, in the order an operator would want to
// fix them: a Host Image unit that gave up (without KVM flintlockd is not
// even started, HI-011), then flintlockd, then the Host Services.
//
// A flintlockd that does not implement ServerInfo is not ready here,
// unlike for the Runner (HO-013): the Host Image pins a flintlockd that has
// it, and without an answer the agent cannot know that exec is enabled.
func checkReadiness(ctx context.Context, cfg *Config, fl *Flintlockd) readiness {
	var problems []string
	reason := ""
	fail := func(why, message string) {
		if reason == "" {
			reason = why
		}
		problems = append(problems, message)
	}

	reasons, err := hostcheck.ReadNotReadyReasons(cfg.NotReadyDir)
	if err != nil {
		fail(reasonHostImageNotReady, err.Error())
	}
	for _, r := range reasons {
		fail(reasonHostImageNotReady, r.Unit+": "+r.Reason)
	}

	infoCtx, cancel := context.WithTimeout(ctx, cfg.CallTimeout)
	info, err := fl.vms.ServerInfo(infoCtx, &emptypb.Empty{})
	cancel()
	switch {
	case err != nil:
		fail(reasonFlintlockdNotReady, "flintlockd does not answer ServerInfo: "+err.Error())
	case !info.GetExec().GetEnabled():
		fail(reasonExecDisabled, "flintlockd reports the exec service disabled")
	}

	for _, name := range cfg.EnabledHostServices() {
		addr := cfg.hostServiceAddress(name)
		if err := hostcheck.DialTCP(ctx, addr); err != nil {
			fail(reasonHostServiceDown, fmt.Sprintf("host service %s does not accept connections on %s: %v", name, addr, err))
		}
	}

	if reason != "" {
		message := strings.Join(problems, "; ")
		if len(message) > maxMessageLength {
			message = message[:maxMessageLength]
		}
		return readiness{reason: reason, message: message}
	}
	return readiness{ready: true, reason: reasonReady, message: "flintlockd answers with exec enabled and every enabled Host Service accepts connections"}
}

//= docs/requirements/12-cluster-fleet.md#exec-agent
//# The Exec Agent SHALL publish the address and port of each
//# enabled Host Service, under the names `buildkit`, `go_proxy`,
//# `registry_mirror` and `http_cache`, as annotations on its Host's Node.

// hostAnnotations are the annotations the agent keeps on its Host's Node:
// one per enabled Host Service, keyed by kubelabels.HostServiceAnnotation
// with the service's name and valued `address:port` on the bridge gateway,
// which is what the Executor reads for a Job (KF-189); the Host's
// readiness (KF-178); and the address of the exec API. A Host Service that
// is not enabled has its annotation removed, a null in the merge patch, so
// that a service switched off stops being offered to Jobs.
func hostAnnotations(cfg *Config, r readiness, agentAddress string) map[string]any {
	out := map[string]any{}
	for _, name := range kubelabels.HostServiceNames() {
		out[kubelabels.HostServiceAnnotation(name)] = nil
	}
	for _, name := range cfg.EnabledHostServices() {
		out[kubelabels.HostServiceAnnotation(name)] = cfg.hostServiceAddress(name)
	}
	out[kubelabels.AnnotationExecAgentReady] = strconv.FormatBool(r.ready)
	out[kubelabels.AnnotationExecAgentReason] = r.reason
	out[kubelabels.AnnotationExecAgentMessage] = r.message
	out[kubelabels.AnnotationExecAgentAddress] = agentAddress
	return out
}

// annotator patches the agent's annotations onto its Host's Node, and only
// when they differ from what it last wrote, with a full write every resync
// period in case something else changed them.
type annotator struct {
	kube     kubernetes.Interface
	hostNode string
	resync   time.Duration
	last     map[string]any
	lastAt   time.Time
}

// publish writes want to the Host's Node when it has changed.
func (a *annotator) publish(ctx context.Context, want map[string]any, now time.Time) error {
	if a.last != nil && maps.Equal(a.last, want) && now.Sub(a.lastAt) < a.resync {
		return nil
	}
	if err := patchNodeAnnotations(ctx, a.kube, a.hostNode, want); err != nil {
		a.last = nil
		return err
	}
	a.last, a.lastAt = maps.Clone(want), now
	return nil
}

// patchNodeAnnotations applies a JSON merge patch of annotations, and of
// nothing else, to a Node: the admission policy of KF-180 refuses the
// agent any other change.
func patchNodeAnnotations(ctx context.Context, kube kubernetes.Interface, node string, annotations map[string]any) error {
	data, err := json.Marshal(map[string]any{"metadata": map[string]any{"annotations": annotations}})
	if err != nil {
		return err
	}
	if _, err := kube.CoreV1().Nodes().Patch(ctx, node, types.MergePatchType, data, metav1.PatchOptions{}); err != nil {
		return fmt.Errorf("annotating the host's node %s: %w", node, err)
	}
	return nil
}

// hostInternalIP is the Host's internal address.
func hostInternalIP(host *corev1.Node) string {
	for _, addr := range host.Status.Addresses {
		if addr.Type == corev1.NodeInternalIP {
			return addr.Address
		}
	}
	return ""
}
