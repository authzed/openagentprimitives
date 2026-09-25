package channelevents

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDecodeMetaagentAcceptsBothWireShapes is the compatibility contract the
// readers-first rollout rests on: the bare payload every publisher sends today
// and the enveloped one they may send later must both decode to the same
// payload bytes, and both must take their session from the SUBJECT.
func TestDecodeMetaagentAcceptsBothWireShapes(t *testing.T) {
	bare := []byte(`{"requester":"U1","body":"applied"}`)

	env, err := BuildEnvelope("ns1", "sess-a", KindMetaagentNotice,
		json.RawMessage(bare))
	require.NoError(t, err, "build an enveloped notice")
	enveloped, err := json.Marshal(env)
	require.NoError(t, err)

	cases := []struct {
		name          string
		data          []byte
		wantEnveloped bool
	}{
		{name: "bare payload: decoded as-is, Enveloped false", data: bare},
		{name: "enveloped payload: inner payload returned, Enveloped true", data: enveloped, wantEnveloped: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := DecodeMetaagentOut(
				SubjectOut(SubjectPrefix("ns1", "sess-a"), KindMetaagentNotice), tc.data, KindMetaagentNotice)
			require.NoError(t, err)
			assert.Equal(t, "ns1", got.Namespace)
			assert.Equal(t, "sess-a", got.Name)
			assert.Equal(t, KindMetaagentNotice, got.Kind)
			assert.Equal(t, tc.wantEnveloped, got.Enveloped)
			assert.JSONEq(t, string(bare), string(got.Payload),
				"both shapes must yield the same payload bytes to the sender")
		})
	}
}

// TestDecodeMetaagentRefusals walks every way a message is refused. Each row is
// a way a publisher permitted on ONE subject could otherwise act somewhere
// else, or a way a handler could render the wrong surface.
func TestDecodeMetaagentRefusals(t *testing.T) {
	envelopeClaiming := func(t *testing.T, ns, name string, k Kind) []byte {
		t.Helper()
		env, err := BuildEnvelope(ns, name, k, map[string]string{"body": "hi"})
		require.NoError(t, err)
		b, err := json.Marshal(env)
		require.NoError(t, err)
		return b
	}

	cases := []struct {
		name    string
		subject string
		data    []byte
		want    Kind
		wantErr error
	}{
		{
			name:    "wrong direction segment: refused as an unparseable subject",
			subject: "ap.session.ns1.sess-a.in.metaagent_notice",
			data:    []byte(`{"body":"hi"}`),
			want:    KindMetaagentNotice,
			wantErr: ErrMetaagentSubject,
		},
		{
			name:    "empty ns and name tokens: refused",
			subject: "ap.session....",
			data:    []byte(`{"body":"hi"}`),
			want:    KindMetaagentNotice,
			wantErr: ErrMetaagentSubject,
		},
		{
			name:    "notice subject handed to the scope-approval handler: refused",
			subject: "ap.session.ns1.sess-a.out.metaagent_notice",
			data:    []byte(`{"body":"hi"}`),
			want:    KindMetaagentScopeApproval,
			wantErr: ErrMetaagentKind,
		},
		{
			name:    "envelope claiming another session: refused as a mismatch",
			subject: "ap.session.ns1.sess-a.out.metaagent_notice",
			data:    envelopeClaiming(t, "ns1", "victim", KindMetaagentNotice),
			want:    KindMetaagentNotice,
			wantErr: ErrMetaagentSessionMismatch,
		},
		{
			name:    "envelope claiming another namespace: refused as a mismatch",
			subject: "ap.session.ns1.sess-a.out.metaagent_notice",
			data:    envelopeClaiming(t, "other-ns", "sess-a", KindMetaagentNotice),
			want:    KindMetaagentNotice,
			wantErr: ErrMetaagentSessionMismatch,
		},
		{
			name:    "envelope whose kind disagrees with its subject: refused",
			subject: "ap.session.ns1.sess-a.out.metaagent_notice",
			data:    envelopeClaiming(t, "ns1", "sess-a", KindMetaagentScopeApproval),
			want:    KindMetaagentNotice,
			wantErr: ErrMetaagentKind,
		},
		{
			name:    "body is not JSON: refused before a channel kind sees it",
			subject: "ap.session.ns1.sess-a.out.metaagent_notice",
			data:    []byte(`not json at all`),
			want:    KindMetaagentNotice,
			wantErr: ErrMetaagentPayload,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := DecodeMetaagentOut(tc.subject, tc.data, tc.want)
			require.Error(t, err)
			assert.ErrorIs(t, err, tc.wantErr)
		})
	}
}

