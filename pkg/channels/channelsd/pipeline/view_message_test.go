package pipeline

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkey"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// newTestPipelineWithSession builds a pipeline whose fake client holds a
// Running AgentSession bound to a `fake`-kind Channel, so
// HandleViewMessage's session -> channel -> Deliver reconstruction resolves
// to the ACTIVE (DispResume) branch of Deliver and returns OutcomeRouted.
func newTestPipelineWithSession(t *testing.T, ns, name string) (*Pipeline, *fakeMemory) {
	t.Helper()
	return newTestPipelineWithSessionKind(t, ns, name, "fake")
}

// newTestPipelineWithSessionKind is newTestPipelineWithSession parameterized
// on the bound Channel's spec.kind, so tests can pin HandleViewMessage's
// AllowsSyntheticIdentity gate to a specific SESSION-witnessed kind
// (local/slack/fake/...) independent of whatever the wire payload claims.
func newTestPipelineWithSessionKind(t *testing.T, ns, name, kind string) (*Pipeline, *fakeMemory) {
	t.Helper()
	ch := &spiceboxv1alpha1.Channel{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "ch"},
		Spec:       spiceboxv1alpha1.ChannelSpec{Kind: kind},
	}
	key := "chat:" + name
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns, Name: name,
			Labels: map[string]string{
				spiceboxv1alpha1.LabelChannelName: "ch",
				spiceboxv1alpha1.LabelChannelKey:  channelkey.LabelValue(key),
			},
			// A human-started session always records who started it. The
			// join-approval path addresses its approver from these
			// annotations, so without one a denied requester produces an
			// interaction_request with no addressable approver — which
			// Validate now rejects. Model the real shape.
			Annotations: map[string]string{
				spiceboxv1alpha1.AnnotationStartedByExternalID:  "U_original",
				spiceboxv1alpha1.AnnotationStartedByCanonicalID: "user:starter",
			},
		},
		Spec: spiceboxv1alpha1.AgentSessionSpec{
			InputChannel: &spiceboxv1alpha1.ChannelBinding{
				Name: "ch", Kind: kind, Key: key,
			},
		},
		Status: spiceboxv1alpha1.AgentSessionStatus{Phase: "Running"},
	}
	p, _, mem, _, _ := newPipeline(t, ch, sess)
	return p, mem
}

// canonOf mirrors identity.Principal.Canonical()'s email encoding.
func canonOf(email string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strings.ToLower(email)))
}

// canonSynthetic mirrors identity.Principal.Canonical()'s synthetic
// (AllowSynthetic, email-less) encoding: base64url(kind:teamScope:externalID).
func canonSynthetic(kind, teamScope, externalID string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(kind + ":" + teamScope + ":" + externalID))
}

func viewEnvelope(t *testing.T, ns, name, text, email string) channelevents.Envelope {
	t.Helper()
	env, err := channelevents.BuildEnvelope(ns, name, channelevents.KindViewMessage,
		channelevents.ViewMessagePayload{
			Text:   text,
			Author: channelevents.ExternalIdentity{Kind: "idp", ExternalID: identity.RawExternalID(email), Email: identity.Email(email)},
		})
	require.NoError(t, err)
	return env
}

func TestHandleViewMessageRoutesThroughDeliver(t *testing.T) {
	p, mem := newTestPipelineWithSession(t, "ns", "sess")
	res, err := p.HandleViewMessage(context.Background(),
		viewEnvelope(t, "ns", "sess", "hello", "alice@example.com"))
	require.NoError(t, err)
	assert.Equal(t, "routed", res.Outcome)
	assert.Empty(t, res.Error)
	require.Len(t, mem.appends, 1, "the inbound must be appended exactly once")
	assert.Equal(t, "user:"+canonOf("alice@example.com"), string(mem.appends[0].turn.Author),
		"Turn.Author must be the browser user, not the session initiator")
}

