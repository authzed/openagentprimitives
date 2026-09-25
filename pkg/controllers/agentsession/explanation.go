// explanation.go computes the structured per-credential explanation the
// channelsd credential_request envelope + identityd portal render for the
// user. Title/Description come from the passthrough resolver (tool field →
// provider catalog → humanized name); Why comes from the AgentClass's
// declared CredentialExplanations (empty when undeclared — the render
// surface applies its own fallback: channelsd LLM-then-static, identityd
// static).
package agentsession

import (
	"context"
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/passthrough"
)

// BuildExplanation produces the structured per-credential explanation the
// SessionUserIdentity.status.explanation carries. Title/Description come
// from the passthrough resolver (tool field → provider catalog →
// humanized name); Why comes from the AgentClass's CredentialExplanations
// (empty when undeclared — the render surface fills it).
//
// The resolver performs the same cluster reads as Required (per-ref GETs);
// a resolution error is returned to the caller, which should use a
// defensive fallback (see parkAwaitingCredentials).
func BuildExplanation(
	ctx context.Context,
	c client.Client,
	ac *spiceboxv1alpha1.AgentClass,
	missing []string,
) (*spiceboxv1alpha1.CredentialExplanation, error) {
	reqs, err := passthrough.Resolve(ctx, c, ac)
	if err != nil {
		return nil, fmt.Errorf("resolve credential metadata: %w", err)
	}
	byName := make(map[string]passthrough.CredRequirement, len(reqs))
	for _, r := range reqs {
		byName[r.Name] = r
	}
	reasonByCred := make(map[string]string, len(ac.Spec.CredentialExplanations))
	for _, ce := range ac.Spec.CredentialExplanations {
		reasonByCred[ce.Credential] = ce.Reason
	}

	items := make([]spiceboxv1alpha1.CredentialExplanationItem, 0, len(missing))
	for _, name := range missing {
		items = append(items, buildItem(name, byName[name], reasonByCred[name]))
	}
	return &spiceboxv1alpha1.CredentialExplanation{Items: items}, nil
}

// buildItem assembles one row. The enterprise sign-in sentinel gets a
// clean label instead of being humanized into "<enterprise Sign-in>".
func buildItem(name string, req passthrough.CredRequirement, reason string) spiceboxv1alpha1.CredentialExplanationItem {
	if name == enterpriseSignInSentinel {
		return spiceboxv1alpha1.CredentialExplanationItem{
			Credential: name, Title: "Enterprise sign-in", Why: reason,
		}
	}
	title := req.Title
	if title == "" {
		title = passthrough.HumanizeCredName(name)
	}
	return spiceboxv1alpha1.CredentialExplanationItem{
		Credential: name, Title: title, Description: req.Description, Why: reason,
	}
}
