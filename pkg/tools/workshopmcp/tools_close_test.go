package workshopmcp

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/testfixtures"
)

// The builder session this file's Server acts for, and the Workshop CR that
// belongs to it — the one close_others writes its requests onto, and the one it
// must never accept as a target.
const (
	closeSessNS   = "builder-b"
	closeSessName = "builder-x"
)

func closeOwnWorkshopName() string { return spiceboxv1alpha1.WorkshopName(closeSessName) }

// shrinkClosePollForTest lowers the tool's wait so a test that exercises the
// timeout does not sit out the real 30s window — the fake client has no
// controller behind it, so a decision that is not pre-seeded never arrives.
// Mirrors shrinkApplyPollForTest (tools_apply_test.go).
func shrinkClosePollForTest(t *testing.T) {
	t.Helper()
	origInterval, origTimeout := closePollInterval, closePollTimeout
	closePollInterval = 5 * time.Millisecond
	closePollTimeout = 50 * time.Millisecond
	t.Cleanup(func() {
		closePollInterval, closePollTimeout = origInterval, origTimeout
	})
}

// closeWorkshopCR builds this session's own Workshop CR carrying the decisions
// the controller has (or has not) already written back.
func closeWorkshopCR(decisions ...spiceboxv1alpha1.WorkshopCloseStatus) *spiceboxv1alpha1.Workshop {
	return &spiceboxv1alpha1.Workshop{
		ObjectMeta: metav1.ObjectMeta{Namespace: closeSessNS, Name: closeOwnWorkshopName()},
		Status:     spiceboxv1alpha1.WorkshopStatus{CloseRequests: decisions},
	}
}

func closeDecision(target, phase, message string) spiceboxv1alpha1.WorkshopCloseStatus {
	now := metav1.Now()
	return spiceboxv1alpha1.WorkshopCloseStatus{Target: target, Phase: phase, Message: message, DecidedAt: &now}
}

// closeExpansion is one decision a WILDCARD ask produced, carrying the ask that
// made it. That stamp is what lets a second ask report its own answer rather
// than the first one's, so a fixture without it describes no ask at all.
func closeExpansion(ask, target, phase, message string) spiceboxv1alpha1.WorkshopCloseStatus {
	entry := closeDecision(target, phase, message)
	entry.Ask = ask
	return entry
}

// closeSecondAsk is the wildcard key the tool writes when one ask is already
// standing, and the key that ask's own answer comes back under.
var closeSecondAsk = spiceboxv1alpha1.WorkshopCloseTargetAll + ":2"

// newCloseServer builds the Server plus the client its Workshop CR lives in.
func newCloseServer(t *testing.T, objs ...client.Object) (*Server, client.Client) {
	t.Helper()
	c := fake.NewClientBuilder().WithScheme(testfixtures.NewScheme(t)).WithObjects(objs...).Build()
	return newCredServer("ws-abc123", closeSessNS, closeSessName, c), c
}

// closeRequestTargets reads back the spec.closeRequests targets the tool wrote.
func closeRequestTargets(t *testing.T, c client.Client) []string {
	t.Helper()
	var got spiceboxv1alpha1.Workshop
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: closeSessNS, Name: closeOwnWorkshopName()}, &got))
	targets := make([]string, 0, len(got.Spec.CloseRequests))
	for _, req := range got.Spec.CloseRequests {
		targets = append(targets, req.Target)
		assert.False(t, req.RequestedAt.IsZero(), "every request records when it was asked")
	}
	return targets
}

// closeMessageOf reads the result's message back as a string. An absent message
// is the empty string — "no caveat" and "the field is missing" mean the same
// thing to whoever reads this, and a nil here would blow up the assertion
// rather than failing it.
func closeMessageOf(t *testing.T, body map[string]any) string {
	t.Helper()
	if body["message"] == nil {
		return ""
	}
	msg, ok := body["message"].(string)
	require.True(t, ok, "result field %q must be a string, got %T", "message", body["message"])
	return msg
}

// stringsOf reads a JSON result array of strings back as a Go slice, so a test
// can assert on the three answer lists without repeating the type dance.
func stringsOf(t *testing.T, body map[string]any, field string) []string {
	t.Helper()
	raw, ok := body[field].([]any)
	require.True(t, ok, "result field %q must be a JSON array, got %T", field, body[field])
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		s, ok := v.(string)
		require.True(t, ok, "result field %q must hold strings, got %T", field, v)
		out = append(out, s)
	}
	return out
}

