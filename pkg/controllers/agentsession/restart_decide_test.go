package agentsession_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/controllers/agentsession"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/tool_dispatch_snapshot"
	"github.com/authzed/openagentprimitives/pkg/platform/workspace"
)

func TestAnalyzePostCut_NoEntries_NoAffectedBundles(t *testing.T) {
	affected, snaps := agentsession.AnalyzePostCut(nil, 5)
	assert.Empty(t, affected)
	assert.Empty(t, snaps)
}

func TestAnalyzePostCut_BundleWithStatefulDispatchAfterCut(t *testing.T) {
	entries := []tool_dispatch_snapshot.Content{
		{ToolUseID: "u1", SpiceboxSession: "b1", TurnIndex: 3, Sequence: 0, SessionUID: "uid"},
		{ToolUseID: "u2", SpiceboxSession: "b1", TurnIndex: 6, Sequence: 0, SessionUID: "uid"},
		{ToolUseID: "u3", SpiceboxSession: "b2", TurnIndex: 7, Sequence: 1, SessionUID: "uid"},
	}
	affected, snaps := agentsession.AnalyzePostCut(entries, 5)
	assert.ElementsMatch(t, []string{"b1", "b2"}, affected)
	require.NotNil(t, snaps["b1"])
	assert.Equal(t, workspace.SnapshotHandle{SessionUID: "uid", TurnIndex: 6, Sequence: 0}, *snaps["b1"])
	require.NotNil(t, snaps["b2"])
	assert.Equal(t, workspace.SnapshotHandle{SessionUID: "uid", TurnIndex: 7, Sequence: 1}, *snaps["b2"])
}

func TestAnalyzePostCut_PicksEarliestPostCutSnapshotPerBundle(t *testing.T) {
	entries := []tool_dispatch_snapshot.Content{
		{ToolUseID: "u1", SpiceboxSession: "b1", TurnIndex: 8, Sequence: 0, SessionUID: "uid"},
		{ToolUseID: "u2", SpiceboxSession: "b1", TurnIndex: 6, Sequence: 0, SessionUID: "uid"},
		{ToolUseID: "u3", SpiceboxSession: "b1", TurnIndex: 6, Sequence: 1, SessionUID: "uid"},
	}
	_, snaps := agentsession.AnalyzePostCut(entries, 5)
	require.NotNil(t, snaps["b1"])
	assert.Equal(t, 6, snaps["b1"].TurnIndex)
	assert.Equal(t, 0, snaps["b1"].Sequence, "earliest seq within turn 6")
}

func TestAnalyzePostCut_IgnoresEntriesAtOrBelowCut(t *testing.T) {
	entries := []tool_dispatch_snapshot.Content{
		{ToolUseID: "u1", SpiceboxSession: "b1", TurnIndex: 3, Sequence: 0, SessionUID: "uid"},
		{ToolUseID: "u2", SpiceboxSession: "b1", TurnIndex: 5, Sequence: 0, SessionUID: "uid"},
	}
	affected, snaps := agentsession.AnalyzePostCut(entries, 5)
	assert.Empty(t, affected)
	assert.Empty(t, snaps)
}

