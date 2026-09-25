// RevokePublisher is the operator-side detective control pairing with the
// runner's in-process tool-origin invalidator cache: on every
// ClusterAgentSettings / AgentSettings reconcile it diffs
// spec.limits.allowedMCPServers and spec.limits.allowedToolkits against the
// previous per-CR observation and emits a "tool-origin" revocation on the
// unified ap.revocation bus for each explicit removal.
//
// Tri-state rule (documented here, enforced in Observe):
//
//	nil  → allow-all; NO emit on transition from nil (the universe cannot be
//	        enumerated; the next-session allow-all→allowlist gate covers it).
//	[]   → allow-none (deny-all).
//	[..] → allowlist.
//
// Emit rules (identical for both lists, applied independently):
//
//	old nil    → new anything    : NO emit   (widening or shrinking from allow-all)
//	old non-nil → new nil        : NO emit   (widening to allow-all)
//	old [A,B,X] → new [A,B]     : emit revoke for X
//	old [X]     → new []        : emit revoke for X
//	first observation of a CR   : prime, NO emit
//
// Emit keys (must match MCPTool.Origin() / SandboxTool.Origin()):
//
//	removed allowedMCPServers entry with .Name==X  → key "mcpserver/"+X
//	removed allowedToolkits entry T                 → key "toolkit/"+T
//
// Scopes:
//
//	ClusterReconciler → scope "" (cluster-wide)
//	NamespaceReconciler → scope = cr.Namespace
//
// # Where the trigger state lives
//
// The previous observation is a DURABLE record on the CR,
// status.observedLimits, with the in-process map in front of it only as a
// cache. That ordering is the whole point: NATS delivery is at-most-once with
// no fallback path for revocations, and the runner applies the allowlist once
// at pod boot, so for a session straddling the change this revoke is the ONLY
// invalidation path — far too important for a process-scoped map to hold the
// only copy. The durable record closes two failure modes:
//
//   - An entry withdrawn while the operator was DOWN. The successor process
//     has an empty map, seeds from status, and re-derives the diff instead of
//     priming past it — so priming is scoped to a CR nobody has ever observed,
//     not to every CR after every operator roll.
//   - A publish that FAILED. The failed origin stays in the recorded
//     allowlist, so the next reconcile re-derives the same removal and
//     re-emits. Observe reports the failure so the reconciler can requeue
//     rather than wait for the informer resync.
//
// Emit happens BEFORE the record advances, deliberately: a lost status write
// costs a duplicate revoke next reconcile, and re-revoking an already-revoked
// origin is a no-op, whereas advancing the record first loses the revocation
// entirely.
//
// Concurrent-safe; share one instance across a controller's reconciles.
package settings

import (
	"context"
	"errors"
	"sort"
	"sync"

	"github.com/go-logr/logr"
	"sigs.k8s.io/controller-runtime/pkg/log"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/revocation"
)

// toolOriginRevokeKind is the registered revocation.Invalidator kind the
// emitted revokes target — matches tool-origin invalidator kind.
const toolOriginRevokeKind = "tool-origin"

// settingsSnapshot is the per-CR last-seen concrete allowlist state. A nil map
// means the previous observed value was nil (allow-all); a non-nil one means a
// concrete list was observed, possibly empty.
type settingsSnapshot struct {
	// AllowedMCPServer names last observed; nil means allow-all.
	mcpServers map[string]struct{}

	// Toolkit names last observed; nil means allow-all.
	toolkits map[string]struct{}
}

// RevokePublisher diffs ClusterAgentSettings / AgentSettings allowlists
// against the last observation and emits tool-origin revokes on the unified
// ap.revocation bus for explicitly removed entries.
//
// Concurrent-safe; share one instance across a controller's reconciles.
type RevokePublisher struct {
	// Bus delivers revocations on the unified ap.revocation subject. Nil is
	// tolerated for an operator running without NATS: Emit no-ops on both a
	// nil-backed publisher and a nil receiver.
	Bus *revocation.Publisher

	mu sync.Mutex
	// observed caches the last-seen allowlists per CR key. A CACHE in front of
	// status.observedLimits, never the source of truth: a cold entry is seeded
	// from the CR's durable record.
	observed map[string]settingsSnapshot
}

