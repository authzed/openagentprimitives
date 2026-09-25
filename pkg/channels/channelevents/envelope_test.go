package channelevents

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEnvelopeRoundTrip(t *testing.T) {
	pl := OutboundUserMessagePayload{Text: "hello"}
	plBytes, err := json.Marshal(pl)
	require.NoError(t, err, "marshal payload")

	now := time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC)
	in := Envelope{
		Version:     1,
		Kind:        KindUserMessage,
		Session:     SessionRef{Namespace: "default", Name: "review-pr-1234"},
		PublishedAt: now,
		Payload:     plBytes,
	}
	bytes, err := json.Marshal(in)
	require.NoError(t, err, "marshal envelope")

	var out Envelope
	require.NoError(t, json.Unmarshal(bytes, &out), "unmarshal envelope")

	assert.True(t, out.PublishedAt.Equal(now), "PublishedAt should roundtrip")
	assert.Equal(t, KindUserMessage, out.Kind, "Kind should roundtrip")
	assert.Equal(t, in.Session, out.Session, "Session should roundtrip")

	var rt OutboundUserMessagePayload
	require.NoError(t, json.Unmarshal(out.Payload, &rt), "unmarshal payload")
	assert.Equal(t, "hello", rt.Text, "payload Text should roundtrip")
}

func TestEnvelopeValidate(t *testing.T) {
	good := Envelope{
		Version:     1,
		Kind:        KindUserMessage,
		Session:     SessionRef{Namespace: "ns", Name: "n"},
		PublishedAt: time.Now().UTC(),
		Payload:     []byte("{}"),
	}
	require.NoError(t, good.Validate(), "good envelope should validate")

	cases := []struct {
		name string
		mut  func(*Envelope)
	}{
		{"version 0: rejected", func(e *Envelope) { e.Version = 0 }},
		{"unknown kind: rejected", func(e *Envelope) { e.Kind = "nope" }},
		{"empty namespace: rejected", func(e *Envelope) { e.Session.Namespace = "" }},
		{"empty name: rejected", func(e *Envelope) { e.Session.Name = "" }},
		{"nil payload: rejected", func(e *Envelope) { e.Payload = nil }},
		{"zero PublishedAt: rejected", func(e *Envelope) { e.PublishedAt = time.Time{} }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := good
			tc.mut(&e)
			assert.Error(t, e.Validate(), "expected validation error")
		})
	}
}

func TestSubjectBuilders(t *testing.T) {
	prefix := "ap.session.default.review-pr-1234"
	cases := []struct {
		name string
		got  string
		want string
	}{
		{"SubjectIn: in.user_message suffix", SubjectIn(prefix, KindUserMessage), prefix + ".in.user_message"},
		{"SubjectOut: out.user_message suffix", SubjectOut(prefix, KindUserMessage), prefix + ".out.user_message"},
		{"SubjectPrefix: ap.session.<ns>.<name>", SubjectPrefix("default", "foo"), "ap.session.default.foo"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, tc.got)
		})
	}
}

func TestOutboundUserMessage_AttachmentsRoundTrip(t *testing.T) {
	pl := OutboundUserMessagePayload{
		Text: "see attached",
		Attachments: []AttachmentRef{
			{RenderName: "ar-1", MIME: "text/html", Filename: "report.html", AltText: "weekly summary"},
		},
	}
	env, err := BuildEnvelope("default", "sess1", KindUserMessage, pl)
	require.NoError(t, err, "BuildEnvelope")

	var got OutboundUserMessagePayload
	require.NoError(t, json.Unmarshal(env.Payload, &got), "unmarshal payload")
	require.Len(t, got.Attachments, 1, "exactly one attachment expected")
	assert.Equal(t, "ar-1", got.Attachments[0].RenderName, "attachment RenderName")
}

func TestOutboundUserMessage_ValidateRejectsEmptyRenderName(t *testing.T) {
	pl := OutboundUserMessagePayload{
		Attachments: []AttachmentRef{{RenderName: ""}},
	}
	assert.Error(t, pl.Validate(), "expected error on empty renderName")
}

func TestAssistantStreamDelta_RoundTrip(t *testing.T) {
	pl := AssistantStreamDeltaPayload{
		EventType: "text_delta",
		Text:      "hello",
		BlockIdx:  0,
	}
	env, err := BuildEnvelope("default", "sess1", KindAssistantStreamDelta, pl)
	require.NoError(t, err, "BuildEnvelope")
	assert.Equal(t, KindAssistantStreamDelta, env.Kind, "envelope Kind")

	var got AssistantStreamDeltaPayload
	require.NoError(t, json.Unmarshal(env.Payload, &got), "unmarshal payload")
	assert.Equal(t, "text_delta", got.EventType, "EventType")
	assert.Equal(t, "hello", got.Text, "Text")
}