// TestCloseOthers_NoNamesAsksForEveryOtherWorkshop pins the wildcard: the
// sidecar cannot list the person's workshops (its Role is name-restricted to
// its own Workshop), so "all of mine" is ONE request the controller expands,
// never a list the tool assembled.
func TestCloseOthers_NoNamesAsksForEveryOtherWorkshop(t *testing.T) {
	shrinkClosePollForTest(t)
	s, c := newCloseServer(t, closeWorkshopCR())

	res := callTool(t, s.handleCloseOthers, closeOthersArgs{})
	require.False(t, res.IsError, "asking is not a failure even when nothing has been decided yet")

	assert.Equal(t, []string{spiceboxv1alpha1.WorkshopCloseTargetAll}, closeRequestTargets(t, c),
		"one wildcard request, not a list the tool guessed at")

	body := decodeResultBody(t, res)
	assert.Empty(t, stringsOf(t, body, "closed"))
	assert.Empty(t, stringsOf(t, body, "notFound"))
	assert.Contains(t, body["message"], "Still being decided",
		"a wait that settles nothing must say so rather than imply everything closed")
}

// Named workshops are requested as named, one entry each, and no wildcard is
// written beside them — a request for two must never close a third.
func TestCloseOthers_NamedWorkshopsAreRequestedIndividually(t *testing.T) {
	shrinkClosePollForTest(t)
	s, c := newCloseServer(t, closeWorkshopCR())

	res := callTool(t, s.handleCloseOthers, closeOthersArgs{Workshops: []string{"other-a-workshop", "other-b-workshop"}})
	require.False(t, res.IsError)

	assert.ElementsMatch(t, []string{"other-a-workshop", "other-b-workshop"}, closeRequestTargets(t, c))
	assert.NotContains(t, closeRequestTargets(t, c), spiceboxv1alpha1.WorkshopCloseTargetAll,
		"naming workshops must never widen into every workshop")
}

// The workshop the builder is IN is refused before anything is written, by
// every name a person might use for it: the session's or the workshop's, bare
// or with this builder's own namespace in front — the spelling the start
// route's refusal shows for a workshop outside the namespace being started in,
// and one a person may well repeat back about this one. The controller refuses
// self too, but a request that reaches the controller is a request that could
// have been recorded — the tool must not make one.
func TestCloseOthers_ItsOwnWorkshopIsRefusedWithoutAWrite(t *testing.T) {
	cases := []struct {
		name  string
		named string
	}{
		{name: "named by its workshop name: refused, nothing written", named: closeOwnWorkshopName()},
		{name: "named by its session name: refused, nothing written", named: closeSessName},
		{name: "named with its own namespace in front of the workshop name: refused, nothing written", named: closeSessNS + "/" + closeOwnWorkshopName()},
		{name: "named with its own namespace in front of the session name: refused, nothing written", named: closeSessNS + "/" + closeSessName},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			shrinkClosePollForTest(t)
			s, c := newCloseServer(t, closeWorkshopCR())

			res := callTool(t, s.handleCloseOthers, closeOthersArgs{Workshops: []string{tc.named}})
			require.False(t, res.IsError, "a refusal to close itself is an answer, not a tool failure")

			assert.Empty(t, closeRequestTargets(t, c), "nothing is recorded for a target the tool refuses outright")
			body := decodeResultBody(t, res)
			refused, ok := body["refused"].([]any)
			require.True(t, ok)
			require.Len(t, refused, 1)
			entry, ok := refused[0].(map[string]any)
			require.True(t, ok)
			assert.Equal(t, tc.named, entry["workshop"])
			assert.Equal(t, "the workshop you are in", entry["why"])
		})
	}
}

// Naming itself alongside a real target refuses only itself — the other
// workshop is still asked about.
func TestCloseOthers_ItselfAmongNamedTargetsRefusesOnlyItself(t *testing.T) {
	shrinkClosePollForTest(t)
	s, c := newCloseServer(t, closeWorkshopCR())

	res := callTool(t, s.handleCloseOthers, closeOthersArgs{Workshops: []string{closeOwnWorkshopName(), "other-a-workshop"}})
	require.False(t, res.IsError)

	assert.Equal(t, []string{"other-a-workshop"}, closeRequestTargets(t, c))
	body := decodeResultBody(t, res)
	refused, _ := body["refused"].([]any)
	require.Len(t, refused, 1)
	assert.Equal(t, []string{"other-a-workshop"}, stringsOf(t, body, "pending"),
		"the target that WAS asked about is still waiting on its answer")
}

// The three decisions the controller can write come back as the three lists,
// each carrying the controller's own plain words unchanged.
func TestCloseOthers_ReportsTheControllersDecisions(t *testing.T) {
	shrinkClosePollForTest(t)
	ws := closeWorkshopCR(
		closeDecision("other-a-workshop", spiceboxv1alpha1.WorkshopClosePhaseClosed, ""),
		closeDecision("other-b-workshop", spiceboxv1alpha1.WorkshopClosePhaseRefused, "not yours to close"),
		closeDecision("other-c-workshop", spiceboxv1alpha1.WorkshopClosePhaseNotFound, ""),
	)
	s, _ := newCloseServer(t, ws)

	res := callTool(t, s.handleCloseOthers, closeOthersArgs{
		Workshops: []string{"other-a-workshop", "other-b-workshop", "other-c-workshop"},
	})
	require.False(t, res.IsError)

	body := decodeResultBody(t, res)
	assert.Equal(t, []string{"other-a-workshop"}, stringsOf(t, body, "closed"))
	assert.Equal(t, []string{"other-c-workshop"}, stringsOf(t, body, "notFound"))
	assert.Empty(t, stringsOf(t, body, "pending"))
	assert.Empty(t, body["message"], "nothing is outstanding, so there is nothing to caveat")

	refused, ok := body["refused"].([]any)
	require.True(t, ok)
	require.Len(t, refused, 1)
	entry, _ := refused[0].(map[string]any)
	assert.Equal(t, "other-b-workshop", entry["workshop"])
	assert.Equal(t, "not yours to close", entry["why"], "the controller's words, not the tool's")
}

