package agentclass

import (
	"fmt"
	"sort"
	"strings"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/authz/precondition"
)

// factProducer is one tool that records a fact, together with what must already
// be bound before that tool may be called.
type factProducer struct {
	// name addresses the tool the way an author would go looking for it.
	name string
	// gatedBy is every resource type a permission check reachable on this tool
	// names, sorted and deduplicated. Empty means the tool is not gated at all
	// — every call of it is allowed — which is a fact about the tool, not an
	// absence of information. unresolved distinguishes the two.
	gatedBy []string
	// unresolved, when non-empty, says why gatedBy could not be determined.
	// A producer whose gate is unknown is NOT the same as one with no gate and
	// is never treated as blocked; see resolveFactSources.
	unresolved string
}

// blockedBy reports whether EVERY way of calling this producer is gated on the
// one resource type slotType — which is what makes a fact it records
// unreachable while a precondition on that slot holds the slot shut.
//
// False whenever the answer is not proven: an unresolved gate, or an ungated
// tool. Both are "we cannot show this is impossible", and admission refuses
// only what it can show.
//
// ONE slot at a time, which is where the detection's coverage ends: a mutual
// deadlock spanning two slots passes every call of this. See the gap section on
// resolveFactSources.
func (p factProducer) blockedBy(slotType string) bool {
	if p.unresolved != "" || len(p.gatedBy) == 0 {
		return false
	}
	for _, t := range p.gatedBy {
		if t != slotType {
			return false
		}
	}
	return true
}

// resolveFactSources answers, for every fact this class's slot preconditions
// read, where that fact can come from — and refuses the one unsatisfiable shape
// admission can actually PROVE.
//
// # The refusal
//
// A slot's `requires` reads an observed fact. Every tool that records that fact
// is gated on the slot's own resource type. So the slot cannot bind until the
// fact is recorded, and the fact cannot be recorded until the slot binds. The
// agent is handed an undeterminedHint naming a call it will be denied, and
// burns its budget retrying. That is an admission failure, not a hang — the
// same argument spec.subagents makes for DAG-validating the delegation graph at
// admission: a bound that only holds at runtime does not hold.
//
// The detection ASSUMES NO WAIVER, and that assumption is deliberate rather
// than an oversight. The human waiver card binds an approved slot through
// authz.BindApproved(PreconditionsWaived), which does NOT evaluate the
// precondition, so a human could break this deadlock by hand on
// every session. A class that only functions that way is misconfigured, not
// designed, so it is still refused — but the message names the waiver, so an
// author who genuinely intended it learns why the class was refused instead of
// guessing.
//
// # Why detection is deliberately partial
//
// It is sound for a fact SOME tool declares a producer for. A fact nothing
// declares a producer for is permanently undetermined too, but at admission
// that is indistinguishable from a fact a channel kind or an out-of-class tool
// legitimately supplies — so it is published on status.factSources with an
// empty producedBy and does NOT fail the class. Refusing it would break every
// class whose facts arrive from outside its own spec.
//
// Conservative in one more direction: a producer's gate is the union of EVERY
// permission check reachable on that tool, because which check runs depends on
// the call's arguments and which observes block fires depends on its result,
// and neither is decidable here. So "blocked" means every possible call of the
// tool is gated on the one type. A producer whose gate cannot be resolved at
// all breaks the conclusion rather than being assumed either way.
//
// # The gap this does NOT cover: a cycle across TWO slots
//
// blockedBy asks only about the slot currently under examination, so the shape
// it finds is a slot deadlocked against ITSELF. A MUTUAL deadlock across two
// slots is not detected and is not implied by anything above: slot A's
// precondition reads a fact produced only by a tool gated on B, and slot B's
// reads a fact produced only by a tool gated on A. Neither call returns true,
// the class is admitted, and at runtime the deadlock is identical to the
// single-slot one — neither slot can ever bind.
//
// Left open deliberately rather than overlooked. Closing it means a reachability
// walk over a slot graph (which slots gate the producers of which slots) rather
// than a per-slot predicate, and that is a bigger change than this refusal
// earned on its own.
//
// Returns the sources to publish along with the verdict: the caller stamps them
// before acting on a refusal, so a refused class still shows an operator what
// its preconditions depend on.
func resolveFactSources(
	slots []compiledSlot,
	mcpServers []spiceboxv1alpha1.MCPServer,
	toolspecs []spiceboxv1alpha1.SpiceboxToolspec,
	toolkits []spiceboxv1alpha1.SpiceboxToolkit,
) ([]spiceboxv1alpha1.FactSource, string, string) {
	refs := referencedFacts(slots)
	if len(refs) == 0 {
		return nil, "", ""
	}
	producers := collectFactProducers(mcpServers, toolspecs, toolkits)
	sources := publishFactSources(refs, producers)

	for _, s := range slots {
		for j, rule := range s.Rules {
			if rule == nil {
				continue
			}
			// References() is exhaustive by construction: precondition.Compile
			// refuses every `facts` shape it cannot classify into a reference,
			// so a predicate that compiled has no unlisted fact read. That is
			// what lets this walk claim to have seen every producer a rule
			// depends on.
			for _, ref := range rule.References() {
				if ref.Provenance != precondition.ProvenanceObserved {
					continue // an envelope fact is never recorded by a tool
				}
				prods := producers[ref.Name]
				if len(prods) == 0 {
					continue // no declared producer: a warning, published above
				}
				blocked := true
				for _, p := range prods {
					if !p.blockedBy(s.ResourceType) {
						blocked = false
						break
					}
				}
				if !blocked {
					continue
				}
				return sources, spiceboxv1alpha1.ReasonSlotPreconditionUnsatisfiable,
					cycleMessage(s.ResourceType, j, ref.Name, prods)
			}
		}
	}
	return sources, "", ""
}

