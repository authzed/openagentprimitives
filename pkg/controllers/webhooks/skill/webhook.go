// pkg/controllers/webhooks/skill/webhook.go
//
// Package skill provides the validating admission webhook for the Skill CRD.
// It denies a Skill whose canonical name or frontmatter is malformed at
// CREATE/UPDATE — the admission-time counterpart to the controller's status
// condition. The VWC uses failurePolicy=Ignore (like its siblings), so this is
// best-effort: when the webhook is unreachable a malformed Skill is admitted
// and the controller's Valid=False condition is the durable backstop. The same
// validate.Skill rule set backs both so they never drift. The provenance
// check's "materialized?" verdict comes from pkg/tools/skills/materialize (an
// unforgeable controller owner-ref + authority match), never from the
// user-writable spec.Source field.
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

// PathSkill is the URL the handler is registered under (must match the
// ValidatingWebhookConfiguration entry).
const PathSkill = "/validate-skill"

// Webhook validates Skill objects.
type Webhook struct {
	Client  client.Reader
	decoder admission.Decoder

	// OperatorUsername is the authenticated username the operator's own
	// ServiceAccount presents ("system:serviceaccount:<ns>:<sa>"). Only that
	// writer may create a skill claiming a git authority — see checkWriter for
	// why the owner-ref test alone was forgeable. Empty leaves the check inert.
	OperatorUsername string
}

// NewSkillWebhook constructs the handler. Build d with admission.NewDecoder(scheme).
func NewSkillWebhook(c client.Reader, d admission.Decoder) *Webhook {
	return &Webhook{Client: c, decoder: d}
}

func (w *Webhook) Handle(ctx context.Context, req admission.Request) admission.Response {
	var skill v1.Skill
	if err := w.decoder.Decode(req, &skill); err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}
	errs := validate.Skill(skill.Spec.CanonicalName, skill.Spec.Frontmatter.Name,
		skill.Spec.Description, skill.Spec.Body)

	// materialized is derived from the unforgeable controller owner-ref to the
	// SkillSource that actually produces this authority — NOT skill.Spec.Source
	// (a user-writable field a tenant could fabricate to spoof a trusted
	// git-authority name). See pkg/tools/skills/materialize.
	materialized, err := materialize.Skill(ctx, w.Client, skill.Namespace, skill.OwnerReferences, skill.Spec.CanonicalName)
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

	// The leg the owner-ref test was missing: WHO wrote this. materialize's
	// three checks all read objects a same-namespace tenant creates itself, so
	// they cannot distinguish a materialization from a forgery. Admission
	// carries the witness — userInfo comes from the API server, not the object.
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
