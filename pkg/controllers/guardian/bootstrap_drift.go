// pkg/controllers/guardian/bootstrap_drift.go
//
// Read-back drift detection for SpiceDBBootstrap. The reconciler's normal
// sync path (bootstrap_sync.go, bootstrap_refcount.go) only ever compares
// what THIS PROCESS believes it wrote (r.lastDesired) against what it wants
// to write next (newDesired) — it never looks at what SpiceDB actually
// holds. A desired tuple that silently vanished from the datastore stays
// absent forever, because the desired set didn't change so no diff gets
// computed for it. A tuple some other writer landed on a relation this
// surface manages is likewise invisible: nothing ever compared against it.
//
// This file closes that gap by reading back, at full consistency, every
// (resourceType, resourceID, relation) triple the desired set claims, and
// diffing both directions. It is observation only — see the doc comment on
// detectBootstrapDrift for why it is structurally incapable of acting on
// what it finds.
//
// Known blind spot this closes only PART of the gap it states: drift that
// happens WHILE THE OPERATOR IS DOWN (a restart, an upgrade, an eviction, an
// OOM kill) is invisible across that restart, and gets silently repaired
// with no event and no log, not merely undetected. NewReconciler starts
// r.lastDesired empty (agentsessiongrants_controller.go), and
// seedPriorClaimsForDeleting only recovers claims for a CR that is already
// mid-deletion (bootstrap_sync.go) — a live CR's claims are not recovered.
// So on the first reconcile after any restart, every live tuple is absent
// from alreadyKnown, which detectBootstrapDrift's own "freshly claimed this
// pass" rule exempts from Missing regardless of whether SpiceDB actually
// still holds it; the ordinary sync path (ComputeDiff -> applyTouches) then
// re-TOUCHes everything and read-back is clean again by the very next pass.
// A tuple some out-of-band actor deleted during the downtime is repaired
// this way with nothing ever reported. Deliberately not fixed here: seeding
// r.lastDesired from a read-back at startup would be a different design that
// has not been reviewed — it would need to decide what a read-back finding
// UNEXPECTED tuples at boot means (a legitimate other writer vs. drift to
// report) before it could safely seed anything. If closing this blind spot
// becomes a requirement, that is the shape to design, not a patch on this
// file's exemption logic.
package guardian

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	v1 "github.com/authzed/authzed-go/proto/authzed/api/v1"
	"github.com/go-logr/logr"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/authzed/openagentprimitives/pkg/authz/spicedb"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
)

// Reader is the minimal SpiceDB read surface drift detection needs — a
// read-back following (*spicedb.Client).ListDeniedUsers's shape and
// consistency level (pkg/authz/spicedb/client.go). Declared here, in the
// guardian package, rather than widened onto spicedb.BootstrapWriter: an
// earlier task on this branch deliberately kept reads off the writer
// interfaces, and a writer interface that also carries reads would muddy
// exactly that line. *spicedb.Client already satisfies this structurally
// via its own ReadRelationships method — no changes to pkg/authz/spicedb
// are needed to wire it in.
type Reader interface {
	ReadRelationships(ctx context.Context, in *v1.ReadRelationshipsRequest) (v1.PermissionsService_ReadRelationshipsClient, error)
}

// driftReport is what one drift-detection pass found. Missing is every
// desired tuple that read-back did not find in SpiceDB; Unexpected is every
// tuple read-back found on a managed relation that nothing in the desired
// set claims. Both fields are exported so tests outside this package can
// assert on them directly (via DetectBootstrapDriftForTest in
// export_test.go); the type itself stays unexported because nothing
// outside this package constructs or consumes one — Reconcile only ever
// reports it (publishDrift below), as a structured monitoring event when a
// publisher is configured or a log line otherwise, without needing this
// type to be public.
type driftReport struct {
	Missing    []spicedb.Tuple
	Unexpected []spicedb.Tuple
}

// Empty reports whether the pass found no drift in either direction.
func (d driftReport) Empty() bool {
	return len(d.Missing) == 0 && len(d.Unexpected) == 0
}

// managedRelation is one distinct (resourceType, resourceID, relation)
// triple the desired set claims at least one tuple on.
type managedRelation struct {
	resourceType string
	resourceID   string
	relation     string
}

