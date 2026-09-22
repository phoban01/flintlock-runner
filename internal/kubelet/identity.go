package kubelet

import (
	"context"
	"fmt"

	authenticationv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// NodeNameExtra is the key of the user information extra in which the API
// server records the node of the pod a bound ServiceAccount token was issued
// to. deploy/host-agent/admission-policy.yaml tells one Host's Pod Provider
// from another's by it.
const NodeNameExtra = "authentication.kubernetes.io/node-name"

//= docs/requirements/12-cluster-fleet.md#cluster-hardening
//# The Fleet Manifests SHALL give each Pod Provider an identity
//# that names its Host, so that the policy of KF-133 can tell one Host's Pod
//# Provider from another's.

// CheckIdentity asks the API server who the client is, with a
// SelfSubjectReview, and fails unless the answer names hostNode as the node
// of the client's pod. That holds for the in-cluster configuration of a pod
// on the Host, whose projected ServiceAccount token is bound to the pod and
// carries its node. A kubeconfig with a static token, a legacy Secret token
// or a pod on another node does not name the Host, and the admission policy
// would refuse every change the provider makes; failing here says why at
// once instead. A SelfSubjectReview needs no permission beyond being
// authenticated.
func CheckIdentity(ctx context.Context, kube kubernetes.Interface, hostNode string) error {
	review, err := kube.AuthenticationV1().SelfSubjectReviews().Create(ctx, &authenticationv1.SelfSubjectReview{}, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("kubelet: asking the API server who this provider is: %w", err)
	}
	info := review.Status.UserInfo
	nodes := info.Extra[NodeNameExtra]
	switch {
	case len(nodes) == 0:
		return fmt.Errorf("kubelet: the provider authenticates as %q, whose identity names no node; "+
			"run it with the bound ServiceAccount token of a pod on %s (the in-cluster configuration), "+
			"or the admission policy refuses everything it does", info.Username, hostNode)
	case len(nodes) != 1 || nodes[0] != hostNode:
		return fmt.Errorf("kubelet: the provider authenticates as a pod on %v, not on its Host %s", []string(nodes), hostNode)
	}
	return nil
}