// A wildcard answer is read from the PER-TARGET entries THIS ask produced; the
// wildcard's own summary entry is what the tool waits for, and is never
// reported as if it were a workshop. A decision some other request made — a
// named call's, an earlier ask's — is not this ask's answer and is left out.
func TestCloseOthers_WildcardReportsEveryExpandedTarget(t *testing.T) {
	shrinkClosePollForTest(t)
	all := spiceboxv1alpha1.WorkshopCloseTargetAll
	ws := closeWorkshopCR(
		closeExpansion(all, "other-a-workshop", spiceboxv1alpha1.WorkshopClosePhaseClosed, ""),
		closeExpansion(all, "other-b-workshop", spiceboxv1alpha1.WorkshopClosePhaseRefused, "not yours to close"),
		closeExpansion(all, "other-ns/other-c-workshop", spiceboxv1alpha1.WorkshopClosePhaseClosed, ""),
		closeDecision("other-d-workshop", spiceboxv1alpha1.WorkshopClosePhaseClosed, ""),
		closeDecision(all, spiceboxv1alpha1.WorkshopClosePhaseClosed, "2 closed, 1 refused"),
	)
	s, _ := newCloseServer(t, ws)

	res := callTool(t, s.handleCloseOthers, closeOthersArgs{})
	require.False(t, res.IsError)

	body := decodeResultBody(t, res)
	// The names the PERSON sees: the builder-session name the page and the
	// start-route refusal show them, not the workshop name the expansion had to
	// decide under. A named call already answers under the name they gave; this
	// is the branch where nobody gave one.
	// A target the expansion decided in ANOTHER namespace keeps that
	// namespace: without it the person would be handed a bare name the tool
	// would resolve to their own builder namespace, naming a different
	// workshop or none at all.
	assert.Equal(t, []string{"other-a", "other-ns/other-c"}, stringsOf(t, body, "closed"))
	assert.NotContains(t, stringsOf(t, body, "closed"), "other-a-workshop",
		"a person is never shown a name they cannot say back")
	assert.NotContains(t, stringsOf(t, body, "closed"), "other-d",
		"a decision this ask did not make is not this ask's answer")
	assert.Empty(t, stringsOf(t, body, "pending"))
	assert.Equal(t, "2 closed, 1 refused", body["message"], "the operator's own summary reaches the person")
	refused, _ := body["refused"].([]any)
	require.Len(t, refused, 1)
	entry, _ := refused[0].(map[string]any)
	assert.Equal(t, "other-b", entry["workshop"])
	for _, field := range []string{"closed", "notFound", "pending"} {
		assert.NotContains(t, stringsOf(t, body, field), spiceboxv1alpha1.WorkshopCloseTargetAll,
			"the request itself is not a workshop and must never be listed as one in %q", field)
	}
}

// The operator refuses a wildcard outright when the workshop asking has nobody
// behind it. That refusal lives only in the summary's message — there are no
// per-target entries to carry it — so a switch that handled every phase but
// this one dropped the one sentence the person needed.
func TestCloseOthers_ARefusedWildcardCarriesTheControllersWords(t *testing.T) {
	shrinkClosePollForTest(t)
	ws := closeWorkshopCR(closeDecision(spiceboxv1alpha1.WorkshopCloseTargetAll,
		spiceboxv1alpha1.WorkshopClosePhaseRefused, "not yours to close"))
	s, _ := newCloseServer(t, ws)

	res := callTool(t, s.handleCloseOthers, closeOthersArgs{})
	require.False(t, res.IsError, "a refusal is an answer, not a tool failure")

	body := decodeResultBody(t, res)
	assert.Equal(t, "not yours to close", body["message"],
		"a refusal the operator wrote must reach the person rather than vanishing with its phase")
	assert.Empty(t, stringsOf(t, body, "closed"))
	assert.Empty(t, stringsOf(t, body, "pending"))
}

