package meta_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/meta"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelfeatures"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

// stubKind lets tests configure SupportedMentionLookups, LookupUser
// outcomes, MentionToolDescription, and RenderMention without pulling
// the slack package.
type stubKind struct {
	supports []channelkinds.MentionLookupKind
	desc     string
	mention  func(externalID string) string
	lookup   func(ctx context.Context, kind channelkinds.MentionLookupKind, value string) (extID, name string, err error)
}

func (s *stubKind) Name() string                                                        { return "stub" }
func (s *stubKind) DefaultSessionScope() string                                         { return "auto" }
func (s *stubKind) Capabilities() []string                                              { return []string{"text"} }
func (s *stubKind) NewListener(channelkinds.Deps) channelkinds.Listener                 { return nil }
func (s *stubKind) NewSender(channelkinds.Deps) channelkinds.Sender                     { return nil }
func (s *stubKind) SubChannelSender(string, channelkinds.Deps) channelkinds.Sender      { return nil }
func (s *stubKind) NewStreamDeltaSink(channelkinds.Deps) channelkinds.StreamDeltaSink   { return nil }
func (s *stubKind) SupportsMonitoring() bool                                            { return false }
func (s *stubKind) SupportsLiveViewOffer() bool                                         { return false }
func (s *stubKind) NewMonitoringSender(channelkinds.Deps) channelkinds.MonitoringSender { return nil }
func (s *stubKind) WebAuthenticator(channelkinds.WebAuthDeps) channelkinds.WebAuthenticator {
	return nil
}
func (s *stubKind) WebhookReceiver(channelkinds.Deps) channelkinds.WebhookReceiver { return nil }
func (s *stubKind) SupportedRoles() []string                                       { return spiceboxv1alpha1.AllChannelRoles() }
func (s *stubKind) ValidateSpec(*spiceboxv1alpha1.Channel) error                   { return nil }
func (s *stubKind) PublicSecretKeys(*spiceboxv1alpha1.Channel) []string            { return nil }
func (s *stubKind) RequiredSecretKeys(*spiceboxv1alpha1.Channel) []string          { return nil }
func (s *stubKind) FeatureSupport() map[channelfeatures.Feature]channelkinds.FeatureRequirement {
	return nil
}
func (s *stubKind) RenderMention(externalID string) string {
	if s.mention != nil {
		return s.mention(externalID)
	}
	return externalID
}
func (s *stubKind) Wizard() channelkinds.Wizard                               { return nil }
func (s *stubKind) SupportedMentionLookups() []channelkinds.MentionLookupKind { return s.supports }
func (s *stubKind) LookupUser(
	ctx context.Context, _ channelkinds.LookupDeps,
	kind channelkinds.MentionLookupKind, value string,
) (string, string, error) {
	if s.lookup != nil {
		return s.lookup(ctx, kind, value)
	}
	return "", "", channelkinds.ErrMentionUnsupported
}
func (s *stubKind) MentionToolDescription() string { return s.desc }
func (s *stubKind) UserAttributable() bool         { return true }
func (s *stubKind) DeliversToHuman() bool          { return true }
func (s *stubKind) AllowsSyntheticIdentity() bool  { return false }
func (s *stubKind) RelayedByChannelsd() bool       { return true }
func (s *stubKind) SpawnsSessionOnInbound() bool   { return true }

func newLookupTool(t *testing.T, k *stubKind) tool.Tool {
	t.Helper()
	return meta.NewLookupUserForMention(meta.LookupUserForMentionConfig{
		Kind:        k,
		LookupDeps:  channelkinds.LookupDeps{Secret: &corev1.Secret{}},
		ChannelKind: "stub",
	})
}

func TestNewLookupUserForMentionNilWhenNoSupportedLookups(t *testing.T) {
	got := newLookupTool(t, &stubKind{})
	assert.Nil(t, got, "NewLookupUserForMention must return nil when no supported lookups")
}

func TestNewLookupUserForMentionSchemaEnumReflectsKind(t *testing.T) {
	k := &stubKind{supports: []channelkinds.MentionLookupKind{channelkinds.MentionLookupEmail}}
	tl := newLookupTool(t, k)
	require.NotNil(t, tl, "NewLookupUserForMention must return non-nil with at least one supported lookup")

	var schema struct {
		Properties struct {
			Kind struct {
				Enum []string `json:"enum"`
			} `json:"kind"`
		} `json:"properties"`
	}
	require.NoError(t, json.Unmarshal(tl.InputSchema(), &schema), "unmarshal schema")
	assert.Equal(t, []string{"email"}, schema.Properties.Kind.Enum, "kind enum must mirror supports")
}

func TestNewLookupUserForMentionUsesKindDescription(t *testing.T) {
	tl := newLookupTool(t, &stubKind{
		supports: []channelkinds.MentionLookupKind{channelkinds.MentionLookupEmail},
		desc:     "stub-description-marker",
	})
	require.NotNil(t, tl, "tool must be non-nil")
	assert.Contains(t, tl.Description(), "stub-description-marker",
		"Description must include the kind's MentionToolDescription")
}

