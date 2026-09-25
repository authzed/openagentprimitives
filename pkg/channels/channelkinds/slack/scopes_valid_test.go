package slack

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/controllers/conditions"
)

// newAgentClassWithCaps builds an AgentClass whose spec.capabilities carries
// the given raw JSON values, keyed by capability name (e.g.
// {"attachments": "{}"}). Values are taken verbatim so a test can supply a
// malformed one.
func newAgentClassWithCaps(name, namespace string, caps map[string]string) *spiceboxv1alpha1.AgentClass {
	ac := &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
	}
	if len(caps) > 0 {
		ac.Spec.Capabilities = map[string]apiextensionsv1.JSON{}
		for k, v := range caps {
			ac.Spec.Capabilities[k] = apiextensionsv1.JSON{Raw: []byte(v)}
		}
	}
	return ac
}

// newSlackChannel builds a slack-kind Channel bound to agentClass (empty for a
// Channel with no agent, e.g. a monitoring sink).
func newSlackChannel(name, namespace, agentClass string) *spiceboxv1alpha1.Channel {
	return &spiceboxv1alpha1.Channel{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: spiceboxv1alpha1.ChannelSpec{
			Kind:       KindName,
			AgentClass: agentClass,
		},
	}
}

// newTestListener wires a slackListener over a fake client seeded with ch plus
// any AgentClasses. No Slack transport is involved: every path under test here
// reads Kubernetes and the cached granted-scope header only.
func newTestListener(t *testing.T, ch *spiceboxv1alpha1.Channel, classes ...*spiceboxv1alpha1.AgentClass) *slackListener {
	t.Helper()
	objs := []runtime.Object{ch}
	for _, c := range classes {
		if c != nil {
			objs = append(objs, c)
		}
	}
	// WithStatusSubresource mirrors the real CRD (config/crds/…_channels.yaml
	// declares subresources.status), without which the fake client rejects
	// every Status().Patch as "not found" and this whole file would assert
	// against writes that never happened.
	cli := newFakeK8s(objs...).WithStatusSubresource(&spiceboxv1alpha1.Channel{}).Build()
	return &slackListener{deps: channelkinds.Deps{Channel: ch, K8sClient: cli}}
}

// liveChannel re-reads the listener's Channel from the fake API server, so
// assertions see what was actually patched rather than the in-memory copy.
func liveChannel(t *testing.T, l *slackListener) *spiceboxv1alpha1.Channel {
	t.Helper()
	var got spiceboxv1alpha1.Channel
	require.NoError(t, l.deps.K8sClient.Get(context.Background(), types.NamespacedName{
		Namespace: l.deps.Channel.Namespace, Name: l.deps.Channel.Name,
	}, &got), "re-read the Channel under test")
	return &got
}

// findCondition returns the named condition off the live Channel, or nil.
func findCondition(t *testing.T, l *slackListener, condType string) *metav1.Condition {
	t.Helper()
	return conditions.Find(liveChannel(t, l).Status.Conditions, condType)
}

// scopesValidStatus returns the live ScopesValid condition's status, or "" when
// no such condition has been written.
func scopesValidStatus(t *testing.T, l *slackListener) metav1.ConditionStatus {
	t.Helper()
	c := findCondition(t, l, spiceboxv1alpha1.ChannelConditionScopesValid)
	if c == nil {
		return ""
	}
	return c.Status
}

// scopesValidMessage returns the live ScopesValid condition's message, or "".
func scopesValidMessage(t *testing.T, l *slackListener) string {
	t.Helper()
	c := findCondition(t, l, spiceboxv1alpha1.ChannelConditionScopesValid)
	if c == nil {
		return ""
	}
	return c.Message
}

// everythingButFilesWrite is a granted-scope header holding every scope the
// full feature set needs EXCEPT files:write — the token of an app installed
// before anyone turned attachments on.
const everythingButFilesWrite = "app_mentions:read,chat:write,chat:write.customize,commands,im:write,assistant:write," +
	"channels:history,groups:history,im:history,users:read,users:read.email,files:read"