func TestAssistantStreamDelta_ToolUseStart_RoundTrip(t *testing.T) {
	pl := AssistantStreamDeltaPayload{
		EventType: "tool_use_start",
		ToolName:  "linear_list_issues",
		ToolID:    "toolu_abc",
		BlockIdx:  1,
	}
	env, err := BuildEnvelope("default", "sess1", KindAssistantStreamDelta, pl)
	require.NoError(t, err, "BuildEnvelope")

	var got AssistantStreamDeltaPayload
	require.NoError(t, json.Unmarshal(env.Payload, &got), "unmarshal payload")
	assert.Equal(t, "linear_list_issues", got.ToolName, "ToolName")
	assert.Equal(t, "toolu_abc", got.ToolID, "ToolID")
}

func TestPackSeqOrders(t *testing.T) {
	// Later turn always sorts after an earlier turn regardless of block.
	assert.Greater(t, PackSeq(5, SeqBlockStart), PackSeq(4, SeqBlockEnd))
	// Within a turn, higher block index sorts later.
	assert.Greater(t, PackSeq(3, 2), PackSeq(3, 1))
	// Start sentinel precedes the first tool block (i+1).
	assert.Less(t, PackSeq(3, SeqBlockStart), PackSeq(3, 1))
	// End sentinel follows any tool block.
	assert.Greater(t, PackSeq(3, SeqBlockEnd), PackSeq(3, 99))
}

func TestUnpackTurnInvertsPackSeq(t *testing.T) {
	// UnpackTurn recovers the memTurnIndex regardless of the block bits.
	assert.Equal(t, 0, UnpackTurn(PackSeq(0, SeqBlockStart)))
	assert.Equal(t, 0, UnpackTurn(PackSeq(0, SeqBlockEnd)))
	assert.Equal(t, 7, UnpackTurn(PackSeq(7, SeqBlockStart)))
	assert.Equal(t, 7, UnpackTurn(PackSeq(7, 42)))
	assert.Equal(t, 7, UnpackTurn(PackSeq(7, SeqBlockEnd)))
}

func TestPublishOutSeqStampsEnvelope(t *testing.T) {
	var got Envelope
	publish := func(_ string, payload []byte) error {
		return json.Unmarshal(payload, &got)
	}
	err := PublishOutSeq(publish, "ns", "n", KindNotification,
		NotificationPayload{Text: "hi"}, PackSeq(2, 1), "uid-123")
	require.NoError(t, err)
	assert.Equal(t, PackSeq(2, 1), got.Seq)
	assert.Equal(t, "uid-123", got.SessionUID)
}

func TestEnvelopeResurfaceInterruptRoundTrips(t *testing.T) {
	in := Envelope{Version: 1, Kind: KindInteractionRequest, ResurfaceInterruptRequestID: "abc123"}
	data, err := json.Marshal(in)
	require.NoError(t, err)
	assert.Contains(t, string(data), `"resurfaceInterruptRequestID":"abc123"`)

	var out Envelope
	require.NoError(t, json.Unmarshal(data, &out))
	assert.Equal(t, "abc123", out.ResurfaceInterruptRequestID)

	// omitempty: absent when unset.
	bare, err := json.Marshal(Envelope{Version: 1, Kind: KindEnqueueAck})
	require.NoError(t, err)
	assert.NotContains(t, string(bare), "resurfaceInterruptRequestID")
}

func TestToolSessionEventPayload_ReasonRoundTrips(t *testing.T) {
	in := ToolSessionEventPayload{ToolCallRef: "tc-1", EventType: "text_delta", Reason: "Have claude write the README"}
	b, err := json.Marshal(in)
	require.NoError(t, err)
	assert.Contains(t, string(b), `"reason":"Have claude write the README"`)

	var out ToolSessionEventPayload
	require.NoError(t, json.Unmarshal(b, &out))
	assert.Equal(t, "Have claude write the README", out.Reason)

	// omitempty: an empty Reason must not appear in the JSON.
	b2, err := json.Marshal(ToolSessionEventPayload{ToolCallRef: "tc-2", EventType: "result"})
	require.NoError(t, err)
	assert.NotContains(t, string(b2), `"reason"`)
}

