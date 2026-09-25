// pkg/channels/channelkinds/slack/capability.go
//
// The capability question: which of the things an agent can do over Slack this
// one should be able to do.
//
// It is what makes the rest of the flow specific to the operator in front of
// it. The answer decides two things:
//
//   - the scopes the generated app manifest requests, via
//     channelkinds.ScopesFor over the selected features plus the baseline; and
//   - the bound AgentClass's spec.capabilities, via capabilityPatch.
//
// Without it the wizard would print every optional scope caveat to everybody —
// the files:read warning to an operator who never sends the agent a file, the
// files:write warning to one whose agent produces no artifacts.
package slack

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/pkg/agent/agentcaps"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelfeatures"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

// keyCapabilities is the answer key this question lands under: the capability
// names the operator checked, as a multi-value answer. Stable, like the rest —
// a caller seeding answers from flags for a non-interactive run addresses the
// question by it.
const keyCapabilities = "capabilities"

// capabilityPrompt is the question text, a named constant so a test asserting
// what the operator is asked compares against the string the question carries
// rather than a second copy of it.
const capabilityPrompt = "Enable for this agent"

// capabilityOption is one offered choice: an AgentClass capability this kind
// can actually deliver, together with what it costs on this transport and what
// stops working without it.
//
// Every field is derived, never transcribed. The set of options is the
// intersection of three sources — the channelfeatures table (which capabilities
// have a channel-side requirement at all), FeaturesFor (which features each
// implies), and the kind's own FeatureSupport (which of those it can satisfy).
type capabilityOption struct {
	capability string
	// defaultOn mirrors the capability registry: true means the capability is
	// active on an AgentClass that never mentions it. It is what makes "absent"
	// ambiguous, and it is why capabilityPatch exists.
	defaultOn bool
	features  []channelfeatures.Feature
	// scopes are the transport permissions the capability costs, deduplicated
	// and sorted by channelkinds.ScopesFor. Empty means it costs none.
	scopes []string
	// setup is where a human grants those permissions, one line per distinct
	// FeatureRequirement.Setup among the capability's features.
	setup []string
	// degrades is what stops working without them, one line per distinct
	// FeatureRequirement.Degrades.
	degrades []string
}

// capabilityOptions is the three-way intersection, as a sorted list.
//
// A capability is offered only when the kind supports EVERY feature it implies.
// Partial support is not offered degraded: the checkbox grants an AgentClass
// capability, and granting one whose transport half can only work in part would
// promise the user something this channel cannot do, with no way to say so on a
// checkbox.
//
// Sorted by capability name because everything downstream is positional — the
// checkbox numbering the accessible renderer prints, the option order the
// answer is canonicalized into, and the note the summary shows. Map iteration
// order reaching any of those would make them churn between runs.
func capabilityOptions(k channelkinds.Kind) []capabilityOption {
	support := k.FeatureSupport()
	defaults := channelfeatures.Capabilities()

	names := make([]string, 0, len(defaults))
	for name := range defaults {
		names = append(names, name)
	}
	sort.Strings(names)

	out := make([]capabilityOption, 0, len(names))
	for _, name := range names {
		features := channelfeatures.FeaturesFor(name)
		if len(features) == 0 {
			continue
		}
		reqs := make([]channelkinds.FeatureRequirement, 0, len(features))
		deliverable := true
		for _, f := range features {
			req, ok := support[f]
			if !ok {
				deliverable = false
				break
			}
			reqs = append(reqs, req)
		}
		if !deliverable {
			continue
		}

		opt := capabilityOption{
			capability: name,
			defaultOn:  defaults[name],
			features:   features,
			scopes:     channelkinds.ScopesFor(k, features),
		}
		for _, req := range reqs {
			opt.setup = appendDistinct(opt.setup, req.Setup)
			opt.degrades = appendDistinct(opt.degrades, req.Degrades)
		}
		out = append(out, opt)
	}
	return out
}

// slackCapabilityOptions is the option list for this kind. It is a pure
// function of the kind's FeatureSupport and the channelfeatures table, so every
// caller that needs it recomputes it rather than threading one copy through the
// flow — there is no state to keep in sync.
func slackCapabilityOptions() []capabilityOption { return capabilityOptions(&Kind{}) }

// appendDistinct appends s unless it is empty or already present. The
// requirements of one capability's features routinely share a Setup line (they
// are granted on the same Slack page), and repeating it would read as two
// separate instructions.
func appendDistinct(list []string, s string) []string {
	if s == "" {
		return list
	}
	for _, existing := range list {
		if existing == s {
			return list
		}
	}
	return append(list, s)
}

// noScopeCost is what a capability costs when it needs no Slack SCOPE — the
// words the CHECKBOX uses, since a checkbox has room for nothing longer.
//
// It says "scope" rather than "permission" deliberately: the credential portal
// costs an event subscription and a manifest setting instead, so a checkbox
// promising no extra permission at all would be wrong. Saying what it does
// cost is the post-apply note's job, and that note prefers the kind's own
// Setup line to this phrase whenever there is one — which for every Slack
// capability there is.
const noScopeCost = "no extra Slack scope"

