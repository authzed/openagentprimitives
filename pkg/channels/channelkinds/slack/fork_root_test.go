package slack

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

// TestForkRootText_LinksBackAndDatesTheOriginal pins the first message of a
// forked thread: it must say where the thread came from, link it, and say when
// the original was posted. A Slack thread_ts IS that timestamp.
func TestForkRootNotice_LinksBackAndDatesTheOriginal(t *testing.T) {
	// 1783536805.645439 → 2026-07-08T21:33:25Z
	got := forkRootNotice("T1", "C1", "1783536805.645439", time.Unix(1783536805, 0).UTC()).Args().Body

	assert.Contains(t, got, BuildSlackThreadURL("T1", "C1", "1783536805.645439"),
		"the original message must be one click away")
	assert.Contains(t, got, "2026", "say when the original was posted")
	assert.NotContains(t, got, "is thinking", "the root is framing, never a status update")
}

// TestForkRootText_UnknownTimeOmitsTheDateRatherThanInventOne pins the
// contract slackTSTime's doc promises: an unparseable thread_ts yields the zero
// time, and the root message must then say nothing about when — never render
// the zero instant as a real date. team_id/channel_id can still be well-formed,
// so the link survives; only the date drops.
func TestForkRootNotice_UnknownTimeOmitsTheDateRatherThanInventOne(t *testing.T) {
	got := forkRootNotice("T1", "C1", "not-a-timestamp", slackTSTime("not-a-timestamp")).Args().Body

	assert.Contains(t, got, BuildSlackThreadURL("T1", "C1", "not-a-timestamp"),
		"the link back does not depend on parsing the timestamp")
	assert.NotContains(t, got, "1 Jan 1", "the zero time must never be printed as the original's date")
	assert.NotContains(t, got, "0001", "the zero year must never leak into the message")
}

// TestForkRootText_NoLinkAndNoTimeStillReadsCleanly covers the doubly-degraded
// case: neither a usable thread URL nor a parseable timestamp. The message must
// still make sense and must not claim a date it does not have.
func TestForkRootNotice_NoLinkAndNoTimeStillReadsCleanly(t *testing.T) {
	n := forkRootNotice("", "", "", time.Time{})
	got := n.Args().Body

	assert.Contains(t, n.Args().Lead, "Continued from", "still explains where the thread came from")
	assert.Contains(t, got, "earlier conversation", "and says so in the body too")
	assert.NotContains(t, got, "0001", "the zero year must never leak into the message")
}

// TestBuildSlackThreadURL covers the happy path plus three missing-arg cases.
// forkRootText/forkRootCache above are BuildSlackThreadURL's only callers.
func TestBuildSlackThreadURL(t *testing.T) {
	cases := []struct {
		name               string
		team, ch, ts, want string
	}{
		{name: "all fields set: full URL", team: "T1", ch: "C1", ts: "1700000000.000001",
			want: "https://app.slack.com/client/T1/C1/thread/C1-1700000000.000001"},
		{name: "missing team_id: empty URL", team: "", ch: "C1", ts: "ts"},
		{name: "missing channel_id: empty URL", team: "T1", ch: "", ts: "ts"},
		{name: "missing thread_ts: empty URL", team: "T1", ch: "C1", ts: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, BuildSlackThreadURL(tc.team, tc.ch, tc.ts))
		})
	}
}

