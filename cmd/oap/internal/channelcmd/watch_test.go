package channelcmd

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/aptest"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/cli/tui"
)

// cond is one status condition, for a fixture.
func cond(condType string, status metav1.ConditionStatus, reason, message string) metav1.Condition {
	return metav1.Condition{Type: condType, Status: status, Reason: reason, Message: message}
}

// channelWith builds a Channel carrying the given conditions.
func channelWith(name string, conds ...metav1.Condition) *spiceboxv1alpha1.Channel {
	return &spiceboxv1alpha1.Channel{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Status:     spiceboxv1alpha1.ChannelStatus{Conditions: conds},
	}
}

// watchClient is a controller client holding just the given Channels.
func watchClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	return fake.NewClientBuilder().WithScheme(aptest.Scheme(t)).WithObjects(objs...).Build()
}

// TestClassifyChannel is the whole decision table of the watch, exercised
// against the conditions the operator and channelsd actually write.
func TestClassifyChannel(t *testing.T) {
	cases := []struct {
		name         string
		conds        []metav1.Condition
		wantState    channelState
		wantBlocker  string // condition type named on a blocked verdict
		wantScopeGap bool
	}{
		{
			name:      "no conditions yet (the second after the apply): pending, not an outcome",
			conds:     nil,
			wantState: channelPending,
		},
		{
			name:      "Valid=True but nothing has attached: still pending",
			conds:     []metav1.Condition{cond(spiceboxv1alpha1.ChannelConditionValid, metav1.ConditionTrue, spiceboxv1alpha1.ReasonChannelAllReferencesResolve, "")},
			wantState: channelPending,
		},
		{
			name:      "Connected=True: connected, with no scope gap",
			conds:     []metav1.Condition{cond(spiceboxv1alpha1.ChannelConditionConnected, metav1.ConditionTrue, spiceboxv1alpha1.ReasonChannelSocketAttached, "")},
			wantState: channelConnected,
		},
		{
			name: "Connected=True + ScopesValid=False: connected, scope gap reported alongside",
			conds: []metav1.Condition{
				cond(spiceboxv1alpha1.ChannelConditionConnected, metav1.ConditionTrue, spiceboxv1alpha1.ReasonChannelSocketAttached, ""),
				cond(spiceboxv1alpha1.ChannelConditionScopesValid, metav1.ConditionFalse, spiceboxv1alpha1.ReasonChannelMissingScopes, "missing scopes: files:read"),
			},
			wantState:    channelConnected,
			wantScopeGap: true,
		},
		{
			name: "ScopesValid=False on its own: pending — it must never decide the verdict",
			conds: []metav1.Condition{
				cond(spiceboxv1alpha1.ChannelConditionScopesValid, metav1.ConditionFalse, spiceboxv1alpha1.ReasonChannelMissingScopes, "missing scopes: files:read"),
			},
			wantState: channelPending,
		},
		{
			name:        "Valid=False: blocked, naming Valid",
			conds:       []metav1.Condition{cond(spiceboxv1alpha1.ChannelConditionValid, metav1.ConditionFalse, spiceboxv1alpha1.ReasonChannelAgentClassMissing, `AgentClass "demo-agent" not found`)},
			wantState:   channelBlocked,
			wantBlocker: spiceboxv1alpha1.ChannelConditionValid,
		},
		{
			name: "Connected=False with Valid=True (the listener refused the token): blocked, naming Connected",
			conds: []metav1.Condition{
				cond(spiceboxv1alpha1.ChannelConditionValid, metav1.ConditionTrue, spiceboxv1alpha1.ReasonChannelAllReferencesResolve, ""),
				cond(spiceboxv1alpha1.ChannelConditionConnected, metav1.ConditionFalse, spiceboxv1alpha1.ReasonChannelListenerStartFailed, "invalid_auth"),
			},
			wantState:   channelBlocked,
			wantBlocker: spiceboxv1alpha1.ChannelConditionConnected,
		},
		{
			name: "both False: blocked, naming Valid — SocketDetached is downstream of it",
			conds: []metav1.Condition{
				cond(spiceboxv1alpha1.ChannelConditionConnected, metav1.ConditionFalse, spiceboxv1alpha1.ReasonChannelSocketDetached, "Channel no longer Valid"),
				cond(spiceboxv1alpha1.ChannelConditionValid, metav1.ConditionFalse, spiceboxv1alpha1.ReasonChannelSecretMissing, `Secret "x-creds" not found`),
			},
			wantState:   channelBlocked,
			wantBlocker: spiceboxv1alpha1.ChannelConditionValid,
		},
		{
			name:        "InformationLeakageReady=False: blocked, naming it",
			conds:       []metav1.Condition{cond(spiceboxv1alpha1.ChannelConditionInformationLeakageReady, metav1.ConditionFalse, spiceboxv1alpha1.ReasonChannelKindLacksAudienceResolver, "no AudienceResolver")},
			wantState:   channelBlocked,
			wantBlocker: spiceboxv1alpha1.ChannelConditionInformationLeakageReady,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyChannel(channelWith("my-channel", tc.conds...))
			assert.Equal(t, tc.wantState, got.state, "state")
			if tc.wantBlocker == "" {
				assert.Nil(t, got.blockedBy, "no blocking condition may be named")
			} else if assert.NotNil(t, got.blockedBy, "a blocked verdict must name the condition that decided it") {
				assert.Equal(t, tc.wantBlocker, got.blockedBy.Type)
			}
			assert.Equal(t, tc.wantScopeGap, got.scopeGap != nil, "scope gap")
		})
	}
}

