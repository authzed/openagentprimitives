package agentclass

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz"
	embeddedtoolkits "github.com/authzed/openagentprimitives/toolkits"
)

// toolkitsForKeying resolves every toolkit whose checks will ACTUALLY run for
// this class — CR-authored or embedded.
//
// Deliberately NOT listToolkitsFor. That one is scoped to CR-authored toolkits
// on purpose: it feeds validation, and failing a class over a spec no operator
// can inspect with kubectl would be unhelpful. Keying has the opposite
// requirement. The chain published here is the one every writer of a slot grant
// must reproduce, and at call time the embedded git/gh toolkit is what runs
// whether or not anyone authored a CR for it.
//
// Reusing the validation scope was a real regression, found on a live cluster
// rather than in a test: a cluster has no SpiceboxToolkit CRs at all, so the
// walk saw nothing and `status.resolvedSlots` published a git_repo slot with no
// valueTransforms. An empty chain does not read as "unknown" downstream —
// threadseed and the approval backfill read it as "the raw value IS the object
// id" — which is precisely the silent drift this derivation exists to prevent.
//
// A CR-authored toolkit takes precedence: authoring one is an override, and
// preferring the embedded copy would make that override do nothing.
func toolkitsForKeying(
	ctx context.Context,
	c client.Reader,
	ac *spiceboxv1alpha1.AgentClass,
) ([]spiceboxv1alpha1.SpiceboxToolkit, error) {
	if len(ac.Spec.ToolBundles) == 0 {
		return nil, nil
	}
	seen := map[string]struct{}{}
	var out []spiceboxv1alpha1.SpiceboxToolkit
	for _, b := range ac.Spec.ToolBundles {
		for _, tsName := range b.Toolspecs {
			var ts spiceboxv1alpha1.SpiceboxToolspec
			if err := c.Get(ctx, types.NamespacedName{Name: tsName}, &ts); err != nil {
				// A missing toolspec is already reported by the slice-1
				// validity check; tolerated here to avoid coupling the paths.
				continue
			}
			tkName := ts.Spec.Toolkit.Name
			if tkName == "" {
				continue
			}
			if _, dup := seen[tkName]; dup {
				continue
			}

			var tk spiceboxv1alpha1.SpiceboxToolkit
			if err := c.Get(ctx, types.NamespacedName{Name: tkName}, &tk); err == nil {
				seen[tkName] = struct{}{}
				out = append(out, tk)
				continue
			}

			emb, found, err := embeddedToolkitAsCR(tkName)
			if err != nil {
				// A shipped toolkit that cannot be converted is a build-time
				// defect, not a cluster condition. Returning it marks the class
				// not-ready rather than publishing a chain derived from a
				// partial view of the tools.
				return nil, fmt.Errorf("embedded toolkit %q: %w", tkName, err)
			}
			if !found {
				continue
			}
			seen[tkName] = struct{}{}
			out = append(out, emb)
		}
	}
	return out, nil
}

// embeddedToolkitAsCR converts an embedded toolkit into the CR shape the
// keying walk consumes. The spec is byte-compatible with the library type by
// design (SpiceboxToolkitSpec.ToToolkit round-trips the other way).
func embeddedToolkitAsCR(name string) (spiceboxv1alpha1.SpiceboxToolkit, bool, error) {
	for _, lib := range embeddedtoolkits.All() {
		if lib.Name != name {
			continue
		}
		data, err := json.Marshal(lib)
		if err != nil {
			return spiceboxv1alpha1.SpiceboxToolkit{}, false, fmt.Errorf("marshal: %w", err)
		}
		var tk spiceboxv1alpha1.SpiceboxToolkit
		if err := json.Unmarshal(data, &tk.Spec); err != nil {
			return spiceboxv1alpha1.SpiceboxToolkit{}, false, fmt.Errorf("unmarshal: %w", err)
		}
		tk.Name = lib.Name
		return tk, true, nil
	}
	return spiceboxv1alpha1.SpiceboxToolkit{}, false, nil
}

// valueKeying is how a free-form value becomes the SpiceDB object id for one
// slot type: the transform chain the tool applies at Check time.
type valueKeying struct {
	transforms []string
	// declaredBy is the tool that established the chain, for the error message
	// when a second tool disagrees.
	declaredBy string
}