// TestDecodeMetaagentInAndOutAgree pins the two directions to one strictness.
// They are separate entry points precisely so a caller cannot accept the wrong
// half of the bus; the day they stop agreeing on everything BUT the segment is
// the day one of them quietly stops checking something.
func TestDecodeMetaagentInAndOutAgree(t *testing.T) {
	body := []byte(`{"requester":"user:alice","text":"widen"}`)

	inMsg, err := DecodeMetaagentIn(
		SubjectIn(SubjectPrefix("ns1", "sess-a"), KindMetaagentRequest), body, KindMetaagentRequest)
	require.NoError(t, err)
	assert.Equal(t, "ns1/sess-a", inMsg.SessionRef())

	_, err = DecodeMetaagentOut(
		SubjectIn(SubjectPrefix("ns1", "sess-a"), KindMetaagentRequest), body, KindMetaagentRequest)
	assert.ErrorIs(t, err, ErrMetaagentSubject, "an IN subject must not decode as OUT")

	_, err = DecodeMetaagentIn(
		SubjectOut(SubjectPrefix("ns1", "sess-a"), KindMetaagentNotice), body, KindMetaagentNotice)
	assert.ErrorIs(t, err, ErrMetaagentSubject, "an OUT subject must not decode as IN")
}

// TestPublishMetaagentBuildsTheSubjectAndStaysBare pins both halves of the
// publish contract: the subject comes from the shared grammar, and the body is
// the payload itself — no envelope wrapper — so a not-yet-upgraded consumer
// still renders it. Flipping this is a deliberate, coordinated change; it must
// not happen by accident.
func TestPublishMetaagentBuildsTheSubjectAndStaysBare(t *testing.T) {
	type published struct {
		subject string
		data    []byte
	}
	var got []published
	capture := func(subject string, data []byte) error {
		got = append(got, published{subject, data})
		return nil
	}

	require.NoError(t, PublishMetaagentOut(capture, "ns1", "sess-a", KindMetaagentNotice,
		map[string]string{"requester": "U1", "body": "applied"}))
	require.Len(t, got, 1)
	assert.Equal(t, "ap.session.ns1.sess-a.out.metaagent_notice", got[0].subject)
	assert.JSONEq(t, `{"requester":"U1","body":"applied"}`, string(got[0].data),
		"the wire body is the payload itself, not an Envelope")

	got = nil
	require.NoError(t, PublishMetaagentIn(capture, "ns1", "sess-a", KindMetaagentRequest,
		MetaagentRequestPayload{Requester: "user:alice", Text: "widen"}))
	require.Len(t, got, 1)
	assert.Equal(t, "ap.session.ns1.sess-a.in.metaagent_request", got[0].subject)

	// Round-trips through the decoder that reads it back.
	msg, err := DecodeMetaagentIn(got[0].subject, got[0].data, KindMetaagentRequest)
	require.NoError(t, err)
	assert.False(t, msg.Enveloped)
	var back MetaagentRequestPayload
	require.NoError(t, json.Unmarshal(msg.Payload, &back))
	assert.Equal(t, "user:alice", back.Requester)
	assert.Equal(t, "widen", back.Text)
}

