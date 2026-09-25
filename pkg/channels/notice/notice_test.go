package notice_test

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions"
	"github.com/authzed/openagentprimitives/pkg/channels/notice"
)

const (
	catDanger  = "test_danger_notice"
	catInfo    = "test_info_notice"
	catPrompt  = "test_prompt"
	catUnknown = "test_never_registered"
)

func registerFixtures(t *testing.T) {
	t.Helper()
	channelinteractions.Reset()
	t.Cleanup(channelinteractions.Reset)
	channelinteractions.Register(channelinteractions.Category{
		Name: catDanger, Notice: true,
		Tone:      channelinteractions.ToneCritical,
		Resurface: channelinteractions.ResurfaceNone,
	})
	channelinteractions.Register(channelinteractions.Category{
		Name: catInfo, Notice: true,
		Tone:      channelinteractions.ToneRoutine,
		Resurface: channelinteractions.ResurfaceNone,
	})
	channelinteractions.Register(channelinteractions.Category{
		Name:      catPrompt,
		Deciders:  channelinteractions.DecideOwner,
		Tone:      channelinteractions.ToneRoutine,
		Resurface: channelinteractions.ResurfaceNone,
	})
}

func participants() channelevents.InteractionAudience {
	return channelevents.InteractionAudience{Scope: channelevents.AudienceParticipants}
}

func TestNew_BuildsAValidPayload(t *testing.T) {
	registerFixtures(t)

	n := notice.New(catDanger, notice.Args{
		Lead:     "Agent stopped",
		Body:     "It ran out of memory.",
		NextStep: "Start a new thread to try again.",
		Audience: participants(),
	})
	require.False(t, n.IsSuppressed())

	pl, err := n.Payload(channelevents.SessionRef{Namespace: "agents", Name: "sre-bot-4f2c"}, "req-1")
	require.NoError(t, err)

	assert.Equal(t, catDanger, pl.Category)
	assert.Equal(t, "req-1", pl.RequestRef)
	assert.Equal(t, "Agent stopped", pl.Lead)
	assert.Equal(t, "Start a new thread to try again.", pl.NextStep)
	assert.Empty(t, pl.Actions, "a notice carries no actions")
	assert.NoError(t, pl.Validate(), "must satisfy the wire contract")
}

