package agentclass

import (
	"fmt"
	"sort"
	"strings"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/authz/precondition"
)

// Valid values for the slot declaration enums. These are duplicated from the
// kubebuilder markers on AuthzSlot deliberately: the CRD rejects a bad value at
// admission, and this rejects one that reached the reconciler anyway — an object
// written before the enum existed, or through a client that bypassed validation.
// A slot whose fill policy cannot be read must not be treated as "no policy".
//
// validFillFrom is no longer hand-typed: it is BUILT from
// authz.FillFromVocabulary(), the one Go-side list — see that function's doc
// comment. validAutoGrantFrom and validMembership stay hand-typed (their
// vocabularies are small, closed, and each has only this one consumer), but
// TestSlotEnumMirrors_MatchTheGeneratedCRD pins all three against the
// generated CRD's own enums, so a future desync on any of them fails a test
// instead of silently refusing every reconcile.
var (
	validFillFrom      = fillFromSet()
	validAutoGrantFrom = map[string]struct{}{"owner": {}, "participants": {}, "none": {}}
	validMembership    = map[string]struct{}{"dynamic": {}, "frozen": {}}
)

// fillFromSet builds validFillFrom's map shape from authz.FillFromVocabulary().
func fillFromSet() map[string]struct{} {
	out := make(map[string]struct{})
	for _, v := range authz.FillFromVocabulary() {
		out[v] = struct{}{}
	}
	return out
}

// fillFromChannelThread is the one source AutoGrantFrom governs.
const fillFromChannelThread = "channel_thread"

// compiledSlot is one slot's identity paired with the COMPILED form of every
// precondition it declares, in declaration order.
//
// It exists so the expressions are compiled exactly once per reconcile.
// validateSlotDeclarations already has to compile them to reject a broken one,
// and the admission-time reachability check downstream needs the same
// programs' fact references. Compiling a second time there would be a second
// call site with its own failure mode — free to reject something this one
// accepted, on an object that already passed validation, with no author
// anywhere to read the message.
type compiledSlot struct {
	// ResourceType is the slot's declared type — the axis a precondition on
	// this slot holds shut.
	ResourceType string
	// Rules are the compiled predicates, one per requires[] entry, in the
	// order they were declared so an index in a message addresses the entry an
	// author wrote.
	Rules []*precondition.Compiled
}