// TestPublishMetaagentRefusesNonMetaagentKinds keeps the bare wire format from
// leaking onto a kind whose consumers expect an Envelope. Every such kind is
// relay-handled, so the gate is stated in exactly those terms.
func TestPublishMetaagentRefusesNonMetaagentKinds(t *testing.T) {
	noop := func(string, []byte) error { return nil }

	cases := []struct {
		name string
		ns   string
		obj  string
		kind Kind
	}{
		{name: "a relay-handled kind: refused", ns: "ns1", obj: "sess-a", kind: KindUserMessage},
		{name: "an unregistered kind: refused", ns: "ns1", obj: "sess-a", kind: Kind("made_up")},
		{name: "empty namespace: refused rather than emitting an empty token", ns: "", obj: "sess-a", kind: KindMetaagentNotice},
		{name: "empty name: refused rather than emitting an empty token", ns: "ns1", obj: "", kind: KindMetaagentNotice},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Error(t, PublishMetaagentOut(noop, tc.ns, tc.obj, tc.kind, map[string]string{}))
		})
	}

	assert.Error(t, PublishMetaagentOut(nil, "ns1", "sess-a", KindMetaagentNotice, map[string]string{}),
		"a nil publisher is a wiring bug and must surface as an error, not a nil deref")
}

// TestRelayHandlesCoversExactlyTheMetaagentFamily is the guard on the relay's
// skip. Too broad and the relay stops delivering real traffic; too narrow and
// an approval prompt is both routed to the main sender and delivered by its own
// subscriber.
func TestRelayHandlesCoversExactlyTheMetaagentFamily(t *testing.T) {
	skipped := []Kind{
		KindMetaagentRequest, KindMetaagentScopeApproval,
		KindMetaagentApprovalApplied, KindMetaagentNotice,
	}
	for _, k := range skipped {
		assert.False(t, k.RelayHandles(), "%s is owned by its own subscriber", k)
		assert.True(t, k.Valid(), "%s must be a registered kind", k)
		assert.True(t, k.Implemented(), "%s is wired up today", k)
	}

	relayed := []Kind{
		KindUserMessage, KindNotification, KindToolActivity, KindTurnActivity,
		KindAssistantStreamDelta, KindInteractionRequest, KindThreadTitle,
	}
	for _, k := range relayed {
		assert.True(t, k.RelayHandles(), "%s must still reach the relay", k)
	}
	assert.True(t, Kind("").RelayHandles(),
		"an unrecognized leaf must still reach the relay, which logs its own drop reason")
}

// TestParseOutSubjectKindRejoinsADottedKind guards the one dotted Kind. Reading
// p[5] alone would rename assistant.stream.delta to "assistant" and, with the
// relay skip keyed on that name, silently change which kinds the relay skips.
func TestParseOutSubjectKindRejoinsADottedKind(t *testing.T) {
	subject := SubjectOut(SubjectPrefix("ns1", "sess-a"), KindAssistantStreamDelta)
	ns, name, k, ok := ParseOutSubjectKind(subject)
	require.True(t, ok)
	assert.Equal(t, "ns1", ns)
	assert.Equal(t, "sess-a", name)
	assert.Equal(t, KindAssistantStreamDelta, k)
}

// TestAnySessionPrefixMatchesRealSubjects pins the wildcard prefix subscribers
// build their patterns from against a subject a publisher actually emits.
func TestAnySessionPrefixMatchesRealSubjects(t *testing.T) {
	assert.Equal(t, "ap.session.*.*", AnySessionPrefix())
	assert.Equal(t, "ap.session.*.*.in.metaagent_request",
		SubjectIn(AnySessionPrefix(), KindMetaagentRequest))
	assert.Equal(t, "ap.session.*.*.out.metaagent_notice",
		SubjectOut(AnySessionPrefix(), KindMetaagentNotice))
}
