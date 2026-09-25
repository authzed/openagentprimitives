package channelkinds

// Coverage for the pure leaf helpers on the channelkinds root package:
// ExternalIdentity's identity derivation, InboundEvent.MsgRef, the text
// formatting fallback, ScopesFor, and the fan-out default.
//
// The identity trio carries the most weight. HasIdentity is documented as the
// gate the delivery/resolve paths MUST use instead of testing ExternalID
// alone, because Principal() treats a bare Subject as authoritative — so a
// gate that checked ExternalID would drop every Subject-passthrough identity
// on the floor, and one that skipped the check would hand an unresolvable
// principal to the authorization layer.

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelfeatures"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

func TestExternalIdentity_HasIdentity(t *testing.T) {
	cases := []struct {
		name string
		in   ExternalIdentity
		want bool
	}{
		{
			name: "kind and external id present: resolvable",
			in:   ExternalIdentity{Kind: "slack", ExternalID: "U123"},
			want: true,
		},
		{
			name: "subject passthrough alone: resolvable",
			in:   ExternalIdentity{Subject: "user:alice@example.com"},
			want: true,
		},
		{
			name: "subject passthrough alongside raw fields: resolvable",
			in:   ExternalIdentity{Kind: "slack", ExternalID: "U123", Subject: "user:alice@example.com"},
			want: true,
		},
		{
			name: "zero value: not resolvable",
			in:   ExternalIdentity{},
			want: false,
		},
		{
			name: "kind without external id: not resolvable",
			in:   ExternalIdentity{Kind: "slack"},
			want: false,
		},
		{
			name: "external id without kind: not resolvable",
			in:   ExternalIdentity{ExternalID: "U123"},
			want: false,
		},
		{
			// Email alone cannot address a channel user and does not make the
			// identity resolvable — Principal() would derive from empty
			// kind/externalID.
			name: "email alone: not resolvable",
			in:   ExternalIdentity{Email: "alice@example.com"},
			want: false,
		},
		{
			name: "display name alone: not resolvable",
			in:   ExternalIdentity{DisplayName: "Alice"},
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, tc.in.HasIdentity())
		})
	}
}

// TestExternalIdentity_Principal_SubjectWins pins the passthrough precedence:
// a pre-formed Subject bypasses the kind/teamScope/externalID/email encoding
// entirely, so the raw channel fields must not leak into the derived subject.
func TestExternalIdentity_Principal_SubjectWins(t *testing.T) {
	e := ExternalIdentity{
		Kind:       "slack",
		ExternalID: "U123",
		Email:      "alice@example.com",
		TeamScope:  "T999",
		Subject:    "user:preformed-subject",
	}
	got := e.Principal()
	assert.Equal(t, identity.RawSubject("user:preformed-subject"), got,
		"a pre-formed Subject must pass through verbatim, not be re-derived from the channel fields")
}

func TestExternalIdentity_Principal_DerivesFromRawFields(t *testing.T) {
	e := ExternalIdentity{
		Kind:       "slack",
		ExternalID: "U123",
		Email:      "Alice@Example.com",
		TeamScope:  "T999",
	}
	got := e.Principal()

	assert.Equal(t, identity.Kind("slack"), got.Kind())
	assert.Equal(t, identity.RawExternalID("U123"), got.ExternalID())
	assert.Equal(t, identity.TeamScope("T999"), got.TeamScope())
	assert.Equal(t, identity.Email("alice@example.com"), got.Email(),
		"the email must be lower-cased on the way in, or the canonical id depends on how the user typed it")
}

