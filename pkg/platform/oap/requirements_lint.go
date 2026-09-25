package oap

import (
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/authzed/openagentprimitives/pkg/agent/agentcaps"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/tools/skills/canonical"
)

// RequirementFinding is one problem found by a bundle-wide authoring
// requirement — a check that applies to every AgentClass a bundle carries,
// not just one AgentUI (contrast Finding in agentui_lint.go, which is scoped
// to the AgentUI/MCPServer/AgentClass triple). Same three-fact shape (name the
// offending CR, name the field, say why) so a caller renders both the same
// way.
type RequirementFinding struct {
	AgentClass string // the offending AgentClass's metadata.name
	Path       string // the offending spec field, e.g. "spec.skills[0]"
	Reason     string

	// Resource names the offending object when it is NOT one of the bundle's
	// AgentClasses — "AgentIdentity/demo-agent-id" for a finding about a
	// credential, "oap.yaml" for one about the manifest itself. Set it INSTEAD
	// of AgentClass, never as well: String() prefers it, so filling in both
	// would render one and silently drop the other.
	//
	// It exists because a bundle-wide requirement is not necessarily an
	// AgentClass's (channelplan.LintRequiredChannels reports against the
	// manifest's requires.channels and against the AgentIdentity holding a
	// dangling secretRef), and an empty AgentClass rendered as the fixed
	// "AgentClass/" prefix names an object that does not exist.
	Resource string
}

func (f RequirementFinding) String() string {
	subject := f.Resource
	if subject == "" {
		subject = "AgentClass/" + f.AgentClass
	}
	return fmt.Sprintf("%s %s: %s", subject, f.Path, f.Reason)
}

// LintSkillPinning flags every entry in a bundled AgentClass's spec.skills
// that is not pinned to a ref. spec.skills opts in by canonical name
// ("<repo-locator>//<subpath>[@<ref>]" — see pkg/tools/skills/canonical); an
// entry with no "@ref" is a ROLLING reference, so the methodology a session
// runs against can change under a rerun of the exact same commit — silently,
// since nothing about the session record would show it. This dispatches
// through canonical.Parse/PinStrength, the SAME parser
// pkg/controllers/internal/skillpin.Baseline uses to classify a Skill's own
// pin at reconcile time, rather than a hand-rolled "@" substring check that
// could drift from what "pinned" actually means there.
//
// A canonical name that fails to parse at all is ALSO a finding (with a
// different Reason) rather than a skipped entry — an author who typos the
// "//" separator gets told why, instead of the check going quiet.
//
// A nil bundle or an undecodable manifest stream is a returned error, never a
// skip that reads as clean.
func LintSkillPinning(b *Bundle) ([]RequirementFinding, error) {
	if b == nil {
		return nil, fmt.Errorf("oap: LintSkillPinning: nil bundle")
	}
	crs, err := b.CRs()
	if err != nil {
		return nil, fmt.Errorf("oap: LintSkillPinning: decode bundled manifests: %w", err)
	}

	var findings []RequirementFinding
	for _, cr := range crs {
		if cr.GetKind() != "AgentClass" {
			continue
		}
		var ac spiceboxv1alpha1.AgentClass
		if err := DecodeBundledCR(cr, &ac); err != nil {
			return nil, fmt.Errorf("oap: LintSkillPinning: AgentClass %q: %w", cr.GetName(), err)
		}
		for i, s := range ac.Spec.Skills {
			path := fmt.Sprintf("spec.skills[%d]", i)
			name, err := canonical.Parse(s.Ref)
			if err != nil {
				findings = append(findings, RequirementFinding{
					AgentClass: ac.Name, Path: path,
					Reason: fmt.Sprintf("%q is not a valid canonical skill name: %s", s.Ref, err),
				})
				continue
			}
			if name.PinStrength() == canonical.PinUnpinned {
				findings = append(findings, RequirementFinding{
					AgentClass: ac.Name, Path: path,
					Reason: fmt.Sprintf(
						"%q has no @ref — an unpinned skill is a rolling reference; the methodology it names can change under a rerun of the exact same commit. Pin it to a tag or commit SHA.", s.Ref),
				})
			}
		}
	}
	return findings, nil
}