// answerCloseAsk stands in for the operator between two tool calls: it appends
// the decisions a fresh expansion would have written. The fake client has no
// controller behind it, so an ask nobody answers never settles.
func answerCloseAsk(t *testing.T, c client.Client, decisions ...spiceboxv1alpha1.WorkshopCloseStatus) {
	t.Helper()
	ctx := context.Background()
	var ws spiceboxv1alpha1.Workshop
	require.NoError(t, c.Get(ctx, client.ObjectKey{Namespace: closeSessNS, Name: closeOwnWorkshopName()}, &ws))
	ws.Status.CloseRequests = append(ws.Status.CloseRequests, decisions...)
	require.NoError(t, c.Update(ctx, &ws))
}

// A second "close everything" from one workshop ASKS AGAIN. The request list is
// keyed by target, so the tool writes a sequenced key of its own ("*:2") and
// the operator expands afresh onto it. What comes back is that ask's own
// answer — the workshop the person opened since — never the first ask's list
// handed back as if it were current.
func TestCloseOthers_ASecondCloseAllAsksAgainUnderItsOwnKey(t *testing.T) {
	shrinkClosePollForTest(t)
	all := spiceboxv1alpha1.WorkshopCloseTargetAll
	ws := closeWorkshopCR(
		closeExpansion(all, "other-a-workshop", spiceboxv1alpha1.WorkshopClosePhaseClosed, ""),
		closeDecision(all, spiceboxv1alpha1.WorkshopClosePhaseClosed, "1 closed, 0 refused"),
	)
	s, c := newCloseServer(t, ws)

	first := callTool(t, s.handleCloseOthers, closeOthersArgs{})
	require.False(t, first.IsError)
	firstBody := decodeResultBody(t, first)
	assert.Equal(t, []string{"other-a"}, stringsOf(t, firstBody, "closed"))
	require.Equal(t, []string{all}, closeRequestTargets(t, c), "the first ask writes the plain wildcard")

	// The operator answers the ask the second call is about to write.
	answerCloseAsk(t, c,
		closeExpansion(closeSecondAsk, "other-b-workshop", spiceboxv1alpha1.WorkshopClosePhaseClosed, ""),
		closeDecision(closeSecondAsk, spiceboxv1alpha1.WorkshopClosePhaseClosed, "1 closed, 0 refused"))

	second := callTool(t, s.handleCloseOthers, closeOthersArgs{})
	require.False(t, second.IsError)
	secondBody := decodeResultBody(t, second)

	assert.Equal(t, []string{all, closeSecondAsk}, closeRequestTargets(t, c),
		"a second ask is a request of its own, not a no-op on the first")
	assert.Equal(t, []string{"other-b"}, stringsOf(t, secondBody, "closed"),
		"its own expansion is the answer; the first ask's is not repeated back")
	assert.NotContains(t, stringsOf(t, secondBody, "closed"), "other-a")
	assert.Equal(t, "1 closed, 0 refused", closeMessageOf(t, secondBody), "the summary its OWN ask was given")
	assert.NotContains(t, closeMessageOf(t, secondBody), "cannot ask again",
		"it just did ask again; saying otherwise would send the person off to name workshops by hand")
}

// A person with nothing else open is told exactly that, in the controller's own
// words — not an empty answer they have to interpret.
func TestCloseOthers_WildcardWithNoOtherWorkshopsSaysSo(t *testing.T) {
	shrinkClosePollForTest(t)
	ws := closeWorkshopCR(closeDecision(spiceboxv1alpha1.WorkshopCloseTargetAll,
		spiceboxv1alpha1.WorkshopClosePhaseNotFound, "no other workshops of yours"))
	s, _ := newCloseServer(t, ws)

	res := callTool(t, s.handleCloseOthers, closeOthersArgs{})
	require.False(t, res.IsError)

	body := decodeResultBody(t, res)
	assert.Equal(t, "no other workshops of yours", body["message"])
	assert.Empty(t, stringsOf(t, body, "closed"))
	assert.Empty(t, stringsOf(t, body, "notFound"))
	assert.Empty(t, stringsOf(t, body, "pending"))
}

// A decision that has not arrived by the deadline is reported as outstanding,
// never guessed at: the settled ones are answered and the rest are named as
// still being decided.
func TestCloseOthers_UndecidedTargetsAreReportedAsPending(t *testing.T) {
	shrinkClosePollForTest(t)
	ws := closeWorkshopCR(closeDecision("other-a-workshop", spiceboxv1alpha1.WorkshopClosePhaseClosed, ""))
	s, _ := newCloseServer(t, ws)

	res := callTool(t, s.handleCloseOthers, closeOthersArgs{Workshops: []string{"other-a-workshop", "other-b-workshop"}})
	require.False(t, res.IsError)

	body := decodeResultBody(t, res)
	assert.Equal(t, []string{"other-a-workshop"}, stringsOf(t, body, "closed"))
	assert.Equal(t, []string{"other-b-workshop"}, stringsOf(t, body, "pending"))
	assert.Contains(t, body["message"], "Still being decided")
}

