package agentsession

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/tool_dispatch_snapshot"
	"github.com/authzed/openagentprimitives/pkg/platform/workspace"
)

// AnalyzePostCut inspects the tool_dispatch_snapshot audit entries
// for a session and returns:
//   - affected: bundle session names that had ≥1 stateful dispatch
//     after cutTurn (those need a PVC restore at fork time)
//   - snapshots: per-bundle, the SnapshotHandle to restore — the
//     earliest snapshot taken after cutTurn (i.e., the snapshot
//     before the bundle's first post-cut stateful tool call)
//
// Returns (nil, empty map) when entries is nil or no entries fall
// after the cut.
func AnalyzePostCut(entries []tool_dispatch_snapshot.Content, cutTurn int) (affected []string, snapshots map[string]*workspace.SnapshotHandle) {
	snapshots = map[string]*workspace.SnapshotHandle{}
	for _, e := range entries {
		if e.TurnIndex <= cutTurn {
			continue
		}
		cur, ok := snapshots[e.SpiceboxSession]
		candidate := workspace.SnapshotHandle{
			SessionUID: e.SessionUID,
			TurnIndex:  e.TurnIndex,
			Sequence:   e.Sequence,
		}
		if !ok || lessHandle(candidate, *cur) {
			c := candidate
			snapshots[e.SpiceboxSession] = &c
		}
	}
	for bundle := range snapshots {
		affected = append(affected, bundle)
	}
	return affected, snapshots
}

func lessHandle(a, b workspace.SnapshotHandle) bool {
	if a.TurnIndex != b.TurnIndex {
		return a.TurnIndex < b.TurnIndex
	}
	return a.Sequence < b.Sequence
}