// TestSlackTSTime covers the parse-success and parse-failure paths: a valid
// "<epoch>.<micros>" ts resolves to the right instant, and an unparseable ts
// yields the zero time so callers can fall back to a link-free message.
func TestSlackTSTime(t *testing.T) {
	cases := []struct {
		name string
		ts   string
		want time.Time
	}{
		{
			name: "valid epoch.micros ts returns the corresponding UTC instant",
			ts:   "1783536805.645439",
			want: time.Unix(1783536805, 0).UTC(),
		},
		{
			name: "unparseable ts returns the zero time",
			ts:   "not-a-timestamp",
			want: time.Time{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := slackTSTime(tc.ts)
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestForkRootCache_FirstSenderCreatesTheRootExactlyOnce pins that the framing
// message is the thread root: whichever sender reaches the channel first posts
// it, every later caller threads beneath it, and it is posted exactly once.
func TestForkRootCache_FirstSenderCreatesTheRootExactlyOnce(t *testing.T) {
	c := &fakeSlackClient{postedTS: "777.1"}
	k8s := fake.NewClientBuilder().WithScheme(forkRootScheme(t)).WithObjects(
		&spiceboxv1alpha1.AgentSession{ObjectMeta: metav1.ObjectMeta{
			Namespace: "ns", Name: "child",
			Annotations: map[string]string{
				spiceboxv1alpha1.AnnotationForkedFromThread: "T1:C1:1783536805.645439",
			},
		}},
	).Build()
	cache := newForkRootCache()
	sess := channelkinds.SessionInfo{Namespace: "ns", Name: "child"}

	first := cache.ensure(context.Background(), c, k8s, sess, "C1")
	second := cache.ensure(context.Background(), c, k8s, sess, "C1")

	assert.Equal(t, "777.1", first, "the framing post becomes the thread root")
	assert.Equal(t, first, second, "later senders reuse the same root")
	// ensure() bundles two posts on the winning call: the framing root (new
	// channel/new thread) and the best-effort forward-link back in the parent
	// thread (BuildSlackThreadURL is non-empty whenever the annotation parsed,
	// so the forward-link branch always fires). The second cache.ensure call
	// above must add neither.
	require.Len(t, c.postMessageCalls, 2, "root posted once, forward-link posted once — no more on cache hits")
	assert.Equal(t, "C1", c.postMessageCalls[0].channelID, "the root is posted at channel level, creating a new thread")
}

// TestForkRootCache_NonForkSessionPostsNothing pins that an ordinary session is
// untouched: no annotation, no framing message, no root.
func TestForkRootCache_NonForkSessionPostsNothing(t *testing.T) {
	c := &fakeSlackClient{postedTS: "777.1"}
	k8s := fake.NewClientBuilder().WithScheme(forkRootScheme(t)).WithObjects(
		&spiceboxv1alpha1.AgentSession{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "plain"}},
	).Build()

	got := newForkRootCache().ensure(context.Background(), c, k8s,
		channelkinds.SessionInfo{Namespace: "ns", Name: "plain"}, "C1")

	assert.Empty(t, got, "not a fork child")
	assert.Empty(t, c.postMessageCalls, "no framing message for an ordinary session")
}

// TestForkRootCache_FailedRootPostIsNotCachedARetrySucceeds pins the
// asymmetry between success and failure: caching "" on a failed post would
// permanently strand a fork child with no root (every later sender would
// read the cached "" and skip posting forever). A later call must retry and,
// once the post succeeds, cache that root exactly like the first-try case.
func TestForkRootCache_FailedRootPostIsNotCachedARetrySucceeds(t *testing.T) {
	c := &fakeSlackClient{
		postedTS:        "recovered.1",
		postMessageErrs: []error{errors.New("rate_limited")}, // only the first call fails
	}
	k8s := fake.NewClientBuilder().WithScheme(forkRootScheme(t)).WithObjects(
		&spiceboxv1alpha1.AgentSession{ObjectMeta: metav1.ObjectMeta{
			Namespace: "ns", Name: "child",
			Annotations: map[string]string{
				spiceboxv1alpha1.AnnotationForkedFromThread: "T1:C1:1783536805.645439",
			},
		}},
	).Build()
	cache := newForkRootCache()
	sess := channelkinds.SessionInfo{Namespace: "ns", Name: "child"}

	failed := cache.ensure(context.Background(), c, k8s, sess, "C1")
	retried := cache.ensure(context.Background(), c, k8s, sess, "C1")

	assert.Empty(t, failed, "a failed post must not fabricate a root")
	assert.Equal(t, "recovered.1", retried, "the retry succeeds and becomes the root")
}

func forkRootScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	sch := runtime.NewScheme()
	require.NoError(t, spiceboxv1alpha1.AddToScheme(sch))
	return sch
}

// The thread a reader is looking at is finished once the session restarts
// elsewhere, so the forward-link must be TERMINAL: that is what stops someone
// waiting in a dead thread for a reply that will never arrive.
func TestThreadMovedNotice_IsTerminal(t *testing.T) {
	withRealCategories(t)
	n := threadMovedNotice("https://app.slack.com/client/T1/C1/thread/C1-1.2")
	cat, ok := channelinteractions.Get(n.Category())
	require.True(t, ok, "the category must be registered")
	assert.True(t, cat.Terminal, "the old thread has nothing more coming")
	assert.Contains(t, n.Args().Body, "the new thread", "and it must link where to go instead")
}

// The fork root heads a LIVE thread, so it must not be terminal — a square
// there would tell a reader to stop waiting in the thread they are meant to
// use.
func TestForkRootNotice_IsNotTerminal(t *testing.T) {
	withRealCategories(t)
	cat, ok := channelinteractions.Get(forkRootNotice("T1", "C1", "1.2", time.Time{}).Category())
	require.True(t, ok, "the category must be registered")
	assert.False(t, cat.Terminal, "the forked thread is where the conversation continues")
}