// A target already carrying a request is not requested again: the list is keyed
// by target, and a second entry for one would be a second decision to make about
// a workshop that may since have been replaced by a later build of the same name.
func TestCloseOthers_AnAlreadyRequestedTargetIsNotRequestedTwice(t *testing.T) {
	shrinkClosePollForTest(t)
	ws := closeWorkshopCR()
	ws.Spec.CloseRequests = []spiceboxv1alpha1.WorkshopCloseRequest{
		{Target: "other-a-workshop", RequestedAt: metav1.NewTime(time.Now().Add(-time.Hour))},
	}
	s, c := newCloseServer(t, ws)

	res := callTool(t, s.handleCloseOthers, closeOthersArgs{Workshops: []string{"other-a-workshop", "other-b-workshop"}})
	require.False(t, res.IsError)

	assert.ElementsMatch(t, []string{"other-a-workshop", "other-b-workshop"}, closeRequestTargets(t, c),
		"the standing request is left as it was; only the new target is added")
}

// A refusal by the apiserver on the WRITE is surfaced verbatim, and nothing is
// claimed to have closed.
func TestCloseOthers_ADeniedWriteSurfacesVerbatim(t *testing.T) {
	shrinkClosePollForTest(t)
	denyErr := apierrors.NewForbidden(schema.GroupResource{Group: spiceboxv1alpha1.GroupName, Resource: "workshops"},
		closeOwnWorkshopName(), errors.New("RBAC denies writes outside this workshop"))
	c := fake.NewClientBuilder().WithScheme(testfixtures.NewScheme(t)).
		WithObjects(closeWorkshopCR()).
		WithInterceptorFuncs(interceptor.Funcs{
			Update: func(_ context.Context, _ client.WithWatch, _ client.Object, _ ...client.UpdateOption) error {
				return denyErr
			},
		}).Build()
	s := newCredServer("ws-abc123", closeSessNS, closeSessName, c)

	res := callTool(t, s.handleCloseOthers, closeOthersArgs{})
	require.True(t, res.IsError, "a write that never landed must not answer as a success")
	body := decodeResultBody(t, res)
	assert.Equal(t, true, body["denied"])
	assert.Equal(t, denyErr.Error(), body["message"])
}

// Any other cluster failure is a tool error too — never a partial success. Both
// halves are covered: the write that records the request, and the read that
// collects the answers.
func TestCloseOthers_ClusterFailureIsAToolError(t *testing.T) {
	cases := []struct {
		name     string
		funcs    func(boom error) interceptor.Funcs
		wantText string
	}{
		{
			name: "the request cannot be recorded: a tool error naming the write",
			funcs: func(boom error) interceptor.Funcs {
				return interceptor.Funcs{
					Get: func(_ context.Context, _ client.WithWatch, _ client.ObjectKey, _ client.Object, _ ...client.GetOption) error {
						return boom
					},
				}
			},
			wantText: "recording the request",
		},
		{
			name: "the answers cannot be read: a tool error saying the request WAS recorded",
			funcs: func(boom error) interceptor.Funcs {
				var writes int
				return interceptor.Funcs{
					Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
						if writes > 0 {
							return boom
						}
						return cl.Get(ctx, key, obj, opts...)
					},
					Update: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
						writes++
						return cl.Update(ctx, obj, opts...)
					},
				}
			},
			wantText: "reading the answer",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			shrinkClosePollForTest(t)
			boom := errors.New("the apiserver is unreachable")
			c := fake.NewClientBuilder().WithScheme(testfixtures.NewScheme(t)).
				WithObjects(closeWorkshopCR()).
				WithInterceptorFuncs(tc.funcs(boom)).Build()
			s := newCredServer("ws-abc123", closeSessNS, closeSessName, c)

			res := callTool(t, s.handleCloseOthers, closeOthersArgs{})
			require.True(t, res.IsError)
			body := decodeResultBody(t, res)
			assert.Contains(t, body["error"], tc.wantText)
			assert.Contains(t, body["error"], boom.Error(), "the cause must reach whoever reads the transcript")
			assert.NotContains(t, body, "closed", "a failure never carries an answer list to be misread as one")
		})
	}
}

// A list that named nothing usable is a tool error, never a silent no-op and
// never a widening: "close these" with an unusable list must not become "close
// everything".
func TestCloseOthers_AListThatNamesNothingUsableIsAToolError(t *testing.T) {
	shrinkClosePollForTest(t)
	s, c := newCloseServer(t, closeWorkshopCR())

	res := callTool(t, s.handleCloseOthers, closeOthersArgs{Workshops: []string{"", "   "}})
	require.True(t, res.IsError)
	body := decodeResultBody(t, res)
	assert.Contains(t, body["error"], "name at least one workshop")
	assert.Empty(t, closeRequestTargets(t, c), "nothing is requested, least of all every workshop the person has")
}