// The whole point of NextStep being a field: omitting it on a tone that
// requires one is an error the caller must handle, not a silent gap the user
// discovers by being stuck.
func TestNew_NextStepEnforcedBySeverity(t *testing.T) {
	registerFixtures(t)

	cases := []struct {
		name     string
		category string
		nextStep string
		wantErr  string
	}{
		{name: "danger without a next step: rejected", category: catDanger, wantErr: "nextStep"},
		{name: "danger with a next step: accepted", category: catDanger, nextStep: "Start a new thread."},
		{name: "info without a next step: accepted", category: catInfo},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			n := notice.New(tc.category, notice.Args{
				Lead: "Something happened", NextStep: tc.nextStep, Audience: participants(),
			})
			_, err := n.Payload(channelevents.SessionRef{Namespace: "a", Name: "b"}, "req-1")
			if tc.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

func TestNew_RejectsNonNoticeAndUnknownCategories(t *testing.T) {
	registerFixtures(t)

	cases := []struct {
		name     string
		category string
		wantErr  string
	}{
		{name: "unregistered category", category: catUnknown, wantErr: "not registered"},
		{name: "prompt category used as a notice", category: catPrompt, wantErr: "not a notice"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			n := notice.New(tc.category, notice.Args{
				Lead: "x", NextStep: "y", Audience: participants(),
			})
			_, err := n.Payload(channelevents.SessionRef{Namespace: "a", Name: "b"}, "req-1")
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

func TestNew_RequiresLead(t *testing.T) {
	registerFixtures(t)
	n := notice.New(catInfo, notice.Args{Audience: participants()})
	_, err := n.Payload(channelevents.SessionRef{Namespace: "a", Name: "b"}, "req-1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "lead")
}

// Suppression is a decision, not an accident. It carries a reason so the
// "posted nothing" path still leaves a log line — the no-silent-errors rule
// applied to the one case where posting nothing is correct.
func TestSuppressed(t *testing.T) {
	n := notice.Suppressed("requester already has a pending rejection in this thread")
	require.True(t, n.IsSuppressed())
	assert.Equal(t, "requester already has a pending rejection in this thread", n.SuppressReason())

	_, err := n.Payload(channelevents.SessionRef{Namespace: "a", Name: "b"}, "req-1")
	require.Error(t, err, "a suppressed notice has no payload; callers must check IsSuppressed first")
}

// A nil *Notice is the "nothing to say" case and must never panic a caller
// that reaches for it — the inbound-decision path has outcomes that set no
// notice at all.
func TestNilNoticeIsInert(t *testing.T) {
	var n *notice.Notice
	assert.True(t, n.IsSuppressed())
	assert.Empty(t, n.SuppressReason())
	_, err := n.Payload(channelevents.SessionRef{Namespace: "a", Name: "b"}, "r")
	require.Error(t, err)
}

func TestPublishSigned_EmitsVerifiableEnvelope(t *testing.T) {
	registerFixtures(t)
	priv := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x44}, ed25519.SeedSize))
	signer, err := channelevents.NewEnvelopeSigner(priv, "session:agents/sre-bot-4f2c", "uid-1")
	require.NoError(t, err)

	var gotSubject string
	var got channelevents.Envelope
	pub := func(subject string, data []byte) error {
		gotSubject = subject
		return json.Unmarshal(data, &got)
	}

	n := notice.New(catDanger, notice.Args{
		Lead:     "Agent stopped",
		NextStep: "Start a new thread to try again.",
		Audience: participants(),
	})
	sess := channelevents.SessionRef{Namespace: "agents", Name: "sre-bot-4f2c"}
	require.NoError(t, n.PublishSigned(signer, pub, sess, "req-1"))

	assert.Equal(t,
		channelevents.SubjectOut(channelevents.SubjectPrefix("agents", "sre-bot-4f2c"), channelevents.KindInteractionRequest),
		gotSubject)
	assert.Equal(t, "session:agents/sre-bot-4f2c", got.Publisher)
	assert.NoError(t, channelevents.VerifyEnvelopeSig(gotSubject, got, priv.Public().(ed25519.PublicKey)))
}

// The legacy Publish path stays unsigned: existing non-runner callers
// (channelsd, webd, authz hooks) keep publishing exactly the bytes they did
// before the signer-aware variant existed.
func TestPublish_LegacyPathStaysUnsigned(t *testing.T) {
	registerFixtures(t)

	var gotSubject string
	var got channelevents.Envelope
	pub := func(subject string, data []byte) error {
		gotSubject = subject
		return json.Unmarshal(data, &got)
	}

	n := notice.New(catDanger, notice.Args{
		Lead:     "Agent stopped",
		NextStep: "Start a new thread to try again.",
		Audience: participants(),
	})
	sess := channelevents.SessionRef{Namespace: "agents", Name: "sre-bot-4f2c"}
	require.NoError(t, n.Publish(pub, sess, "req-1"))

	assert.Empty(t, got.Sig, "legacy Publish must not sign")
	assert.Empty(t, got.Publisher, "legacy Publish must not stamp a publisher identity")
	pub2 := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x44}, ed25519.SeedSize)).Public().(ed25519.PublicKey)
	assert.ErrorIs(t, channelevents.VerifyEnvelopeSig(gotSubject, got, pub2), channelevents.ErrEnvelopeUnsigned)
}

func TestToneAndGlyphComeFromTheRegistry(t *testing.T) {
	channelinteractions.Reset()
	t.Cleanup(channelinteractions.Reset)
	channelinteractions.Register(channelinteractions.Category{
		Name: "test_locked", Notice: true,
		Tone:      channelinteractions.ToneCritical,
		Glyph:     channelinteractions.GlyphMoney,
		Resurface: channelinteractions.ResurfaceNone,
	})

	n := notice.New("test_locked", notice.Args{
		Lead: "Blocked", NextStep: "Ask an approver.", Audience: participants(),
	})
	tone, terminal, glyph, err := n.Style()
	require.NoError(t, err)
	assert.Equal(t, channelinteractions.ToneCritical, tone)
	assert.False(t, terminal, "a category that does not declare Terminal is not terminal")
	assert.Equal(t, channelinteractions.GlyphMoney, glyph)
}
