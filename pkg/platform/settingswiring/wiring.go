package settingswiring

import (
	"context"
	"fmt"
	"strings"

	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/pinning"
	clipin "github.com/authzed/openagentprimitives/pkg/authz/pinning/kinds/cli"
	imagepin "github.com/authzed/openagentprimitives/pkg/authz/pinning/kinds/image"
	mcppin "github.com/authzed/openagentprimitives/pkg/authz/pinning/kinds/mcp"
	"github.com/authzed/openagentprimitives/pkg/controllers/conditions"
	"github.com/authzed/openagentprimitives/pkg/platform/settings"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// rootBudgetOf walks session.ParentRef() up to the delegation tree's root and
// returns the root's resolved budget, to be folded in by settings.Resolve as
// Inputs.RootBudget — an additional CEILING so a child can never re-grant
// itself a larger budget than the root's. This bounds each node individually,
// not the tree's total spend: N siblings can each independently spend up to
// the root's budget, so it is not a shared pool. True pooled accounting is
// follow-on work (see settings.Inputs.RootBudget). It deliberately climbs
// past the immediate parent to the true root: taking a mid-tree node's
// budget would let that node's own (already-ceilinged) grant become a fresh
// ceiling for its children, loosening the bound at every level instead of
// holding every node to the same root-derived ceiling.
//
// (nil, nil) is returned, not an error, in two legitimate cases: session has
// no parent (it IS the root, so nothing pools over it), and the root's
// EffectiveSettings has not been written yet (a transient ordering, not a
// failure — the caller falls back to no ceiling for this reconcile and picks
// it up once the root has one). Any other read failure while climbing IS
// returned: silently treating an unreadable ancestor as "no ceiling" would
// fail a cost control open.
func rootBudgetOf(ctx context.Context, r client.Reader, session *v1.AgentSession) (*v1.BudgetConfig, error) {
	if _, ok := session.ParentRef(); !ok {
		return nil, nil // session IS the root; a root does not cap itself
	}
	root, err := v1.ResolveRoot(ctx, r, session)
	if err != nil {
		return nil, fmt.Errorf("resolve delegation root for session %s/%s: %w",
			session.Namespace, session.Name, err)
	}
	if root.Status.EffectiveSettings == nil {
		return nil, nil
	}
	return &root.Status.EffectiveSettings.Budget, nil
}

// FetchTiers loads the singleton ClusterAgentSettings ("cluster") and the
// namespace's AgentSettings ("default"). A missing CR yields a nil spec (that
// tier simply imposes no ceilings/defaults) — NotFound is not an error.
func FetchTiers(ctx context.Context, r client.Reader, namespace string) (*v1.SettingsSpec, *v1.SettingsSpec, error) {
	var cluster *v1.SettingsSpec
	var cas v1.ClusterAgentSettings
	switch err := r.Get(ctx, types.NamespacedName{Name: v1.ClusterAgentSettingsName}, &cas); {
	case err == nil:
		cluster = &cas.Spec
	case apierrors.IsNotFound(err):
		// no cluster tier
	default:
		return nil, nil, err
	}

	var ns *v1.SettingsSpec
	var as v1.AgentSettings
	switch err := r.Get(ctx, types.NamespacedName{Namespace: namespace, Name: v1.AgentSettingsName}, &as); {
	case err == nil:
		ns = &as.Spec
	case apierrors.IsNotFound(err):
		// no namespace tier
	default:
		return nil, nil, err
	}
	return cluster, ns, nil
}

// ClassRefs resolves an AgentClass's referenced toolkits + MCP servers into the
// resolver's request shape: toolkit names (toolBundles → SpiceboxToolspec →
// Toolkit.Name), MCP server+tool names (mcpServers → MCPServer.Spec.Tools), and
// DeclaredPins for each MCPServer ref (frozen iff spec.pinnedManifestHash is
// set), each SpiceboxToolkit (cli kind; NotFound = embedded-catalog toolkit =
// unpinned, no error), and each SidecarToolbox (image kind; NotFound → skip).
func ClassRefs(ctx context.Context, r client.Reader, class *v1.AgentClass) ([]string, []settings.MCPRequest, []settings.DeclaredPin, error) {
	seen := map[string]bool{}
	var toolkits []string
	for _, b := range class.Spec.ToolBundles {
		for _, tsName := range b.Toolspecs {
			var ts v1.SpiceboxToolspec
			if err := r.Get(ctx, types.NamespacedName{Name: tsName}, &ts); err != nil {
				if apierrors.IsNotFound(err) {
					continue // class validity already gates on this; skip here
				}
				return nil, nil, nil, err
			}
			tk := ts.Spec.Toolkit.Name
			if tk != "" && !seen[tk] {
				seen[tk] = true
				toolkits = append(toolkits, tk)
			}
		}
	}

	var mcp []settings.MCPRequest
	var classPins []settings.DeclaredPin
	for _, ref := range class.Spec.MCPServers {
		var srv v1.MCPServer
		if err := r.Get(ctx, types.NamespacedName{Namespace: class.Namespace, Name: ref.Ref}, &srv); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return nil, nil, nil, err
		}
		var tools []string
		for _, t := range srv.Spec.Tools {
			tools = append(tools, t.Name)
		}
		mcp = append(mcp, settings.MCPRequest{Server: ref.Ref, Tools: tools})

		// Build the DeclaredPin: frozen iff spec.pinnedManifestHash is asserted.
		strength := string(pinning.StrengthUnpinned)
		if srv.Spec.PinnedManifestHash != "" {
			strength = string(pinning.StrengthFrozen)
		}
		classPins = append(classPins, settings.DeclaredPin{
			Kind:     mcppin.KindName,
			Name:     ref.Ref,
			Strength: strength,
		})
	}

	// cli pins: for each gathered toolkit name, fetch the cluster-scoped
	// SpiceboxToolkit CR. NotFound means it is an embedded-catalog toolkit
	// with no CR — classify as unpinned, do not error.
	for _, tkName := range toolkits {
		var stk v1.SpiceboxToolkit
		strength := string(pinning.StrengthUnpinned)
		if err := r.Get(ctx, types.NamespacedName{Name: tkName}, &stk); err != nil {
			if !apierrors.IsNotFound(err) {
				return nil, nil, nil, err
			}
			// Not found = embedded-catalog toolkit; treat as unpinned.
		} else {
			strength = string(clipin.StrengthFor(stk.Spec.Target.PinnedBinaryHash, stk.Spec.Target.VersionRange))
		}
		classPins = append(classPins, settings.DeclaredPin{
			Kind:     clipin.KindName,
			Name:     tkName,
			Strength: strength,
		})
	}

	// image pins: for each SidecarToolbox ref, fetch the namespaced CR and
	// classify the source ref via the image kind. NotFound → continue (same
	// as the MCPServer path above).
	for _, ref := range class.Spec.SidecarToolboxes {
		var tb v1.SidecarToolbox
		if err := r.Get(ctx, types.NamespacedName{Namespace: class.Namespace, Name: ref.Ref}, &tb); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return nil, nil, nil, err
		}
		// Determine the source ref: Image or Inline.BaseImage.
		sourceRef := tb.Spec.Source.Image
		if sourceRef == "" && tb.Spec.Source.Inline != nil {
			sourceRef = tb.Spec.Source.Inline.BaseImage
		}
		strength := string(pinning.StrengthUnpinned)
		if sourceRef != "" {
			// parse failure → unpinned, which trips strength floors — fail closed.
			if parsed, err := (&imagepin.Kind{}).ParseRef(sourceRef); err == nil {
				strength = string(parsed.Strength)
			}
		}
		classPins = append(classPins, settings.DeclaredPin{
			Kind:     imagepin.KindName,
			Name:     ref.Ref,
			Strength: strength,
		})
	}

	return toolkits, mcp, classPins, nil
}