// A person is shown builder-SESSION names — that is what the start-route
// refusal lists when they are at their limit — so that is what they say back.
// A request has to name the Workshop, and WorkshopName is a deterministic
// suffix, so either spelling is accepted, written as the Workshop name, and
// answered under the name they gave.
func TestCloseOthers_EitherSpellingOfANameIsUnderstoodAndAnsweredAsGiven(t *testing.T) {
	cases := []struct {
		name  string
		given []string
		// wantTargets are the spec.closeRequests targets written.
		wantTargets []string
		// wantClosed is the answer, spelled the way the person said it.
		wantClosed []string
	}{
		{
			name:        "the name shown on the page: written as the workshop, answered as given",
			given:       []string{"other-a"},
			wantTargets: []string{"other-a-workshop"},
			wantClosed:  []string{"other-a"},
		},
		{
			name:        "the workshop name: passed through unchanged, never suffixed twice",
			given:       []string{"other-a-workshop"},
			wantTargets: []string{"other-a-workshop"},
			wantClosed:  []string{"other-a-workshop"},
		},
		{
			name:        "both spellings of one workshop: one request, one answer",
			given:       []string{"other-a", "other-a-workshop"},
			wantTargets: []string{"other-a-workshop"},
			wantClosed:  []string{"other-a"},
		},
		{
			// The start-route refusal names a workshop outside the namespace
			// being started in as <namespace>/<session name>, so that is what
			// the person says back. Only the NAME part is a session name; the
			// suffix rule applies to it alone.
			name:        "a name in another namespace: only the name part is suffixed, the prefix is kept",
			given:       []string{"other-ns/other-a"},
			wantTargets: []string{"other-ns/other-a-workshop"},
			wantClosed:  []string{"other-ns/other-a"},
		},
		{
			name:        "a workshop name in another namespace: passed through unchanged",
			given:       []string{"other-ns/other-a-workshop"},
			wantTargets: []string{"other-ns/other-a-workshop"},
			wantClosed:  []string{"other-ns/other-a-workshop"},
		},
		{
			// Same name, two namespaces: two workshops, two requests. Folding
			// them into one would close a workshop the person did not name.
			name:        "the same name in two namespaces: two requests, each answered as given",
			given:       []string{"other-a", "other-ns/other-a"},
			wantTargets: []string{"other-a-workshop", "other-ns/other-a-workshop"},
			wantClosed:  []string{"other-a", "other-ns/other-a"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			shrinkClosePollForTest(t)
			// The controller has already decided every target this call will
			// write, so what the row is about is the spelling on either side.
			decisions := make([]spiceboxv1alpha1.WorkshopCloseStatus, 0, len(tc.wantTargets))
			for _, target := range tc.wantTargets {
				decisions = append(decisions, closeDecision(target, spiceboxv1alpha1.WorkshopClosePhaseClosed, ""))
			}
			s, c := newCloseServer(t, closeWorkshopCR(decisions...))

			res := callTool(t, s.handleCloseOthers, closeOthersArgs{Workshops: tc.given})
			require.False(t, res.IsError)

			assert.Equal(t, tc.wantTargets, closeRequestTargets(t, c),
				"the request must name the workshop, whichever spelling the person used")
			body := decodeResultBody(t, res)
			assert.Equal(t, tc.wantClosed, stringsOf(t, body, "closed"),
				"the answer is spelled the way the person said it")
			assert.Empty(t, stringsOf(t, body, "pending"))
		})
	}
}

// nextWildcardKey is what makes a second "close everything" a second ASK rather
// than a no-op on the first, so it has to land on a target the request list
// does not already hold: spec.closeRequests is keyed by target, and a duplicate
// key is a write the apiserver refuses outright.
func TestNextWildcardKey(t *testing.T) {
	all := spiceboxv1alpha1.WorkshopCloseTargetAll
	cases := []struct {
		name    string
		targets []string
		want    string
	}{
		{
			name:    "a workshop that has never asked: the plain wildcard",
			targets: nil,
			want:    all,
		},
		{
			name:    "named requests only: they are not asks, so the plain wildcard is still free",
			targets: []string{"other-a-workshop", "other-ns/other-b-workshop"},
			want:    all,
		},
		{
			name:    "one ask standing: the second is sequenced",
			targets: []string{all},
			want:    all + ":2",
		},
		{
			name:    "two asks standing, named requests beside them: only the asks are counted",
			targets: []string{all, "other-a-workshop", all + ":2"},
			want:    all + ":3",
		},
		{
			name:    "the sequence has a gap: the count lands on a key already taken, so it is bumped past it",
			targets: []string{all, all + ":3"},
			want:    all + ":4",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			requests := make([]spiceboxv1alpha1.WorkshopCloseRequest, 0, len(tc.targets))
			for _, target := range tc.targets {
				requests = append(requests, spiceboxv1alpha1.WorkshopCloseRequest{Target: target})
			}
			assert.Equal(t, tc.want, nextWildcardKey(requests))
		})
	}
}