// resolveSlotValueKeying derives, per slot resourceType, the value→id transform
// chain that any writer of a grant must reproduce.
//
// WHY THIS IS DERIVED RATHER THAN DECLARED. A slot grant names an object id. For
// a value slot that id is minted from the value by the TOOL's own
// PermissionCheck (resourceIDExpr + resourceIDTransforms), so anything that
// writes a grant ahead of the call — thread seeding, an approval, a default —
// has to mint the identical id. Asking the slot author to restate the chain
// would create a second copy that drifts, and the failure of drift is silent:
// the grant is written, never matched, and nothing errors. Deriving it from the
// tools keeps one source of truth.
//
// TWO TOOLS THAT DISAGREE IS A REJECTION, not a pick-one. If curl keys an
// http_target as sha256(normalize_url(url)) and some other tool keys it as
// sha256(url) alone, then the same URL is two different resources and an
// approval for one authorizes neither reliably — which reads to a human as
// "approval randomly does not work". There is no safe way to choose between
// them, so the class does not become runnable until the author settles it.
//
// Only the EXPR branch participates. A template-keyed check derives its id from
// a named argument that is already a distinct resource, so there is no
// value→id mapping for a seeder to reproduce.
//
// Also publishes, per slot, the Standing resolved by resolveStandingFor —
// requireStandingFor is the cluster/namespace admin veto, unioned upstream
// in EffectiveSettings.RequireStandingFor.
func resolveSlotValueKeying(
	ac *spiceboxv1alpha1.AgentClass,
	mcpServers []spiceboxv1alpha1.MCPServer,
	toolkits []spiceboxv1alpha1.SpiceboxToolkit,
	sidecarToolboxes []spiceboxv1alpha1.SidecarToolbox,
	requireStandingFor []string,
) ([]spiceboxv1alpha1.ResolvedSlot, string, string) {
	slots := ac.Spec.GetSlots()
	if len(slots) == 0 {
		return nil, "", ""
	}

	veto := make(map[string]struct{}, len(requireStandingFor))
	for _, t := range requireStandingFor {
		veto[t] = struct{}{}
	}

	// Collect every expr-keyed chain the tools declare, per resource type.
	//
	// Template-keyed checks are deliberately NOT collected — see
	// TestResolveSlotValueKeying_NonValueSlotsPublishNoChain. Note the open
	// question that leaves: a template's transforms ARE applied at check time
	// (ResolveTemplate runs the chain), so a slot declared "Demo-Org/Demo-Repo"
	// mints a grant on that value while a {repo}+lowercase check computes
	// "demo-org/demo-repo", and the grant never matches. Tightening this was
	// tried and backed out: it changes which id every template-keyed slot's
	// grant is minted with, which is a decision to make deliberately rather
	// than as a side effect. TestBuiltinToolkits_KeyEachResourceTypeConsistently
	// guards the shipped toolkits against the divergence meanwhile.
	keying := map[string]valueKeying{}
	for _, src := range keyingSources(mcpServers, toolkits, sidecarToolboxes) {
		for _, chk := range src.checks {
			if chk.ResourceIDExpr == "" || chk.ResourceType == "" {
				continue
			}
			prev, seen := keying[chk.ResourceType]
			if !seen {
				keying[chk.ResourceType] = valueKeying{
					transforms: append([]string(nil), chk.ResourceIDTransforms...),
					declaredBy: src.ref,
				}
				continue
			}
			if !sameChain(prev.transforms, chk.ResourceIDTransforms) {
				return nil, spiceboxv1alpha1.ReasonSlotDeclarationInvalid,
					fmt.Sprintf("authz.slots[%s]: two tools key the same value differently — %s uses [%s] "+
						"and %s uses [%s]. One value would become two object ids, so a grant written for "+
						"one call would not match the other. Make the resourceIDTransforms identical.",
						chk.ResourceType, prev.declaredBy, strings.Join(prev.transforms, ", "),
						src.ref, strings.Join(chk.ResourceIDTransforms, ", "))
			}
		}
	}

	out := make([]spiceboxv1alpha1.ResolvedSlot, 0, len(slots))
	for _, s := range slots {
		standing, approverPerm, err := resolveStandingFor(s.ResourceType, mcpServers, toolkits, sidecarToolboxes, veto)
		if err != nil {
			// Refused, not defaulted: an undeclared or unsatisfiable standing
			// decides who may approve this type, and guessing either way is
			// silent. Surfaced on the class so the author sees the type named.
			return nil, spiceboxv1alpha1.ReasonSlotDeclarationInvalid,
				fmt.Sprintf("authz.slots[%s]: %v", s.ResourceType, err)
		}

		// A triggerInstance that fails to compile would otherwise surface only
		// at delivery time — per webhook, fail-closed, and silent (see
		// AuthzSlot.TriggerInstance's doc comment) — rather than once, here, at
		// admission. Refused the same way an unresolvable standing is refused
		// above: same reason, same "authz.slots[%s]: …" message shape.
		if s.TriggerInstance != "" {
			if _, cerr := authz.CompileTriggerInstanceExpr(s.TriggerInstance); cerr != nil {
				return nil, spiceboxv1alpha1.ReasonSlotDeclarationInvalid,
					fmt.Sprintf("authz.slots[%s]: triggerInstance: %v", s.ResourceType, cerr)
			}
		}

		rs := spiceboxv1alpha1.ResolvedSlot{
			ResourceType:       s.ResourceType,
			Permission:         s.Permission,
			Standing:           standing,
			ApproverPermission: approverPerm,
		}
		if k, ok := keying[s.ResourceType]; ok {
			rs.ValueTransforms = k.transforms
			// A value slot with no injective terminal would put the raw value
			// into the object id. validatePermissionShape already refuses that
			// on the tool, so reaching it here means the tool was admitted
			// before the rule existed; say so rather than publish a mapping a
			// seeder would faithfully reproduce.
			if bad := authz.UnsafeSlotTransforms(k.transforms); len(bad) > 0 {
				return nil, spiceboxv1alpha1.ReasonSlotDeclarationInvalid,
					fmt.Sprintf("authz.slots[%s]: the tool keying this slot uses %v, which may map two "+
						"different values onto one object id", s.ResourceType, bad)
			}
		}
		out = append(out, rs)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ResourceType < out[j].ResourceType })
	return out, "", ""
}

