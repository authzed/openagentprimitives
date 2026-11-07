package agentclass

import (
	"context"
	"fmt"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// agentIdentityUnusedProblem reports (reason, message) for
// AgentClassConditionAgentIdentityInUse, or ("","") when there is nothing to
// warn about: no spec.agentIdentity, a tool that consumes it
// (AgentClassSpec.ConsumesAgentIdentity), an identity that does not exist
// (nothing is bound, so nothing is wasted), or one carrying no credentials.
//
// The warning matters because an unconsumed identity is not merely inert: it
// is a credential bound to an agent that will never use it, and it usually
// means a tool ref was dropped from the class while the identity stayed. A
// non-NotFound Get error is returned so the caller can log it and leave the
// condition as it was rather than guess.
func agentIdentityUnusedProblem(ctx context.Context, reader client.Reader, ac *spiceboxv1alpha1.AgentClass) (reason, message string, err error) {
	if ac.Spec.AgentIdentity == "" || ac.Spec.ConsumesAgentIdentity() {
		return "", "", nil
	}
	var ai spiceboxv1alpha1.AgentIdentity
	if err := reader.Get(ctx, types.NamespacedName{Namespace: ac.Namespace, Name: ac.Spec.AgentIdentity}, &ai); err != nil {
		if apierrors.IsNotFound(err) {
			return "", "", nil
		}
		return "", "", fmt.Errorf("get AgentIdentity %q: %w", ac.Spec.AgentIdentity, err)
	}
	if len(ai.Spec.Credentials) == 0 {
		return "", "", nil
	}
	names := make([]string, 0, len(ai.Spec.Credentials))
	for _, c := range ai.Spec.Credentials {
		names = append(names, c.Name)
	}
	return spiceboxv1alpha1.ReasonAgentIdentityUnused, fmt.Sprintf(
		"spec.agentIdentity %q carries credential(s) %s, but the class offers no MCPServer, ToolBundle, or SidecarToolbox that uses them — "+
			"add the tool that needs them, or drop spec.agentIdentity",
		ac.Spec.AgentIdentity, strings.Join(names, ", ")), nil
}