// A name that cannot BE a target is answered about that name, before anything
// is written. spec.closeRequests is one list under one CRD pattern, so a
// single unspellable name fails the whole write and takes the good names in
// the same ask down with it — a person who fat-fingered one workshop would be
// told nothing about the three they got right.
//
// The refusal points at the one thing that does reach a workshop whose name
// the grammar cannot spell: asking to close them all.
func TestCloseOthers_ANameTheGrammarCannotSpellIsRefusedAndTheRestProceed(t *testing.T) {
	cases := []struct {
		name  string
		given string
	}{
		{name: "characters no workshop name can hold: refused, the good name still proceeds", given: "Other B"},
		{name: "a dot in the name: refused, the good name still proceeds", given: "other.b"},
		{name: "a second slash: refused, the good name still proceeds", given: "other-ns/sub/other-b"},
		{name: "an empty namespace half: refused, the good name still proceeds", given: "/other-b"},
		{name: "longer than the target field may be: refused, the good name still proceeds", given: strings.Repeat("b", 130)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			shrinkClosePollForTest(t)
			s, c := newCloseServer(t, closeWorkshopCR(
				closeDecision("other-a-workshop", spiceboxv1alpha1.WorkshopClosePhaseClosed, "")))

			res := callTool(t, s.handleCloseOthers, closeOthersArgs{Workshops: []string{tc.given, "other-a"}})
			require.False(t, res.IsError, "a name that cannot be a target is an answer about that name, not a tool failure")

			assert.Equal(t, []string{"other-a-workshop"}, closeRequestTargets(t, c),
				"only the name that can be a target is written, and it is written")
			body := decodeResultBody(t, res)
			assert.Equal(t, []string{"other-a"}, stringsOf(t, body, "closed"),
				"the workshop that CAN be named is still requested and still answered")
			refused, ok := body["refused"].([]any)
			require.True(t, ok)
			require.Len(t, refused, 1, "exactly the unspellable name is refused: %+v", refused)
			entry, ok := refused[0].(map[string]any)
			require.True(t, ok)
			assert.Equal(t, tc.given, entry["workshop"], "refused under the name the person used")
			assert.Equal(t, "not a name that can be closed by itself; ask to close them all", entry["why"],
				"the wildcard reaches every workshop, including one whose name the grammar cannot spell")
		})
	}
}

// closeAskStanding is this workshop's CR with one wildcard ask already asked
// and not yet answered — the state a person is in when they check again while
// "close the others" is still being decided.
func closeAskStanding() *spiceboxv1alpha1.Workshop {
	ws := closeWorkshopCR()
	ws.Spec.CloseRequests = []spiceboxv1alpha1.WorkshopCloseRequest{
		{Target: spiceboxv1alpha1.WorkshopCloseTargetAll, RequestedAt: metav1.NewTime(time.Now())},
	}
	return ws
}

// answerCloseAskMidPoll is the operator arriving while the tool waits: the
// call's FIRST poll read finds the standing ask decided. Nothing else answers
// it — the fake client has no controller behind it — so this is how a call
// that waits for an ask it did not write gets something to report.
//
// The read that records the request is read 1; every read after it is the
// poll's.
func answerCloseAskMidPoll(t *testing.T, seed *spiceboxv1alpha1.Workshop, decisions ...spiceboxv1alpha1.WorkshopCloseStatus) (*Server, client.Client) {
	t.Helper()
	reads := 0
	c := fake.NewClientBuilder().WithScheme(testfixtures.NewScheme(t)).
		WithObjects(seed).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				reads++
				if reads == 2 {
					var ws spiceboxv1alpha1.Workshop
					if err := cl.Get(ctx, key, &ws); err != nil {
						return err
					}
					ws.Status.CloseRequests = append(ws.Status.CloseRequests, decisions...)
					if err := cl.Update(ctx, &ws); err != nil {
						return err
					}
				}
				return cl.Get(ctx, key, obj, opts...)
			},
		}).Build()
	return newCredServer("ws-abc123", closeSessNS, closeSessName, c), c
}