// label is the option's text in the checkbox list: the capability and the
// scopes checking it costs. huh's Option carries no separate description field
// and its accessible renderer prints only these, so what a choice costs has to
// be IN the label or it is invisible off-TTY.
//
// It deliberately stops at the scopes. Where a capability costs something other
// than a scope, that instruction is a URL and a manifest path — readable in the
// note printed after the run, unreadable as a line of a checkbox list.
func (o capabilityOption) label() string {
	if len(o.scopes) == 0 {
		return o.capability + " — " + noScopeCost
	}
	return o.capability + " — " + strings.Join(o.scopes, ", ")
}

// note is the post-apply line for a capability the user chose: what it cost and
// what breaks without it. Derived from the kind's own FeatureSupport rather than
// written out, so a caveat can exist only for a capability that was actually
// chosen, and can never disagree with the scope the manifest requested for it.
//
// The cost here is the scopes when there are any and the Setup line otherwise:
// the credential portal costs an event subscription and a manifest setting
// rather than a scope, and reporting only "no extra permission" would hide the
// one step its user still has to take.
func (o capabilityOption) note() string {
	cost := noScopeCost
	switch {
	case len(o.scopes) > 0:
		cost = strings.Join(o.scopes, ", ")
	case len(o.setup) > 0:
		cost = strings.Join(o.setup, " ")
	}
	s := fmt.Sprintf("%s — %s.", o.capability, cost)
	if len(o.degrades) > 0 {
		s += " Without it: " + strings.Join(o.degrades, "; ") + "."
	}
	return s
}

// boundClass loads the AgentClass the capability question's boxes are
// pre-checked from. A nil client is an offline dry-run with nothing to read,
// and an empty name is a run in which the class is not yet known; both
// pre-check from the capability defaults alone (activeCapabilities' nil-class
// branch).
func boundClass(ctx context.Context, k8s client.Client, namespace, name string) (*spiceboxv1alpha1.AgentClass, error) {
	if k8s == nil || name == "" {
		return nil, nil
	}
	var class spiceboxv1alpha1.AgentClass
	if err := k8s.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &class); err != nil {
		return nil, fmt.Errorf("read AgentClass %s/%s to see what it already grants: %w", namespace, name, err)
	}
	return &class, nil
}

// activeCapabilities is what the checkboxes start out as: the capabilities the
// bound AgentClass has active right now, resolved through agentcaps — the same
// one definition every runtime reader uses — so a default-on capability with no
// explicit grant shows checked and the user is not asked to re-decide it.
//
// A malformed capability value is refused rather than guessed at. This run
// is about to rewrite that very field, and the two readings of a value neither
// agentcaps nor anything else can parse ("the user meant on" / "the user meant
// off") differ by a capability being silently revoked.
func activeCapabilities(opts []capabilityOption, class *spiceboxv1alpha1.AgentClass) ([]string, error) {
	out := make([]string, 0, len(opts))
	for _, o := range opts {
		grant, err := agentcaps.GrantOf(class, o.capability)
		if err != nil {
			return nil, fmt.Errorf("AgentClass %s: capability %q holds a value this run cannot read, so it cannot tell whether it is on: %w",
				classRef(class), o.capability, err)
		}
		if agentcaps.Active(o.defaultOn, grant) {
			out = append(out, o.capability)
		}
	}
	return out, nil
}

// classRef names an AgentClass for an error message, nil included.
func classRef(class *spiceboxv1alpha1.AgentClass) string {
	if class == nil {
		return "<none>"
	}
	return class.Namespace + "/" + class.Name
}

// canonicalCapabilities reduces an answer to the offered options it names, in
// option order.
//
// The order matters because the answer is what the summary shows and what
// Result reads; canonicalizing it here means a seeded answer and a checked one
// are recorded identically. The membership check matters more: the result is
// written verbatim onto an AgentClass, so a name outside the offered set is
// refused rather than granted — it is either a capability this kind cannot
// deliver or a typo, and neither should reach a cluster.
func canonicalCapabilities(opts []capabilityOption, answer []string) ([]string, error) {
	offered := make(map[string]bool, len(opts))
	for _, o := range opts {
		offered[o.capability] = true
	}

	want := make(map[string]bool, len(answer))
	for _, name := range answer {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if !offered[name] {
			return nil, fmt.Errorf("capability %q is not one a Slack channel can enable; this kind offers: %s",
				name, strings.Join(optionCapabilityNames(opts), ", "))
		}
		want[name] = true
	}

	out := make([]string, 0, len(want))
	for _, o := range opts {
		if want[o.capability] {
			out = append(out, o.capability)
		}
	}
	return out, nil
}