// testClock is an injectable clock for exercising agentClassRefreshInterval
// without sleeping.
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// installClock puts a controllable clock on the listener, so a test can move
// past the AgentClass re-read interval deliberately rather than by waiting.
func installClock(t *testing.T, l *slackListener) *testClock {
	t.Helper()
	c := &testClock{t: time.Now()}
	l.now = c.now
	return c
}

// countAgentClassReads wraps the listener's client so a test can prove how many
// times the bound AgentClass was actually fetched.
func countAgentClassReads(t *testing.T, l *slackListener) *atomic.Int32 {
	t.Helper()
	var n atomic.Int32
	inner := l.deps.K8sClient
	l.deps.K8sClient = &classReadCounter{Client: inner, n: &n}
	return &n
}

type classReadCounter struct {
	client.Client
	n *atomic.Int32
}

func (c *classReadCounter) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if _, ok := obj.(*spiceboxv1alpha1.AgentClass); ok {
		c.n.Add(1)
	}
	return c.Client.Get(ctx, key, obj, opts...)
}

// refreshedChannel re-reads the Channel and hands it back the way channelsd
// does: the manager lists Channels at the top of every tick and passes the
// fresh object to RefreshScopes.
func refreshedChannel(t *testing.T, l *slackListener) *spiceboxv1alpha1.Channel {
	t.Helper()
	return liveChannel(t, l)
}

func TestRequiredScopesDependsOnTheBoundAgentClass(t *testing.T) {
	cases := []struct {
		name       string
		class      *spiceboxv1alpha1.AgentClass
		wantScope  string
		wantAbsent string
	}{
		{
			name:       "class without attachments: files:write is NOT required",
			class:      newAgentClassWithCaps("demo-agent", "demo-ns", nil),
			wantScope:  "chat:write",
			wantAbsent: "files:write",
		},
		{
			name:      "class granting attachments: files:write IS required",
			class:     newAgentClassWithCaps("demo-agent", "demo-ns", map[string]string{"attachments": `{}`}),
			wantScope: "files:write",
		},
		{
			name: "class disabling thread and channel history: channels:history is NOT required",
			class: newAgentClassWithCaps("demo-agent", "demo-ns", map[string]string{
				"thread_history": `{"enabled":false}`, "channel_history": `{"enabled":false}`,
			}),
			wantScope:  "chat:write",
			wantAbsent: "channels:history",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ch := newSlackChannel("demo-channel", "demo-ns", "demo-agent")
			l := newTestListener(t, ch, tc.class)

			got := l.requiredScopes(context.Background(), ch)
			assert.Contains(t, got, tc.wantScope)
			if tc.wantAbsent != "" {
				assert.NotContains(t, got, tc.wantAbsent)
			}
		})
	}
}

// TestRequiredScopesForAChannelBindingNoAgentClassUsesBaseline covers a Channel
// with an empty spec.agentClass. (A monitoring Channel is the archetype, but it
// never reaches this code — channelManager.reconcile skips role=monitoring
// before startListener, so one never has a listener at all.)
func TestRequiredScopesForAChannelBindingNoAgentClassUsesBaseline(t *testing.T) {
	ch := newSlackChannel("demo-unbound", "demo-ns", "")
	l := newTestListener(t, ch /* no AgentClass */)

	got := l.requiredScopes(context.Background(), ch)
	assert.Contains(t, got, "chat:write", "a Channel with no agent must still be able to post")
	assert.NotContains(t, got, "files:write", "no agent means no attachments")
}

func TestRequiredScopesWhenTheClassIsMissingFallsBackToBaseline(t *testing.T) {
	// The Channel names an AgentClass that does not exist (deleted, or not yet
	// created). Fail SAFE here: report only the baseline rather than crying
	// about every optional scope.
	ch := newSlackChannel("demo-channel", "demo-ns", "gone-agent")
	l := newTestListener(t, ch /* no AgentClass */)

	got := l.requiredScopes(context.Background(), ch)
	assert.Contains(t, got, "chat:write")
	assert.NotContains(t, got, "files:write")
}