// LintSkillsShape flags every bundled AgentClass whose spec.skills still
// carries the shape it had before AgentSkill existed: a bare canonical-name
// string, rather than the {name, ref, target} object. Detection happens HERE,
// leniently against the raw unstructured YAML, rather than by decoding
// straight into the typed AgentClass the way LintSkillPinning and
// LintArtifactsCapability do — DecodeBundledCR against the old shape fails
// with a raw "cannot unmarshal string into Go struct field" error that names
// no fix, which is exactly the failure this check exists to pre-empt.
//
// This is not merely an authoring nit: a bundle carrying the old shape must
// never reach an actual apply. Verified against the generated CRD schema
// (config/crds/agentprimitives.authzed.com_agentclasses.yaml): structural-schema
// pruning leaves a scalar string sitting where an object is expected
// untouched (pruning only strips unrecognized keys FROM an object, it does not
// coerce a type mismatch), so the apiserver's own schema validation is what
// catches it — with the opaque, fix-free "spec.skills[0] in body must be of
// type object: \"string\"". cmd/oap/internal/agentcmd/install.go calls this
// before install.Preflight so the operator sees the rewrite instead of that.
//
// Only the first offending entry per AgentClass is reported: a skills list
// either was, or wasn't, migrated, so enumerating every remaining bare
// string in the same list adds noise, not information, once the rewrite has
// been shown once.
//
// A nil bundle or an undecodable manifest stream is a returned error, never a
// skip that reads as clean.
func LintSkillsShape(b *Bundle) ([]RequirementFinding, error) {
	if b == nil {
		return nil, fmt.Errorf("oap: LintSkillsShape: nil bundle")
	}
	crs, err := b.CRs()
	if err != nil {
		return nil, fmt.Errorf("oap: LintSkillsShape: decode bundled manifests: %w", err)
	}

	var findings []RequirementFinding
	for _, cr := range crs {
		if cr.GetKind() != "AgentClass" {
			continue
		}
		raw, found, err := unstructured.NestedSlice(cr.Object, "spec", "skills")
		if err != nil {
			return nil, fmt.Errorf("oap: LintSkillsShape: AgentClass %q: spec.skills: %w", cr.GetName(), err)
		}
		if !found {
			continue
		}
		for i, entry := range raw {
			old, isString := entry.(string)
			if !isString {
				// Already an object — either the current {name, ref, target}
				// shape, or some other malformed shape a different check (or the
				// eventual decode) will catch. Not this lint's concern.
				continue
			}
			findings = append(findings, RequirementFinding{
				AgentClass: cr.GetName(),
				Path:       fmt.Sprintf("spec.skills[%d]", i),
				Reason:     oldSkillsShapeRewrite(old),
			})
			break
		}
	}
	return findings, nil
}

// oldSkillsShapeRewrite renders the fix for one pre-migration bare-string
// skills entry: the exact {name, ref, target} object it must become. The
// suggested name is derived from the ref's last path segment (via
// canonical.Parse + Name.LastSegment) when it parses as a canonical skill
// name — flagged as a suggestion, not prescribed, because the local handle is
// the operator's to choose, not this lint's.
func oldSkillsShapeRewrite(old string) string {
	nameHint := "<pick-a-local-handle>"
	nameComment := "unique within this class"
	if suggested := suggestSkillName(old); suggested != "" {
		nameHint = suggested
		nameComment = "suggested from the ref's last path segment — pick any unique local handle"
	}
	return fmt.Sprintf(
		"%q is the pre-migration bare-string skills entry; spec.skills is now a list of {name, ref, target} objects. Rewrite it to:\n"+
			"    - name: %s  # %s\n"+
			"      ref: %s\n"+
			"      target: agent  # agent (default) | sandbox | both",
		old, nameHint, nameComment, old)
}

// suggestSkillName derives a candidate local handle from a bare skill
// reference string, via the same canonical.Name.LastSegment() accessor
// pkg/tools/skills/validate uses to check a sandbox-targeted skill's frontmatter
// name against its directory. Returns "" when the string doesn't even parse as
// a canonical name, so the caller can fall back to a placeholder instead of
// proposing garbage.
func suggestSkillName(ref string) string {
	n, err := canonical.Parse(ref)
	if err != nil {
		return ""
	}
	return n.LastSegment()
}