// TestWatchChannel_Outcomes drives the poll loop itself over a fake client,
// covering all three outcomes a run can end on. Green and red settle on the
// first poll (the fixture is already in that state), so only the timeout case
// spends any time — and it is given a deadline shorter than one interval.
func TestWatchChannel_Outcomes(t *testing.T) {
	ctx := context.Background()

	t.Run("Connected=True: returns connected before the deadline", func(t *testing.T) {
		c := watchClient(t, channelWith("my-channel",
			cond(spiceboxv1alpha1.ChannelConditionConnected, metav1.ConditionTrue, spiceboxv1alpha1.ReasonChannelSocketAttached, "")))
		v, err := watchChannel(ctx, c, "default", "my-channel", time.Millisecond, time.Second)
		require.NoError(t, err)
		assert.Equal(t, channelConnected, v.state)
	})

	t.Run("Valid=False: returns blocked, carrying the controller's own message", func(t *testing.T) {
		c := watchClient(t, channelWith("my-channel",
			cond(spiceboxv1alpha1.ChannelConditionValid, metav1.ConditionFalse,
				spiceboxv1alpha1.ReasonChannelAgentClassMissing, `AgentClass "demo-agent" not found`)))
		v, err := watchChannel(ctx, c, "default", "my-channel", time.Millisecond, time.Second)
		require.NoError(t, err)
		require.Equal(t, channelBlocked, v.state)
		require.NotNil(t, v.blockedBy)
		assert.Equal(t, `AgentClass "demo-agent" not found`, v.blockedBy.Message,
			"the message the controller wrote is the specific part; it must reach the user unrewritten")
	})

	t.Run("never settles: returns pending and no error, so the caller renders a timeout", func(t *testing.T) {
		c := watchClient(t, channelWith("my-channel"))
		v, err := watchChannel(ctx, c, "default", "my-channel", 50*time.Millisecond, 10*time.Millisecond)
		require.NoError(t, err, "a deadline is one of the three outcomes, not a failure of the watch")
		assert.Equal(t, channelPending, v.state)
	})

	t.Run("Channel absent: NotFound is not an error, it is not-there-yet", func(t *testing.T) {
		c := watchClient(t)
		v, err := watchChannel(ctx, c, "default", "my-channel", 50*time.Millisecond, 10*time.Millisecond)
		require.NoError(t, err)
		assert.Equal(t, channelPending, v.state)
	})

	t.Run("unreadable Channel: the read error is returned, not looped on", func(t *testing.T) {
		c := fake.NewClientBuilder().WithScheme(aptest.Scheme(t)).
			WithInterceptorFuncs(interceptor.Funcs{
				Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
					return assert.AnError
				},
			}).Build()
		_, err := watchChannel(ctx, c, "default", "my-channel", time.Millisecond, time.Second)
		require.Error(t, err, "a client that cannot read must not be watched in a loop")
		assert.Contains(t, err.Error(), "my-channel", "and the error must name what it failed to read")
	})
}

