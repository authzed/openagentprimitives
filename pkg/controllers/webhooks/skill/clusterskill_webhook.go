// pkg/controllers/webhooks/skill/clusterskill_webhook.go
//
// ClusterSkillWebhook is the cluster-scoped counterpart to the namespaced
// Skill admission webhook. ClusterSkills are the more-trusted tier (visible to
// every namespace, shadow-able only by a same-named namespaced Skill), so they
// get the same admission-time validation: the canonical name / frontmatter rule
// set plus the provenance check that a hand-authored skill cannot claim a
// trusted git-authority canonical name. Mirrors webhook.go; the same
// validate.Skill + validate.CheckProvenance rule set backs both the webhook and
// the controller so they never drift.
package skill

import (
	"context"
	"net/http"
	"strings"

	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/tools/skills/materialize"
	"github.com/authzed/openagentprimitives/pkg/tools/skills/validate"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// PathClusterSkill is the URL the handler is registered under (must match the
// ValidatingWebhookConfiguration entry).
const PathClusterSkill = "/validate-clusterskill"

// ClusterSkillWebhook validates ClusterSkill objects.
type ClusterSkillWebhook struct {
	Client  client.Reader
	decoder admission.Decoder

	// OperatorUsername is the authenticated username the operator's own
	// ServiceAccount presents. Only that writer may create a skill claiming a
	// git authority — see checkWriter. Empty leaves the check inert.
	OperatorUsername string
}

// NewClusterSkillWebhook constructs the handler. Build d with admission.NewDecoder(scheme).
func NewClusterSkillWebhook(c client.Reader, d admission.Decoder) *ClusterSkillWebhook {
	return &ClusterSkillWebhook{Client: c, decoder: d}
}

func (w *ClusterSkillWebhook) Handle(ctx context.Context, req admission.Request) admission.Response {
	var skill v1.ClusterSkill
	if err := w.decoder.Decode(req, &skill); err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}
	errs := validate.Skill(skill.Spec.CanonicalName, skill.Spec.Frontmatter.Name,
		skill.Spec.Description, skill.Spec.Body)

	// materialized is derived from the unforgeable controller owner-ref to the
	// ClusterSkillSource that actually produces this authority — NOT
	// skill.Spec.Source (a user-writable field a tenant could fabricate to
	// spoof a trusted git-authority name). See pkg/tools/skills/materialize.
	materialized, err := materialize.ClusterSkill(ctx, w.Client, skill.OwnerReferences, skill.Spec.CanonicalName)
	if err != nil {
		// An inconclusive owner lookup (real API error, not NotFound) must not be
		// silently treated as either verdict; the webhook is best-effort anyway
		// (failurePolicy=Ignore), the controller backstop re-checks durably.
		return admission.Errored(http.StatusInternalServerError, err)
	}
	// The always-on prompt text: capped by the controller at materialization
	// and never looked at here, so a hand-authored Skill set it directly and it
	// reached every system prompt uncapped.
	if repo := skill.Spec.RepoInstructions; repo != nil {
		errs = append(errs, validate.RepoInstructions(repo.Content)...)
	}

	// The leg the owner-ref test was missing: WHO wrote this. See checkWriter.
	if err := checkWriter(w.OperatorUsername, skill.Spec.CanonicalName, req); err != nil {
		errs = append(errs, err)
	}
	if err := validate.CheckProvenance(skill.Spec.CanonicalName, materialized); err != nil {
		errs = append(errs, err)
	}
	if len(errs) > 0 {
		msgs := make([]string, 0, len(errs))
		for _, e := range errs {
			msgs = append(msgs, e.Error())
		}
		return admission.Denied(strings.Join(msgs, "; "))
	}
	return admission.Allowed("")
}