func TestRefreshScopesValidRecomputesAfterACapabilityEdit(t *testing.T) {
	class := newAgentClassWithCaps("demo-agent", "demo-ns", nil)
	ch := newSlackChannel("demo-channel", "demo-ns", "demo-agent")
	l := newTestListener(t, ch, class)
	clock := installClock(t, l)

	// The app has everything EXCEPT files:write.
	l.recordGrantedScopes(everythingButFilesWrite)

	l.refreshScopesValid(context.Background(), ch)
	assert.Equal(t, metav1.ConditionTrue, scopesValidStatus(t, l),
		"an agent with no attachments capability must not be flagged for files:write")

	// Grant attachments; the same token is now genuinely insufficient.
	class.Spec.Capabilities = map[string]apiextensionsv1.JSON{
		"attachments": {Raw: []byte(`{}`)},
	}
	require.NoError(t, l.deps.K8sClient.Update(context.Background(), class))

	// The AgentClass is re-read once per agentClassRefreshInterval, not once
	// per tick; a capability edit surfaces within that bound.
	clock.advance(agentClassRefreshInterval)
	l.refreshScopesValid(context.Background(), refreshedChannel(t, l))
	assert.Equal(t, metav1.ConditionFalse, scopesValidStatus(t, l),
		"granting attachments must make the missing files:write scope surface")
	assert.Contains(t, scopesValidMessage(t, l), "files:write")
	assert.Contains(t, scopesValidMessage(t, l), "artifact attachments are dropped",
		"the message must carry the feature's Degrades text, not just a scope name")
}

// TestRefreshScopesValidThrottlesTheAgentClassRead pins the cost of riding the
// 5s tick. channelsd's client is uncached and takes client-go's DefaultQPS of
// 5, so one AgentClass GET per Channel per tick is N/5 QPS — at 25 Channels the
// scope refresh alone saturates the limiter and inbound message handling queues
// behind it.
func TestRefreshScopesValidThrottlesTheAgentClassRead(t *testing.T) {
	class := newAgentClassWithCaps("demo-agent", "demo-ns", nil)
	ch := newSlackChannel("demo-channel", "demo-ns", "demo-agent")
	l := newTestListener(t, ch, class)
	clock := installClock(t, l)
	reads := countAgentClassReads(t, l)
	l.recordGrantedScopes(everythingButFilesWrite)

	// A minute of ticks at 5s.
	for range 12 {
		l.refreshScopesValid(context.Background(), refreshedChannel(t, l))
		clock.advance(5 * time.Second)
	}
	assert.Equal(t, int32(1), reads.Load(),
		"the bound AgentClass must be read once per refresh interval, not once per tick")

	clock.advance(agentClassRefreshInterval)
	l.refreshScopesValid(context.Background(), refreshedChannel(t, l))
	assert.Equal(t, int32(2), reads.Load(),
		"past the interval the class must be re-read, or a capability edit would never land")
}

// TestRefreshScopesValidPicksUpARebindWithoutWaitingForTheInterval covers
// re-pointing a Channel at a different agent. The listener's own Deps.Channel
// is the snapshot it started with and a running listener is never restarted for
// a spec edit, so the fresh Channel the caller passes is the only way this is
// ever seen — and it must not be delayed by the read interval, since the answer
// is for a different agent entirely.
func TestRefreshScopesValidPicksUpARebindWithoutWaitingForTheInterval(t *testing.T) {
	plain := newAgentClassWithCaps("demo-agent", "demo-ns", nil)
	attaching := newAgentClassWithCaps("other-agent", "demo-ns", map[string]string{"attachments": `{}`})
	ch := newSlackChannel("demo-channel", "demo-ns", "demo-agent")
	l := newTestListener(t, ch, plain, attaching)
	installClock(t, l) // frozen: no interval elapses during this test
	l.recordGrantedScopes(everythingButFilesWrite)

	l.refreshScopesValid(context.Background(), ch)
	require.Equal(t, metav1.ConditionTrue, scopesValidStatus(t, l))

	// Rebind the Channel to the attachment-using agent.
	live := liveChannel(t, l)
	live.Spec.AgentClass = "other-agent"
	require.NoError(t, l.deps.K8sClient.Update(context.Background(), live))

	l.refreshScopesValid(context.Background(), refreshedChannel(t, l))
	assert.Equal(t, metav1.ConditionFalse, scopesValidStatus(t, l),
		"a rebind must be honored on the next tick, not after the class-read interval")
	assert.Contains(t, scopesValidMessage(t, l), "files:write")
}

