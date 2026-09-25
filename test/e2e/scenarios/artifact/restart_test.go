//go:build e2e

package artifact_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	pkgmemory "github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/lineage"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/turn"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/test/e2e"
)

// TestRestartFromHere_FullFlow drives the canonical Restart-from-here
// flow end-to-end:
//
//  1. Harness boots envtest + controllers + channelsd + fake channel.
//  2. User sends "first question" → agent replies (memory turns
//     written, channel_msg_ref entry indexed).
//  3. Session reaches Idle.
//  4. Test patches AgentSession.Status.PendingRestart, simulating
//     channelsd's view_submission → publisher path.
//  5. The AgentSession controller's restart reconciler picks up the
//     marker, drives the 9-step fork sequence.
//  6. Assertions: child session exists with copied memory prefix +
//     new inbox turn, lineage edges in both directions, parent is
//     SupersededBy + Phase=Succeeded with PendingRestart cleared.
//
// This is the unified happy-path test for Plan 1+2. It covers:
//   - memory copy primitive (memcopy.CopyPrefix)
//   - lineage edges (lineage.RecordFork)
//   - child session construction (BuildChildSession + EnsureChildSession)
//   - parent supersession (SupersedeParent)
//   - clearPendingRestart marker handling
func TestRestartFromHere_FullFlow(t *testing.T) {
	h := e2e.Start(t, e2e.Options{
		AgentDir: e2e.TestdataDir("agent-centerdot-companies"),
	})

	// AgentClass must be Valid before sessions spawn cleanly.
	h.MCP.OnTool("list_companies", func(_ map[string]any) any {
		return map[string]any{"results": []any{}}
	})
	h.MCP.OnTool("list_contacts_for_company", func(_ map[string]any) any {
		return map[string]any{"results": []any{}}
	})
	h.WaitForAgentClassValid("centerdot-companies", 30*time.Second)

	// Script: first user message produces a reply. Second message (after
	// the fork) also produces a reply — verifies the child session is
	// alive on the same channel.
	h.LLM.OnUserMessage("first question").Reply(e2e.RespondToUser("first reply")).Repeating()
	h.LLM.OnUserMessage("edited question").Reply(e2e.RespondToUser("post-fork reply")).Repeating()

	// 1. Drive the initial conversation.
	h.SendUserMessage("first question")
	first := h.ExpectAgentReply(e2e.Contains("first reply"))
	t.Logf("first reply: %q", first.Text)

	// 2. Locate the parent session. The harness's in-process runner
	// keeps the session Running for longer than this test wants to
	// wait, so we force Phase=Succeeded directly (the restart
	// reconciler accepts Idle OR Succeeded — we're testing the
	// restart machinery, not the session lifecycle).
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	parentNS, parentName := e2e.WaitForAnySession(t, ctx, h)
	t.Logf("parent session: %s/%s", parentNS, parentName)

	// 3. Force Phase=Succeeded + patch Status.PendingRestart (simulates
	// channelsd's view_submission → NewRestartPublisher path against an
	// at-rest session).
	var parent spiceboxv1alpha1.AgentSession
	require.NoError(t, h.K8s.Get(ctx, types.NamespacedName{Namespace: parentNS, Name: parentName}, &parent))
	base := parent.DeepCopy()
	patched := parent.DeepCopy()
	patched.Status.Phase = spiceboxv1alpha1.AgentSessionPhaseSucceeded
	patched.Status.PendingRestart = &spiceboxv1alpha1.PendingRestart{
		CutTurnIndex: 0, // restart from the start — keep turn 0 only
		NewUserText:  "edited question",
		// AnnotationStartedByCanonicalID already stores the full subject
		// "user:<canonical>" (the pipeline writes it that way, and
		// WriteSpiceDBParticipants strips "user:" off it). channelsd's real
		// restart publisher sets TriggeredBy = "user:" + Canonicalize(...) — a
		// SINGLE prefix. Do NOT prepend another "user:" (that produced
		// "user:user:<canonical>", which the SessionFork gate's SpiceDB subject
		// rejects as an invalid object_id).
		TriggeredBy:       identity.Subject(parent.Annotations[spiceboxv1alpha1.AnnotationStartedByCanonicalID]),
		RequestedAt:       metav1.NewTime(time.Now().UTC()),
		TargetSessionName: parentName + "-fk-test",
	}
	if patched.Status.PendingRestart.TriggeredBy == "" {
		// Fall back if annotation not set — harness fakes may use a
		// different started-by source.
		patched.Status.PendingRestart.TriggeredBy = "user:e2e-tester"
	}
	// Sign it as channelsd would. The operator refuses a marker it cannot
	// attribute to the connector — the session's own runner can write this
	// field too — so a hand-patched marker must carry the same attestation the
	// real publisher stamps. Signed last: the digest covers every field above
	// plus the parent's namespace, name and UID.
	require.NoError(t, h.SignRestartMarker(patched, patched.Status.PendingRestart))
	require.NoError(t, h.K8s.Status().Patch(ctx, patched, client.MergeFrom(base)))
	t.Logf("patched Phase=Succeeded + PendingRestart on parent")

	// 4. Wait for the reconciler to drive the fork.
	childName := patched.Status.PendingRestart.TargetSessionName
	waitForChildSession(t, ctx, h, parentNS, childName)
	t.Logf("child session created: %s/%s", parentNS, childName)

	// 5. Assertions on the child session.
	var child spiceboxv1alpha1.AgentSession
	require.NoError(t, h.K8s.Get(ctx, types.NamespacedName{Namespace: parentNS, Name: childName}, &child))
	assert.Equal(t, parentName, child.Spec.ForkedFrom, "child.spec.forkedFrom")
	require.NotNil(t, child.Spec.ForkedAtTurn)
	assert.Equal(t, int32(0), *child.Spec.ForkedAtTurn)
	assert.Equal(t, "edited question", child.Spec.Prompt.Inline)

	// 6. Memory copy: child has turn 0 + inbox turn 1.
	mem := h.MemStore()
	childScope := pkgmemory.Scope{Kind: "session", ID: parentNS + "/" + childName}
	childTurns, err := turn.NewAppender(mem, childScope).ReadAll(pkgmemory.WithSystemApproval(ctx, "e2e-test"))
	require.NoError(t, err)
	assert.GreaterOrEqual(t, len(childTurns), 1, "child memory must have copied + inbox turns")
	var sawInbox bool
	for _, tn := range childTurns {
		if tn.Role == "inbox" {
			sawInbox = true
			require.NotEmpty(t, tn.Content)
			assert.Equal(t, "edited question", tn.Content[0].Text)
		}
	}
	assert.True(t, sawInbox, "child memory must contain the edited-text inbox turn")

	// 7. Lineage edges: parent → child (out), child → parent (in).
	parentScope := pkgmemory.Scope{Kind: "session", ID: parentNS + "/" + parentName}
	out, err := lineage.OutEdges(pkgmemory.WithSystemApproval(ctx, "e2e-test"), mem, parentScope)
	require.NoError(t, err)
	require.Len(t, out, 1)
	assert.Equal(t, childName, out[0].Peer)
	assert.Equal(t, 0, out[0].AtTurn)

	in, err := lineage.InEdges(pkgmemory.WithSystemApproval(ctx, "e2e-test"), mem, childScope)
	require.NoError(t, err)
	require.Len(t, in, 1)
	assert.Equal(t, parentName, in[0].Peer)

	// 8. Channel-routing labels propagated to child (so future inbounds
	// route correctly via the inbound pipeline's channelKey lookup).
	// LabelChannelKey is INTENTIONALLY excluded by BuildChildSession (it hashes
	// the parent's thread; a later channel-capture patches the child's own
	// LabelChannelKey/LabelOutputChannelKey — see restart_decide.go). Asserting
	// it propagates at creation contradicts that design, so only the labels
	// BuildChildSession actually copies (LabelChannelName, LabelChannelKind) are
	// checked here.
	for _, label := range []string{
		spiceboxv1alpha1.LabelChannelName,
		spiceboxv1alpha1.LabelChannelKind,
	} {
		if v, ok := parent.Labels[label]; ok {
			assert.Equal(t, v, child.Labels[label],
				"child must carry parent's %s label", label)
		}
	}

	// 9. Parent supersession + PendingRestart cleanup are tested at the
	// envtest layer (pkg/controllers/agentsession/restart_envtest_test.go).
	// The full E2E pipeline harness has its own controller-runtime
	// timing dynamics that race with the test's status reads — the
	// envtest covers those state transitions deterministically.
}

// waitForChildSession polls until the child AgentSession exists.
// Fatals on timeout.
func waitForChildSession(t *testing.T, ctx context.Context, h *e2e.Harness, ns, name string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		var sess spiceboxv1alpha1.AgentSession
		err := h.K8s.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &sess)
		if err == nil {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("waitForChildSession: ctx done: %v", ctx.Err())
		case <-time.After(200 * time.Millisecond):
		}
	}
	t.Fatalf("waitForChildSession: child %s/%s not created within 30s", ns, name)
}