// managedRelations returns the distinct triples d's tuples span, sorted for
// a deterministic read order (stable logs, stable tests). This is the
// bound that keeps drift detection from ever reading a relation the
// surface does not manage: the read set is a pure function of the desired
// map's own contents, nothing broader.
func (d *DesiredMap) managedRelations() []managedRelation {
	seen := map[managedRelation]struct{}{}
	var out []managedRelation
	for _, t := range d.tuples {
		mr := managedRelation{resourceType: t.ResourceType, resourceID: t.ResourceID, relation: t.Relation}
		if _, ok := seen[mr]; ok {
			continue
		}
		seen[mr] = struct{}{}
		out = append(out, mr)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].resourceType != out[j].resourceType {
			return out[i].resourceType < out[j].resourceType
		}
		if out[i].resourceID != out[j].resourceID {
			return out[i].resourceID < out[j].resourceID
		}
		return out[i].relation < out[j].relation
	})
	return out
}

// readManagedRelation reads every tuple SpiceDB currently holds for one
// (resourceType, resourceID, relation) triple, at full consistency — the
// same level ListDeniedUsers uses, for the same reason: a drift check
// racing a write it should have observed is worse than a slower one.
func readManagedRelation(ctx context.Context, r Reader, mr managedRelation) ([]spicedb.Tuple, error) {
	stream, err := r.ReadRelationships(ctx, &v1.ReadRelationshipsRequest{
		Consistency: &v1.Consistency{Requirement: &v1.Consistency_FullyConsistent{FullyConsistent: true}},
		RelationshipFilter: &v1.RelationshipFilter{
			ResourceType:       mr.resourceType,
			OptionalResourceId: mr.resourceID,
			OptionalRelation:   mr.relation,
		},
	})
	if err != nil {
		return nil, err
	}
	var out []spicedb.Tuple
	for {
		resp, rerr := stream.Recv()
		if errors.Is(rerr, io.EOF) {
			break
		}
		if rerr != nil {
			return nil, rerr
		}
		rel := resp.GetRelationship()
		subj := rel.GetSubject()
		out = append(out, spicedb.Tuple{
			ResourceType:    rel.GetResource().GetObjectType(),
			ResourceID:      rel.GetResource().GetObjectId(),
			Relation:        rel.GetRelation(),
			SubjectType:     subj.GetObject().GetObjectType(),
			SubjectID:       subj.GetObject().GetObjectId(),
			SubjectRelation: subj.GetOptionalRelation(),
		})
	}
	return out, nil
}

