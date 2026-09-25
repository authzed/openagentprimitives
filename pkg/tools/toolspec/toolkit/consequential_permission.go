package toolkit

import (
	"fmt"

	"github.com/authzed/openagentprimitives/pkg/authz"
)

// terminalAllowImpacts are the StateImpacts every gate treats as a decided
// ALLOW: the tool checker returns Allowed with no SpiceDB call, permsurface
// scores them severity 0 and mints no handle, so nothing enters the permission
// surface, nothing is declarable in a plan, and nothing renders on an approval
// card. The plan gate states it directly — a call with no handle is not a
// plan-gate concern.
var terminalAllowImpacts = map[authz.StateImpact]bool{
	authz.Passthrough: true,
	authz.Stateless:   true,
}

// validateConsequentialPermission refuses a subcommand that declares
// CONSEQUENCE and resolves to a terminal allow.
//
// kubectl shipped exactly that shape: one toolkit-wide `stateImpact:
// passthrough`, inherited by every subcommand, over an `apply` and a `delete`
// the same file marks destructive: true. The YAML contradicted itself and
// nothing read it, so `kubectl apply -f -` with a cluster-admin
// ClusterRoleBinding on stdin was allowed with no check, no card and no audit
// handle — while the identical mutation through gh or git is external and
// re-approved per call.
//
// The rule reads the two declarations a subcommand already makes about itself.
// It is deliberately structural rather than a list of dangerous subcommand
// names: a name list is a thing to keep up to date, and this is a
// contradiction the file states in its own terms.
func validateConsequentialPermission(tk *Toolkit, sc *Subcommand) error {
	// The effective permission: the subcommand's own override, else the
	// toolkit default. An absent default is not a terminal allow — it is no
	// declaration at all, which resolution handles separately.
	perm := sc.Permission
	if perm == nil {
		perm = tk.Permission
	}
	if perm == nil || !terminalAllowImpacts[perm.StateImpact] {
		return nil
	}

	// ONE declaration counts: destructive. It is the file saying, in its own
	// unambiguous words, that this subcommand has consequence — no
	// interpretation, nothing for a future author to argue about.
	//
	// effects.writes is deliberately NOT a trigger, and the reason is that its
	// categories do not mean what the name suggests. A filesystem write is
	// usually confined to the call's own workspace, where the sandbox is
	// already the boundary; and "network" in these toolkits routinely means
	// "talks to its own daemon or API server" — `docker pull`, `docker tag`
	// and `docker build` all declare a network write and none of them is an
	// action a human should clear per call. Triggering on writes would fire on
	// the safe majority, and a rule that fires on the safe majority gets
	// switched off.
	//
	// The consequence of that choice: a subcommand that IS dangerous and
	// declares destructive: false is not caught here. That is a defect in the
	// declaration, and the place to fix it is the declaration.
	if !sc.Effects.Destructive {
		return nil
	}
	why := "declares effects.destructive: true"
	source := "its own permission block"
	if sc.Permission == nil {
		source = "the toolkit-wide permission default"
	}
	return fmt.Errorf(
		"%s but resolves to stateImpact %q via %s. %q is a terminal allow at every gate: "+
			"no authorization check runs, no handle is minted, and the call is invisible to the "+
			"plan gate and to every approval card. Give this subcommand its own permission block "+
			"— readonly/readwrite for an authorization-checked action, external for one a human "+
			"must clear per call",
		why, perm.StateImpact, source, perm.StateImpact)
}