// LintArtifactsCapability flags every bundled AgentClass whose system prompt
// calls artifact_prepare without also declaring the "artifacts" capability.
// artifacts is opt-in and default-off (see
// pkg/agent/tool/meta/capability/artifacts.go) — without it, artifact_prepare
// (and its await/history/offer-view siblings) is never injected into the
// agent's tool set, so a prompt that tells the agent to call it is
// instructing the agent to call a tool that will never exist. The failure is
// silent in the worst way: nothing errors, the agent just improvises around
// the missing tool and may still respond, so the defect only shows up as a
// missing artifact nobody asked about.
//
// The prompt text is read from spec.systemPrompt.inline directly, or, when
// the class instead points at spec.systemPrompt.configMapRef, from the
// matching bundled ConfigMap's data key (ConfigMap is an allowed bundle
// kind — see allowedBundleKinds). When neither resolves to visible text —
// inline is empty AND the referenced ConfigMap is not itself bundled — this
// makes no claim about that class: it cannot see the prompt, so it is not
// evidence of anything, positive or negative.
//
// A nil bundle or an undecodable manifest stream is a returned error, never a
// skip that reads as clean.
func LintArtifactsCapability(b *Bundle) ([]RequirementFinding, error) {
	if b == nil {
		return nil, fmt.Errorf("oap: LintArtifactsCapability: nil bundle")
	}
	crs, err := b.CRs()
	if err != nil {
		return nil, fmt.Errorf("oap: LintArtifactsCapability: decode bundled manifests: %w", err)
	}

	var classes []*spiceboxv1alpha1.AgentClass
	configMaps := map[string]*unstructured.Unstructured{}
	for _, cr := range crs {
		switch cr.GetKind() {
		case "AgentClass":
			var ac spiceboxv1alpha1.AgentClass
			if err := DecodeBundledCR(cr, &ac); err != nil {
				return nil, fmt.Errorf("oap: LintArtifactsCapability: AgentClass %q: %w", cr.GetName(), err)
			}
			classes = append(classes, &ac)
		case "ConfigMap":
			configMaps[cr.GetName()] = cr
		}
	}

	var findings []RequirementFinding
	for _, ac := range classes {
		prompt, ok := resolveSystemPromptText(ac, configMaps)
		if !ok || !strings.Contains(prompt, "artifact_prepare") {
			continue
		}
		// Use the same activation semantics the runtime uses (agentcaps.Active),
		// not mere key presence: artifacts is opt-in/default-off, so
		// {artifacts: {enabled: false}} is present in the map but inactive —
		// key-presence alone would lint that combination clean while
		// artifact_prepare is still never injected.
		grant, err := agentcaps.GrantOf(ac, "artifacts")
		if err != nil {
			return nil, fmt.Errorf("oap: LintArtifactsCapability: AgentClass %q: capabilities.artifacts: %w", ac.Name, err)
		}
		if agentcaps.Active(false, grant) {
			continue
		}
		findings = append(findings, RequirementFinding{
			AgentClass: ac.Name, Path: "spec.capabilities",
			Reason: `system prompt calls artifact_prepare, but spec.capabilities does not include "artifacts" (opt-in, default-off) — the meta tool is never injected and the agent silently cannot produce its artifact`,
		})
	}
	return findings, nil
}

// resolveSystemPromptText returns an AgentClass's system prompt text and
// whether it could be resolved at all from what the bundle carries. Inline
// wins when both are set (PromptSource "carries exactly one of inline or
// configMapRef", but a lint tolerates the CRD's own comment being violated by
// preferring the field the runtime would also read first).
func resolveSystemPromptText(ac *spiceboxv1alpha1.AgentClass, configMaps map[string]*unstructured.Unstructured) (string, bool) {
	if ac.Spec.SystemPrompt.Inline != "" {
		return ac.Spec.SystemPrompt.Inline, true
	}
	ref := ac.Spec.SystemPrompt.ConfigMapRef
	if ref == nil {
		return "", false
	}
	cm, present := configMaps[ref.Name]
	if !present {
		return "", false
	}
	text, found, err := unstructured.NestedString(cm.Object, "data", ref.Key)
	if err != nil || !found {
		return "", false
	}
	return text, true
}