// buildBundleSandboxInputs reads the two lowest sandbox tiers for every bundle:
// the AgentClass's own per-bundle override, and the referenced SpiceboxClass's
// declaration. This is where the I/O lives, so settings.Resolve can stay pure.
//
// A referenced class that does not exist (NotFound specifically) contributes no
// tier-4 preference. That is not silent: the AgentClass controller already
// reports a missing class, and raising it again here would put two conditions on
// one fault. Any OTHER error (Forbidden, timeout, apiserver 5xx) is returned to
// the caller rather than folded into "no preference", which would silently drop
// the SpiceboxClass's sandbox declaration.
func buildBundleSandboxInputs(
	ctx context.Context,
	r client.Reader,
	bundles []v1.ToolBundle,
) (map[string]settings.BundleSandboxInputs, error) {
	if len(bundles) == 0 {
		return nil, nil
	}
	out := make(map[string]settings.BundleSandboxInputs, len(bundles))
	// Cache: several bundles commonly share one class. Only a resolved result is
	// cached (found-with-preference, found-without, or NotFound); an error is
	// never cached, so it is retried per bundle and surfaced rather than reused
	// as a false "no preference" for later bundles.
	seen := map[string]*v1.SandboxBackend{}

	for _, b := range bundles {
		in := settings.BundleSandboxInputs{FromAgentClass: b.Sandbox}

		if cached, ok := seen[b.Class]; ok {
			in.FromSpiceboxClass = cached
		} else {
			var cls v1.SpiceboxClass
			switch err := r.Get(ctx, types.NamespacedName{Name: b.Class}, &cls); {
			case err == nil:
				// Kind/Config only. WarmPool is deliberately NOT a tier-4
				// preference: the per-bundle fold does not resolve it at all
				// (see foldSandboxTiers in pkg/platform/settings/sandbox.go).
				// A class setting ONLY warmPool therefore expresses no
				// preference this fold can act on; including it would report a
				// value as effective that this path never honors.
				if cls.Spec.Sandbox.Kind != "" || cls.Spec.Sandbox.Config != nil {
					sb := cls.Spec.Sandbox
					in.FromSpiceboxClass = &sb
				}
			case apierrors.IsNotFound(err):
				// no tier-4 preference; see doc comment above
			default:
				return nil, err
			}
			seen[b.Class] = in.FromSpiceboxClass
		}
		out[b.Name] = in
	}
	return out, nil
}