// TestRefreshScopesValidRestoresAConditionAnotherWriterErased is the listener's
// half of the clobber defect. A merge patch of status.conditions by any other
// writer replaces the list wholesale and can drop ScopesValid. Authority for
// "does this need writing?" is therefore the object, never a memo of what this
// listener last wrote — with a memo, the erasure is permanent.
func TestRefreshScopesValidRestoresAConditionAnotherWriterErased(t *testing.T) {
	class := newAgentClassWithCaps("demo-agent", "demo-ns", map[string]string{"attachments": `{}`})
	ch := newSlackChannel("demo-channel", "demo-ns", "demo-agent")
	l := newTestListener(t, ch, class)
	installClock(t, l)
	l.recordGrantedScopes(everythingButFilesWrite)

	l.refreshScopesValid(context.Background(), ch)
	require.Equal(t, metav1.ConditionFalse, scopesValidStatus(t, l), "the condition must be written first")

	// Another writer patches conditions from a stale base and drops ours.
	live := liveChannel(t, l)
	wiped := live.DeepCopy()
	wiped.Status.Conditions = nil
	require.NoError(t, l.deps.K8sClient.Status().Update(context.Background(), wiped))
	require.Nil(t, findCondition(t, l, spiceboxv1alpha1.ChannelConditionScopesValid), "precondition: erased")

	l.refreshScopesValid(context.Background(), refreshedChannel(t, l))
	assert.Equal(t, metav1.ConditionFalse, scopesValidStatus(t, l),
		"the next tick must restore a condition another writer erased")
}

func TestRefreshScopesValidIsANoOpBeforeTheFirstAuthTest(t *testing.T) {
	// grantedScopeHeader is empty until auth.test has run. Recomputing then
	// would stamp "everything missing" onto a Channel whose token is fine.
	class := newAgentClassWithCaps("demo-agent", "demo-ns", nil)
	ch := newSlackChannel("demo-channel", "demo-ns", "demo-agent")
	l := newTestListener(t, ch, class)

	l.refreshScopesValid(context.Background(), ch)
	assert.Nil(t, findCondition(t, l, spiceboxv1alpha1.ChannelConditionScopesValid),
		"no ScopesValid condition may be written before auth.test supplies the granted header")
}

// TestRefreshScopesValidAfterAuthTestReportsAnEmptyGrantedHeader keeps the
// no-op-before-auth.test rule from swallowing the case missingScopes exists to
// surface: Slack sets X-OAuth-Scopes on every successful response, so an EMPTY
// one after a successful auth.test is suspicious and must be reported, not
// mistaken for "not asked yet".
func TestRefreshScopesValidAfterAuthTestReportsAnEmptyGrantedHeader(t *testing.T) {
	class := newAgentClassWithCaps("demo-agent", "demo-ns", nil)
	ch := newSlackChannel("demo-channel", "demo-ns", "demo-agent")
	l := newTestListener(t, ch, class)

	l.recordGrantedScopes("")
	l.refreshScopesValid(context.Background(), ch)

	assert.Equal(t, metav1.ConditionFalse, scopesValidStatus(t, l),
		"an empty scope header from a successful auth.test must be surfaced")
	assert.Contains(t, scopesValidMessage(t, l), "chat:write",
		"the baseline scopes are all reported missing when the header says nothing was granted")
}

// TestScopesValidMessageOnlyExplainsFeaturesTheAgentActuallyHas is the other
// half of class-awareness: the required-scope SET narrows to the bound agent,
// and so must the prose. channels:history is required by BOTH ThreadHistory
// and ChannelHistory, so a message built from every declared feature would tell
// an operator that read_channel_history is broken on an agent that never had
// channel history in the first place.
func TestScopesValidMessageOnlyExplainsFeaturesTheAgentActuallyHas(t *testing.T) {
	class := newAgentClassWithCaps("demo-agent", "demo-ns", map[string]string{
		"channel_history": `{"enabled":false}`,
	})
	ch := newSlackChannel("demo-channel", "demo-ns", "demo-agent")
	l := newTestListener(t, ch, class)

	// Everything except the history scopes.
	l.recordGrantedScopes("app_mentions:read,chat:write,chat:write.customize,commands,im:write,assistant:write,users:read,users:read.email")
	l.refreshScopesValid(context.Background(), ch)

	msg := scopesValidMessage(t, l)
	assert.Contains(t, msg, "channels:history")
	assert.Contains(t, msg, "loses context between turns",
		"thread history is active on this agent, so its degradation must be explained")
	assert.NotContains(t, msg, "read_channel_history returns nothing",
		"channel history is disabled on this agent; claiming it is broken is a false alarm")
}

