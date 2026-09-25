package hold

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/testfixtures"
	"github.com/authzed/openagentprimitives/pkg/memory"
)

func trifectaSession() *spiceboxv1alpha1.AgentSession {
	return &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "child", Namespace: "ns"},
	}
}

// tripperOver builds a tripper whose every fact is supplied as
// operator-visible state.
func tripperOver(t *testing.T, c client.Client, untrusted bool, tagReaders, audience []string, canAct bool) *TrifectaTripper {
	t.Helper()
	return NewTrifectaTripper(TrifectaTripperDeps{
		Enabled: true,
		Client:  c,
		BoundTagsOf: func(context.Context, *spiceboxv1alpha1.AgentSession) ([]string, error) {
			return []string{"ptt-1"}, nil
		},
		TagCarriesUntrusted: func(context.Context, string) (bool, error) { return untrusted, nil },
		TagReaders:          func(context.Context, string) ([]string, error) { return tagReaders, nil },
		ChildAudience: func(context.Context, *spiceboxv1alpha1.AgentSession) ([]string, error) {
			return audience, nil
		},
		CanAct: func(context.Context, *spiceboxv1alpha1.AgentSession) (bool, error) { return canAct, nil },
	})
}

func appendSignal() memory.Signal {
	return memory.Signal{Kind: memory.SignalEntryAppended, Scope: memory.Scope{Kind: "session", ID: "ns/child"}}
}

func newClientWith(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	sch := testfixtures.NewScheme(t)
	return fake.NewClientBuilder().WithScheme(sch).WithObjects(objs...).Build()
}

func holdsIn(t *testing.T, c client.Client) []spiceboxv1alpha1.SessionHold {
	t.Helper()
	var list spiceboxv1alpha1.SessionHoldList
	require.NoError(t, c.List(context.Background(), &list))
	return list.Items
}

// TestTripsWhenAllThreeLegsHold is the containment judgement firing.
func TestTripsWhenAllThreeLegsHold(t *testing.T) {
	c := newClientWith(t, trifectaSession())
	// Untrusted tag, readable only by tim, on a child visible to sarah too,
	// with a class that can act.
	tr := tripperOver(t, c, true, []string{"user:tim"}, []string{"user:tim", "user:sarah"}, true)

	require.NoError(t, tr.OnSignal(context.Background(), appendSignal()))

	held := holdsIn(t, c)
	require.Len(t, held, 1)
	assert.Equal(t, "trifecta-closure-child", held[0].Name)
	assert.Equal(t, "tripper/trifecta-closure", held[0].Spec.Source)
	assert.Contains(t, held[0].Spec.Reason, "untrusted")
}

// TestDoesNotTripOnTwoLegs — the same near-miss discipline as the gate. A
// containment control that froze sessions on two legs would freeze ordinary
// work, and one that does that gets switched off.
func TestDoesNotTripOnTwoLegs(t *testing.T) {
	cases := []struct {
		name       string
		untrusted  bool
		tagReaders []string
		audience   []string
		canAct     bool
	}{
		{name: "not untrusted", untrusted: false, tagReaders: []string{"user:tim"}, audience: []string{"user:tim", "user:sarah"}, canAct: true},
		{name: "audience already covered", untrusted: true, tagReaders: []string{"user:tim", "user:sarah"}, audience: []string{"user:tim"}, canAct: true},
		{name: "cannot act", untrusted: true, tagReaders: []string{"user:tim"}, audience: []string{"user:tim", "user:sarah"}, canAct: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newClientWith(t, trifectaSession())
			tr := tripperOver(t, c, tc.untrusted, tc.tagReaders, tc.audience, tc.canAct)

			require.NoError(t, tr.OnSignal(context.Background(), appendSignal()))
			assert.Empty(t, holdsIn(t, c), "two legs must not freeze a session")
		})
	}
}

