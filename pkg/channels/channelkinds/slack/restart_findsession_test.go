package slack

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkey"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

// newFindSessionListener builds a minimal slackListener backed by a fake
// client seeded with sessions, for exercising findSessionByChannelKey in
// isolation.
func newFindSessionListener(t *testing.T, sessions ...*spiceboxv1alpha1.AgentSession) *slackListener {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, spiceboxv1alpha1.AddToScheme(scheme))
	channel := &spiceboxv1alpha1.Channel{
		ObjectMeta: metav1.ObjectMeta{Name: "slack-ch", Namespace: "default"},
		Spec:       spiceboxv1alpha1.ChannelSpec{Kind: KindName},
	}
	objs := make([]client.Object, 0, len(sessions)+1)
	objs = append(objs, channel)
	for _, s := range sessions {
		objs = append(objs, s)
	}
	cli := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	return &slackListener{
		deps: channelkinds.Deps{
			Channel:   channel,
			K8sClient: cli,
		},
	}
}

// sessionWithChannelKey builds an AgentSession labeled with the LabelChannelKey
// hash for (channelID, threadTS), so it is a candidate match for
// findSessionByChannelKey(channelID, threadTS).
func sessionWithChannelKey(name string, created time.Time, phase, supersededBy, channelID, threadTS string) *spiceboxv1alpha1.AgentSession {
	hash := channelkey.LabelValue("thread:" + channelID + ":" + threadTS)
	return &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         "default",
			CreationTimestamp: metav1.NewTime(created),
			Labels:            map[string]string{spiceboxv1alpha1.LabelChannelKey: hash},
		},
		Status: spiceboxv1alpha1.AgentSessionStatus{
			Phase:        phase,
			SupersededBy: supersededBy,
		},
	}
}

// TestFindSessionByChannelKey_SupersededFailedParentAndLivePendingChild_ReturnsChild
// pins the core Finding-1 fix: SupersedeParent deliberately leaves a
// transient-boot-Failed parent at Phase=Failed forever (only non-Failed
// parents settle to Succeeded), so "Phase != Succeeded" alone cannot tell a
// dead superseded parent from a live child — both read as non-Succeeded. The
// parent's SupersededBy must be the tie-breaker: once set, the parent is
// explicitly done and its successor owns the thread.
func TestFindSessionByChannelKey_SupersededFailedParentAndLivePendingChild_ReturnsChild(t *testing.T) {
	base := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	// Name "parent" sorts before "parent-icabc123" lexicographically, so a
	// naive first-match loop over list.Items (name order) would return the
	// parent first.
	parent := sessionWithChannelKey("parent", base, spiceboxv1alpha1.AgentSessionPhaseFailed, "parent-icabc123", "C1", "111.1")
	child := sessionWithChannelKey("parent-icabc123", base.Add(time.Minute), spiceboxv1alpha1.AgentSessionPhasePending, "", "C1", "111.1")

	l := newFindSessionListener(t, parent, child)
	got, err := l.findSessionByChannelKey(context.Background(), "C1", "111.1")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "parent-icabc123", got.Name, "the live child must win over the dead superseded parent")
}

// TestFindSessionByChannelKey_OrderingIndependence_NewestNonSucceededWinsRegardlessOfListOrder
// asserts that when two live (non-superseded, non-Succeeded) sessions share a
// channel key, the newest by CreationTimestamp wins even when its name would
// sort BEFORE the older session's name — i.e. the result must not depend on
// list.Items iteration order.
func TestFindSessionByChannelKey_OrderingIndependence_NewestNonSucceededWinsRegardlessOfListOrder(t *testing.T) {
	base := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	// "aaa-older" sorts before "zzz-newer" lexicographically, so a naive
	// first-match loop would return the older session first; the newest
	// CreationTimestamp must still win.
	older := sessionWithChannelKey("aaa-older", base, spiceboxv1alpha1.AgentSessionPhasePending, "", "C1", "111.1")
	newer := sessionWithChannelKey("zzz-newer", base.Add(time.Minute), spiceboxv1alpha1.AgentSessionPhasePending, "", "C1", "111.1")

	l := newFindSessionListener(t, older, newer)
	got, err := l.findSessionByChannelKey(context.Background(), "C1", "111.1")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "zzz-newer", got.Name, "the newest non-Succeeded session must win regardless of list order")
}

// TestFindSessionByChannelKey_OnlySucceededSessions_ReturnsNewest covers the
// case where every candidate has come to rest: the newest by
// CreationTimestamp is the one the restart shortcut should operate on.
func TestFindSessionByChannelKey_OnlySucceededSessions_ReturnsNewest(t *testing.T) {
	base := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	older := sessionWithChannelKey("older", base, spiceboxv1alpha1.AgentSessionPhaseSucceeded, "", "C1", "111.1")
	newer := sessionWithChannelKey("newer", base.Add(time.Minute), spiceboxv1alpha1.AgentSessionPhaseSucceeded, "", "C1", "111.1")

	l := newFindSessionListener(t, older, newer)
	got, err := l.findSessionByChannelKey(context.Background(), "C1", "111.1")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "newer", got.Name, "among only-Succeeded sessions, the newest must win")
}

// TestFindSessionByChannelKey_NoSessions_ReturnsNilNil confirms the
// no-match path stays (nil, nil) so handleRestartShortcut can render its
// "no session bound to this thread" modal.
func TestFindSessionByChannelKey_NoSessions_ReturnsNilNil(t *testing.T) {
	l := newFindSessionListener(t)
	got, err := l.findSessionByChannelKey(context.Background(), "C1", "111.1")
	require.NoError(t, err)
	assert.Nil(t, got)
}

// TestFindSessionByChannelKey_AllSuperseded_FallsBackToNewestOverall covers
// the mid-fork race where every session sharing the channel key has already
// been superseded (e.g. a grandparent superseded by a parent that was itself
// just superseded by a new child, and the label hasn't been re-queried yet).
// The shortcut must never strand the user with a nil result when there is a
// resolvable thread; it falls back to the newest session overall.
func TestFindSessionByChannelKey_AllSuperseded_FallsBackToNewestOverall(t *testing.T) {
	base := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	grandparent := sessionWithChannelKey("grandparent", base, spiceboxv1alpha1.AgentSessionPhaseSucceeded, "parent", "C1", "111.1")
	parent := sessionWithChannelKey("parent", base.Add(time.Minute), spiceboxv1alpha1.AgentSessionPhaseFailed, "parent-icabc123", "C1", "111.1")

	l := newFindSessionListener(t, grandparent, parent)
	got, err := l.findSessionByChannelKey(context.Background(), "C1", "111.1")
	require.NoError(t, err)
	require.NotNil(t, got, "must never return nil when a resolvable thread exists")
	assert.Equal(t, "parent", got.Name, "falls back to the newest session overall when everything is superseded")
}