func TestBuildChildSession_CarriesForwardSpec(t *testing.T) {
	parent := &v1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "parent",
			Namespace: "ns",
			Labels: map[string]string{
				v1alpha1.LabelChannelKey:  "key-hash",
				v1alpha1.LabelChannelName: "ch",
				v1alpha1.LabelChannelKind: "slack",
				"unrelated":               "drop-me",
			},
			Annotations: map[string]string{
				v1alpha1.AnnotationStartedByCanonicalID: "user:alice",
				v1alpha1.AnnotationStartedByExternalID:  "U1",
			},
		},
		Spec: v1alpha1.AgentSessionSpec{
			Class:         "demo",
			AgentIdentity: "ai-bot",
			InputChannel: &v1alpha1.ChannelBinding{
				Name: "ch", Kind: "slack", Key: "thread:C1:1.0",
				Capabilities: []string{"text"}, NATSSubjectPrefix: "ap.sess.ns.parent",
			},
		},
	}
	pr := &v1alpha1.PendingRestart{
		CutTurnIndex:      5,
		NewUserText:       "edited",
		TriggeredBy:       "user:alice",
		TargetSessionName: "parent-fkabc",
	}

	child := agentsession.BuildChildSession(parent, pr)
	assert.Equal(t, "parent-fkabc", child.Name)
	assert.Equal(t, "ns", child.Namespace)
	assert.Equal(t, "demo", child.Spec.Class)
	assert.Equal(t, "ai-bot", child.Spec.AgentIdentity)
	assert.Equal(t, "parent", child.Spec.ForkedFrom)
	require.NotNil(t, child.Spec.ForkedAtTurn)
	assert.Equal(t, int32(5), *child.Spec.ForkedAtTurn)
	assert.Equal(t, "edited", child.Spec.Prompt.Inline)
	require.NotNil(t, child.Spec.InputChannel)
	assert.Equal(t, "ch", child.Spec.InputChannel.Name)
	assert.Equal(t, "parent", child.Spec.InputChannel.InheritFrom)

	// Channel-routing labels: LabelChannelKey is intentionally NOT carried
	// forward — it hashes the parent's thread_ts which the child no longer
	// shares (child starts a new thread). LabelChannelName and LabelChannelKind
	// are carried so the outbound relay and status reconcilers still work.
	assert.NotContains(t, child.Labels, v1alpha1.LabelChannelKey, "LabelChannelKey must NOT be carried: wrong thread_ts hash")
	assert.Equal(t, "ch", child.Labels[v1alpha1.LabelChannelName])
	assert.Equal(t, "slack", child.Labels[v1alpha1.LabelChannelKind])
	assert.NotContains(t, child.Labels, "unrelated", "only carry routing labels, not arbitrary parent labels")

	// Identity annotations must be carried forward.
	assert.Equal(t, "user:alice", child.Annotations[v1alpha1.AnnotationStartedByCanonicalID])
	assert.Equal(t, "U1", child.Annotations[v1alpha1.AnnotationStartedByExternalID])
}

// TestBuildChildSession_ClearsThreadTSAndChannelKeyLabel verifies that a
// restart-from-here (-fk) fork's ChannelBinding has thread_ts stripped (so
// the Slack sender posts at channel level, creating a new thread root) and
// LabelChannelKey is absent (the old hash was tied to the parent's
// thread_ts). An inherit fork does NOT do this — see
// TestBuildChildSession_InheritForkStaysInTheParentThread.
func TestBuildChildSession_ClearsThreadTSAndChannelKeyLabel(t *testing.T) {
	parent := &v1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "parent",
			Namespace: "ns",
			Labels: map[string]string{
				v1alpha1.LabelChannelKey:  "old-thread-hash",
				v1alpha1.LabelChannelName: "ch",
				v1alpha1.LabelChannelKind: "slack",
			},
		},
		Spec: v1alpha1.AgentSessionSpec{
			Class: "demo",
			InputChannel: &v1alpha1.ChannelBinding{
				Name: "ch", Kind: "slack",
				External: map[string]string{
					"channel_id":  "C123",
					"thread_ts":   "1234.5678",
					"team_id":     "T99",
					"bot_user_id": "B1",
				},
			},
		},
	}
	pr := &v1alpha1.PendingRestart{
		Mode: v1alpha1.PendingRestartModeRestart, CutTurnIndex: 2, NewUserText: "x", TargetSessionName: "parent-fk",
	}

	child := agentsession.BuildChildSession(parent, pr)

	// thread_ts must be absent from child's External so the sender posts
	// at channel level (new thread root).
	require.NotNil(t, child.Spec.InputChannel)
	assert.NotContains(t, child.Spec.InputChannel.External, "thread_ts",
		"thread_ts must be cleared on child to spawn a new thread")

	// Other external keys that are needed for routing must survive.
	assert.Equal(t, "C123", child.Spec.InputChannel.External["channel_id"])
	assert.Equal(t, "T99", child.Spec.InputChannel.External["team_id"])

	// LabelChannelKey must be absent — it hashed the old thread_ts.
	assert.NotContains(t, child.Labels, v1alpha1.LabelChannelKey,
		"LabelChannelKey must be cleared; first-send capture will set the correct one")
}