func TestHandleViewMessageMissingSessionReturnsErrorInResult(t *testing.T) {
	p, _ := newTestPipelineWithSession(t, "ns", "sess")
	res, err := p.HandleViewMessage(context.Background(),
		viewEnvelope(t, "ns", "does-not-exist", "hi", "alice@example.com"))
	require.Error(t, err, "a handler failure must be returned so channelsd logs it")
	assert.NotEmpty(t, res.Error, "and carried in the reply so the requester does not wait out its timeout")
	assert.Equal(t, "internal_error", res.Outcome)
	// Pin the actual guard (the session Get's error is checked and returned),
	// not just "some later fail-closed check happened to catch it too" — a
	// swallowed Get error still falls through to the InputChannel-nil check
	// below it and produces a DIFFERENT message ("has no input channel
	// binding"), which would pass the assertions above even with the Get
	// error check deleted. Mutation-tested: deleting the Get error check
	// makes this specific assertion fail while the three above it still pass.
	assert.Contains(t, res.Error, "get session", "must be the session-lookup guard, not a downstream fallback")
}

func TestHandleViewMessageRejectsSyntheticIdentity(t *testing.T) {
	// The fixture's bound Channel is kind="fake" (newTestPipelineWithSession),
	// and fake.Kind.AllowsSyntheticIdentity()==false (matches production
	// posture: e2e uses email-bearing identities) — so an email-less author
	// must still fail closed here, same as before AllowsSyntheticIdentity
	// existed. This test remains meaningful post-fix: it pins the
	// still-fail-closed default for every kind that doesn't explicitly opt in.
	p, mem := newTestPipelineWithSession(t, "ns", "sess")
	// No email => Principal.Canonical() returns ErrSyntheticSubject with
	// allowSynthetic=false. A synthetic subject resolves to no grant at best,
	// and at worst collides with one an attacker shaped.
	env, err := channelevents.BuildEnvelope("ns", "sess", channelevents.KindViewMessage,
		channelevents.ViewMessagePayload{
			Text:   "hi",
			Author: channelevents.ExternalIdentity{Kind: "idp", ExternalID: "x"},
		})
	require.NoError(t, err)

	res, hErr := p.HandleViewMessage(context.Background(), env)
	require.Error(t, hErr)
	assert.Equal(t, "internal_error", res.Outcome)
	assert.Contains(t, res.Error, "synthetic")
	assert.Empty(t, mem.appends, "a rejected/unresolvable author must never reach the memory append")
}

// TestHandleViewMessageLocalKindSessionAllowsSyntheticIdentity pins the
// regression fix: `oap agent chat` on a no-IdP cluster mints a legitimately
// email-less local-OS-user identity (cmd/oap/internal/clilogin/login.go,
// identity.FromExternal("local","",username,"").AllowSynthetic()). Before the
// fix, HandleViewMessage's re-derivation always left allowSynthetic=false, so
// every follow-up turn after the first hit ErrSyntheticSubject and the TUI's
// multi-turn chat broke. The fix keys AllowSynthetic off the SESSION's own
// witnessed InputChannel.Kind — "local" here — via
// channelkinds.Kind.AllowsSyntheticIdentity(), never off the wire payload.
func TestHandleViewMessageLocalKindSessionAllowsSyntheticIdentity(t *testing.T) {
	p, mem := newTestPipelineWithSessionKind(t, "ns", "sess", "local")
	env, err := channelevents.BuildEnvelope("ns", "sess", channelevents.KindViewMessage,
		channelevents.ViewMessagePayload{
			Text:   "hello",
			Author: channelevents.ExternalIdentity{Kind: "local", ExternalID: "alice"},
		})
	require.NoError(t, err)

	res, hErr := p.HandleViewMessage(context.Background(), env)
	require.NoError(t, hErr, "a local-kind session's email-less OS-user identity must be routed, not rejected")
	assert.Equal(t, "routed", res.Outcome)
	assert.Empty(t, res.Error)
	require.Len(t, mem.appends, 1, "the inbound must be appended exactly once")
	assert.Equal(t, "user:"+canonSynthetic("local", "", "alice"), string(mem.appends[0].turn.Author),
		"Turn.Author must be the synthetic local-kind subject")
}