// TestFromPrincipal_RoundTripsRawFields covers the documented round trip and
// its documented exception: a Subject-passthrough Principal has no accessor
// for its subject, so FromPrincipal cannot recover it.
func TestFromPrincipal_RoundTripsRawFields(t *testing.T) {
	t.Run("raw-field identity survives the round trip", func(t *testing.T) {
		orig := ExternalIdentity{
			Kind:       "slack",
			ExternalID: "U123",
			Email:      "alice@example.com",
			TeamScope:  "T999",
		}
		got := FromPrincipal(orig.Principal())

		assert.Equal(t, orig.Kind, got.Kind)
		assert.Equal(t, orig.ExternalID, got.ExternalID)
		assert.Equal(t, orig.Email, got.Email)
		assert.Equal(t, orig.TeamScope, got.TeamScope)
		assert.True(t, got.HasIdentity(), "a round-tripped identity must still pass the delivery gate")
	})

	t.Run("DisplayName is not part of Principal and is dropped", func(t *testing.T) {
		orig := ExternalIdentity{Kind: "slack", ExternalID: "U123", DisplayName: "Alice"}
		assert.Empty(t, FromPrincipal(orig.Principal()).DisplayName)
	})

	t.Run("a Subject passthrough does NOT round-trip and the result is unresolvable", func(t *testing.T) {
		orig := ExternalIdentity{Subject: "user:preformed-subject"}
		got := FromPrincipal(orig.Principal())

		assert.Empty(t, got.Subject,
			"documented: FromPrincipal cannot recover a RawSubject passthrough")
		assert.False(t, got.HasIdentity(),
			"the lost passthrough must read as unresolvable, not as a silently-empty identity that passes the gate")
	})
}

func TestInboundEvent_MsgRef(t *testing.T) {
	cases := []struct {
		name string
		in   InboundEvent
		want string
	}{
		{
			name: "slack with thread: channel:thread:message",
			in: InboundEvent{
				ExternalIDs: ExternalIdentity{Kind: "slack"},
				External:    map[string]string{"channel_id": "C1", "thread_ts": "111.1", "message_ts": "222.2"},
			},
			want: "C1:111.1:222.2",
		},
		{
			name: "slack root message (no thread_ts): empty middle segment is kept",
			in: InboundEvent{
				ExternalIDs: ExternalIdentity{Kind: "slack"},
				External:    map[string]string{"channel_id": "C1", "message_ts": "222.2"},
			},
			want: "C1::222.2",
		},
		{
			name: "slack missing channel_id: empty, so the index entry is skipped",
			in: InboundEvent{
				ExternalIDs: ExternalIdentity{Kind: "slack"},
				External:    map[string]string{"message_ts": "222.2"},
			},
			want: "",
		},
		{
			name: "slack missing message_ts: empty, so the index entry is skipped",
			in: InboundEvent{
				ExternalIDs: ExternalIdentity{Kind: "slack"},
				External:    map[string]string{"channel_id": "C1"},
			},
			want: "",
		},
		{
			name: "slack with no External map at all: empty",
			in:   InboundEvent{ExternalIDs: ExternalIdentity{Kind: "slack"}},
			want: "",
		},
		{
			name: "fake reads its own key",
			in: InboundEvent{
				ExternalIDs: ExternalIdentity{Kind: "fake"},
				External:    map[string]string{"fake_ref": "ref-1"},
			},
			want: "ref-1",
		},
		{
			name: "fake without its key: empty",
			in:   InboundEvent{ExternalIDs: ExternalIdentity{Kind: "fake"}, External: map[string]string{}},
			want: "",
		},
		{
			name: "an unknown kind never invents a ref",
			in: InboundEvent{
				ExternalIDs: ExternalIdentity{Kind: "browser"},
				External:    map[string]string{"channel_id": "C1", "message_ts": "222.2"},
			},
			want: "",
		},
		{
			name: "an empty kind never invents a ref",
			in:   InboundEvent{External: map[string]string{"channel_id": "C1", "message_ts": "222.2"}},
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, tc.in.MsgRef())
		})
	}
}

// formatterKind is a Kind stub that declares text-formatting instructions.
// Only the two methods under test are meaningful; the rest satisfy the
// interface via the embedded stub.
type formatterKind struct {
	Kind
	instr string
}

func (f formatterKind) TextFormattingInstructions() string { return f.instr }