func TestNewLookupUserForMentionFallsBackToGenericDescription(t *testing.T) {
	tl := newLookupTool(t, &stubKind{
		supports: []channelkinds.MentionLookupKind{channelkinds.MentionLookupEmail},
		desc:     "",
	})
	require.NotNil(t, tl, "tool must be non-nil")
	assert.NotEmpty(t, strings.TrimSpace(tl.Description()),
		"Description must be non-empty even when the kind returns empty")
}

// TestLookupUserForMentionExecute collapses the six Execute-shape cases:
// each builds a stubKind with specific supports/lookup, runs Execute with
// the input args, and inspects the result. The shared shape (build kind,
// build tool, execute, check) is what makes a table the right fit here.
func TestLookupUserForMentionExecute(t *testing.T) {
	cases := []struct {
		name  string
		stub  *stubKind
		args  string
		check func(t *testing.T, res tool.Result)
	}{
		{
			name: "happy path: renders mention markup",
			stub: &stubKind{
				supports: []channelkinds.MentionLookupKind{channelkinds.MentionLookupEmail},
				mention:  func(extID string) string { return "<@" + extID + ">" },
				lookup: func(_ context.Context, _ channelkinds.MentionLookupKind, _ string) (string, string, error) {
					return "U01234", "Fred Smith", nil
				},
			},
			args: `{"kind":"email","value":"fred@example.com"}`,
			check: func(t *testing.T, res tool.Result) {
				assert.False(t, res.IsError, "happy path must not be IsError")
				assert.Equal(t, "<@U01234>", res.Content, "content must be rendered mention")
				assert.True(t, res.Trusted, "lookup_user_for_mention is a framework meta tool and must opt out of content-guard inspection")
			},
		},
		{
			name: "lookup returns not-found: IsError mentions value",
			stub: &stubKind{
				supports: []channelkinds.MentionLookupKind{channelkinds.MentionLookupEmail},
				lookup: func(_ context.Context, _ channelkinds.MentionLookupKind, _ string) (string, string, error) {
					return "", "", channelkinds.ErrMentionNotFound
				},
			},
			args: `{"kind":"email","value":"ghost@example.com"}`,
			check: func(t *testing.T, res tool.Result) {
				assert.True(t, res.IsError, "not-found must produce IsError")
				assert.Contains(t, res.Content, "no user found", "error must say 'no user found'")
				assert.Contains(t, res.Content, "ghost@example.com", "error must include the value")
			},
		},
		{
			name: "lookup returns ambiguous: IsError mentions 'multiple users'",
			stub: &stubKind{
				supports: []channelkinds.MentionLookupKind{channelkinds.MentionLookupName},
				lookup: func(_ context.Context, _ channelkinds.MentionLookupKind, _ string) (string, string, error) {
					return "", "", channelkinds.ErrMentionAmbiguous
				},
			},
			args: `{"kind":"name","value":"Fred"}`,
			check: func(t *testing.T, res tool.Result) {
				assert.True(t, res.IsError, "ambiguous must produce IsError")
				assert.Contains(t, res.Content, "multiple users", "error must mention 'multiple users'")
			},
		},
		{
			name: "kind not in supports: IsError mentions supported kinds",
			stub: &stubKind{
				supports: []channelkinds.MentionLookupKind{channelkinds.MentionLookupEmail},
			},
			args: `{"kind":"name","value":"Fred"}`,
			check: func(t *testing.T, res tool.Result) {
				assert.True(t, res.IsError, "unsupported kind must produce IsError")
				assert.Contains(t, res.Content, "supported", "error must mention supported kinds")
			},
		},
		{
			name: "empty value: IsError",
			stub: &stubKind{
				supports: []channelkinds.MentionLookupKind{channelkinds.MentionLookupEmail},
			},
			args: `{"kind":"email","value":""}`,
			check: func(t *testing.T, res tool.Result) {
				assert.True(t, res.IsError, "empty value must produce IsError")
			},
		},
		{
			name: "transport error: IsError wraps underlying error",
			stub: &stubKind{
				supports: []channelkinds.MentionLookupKind{channelkinds.MentionLookupEmail},
				lookup: func(_ context.Context, _ channelkinds.MentionLookupKind, _ string) (string, string, error) {
					return "", "", errors.New("slack 429 rate-limited")
				},
			},
			args: `{"kind":"email","value":"fred@x"}`,
			check: func(t *testing.T, res tool.Result) {
				assert.True(t, res.IsError, "transport error must produce IsError")
				assert.Contains(t, res.Content, "rate-limited", "error must include underlying message")
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tl := meta.NewLookupUserForMention(meta.LookupUserForMentionConfig{
				Kind: tc.stub, ChannelKind: "stub",
			})
			require.NotNil(t, tl, "tool must be non-nil for Execute case")
			res, _ := tl.Execute(context.Background(), json.RawMessage(tc.args), &tool.SessionContext{})
			tc.check(t, res)
		})
	}
}