// validateSlotDeclarations checks each authz.slots entry for internal
// consistency — the rules the CRD schema cannot express because they span two
// fields or two list elements.
//
// Every rule here rejects a declaration whose written form implies a protection
// it does not actually provide. That is the failure mode worth being strict
// about: an absent policy is visibly absent, while an inert one sits in the YAML
// looking like a control.
//
// The compiled preconditions come back with the verdict because compiling them
// is how the verdict is reached; see compiledSlot. They are meaningful only
// when the returned reason is empty — a rejected class stopped mid-walk and its
// slice is partial by construction.
func validateSlotDeclarations(ac *spiceboxv1alpha1.AgentClass) ([]compiledSlot, string, string) {
	slots := ac.Spec.GetSlots()
	if len(slots) == 0 {
		return nil, "", ""
	}

	compiled := make([]compiledSlot, 0, len(slots))
	seen := make(map[string]struct{}, len(slots))
	for _, s := range slots {
		ref := fmt.Sprintf("authz.slots[%s]", s.ResourceType)

		if _, dup := seen[s.ResourceType]; dup {
			return nil, spiceboxv1alpha1.ReasonSlotDeclarationInvalid,
				fmt.Sprintf("%s: resourceType %q is declared more than once; "+
					"two entries for one type leave it ambiguous which governs when they disagree",
					ref, s.ResourceType)
		}
		seen[s.ResourceType] = struct{}{}

		if reason, msg := validateEnumList(ref, "fillFrom", s.FillFrom, validFillFrom); reason != "" {
			return nil, reason, msg
		}
		if reason, msg := validateEnumList(ref, "autoGrantFrom", s.AutoGrantFrom, validAutoGrantFrom); reason != "" {
			return nil, reason, msg
		}
		if s.Membership != "" {
			if _, ok := validMembership[s.Membership]; !ok {
				return nil, spiceboxv1alpha1.ReasonSlotDeclarationInvalid,
					fmt.Sprintf("%s: unknown membership %q; valid values are %s",
						ref, s.Membership, sortedKeys(validMembership))
			}
		}

		// autoGrantFrom names whose thread contributions may bind without an
		// approval. With no channel_thread source nothing is ever seeded from a
		// thread, so the policy never runs — and unlike an omitted field, a
		// written one reads as though the agent is being constrained.
		if len(s.AutoGrantFrom) > 0 && !containsString(s.FillFrom, fillFromChannelThread) {
			return nil, spiceboxv1alpha1.ReasonSlotDeclarationInvalid,
				fmt.Sprintf("%s: autoGrantFrom is set but fillFrom does not include %q, "+
					"so the policy can never take effect. Either add %q to fillFrom or drop "+
					"autoGrantFrom — an inert trust policy reads as a control that is not there.",
					ref, fillFromChannelThread, fillFromChannelThread)
		}

		// The two inert-declaration cases that point the OTHER way: a fill
		// mechanism configured for a source the slot has excluded. These read as
		// capability rather than constraint, so the failure is an author
		// believing the agent can reach something it cannot — a class pinned to
		// a repo that never binds, or an extraction prompt no extractor runs.
		if len(s.Defaults) > 0 && !authz.AllowsFill(s.FillFrom, authz.FillDefault) {
			return nil, spiceboxv1alpha1.ReasonSlotDeclarationInvalid,
				fmt.Sprintf("%s: defaults are set but fillFrom does not include %q, so none of "+
					"them can ever bind. Either add %q to fillFrom or drop defaults.",
					ref, authz.FillDefault, authz.FillDefault)
		}
		if s.ExtractionPrompt != "" && !authz.AllowsExtractedBinding(s.FillFrom) {
			return nil, spiceboxv1alpha1.ReasonSlotDeclarationInvalid,
				fmt.Sprintf("%s: extractionPrompt is set but fillFrom admits no extractor binding; "+
					"add one of query, extract or ask to fillFrom, or drop extractionPrompt (no "+
					"extractor ever reads it as written).", ref)
		}

		// A slot that declares requires[] must not admit a fill source that
		// writes a slot grant OUTSIDE the admissibleCandidates filter. Two
		// sources do this today: channel_thread, whose channelsd backfill seeds
		// SeedFromThread's output straight into GrantSlots
		// (pkg/channels/channelsd/pipeline/backfill.go); and trigger, whose
		// BindTriggerSlots does the same directly at session mint
		// (pkg/channels/channelsd/pipeline/triggerslots.go). Neither runs
		// checkPreconditions, so a gated slot admitting either would bind with
		// its precondition unevaluated — the gate this branch builds, bypassed.
		// The other fill sources are safe here and do not belong in this
		// refusal: the three that route through admissibleCandidates (default,
		// query/extract/ask, observed) run the gate; metaagent applies a scope
		// delta and grants no slot at all (pkg/authz/hooks/metaagent_apply.go);
		// approved is the human waiver by design. If a FUTURE fill source gains
		// a direct-grant path like these two, it must be added to this check.
		//
		// channel_thread is checked with AllowsFill, not a contains-check, on
		// purpose: an empty fillFrom admits EVERY source (absence narrows
		// nothing), so a gated slot with no fillFrom at all is the likelier
		// real-world mistake — an author writes requires[] and no fillFrom,
		// believing the gate protects them — and that shape must be refused
		// just as an explicit channel_thread is. trigger is the opposite case:
		// ExplicitlyAllowsFill is the right predicate there because an unset
		// fillFrom never makes a slot trigger-eligible in the first place (see
		// authz.ExplicitlyAllowsFill's doc comment) — the empty-fillFrom shape
		// is already covered by the channel_thread arm above. This is cheaper
		// than compiling the predicates, so it runs before
		// validateSlotPreconditions below.
		if len(s.Requires) > 0 && authz.AllowsFill(s.FillFrom, authz.FillChannelThread) {
			return nil, spiceboxv1alpha1.ReasonSlotDeclarationInvalid,
				fmt.Sprintf("%s: requires[] declares a precondition but fillFrom admits %q, "+
					"which seeds a slot grant in channelsd without evaluating that precondition — "+
					"the gate it declares would be bypassed. Restrict fillFrom to sources that run "+
					"the gate (e.g. observed), or drop requires.",
					ref, authz.FillChannelThread)
		}
		if len(s.Requires) > 0 && authz.ExplicitlyAllowsFill(s.FillFrom, authz.FillTrigger) {
			return nil, spiceboxv1alpha1.ReasonSlotDeclarationInvalid,
				fmt.Sprintf("%s: requires[] declares a precondition but fillFrom admits %q, which writes "+
					"a slot grant in channelsd at session mint without evaluating that precondition — "+
					"the gate it declares would be bypassed. Restrict fillFrom to sources that run "+
					"the gate (e.g. observed), or drop requires.",
					ref, authz.FillTrigger)
		}

		rules, reason, msg := validateSlotPreconditions(ref, s.Requires)
		if reason != "" {
			return nil, reason, msg
		}
		compiled = append(compiled, compiledSlot{ResourceType: s.ResourceType, Rules: rules})
	}
	return compiled, "", ""
}