// cycleMessage names both sides of the deadlock and the one thing that could
// break it, so an author is not left to infer either.
func cycleMessage(slotType string, requiresIndex int, factName string, prods []factProducer) string {
	names := make([]string, 0, len(prods))
	for _, p := range prods {
		names = append(names, p.name)
	}
	sort.Strings(names)
	return fmt.Sprintf(
		"authz.slots[%s].requires[%d]: the predicate reads observed fact %q, and every tool that "+
			"records it (%s) is itself gated on %s — the very slot this precondition holds shut. "+
			"Nothing can ever satisfy it: the agent is handed a hint naming a call it will be "+
			"denied, and burns its budget retrying. Record the fact from a tool gated on some "+
			"other resource type, or drop the requirement. (An approval binds a slot without "+
			"evaluating its preconditions, so a human could break this deadlock by hand on every "+
			"session — a class that only works that way is misconfigured, not designed, which is "+
			"why it is refused here rather than left to run.)",
		slotType, requiresIndex, factName, strings.Join(names, ", "), slotType)
}

// referencedFacts collects every fact this class's preconditions read,
// deduplicated and sorted, so the published list is stable across reconciles.
func referencedFacts(slots []compiledSlot) []precondition.FactRef {
	seen := map[precondition.FactRef]struct{}{}
	for _, s := range slots {
		for _, rule := range s.Rules {
			if rule == nil {
				continue
			}
			for _, ref := range rule.References() {
				seen[ref] = struct{}{}
			}
		}
	}
	if len(seen) == 0 {
		return nil
	}
	out := make([]precondition.FactRef, 0, len(seen))
	for ref := range seen {
		out = append(out, ref)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Provenance != out[j].Provenance {
			return out[i].Provenance < out[j].Provenance
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// publishFactSources renders the observation: one entry per (fact, producer),
// and one entry with an empty producedBy for a fact nothing on this class
// records.
//
// Every entry carries a State — the machine-readable discriminator a consumer
// branches on — because two of the five states share an empty gatedBy and two
// share an empty producedBy, so neither field separates them on its own. Detail
// says the same thing in prose for a person; it is not the discriminator, and a
// consumer matching on its wording would break the first time the wording
// improves.
func publishFactSources(
	refs []precondition.FactRef,
	producers map[string][]factProducer,
) []spiceboxv1alpha1.FactSource {
	out := make([]spiceboxv1alpha1.FactSource, 0, len(refs))
	for _, ref := range refs {
		if ref.Provenance != precondition.ProvenanceObserved {
			out = append(out, spiceboxv1alpha1.FactSource{
				Provenance: ref.Provenance,
				Name:       ref.Name,
				State:      spiceboxv1alpha1.FactSourceStateEnvelope,
				Detail: "an envelope fact is derived by platform code from a signed payload, " +
					"not recorded by any tool; nothing on this class can supply it if the " +
					"channel kind carrying it is not bound",
			})
			continue
		}
		prods := producers[ref.Name]
		if len(prods) == 0 {
			out = append(out, spiceboxv1alpha1.FactSource{
				Provenance: ref.Provenance,
				Name:       ref.Name,
				State:      spiceboxv1alpha1.FactSourceStateAbsent,
				Detail: "no tool on this class declares an observes block recording this fact, " +
					"so every precondition reading it can only ever be undetermined and the " +
					"slot never binds. Declare a producer, or check the spelling.",
			})
			continue
		}
		for _, p := range prods {
			e := spiceboxv1alpha1.FactSource{
				Provenance: ref.Provenance,
				Name:       ref.Name,
				State:      spiceboxv1alpha1.FactSourceStateGated,
				ProducedBy: p.name,
				GatedBy:    p.gatedBy,
			}
			switch {
			case p.unresolved != "":
				e.State = spiceboxv1alpha1.FactSourceStateUnresolved
				e.Detail = p.unresolved
			case len(p.gatedBy) == 0:
				e.State = spiceboxv1alpha1.FactSourceStateUngated
				e.Detail = "this producer is not gated by any permission check, so the call " +
					"that records the fact is always allowed"
			}
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Provenance != out[j].Provenance {
			return out[i].Provenance < out[j].Provenance
		}
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].ProducedBy < out[j].ProducedBy
	})
	return out
}

// collectFactProducers maps each observed fact name to every tool on this class
// that records it.
//
// BOTH producer surfaces, and that is load-bearing rather than completeness for
// its own sake. Walking MCPServer tools alone would report a toolspec-produced
// fact as having no producer at all, which routes a genuine cycle to the
// warning path and admits the class — the silently-inert validation this check
// exists to prevent.
//
// Keyed on the fact NAME alone. The observes subject's resource type is a CEL
// expression, not a literal, so it cannot be read here — which means a tool
// recording `x` about a DIFFERENT resource type than the precondition's slot
// still counts as a reachable producer of `x` and can mask a real cycle. In the
// conservative direction (a class is admitted, never refused, on this), and the
// price of the subject type not being decidable at admission.
func collectFactProducers(
	mcpServers []spiceboxv1alpha1.MCPServer,
	toolspecs []spiceboxv1alpha1.SpiceboxToolspec,
	toolkits []spiceboxv1alpha1.SpiceboxToolkit,
) map[string][]factProducer {
	out := map[string][]factProducer{}
	add := func(p factProducer, observes []spiceboxv1alpha1.ObservesBlock) {
		for _, name := range factNamesOf(observes) {
			out[name] = append(out[name], p)
		}
	}

	for _, srv := range mcpServers {
		for _, t := range srv.Spec.Tools {
			types := map[string]struct{}{}
			addCheckType(types, t.Permission)
			for _, v := range t.PermissionVariants {
				addCheckType(types, &v.Check)
			}
			add(factProducer{
				name:    fmt.Sprintf("MCPServer/%s tool/%s", srv.Name, t.Name),
				gatedBy: sortedTypes(types),
			}, t.Observes)
		}
	}

	byName := make(map[string]*spiceboxv1alpha1.SpiceboxToolkit, len(toolkits))
	for i := range toolkits {
		byName[toolkits[i].Name] = &toolkits[i]
	}
	for i := range toolspecs {
		ts := &toolspecs[i]
		p := factProducer{name: fmt.Sprintf("SpiceboxToolspec/%s", ts.Name)}
		tk, ok := byName[ts.Spec.Toolkit.Name]
		if !ok {
			// A toolspec whose toolkit is not among the ones resolved for this
			// class. Its facts are real and its gate is unknown, which is a
			// third state — reported as itself rather than folded into "no
			// producer", which would be a claim this walk cannot make. A
			// warning that overstates what it knows is worse than one that
			// admits a gap.
			p.unresolved = fmt.Sprintf(
				"toolkit %q could not be resolved for this class, so the permission checks "+
					"gating the tool this toolspec becomes are unknown",
				ts.Spec.Toolkit.Name)
		} else {
			p.gatedBy = sortedTypes(toolspecGateTypes(ts, tk))
		}
		add(p, ts.Spec.Observes)
	}
	return out
}

// toolspecGateTypes returns every resource type a permission check reachable
// through this toolspec names.
//
// A toolspec carries no Permission of its own: the tool it becomes is gated by
// the TOOLKIT — the matched subcommand's own check, one of that subcommand's
// argument variants, or the toolkit-level default when neither applies
// (pkg/agent/tool/sandbox.SandboxTool.PermissionForCall). Which of those runs
// depends on the call's argv, so all of them are collected: the union is the
// only sound answer to "what could gate a call that records this fact".
//
// The toolkit default is always included when one is declared, because an argv
// matching no subcommand resolves to it.
func toolspecGateTypes(
	ts *spiceboxv1alpha1.SpiceboxToolspec,
	tk *spiceboxv1alpha1.SpiceboxToolkit,
) map[string]struct{} {
	types := map[string]struct{}{}
	addCheckType(types, tk.Spec.Permission)
	for _, sub := range tk.Spec.Subcommands {
		if !toolspecAllowsSubcommand(ts, sub) {
			continue
		}
		addCheckType(types, sub.Permission)
		for _, v := range sub.PermissionVariants {
			addCheckType(types, &v.Check)
		}
	}
	return types
}

// toolspecAllowsSubcommand reports whether the toolspec's narrowing admits sub,
// mirroring pkg/agent/tool/sandbox.SandboxTool.allowsSubcommand — a subcommand
// the spec excluded can never be called, so nothing it declares gates anything.
//
// Entries are space-joined paths. That spelling is not assumed here: the
// SpiceboxToolspec reconciler refuses an allowSubcommands entry that is not the
// space-joined path of a real subcommand, and validateBundles refuses a class
// whose toolspec is not Valid=True, so by the time this runs every entry has
// already been matched against a toolkit by the same rule.
func toolspecAllowsSubcommand(
	ts *spiceboxv1alpha1.SpiceboxToolspec,
	sub spiceboxv1alpha1.ToolkitSubcommand,
) bool {
	if len(ts.Spec.AllowSubcommands) == 0 {
		return true // no narrowing declared: the whole toolkit is reachable
	}
	want := strings.Join(sub.Path, " ")
	for _, a := range ts.Spec.AllowSubcommands {
		if strings.TrimSpace(a) == want {
			return true
		}
	}
	return false
}

// factNamesOf reads the fact NAMES a tool's observes blocks record, once each.
// The Facts map's keys are the names a precondition's reference must match; the
// values are CEL and the Subjects beside them are CEL string expressions, so
// neither can be keyed on here.
//
// Deduplicated across blocks: one tool may record the same fact from two
// observes blocks (different subjects, different conditions), and that is one
// producer of it, not two. Without this the class would publish two identical
// factSources rows.
func factNamesOf(blocks []spiceboxv1alpha1.ObservesBlock) []string {
	seen := map[string]struct{}{}
	for _, b := range blocks {
		for name := range b.Facts {
			seen[name] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// addCheckType records the resource type a permission's check names, tolerating
// every absent level: a permission with no check gates nothing.
func addCheckType(into map[string]struct{}, p *authz.Permission) {
	if p == nil || p.Check == nil || p.Check.ResourceType == "" {
		return
	}
	into[p.Check.ResourceType] = struct{}{}
}

// sortedTypes renders a resource-type set deterministically, so an unchanged
// class publishes an unchanged status and the set-on-change write stays a
// no-op.
func sortedTypes(m map[string]struct{}) []string {
	if len(m) == 0 {
		return nil
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