// classSkillRefs extracts the canonical Ref from each opted-in AgentSkill, in
// order. The tiered AllowedSkills/DeniedSkills ceilings (settings.Inputs.
// ClassSkills) match against the canonical name, never the class-local Name
// handle, so this is the only field settings.Resolve is allowed to see.
func classSkillRefs(skills []v1.AgentSkill) []string {
	if len(skills) == 0 {
		return nil
	}
	refs := make([]string, len(skills))
	for i, s := range skills {
		refs[i] = s.Ref
	}
	return refs
}

// ResolveForClass resolves the class-only chain (cluster → namespace → class).
func ResolveForClass(ctx context.Context, r client.Reader, class *v1.AgentClass) (v1.EffectiveSettings, []settings.Violation, error) {
	cluster, ns, err := FetchTiers(ctx, r, class.Namespace)
	if err != nil {
		return v1.EffectiveSettings{}, nil, err
	}
	toolkits, mcp, classPins, err := ClassRefs(ctx, r, class)
	if err != nil {
		return v1.EffectiveSettings{}, nil, err
	}
	in := settings.Inputs{
		Cluster: cluster, Namespace: ns,
		ClassModel: class.Spec.Model, ClassBudget: class.Spec.Budget, ClassAuthz: class.Spec.Authz,
		ClassToolkits: toolkits, ClassMCP: mcp,
		ClassSkills: classSkillRefs(class.Spec.Skills),
		ClassPins:   classPins,
		ForSession:  false,
	}
	bundleSandbox, err := buildBundleSandboxInputs(ctx, r, class.Spec.ToolBundles)
	if err != nil {
		return v1.EffectiveSettings{}, nil, err
	}
	in.BundleSandbox = bundleSandbox
	eff, vs := settings.Resolve(in)
	return eff.ToStatus(), vs, nil
}