// TestRenderChannelVerdict pins the words each outcome puts in the user's
// scrollback — the whole point of the feature is that this line replaces eight
// bullets of prose, so what it says is the deliverable.
func TestRenderChannelVerdict(t *testing.T) {
	th := tui.NewTheme(tui.Caps{}) // no color: the output is byte-clean to assert on

	cases := []struct {
		name        string
		verdict     channelVerdict
		watchErr    error
		wantContain []string
		wantAbsent  []string
	}{
		{
			name:        "connected: one line, no follow-up command — there is nothing to debug",
			verdict:     channelVerdict{state: channelConnected},
			wantContain: []string{`Connected. Channel "my-channel" is live.`},
			wantAbsent:  []string{"oap channel show"},
		},
		{
			name: "connected with a scope gap: still green, plus the missing scopes and the follow-up",
			verdict: channelVerdict{
				state:    channelConnected,
				scopeGap: ptrCond(cond(spiceboxv1alpha1.ChannelConditionScopesValid, metav1.ConditionFalse, spiceboxv1alpha1.ReasonChannelMissingScopes, "missing scopes: files:read")),
			},
			wantContain: []string{"is live.", "ScopesValid=False (MissingScopes): missing scopes: files:read", "oap channel show my-channel"},
		},
		{
			name: "blocked: names the condition, its reason and the controller's message, then the follow-up",
			verdict: channelVerdict{
				state:     channelBlocked,
				blockedBy: ptrCond(cond(spiceboxv1alpha1.ChannelConditionValid, metav1.ConditionFalse, spiceboxv1alpha1.ReasonChannelAgentClassMissing, `AgentClass "demo-agent" not found`)),
			},
			wantContain: []string{`Not connected. Valid=False (AgentClassMissing): AgentClass "demo-agent" not found`, "oap channel show my-channel"},
		},
		{
			name:        "timeout: says it has not connected YET, and points at the same command",
			verdict:     channelVerdict{state: channelPending},
			wantContain: []string{"has not reported Connected=True within 30s", "oap channel show my-channel"},
		},
		{
			name:        "read error: surfaced, never swallowed, with the same follow-up",
			verdict:     channelVerdict{state: channelPending},
			watchErr:    assert.AnError,
			wantContain: []string{"Could not read the Channel's status:", "oap channel show my-channel"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			renderChannelVerdict(&out, th, "my-channel", tc.verdict, tc.watchErr, 30*time.Second)
			for _, want := range tc.wantContain {
				assert.Contains(t, out.String(), want)
			}
			for _, absent := range tc.wantAbsent {
				assert.NotContains(t, out.String(), absent)
			}
		})
	}
}

func ptrCond(c metav1.Condition) *metav1.Condition { return &c }

// TestChannelCreate_WaitFlagDecidesWhetherTheAppliedChannelIsWatched is the
// wiring: the run applies through the DYNAMIC client, so the controller client
// the watch reads never sees the Channel and the watch always times out. That
// makes the presence or absence of the watch's own output the observable
// difference between the two flag settings.
func TestChannelCreate_WaitFlagDecidesWhetherTheAppliedChannelIsWatched(t *testing.T) {
	cases := []struct {
		name string
		wait bool
	}{
		{name: "--wait: the run reports how the Channel it applied is doing", wait: true},
		{name: "--wait=false: the run ends at applied and says nothing about the Channel's status", wait: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b, _ := newFakeBundle(t)
			var out bytes.Buffer
			require.NoError(t, runChannelCreate(context.Background(), strings.NewReader(""), &out, aptest.GlobalsFor(b),
				channelCreateOptions{kind: "fake", apply: true, wait: tc.wait, timeout: 10 * time.Millisecond}))

			s := out.String()
			assert.Contains(t, s, "Applied to namespace default.", "both settings still apply")
			if tc.wait {
				assert.Contains(t, s, `Waiting for Channel "fake-channel" to connect`)
				assert.Contains(t, s, "oap channel show fake-channel")
			} else {
				assert.NotContains(t, s, "Waiting for Channel")
				assert.NotContains(t, s, "oap channel show")
			}
		})
	}
}

// TestChannelCreate_NoWatchWithoutAnApply: --apply=false prints the manifests
// and creates nothing, so there is no Channel to watch even with --wait on.
// Watching would poll for an object this run deliberately did not create and
// then report it as not connected — a failure invented by the report.
func TestChannelCreate_NoWatchWithoutAnApply(t *testing.T) {
	b, _ := newFakeBundle(t)
	var out bytes.Buffer
	require.NoError(t, runChannelCreate(context.Background(), strings.NewReader(""), &out, aptest.GlobalsFor(b),
		channelCreateOptions{kind: "fake", apply: false, wait: true, timeout: 10 * time.Millisecond}))

	assert.NotContains(t, out.String(), "Waiting for Channel")
	assert.NotContains(t, out.String(), "Not connected")
}

// TestChannelCreateCmd_WaitFlagDefaults pins the flag shape against the
// precedent `oap session approve` set: --wait defaults ON, with a --timeout
// beside it. A user who wants the old behaviour types --wait=false; nobody has
// to know the flag exists to get the report.
func TestChannelCreateCmd_WaitFlagDefaults(t *testing.T) {
	f := newChannelCreateCmd(&apcmd.Globals{}).Flags()

	waitFlag := f.Lookup("wait")
	require.NotNil(t, waitFlag, "--wait must exist")
	assert.Equal(t, "true", waitFlag.DefValue, "watching is the default; opting out is the flag")

	timeoutFlag := f.Lookup("timeout")
	require.NotNil(t, timeoutFlag, "--timeout must exist beside it")
	assert.Equal(t, channelWatchTimeout.String(), timeoutFlag.DefValue)
}
