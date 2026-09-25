package plangate

import (
	"context"
	"fmt"
	"strings"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/plangateaudit"
)

// HasInheritedCeiling reports whether scope's plan-gate log holds a root
// PROJECTED FROM A PARENT — a ceiling this session did not ask for and cannot
// widen, written by WriteChildRoot at delegation time.
//
// It is keyed on the root's DelegatedFrom field rather than on the presence of
// an EventPlanApproved record, because a session that ran its own gate has those
// too: conflating them would flip the mode of any session whose class
// deliberately runs the gate in logging mode.
//
// A read failure is returned, never folded into false. Answering "inherited
// nothing" on an outage would silently disable the inherited gate, which is
// precisely the failure this control exists to prevent.
func HasInheritedCeiling(ctx context.Context, mem memory.Memory, scope memory.Scope) (bool, error) {
	if mem == nil {
		return false, nil
	}
	records, err := plangateaudit.List(ctx, mem, scope)
	if err != nil {
		return false, fmt.Errorf("plangate: read plan-gate log for inherited ceiling: %w", err)
	}
	for _, r := range records {
		if r.Event != plangateaudit.EventPlanApproved {
			continue
		}
		if r.DelegatedFrom != "" {
			return true, nil
		}
		// Compatibility with roots written before DelegatedFrom existed. This
		// log is append-only, so a session already delegated when the field
		// landed keeps a root that carries the same fact only in prose — and
		// reading it as "not inherited" would leave exactly those sessions
		// ungated, which is the bug rather than a tidy-up of it. Same shape as
		// the fold's PhaseKey → (PlanDigest, PhaseIndex) fallback.
		//
		// The FIELD stays authoritative for everything written from here on;
		// this is a floor under records that predate it, not a second spelling.
		if strings.HasPrefix(r.Provenance, legacyDelegatedProvenancePrefix) {
			return true, nil
		}
	}
	return false, nil
}

// legacyDelegatedProvenancePrefix is the opening of the Provenance sentence
// DeriveForChild writes ("delegated from <parent> phase(s) …"). Matched only as
// a fallback for pre-DelegatedFrom records; see HasInheritedCeiling.
const legacyDelegatedProvenancePrefix = "delegated from "

// EffectiveMode is the plan-gate mode a session actually runs: its class's
// resolved mode, RAISED to enforcing when the session holds an inherited
// ceiling.
//
// The ceiling a parent projects into a child is written unconditionally, but it
// is inert data unless the child's runner builds a gate — and that build reads
// the child CLASS's own mode. A parent under an enforcing gate delegating to a
// class with the gate disabled therefore handed its child MORE reach than the
// parent held, with nothing downstream to bound it: the tool authz check is
// per-call against the child's own surface and can itself be permissive, and
// session scope is per-class. The only other lever is a cluster or namespace
// floor, which is operator discipline in a different object, not a parent-child
// tie.
//
// Raising rather than replacing: a class already at enforcing stays there, and
// a session that inherited nothing keeps exactly the mode its class chose, so
// an ordinary session is never force-enforced with no plan (which would deny
// everything).
//
// It lives here, in one exported function, because there are TWO wiring sites —
// the runner binary and the in-process e2e harness, which builds its own Loop —
// and a rule spelled at each is a rule one of them will omit.
func EffectiveMode(classMode string, inherited bool) string {
	if inherited {
		return ModeEnforcing
	}
	return classMode
}