// TestBuildChildSession_StampsForkedFromThreadAnnotation verifies that when a
// restart-from-here (-fk) fork's parent input channel carries team_id +
// channel_id + thread_ts, the child gets an AnnotationForkedFromThread
// annotation in the form "<team_id>:<channel_id>:<thread_ts>". An inherit
// fork never stamps this — it has no old thread to link back to.
func TestBuildChildSession_StampsForkedFromThreadAnnotation(t *testing.T) {
	parent := &v1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "parent", Namespace: "ns"},
		Spec: v1alpha1.AgentSessionSpec{
			Class: "demo",
			InputChannel: &v1alpha1.ChannelBinding{
				Name: "ch", Kind: "slack",
				External: map[string]string{
					"team_id":    "T99",
					"channel_id": "C123",
					"thread_ts":  "1234.5678",
				},
			},
		},
	}
	pr := &v1alpha1.PendingRestart{
		Mode: v1alpha1.PendingRestartModeRestart, CutTurnIndex: 1, NewUserText: "y", TargetSessionName: "parent-fk",
	}

	child := agentsession.BuildChildSession(parent, pr)

	assert.Equal(t, "T99:C123:1234.5678",
		child.Annotations[v1alpha1.AnnotationForkedFromThread],
		"forked-from-thread annotation must carry team:channel:ts for the Slack sender")
}

// TestBuildChildSession_NoForkedFromThreadAnnotation_WhenParentMissingFields
// verifies that the annotation is absent when the parent lacks any of
// team_id / channel_id / thread_ts (e.g., a DM session or a non-Slack session).
func TestBuildChildSession_NoForkedFromThreadAnnotation_WhenParentMissingFields(t *testing.T) {
	cases := []struct {
		name     string
		external map[string]string
	}{
		{"no external", nil},
		{"missing thread_ts", map[string]string{"team_id": "T1", "channel_id": "C1"}},
		{"missing team_id", map[string]string{"channel_id": "C1", "thread_ts": "1.2"}},
		{"missing channel_id", map[string]string{"team_id": "T1", "thread_ts": "1.2"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cb := &v1alpha1.ChannelBinding{Name: "ch", Kind: "slack", External: tc.external}
			parent := &v1alpha1.AgentSession{
				ObjectMeta: metav1.ObjectMeta{Name: "parent", Namespace: "ns"},
				Spec:       v1alpha1.AgentSessionSpec{Class: "demo", InputChannel: cb},
			}
			pr := &v1alpha1.PendingRestart{
				Mode: v1alpha1.PendingRestartModeRestart, CutTurnIndex: 0, NewUserText: "z", TargetSessionName: "parent-fk",
			}
			child := agentsession.BuildChildSession(parent, pr)
			assert.NotContains(t, child.Annotations, v1alpha1.AnnotationForkedFromThread,
				"annotation must be absent when parent lacks full Slack thread binding")
		})
	}
}