// TestHandleViewMessageSlackKindSessionRejectsClaimedLocalIdentity: an
// attacker publishing {Author:{Kind:"local", ExternalID:"victim"}} against a
// SLACK-bound session must fail closed, because the AllowSynthetic decision is
// keyed off the SESSION's own K8s-witnessed InputChannel.Kind ("slack" here,
// whose AllowsSyntheticIdentity()==false) — never off the untrusted wire
// payload's claimed Author.Kind. Keyed off the payload instead, a bare
// {Kind:"local"} claim bypasses fail-closed identity resolution on any
// non-local session.
func TestHandleViewMessageSlackKindSessionRejectsClaimedLocalIdentity(t *testing.T) {
	p, mem := newTestPipelineWithSessionKind(t, "ns", "sess", "slack")
	env, err := channelevents.BuildEnvelope("ns", "sess", channelevents.KindViewMessage,
		channelevents.ViewMessagePayload{
			Text:   "hi",
			Author: channelevents.ExternalIdentity{Kind: "local", ExternalID: "victim"},
		})
	require.NoError(t, err)

	res, hErr := p.HandleViewMessage(context.Background(), env)
	require.Error(t, hErr, "a claimed local identity against a slack-bound session must fail closed")
	assert.Equal(t, "internal_error", res.Outcome)
	assert.Contains(t, res.Error, "synthetic")
	assert.Empty(t, mem.appends, "an attacker's Kind:local claim on a slack session must never reach the memory append")
}

func TestHandleViewMessageMalformedPayloadFailsClosed(t *testing.T) {
	p, mem := newTestPipelineWithSession(t, "ns", "sess")
	env := channelevents.Envelope{
		Version: 1, Kind: channelevents.KindViewMessage,
		Session: channelevents.SessionRef{Namespace: "ns", Name: "sess"},
		Payload: json.RawMessage(`{`),
	}
	res, err := p.HandleViewMessage(context.Background(), env)
	require.Error(t, err)
	assert.NotEmpty(t, res.Error)
	assert.Equal(t, "internal_error", res.Outcome)
	assert.Empty(t, mem.appends, "a malformed payload must never reach the memory append")
	// Pin the actual decode guard. A malformed `{` leaves pl zero-valued
	// (json.Unmarshal never populates a partial struct on this error), so an
	// author-less pl also trips the downstream synthetic-identity guard with
	// the same Outcome/non-empty-Error shape — a mutant that deletes the
	// decode-error check entirely still passes the three assertions above.
	// Mutation-tested: deleting the decode check makes this assertion fail
	// (the error instead reads "unresolvable author identity: ... synthetic").
	assert.Contains(t, res.Error, "decode payload", "must be the JSON-decode guard, not a downstream fallback")
}

func TestHandleViewMessageDeniedByInteractCheck(t *testing.T) {
	// The claimed author has no interact grant on the session (fakeAuthz's
	// CheckInteract returns false), so even though the identity itself
	// resolves cleanly, Deliver's re-check must deny it. This is the second
	// half of the security contract: the claim alone is never sufficient —
	// re-derivation (tested above) AND Deliver's authz re-check (tested here)
	// both gate delivery.
	p, mem := newTestPipelineWithSession(t, "ns", "sess")
	p.Authz.(*fakeAuthz).checkResult = false

	res, err := p.HandleViewMessage(context.Background(),
		viewEnvelope(t, "ns", "sess", "hello", "mallory@example.com"))
	require.NoError(t, err, "a permission denial is a clean decision, not a handler error")
	assert.Equal(t, "denied_by_permission", res.Outcome)
	assert.Empty(t, mem.appends, "a denied author's message must never be appended")
}