// NewRevokePublisher constructs the publisher. A nil bus is tolerated
// for local-dev configurations.
func NewRevokePublisher(bus *revocation.Publisher) *RevokePublisher {
	return &RevokePublisher{
		Bus:      bus,
		observed: make(map[string]settingsSnapshot),
	}
}

// Observe is called from the ClusterReconciler and NamespaceReconciler
// after a successful CR load. It diffs the new allowlists against the
// last observation for this CR and emits tool-origin revokes for each
// explicitly removed entry.
//
// Parameters:
//   - mcpAllowed: the current spec.limits.allowedMCPServers (nil = allow-all).
//   - toolkitsAllowed: the current spec.limits.allowedToolkits (nil = allow-all).
//   - status: the CR's operator-owned status. Observe reads status.observedLimits
//     as the durable previous observation and stamps the advanced record back
//     onto it; the CALLER's status write is what persists it, so Observe must
//     run before that write.
//   - crKey: stable identifier for the CR, e.g. "namespace/name" or
//     "name" for cluster-scoped; the cache key and a log value.
//   - scope: revocation scope ("" for cluster, cr.Namespace for namespaced).
//
// The first observation of a CR — no cache entry and no durable record —
// primes without emitting.
//
// Returns a non-nil error when at least one revoke failed to publish. Each
// failure is also logged; the error exists so the reconciler can requeue,
// because the origins that failed are deliberately held in the recorded
// allowlist and only another reconcile can retry them.
func (p *RevokePublisher) Observe(
	ctx context.Context,
	mcpAllowed *[]spiceboxv1alpha1.AllowedMCPServer,
	toolkitsAllowed *[]string,
	status *spiceboxv1alpha1.SettingsStatus,
	crKey string,
	scope string,
) error {
	logger := log.FromContext(ctx).WithName("settings-revoke-publisher").WithValues("crKey", crKey, "scope", scope)
	p.mu.Lock()
	defer p.mu.Unlock()

	if status == nil {
		// Both production call sites pass the CR's own status. A nil here
		// means a caller wired the publisher without one, which silently
		// downgrades revocation to process-scoped state — say so rather than
		// panicking into controller-runtime's recover, which would hide it.
		logger.Info("settings revoke publisher: nil status; the tool-origin trigger state cannot be persisted and will not survive an operator restart")
	}

	current := buildSnapshot(mcpAllowed, toolkitsAllowed)
	prev, hadPrev := p.observed[crKey]
	if !hadPrev && status != nil {
		// Cold cache — a fresh process, or the first reconcile of this CR.
		// The durable record is the only thing that can say whether an origin
		// was withdrawn while nobody was watching.
		if durable, ok := snapshotFromStatus(status.ObservedLimits); ok {
			prev, hadPrev = durable, true
			logger.V(1).Info("seeded observed state from status.observedLimits")
		}
	}
	if !hadPrev {
		p.observed[crKey] = current
		stampObservedLimits(status, current)
		logger.V(1).Info("prime observed state; no emit")
		return nil
	}

	// heldMCP / heldToolkits are the origins whose revoke did not publish.
	// They stay in the recorded allowlist — both in the cache and in status —
	// so the next reconcile re-derives the identical removal and retries.
	heldMCP, mcpErr := p.diffAndEmit(ctx, logger, prev.mcpServers, current.mcpServers, "mcpserver/", scope, "allowedMCPServers")
	heldToolkits, tkErr := p.diffAndEmit(ctx, logger, prev.toolkits, current.toolkits, "toolkit/", scope, "allowedToolkits")

	next := settingsSnapshot{
		mcpServers: withHeldBack(current.mcpServers, heldMCP),
		toolkits:   withHeldBack(current.toolkits, heldToolkits),
	}
	p.observed[crKey] = next
	stampObservedLimits(status, next)
	return errors.Join(mcpErr, tkErr)
}