// plainKind implements Kind but NOT TextFormatter, the case the fallback
// exists for.
type plainKind struct{ Kind }

func TestTextFormattingInstructionsFor(t *testing.T) {
	cases := []struct {
		name string
		in   Kind
		want string
	}{
		{
			name: "a kind declaring a dialect: its own instructions",
			in:   formatterKind{instr: "Use mrkdwn: *bold*, _italic_. Markdown ** is NOT supported."},
			want: "Use mrkdwn: *bold*, _italic_. Markdown ** is NOT supported.",
		},
		{
			name: "a kind not implementing TextFormatter: the weakest true default",
			in:   plainKind{},
			want: DefaultTextFormattingInstructions,
		},
		{
			name: "a nil kind (unregistered name): the default, never a panic",
			in:   nil,
			want: DefaultTextFormattingInstructions,
		},
		{
			name: "a kind returning an empty string: the default, never empty prompt text",
			in:   formatterKind{instr: ""},
			want: DefaultTextFormattingInstructions,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, TextFormattingInstructionsFor(tc.in))
		})
	}
}

// scopedKind is a Kind stub with a scriptable FeatureSupport map.
type scopedKind struct {
	Kind
	support map[channelfeatures.Feature]FeatureRequirement
}

func (s scopedKind) FeatureSupport() map[channelfeatures.Feature]FeatureRequirement { return s.support }

func TestScopesFor(t *testing.T) {
	const (
		featA channelfeatures.Feature = "feature-a"
		featB channelfeatures.Feature = "feature-b"
		featC channelfeatures.Feature = "feature-c"
	)
	kind := scopedKind{support: map[channelfeatures.Feature]FeatureRequirement{
		featA: {Scopes: []string{"chat:write", "channels:read"}},
		featB: {Scopes: []string{"channels:read", "users:read"}},
		featC: {Scopes: nil}, // supported, needs no extra permission
	}}

	t.Run("union across features is deduplicated and sorted", func(t *testing.T) {
		// Sorted output is load-bearing: the generated app manifest is compared
		// byte-for-byte across runs, and map order would make it churn.
		got := ScopesFor(kind, []channelfeatures.Feature{featA, featB})
		assert.Equal(t, []string{"channels:read", "chat:write", "users:read"}, got)
	})

	t.Run("an unsupported feature contributes nothing", func(t *testing.T) {
		got := ScopesFor(kind, []channelfeatures.Feature{featA, "not-supported"})
		assert.Equal(t, []string{"channels:read", "chat:write"}, got)
	})

	t.Run("a supported feature needing no scopes contributes nothing", func(t *testing.T) {
		assert.Empty(t, ScopesFor(kind, []channelfeatures.Feature{featC}))
	})

	t.Run("no features requested: empty", func(t *testing.T) {
		assert.Empty(t, ScopesFor(kind, nil))
	})

	t.Run("a kind supporting no features at all: nil", func(t *testing.T) {
		assert.Nil(t, ScopesFor(scopedKind{}, []channelfeatures.Feature{featA}))
	})

	t.Run("the result is stable across repeated calls", func(t *testing.T) {
		feats := []channelfeatures.Feature{featA, featB}
		first := ScopesFor(kind, feats)
		for range 20 {
			require.Equal(t, first, ScopesFor(kind, feats),
				"map iteration order must not reach the output, or the manifest churns")
		}
	})
}

func TestDeps_ResolvedApproverFanoutLimit(t *testing.T) {
	cases := []struct {
		name string
		in   int
		want int
	}{
		{name: "unset: the package default", in: 0, want: DefaultApproverFanoutLimit},
		{name: "negative (invalid): the package default", in: -5, want: DefaultApproverFanoutLimit},
		{name: "a positive value: used as configured", in: 3, want: 3},
		{name: "one: used as configured", in: 1, want: 1},
		{name: "a value above the default: used as configured", in: 100, want: 100},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, Deps{ApproverFanoutLimit: tc.in}.ResolvedApproverFanoutLimit())
		})
	}
}