// TestHandleViewMessageDeniedWithViaNoEcho pins the OTHER half of the echo
// gate: a message with a non-empty Via that is denied by Deliver's Interact
// re-check must NOT echo, even though pl.Via != "" alone. Mutation-tested:
// dropping the `dec.Outcome == channelkinds.OutcomeRouted` guard in
// view_message.go (leaving only the Via check) makes this test fail — an
// echo is published on this denied path.
func TestHandleViewMessageDeniedWithViaNoEcho(t *testing.T) {
	p, mem := newTestPipelineWithSession(t, "ns", "sess")
	p.Authz.(*fakeAuthz).checkResult = false

	env, err := channelevents.BuildEnvelope("ns", "sess", channelevents.KindViewMessage,
		channelevents.ViewMessagePayload{
			Text:   "hello",
			Via:    "urn:ap:view:artifact:artifact-3f2a1b8c",
			Author: channelevents.ExternalIdentity{Kind: "idp", ExternalID: "mallory@example.com", Email: "mallory@example.com"},
		})
	require.NoError(t, err)

	res, hErr := p.HandleViewMessage(context.Background(), env)
	require.NoError(t, hErr, "a permission denial is a clean decision, not a handler error")
	assert.Equal(t, "denied_by_permission", res.Outcome)
	assert.Empty(t, mem.appends, "a denied author's message must never be appended")
	assertNotPublished(t, p, "ap.session.ns.sess.out.user_echo")
}

// TestHandleViewMessageBlankExternalIDDefaultsToEmailNotEmptyAuthor pins a
// privilege escalation. A payload with Email set but ExternalID blank passes
// the Canonical() guard above (Canonical prefers Email over ExternalID), and
// Deliver's Interact re-check is keyed off that same email-derived subject —
// but authorSubject keys off ExternalID alone with no email fallback, so
// without the default Turn.Author comes out empty. An empty Turn.Author makes
// advanceRequester (pkg/agent/runner/loop.go) a no-op, leaving a multiplayer
// session's authSubjectMode=currentRequester pinned to the PRIOR requester, so
// this sender's later tool calls authorize as whoever sent the previous turn.
// Removing the ExternalID-defaulting in HandleViewMessage makes this fail.
func TestHandleViewMessageBlankExternalIDDefaultsToEmailNotEmptyAuthor(t *testing.T) {
	p, mem := newTestPipelineWithSession(t, "ns", "sess")
	env, err := channelevents.BuildEnvelope("ns", "sess", channelevents.KindViewMessage,
		channelevents.ViewMessagePayload{
			Text:   "hello",
			Author: channelevents.ExternalIdentity{Kind: "idp", Email: "mallory@example.com"},
		})
	require.NoError(t, err)

	res, hErr := p.HandleViewMessage(context.Background(), env)
	require.NoError(t, hErr)
	assert.Equal(t, "routed", res.Outcome)
	require.Len(t, mem.appends, 1, "the inbound must be appended exactly once")
	assert.NotEmpty(t, mem.appends[0].turn.Author,
		"a blank-ExternalID/email-set payload must never produce an empty Turn.Author")
	assert.Equal(t, "user:"+canonOf("mallory@example.com"), string(mem.appends[0].turn.Author),
		"the defaulted ExternalID must yield the same subject the Interact check already authorized")
}

func TestHandleViewMessageStampsValidVia(t *testing.T) {
	p, mem := newTestPipelineWithSession(t, "ns", "sess")
	env, err := channelevents.BuildEnvelope("ns", "sess", channelevents.KindViewMessage,
		channelevents.ViewMessagePayload{
			Text:   "the CTA padding is wrong",
			Via:    "urn:ap:view:artifact:artifact-3f2a1b8c",
			Author: channelevents.ExternalIdentity{Kind: "idp", ExternalID: "a@example.com", Email: "a@example.com"},
		})
	require.NoError(t, err)
	res, err := p.HandleViewMessage(context.Background(), env)
	require.NoError(t, err)
	assert.Equal(t, "routed", res.Outcome)
	require.NotEmpty(t, mem.appends)
	assert.Equal(t, "urn:ap:view:artifact:artifact-3f2a1b8c", mem.appends[0].turn.Via)
}

