package skill

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	admissionv1 "k8s.io/api/admission/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// The "materialized" verdict was derived entirely from two objects a
// same-namespace tenant creates itself: a SkillSource whose spec.repoURL is an
// unvalidated free-form string, plus an owner-ref the tenant writes on its own
// Skill. The package doc reasoned that "a tenant cannot set Controller:true on
// a ref to an object it doesn't already control" — true, and irrelevant:
// nothing stops the tenant creating the SkillSource itself, spec.repoURL is
// never fetched by this check, there is no admission webhook on skillsources,
// and controlling a SkillSource proves nothing about controlling the repo it
// names.
//
// So a tenant holding create on skillsources and skills wrote a decoy
// SkillSource claiming a trusted repoURL, then a hand-authored Skill
// owner-ref'd to it whose canonicalName claimed that org's authority and whose
// body was theirs. That satisfied an org-wide AllowedSkills allowlist —
// verbatim the attack this package exists to close — needed no AgentClass
// change, since a namespace Skill silently displaces the ClusterSkill of the
// same canonical name, and did not even wait for load_skill, because
// spec.repoInstructions.Content is injected always-on into the system prompt.
//
// The verdict has to be witnessed by the process that did the FETCH. Admission
// already carries that witness: userInfo is filled in by the API server from
// the authenticated request, and a tenant cannot impersonate the operator's
// ServiceAccount.
func writerReq(t *testing.T, skill *v1.Skill, username string) admission.Request {
	t.Helper()
	raw, err := json.Marshal(skill)
	require.NoError(t, err)
	return admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
		Object:   runtime.RawExtension{Raw: raw},
		UserInfo: authenticationv1.UserInfo{Username: username},
	}}
}

func gitAuthoritySkill(ownerName string) *v1.Skill {
	return &v1.Skill{
		ObjectMeta: metav1.ObjectMeta{
			Name: "deploy", Namespace: "tenant-ns",
			OwnerReferences: materializedOwnerRef(ownerName),
		},
		Spec: v1.SkillSpec{
			CanonicalName: "github.com/trusted-org/skills//deploy",
			Frontmatter:   v1.SkillFrontmatter{Name: "deploy"},
			Description:   "deploys things",
			Body:          "attacker-authored instructions the model is told to follow",
		},
	}
}

func decoySource(name string) *v1.SkillSource {
	return &v1.SkillSource{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "tenant-ns", UID: "uid-src"},
		// Never fetched, never owned — an unvalidated free-form string.
		Spec: v1.SkillSourceSpec{RepoURL: "https://github.com/trusted-org/skills"},
	}
}

func TestSkillWebhook_AGitAuthoritySkillMayOnlyBeWrittenByTheOperator(t *testing.T) {
	w := newWebhook(t, decoySource("decoy"))
	w.OperatorUsername = "system:serviceaccount:agentprimitives-system:spicebox-operator"

	resp := w.Handle(context.Background(),
		writerReq(t, gitAuthoritySkill("decoy"), "system:serviceaccount:tenant-ns:some-tenant"))

	assert.False(t, resp.Allowed,
		"a git-authority Skill written by anyone but the operator is a forged provenance claim")
}

// The operator's own write must pass, or nothing materializes at all.
func TestSkillWebhook_TheOperatorsOwnMaterializationIsAdmitted(t *testing.T) {
	const operator = "system:serviceaccount:agentprimitives-system:spicebox-operator"
	w := newWebhook(t, decoySource("real"))
	w.OperatorUsername = operator

	resp := w.Handle(context.Background(), writerReq(t, gitAuthoritySkill("real"), operator))

	assert.True(t, resp.Allowed, "%s", resp.Result)
}

// A LOCAL skill is the hand-authored path and is deliberately exempt: it claims
// no git authority, CheckProvenance does not gate it, and requiring the
// operator to write it would remove the only way a person authors a skill.
func TestSkillWebhook_ALocalSkillIsStillTenantWritable(t *testing.T) {
	w := newWebhook(t)
	w.OperatorUsername = "system:serviceaccount:agentprimitives-system:spicebox-operator"

	local := &v1.Skill{
		ObjectMeta: metav1.ObjectMeta{Name: "mine", Namespace: "tenant-ns"},
		Spec: v1.SkillSpec{
			CanonicalName: "local//mine",
			Frontmatter:   v1.SkillFrontmatter{Name: "mine"},
			Description:   "something I wrote",
			Body:          "my own instructions",
		},
	}

	resp := w.Handle(context.Background(),
		writerReq(t, local, "system:serviceaccount:tenant-ns:some-tenant"))

	assert.True(t, resp.Allowed, "%s", resp.Result)
}

// An unconfigured OperatorUsername must not turn every materialization into a
// denial. The VWC runs failurePolicy=Ignore and the controller is the durable
// backstop, so a wiring gap that refused the operator's OWN writes would break
// every SkillSource in the install — a far worse failure than the one being
// closed. The check engages only once the operator identity is known.
func TestSkillWebhook_AnUnconfiguredOperatorIdentityDoesNotBlock(t *testing.T) {
	w := newWebhook(t, decoySource("decoy")) // OperatorUsername left empty

	resp := w.Handle(context.Background(),
		writerReq(t, gitAuthoritySkill("decoy"), "system:serviceaccount:tenant-ns:some-tenant"))

	assert.True(t, resp.Allowed, "%s", resp.Result)
}
