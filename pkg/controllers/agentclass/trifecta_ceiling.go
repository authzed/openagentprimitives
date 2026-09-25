package agentclass

import (
	"context"
	"fmt"
	"sort"
	"strings"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// validateTrifectaCeiling refuses a class that declares it can never ACT while
// holding a tool that can.
//
// A DECLARED ceiling checked against the DERIVED surface, the same shape budget
// already uses: the class declares, the controller derives, and a contradiction
// is surfaced rather than silently ignored. The alternative — accepting the
// declaration and letting the delegation fail later — hides a configuration
// mistake behind a runtime refusal nobody can trace back to the YAML that
// caused it.
//
// Walked from the CRD specs rather than from permsurface.Enumerate because the
// controller has no live tools, exactly as declarableSurface does. The two read
// the same source for the same reason.
func validateTrifectaCeiling(
	ac *spiceboxv1alpha1.AgentClass,
	mcpServers []spiceboxv1alpha1.MCPServer,
	toolkits []spiceboxv1alpha1.SpiceboxToolkit,
	sidecarToolboxes []spiceboxv1alpha1.SidecarToolbox,
) (string, string) {
	if ac == nil || ac.Spec.Authz == nil || ac.Spec.Authz.Trifecta == nil {
		return "", ""
	}
	if !ac.Spec.Authz.Trifecta.NeverConsequential {
		return "", ""
	}
	offenders := ConsequentialHandles(mcpServers, toolkits, sidecarToolboxes)
	if len(offenders) == 0 {
		return "", ""
	}
	return spiceboxv1alpha1.ReasonTrifectaCeilingContradicted, fmt.Sprintf(
		"authz.trifecta.neverConsequential is set, but this class holds %d tool permission(s) that can act: %s. "+
			"Either drop the declaration or remove the readwrite/external permission(s) — a class cannot both "+
			"promise it never acts and be given the means to",
		len(offenders), strings.Join(offenders, ", "))
}

// ConsequentialHandles returns the handles this class's tools key on whose
// StateImpact can ACT — readwrite or external.
//
// EXPORTED because the trifecta's containment tripper asks the same question
// of the same class, and two implementations of "can this class act" would
// drift into the admission validator and the tripper disagreeing about one
// AgentClass — a class accepted as never-consequential that the tripper then
// treats as able to act, or the reverse. One derivation, two consumers.
//
// It is also the only form of the question the OPERATOR can answer. The tripper
// must not read anything the runner produced (this package's doc: the runner is
// the party under suspicion), and a live tool set is runner knowledge. Walking
// the CRD specs is what makes the answer independent of the process being
// judged.
//
// ALL THREE tool-bearing CR kinds are walked — MCPServer, SpiceboxToolkit,
// SidecarToolbox — because the question is "can this class act", and which CR
// kind a tool arrived in has never changed whether calling it acts.
// SidecarToolboxSpec.Tools IS []MCPServerTool, so a sidecar tool carries the
// identical Permission and the identical StateImpact. Walking only the first
// two read "cannot act" for the shipped agent-builder, whose workshop sidecar
// is its only tool source and holds eight `external` tools — and a leg C that
// reads false is a trifecta that never trips, so the omission was fail-OPEN.
//
// A separate walk from keyingSources (slot_valuekey.go) for the same reason
// standingSources is: it flattens the same three kinds, but into
// PermissionChecks, which carry no StateImpact and no tool name — the two
// fields the consequential test and its handle naming are made of. Different
// part of the same CRs.
//
// READONLY is excluded, matching trifecta leg C exactly. Being able to read is
// leg B's territory; a class that can only look at things has not broken a
// never-act promise, and reporting one here would make the ceiling unusable for
// precisely the read-only classes it suits best.
func ConsequentialHandles(
	mcpServers []spiceboxv1alpha1.MCPServer,
	toolkits []spiceboxv1alpha1.SpiceboxToolkit,
	sidecarToolboxes []spiceboxv1alpha1.SidecarToolbox,
) []string {
	seen := map[string]struct{}{}
	// Reads the declarer's BASE permission only, on every kind — a variant's
	// StateImpact is not consulted here. Kept uniform deliberately: a
	// sidecar-specific rule would be the drift this one derivation exists to
	// prevent.
	add := func(name string, p *authz.Permission) {
		if p == nil {
			return
		}
		if p.StateImpact != authz.Readwrite && p.StateImpact != authz.External {
			return
		}
		// Named by the handle where one exists, because that is what the
		// runtime surface will call it and what an operator will grep for.
		// Falling back to the tool name keeps a permission with no check
		// reportable rather than silently dropped.
		if p.Check != nil && p.Check.Permission != "" && p.Check.ResourceType != "" {
			seen["perm:"+p.Check.Permission+":"+p.Check.ResourceType] = struct{}{}
			return
		}
		seen["tool:"+name] = struct{}{}
	}
	for _, srv := range mcpServers {
		for _, t := range srv.Spec.Tools {
			add(t.Name, t.Permission)
		}
	}
	for _, tk := range toolkits {
		for _, sub := range tk.Spec.Subcommands {
			add(tk.Name+" "+strings.Join(sub.Path, " "), sub.Permission)
		}
	}
	for _, tb := range sidecarToolboxes {
		for _, t := range tb.Spec.Tools {
			add(t.Name, t.Permission)
		}
	}
	out := make([]string, 0, len(seen))
	for h := range seen {
		out = append(out, h)
	}
	// Sorted so the condition message is stable across reconciles; an unsorted
	// list would rewrite status on every pass and churn field ownership.
	sort.Strings(out)
	return out
}

// ClassCanAct reports whether ac holds any tool permission that can ACT —
// trifecta leg C, asked of a class rather than of a call.
//
// It resolves the class's MCPServers, toolkits and SidecarToolboxes with the
// SAME helpers the reconciler uses and feeds them to the SAME
// ConsequentialHandles, so there is exactly one answer to "can this class act"
// in the codebase. The operator's containment tripper is the caller; giving it
// its own resolution would make a third copy, and three copies of a security
// question drift into a validator and a tripper disagreeing about one
// AgentClass.
//
// ALL THREE sources are read because leg C is FAIL-OPEN when one is omitted: a
// missed tool source makes the answer false, a false leg C stops the trifecta
// completing, and a trifecta that cannot complete never trips. The failure is
// silent in the dangerous direction, so this reads every kind that can carry a
// tool rather than the kinds a given class happens to use — the shipped
// agent-builder's only tool source is a SidecarToolbox, and it read "cannot
// act" while holding eight `external` tools.
//
// A read failure is returned, never swallowed into false. false is
// indistinguishable from "this class genuinely cannot act", so a transient API
// error would silently disarm containment; the caller (operatorClassCanAct)
// treats an error as a tripper that could not judge.
//
// Reads CRD specs only. That is what lets the tripper judge a session without
// consulting the runner it is judging — a live tool set is runner knowledge,
// and hold's doc is explicit that the runner is the party under suspicion.
func ClassCanAct(ctx context.Context, c client.Reader, ac *spiceboxv1alpha1.AgentClass) (bool, error) {
	if ac == nil {
		return false, fmt.Errorf("trifecta leg C: no AgentClass to judge")
	}
	toolkits, err := listToolkitsFor(ctx, c, ac)
	if err != nil {
		return false, fmt.Errorf("trifecta leg C: toolkits of %s/%s: %w", ac.Namespace, ac.Name, err)
	}
	var servers []spiceboxv1alpha1.MCPServer
	if len(ac.Spec.MCPServers) > 0 {
		servers, err = listMCPServersFor(ctx, c, ac)
		if err != nil {
			return false, fmt.Errorf("trifecta leg C: mcpServers of %s/%s: %w", ac.Namespace, ac.Name, err)
		}
	}
	// gatherSidecarToolboxes is the reconciler's own resolver and self-guards
	// on an empty ref list, so no caller-side gate is needed. Unlike the
	// reconcile path it runs here WITHOUT validateSidecarToolboxes ahead of it
	// — the tripper judges a running session, not an incoming spec — so a ref
	// that has since been deleted surfaces as the returned error rather than as
	// a quietly narrower walk.
	toolboxes, err := gatherSidecarToolboxes(ctx, c, ac.Namespace, ac.Spec.SidecarToolboxes)
	if err != nil {
		return false, fmt.Errorf("trifecta leg C: sidecarToolboxes of %s/%s: %w", ac.Namespace, ac.Name, err)
	}
	return len(ConsequentialHandles(servers, toolkits, toolboxes)) > 0, nil
}