// A malformed Via is a forgery/injection attempt (Via is server-minted; a bad
// one means a crafted publish). Fail closed BEFORE Deliver.
func TestHandleViewMessageRejectsMalformedVia(t *testing.T) {
	p, mem := newTestPipelineWithSession(t, "ns", "sess")
	env, err := channelevents.BuildEnvelope("ns", "sess", channelevents.KindViewMessage,
		channelevents.ViewMessagePayload{
			Text:   "x",
			Via:    "urn:ap:view:artifact:<script>",
			Author: channelevents.ExternalIdentity{Kind: "idp", ExternalID: "a@example.com", Email: "a@example.com"},
		})
	require.NoError(t, err)
	res, hErr := p.HandleViewMessage(context.Background(), env)
	require.Error(t, hErr)
	assert.Equal(t, "internal_error", res.Outcome)
	assert.Contains(t, res.Error, "via")
	assert.Empty(t, mem.appends, "a malformed Via must never reach the memory append")
}

// Empty Via is tolerated (a surface without a URN) — routed, turn has no Via.
func TestHandleViewMessageEmptyViaIsRouted(t *testing.T) {
	p, mem := newTestPipelineWithSession(t, "ns", "sess")
	env, err := channelevents.BuildEnvelope("ns", "sess", channelevents.KindViewMessage,
		channelevents.ViewMessagePayload{
			Text:   "x",
			Author: channelevents.ExternalIdentity{Kind: "idp", ExternalID: "a@example.com", Email: "a@example.com"},
		})
	require.NoError(t, err)
	res, err := p.HandleViewMessage(context.Background(), env)
	require.NoError(t, err)
	assert.Equal(t, "routed", res.Outcome)
	require.NotEmpty(t, mem.appends)
	assert.Empty(t, mem.appends[0].turn.Via)
}

// findPublished returns the decoded envelope for the first recorded publish
// on the fixture's fakeNATS matching subject, failing the test if none was
// published there. Thin helper over fakeNATS's parallel subjects/payloads
// slices (pipeline_test.go); p.NATS is the interface field, so the
// concrete *fakeNATS the test fixtures wire in is recovered via a type
// assertion.
func findPublished(t *testing.T, p *Pipeline, subject string) channelevents.Envelope {
	t.Helper()
	nats, ok := p.NATS.(*fakeNATS)
	require.True(t, ok, "pipeline fixture must wire a *fakeNATS")
	for i, s := range nats.subjects {
		if s == subject {
			var env channelevents.Envelope
			require.NoError(t, json.Unmarshal(nats.payloads[i], &env), "unmarshal recorded envelope")
			return env
		}
	}
	require.Fail(t, "no envelope published", "subject %q; got subjects: %v", subject, nats.subjects)
	return channelevents.Envelope{}
}

// assertNotPublished asserts nothing was recorded on subject.
func assertNotPublished(t *testing.T, p *Pipeline, subject string) {
	t.Helper()
	nats, ok := p.NATS.(*fakeNATS)
	require.True(t, ok, "pipeline fixture must wire a *fakeNATS")
	for _, s := range nats.subjects {
		assert.NotEqual(t, subject, s, "unexpected publish on subject %q", subject)
	}
}