// TestBuildChildSession_InheritForkStaysInTheParentThread pins that a
// continuation lands where the user is already reading. Only a user-initiated
// restart (-fk) branches into a new thread.
func TestBuildChildSession_InheritForkStaysInTheParentThread(t *testing.T) {
	parent := &v1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{
			Name: "parent", Namespace: "ns",
			Labels: map[string]string{v1alpha1.LabelChannelKey: "key-hash", v1alpha1.LabelChannelName: "ch"},
		},
		Spec: v1alpha1.AgentSessionSpec{
			InputChannel: &v1alpha1.ChannelBinding{
				Name: "ch", Kind: "slack",
				External: map[string]string{"team_id": "T1", "channel_id": "C1", "thread_ts": "111.1"},
			},
		},
	}

	inherit := agentsession.BuildChildSession(parent, &v1alpha1.PendingRestart{
		Mode: v1alpha1.PendingRestartModeInherit, NewUserText: "go on", TargetSessionName: "parent-icabc123",
	})
	require.NotNil(t, inherit.Spec.InputChannel)
	assert.Equal(t, "111.1", inherit.Spec.InputChannel.External["thread_ts"],
		"a continuation must stay in the thread the user is reading")
	assert.Equal(t, "key-hash", inherit.Labels[v1alpha1.LabelChannelKey],
		"same thread ⇒ same channel key; Deliver prefers the active child")
	assert.NotContains(t, inherit.Annotations, v1alpha1.AnnotationForkedFromThread,
		"an in-thread continuation has no old thread to link back to")

	restart := agentsession.BuildChildSession(parent, &v1alpha1.PendingRestart{
		CutTurnIndex: 3, NewUserText: "redo", TargetSessionName: "parent-fkabc123",
	})
	require.NotNil(t, restart.Spec.InputChannel)
	assert.NotContains(t, restart.Spec.InputChannel.External, "thread_ts",
		"a restart-from-here branches into its own thread")
	assert.NotContains(t, restart.Labels, v1alpha1.LabelChannelKey)
}

func TestBuildChildSession_NilInputChannel_OK(t *testing.T) {
	parent := &v1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "parent", Namespace: "ns"},
		Spec:       v1alpha1.AgentSessionSpec{Class: "demo"},
	}
	pr := &v1alpha1.PendingRestart{CutTurnIndex: 0, NewUserText: "x", TargetSessionName: "p-fk"}
	child := agentsession.BuildChildSession(parent, pr)
	assert.Nil(t, child.Spec.InputChannel)
}

// TestBuildChildSession_RecomputesNATSSubjectPrefix pins the child's event
// subject prefix to the CHILD's own name.
//
// The runner reads spec.inputChannel.natsSubjectPrefix verbatim, while
// channelsd derives the session's subjects as SubjectPrefix(ns, name). When the
// child inherited the parent's prefix the two spoke on different subjects: the
// child's runner never received inbound (so the silence watchdog declared it
// stalled) and its output was published onto the parent's stream (so the new
// messages appeared under the older session).
func TestBuildChildSession_RecomputesNATSSubjectPrefix(t *testing.T) {
	parent := &v1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "parent", Namespace: "ns"},
		Spec: v1alpha1.AgentSessionSpec{
			Class: "demo",
			InputChannel: &v1alpha1.ChannelBinding{
				Name: "ch", Kind: "slack", Key: "thread:C1:1.0",
				NATSSubjectPrefix: channelevents.SubjectPrefix("ns", "parent"),
			},
		},
	}
	pr := &v1alpha1.PendingRestart{
		Mode:              v1alpha1.PendingRestartModeInherit,
		NewUserText:       "follow-up",
		TargetSessionName: "parent-icabc123",
	}

	child := agentsession.BuildChildSession(parent, pr)

	require.NotNil(t, child.Spec.InputChannel)
	assert.Equal(t,
		channelevents.SubjectPrefix("ns", "parent-icabc123"),
		child.Spec.InputChannel.NATSSubjectPrefix,
		"child must own its subject prefix; inheriting the parent's splits runner from channelsd")
	assert.NotEqual(t,
		parent.Spec.InputChannel.NATSSubjectPrefix,
		child.Spec.InputChannel.NATSSubjectPrefix,
		"child must not publish on the parent's stream")
}