// diffAndEmit emits one tool-origin revoke per name present in prev and
// absent from current, and returns the names whose emit failed.
//
// Emitting is gated on both sides being non-nil (concrete lists): a nil on
// either side means allow-all, and the universe of origins an allow-all
// covers cannot be enumerated, so there is nothing to revoke against.
func (p *RevokePublisher) diffAndEmit(ctx context.Context, logger logr.Logger,
	prev, current map[string]struct{}, keyPrefix, scope, listName string) (map[string]struct{}, error) {
	if prev == nil || current == nil {
		return nil, nil
	}
	var held map[string]struct{}
	var errs []error
	for name := range prev {
		if _, stillThere := current[name]; stillThere {
			continue
		}
		key := keyPrefix + name
		if err := p.Bus.Emit(ctx, toolOriginRevokeKind, key, scope); err != nil {
			logger.Info("revoke publisher: emit failed; holding the origin in the recorded allowlist so the next reconcile retries",
				"err", err.Error(), "key", key, "list", listName)
			if held == nil {
				held = map[string]struct{}{}
			}
			held[name] = struct{}{}
			errs = append(errs, err)
			continue
		}
		logger.Info("tool-origin revoke published",
			"key", key, "reason", "removed from "+listName)
	}
	return held, errors.Join(errs...)
}

// withHeldBack returns current plus every name whose revoke failed, so the
// next reconcile sees the same removal again. held is only ever non-empty
// when current is non-nil (diffAndEmit refuses to emit otherwise), so the
// allow-all sentinel — a nil set — is never accidentally materialised.
func withHeldBack(current, held map[string]struct{}) map[string]struct{} {
	if len(held) == 0 {
		return current
	}
	out := make(map[string]struct{}, len(current)+len(held))
	for name := range current {
		out[name] = struct{}{}
	}
	for name := range held {
		out[name] = struct{}{}
	}
	return out
}

// snapshotFromStatus rebuilds the in-memory snapshot from the durable record.
// The bool reports whether a record existed at all: nil means the CR has
// never been observed, which is what makes the prime-without-emit decision.
func snapshotFromStatus(ol *spiceboxv1alpha1.ObservedSettingsLimits) (settingsSnapshot, bool) {
	if ol == nil {
		return settingsSnapshot{}, false
	}
	s := settingsSnapshot{}
	if ol.AllowedMCPServers != nil {
		s.mcpServers = nameSet(*ol.AllowedMCPServers)
	}
	if ol.AllowedToolkits != nil {
		s.toolkits = nameSet(*ol.AllowedToolkits)
	}
	return s, true
}

// stampObservedLimits records the snapshot on the CR's status for the
// caller's status write to persist. Names are sorted so an unchanged spec
// produces a byte-identical value and the write stays a no-op; a nil set
// stays a nil pointer (allow-all) while an empty set becomes a pointer to an
// empty list (deny-all), preserving the tri-state through JSON.
func stampObservedLimits(status *spiceboxv1alpha1.SettingsStatus, s settingsSnapshot) {
	if status == nil {
		return
	}
	out := &spiceboxv1alpha1.ObservedSettingsLimits{}
	if s.mcpServers != nil {
		out.AllowedMCPServers = sortedNames(s.mcpServers)
	}
	if s.toolkits != nil {
		out.AllowedToolkits = sortedNames(s.toolkits)
	}
	status.ObservedLimits = out
}

// sortedNames renders a set as a pointer to a sorted slice. An empty set
// yields a pointer to an empty slice, never nil — nil is the allow-all
// sentinel and means something different.
func sortedNames(set map[string]struct{}) *[]string {
	out := make([]string, 0, len(set))
	for name := range set {
		out = append(out, name)
	}
	sort.Strings(out)
	return &out
}

// nameSet is the inverse of sortedNames.
func nameSet(names []string) map[string]struct{} {
	out := make(map[string]struct{}, len(names))
	for _, n := range names {
		out[n] = struct{}{}
	}
	return out
}

// buildSnapshot converts the two allowlist pointers to an internal settingsSnapshot.
// A nil pointer → nil set (allow-all, no diff tracking).
// A non-nil pointer → a set of names (possibly empty for deny-all).
func buildSnapshot(
	mcpAllowed *[]spiceboxv1alpha1.AllowedMCPServer,
	toolkitsAllowed *[]string,
) settingsSnapshot {
	s := settingsSnapshot{}
	if mcpAllowed != nil {
		s.mcpServers = make(map[string]struct{}, len(*mcpAllowed))
		for _, entry := range *mcpAllowed {
			s.mcpServers[entry.Name] = struct{}{}
		}
	}
	if toolkitsAllowed != nil {
		s.toolkits = make(map[string]struct{}, len(*toolkitsAllowed))
		for _, name := range *toolkitsAllowed {
			s.toolkits[name] = struct{}{}
		}
	}
	return s
}