// BuildChildSession constructs the AgentSession the restart
// reconciler will create as the parent's successor. Carries forward
// the parent's class, agentIdentity, and BOTH channel bindings — input and
// output (each with InheritFrom set to the parent). A split-channel session
// (github input + slack output, reviewbot's shape) delivers through its
// OutputChannel, so dropping it would leave the child with no human-readable
// surface. Pure: no K8s reads.
//
// Thread handling depends on pr.Mode and applies to both bindings alike. A
// continuation-inherit fork (PendingRestartModeInherit) keeps thread_ts and the
// channel-key labels as-is: the reply lands where the user is already reading. A
// restart-from-here fork (-fk) branches into a new thread — each binding's
// External has thread_ts stripped so the Slack sender posts a new top-level
// message (becoming the new thread root), and LabelChannelKey /
// LabelOutputChannelKey are cleared — they hash the old thread_ts and would
// mismatch; the sender's first-send capture patches the correct labels once the
// new thread root ts is known.
//
// The AnnotationForkedFromThread annotation is stamped on a branching fork when
// the parent's OUTBOUND binding (OutputChannel if set, else InputChannel — the
// thread the human reads) carries Slack-style team_id + channel_id + thread_ts.
// The Slack sender reads this annotation to post a notice in the parent thread
// and an ephemeral in the new thread after first-send captures the new thread
// root. An inherit fork never stamps it — it has no old thread to link back to.
func BuildChildSession(parent *spiceboxv1alpha1.AgentSession, pr *spiceboxv1alpha1.PendingRestart) *spiceboxv1alpha1.AgentSession {
	cb := parent.Spec.InputChannel
	// A continuation-inherit fork resumes the SAME conversation, so it keeps the
	// parent's thread anchor and channel key: the reply lands where the user is
	// already reading. Deliver prefers the active child over the superseded
	// parent for a shared key, so the two never contend.
	//
	// A restart-from-here fork (-fk) is a deliberate branch of history. It gets
	// its own thread, and LabelChannelKey is dropped because the old hash is
	// tied to the parent's thread_ts. inherit AND takeover both resume the SAME
	// conversation in-thread — only restart-from-here branches.
	branchesThread := pr.Mode == spiceboxv1alpha1.PendingRestartModeRestart
	// The child gets its OWN subject prefix. The runner reads this field
	// verbatim while channelsd derives subjects from the session's own (ns,
	// name); carrying the parent's value forward would leave the child's runner
	// listening on the parent's subjects — inbound never reaches it and its
	// output lands on the parent's stream.
	childPrefix := channelevents.SubjectPrefix(parent.Namespace, pr.TargetSessionName)
	// Both bindings fork identically. A split-channel session (github INPUT +
	// slack OUTPUT — reviewbot's shape) delivers through its OutputChannel, so a
	// child that kept only the input would have no human-readable surface and
	// every reply would hit the input kind's refusing sender. Carry both.
	childCB := forkChildBinding(cb, parent.Name, childPrefix, branchesThread)
	childOutCB := forkChildBinding(parent.Spec.OutputChannel, parent.Name, childPrefix, branchesThread)
	// Carry forward channel-routing labels so the inbound pipeline can
	// find the child session when a user message arrives. LabelChannelKey and
	// LabelOutputChannelKey are excluded on a branching fork: they hash the
	// parent's thread_ts (which the child no longer shares). The sender's
	// first-send capture patches both with the correct new-thread hash after the
	// first chat.postMessage lands. An inherit fork keeps them as-is since it
	// shares the parent's thread_ts.
	carried := []string{spiceboxv1alpha1.LabelChannelName, spiceboxv1alpha1.LabelChannelKind}
	if !branchesThread {
		carried = append(carried, spiceboxv1alpha1.LabelChannelKey, spiceboxv1alpha1.LabelOutputChannelKey)
	}
	childLabels := map[string]string{}
	for _, key := range carried {
		if v, ok := parent.Labels[key]; ok {
			childLabels[key] = v
		}
	}
	// Identity annotations for SpiceDB subject resolution + the pipeline's
	// external-ID fast path. restart/inherit carry the parent's started_by
	// forward (same owner continues). takeover transfers ownership: a DIFFERENT
	// user is taking the thread over, so stamp THEIR identity instead of
	// copying the parent's.
	childAnnotations := map[string]string{}
	if pr.Mode == spiceboxv1alpha1.PendingRestartModeTakeover {
		if pr.NewOwnerExternalID != "" {
			childAnnotations[spiceboxv1alpha1.AnnotationStartedByExternalID] = pr.NewOwnerExternalID
		}
		if pr.TriggeredBy != "" {
			childAnnotations[spiceboxv1alpha1.AnnotationStartedByCanonicalID] = pr.TriggeredBy.String()
		}
		// NOTE: no AnnotationStartedByEmail here — PendingRestart carries no
		// NewOwnerEmail field (takeover only stamps the external id + the
		// pre-computed canonical subject). A takeover child that lands in
		// identityMode=ask|dynamic therefore has an email-less
		// IdentityChoiceGate requester and fails that gate closed rather than
		// mis-deriving an email. Adding NewOwnerEmail is a CRD field change
		// (mage gen:api + mage manifests); tracked as a follow-up, out of
		// scope for this fix.
	} else {
		for _, key := range []string{
			spiceboxv1alpha1.AnnotationStartedByExternalID,
			spiceboxv1alpha1.AnnotationStartedByCanonicalID,
			spiceboxv1alpha1.AnnotationStartedByEmail,
			// The non-human subject a session with no human starter acts as.
			// Same rule, same reason: restart and inherit continue the SAME
			// session's identity, and a child that dropped it would come back
			// up with no acting principal at all — every governed tool call
			// after the restart authorizing as nobody, which is a malformed
			// SpiceDB check rather than a denial. It is deliberately absent
			// from the takeover arm above: that hands the thread to a human
			// who is not this service, and leaving the fallback armed behind
			// them would attribute their work to it the moment their own
			// attribution went missing.
			spiceboxv1alpha1.AnnotationAuthzServiceSubject,
		} {
			if v, ok := parent.Annotations[key]; ok {
				childAnnotations[key] = v
			}
		}
	}
	// Stamp the forked-from-thread annotation when the parent had a Slack thread
	// binding AND this fork branches into a new thread. The Slack sender reads it
	// to post a forward-link notice in the parent thread and an ephemeral in the
	// new thread after first-send captures the new thread root ts. An in-thread
	// continuation has no old thread to link back to — it never left.
	//
	// Derived from the OUTBOUND binding — the thread the human actually reads.
	// For a slack-input session that is the input; for a split-channel session
	// (github in, slack out) it is the OUTPUT thread, and an input-derived value
	// would be empty (github carries no slack thread) so the fork framing would
	// silently vanish.
	if src := spiceboxv1alpha1.OutboundBinding(parent); branchesThread && src != nil {
		teamID := src.External["team_id"]
		channelID := src.External["channel_id"]
		threadTS := src.External["thread_ts"]
		if teamID != "" && channelID != "" && threadTS != "" {
			childAnnotations[spiceboxv1alpha1.AnnotationForkedFromThread] =
				teamID + ":" + channelID + ":" + threadTS
		}
	}
	return &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{
			Name:        pr.TargetSessionName,
			Namespace:   parent.Namespace,
			Labels:      childLabels,
			Annotations: childAnnotations,
		},
		Spec: spiceboxv1alpha1.AgentSessionSpec{
			Class:         parent.Spec.Class,
			AgentIdentity: parent.Spec.AgentIdentity,
			ForkedFrom:    parent.Name,
			ForkedAtTurn:  ptr.To(pr.CutTurnIndex),
			Prompt:        spiceboxv1alpha1.PromptSource{Inline: pr.NewUserText},
			InputChannel:  childCB,
			OutputChannel: childOutCB,
		},
	}
}

// forkChildBinding derives a restart-fork child's copy of one ChannelBinding.
// Returns nil for a nil binding. The child gets its own subject prefix and
// records its predecessor via InheritFrom. On a branching fork (restart-from-
// here) thread_ts is dropped from a deep-copied External so the Slack sender
// posts at channel level and the returned ts becomes the new thread root
// (first-send capture); every other External key survives. An inherit fork
// keeps thread_ts so the child resumes the same thread. Shared by the input and
// output bindings — both branch the same way.
func forkChildBinding(cb *spiceboxv1alpha1.ChannelBinding, parentName, childSubjectPrefix string, branchesThread bool) *spiceboxv1alpha1.ChannelBinding {
	if cb == nil {
		return nil
	}
	copied := *cb
	copied.InheritFrom = parentName
	copied.NATSSubjectPrefix = childSubjectPrefix
	if branchesThread && len(copied.External) > 0 {
		ext := make(map[string]string, len(copied.External))
		for k, v := range copied.External {
			if k != "thread_ts" {
				ext[k] = v
			}
		}
		copied.External = ext
	}
	return &copied
}