// ResolveForSession resolves the full chain including the session budget, for an
// about-to-run session (ForSession=true makes a missing model fatal).
func ResolveForSession(ctx context.Context, r client.Reader, class *v1.AgentClass, session *v1.AgentSession) (v1.EffectiveSettings, []settings.Violation, error) {
	cluster, ns, err := FetchTiers(ctx, r, class.Namespace)
	if err != nil {
		return v1.EffectiveSettings{}, nil, err
	}
	toolkits, mcp, classPins, err := ClassRefs(ctx, r, class)
	if err != nil {
		return v1.EffectiveSettings{}, nil, err
	}
	// A delegated child spends the ROOT's pool, not its own — see rootBudgetOf
	// for why the walk climbs past the immediate parent. Nil for a root
	// session (no parent), which leaves settings.Resolve's fold unaffected.
	rootBudget, err := rootBudgetOf(ctx, r, session)
	if err != nil {
		return v1.EffectiveSettings{}, nil, fmt.Errorf("resolve root session budget: %w", err)
	}
	in := settings.Inputs{
		Cluster: cluster, Namespace: ns,
		ClassModel: class.Spec.Model, ClassBudget: class.Spec.Budget, ClassAuthz: class.Spec.Authz,
		ClassToolkits: toolkits, ClassMCP: mcp,
		ClassSkills:   classSkillRefs(class.Spec.Skills),
		ClassPins:     classPins,
		SessionBudget: session.Spec.Budget,
		RootBudget:    rootBudget,
		ForSession:    true,
	}
	bundleSandbox, err := buildBundleSandboxInputs(ctx, r, class.Spec.ToolBundles)
	if err != nil {
		return v1.EffectiveSettings{}, nil, err
	}
	in.BundleSandbox = bundleSandbox
	eff, vs := settings.Resolve(in)
	return eff.ToStatus(), vs, nil
}

// HasFatal reports whether any violation is fatal.
func HasFatal(vs []settings.Violation) bool { return FirstFatal(vs) != nil }

// FirstFatal returns the first fatal violation, or nil.
func FirstFatal(vs []settings.Violation) *settings.Violation {
	for i := range vs {
		if vs[i].Fatal {
			return &vs[i]
		}
	}
	return nil
}

// StampAccepted sets the SettingsAccepted condition on conds from the violations:
// fatal → False with the offending reason; non-fatal warnings → True/Clamped;
// clean → True/Resolved. Returns true if there is a fatal violation.
func StampAccepted(obj conditions.Generationer, conds *[]metav1.Condition, condType string, vs []settings.Violation) bool {
	if f := FirstFatal(vs); f != nil {
		conditions.SetFalse(obj, conds, condType, f.Reason, joinMessages(vs, true))
		return true
	}
	if len(vs) > 0 {
		conditions.Set(obj, conds, metav1.Condition{
			Type: condType, Status: metav1.ConditionTrue,
			Reason: v1.ReasonSettingsClamped, Message: joinMessages(vs, false),
		})
		return false
	}
	conditions.SetTrue(obj, conds, condType, v1.ReasonSettingsResolved)
	return false
}

func joinMessages(vs []settings.Violation, fatalOnly bool) string {
	var ms []string
	for _, v := range vs {
		if fatalOnly && !v.Fatal {
			continue
		}
		ms = append(ms, v.Message)
	}
	return strings.Join(ms, "; ")
}
