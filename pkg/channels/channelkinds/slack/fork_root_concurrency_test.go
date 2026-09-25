package slack

import (
	"context"
	"testing"
	"time"

	slackapi "github.com/slack-go/slack"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

// blockingForkClient stalls PostMessageContext for one channel until the test
// releases it, standing in for a Slack call that is slow or rate-limited. Every
// other channel returns immediately. It overrides PostMessageContext rather
// than reusing fakeSlackClient's, whose call recording is not mutex-guarded.
type blockingForkClient struct {
	*fakeSlackClient
	stallChannel string
	entered      chan struct{} // closed on the first stalled call
	release      chan struct{} // test closes this to let the stalled call finish
}

func (c *blockingForkClient) PostMessageContext(_ context.Context, channelID string, _ ...slackapi.MsgOption) (string, string, error) {
	if channelID == c.stallChannel {
		select {
		case <-c.entered:
		default:
			close(c.entered)
		}
		<-c.release
	}
	return channelID, "root." + channelID, nil
}

func forkChild(ns, name string) *spiceboxv1alpha1.AgentSession {
	return &spiceboxv1alpha1.AgentSession{ObjectMeta: metav1.ObjectMeta{
		Namespace: ns, Name: name,
		Annotations: map[string]string{
			spiceboxv1alpha1.AnnotationForkedFromThread: "T1:C_PARENT:1783536805.645439",
		},
	}}
}

// TestForkRootCache_OneSlowPostDoesNotStallOtherSessions is the regression
// guard for a cross-session head-of-line block: ensure() took a single
// process-wide mutex and held it across a K8s Get plus up to two Slack posts.
// The cache is a per-Kind singleton handed to every sender, so one slow or
// rate-limited Slack call froze the fork-root path for every OTHER session too.
//
// The comment justifying that ("safe only because channelsd's outbound relay
// dispatches serially on one goroutine") is not true: Send is also reached from
// the status watchdog's tick and the session watcher, each resolving the same
// cached sender, and it calls ensure whenever the resolved thread_ts is empty.
func TestForkRootCache_OneSlowPostDoesNotStallOtherSessions(t *testing.T) {
	cli := &blockingForkClient{
		fakeSlackClient: &fakeSlackClient{},
		stallChannel:    "C_SLOW",
		entered:         make(chan struct{}),
		release:         make(chan struct{}),
	}
	k8s := fake.NewClientBuilder().WithScheme(forkRootScheme(t)).
		WithObjects(forkChild("ns", "slow"), forkChild("ns", "quick")).Build()
	cache := newForkRootCache()

	slowDone := make(chan struct{})
	go func() {
		defer close(slowDone)
		cache.ensure(context.Background(), cli, k8s,
			channelkinds.SessionInfo{Namespace: "ns", Name: "slow"}, "C_SLOW")
	}()
	select {
	case <-cli.entered:
	case <-time.After(5 * time.Second):
		close(cli.release)
		t.Fatal("the slow session never reached its Slack post")
	}

	quickDone := make(chan struct{})
	var quickRoot string
	go func() {
		defer close(quickDone)
		quickRoot = cache.ensure(context.Background(), cli, k8s,
			channelkinds.SessionInfo{Namespace: "ns", Name: "quick"}, "C_QUICK")
	}()

	select {
	case <-quickDone:
		assert.Equal(t, "root.C_QUICK", quickRoot, "the unrelated session posts its own root")
	case <-time.After(2 * time.Second):
		close(cli.release)
		<-slowDone
		t.Fatal("an unrelated session's fork root waited on another session's in-flight Slack post")
	}

	close(cli.release)
	<-slowDone
}

// TestForkRootCache_SameSessionStillPostsOneRoot pins the invariant the shared
// lock existed to protect: two senders racing for the SAME session must still
// produce exactly one root, whatever the locking granularity.
func TestForkRootCache_SameSessionStillPostsOneRoot(t *testing.T) {
	cli := &blockingForkClient{
		fakeSlackClient: &fakeSlackClient{},
		stallChannel:    "C_SLOW",
		entered:         make(chan struct{}),
		release:         make(chan struct{}),
	}
	k8s := fake.NewClientBuilder().WithScheme(forkRootScheme(t)).
		WithObjects(forkChild("ns", "slow")).Build()
	cache := newForkRootCache()
	sess := channelkinds.SessionInfo{Namespace: "ns", Name: "slow"}

	first := make(chan string, 1)
	go func() { first <- cache.ensure(context.Background(), cli, k8s, sess, "C_SLOW") }()
	select {
	case <-cli.entered:
	case <-time.After(5 * time.Second):
		close(cli.release)
		t.Fatal("the first caller never reached its Slack post")
	}

	second := make(chan string, 1)
	go func() { second <- cache.ensure(context.Background(), cli, k8s, sess, "C_SLOW") }()

	// The second caller for the same session must NOT race ahead and post a
	// competing root; it waits for the winner and reuses its answer.
	select {
	case ts := <-second:
		close(cli.release)
		t.Fatalf("a second sender for the same session posted its own root %q instead of waiting", ts)
	case <-time.After(200 * time.Millisecond):
	}

	close(cli.release)
	require.Equal(t, "root.C_SLOW", <-first, "the winner's post becomes the root")
	assert.Equal(t, "root.C_SLOW", <-second, "the loser reuses the winner's root")
}