// TestParseOutSubject covers the inverse of SubjectOut(SubjectPrefix(...)).
// The dotted-kind row is the important one: KindAssistantStreamDelta is
// "assistant.stream.delta", so its subject has EIGHT tokens, and a parser
// copied verbatim from ParseHistorySubject's fixed len==6 would reject every
// stream delta the relay sees.
func TestParseOutSubject(t *testing.T) {
	cases := []struct {
		name       string
		subject    string
		wantNS     string
		wantName   string
		wantParsed bool
	}{
		{
			name:       "well-formed subject: parsed as (ns, name)",
			subject:    SubjectOut(SubjectPrefix("team-a", "sess-1"), KindUserMessage),
			wantNS:     "team-a",
			wantName:   "sess-1",
			wantParsed: true,
		},
		{
			name:       "dotted kind (assistant.stream.delta): still parsed, extra tokens ignored",
			subject:    SubjectOut(SubjectPrefix("team-a", "sess-1"), KindAssistantStreamDelta),
			wantNS:     "team-a",
			wantName:   "sess-1",
			wantParsed: true,
		},
		{
			name:       "inbound subject: refused, .in. is not .out.",
			subject:    SubjectIn(SubjectPrefix("team-a", "sess-1"), KindUserMessage),
			wantParsed: false,
		},
		{
			name:       "history subject: refused, wrong segment",
			subject:    HistoryRequestSubject(SubjectPrefix("team-a", "sess-1")),
			wantParsed: false,
		},
		{
			name:       "truncated subject with no kind token: refused",
			subject:    "ap.session.team-a.sess-1.out",
			wantParsed: false,
		},
		{
			name:       "foreign prefix: refused",
			subject:    "evil.session.team-a.sess-1.out.user_message",
			wantParsed: false,
		},
		{
			name:       "empty name token: refused rather than routed to a nameless session",
			subject:    "ap.session.team-a..out.user_message",
			wantParsed: false,
		},
		{
			name:       "empty subject: refused",
			subject:    "",
			wantParsed: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ns, name, ok := ParseOutSubject(tc.subject)
			assert.Equal(t, tc.wantParsed, ok)
			assert.Equal(t, tc.wantNS, ns)
			assert.Equal(t, tc.wantName, name)
		})
	}
}

// TestParseInSubject is TestParseOutSubject's twin, for the direction where
// getting it wrong is worse: a runner's PubAllow tree covers ".in." too, so
// this parse is what stops a publisher authorized on one session's inbound
// subject from driving channelsd's handlers against another session.
//
// The dotted-kind row pins the six-or-MORE rule on this side as well. No
// inbound Kind is dotted today (KindAssistantStreamDelta is the only dotted
// Kind and travels outbound only), so the row uses it deliberately as the
// forward guard: the twins must not differ in strictness, and this fails the
// day someone tightens one of them to len==6.
func TestParseInSubject(t *testing.T) {
	cases := []struct {
		name       string
		subject    string
		wantNS     string
		wantName   string
		wantParsed bool
	}{
		{
			name:       "well-formed subject: parsed as (ns, name)",
			subject:    SubjectIn(SubjectPrefix("team-a", "sess-1"), KindInteractionDecision),
			wantNS:     "team-a",
			wantName:   "sess-1",
			wantParsed: true,
		},
		{
			name:       "dotted kind: still parsed, matching ParseOutSubject's strictness exactly",
			subject:    SubjectIn(SubjectPrefix("team-a", "sess-1"), KindAssistantStreamDelta),
			wantNS:     "team-a",
			wantName:   "sess-1",
			wantParsed: true,
		},
		{
			name:       "outbound subject: refused, .out. is not .in.",
			subject:    SubjectOut(SubjectPrefix("team-a", "sess-1"), KindUserMessage),
			wantParsed: false,
		},
		{
			name:       "history subject: refused, wrong segment",
			subject:    HistoryRequestSubject(SubjectPrefix("team-a", "sess-1")),
			wantParsed: false,
		},
		{
			name:       "truncated subject with no kind token: refused",
			subject:    "ap.session.team-a.sess-1.in",
			wantParsed: false,
		},
		{
			name:       "foreign prefix: refused",
			subject:    "evil.session.team-a.sess-1.in.interaction_decision",
			wantParsed: false,
		},
		{
			name:       "empty name token: refused rather than routed to a nameless session",
			subject:    "ap.session.team-a..in.interaction_decision",
			wantParsed: false,
		},
		{
			name:       "empty subject: refused",
			subject:    "",
			wantParsed: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ns, name, ok := ParseInSubject(tc.subject)
			assert.Equal(t, tc.wantParsed, ok)
			assert.Equal(t, tc.wantNS, ns)
			assert.Equal(t, tc.wantName, name)
		})
	}
}
