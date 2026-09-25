package slack

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	slackapi "github.com/slack-go/slack"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// scopeHeaderClient is a listenerAPIClient whose auth.test answers with a
// granted-scope header the test controls. It stands in for the Slack side of
// an app re-install: the bot token is unchanged, only the scopes Slack reports
// against it differ — which is exactly why nothing in the cluster changes when
// an operator follows the ScopesValid=False instruction.
type scopeHeaderClient struct {
	*stubClient

	mu     sync.Mutex
	header string
	err    error
	calls  int
}

func newScopeHeaderClient(header string) *scopeHeaderClient {
	return &scopeHeaderClient{stubClient: &stubClient{}, header: header}
}

func (c *scopeHeaderClient) AuthTestContext(context.Context) (*slackapi.AuthTestResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	if c.err != nil {
		return nil, c.err
	}
	hdr := http.Header{}
	hdr.Set("X-OAuth-Scopes", c.header)
	return &slackapi.AuthTestResponse{UserID: "UBOT", TeamID: "T-demo", Header: hdr}, nil
}

// grant replaces what Slack reports as granted, as a re-install does.
func (c *scopeHeaderClient) grant(header string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.header = header
}

// failWith makes every subsequent auth.test error.
func (c *scopeHeaderClient) failWith(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.err = err
}

func (c *scopeHeaderClient) callCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

// everyRequiredScope is a granted-scope header holding the full set the
// complete feature list needs — the token of an app installed with everything
// turned on.
const everyRequiredScope = everythingButFilesWrite + ",files:write"

// newRereadListener wires a listener with a Slack transport whose granted
// scopes the test drives, plus a frozen clock. Unlike newTestListener's
// K8s-only shape, this one can actually be asked what the app is granted now.
func newRereadListener(t *testing.T, ch *spiceboxv1alpha1.Channel, header string, classes ...*spiceboxv1alpha1.AgentClass) (*slackListener, *testClock, *scopeHeaderClient) {
	t.Helper()
	l := newTestListener(t, ch, classes...)
	clock := installClock(t, l)
	api := newScopeHeaderClient(header)
	l.api = api
	// What Start caches from its own auth.test, at the frozen "now".
	l.recordGrantedScopes(header)
	return l, clock, api
}

// tickFor runs the reconcile tick every 5s of the listener's clock for the
// given span, exactly as channelsd does: advance, list the Channel afresh,
// hand it to RefreshScopes.
func tickFor(t *testing.T, l *slackListener, clock *testClock, span time.Duration) {
	t.Helper()
	for elapsed := time.Duration(0); elapsed < span; elapsed += 5 * time.Second {
		clock.advance(5 * time.Second)
		l.RefreshScopes(context.Background(), refreshedChannel(t, l))
	}
}