// TestBuildChildSession_TakeoverTransfersOwnership pins the takeover-mode
// departure from inherit: the child stays in the SAME thread (like inherit),
// but its started_by is stamped from the NEW owner (bob) rather than copied
// from the parent's owner (alice) — a different user is taking the thread over.
func TestBuildChildSession_TakeoverTransfersOwnership(t *testing.T) {
	parent := &v1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "chan-abcd1234",
			Namespace: "ns",
			Labels: map[string]string{
				v1alpha1.LabelChannelName: "chan",
				v1alpha1.LabelChannelKind: "slack",
				v1alpha1.LabelChannelKey:  "hashofthread",
			},
			Annotations: map[string]string{
				v1alpha1.AnnotationStartedByExternalID:  "UALICE",
				v1alpha1.AnnotationStartedByCanonicalID: "user:alice",
			},
		},
		Spec: v1alpha1.AgentSessionSpec{
			Class: "cls",
			InputChannel: &v1alpha1.ChannelBinding{
				Name: "chan", Kind: "slack", Key: "thread:C1:100.1",
				External: map[string]string{"channel_id": "C1", "thread_ts": "100.1", "team_id": "T1"},
			},
		},
	}
	pr := &v1alpha1.PendingRestart{
		Mode:               v1alpha1.PendingRestartModeTakeover,
		NewUserText:        "hello from bob",
		TriggeredBy:        "user:bob",
		NewOwnerExternalID: "UBOB",
		InheritHistory:     true,
		TargetSessionName:  "chan-abcd1234-tk-bob01",
	}

	child := agentsession.BuildChildSession(parent, pr)

	// Ownership transferred to bob, NOT copied from alice.
	assert.Equal(t, "UBOB", child.Annotations[v1alpha1.AnnotationStartedByExternalID],
		"takeover stamps the NEW owner's external id")
	assert.Equal(t, "user:bob", child.Annotations[v1alpha1.AnnotationStartedByCanonicalID],
		"takeover stamps the NEW owner's canonical id")

	// Stays in-thread: keeps the channel-key label + thread_ts (like inherit).
	assert.Equal(t, "hashofthread", child.Labels[v1alpha1.LabelChannelKey],
		"same thread ⇒ same channel key; Deliver prefers the active child")
	require.NotNil(t, child.Spec.InputChannel)
	assert.Equal(t, "100.1", child.Spec.InputChannel.External["thread_ts"],
		"takeover continues in the thread the users are reading")
	assert.Equal(t, "chan-abcd1234", child.Spec.InputChannel.InheritFrom)

	// No forked-from-thread annotation (that is only for branching -fk forks).
	assert.NotContains(t, child.Annotations, v1alpha1.AnnotationForkedFromThread)
}

// splitChannelParent builds a reviewbot-shaped parent: a github INPUT channel
// (input-only, no slack thread) paired with a slack OUTPUT channel that carries
// the thread the review was delivered to. This is the shape a restart fork used
// to silently break — the child kept the github input and lost the slack output
// entirely, so its reply had nowhere a person could read it.
func splitChannelParent(outExternal map[string]string, labels map[string]string) *v1alpha1.AgentSession {
	return &v1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "parent", Namespace: "ns", Labels: labels},
		Spec: v1alpha1.AgentSessionSpec{
			Class: "demo",
			InputChannel: &v1alpha1.ChannelBinding{
				Name: "gh-in", Kind: "github", NATSSubjectPrefix: "ap.session.ns.parent",
			},
			OutputChannel: &v1alpha1.ChannelBinding{
				Name: "slack-out", Kind: "slack", NATSSubjectPrefix: "ap.session.ns.parent",
				External: outExternal,
			},
		},
	}
}

// TestBuildChildSession_BranchingFork_CarriesForwardOutputChannel is the
// follow-up fix: a split-channel (github-in / slack-out) session's fork child
// must KEEP its slack output binding, or the child has no human-readable surface
// and every reply fails on the github input's refusing sender. On a branching
// fork the output thread_ts is dropped (new thread) but channel_id survives, and
// the child's own subject prefix + InheritFrom are set exactly as the input
// binding's are.
func TestBuildChildSession_BranchingFork_CarriesForwardOutputChannel(t *testing.T) {
	parent := splitChannelParent(map[string]string{
		"channel_id": "C0OUT", "thread_ts": "500.500",
	}, nil)
	pr := &v1alpha1.PendingRestart{
		Mode: v1alpha1.PendingRestartModeRestart, CutTurnIndex: 1, NewUserText: "again", TargetSessionName: "parent-fk",
	}

	child := agentsession.BuildChildSession(parent, pr)

	require.NotNil(t, child.Spec.OutputChannel, "the child must keep its slack output binding")
	assert.Equal(t, "slack-out", child.Spec.OutputChannel.Name)
	assert.Equal(t, "C0OUT", child.Spec.OutputChannel.External["channel_id"],
		"channel_id must survive so the reply lands on the configured slack channel")
	assert.NotContains(t, child.Spec.OutputChannel.External, "thread_ts",
		"thread_ts must be dropped so a branching fork starts a new thread")
	assert.Equal(t, "parent", child.Spec.OutputChannel.InheritFrom,
		"the output binding must record its predecessor like the input binding does")
	assert.Equal(t, channelevents.SubjectPrefix("ns", "parent-fk"), child.Spec.OutputChannel.NATSSubjectPrefix,
		"the child's output binding must not carry the parent's subject prefix")
}

