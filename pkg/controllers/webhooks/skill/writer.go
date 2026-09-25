package skill

import (
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"github.com/authzed/openagentprimitives/pkg/tools/skills/canonical"
)

// checkWriter enforces that a GIT-AUTHORITY skill may only be written by the
// operator itself.
//
// The materialize package's "materialized" verdict is derived from two objects
// a same-namespace tenant creates itself: a SkillSource whose spec.repoURL is
// an unvalidated free-form string never fetched by the check, plus an owner-ref
// the tenant writes on its own Skill. Its doc reasoned that "a tenant cannot
// set Controller:true on a ref to an object it doesn't already control" —
// which is true, and beside the point: nothing stops the tenant creating that
// SkillSource, there is no admission webhook on skillsources, and controlling a
// SkillSource proves nothing about controlling the repo it names.
//
// So a tenant holding create on skillsources and skills wrote a decoy
// SkillSource claiming a trusted repoURL, then a hand-authored Skill
// owner-ref'd to it, claiming that org's authority for its own body. That
// satisfied an org-wide AllowedSkills allowlist — verbatim the attack
// materialize exists to close — needed no AgentClass change, since a namespace
// Skill silently displaces the ClusterSkill of the same canonical name, and did
// not even wait for load_skill, because spec.repoInstructions.Content is
// injected always-on into the system prompt.
//
// The verdict has to be witnessed by the process that did the FETCH, and
// admission already carries that witness: userInfo is filled in by the API
// server from the authenticated request, and a tenant cannot impersonate the
// operator's ServiceAccount. This does not replace the owner-ref and authority
// checks; it is the leg they were missing.
//
// LOCAL skills are exempt: they claim no git authority, CheckProvenance does
// not gate them, and requiring the operator to write one would remove the only
// way a person authors a skill by hand.
//
// An EMPTY operatorUsername leaves the check inert. The VWC runs
// failurePolicy=Ignore and the controller is the durable backstop, so a wiring
// gap that refused the operator's own writes would break every SkillSource in
// the install — a far worse failure than the one being closed.
func checkWriter(operatorUsername, canonicalName string, req admission.Request) error {
	if operatorUsername == "" {
		return nil
	}
	n, err := canonical.Parse(canonicalName)
	if err != nil {
		// A name that does not parse is already denied by validate.Skill; there
		// is no authority claim here to adjudicate.
		return nil
	}
	if n.IsLocal {
		return nil
	}
	if req.UserInfo.Username == operatorUsername {
		return nil
	}
	return fmt.Errorf(
		"a skill claiming the git authority %q may only be created by the platform that fetched it "+
			"(written by %q); author it under the \"local\" authority instead, or add it to a "+
			"SkillSource/ClusterSkillSource so the platform materializes it",
		n.Authority, req.UserInfo.Username)
}