// awaitAuthTestCalls waits for the out-of-band re-read to have reached Slack.
// The tick only SCHEDULES that call (it must not block channelsd's single
// reconcile goroutine on a Slack round-trip), so a test observes it by polling
// rather than by sleeping a fixed amount.
func awaitAuthTestCalls(t *testing.T, api *scopeHeaderClient, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if api.callCount() >= want {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("auth.test was called %d times, want at least %d", api.callCount(), want)
}

// awaitGrantedHeader waits for the cached granted-scope header to satisfy want.
func awaitGrantedHeader(t *testing.T, l *slackListener, want func(string) bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if h, known := l.grantedScopes(); known && want(h) {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	h, _ := l.grantedScopes()
	t.Fatalf("the cached granted-scope header never satisfied the condition; last value %q", h)
}

// TestScopesValidClearsAfterTheScopeIsGrantedOnTheSlackSide is the test that
// would have caught the live defect: the ScopesValid=False message tells the
// operator to add the scope and re-install the Slack app, they do exactly
// that, and the condition keeps reporting the scope missing.
//
// Nothing in the cluster changes when they comply — Slack grants the new scope
// on the SAME bot token — so the only thing that can notice is the listener
// asking Slack again. Every existing test in this package seeds the granted
// header directly and therefore passes while this is broken.
func TestScopesValidClearsAfterTheScopeIsGrantedOnTheSlackSide(t *testing.T) {
	class := newAgentClassWithCaps("demo-agent", "demo-ns", map[string]string{"attachments": `{}`})
	ch := newSlackChannel("demo-channel", "demo-ns", "demo-agent")
	l, clock, api := newRereadListener(t, ch, everythingButFilesWrite, class)

	l.RefreshScopes(context.Background(), ch)
	require.Equal(t, metav1.ConditionFalse, scopesValidStatus(t, l),
		"precondition: the app is missing files:write and the condition says so")
	require.Contains(t, scopesValidMessage(t, l), "files:write")

	// The operator does exactly what the message told them to: add the scope
	// under 'OAuth & Permissions', then 'Reinstall to Workspace'. Same token,
	// so no Secret, Channel or AgentClass in the cluster is touched.
	api.grant(everyRequiredScope)

	// Ticks pass. No restart of channelsd, no edit to anything in-cluster.
	tickFor(t, l, clock, scopeHeaderRecheckMissing)
	awaitGrantedHeader(t, l, func(h string) bool { return strings.Contains(h, "files:write") })
	l.RefreshScopes(context.Background(), refreshedChannel(t, l))

	assert.Equal(t, metav1.ConditionTrue, scopesValidStatus(t, l),
		"granting the scope Slack-side must clear ScopesValid on its own; needing a channelsd restart is the defect")
}

// TestGrantedScopeRereadOnAHealthyChannelIsThrottledButStillHappens pins both
// halves of the throttle. Riding the 5s tick unthrottled would be 12 auth.test
// calls per minute per Channel forever, against a rate limit this cluster does
// not control; never re-reading at all is the defect. A healthy Channel
// therefore re-reads on the slow interval — which is also the only thing that
// notices a scope REMOVED by a later re-install.
func TestGrantedScopeRereadOnAHealthyChannelIsThrottledButStillHappens(t *testing.T) {
	class := newAgentClassWithCaps("demo-agent", "demo-ns", map[string]string{"attachments": `{}`})
	ch := newSlackChannel("demo-channel", "demo-ns", "demo-agent")
	l, clock, api := newRereadListener(t, ch, everyRequiredScope, class)

	l.RefreshScopes(context.Background(), ch)
	require.Equal(t, metav1.ConditionTrue, scopesValidStatus(t, l), "precondition: every required scope is granted")

	tickFor(t, l, clock, scopeHeaderRecheckMissing)
	assert.Equal(t, 0, api.callCount(),
		"a Channel whose scopes are fine must not re-read auth.test on the fast interval, let alone every tick")

	// A later re-install trims files:write back out of the app.
	api.grant(everythingButFilesWrite)

	clock.advance(scopeHeaderRecheckGranted)
	l.RefreshScopes(context.Background(), refreshedChannel(t, l))
	awaitAuthTestCalls(t, api, 1)
	l.RefreshScopes(context.Background(), refreshedChannel(t, l))

	assert.Equal(t, metav1.ConditionFalse, scopesValidStatus(t, l),
		"past the slow interval the re-read must happen, or a revoked scope stays invisible until a restart")
	assert.Contains(t, scopesValidMessage(t, l), "files:write")
}

// TestGrantedScopeRereadFailureKeepsTheLastKnownHeader covers the transient
// Slack error. missingScopes reads an empty header as "everything missing", so
// clearing the cache on a failed re-read would flip a healthy Channel to a
// full-outage condition on one rate-limited call — the same flap activeFeatures
// already refuses to make on a failed AgentClass read.
func TestGrantedScopeRereadFailureKeepsTheLastKnownHeader(t *testing.T) {
	class := newAgentClassWithCaps("demo-agent", "demo-ns", map[string]string{"attachments": `{}`})
	ch := newSlackChannel("demo-channel", "demo-ns", "demo-agent")
	l, clock, api := newRereadListener(t, ch, everyRequiredScope, class)

	l.RefreshScopes(context.Background(), ch)
	require.Equal(t, metav1.ConditionTrue, scopesValidStatus(t, l), "precondition: the token is fine")

	api.failWith(errors.New("slack: ratelimited"))

	clock.advance(scopeHeaderRecheckGranted)
	l.RefreshScopes(context.Background(), refreshedChannel(t, l))
	awaitAuthTestCalls(t, api, 1)
	l.RefreshScopes(context.Background(), refreshedChannel(t, l))

	header, known := l.grantedScopes()
	assert.True(t, known, "a failed re-read must not un-answer the granted scopes")
	assert.Equal(t, everyRequiredScope, header, "a failed re-read must leave the last known header standing")
	assert.Equal(t, metav1.ConditionTrue, scopesValidStatus(t, l),
		"one failed auth.test must not report every scope as missing on a healthy Channel")
}

// TestGrantedScopeRereadBacksOffWhileSlackIsFailing pins the attempt-stamping.
// A failing re-read that did not count as a check would be retried on every
// one of the 5s ticks — hammering the very rate limit that is the most likely
// reason it is failing.
func TestGrantedScopeRereadBacksOffWhileSlackIsFailing(t *testing.T) {
	class := newAgentClassWithCaps("demo-agent", "demo-ns", map[string]string{"attachments": `{}`})
	ch := newSlackChannel("demo-channel", "demo-ns", "demo-agent")
	l, clock, api := newRereadListener(t, ch, everythingButFilesWrite, class)
	api.failWith(errors.New("slack: ratelimited"))

	l.RefreshScopes(context.Background(), ch)
	require.Equal(t, metav1.ConditionFalse, scopesValidStatus(t, l), "precondition: files:write is missing")

	// Two full fast intervals of ticks: one attempt is due per interval, no
	// more, however many ticks fall inside it.
	tickFor(t, l, clock, scopeHeaderRecheckMissing)
	awaitAuthTestCalls(t, api, 1)
	tickFor(t, l, clock, scopeHeaderRecheckMissing)
	awaitAuthTestCalls(t, api, 2)

	assert.Equal(t, 2, api.callCount(),
		"a failed re-read must count as a check, or a rate-limited Slack is retried on every 5s tick")
}

// TestScopesValidMessageDoesNotSendTheOperatorToRestartAnything is the prose
// half of the same defect. The remedy now works on its own, so the message has
// to say so — an operator who reads "re-install" and then watches the condition
// sit unchanged for a minute is exactly who reaches for a rollout restart.
func TestScopesValidMessageDoesNotSendTheOperatorToRestartAnything(t *testing.T) {
	class := newAgentClassWithCaps("demo-agent", "demo-ns", map[string]string{"attachments": `{}`})
	ch := newSlackChannel("demo-channel", "demo-ns", "demo-agent")
	l := newTestListener(t, ch, class)
	l.recordGrantedScopes(everythingButFilesWrite)

	l.RefreshScopes(context.Background(), ch)
	msg := scopesValidMessage(t, l)

	require.Contains(t, msg, "Reinstall to Workspace", "precondition: the message still carries the remedy")
	assert.Contains(t, strings.ToLower(msg), "on its own",
		"the message must tell the operator the condition clears by itself once the scope is granted")
	assert.Contains(t, strings.ToLower(msg), "nothing here needs restarting",
		"the message must rule out the restart an operator would otherwise try next")
}