// validateSlotPreconditions compiles every predicate a slot declares, so an
// author learns a broken one HERE — where the message can name the entry — and
// not at dispatch, where a predicate that cannot be evaluated surfaces as a slot
// that never binds, for no visible reason.
//
// The rules themselves belong to precondition.Compile and are not restated: it
// refuses a non-bool result, an unknown provenance, has() over a fact name, and
// every `facts` shape it cannot classify into a reference. Its errors are
// written to be read by an author, so they are quoted verbatim and only prefixed
// with the entry's address.
//
// The compiled programs are returned so the single compile done here serves
// every later reader of a predicate's fact references; see compiledSlot.
func validateSlotPreconditions(ref string, requires []spiceboxv1alpha1.SlotPrecondition) ([]*precondition.Compiled, string, string) {
	if len(requires) == 0 {
		return nil, "", ""
	}
	out := make([]*precondition.Compiled, 0, len(requires))
	for j, p := range requires {
		where := fmt.Sprintf("%s.requires[%d]", ref, j)

		// The three message fields carry MinLength=1 in the CRD, so on every
		// normal write path the apiserver has already refused an empty one and
		// this cannot fire. It is NOT the enum maps' case above: those defend
		// against an object written before their marker existed, and this field
		// has never shipped without its MinLength. What is left is a CRD
		// hand-edited or downgraded in a cluster, a direct etcd write, and a
		// future in-process caller that constructs a slot without going through
		// the apiserver at all — the third being the one likely to actually
		// happen.
		//
		// Worth the four lines because the failure is silent in the direction
		// that matters: a precondition missing either message can only ever deny
		// without explaining. undeterminedHint is the one thing that tells the
		// agent the gate is answerable, and refusalMessage is the only account a
		// human ever gets.
		if p.UndeterminedHint == "" {
			return nil, spiceboxv1alpha1.ReasonSlotDeclarationInvalid,
				fmt.Sprintf("%s: undeterminedHint is required; without one the agent sees a bare "+
					"denial for a fact it has not established yet, which reads as a system fault "+
					"and tells it not to bother making the call that would satisfy the gate", where)
		}
		if p.RefusalMessage == "" {
			return nil, spiceboxv1alpha1.ReasonSlotDeclarationInvalid,
				fmt.Sprintf("%s: refusalMessage is required; it cannot be derived from cel, and it "+
					"is the only plain-language account of what was refused a human ever sees", where)
		}

		c, err := precondition.Compile(p.CEL)
		if err != nil {
			return nil, spiceboxv1alpha1.ReasonSlotDeclarationInvalid,
				fmt.Sprintf("%s: %v", where, err)
		}
		out = append(out, c)
	}
	return out, "", ""
}

func validateEnumList(ref, field string, values []string, valid map[string]struct{}) (string, string) {
	for _, v := range values {
		if _, ok := valid[v]; !ok {
			return spiceboxv1alpha1.ReasonSlotDeclarationInvalid,
				fmt.Sprintf("%s: unknown %s value %q; valid values are %s",
					ref, field, v, sortedKeys(valid))
		}
	}
	return "", ""
}

func containsString(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

// sortedKeys renders a valid-value set deterministically, so the same bad input
// always produces the same message and the condition does not churn.
func sortedKeys(m map[string]struct{}) string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return strings.Join(out, ", ")
}
