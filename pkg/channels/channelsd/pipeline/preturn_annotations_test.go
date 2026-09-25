package pipeline

// These pin the write itself. The ORDERING it exists for — the annotations
// landing before anything wakes — is pinned by the e2e scenario
// test/e2e/scenarios/slack_dm_threading, which is where the bug was found:
// under the gate's own parallelism the agent's DM reply posted TOP-LEVEL
// instead of threading under the user's message, three runs out of three,
// while passing alone in a fifth of the time.
//
// That shape is worth naming, because the repo's own guidance would otherwise
// file it as contention: a timeout-shaped failure that passes in isolation.
// The tell was the counts in the failure message — `top-level=2, replies=0`.
// Two top-level messages means the reply ARRIVED and threaded nowhere. A
// starved run would have shown one. Slow produces late; this produced wrong.

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

const (
	tsAnnotation    = "slack.agentprimitives.authzed.com/last-inbound-ts"
	canonAnnotation = "slack.agentprimitives.authzed.com/last-inbound-canonical-id"
)

func preTurnSession(t *testing.T, ann map[string]string) *spiceboxv1alpha1.AgentSession {
	t.Helper()
	return &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{
			Name: "sess", Namespace: "default", ResourceVersion: "10", Annotations: ann,
		},
	}
}

func newPreTurnPipeline(t *testing.T, sess *spiceboxv1alpha1.AgentSession) (*Pipeline, client.Client) {
	t.Helper()
	c := fake.NewClientBuilder().
		WithScheme(newWakeTestScheme(t)).
		WithObjects(sess).
		WithStatusSubresource(sess).
		Build()
	return &Pipeline{K8s: c}, c
}

func storedAnnotations(t *testing.T, c client.Client) map[string]string {
	t.Helper()
	var got spiceboxv1alpha1.AgentSession
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "sess"}, &got))
	return got.Annotations
}

// TestTheAnchorAndRequesterLandTogether: one patch, both values, before the
// caller wakes anything.
func TestTheAnchorAndRequesterLandTogether(t *testing.T) {
	sess := preTurnSession(t, nil)
	p, c := newPreTurnPipeline(t, sess)

	ok := p.stampPreTurnAnnotations(context.Background(), sess,
		channelkinds.InboundEvent{
			PreTurnAnnotations:             map[string]string{tsAnnotation: "1700000000.000001"},
			RequesterCanonicalIDAnnotation: canonAnnotation,
		}, "user:someone")

	assert.True(t, ok, "a successful write must tell the kind to skip its own fallback stamp")
	got := storedAnnotations(t, c)
	assert.Equal(t, "1700000000.000001", got[tsAnnotation])
	assert.Equal(t, "user:someone", got[canonAnnotation])
}

// TestTheInMemoryCopyIsUpdatedToo.
//
// Everything after this point in Deliver reads the same in-memory session —
// the wake decision, the relay's snapshot. Writing only to the API server
// would reintroduce the very race one layer up: the object would be correct
// and the copy driving the turn would not.
func TestTheInMemoryCopyIsUpdatedToo(t *testing.T) {
	sess := preTurnSession(t, nil)
	p, _ := newPreTurnPipeline(t, sess)

	p.stampPreTurnAnnotations(context.Background(), sess,
		channelkinds.InboundEvent{
			PreTurnAnnotations:             map[string]string{tsAnnotation: "1700000000.000001"},
			RequesterCanonicalIDAnnotation: canonAnnotation,
		}, "user:someone")

	assert.Equal(t, "1700000000.000001", sess.Annotations[tsAnnotation],
		"the caller reads this object for the rest of the turn; a stale copy is the same bug one layer up")
}

// TestAnIdenticalStampIsNotWritten is the churn pin. The common case in a busy
// thread is the same person replying under the same anchor, and a merge patch
// that changes nothing still bumps resourceVersion and wakes every watcher.
func TestAnIdenticalStampIsNotWritten(t *testing.T) {
	sess := preTurnSession(t, map[string]string{
		tsAnnotation:    "1700000000.000001",
		canonAnnotation: "user:someone",
	})
	p, c := newPreTurnPipeline(t, sess)
	before := storedAnnotations(t, c)
	var rv string
	{
		var got spiceboxv1alpha1.AgentSession
		require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "sess"}, &got))
		rv = got.ResourceVersion
	}

	ok := p.stampPreTurnAnnotations(context.Background(), sess,
		channelkinds.InboundEvent{
			PreTurnAnnotations:             map[string]string{tsAnnotation: "1700000000.000001"},
			RequesterCanonicalIDAnnotation: canonAnnotation,
		}, "user:someone")

	assert.True(t, ok, "already-correct is still stamped: the kind must not redo it")
	var after spiceboxv1alpha1.AgentSession
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "sess"}, &after))
	assert.Equal(t, rv, after.ResourceVersion, "an identical stamp must not write")
	assert.Equal(t, before, after.Annotations)
}