// detectBootstrapDrift reads back, at full consistency, every relation the
// desired set claims and diffs it against what SpiceDB actually holds.
//
// Deliberately takes no writer of any kind — it never references r.Writer —
// so it is structurally incapable of acting on what it finds. A missing
// tuple is re-TOUCHed by the ordinary sync path (bootstrap_sync.go /
// ComputeDiff) the next reconcile that sees it in a diff, not here; an
// unexpected tuple is never deleted by this surface at all — deleting on
// suspicion is the unsafe behaviour this design refuses. See
// bootstrap_drift_test.go's TestDrift_IssuesNoWrites, which is the
// regression test for a future change that tries to wire this report into
// applyTouches/applyDeletes.
//
// A nil r.Reader means drift detection is not configured (mirrors r.Writer's
// nil-tolerant contract): logged once here rather than silently, then
// reports nothing. r.Reader is declared as the Reader interface type
// throughout — never a concrete *spicedb.Client — so this comparison
// against nil is a genuine nil-interface check, not the typed-nil trap
// AGENTS.md documents against this exact file's Writer field: a
// `!= nil` guard over an interface holding a typed-nil pointer would
// return true and panic on the first method call, not degrade gracefully.
//
// A read-back failure on one relation is logged and that relation's tuples
// are skipped from the report (neither confirmed present nor reported
// missing — unread means unknown) — never fatal to the pass. This method
// has no error return at all, so a broken read literally cannot change
// Reconcile's outcome: no error propagates, no requeue is forced, no
// condition is flipped.
//
// alreadyKnown names the tuples this process has previously converged (in
// production, r.lastDesired as it stood at the START of this reconcile,
// before this pass's applyTouches runs). A tuple in `desired` that is
// ABSENT from alreadyKnown is a brand-new claim — nothing has ever TOUCHed
// it yet, so reading it back absent is the expected state of a healthy
// surface, not drift, and it is excluded from report.Missing (Unexpected is
// unaffected: it is computed from what read-back finds on a MANAGED
// relation regardless of which specific tuples are new). Without this, the
// first reconcile of any newly-created SpiceDBBootstrap — or every
// bootstrap tuple at once, on a fresh install — reads back 100% absent and
// reports a warning for a surface that is converging normally; the ordinary
// sync path (ComputeDiff → applyTouches) is about to write every one of
// those tuples this same reconcile regardless of what this call finds. This
// SAME exemption is what makes the operator-downtime blind spot this file's
// package doc names: a restart's first reconcile can't tell "brand-new
// claim" from "a claim this process converged before the restart, now
// possibly gone" — alreadyKnown is empty either way.
//
// alreadyKnown == nil means the caller supplied no convergence history at
// all — every unit test in bootstrap_drift_test.go that exercises this
// method directly via DetectBootstrapDriftForTest does exactly this, and
// gets the ORIGINAL behaviour: every absent-on-read-back tuple is
// reportable, full stop. This is deliberate, not an oversight: NewReconciler
// already initializes r.lastDesired to an EMPTY (non-nil) map, so "nil" and
// "empty" must mean different things here — nil is "the caller isn't testing
// this exemption", empty is "this process has genuinely converged nothing
// yet", and only the latter should exempt every tuple in `desired`.
func (r *Reconciler) detectBootstrapDrift(ctx context.Context, desired, alreadyKnown *DesiredMap) driftReport {
	var report driftReport
	logger := log.FromContext(ctx).WithName("guardian.bootstrap.drift")
	if r.Reader == nil {
		logger.Info("bootstrap drift detection skipped: no Reader configured")
		return report
	}

	relations := desired.managedRelations()
	actualKeys := map[string]struct{}{}
	succeeded := map[managedRelation]bool{}
	for _, mr := range relations {
		actual, err := readManagedRelation(ctx, r.Reader, mr)
		if err != nil {
			logger.Info("bootstrap drift read-back failed; skipping this relation",
				"resourceType", mr.resourceType, "resourceID", mr.resourceID,
				"relation", mr.relation, "err", err.Error())
			continue
		}
		succeeded[mr] = true
		for _, t := range actual {
			actualKeys[t.Key()] = struct{}{}
			if !desired.Has(t.Key()) {
				report.Unexpected = append(report.Unexpected, t)
			}
		}
	}

	for key, t := range desired.tuples {
		mr := managedRelation{resourceType: t.ResourceType, resourceID: t.ResourceID, relation: t.Relation}
		if !succeeded[mr] {
			continue // read failed for this relation: unknown, not reported as missing
		}
		if _, ok := actualKeys[key]; ok {
			continue // present: not missing
		}
		if alreadyKnown != nil && !alreadyKnown.Has(key) {
			continue // freshly claimed this pass — not yet written, so absence is expected, not drift
		}
		report.Missing = append(report.Missing, t)
	}

	sort.Slice(report.Missing, func(i, j int) bool { return report.Missing[i].Key() < report.Missing[j].Key() })
	sort.Slice(report.Unexpected, func(i, j int) bool { return report.Unexpected[i].Key() < report.Unexpected[j].Key() })

	if !report.Empty() {
		r.publishDrift(logger, report)
	}
	return report
}

// driftSampleSize bounds the sample carried by both sinks below. An
// unbounded sample of a large drift would flood the log the same way it
// would flood the monitoring bus — the count (len(report.Missing) /
// len(report.Unexpected)) always reflects the true total regardless of how
// much of the sample is shown.
const driftSampleSize = 5

// tupleSample is one drift tuple's fields, decomposed rather than packed
// into Tuple.Key()'s single "type:id#relation@subjType:subjID#subjRelation"
// string — the same reason readManagedRelation's read-failure log a few
// lines above logs "resourceType"/"resourceID"/"relation" as separate
// key/value pairs instead of one composite string: a structured-log query
// can filter on a field name only when the field actually arrives
// decomposed.
type tupleSample struct {
	ResourceType    string `json:"resourceType"`
	ResourceID      string `json:"resourceID"`
	Relation        string `json:"relation"`
	SubjectType     string `json:"subjectType"`
	SubjectID       string `json:"subjectID"`
	SubjectRelation string `json:"subjectRelation,omitempty"`
}

