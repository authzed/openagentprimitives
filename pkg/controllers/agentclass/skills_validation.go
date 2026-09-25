// pkg/controllers/agentclass/skills_validation.go
//
// Two independent skill checks live here:
//
//   - validateSkills checks that every skill the AgentClass opts into
//     (spec.skills, matched by AgentSkill.Ref) resolves to a materialized Skill
//     or ClusterSkill that is itself Valid=True. A missing or not-yet-Valid skill
//     parks the AgentClass at Valid=False — the existing classIsValid gate in the
//     AgentSession reconciler then refuses to spawn sessions until the skill
//     materializes (fail-closed + diagnosable, vs. the runner's previous
//     log-and-skip). The class recovers automatically: the SetupWithManager
//     watches on Skill/ClusterSkill re-enqueue this AgentClass when the
//     referenced skill flips Valid=True.
//   - validateSkillsSpec checks the structural, cluster-access-free part of
//     skill staging: AgentSkill.Name uniqueness and each toolBundle's
//     StageSkills references (§4.4 rules 1-3). It never touches the cluster,
//     so the reconciler runs it before the (cluster-dependent) validateSkills
//     resolution gate.
package agentclass

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/conditions"
)

// resolvedSkill carries the Valid-condition for one canonical name, regardless
// of whether it came from a namespace Skill or a cluster-scoped ClusterSkill.
type resolvedSkill struct {
	conds []metav1.Condition
}

// validateSkills resolves each opted-in canonical skill name to a materialized
// Skill (namespace) or ClusterSkill (cluster) and confirms it is Valid=True.
// Returns ("","") when there are no skills or all resolve+valid; otherwise a
// (reason, message) for setInvalid. Mirrors validateMCPServers /
// validateSidecarToolboxes in style; namespace Skills shadow ClusterSkills of
// the same canonical name (same rule as the runner's mergeSkillSources / the
// session reconciler's mergeSkillEntries).
func validateSkills(ctx context.Context, reader client.Reader, namespace string, skills []spiceboxv1alpha1.AgentSkill) (reason, message string) {
	if len(skills) == 0 {
		return "", ""
	}

	byCanonical := make(map[string]resolvedSkill)

	// Namespace Skills first — they take precedence over a ClusterSkill of the
	// same canonical name.
	var nsList spiceboxv1alpha1.SkillList
	if err := reader.List(ctx, &nsList, client.InNamespace(namespace)); err != nil {
		// A List failure is a transient API-server condition, not a spec
		// problem: return it to the caller so the reconcile requeues rather
		// than wrongly parking the class as SkillMissing.
		return spiceboxv1alpha1.ReasonAgentClassSkillInvalid,
			fmt.Sprintf("listing Skills in namespace %s: %v", namespace, err)
	}
	for i := range nsList.Items {
		sk := &nsList.Items[i]
		byCanonical[sk.Spec.CanonicalName] = resolvedSkill{conds: sk.Status.Conditions}
	}

	// Cluster-scoped ClusterSkills fill in any canonical name not already
	// covered by a namespace Skill (namespace-wins).
	var clusterList spiceboxv1alpha1.ClusterSkillList
	if err := reader.List(ctx, &clusterList); err != nil {
		return spiceboxv1alpha1.ReasonAgentClassSkillInvalid,
			fmt.Sprintf("listing ClusterSkills: %v", err)
	}
	for i := range clusterList.Items {
		csk := &clusterList.Items[i]
		if _, present := byCanonical[csk.Spec.CanonicalName]; !present {
			byCanonical[csk.Spec.CanonicalName] = resolvedSkill{conds: csk.Status.Conditions}
		}
	}

	for _, s := range skills {
		name := s.Ref
		rs, ok := byCanonical[name]
		if !ok {
			return spiceboxv1alpha1.ReasonAgentClassSkillMissing,
				fmt.Sprintf("skill %s is not available: no Skill or ClusterSkill with that canonical name has materialized yet — check the SkillSource's Ready condition (a source that fetched but materialized nothing reports Ready=False/NoSkillsDiscovered) and its status.discoveryProblems (which names each SKILL.md that was found and rejected)", name)
		}
		c := conditions.Find(rs.conds, spiceboxv1alpha1.SkillConditionValid)
		if c == nil || c.Status != metav1.ConditionTrue {
			detail := "no Valid condition yet"
			if c != nil && c.Message != "" {
				detail = c.Message
			} else if c != nil {
				detail = fmt.Sprintf("Valid=%s reason=%s", c.Status, c.Reason)
			}
			return spiceboxv1alpha1.ReasonAgentClassSkillInvalid,
				fmt.Sprintf("skill %s is not Valid=True: %s", name, detail)
		}
	}
	return "", ""
}