// TestRefreshScopesValidDoesNotRewriteStatusWhenNothingChanged pins the
// observation-write discipline: refreshScopesValid runs on channelsd's 5s tick,
// so patching unconditionally would bump the Channel's resourceVersion every
// five seconds forever and re-trigger every watcher of it.
func TestRefreshScopesValidDoesNotRewriteStatusWhenNothingChanged(t *testing.T) {
	class := newAgentClassWithCaps("demo-agent", "demo-ns", nil)
	ch := newSlackChannel("demo-channel", "demo-ns", "demo-agent")
	l := newTestListener(t, ch, class)
	installClock(t, l)
	l.recordGrantedScopes(everythingButFilesWrite)

	l.refreshScopesValid(context.Background(), ch)
	require.Equal(t, metav1.ConditionTrue, scopesValidStatus(t, l), "first refresh must write the condition")
	rv := liveChannel(t, l).ResourceVersion

	for range 3 {
		l.refreshScopesValid(context.Background(), refreshedChannel(t, l))
	}
	assert.Equal(t, rv, liveChannel(t, l).ResourceVersion,
		"a refresh whose answer is unchanged must not write to the API server")
}

// TestRefreshScopesValidDoesNotRewriteWhatAnotherListenerAlreadyWrote covers
// the restart path, where there is no memory of a previous write: a listener
// handed a Channel that predates the condition must fall through to the live
// object and find its answer already there, rather than re-writing it.
func TestRefreshScopesValidDoesNotRewriteWhatAnotherListenerAlreadyWrote(t *testing.T) {
	class := newAgentClassWithCaps("demo-agent", "demo-ns", nil)
	ch := newSlackChannel("demo-channel", "demo-ns", "demo-agent")
	first := newTestListener(t, ch, class)
	installClock(t, first)
	first.recordGrantedScopes(everythingButFilesWrite)
	first.refreshScopesValid(context.Background(), ch)
	require.Equal(t, metav1.ConditionTrue, scopesValidStatus(t, first))
	rv := liveChannel(t, first).ResourceVersion

	// A second listener over the same API server, with a STALE Channel (the
	// copy from before the condition existed) and no memory of any write.
	second := &slackListener{deps: channelkinds.Deps{Channel: ch, K8sClient: first.deps.K8sClient}}
	installClock(t, second)
	second.recordGrantedScopes(everythingButFilesWrite)
	second.refreshScopesValid(context.Background(), ch)

	assert.Equal(t, rv, liveChannel(t, first).ResourceVersion,
		"the live object already said this; re-writing it would churn every restart")
}

// TestRefreshScopesIsTheScopeRefresherEntryPoint pins the seam channelsd
// dispatches through: the manager holds a channelkinds.Listener and calls
// RefreshScopes only via the optional interface, handing over the Channel it
// just listed.
func TestRefreshScopesIsTheScopeRefresherEntryPoint(t *testing.T) {
	class := newAgentClassWithCaps("demo-agent", "demo-ns", nil)
	ch := newSlackChannel("demo-channel", "demo-ns", "demo-agent")
	l := newTestListener(t, ch, class)
	l.recordGrantedScopes(everythingButFilesWrite)

	var listener channelkinds.Listener = l
	r, ok := listener.(channelkinds.ScopeRefresher)
	require.True(t, ok, "the slack listener must be reachable as a ScopeRefresher")
	r.RefreshScopes(context.Background(), ch)

	assert.Equal(t, metav1.ConditionTrue, scopesValidStatus(t, l),
		"RefreshScopes must run the same recompute as refreshScopesValid")
}