// sampleTuples decomposes at most the first n tuples (report.Missing and
// report.Unexpected are sorted before this is called, so the sample is
// deterministic) into tupleSample — the bounded sample shared by the log
// fallback and the monitoring event's Summary.
func sampleTuples(tuples []spicedb.Tuple, n int) []tupleSample {
	if len(tuples) < n {
		n = len(tuples)
	}
	out := make([]tupleSample, 0, n)
	for i := 0; i < n; i++ {
		t := tuples[i]
		out = append(out, tupleSample{
			ResourceType: t.ResourceType, ResourceID: t.ResourceID, Relation: t.Relation,
			SubjectType: t.SubjectType, SubjectID: t.SubjectID, SubjectRelation: t.SubjectRelation,
		})
	}
	return out
}

// formatSample renders a decomposed sample as labeled field=value pairs
// (not Tuple.Key()'s packed string) for the monitoring event's human-
// readable Summary.
func formatSample(sample []tupleSample) string {
	if len(sample) == 0 {
		return "none"
	}
	parts := make([]string, len(sample))
	for i, s := range sample {
		part := fmt.Sprintf("resourceType=%s resourceID=%s relation=%s subjectType=%s subjectID=%s",
			s.ResourceType, s.ResourceID, s.Relation, s.SubjectType, s.SubjectID)
		if s.SubjectRelation != "" {
			part += fmt.Sprintf(" subjectRelation=%s", s.SubjectRelation)
		}
		parts[i] = part
	}
	return strings.Join(parts, "; ")
}

// now returns the drift monitoring event's clock, defaulting to time.Now
// when unset. r.clock is a test seam only — production never assigns it.
func (r *Reconciler) now() time.Time {
	if r.clock != nil {
		return r.clock()
	}
	return time.Now()
}

// publishDrift is the sink for a non-empty drift report — the caller
// (detectBootstrapDrift) already checked report.Empty(), so nothing here
// runs on a clean pass. It renders a bounded, decomposed sample of both
// directions (driftSampleSize), then either publishes a structured
// MonitoringEvent (when a publisher is configured) or logs the same counts
// and sample (when it is not) — mirroring
// pkg/controllers/agentsession/controller.go's maybeEmitUnschedulable and
// pkg/controllers/useridentity/attested_edge.go's publishAttestedConflict.
//
// Report-only, same as detectBootstrapDrift itself: this has no error
// return and never influences Reconcile's outcome. A publish failure is
// logged, never swallowed (AGENTS.md's no-silent-errors rule) — the event
// not reaching monitoring must never look like a clean pass in the logs.
func (r *Reconciler) publishDrift(logger logr.Logger, report driftReport) {
	missingSample := sampleTuples(report.Missing, driftSampleSize)
	unexpectedSample := sampleTuples(report.Unexpected, driftSampleSize)

	if r.MonitoringPublish == nil {
		logger.Info("bootstrap drift detected; not reported to monitoring (no publisher configured)",
			"missing", len(report.Missing),
			"unexpected", len(report.Unexpected),
			"missingSample", missingSample,
			"unexpectedSample", unexpectedSample,
		)
		return
	}

	ev := channelevents.MonitoringEvent{
		Level:      channelevents.MonitoringLevelWarning,
		Category:   "reconcile",
		Transition: channelevents.MonitoringTransitionFailed,
		Source: channelevents.MonitoringSourceRef{
			// Drift is not attributable to any single SpiceDBBootstrap — see
			// driftReport's doc comment above and
			// TestDrift_StampsNoConditionOnAnyBootstrap: an unexpected tuple
			// could have come from anywhere, and a missing one may have been
			// claimed by several CRs at once. Kind/Name therefore name the
			// DETECTION PASS itself, never a bootstrap CR: "SpiceDBBootstrapDrift
			// Detector" is not a registered CRD kind and "cluster" is not a real
			// object name, so neither string can be mistaken for — or "fixed" by
			// substituting — an actual SpiceDBBootstrap.
			Kind: "SpiceDBBootstrapDriftDetector",
			Name: "cluster",
		},
		Condition: "BootstrapDrift",
		Reason:    "TupleDrift",
		Summary: fmt.Sprintf(
			"SpiceDBBootstrap read-back found %d desired tuple(s) missing from SpiceDB (sample: %s) and "+
				"%d tuple(s) present on a surface-managed relation but claimed by no desired tuple (sample: %s)",
			len(report.Missing), formatSample(missingSample),
			len(report.Unexpected), formatSample(unexpectedSample),
		),
		Timestamp: r.now(),
	}
	if err := channelevents.PublishMonitoring(r.MonitoringPublish, ev); err != nil {
		logger.Info("publish bootstrap drift monitoring event failed",
			"missing", len(report.Missing),
			"unexpected", len(report.Unexpected),
			"err", err.Error())
	}
}