func TestHandleViewMessagePublishesEchoOnRoutedVia(t *testing.T) {
	p, _ := newTestPipelineWithSession(t, "ns", "sess")
	env, err := channelevents.BuildEnvelope("ns", "sess", channelevents.KindViewMessage,
		channelevents.ViewMessagePayload{
			Text:      "the CTA padding is wrong",
			Via:       "urn:ap:view:artifact:artifact-3f2a1b8c",
			Author:    channelevents.ExternalIdentity{Kind: "idp", ExternalID: "a@example.com", Email: "a@example.com"},
			RequestID: "req-77",
		})
	require.NoError(t, err)
	res, err := p.HandleViewMessage(context.Background(), env)
	require.NoError(t, err)
	require.Equal(t, "routed", res.Outcome)

	// An out.user_echo envelope was published with the text/author/via + the
	// originating RequestID (so a view rendering its own send optimistically can
	// suppress the echo it already showed).
	echo := findPublished(t, p, "ap.session.ns.sess.out.user_echo") // helper over the fixture's recorded publishes
	var pl channelevents.UserEchoPayload
	require.NoError(t, json.Unmarshal(echo.Payload, &pl))
	assert.Equal(t, "the CTA padding is wrong", pl.Text)
	assert.Equal(t, "urn:ap:view:artifact:artifact-3f2a1b8c", pl.Via)
	assert.Equal(t, "a@example.com", pl.Author.Email.String())
	assert.Equal(t, "req-77", pl.RequestID, "the echo must carry the originating send's idempotency key")
}

// No Via ⇒ no echo (a channel's own listener, or a view with no URN).
func TestHandleViewMessageNoViaNoEcho(t *testing.T) {
	p, _ := newTestPipelineWithSession(t, "ns", "sess")
	env, err := channelevents.BuildEnvelope("ns", "sess", channelevents.KindViewMessage,
		channelevents.ViewMessagePayload{Text: "x",
			Author: channelevents.ExternalIdentity{Kind: "idp", ExternalID: "a@example.com", Email: "a@example.com"}})
	require.NoError(t, err)
	_, err = p.HandleViewMessage(context.Background(), env)
	require.NoError(t, err)
	assertNotPublished(t, p, "ap.session.ns.sess.out.user_echo")
}

// TestHandleViewMessageDeferEchoSuppressesRawEcho pins the annotation-bundle
// case: a routed message with a Via AND DeferEcho=true must NOT publish the
// raw user_echo — its human-readable mirror is authored elsewhere (the
// runner's summary echo, plan D4), so echoing the raw structured Text here
// would dump the bundle into Slack/chat.
func TestHandleViewMessageDeferEchoSuppressesRawEcho(t *testing.T) {
	p, _ := newTestPipelineWithSession(t, "ns", "sess")
	env, err := channelevents.BuildEnvelope("ns", "sess", channelevents.KindViewMessage,
		channelevents.ViewMessagePayload{
			Text:      "big structured bundle",
			Via:       "urn:ap:view:artifact:artifact-9",
			DeferEcho: true,
			Author:    channelevents.ExternalIdentity{Kind: "idp", ExternalID: "a@example.com", Email: "a@example.com"},
		})
	require.NoError(t, err)
	res, err := p.HandleViewMessage(context.Background(), env)
	require.NoError(t, err)
	require.Equal(t, "routed", res.Outcome)
	assertNotPublished(t, p, "ap.session.ns.sess.out.user_echo")
}

// TestHandleViewMessageDeferEchoFalseStillEchoes is the control: an
// otherwise-identical routed message with DeferEcho=false (the zero value,
// what every ordinary user message sends) still echoes verbatim as before
// this task. Pins that the new gate only suppresses when explicitly asked.
func TestHandleViewMessageDeferEchoFalseStillEchoes(t *testing.T) {
	p, _ := newTestPipelineWithSession(t, "ns", "sess")
	env, err := channelevents.BuildEnvelope("ns", "sess", channelevents.KindViewMessage,
		channelevents.ViewMessagePayload{
			Text:      "ordinary message",
			Via:       "urn:ap:view:artifact:artifact-9",
			DeferEcho: false,
			Author:    channelevents.ExternalIdentity{Kind: "idp", ExternalID: "a@example.com", Email: "a@example.com"},
		})
	require.NoError(t, err)
	res, err := p.HandleViewMessage(context.Background(), env)
	require.NoError(t, err)
	require.Equal(t, "routed", res.Outcome)

	echo := findPublished(t, p, "ap.session.ns.sess.out.user_echo")
	var pl channelevents.UserEchoPayload
	require.NoError(t, json.Unmarshal(echo.Payload, &pl))
	assert.Equal(t, "ordinary message", pl.Text)
}
