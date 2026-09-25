package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/authzed/openagentprimitives/pkg/memory"
	asc "github.com/authzed/openagentprimitives/pkg/memory/kinds/authz_session_config"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/coldstarttask"
)

// cold_start_policy.go answers the two questions a cold-start metaagent request
// must NOT be allowed to answer for itself.
//
// `ap.session.<ns>.<name>.in.metaagent_request` is a subject the runner's own
// per-session NATS grant permits (runnerNATSUserGrant), and core NATS gives the
// subscriber NO publisher identity — so every field on that wire is chosen by
// the publisher. A gate keyed off a request field (a `coldStart` that skips the
// manage_scope owner check, an `autoApply` that skips human approval) is
// therefore a gate any publisher can open by adding a field. Never reintroduce
// one.
//
// The authority is the AgentClass — `spec.authz.scope.enabled` and
// `spec.authz.scope.coldStart` — read through the session's
// authz_session_config snapshot, the only view of the class that authzd, which
// holds no Kubernetes client, can reach.
//
// That snapshot is K8s-witnessed, which is what makes reading it different IN
// KIND from reading the request. The AgentSession reconciler authors it from
// the AgentClass the operator itself Got from the API server, before the runner
// pod exists, and the Kind is ComponentWritten, so the memory facade's per-kind
// door refuses a per-session bearer the write. One per-session record, one
// writer, and that writer is the cluster.
//
// `session_scope` is deliberately NOT locked the same way: it has two
// legitimate writers, one being the runner binding class defaults at cold
// start, so it stays SessionWritten. It is not an input to either question this
// file answers.

// errColdStartConfigUnavailable means authzd could not read the session's
// authz_session_config snapshot, so it cannot establish the class's cold-start
// policy. Fail closed: a session whose scope policy is unknown must not run
// scoped.
var errColdStartConfigUnavailable = errors.New("cold-start policy: authz_session_config snapshot unavailable")

// errColdStartNotPermitted means the snapshot says the class does not run
// cold-start scope review at all (scope disabled, or coldStart off/unset). The
// runner never publishes in that state, so a request that arrives anyway did
// not come from the cold-start hook.
var errColdStartNotPermitted = errors.New("cold-start policy: class does not permit cold-start scope review")

// coldStartAutoApplyMode is the AgentClass.spec.authz.scope.coldStart value that
// waives the human approval gate.
const coldStartAutoApplyMode = "extractAndAutoApply"

// coldStartPolicy is the resolved, authzd-side answer to "how does this
// session's class run cold-start scope review?".
type coldStartPolicy struct {
	// autoApply waives the human approval gate (class coldStart ==
	// extractAndAutoApply). False for every other mode, and for every state in
	// which the mode could not be established.
	autoApply bool
}

// resolveColdStartPolicy derives the cold-start policy from the session's
// authz_session_config snapshot. It never consults the request payload.
//
// Both error returns are fail-closed outcomes the caller turns into a
// StatusScopeReviewFailed cold_start_task, which halts the runner rather than
// letting it run unscoped — the same disposition an extractor failure takes.
func resolveColdStartPolicy(ctx context.Context, mem memory.Memory, scopeRef memory.Scope) (coldStartPolicy, error) {
	cfg, found, err := asc.Get(ctx, mem, scopeRef)
	if err != nil {
		return coldStartPolicy{}, fmt.Errorf("%w: %w", errColdStartConfigUnavailable, err)
	}
	if !found {
		return coldStartPolicy{}, errColdStartConfigUnavailable
	}
	if !cfg.ScopeEnabled || cfg.ColdStart == "" || cfg.ColdStart == "off" {
		return coldStartPolicy{}, fmt.Errorf("%w (scopeEnabled=%t coldStart=%q)",
			errColdStartNotPermitted, cfg.ScopeEnabled, cfg.ColdStart)
	}
	return coldStartPolicy{autoApply: cfg.ColdStart == coldStartAutoApplyMode}, nil
}

// coldStartAlreadyDecided reports whether this session's cold-start review has
// already produced a decision.
//
// Cold start is a once-per-session capability: the runner publishes exactly one
// request, from the SessionStart hook, and blocks on the resulting
// cold_start_task. A second coldStart:true request therefore cannot be the
// cold-start hook, and honoring it would re-run the gate-skipping path — each
// pass applying a fresh scope delta and writing an authzd-signed
// metaagent_audit record attesting an approval that no human gave. A read error
// counts as "decided": authzd refuses rather than replaying on an unverifiable
// read.
func coldStartAlreadyDecided(ctx context.Context, mem memory.Memory, scopeRef memory.Scope) (bool, error) {
	_, found, err := coldstarttask.Get(ctx, mem, scopeRef)
	if err != nil {
		return true, fmt.Errorf("cold-start policy: read cold_start_task: %w", err)
	}
	return found, nil
}