// TestBuildChildSession_BranchingFork_OutputThreadIsForkedFromSource pins that
// for a split-channel session the "continued from" link points at the slack
// OUTPUT thread — the one the human is actually reading — not the input. The
// github input carries no slack thread, so an input-derived annotation would be
// absent and the fork framing would silently vanish.
func TestBuildChildSession_BranchingFork_OutputThreadIsForkedFromSource(t *testing.T) {
	parent := splitChannelParent(map[string]string{
		"team_id": "T0", "channel_id": "C0OUT", "thread_ts": "500.500",
	}, nil)
	pr := &v1alpha1.PendingRestart{
		Mode: v1alpha1.PendingRestartModeRestart, CutTurnIndex: 1, NewUserText: "again", TargetSessionName: "parent-fk",
	}

	child := agentsession.BuildChildSession(parent, pr)

	assert.Equal(t, "T0:C0OUT:500.500", child.Annotations[v1alpha1.AnnotationForkedFromThread],
		"forked-from must be derived from the outbound (slack output) thread, not the github input")
}

// TestBuildChildSession_InheritFork_KeepsOutputChannelThread pins the
// continuation case: an inherit fork of a split-channel session resumes the SAME
// slack output thread (thread_ts preserved) and keeps LabelOutputChannelKey.
func TestBuildChildSession_InheritFork_KeepsOutputChannelThread(t *testing.T) {
	parent := splitChannelParent(
		map[string]string{"channel_id": "C0OUT", "thread_ts": "500.500"},
		map[string]string{v1alpha1.LabelOutputChannelKey: "out-key-hash"},
	)
	child := agentsession.BuildChildSession(parent, &v1alpha1.PendingRestart{
		Mode: v1alpha1.PendingRestartModeInherit, NewUserText: "carry on", TargetSessionName: "parent-ic1",
	})

	require.NotNil(t, child.Spec.OutputChannel)
	assert.Equal(t, "500.500", child.Spec.OutputChannel.External["thread_ts"],
		"an inherit fork resumes the same output thread")
	assert.Equal(t, "out-key-hash", child.Labels[v1alpha1.LabelOutputChannelKey],
		"same output thread ⇒ keep the output-channel key label")
}

// TestBuildChildSession_BranchingFork_DropsOutputChannelKeyLabel pins that a
// branching fork clears LabelOutputChannelKey (it hashed the parent's output
// thread_ts, which the child no longer shares); first-send capture re-stamps it.
func TestBuildChildSession_BranchingFork_DropsOutputChannelKeyLabel(t *testing.T) {
	parent := splitChannelParent(
		map[string]string{"channel_id": "C0OUT", "thread_ts": "500.500"},
		map[string]string{v1alpha1.LabelOutputChannelKey: "out-key-hash"},
	)
	child := agentsession.BuildChildSession(parent, &v1alpha1.PendingRestart{
		Mode: v1alpha1.PendingRestartModeRestart, CutTurnIndex: 1, NewUserText: "again", TargetSessionName: "parent-fk",
	})

	assert.NotContains(t, child.Labels, v1alpha1.LabelOutputChannelKey,
		"a branching fork must clear the output-channel key; it hashed the old thread_ts")
}