// TestTheJudgementNeedsNothingFromTheRunner is the property this whole
// component exists for.
//
// hold's package doc: "the evidence it reads is produced by the runner, and the
// runner is the party under suspicion — a compromised one would simply decline
// to trip itself." A tripper that read runner-written trifecta audit records
// would be defeated by that runner writing none.
//
// So this asserts the judgement is reached with NO audit records anywhere: the
// store is empty, nothing was ever reported, and the hold is created anyway
// from the controller-written status, the tags' own SpiceDB facts and the
// class's declared tools.
func TestTheJudgementNeedsNothingFromTheRunner(t *testing.T) {
	c := newClientWith(t, trifectaSession())
	tr := tripperOver(t, c, true, []string{"user:tim"}, []string{"user:tim", "user:sarah"}, true)

	// No memory, no audit records, no cooperation of any kind — the tripper is
	// given a Client and its own lookups and nothing else.
	require.NoError(t, tr.OnSignal(context.Background(), appendSignal()))

	assert.Len(t, holdsIn(t, c), 1,
		"the containment judgement must not depend on the party under suspicion having reported anything")
}

// TestDisabledByDefault: containment that freezes sessions is opt-in.
func TestDisabledByDefault(t *testing.T) {
	c := newClientWith(t, trifectaSession())
	tr := NewTrifectaTripper(TrifectaTripperDeps{
		Client: c, // Enabled omitted
		BoundTagsOf: func(context.Context, *spiceboxv1alpha1.AgentSession) ([]string, error) {
			return []string{"ptt-1"}, nil
		},
		CanAct: func(context.Context, *spiceboxv1alpha1.AgentSession) (bool, error) { return true, nil },
	})
	require.NoError(t, tr.OnSignal(context.Background(), appendSignal()))
	assert.Empty(t, holdsIn(t, c))
}

// TestARepeatedSignalDoesNotCreateASecondHold.
//
// SignalEntryAppended fires after EVERY append-only Put, so a trifecta session
// produces a stream of them. The hold name is deterministic so the second and
// subsequent trips are AlreadyExists no-ops rather than one hold per append.
func TestARepeatedSignalDoesNotCreateASecondHold(t *testing.T) {
	c := newClientWith(t, trifectaSession())
	tr := tripperOver(t, c, true, []string{"user:tim"}, []string{"user:tim", "user:sarah"}, true)

	for range 3 {
		require.NoError(t, tr.OnSignal(context.Background(), appendSignal()))
	}
	assert.Len(t, holdsIn(t, c), 1, "a repeated signal must not stack holds")
}

// TestAnUnresolvableLegSurfaces rather than concluding the closure is clean.
//
// The signal dispatcher logs and retries on an error; a swallowed one would
// mean a closure nobody could characterise was silently treated as fine.
func TestAnUnresolvableLegSurfaces(t *testing.T) {
	c := newClientWith(t, trifectaSession())
	tr := NewTrifectaTripper(TrifectaTripperDeps{
		Enabled: true, Client: c,
		BoundTagsOf: func(context.Context, *spiceboxv1alpha1.AgentSession) ([]string, error) {
			return nil, errors.New("apiserver unavailable")
		},
		CanAct: func(context.Context, *spiceboxv1alpha1.AgentSession) (bool, error) { return true, nil },
	})

	err := tr.OnSignal(context.Background(), appendSignal())
	require.Error(t, err, "a closure whose legs cannot be established is not thereby clean")
	assert.Empty(t, holdsIn(t, c))
}

// TestAVanishedSessionIsNotAnError: gone before we looked means nothing to
// freeze and nothing wrong.
func TestAVanishedSessionIsNotAnError(t *testing.T) {
	c := newClientWith(t) // no session
	tr := tripperOver(t, c, true, []string{"user:tim"}, []string{"user:tim", "user:sarah"}, true)

	assert.NoError(t, tr.OnSignal(context.Background(), appendSignal()))
	assert.Empty(t, holdsIn(t, c))
}