// validateSkillsSpec enforces the structural (cluster-access-free) part of
// §4.4's skill-staging rules, over the AgentClassSpec alone:
//
//  1. AgentSkill.Name is unique within spec.skills — it is the local handle
//     that a toolBundle's StageSkills references, and the directory name a
//     staged skill lands under, so a collision is ambiguous on both counts.
//  2. Every non-"*" entry in a toolBundle's StageSkills names a skill that
//     actually appears in spec.skills.
//  3. Every non-"*" entry names a skill whose Target is "sandbox" or "both" —
//     an "agent"-targeted skill (the default, including the CRD-defaulted
//     zero value which arrives here as "") is never staged to disk.
//
// "*" is always valid, even when no sandbox-targeted skill exists yet: it
// expands to "every skill whose Target is sandbox or both", which is
// legitimately empty for a bundle declaring intent ahead of the skill being
// added — so an empty expansion is not an error.
//
// The fourth §4.4 rule — a staged skill's local Name must equal its SKILL.md
// frontmatter name — needs a resolved Skill/ClusterSkill CR to check against,
// which this function does not have (it never touches the cluster). That
// check belongs where staging resolution happens and is deliberately not
// implemented here.
//
// Beyond §4.4 itself (added in Task 10's fix round 1, once BuildBundleSession
// started building the sandbox MountPath directly from AgentSkill.Name — see
// ResolvedSkillBundle.LocalName): Name must also match bundleNameRE, the same
// [a-z0-9_-]{1,32} character set ToolBundle.Name is checked against elsewhere
// in this package. Nothing constrained AgentSkill.Name's charset before this;
// once a sandbox-targeted skill's Name is interpolated straight into an
// absolute container path, an unconstrained value (a "/", a "..", anything
// needing shell-quoting) is a path-escape or injection surface, not just a
// cosmetic concern — so this is checked unconditionally, for every skill
// regardless of Target, rather than trusted per AgentClass author.
func validateSkillsSpec(spec *spiceboxv1alpha1.AgentClassSpec) (reason, message string) {
	byName := make(map[string]spiceboxv1alpha1.AgentSkill, len(spec.Skills))
	for _, s := range spec.Skills {
		if !bundleNameRE.MatchString(s.Name) {
			return spiceboxv1alpha1.ReasonSpecInvalid,
				fmt.Sprintf("spec.skills: name %q must match [a-z0-9_-]{1,32} — a sandbox-targeted skill's Name becomes a literal path segment under /skills/", s.Name)
		}
		if _, dup := byName[s.Name]; dup {
			return spiceboxv1alpha1.ReasonSpecInvalid,
				fmt.Sprintf("spec.skills: duplicate name %q — AgentSkill.Name must be unique within the class", s.Name)
		}
		byName[s.Name] = s
	}

	for _, b := range spec.ToolBundles {
		for _, staged := range b.StageSkills {
			if staged == "*" {
				continue
			}
			sk, ok := byName[staged]
			if !ok {
				return spiceboxv1alpha1.ReasonSpecInvalid,
					fmt.Sprintf("toolBundles[%s].stageSkills names %q, which is not in spec.skills", b.Name, staged)
			}
			// An in-memory AgentSkill has Target == "" until the API server
			// applies the CRD's +kubebuilder:default=agent — treat empty the
			// same as the explicit "agent" default.
			target := sk.Target
			if target == "" {
				target = spiceboxv1alpha1.SkillTargetAgent
			}
			if target == spiceboxv1alpha1.SkillTargetAgent {
				return spiceboxv1alpha1.ReasonSpecInvalid,
					fmt.Sprintf("toolBundles[%s].stageSkills names %q, whose target is %q: only a skill targeted at sandbox or both can be staged to disk",
						b.Name, staged, target)
			}
		}
	}
	return "", ""
}