// optionCapabilityNames lists the offered capability names, for an error that
// tells the caller what they could have said instead.
func optionCapabilityNames(opts []capabilityOption) []string {
	out := make([]string, 0, len(opts))
	for _, o := range opts {
		out = append(out, o.capability)
	}
	return out
}

// selectedCapabilitiesFrom returns the offered options this run's answer
// checked, resolved through the one option list so a name nothing offers is
// simply absent rather than silently carried.
func selectedCapabilitiesFrom(checked []string) []capabilityOption {
	want := map[string]bool{}
	for _, name := range checked {
		want[name] = true
	}
	var out []capabilityOption
	for _, o := range slackCapabilityOptions() {
		if want[o.capability] {
			out = append(out, o)
		}
	}
	return out
}

// selectedFeaturesFrom is what the Slack app must be able to do: the baseline
// every channel needs whatever agent is behind it, plus the features the
// checked capabilities imply.
//
// This is what the generated app manifest's scope list is derived from, and it
// is the whole reason the capability question is asked at all: the manifest
// requests exactly what was chosen, rather than the union of what any agent
// could ever need.
func selectedFeaturesFrom(checked []string) []channelfeatures.Feature {
	seen := map[channelfeatures.Feature]bool{}
	var out []channelfeatures.Feature
	add := func(features []channelfeatures.Feature) {
		for _, f := range features {
			if !seen[f] {
				seen[f] = true
				out = append(out, f)
			}
		}
	}
	add(channelfeatures.Baseline())
	for _, o := range selectedCapabilitiesFrom(checked) {
		add(o.features)
	}
	return out
}

// capabilityPatch is the server-side-apply payload for the bound AgentClass's
// spec.capabilities: exactly one explicit {"enabled": <checked>} per OFFERED
// capability, and nothing else.
//
// Every offered capability gets a key regardless of its default. Writing a key
// only where the answer differs from the default is silently wrong in BOTH
// directions, because "no key" cannot express a decision to a server where
// somebody else may already have written one — SSA removes only fields the
// applying manager itself owns:
//
//   - a checked default-on capability left keyless cannot clear another
//     manager's {"enabled": false}: the box shows checked and it stays off;
//   - an UNCHECKED opt-in capability left keyless cannot revoke another
//     manager's grant — the user unchecks it, the wizard writes nothing, and
//     the grant survives. A revocation that silently did not happen.
//
// The ownership is real: `oap agent install` applies a bundle's AgentClass under
// its own field manager with ForceOwnership
// (pkg/platform/oap/install/apply.go), so any .oap bundle declaring
// spec.capabilities owns those keys. An explicit value is the only form of the
// answer that reaches the object through another manager's ownership.
//
// The map is a pure function of (offered options, checked set): no wall clock,
// no counters, no map iteration order — fixed literal values, keys built by
// walking the sorted option list. Unchanged answers therefore produce a
// byte-identical apply, which is what SSA requires of a client-owned field.
//
// Only capabilities in opts are mentioned. One outside the channelfeatures
// table, or one this kind cannot deliver, is left exactly as the AgentClass has
// it — it may be serving another channel the agent is bound to.
//
// It is an *unstructured.Unstructured, not a typed AgentClass, so what is
// applied is precisely what is written here. AgentClassSpec.SystemPrompt is a
// struct value with no omitempty, so marshalling a typed AgentClass emits
// "spec":{"systemPrompt":{},…}, and the CLI's apply path forces ownership —
// handing the wizard's field manager somebody else's system prompt on every
// channel create. Building the payload field by field cannot regress that way;
// sanitizing afterwards would need re-verifying on every new spec field.
func capabilityPatch(namespace, className string, opts []capabilityOption, checked []string) *unstructured.Unstructured {
	want := make(map[string]bool, len(checked))
	for _, name := range checked {
		want[name] = true
	}

	caps := make(map[string]any, len(opts))
	for _, o := range opts {
		caps[o.capability] = map[string]any{"enabled": want[o.capability]}
	}

	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "agentprimitives.authzed.com/v1alpha1",
		"kind":       "AgentClass",
		"metadata": map[string]any{
			"name":      className,
			"namespace": namespace,
		},
		"spec": map[string]any{"capabilities": caps},
	}}
}

// capabilityGuidance says what the question is and what each answer breaks if
// left unchecked. The scopes are on the checkboxes themselves; this carries the
// half that will not fit there.
func capabilityGuidance(opts []capabilityOption) string {
	var b strings.Builder
	b.WriteString("What should this agent be able to do on Slack?\n\n")
	b.WriteString("Only what you check is requested: the app manifest asks for exactly these\n")
	b.WriteString("scopes, and the agent's capabilities are set to match.\n\n")
	b.WriteString("Leaving one off means:\n")
	for _, o := range opts {
		if len(o.degrades) == 0 {
			continue
		}
		fmt.Fprintf(&b, "  %s — %s\n", o.capability, strings.Join(o.degrades, "; "))
	}
	return b.String()
}