// "Check again with the same call" is what the skills tell a person to do while
// an ask is outstanding, so a no-names call made then must not become a SECOND
// ask. A new ask expands afresh, and its answer would name only the workshops
// opened since the first one — so a person checking on "close the others"
// would be told "nothing new to close" about the very ask they were waiting
// for. The call waits for the standing ask instead, and says what its answer
// does not cover.
func TestCloseOthers_ANoNamesCallWhileAnAskIsOutstandingWaitsForThatAsk(t *testing.T) {
	all := spiceboxv1alpha1.WorkshopCloseTargetAll

	t.Run("the standing ask lands during the wait: its answer is reported, and no second ask is written", func(t *testing.T) {
		shrinkClosePollForTest(t)
		s, c := answerCloseAskMidPoll(t, closeAskStanding(),
			closeExpansion(all, "other-a-workshop", spiceboxv1alpha1.WorkshopClosePhaseClosed, ""),
			closeDecision(all, spiceboxv1alpha1.WorkshopClosePhaseClosed, "1 closed, 0 refused"))

		res := callTool(t, s.handleCloseOthers, closeOthersArgs{})
		require.False(t, res.IsError)

		assert.Equal(t, []string{all}, closeRequestTargets(t, c),
			"a call made while an ask is outstanding must not write a second one")
		body := decodeResultBody(t, res)
		assert.Equal(t, []string{"other-a"}, stringsOf(t, body, "closed"), "the standing ask's own answer is reported")
		assert.Contains(t, closeMessageOf(t, body), "1 closed, 0 refused", "the operator's summary reaches the person")
		assert.Contains(t, closeMessageOf(t, body), "This answers your earlier ask.",
			"a person must be told this is the earlier answer, not a fresh one")
	})

	t.Run("the standing ask is decided already: a second ask is written and answered", func(t *testing.T) {
		shrinkClosePollForTest(t)
		s, c := newCloseServer(t, closeWorkshopCR())

		require.False(t, callTool(t, s.handleCloseOthers, closeOthersArgs{}).IsError)
		answerCloseAsk(t, c, closeDecision(all, spiceboxv1alpha1.WorkshopClosePhaseClosed, "1 closed, 0 refused"))
		answerCloseAsk(t, c,
			closeExpansion(closeSecondAsk, "other-b-workshop", spiceboxv1alpha1.WorkshopClosePhaseClosed, ""),
			closeDecision(closeSecondAsk, spiceboxv1alpha1.WorkshopClosePhaseClosed, "1 closed, 0 refused"))

		second := callTool(t, s.handleCloseOthers, closeOthersArgs{})
		require.False(t, second.IsError)

		assert.Equal(t, []string{all, closeSecondAsk}, closeRequestTargets(t, c),
			"an ask made after the last one was answered is a deliberate second ask")
		body := decodeResultBody(t, second)
		assert.Equal(t, []string{"other-b"}, stringsOf(t, body, "closed"), "its OWN expansion is the answer")
		assert.NotContains(t, closeMessageOf(t, body), "This answers your earlier ask.",
			"this call asked its own question; saying otherwise would tell a person to ask again for nothing")
	})

	t.Run("the standing ask is still undecided at the deadline: reported as outstanding, and still no second ask", func(t *testing.T) {
		shrinkClosePollForTest(t)
		s, c := newCloseServer(t, closeWorkshopCR())

		require.False(t, callTool(t, s.handleCloseOthers, closeOthersArgs{}).IsError)
		second := callTool(t, s.handleCloseOthers, closeOthersArgs{})
		require.False(t, second.IsError)

		assert.Equal(t, []string{all}, closeRequestTargets(t, c),
			"the outstanding ask is what this call is waiting on; a second one would ask a different question")
		body := decodeResultBody(t, second)
		assert.Equal(t, "Your earlier ask is still being decided; a workshop opened since then is not covered. Ask again in a moment.",
			closeMessageOf(t, body),
			"one sentence about the wait: nothing has been answered, so nothing may read as if it had")
		assert.NotContains(t, closeMessageOf(t, body), "This answers",
			"an ask still open has answered nothing")
		assert.Empty(t, stringsOf(t, body, "closed"), "nothing may be claimed for an ask nobody has answered")
	})
}

// The wildcard is how a person asks for every other workshop, not a workshop
// itself. Handed over as a name, it is refused with the way to ask for that —
// not with the general "cannot be closed by itself; ask to close them all",
// which for this one input reads as a riddle, and not with the whole write
// failing at the apiserver.
func TestCloseOthers_TheWildcardGivenAsANameIsRefusedWithTheWayToAsk(t *testing.T) {
	for _, given := range []string{"*", "*:2"} {
		t.Run(given+": refused, the good name still proceeds", func(t *testing.T) {
			shrinkClosePollForTest(t)
			s, c := newCloseServer(t, closeWorkshopCR(
				closeDecision("other-a-workshop", spiceboxv1alpha1.WorkshopClosePhaseClosed, "")))

			res := callTool(t, s.handleCloseOthers, closeOthersArgs{Workshops: []string{given, "other-a"}})
			require.False(t, res.IsError, "the wildcard among names is an answer about that entry, not a tool failure")

			assert.Equal(t, []string{"other-a-workshop"}, closeRequestTargets(t, c),
				"only the real name is written; the wildcard is never written as a named target")
			body := decodeResultBody(t, res)
			assert.Equal(t, []string{"other-a"}, stringsOf(t, body, "closed"))
			refused, ok := body["refused"].([]any)
			require.True(t, ok)
			require.Len(t, refused, 1, "exactly the wildcard entry is refused: %+v", refused)
			entry, ok := refused[0].(map[string]any)
			require.True(t, ok)
			assert.Equal(t, given, entry["workshop"])
			assert.Equal(t, "not a workshop name; leave the names out to close every other workshop", entry["why"])
		})
	}
}