// keyingSource is one declarer of permission checks, named for an error
// message. An MCP tool and a toolkit subcommand are the same thing here: both
// mint the object id a slot grant has to match.
type keyingSource struct {
	ref    string
	checks []authz.PermissionCheck
}

// keyingSources flattens every tool source into one list.
//
// Walking only mcpServers is what this replaced, and the omission was invisible
// rather than partial: a toolkit-keyed slot published NO chain, and an empty
// chain does not read as "unknown" downstream — threadseed and the approval
// backfill read it as "the raw value is already the object id". So a git_repo
// grant was seeded on `https://github.com/o/n` while `git clone` Checked
// `spicedb_object_id(normalize_url(…))`: written, never matched, no error. That
// is the exact silent drift this derivation exists to prevent, reintroduced by
// the set of sources it looked at rather than by the rule it applied.
//
// SidecarToolboxes are in the list for that same reason, and the case is even
// plainer: SidecarToolboxSpec.Tools IS []MCPServerTool, so a sidecar tool
// carries the identical check and mints the identical object id. Which CR kind
// a tool arrived in has never been a reason to publish a different chain for
// it. Mirrors standingSources in slot_standing.go, which walks the same three
// kinds for their SCHEMA fragments.
//
// What PUBLISHING a chain (rather than nothing) turns on, so the cost of
// widening this list is on the page beside the reason to:
//
//   - A cross-kind disagreement becomes fatal. Two tools keying one type
//     differently is refused above, so a sidecar tool that disagrees with an
//     MCPServer tool makes a previously-Valid class Invalid. That is the
//     fail-closed direction — the two ids never matched at runtime either, and
//     the class merely stopped saying so — but it is a state change an operator
//     sees at upgrade, not only at authoring.
//   - An expr-keyed sidecar slot becomes thread-seedable. SeedFromThread skips
//     any slot whose chain is ValueKindUnknown (pkg/authz/threadseed.go), and an
//     unpublished chain is exactly that, so such a slot could never be bound
//     from what a person had already typed. With the chain published it can be.
//     The workshop draft is unaffected: its id is a constant, so it declares no
//     chain at all and there is no value in any thread to find.
func keyingSources(
	mcpServers []spiceboxv1alpha1.MCPServer,
	toolkits []spiceboxv1alpha1.SpiceboxToolkit,
	sidecarToolboxes []spiceboxv1alpha1.SidecarToolbox,
) []keyingSource {
	var out []keyingSource
	for _, srv := range mcpServers {
		for _, tool := range srv.Spec.Tools {
			out = append(out, keyingSource{
				ref:    fmt.Sprintf("MCPServer/%s tool/%s", srv.Name, tool.Name),
				checks: checksFrom(tool.Permission, tool.PermissionVariants),
			})
		}
	}
	for _, tk := range toolkits {
		for _, sub := range tk.Spec.Subcommands {
			out = append(out, keyingSource{
				ref:    fmt.Sprintf("SpiceboxToolkit/%s %s", tk.Name, strings.Join(sub.Path, " ")),
				checks: checksFrom(sub.Permission, sub.PermissionVariants),
			})
		}
	}
	for _, sc := range sidecarToolboxes {
		for _, tool := range sc.Spec.Tools {
			out = append(out, keyingSource{
				ref:    fmt.Sprintf("SidecarToolbox/%s tool/%s", sc.Name, tool.Name),
				checks: checksFrom(tool.Permission, tool.PermissionVariants),
			})
		}
	}
	return out
}

// checksFrom returns a declarer's own check plus every variant's. Variants are
// included deliberately: a variant can key the same resource type differently
// from the base, and validatePermissions has historically walked only the base
// permission — so a divergent variant is exactly the kind that slips through
// unnoticed.
func checksFrom(base *authz.Permission, variants []authz.PermissionVariant) []authz.PermissionCheck {
	var out []authz.PermissionCheck
	if base != nil && base.Check != nil {
		out = append(out, *base.Check)
	}
	for _, v := range variants {
		if v.Check.Check != nil {
			out = append(out, *v.Check.Check)
		}
	}
	return out
}

func sameChain(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
