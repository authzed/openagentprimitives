// Package workspace defines the Snapshotter interface for capturing
// and restoring AgentSession workspace PVC contents.
//
// The interface anticipates swap-out: the default impl is cp-by-pod
// (a one-shot Job mounting source + snapshot-store, running
// `cp -a --reflink=auto`). A future csi-snapshot impl can drop in
// behind the same interface for clusters that prefer native CSI
// VolumeSnapshot.
//
// Selected at operator start-up by DI; consumers in
// pkg/controllers/toolcall and the AgentSession controller depend
// on the interface only.
package workspace

import (
	"context"
	"fmt"
	"hash/fnv"
	"strings"
)

// Snapshotter captures and restores PVC contents at session-scoped
// handles. All methods are idempotent: re-Snapshot at the same handle
// is a no-op; Restore against a non-existent handle returns
// ErrSnapshotNotFound.
//
// Snapshot/Restore return at Job *creation*, not completion — the
// underlying copy runs asynchronously. Callers that must sequence on
// the copy actually finishing (the restart reconciler: a restore must
// not start before its snapshot completed, and the parent must not be
// superseded before the restore completed) poll SnapshotDone /
// RestoreDone.
type Snapshotter interface {
	Snapshot(ctx context.Context, src PVCRef, h SnapshotHandle) error
	Restore(ctx context.Context, h SnapshotHandle, dst PVCRef) error
	GC(ctx context.Context, h SnapshotHandle) error
	// SnapshotDone reports whether the snapshot Job for h has finished.
	// (false, nil) = still running; (true, nil) = Complete;
	// ErrSnapshotJobFailed (wrapped, with the Job's failure message) =
	// terminal failure; ErrSnapshotNotFound (wrapped) = the Job does
	// not exist. src carries the same namespace resolution as Snapshot.
	SnapshotDone(ctx context.Context, src PVCRef, h SnapshotHandle) (bool, error)
	// RestoreDone: same contract for the restore Job of (h, dst).
	RestoreDone(ctx context.Context, h SnapshotHandle, dst PVCRef) (bool, error)
}

// PVCRef is a namespace-scoped PVC reference. Treated as opaque by
// callers; impls dispatch by mounting both src and dst in a single
// Job.
type PVCRef struct {
	Namespace string
	Name      string
}

func (p PVCRef) String() string { return p.Namespace + "/" + p.Name }

// SnapshotHandle uniquely identifies a snapshot within a session. All three
// fields are required for ID. The runner emits one snapshot per stateful tool
// dispatch, so (TurnIndex, Sequence) is the natural ordering.
type SnapshotHandle struct {
	SessionUID string
	TurnIndex  int
	Sequence   int
	// Qualifier is an optional extra segment distinguishing ad-hoc
	// snapshots that share (SessionUID, TurnIndex); empty for JIT turn
	// snapshots. The CLEAN-fork path sets it to the child session's
	// name so a second fork of the same parent gets a fresh snapshot
	// instead of silently restoring the first fork's stale one, while a
	// same-fork replay (same child name → same handle) stays idempotent.
	// Sanitized (lowercase, [a-z0-9-], ≤40 chars) before use in Job
	// names, labels, and path segments.
	Qualifier string
	// SnapshotStorePVC is the RWX PVC where this snapshot's
	// subdirectory lives. Set by the caller so the same Snapshotter
	// instance can serve handles across many AgentSessions. Required
	// by cp-by-pod's Snapshot/Restore/GC. When empty, CPByPod falls
	// back to CPByPodConfig.SnapshotStorePVC.
	SnapshotStorePVC string
}

// turnSegment renders the handle's (TurnIndex, Sequence, Qualifier) as a
// metadata-safe segment used in snapshot paths, Job names, and Job labels.
//
// A negative TurnIndex is the ad-hoc clean-fork marker (see
// agentsession.RestoreOrCloneBundlePVCs), rendered as "adhoc" because the raw
// "%06d" of -1 ("-00001") is an INVALID k8s label value — the apiserver rejects
// the snapshot Job on every reconcile, wedging inherit-restarts.
//
// A non-empty Qualifier is folded in (sanitized) so two handles sharing
// (SessionUID, TurnIndex, Sequence) but belonging to different forks yield
// distinct paths and Job names. Without one the output keeps the plain
// two-number format, so existing snapshot paths stay resolvable.
func (h SnapshotHandle) turnSegment() string {
	q := sanitizeQualifier(h.Qualifier)
	if h.TurnIndex < 0 {
		if q != "" {
			return fmt.Sprintf("adhoc-%s-%03d", q, h.Sequence)
		}
		return fmt.Sprintf("adhoc-%03d", h.Sequence)
	}
	if q != "" {
		return fmt.Sprintf("%06d-%s-%03d", h.TurnIndex, q, h.Sequence)
	}
	return fmt.Sprintf("%06d-%03d", h.TurnIndex, h.Sequence)
}

// sanitizeQualifier renders q safe for embedding in Job names (DNS-1123
// subdomains), label values, and on-disk path segments: lowercased,
// restricted to [a-z0-9-] (other runes dropped), bounded to 40 chars.
// An all-invalid qualifier sanitizes to "" and is treated as absent.
//
// Over-length qualifiers are NOT prefix-truncated: session names put the fork
// discriminator at the END (…-fk<6hex>, and takeover-of-takeover names reach
// 40 chars), so a prefix would collapse two distinct forks onto one snapshot
// identity. The output instead folds in a deterministic hash of the FULL
// sanitized qualifier — first 24 chars + "-" + 8 hex of fnv64a — which stays
// [a-z0-9-], stable across calls, and distinct for distinct inputs.
func sanitizeQualifier(q string) string {
	q = strings.ToLower(q)
	var b strings.Builder
	for _, r := range q {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			b.WriteRune(r)
		}
	}
	s := b.String()
	if len(s) <= 40 {
		return s
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte(s)) // hash.Hash Write never errors
	return s[:24] + "-" + fmt.Sprintf("%016x", h.Sum64())[:8]
}

// PathSegment is the on-disk subdirectory layout under the snapshot
// store. Zero-padded indices keep lex order aligned with
// (TurnIndex, Sequence) order.
func (h SnapshotHandle) PathSegment() string {
	return fmt.Sprintf("%s/%s", h.SessionUID, h.turnSegment())
}

// ErrSnapshotNotFound is returned by Restore when the handle's underlying
// snapshot is absent. Callers translate it to a user-visible error.
var ErrSnapshotNotFound = fmt.Errorf("workspace: snapshot not found")

// ErrSnapshotJobFailed is returned by SnapshotDone/RestoreDone when the
// underlying Job failed terminally (JobFailed condition true, backoff
// exhausted). Callers treat it as permanent: retrying cannot succeed
// without operator intervention.
var ErrSnapshotJobFailed = fmt.Errorf("workspace: snapshot/restore job failed")