// TestAnEmptyRequesterDoesNotErasePriorAttribution.
//
// An absent canonical id is a real state — a kubectl-driven session has none —
// and writing "" would overwrite a good value from an earlier inbound. The
// runner reads that annotation to pick the authz subject, so erasing it turns
// a correct attribution into no attribution.
func TestAnEmptyRequesterDoesNotErasePriorAttribution(t *testing.T) {
	sess := preTurnSession(t, map[string]string{canonAnnotation: "user:someone"})
	p, c := newPreTurnPipeline(t, sess)

	p.stampPreTurnAnnotations(context.Background(), sess,
		channelkinds.InboundEvent{
			PreTurnAnnotations:             map[string]string{tsAnnotation: "1700000000.000002"},
			RequesterCanonicalIDAnnotation: canonAnnotation,
		}, "")

	got := storedAnnotations(t, c)
	assert.Equal(t, "1700000000.000002", got[tsAnnotation], "the anchor still updates")
	assert.Equal(t, "user:someone", got[canonAnnotation],
		"an unavailable canonical must not erase the one an earlier inbound established")
}

// TestAKindThatAsksForNothingIsNotWritten: every other channel kind supplies
// neither field, and must cost no API write at all.
func TestAKindThatAsksForNothingIsNotWritten(t *testing.T) {
	sess := preTurnSession(t, nil)
	p, c := newPreTurnPipeline(t, sess)

	ok := p.stampPreTurnAnnotations(context.Background(), sess, channelkinds.InboundEvent{}, "user:someone")

	assert.False(t, ok,
		"nothing was asked for and nothing was written, so a kind with its own fallback must still run it")
	assert.Empty(t, storedAnnotations(t, c))
}

// TestAFailedStampTellsTheKindToFallBack. The inbound is already appended and
// worth delivering, so this must not fail the turn — but the kind's own
// post-Deliver stamp is the only remaining writer and has to be told to run,
// even though it races the turn it is chasing.
func TestAFailedStampTellsTheKindToFallBack(t *testing.T) {
	sess := preTurnSession(t, nil)
	// An empty client: the session does not exist, so the patch fails.
	c := fake.NewClientBuilder().WithScheme(newWakeTestScheme(t)).Build()
	p := &Pipeline{K8s: c}

	ok := p.stampPreTurnAnnotations(context.Background(), sess,
		channelkinds.InboundEvent{
			PreTurnAnnotations:             map[string]string{tsAnnotation: "1700000000.000001"},
			RequesterCanonicalIDAnnotation: canonAnnotation,
		}, "user:someone")

	assert.False(t, ok, "a failed write must not report itself as stamped, or the fallback never runs")
}

// TestANewSessionIsBORNWithTheAnnotations is the case the first fix missed.
//
// A session's FIRST message creates it, and that path wakes nothing — the
// operator starts the runner when it observes the new object — so there is no
// later moment guaranteed to precede the turn. The annotations therefore go on
// at construction, and the property to assert is stronger than "written
// early": no version of the object without them is ever visible.
func TestANewSessionIsBORNWithTheAnnotations(t *testing.T) {
	got := withPreTurnAnnotations(
		map[string]string{"existing": "kept"},
		channelkinds.InboundEvent{
			PreTurnAnnotations:             map[string]string{tsAnnotation: "1700000000.000001"},
			RequesterCanonicalIDAnnotation: canonAnnotation,
		}, "user:someone")

	assert.Equal(t, "1700000000.000001", got[tsAnnotation],
		"the reply's thread anchor must be present from the object's first moment")
	assert.Equal(t, "user:someone", got[canonAnnotation])
	assert.Equal(t, "kept", got["existing"],
		"and the started-by annotations the session is created with must survive")
}

// TestANilAnnotationMapIsSafe: inheritedAnnotations may hand back nil, and a
// create path must not panic assigning into it.
func TestANilAnnotationMapIsSafe(t *testing.T) {
	got := withPreTurnAnnotations(nil, channelkinds.InboundEvent{
		PreTurnAnnotations:             map[string]string{tsAnnotation: "1700000000.000001"},
		RequesterCanonicalIDAnnotation: canonAnnotation,
	}, "user:someone")
	assert.Equal(t, "1700000000.000001", got[tsAnnotation])
}

// TestAKindThatAsksForNothingAddsNothingAtCreate mirrors the churn pin on the
// patch path: every other channel kind supplies neither field and must leave
// the created object exactly as it was.
func TestAKindThatAsksForNothingAddsNothingAtCreate(t *testing.T) {
	base := map[string]string{"existing": "kept"}
	got := withPreTurnAnnotations(base, channelkinds.InboundEvent{}, "user:someone")
	assert.Equal(t, map[string]string{"existing": "kept"}, got)
}

// TestAnEmptyCanonicalIsNotWrittenAtCreate: absent is a real state (a
// kubectl-driven session has no canonical), and an empty-valued annotation is
// worse than an absent one — it reads as an attribution that was made.
func TestAnEmptyCanonicalIsNotWrittenAtCreate(t *testing.T) {
	got := withPreTurnAnnotations(nil, channelkinds.InboundEvent{
		PreTurnAnnotations:             map[string]string{tsAnnotation: "1700000000.000001"},
		RequesterCanonicalIDAnnotation: canonAnnotation,
	}, "")
	assert.NotContains(t, got, canonAnnotation,
		"an unavailable canonical must be absent, never present-and-empty")
}
