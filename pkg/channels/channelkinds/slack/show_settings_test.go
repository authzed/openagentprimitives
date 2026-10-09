package slack

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	slackapi "github.com/slack-go/slack"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

// --- test helpers ---

// settingsTestScheme builds a minimal runtime scheme with the agentprimitives types.
func settingsTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	sch := runtime.NewScheme()
	require.NoError(t, spiceboxv1alpha1.AddToScheme(sch))
	return sch
}

// buildAgentSession constructs an AgentSession with the given effectiveSettings.
func buildAgentSession(ns, name string, eff *spiceboxv1alpha1.EffectiveSettings) spiceboxv1alpha1.AgentSession {
	return spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Status:     spiceboxv1alpha1.AgentSessionStatus{EffectiveSettings: eff},
	}
}

// sessionInfoForTest builds a minimal SessionInfo for block-builder tests.
func sessionInfoForTest(ns, name string) channelkinds.SessionInfo {
	return channelkinds.SessionInfo{
		Namespace: ns,
		Name:      name,
		Channel: &spiceboxv1alpha1.ChannelBinding{
			Name: "slack-test", Kind: "slack",
			External: map[string]string{
				"channel_id": "C-TEST",
				"thread_ts":  "1234.5678",
			},
		},
	}
}

// noopCtx returns a background context for tests.
func noopCtx(t *testing.T) context.Context {
	t.Helper()
	return context.Background()
}

// noopLogger implements the logger interface expected by buildUserMessageBlocks.
type noopLogger struct{}

func (noopLogger) Info(_ string, _ ...any) {}

// --- helpers for block inspection ---

// blocksText collects all mrkdwn/plain_text text from the block set.
func blocksText(blocks []slackapi.Block) string {
	var parts []string
	for _, b := range blocks {
		switch v := b.(type) {
		case *slackapi.SectionBlock:
			if v.Text != nil {
				parts = append(parts, v.Text.Text)
			}
		case *slackapi.ContextBlock:
			for _, e := range v.ContextElements.Elements {
				if t, ok := e.(*slackapi.TextBlockObject); ok {
					parts = append(parts, t.Text)
				}
			}
		}
	}
	return strings.Join(parts, "\n")
}

func hasActionBlockWithID(blocks []slackapi.Block, actionID string) bool {
	for _, b := range blocks {
		ab, ok := b.(*slackapi.ActionBlock)
		if !ok {
			continue
		}
		for _, el := range ab.Elements.ElementSet {
			if btn, ok := el.(*slackapi.ButtonBlockElement); ok {
				if btn.ActionID == actionID {
					return true
				}
			}
		}
	}
	return false
}

// --- render function tests ---

func TestRenderSettingsModalBlocks_NilEff(t *testing.T) {
	blocks := renderSettingsModalBlocks("default/my-session", nil)
	require.NotEmpty(t, blocks)
	txt := blocksText(blocks)
	assert.Contains(t, txt, "aren't ready yet")
}

func TestRenderSettingsModalBlocks_ModelAndBudget(t *testing.T) {
	eff := &spiceboxv1alpha1.EffectiveSettings{
		Model: spiceboxv1alpha1.ModelConfig{
			Provider: "anthropic",
			Name:     "claude-3-5-sonnet",
		},
		Budget: spiceboxv1alpha1.BudgetConfig{
			MaxTurns:    10,
			MaxTokens:   50000,
			MaxDuration: metav1.Duration{Duration: 30 * time.Minute},
		},
		Provenance: map[string]string{
			"model": "class",
		},
	}

	blocks := renderSettingsModalBlocks("default/my-session", eff)
	require.NotEmpty(t, blocks)
	txt := blocksText(blocks)

	assert.Contains(t, txt, "anthropic/claude-3-5-sonnet")
	assert.Contains(t, txt, "Max turns: `10`")
	assert.Contains(t, txt, "Max tokens: `50000`")
	assert.Contains(t, txt, "30m0s")
	// No clamp annotation expected.
	assert.NotContains(t, txt, "clamped")
}

func TestRenderSettingsModalBlocks_ClampAnnotated(t *testing.T) {
	eff := &spiceboxv1alpha1.EffectiveSettings{
		Model: spiceboxv1alpha1.ModelConfig{
			Provider: "anthropic",
			Name:     "claude-3-5-sonnet",
		},
		Budget: spiceboxv1alpha1.BudgetConfig{
			MaxTurns:  5,
			MaxTokens: 10000,
		},
		Provenance: map[string]string{
			"budget.maxTurns":  "clamped",
			"budget.maxTokens": "clamped",
		},
	}

	blocks := renderSettingsModalBlocks("default/my-session", eff)
	require.NotEmpty(t, blocks)
	txt := blocksText(blocks)

	// Both clamped dims should be annotated.
	assert.Contains(t, txt, "Max turns: `5` _(clamped)_")
	assert.Contains(t, txt, "Max tokens: `10000` _(clamped)_")
	// Max duration was not clamped — rendered without "(clamped)" suffix.
	assert.Contains(t, txt, "Max duration: `none`")
	assert.NotContains(t, txt, "Max duration: `none` _(clamped)_")
}

func TestRenderSettingsModalBlocks_Allowlists(t *testing.T) {
	eff := &spiceboxv1alpha1.EffectiveSettings{
		AllowedToolkits: []string{"search", "code"},
	}

	blocks := renderSettingsModalBlocks("default/my-session", eff)
	txt := blocksText(blocks)

	assert.NotContains(t, txt, "Allowed models:")
	assert.Contains(t, txt, "Allowed toolkits:")
	assert.Contains(t, txt, "search")
}

func TestRenderSettingsModalBlocks_IncludesSessionID(t *testing.T) {
	eff := &spiceboxv1alpha1.EffectiveSettings{
		Model: spiceboxv1alpha1.ModelConfig{Provider: "anthropic", Name: "claude-3-5-sonnet"},
	}
	blocks := renderSettingsModalBlocks("team-a/support-session", eff)
	txt := blocksText(blocks)
	assert.Contains(t, txt, "team-a/support-session",
		"the resolved settings modal must identify which session it describes")
}

func TestRenderSettingsModalBlocks_NilEff_IncludesSessionID(t *testing.T) {
	// Even the not-ready-yet placeholder must name the session.
	blocks := renderSettingsModalBlocks("team-a/support-session", nil)
	txt := blocksText(blocks)
	assert.Contains(t, txt, "aren't ready yet")
	assert.Contains(t, txt, "team-a/support-session",
		"the unresolved-settings modal must still identify the session")
}

// --- isSettingsClamped tests ---

func TestIsSettingsClamped_Clamped(t *testing.T) {
	eff := &spiceboxv1alpha1.EffectiveSettings{
		Provenance: map[string]string{
			"budget.maxTurns": "clamped",
		},
	}
	assert.True(t, isSettingsClamped(eff))
}

func TestIsSettingsClamped_NotClamped(t *testing.T) {
	eff := &spiceboxv1alpha1.EffectiveSettings{
		Provenance: map[string]string{
			"budget.maxTurns": "class",
			"model":           "namespace",
		},
	}
	assert.False(t, isSettingsClamped(eff))
}

func TestIsSettingsClamped_NilEff(t *testing.T) {
	assert.False(t, isSettingsClamped(nil))
}

func TestIsSettingsClamped_EmptyProvenance(t *testing.T) {
	assert.False(t, isSettingsClamped(&spiceboxv1alpha1.EffectiveSettings{}))
}

// --- button encode/decode round-trip tests (Settings discriminator) ---

func TestSettingsButtonValue_RoundTrip(t *testing.T) {
	const sessRef = "mynamespace/mysession"
	btn := settingsButton(sessRef)
	bte, ok := btn.(*slackapi.ButtonBlockElement)
	require.True(t, ok, "settingsButton must return a *ButtonBlockElement")
	assert.Equal(t, showSettingsActionID, bte.ActionID)

	// Value must round-trip via the shared decode.
	got, ok := decodeApprovalButtonValue(bte.Value)
	require.True(t, ok, "decodeApprovalButtonValue must accept discShowSettings")
	assert.Equal(t, discShowSettings, got.V)
	assert.Equal(t, sessRef, got.S)
}

func TestShowSettingsButtonValue_IsUnder2000Chars(t *testing.T) {
	// Slack button values have a 2000-char hard cap.
	val := encodeApprovalButtonValue(discShowSettings, "", "", "very-long-namespace/very-long-session-name-that-should-still-fit")
	assert.LessOrEqual(t, len(val), 2000)
}

// --- buildUserMessageBlocks tests ---

// TestBuildUserMessageBlocks_SettingsButton_FirstMessageOnly: the button
// renders when includeSettings is true (session's first agent message)
// and is absent otherwise.
func TestBuildUserMessageBlocks_SettingsButton_FirstMessageOnly(t *testing.T) {
	s := &slackSender{
		statusCache: make(map[string]cachedStatus),
	} // no K8sClient: clamp line skipped
	sess := sessionInfoForTest("default", "my-session")

	first := blocksJSON(t, s.buildUserMessageBlocks(noopCtx(t), sess, "hello", nil, "default/my-session", true, noopLogger{}))
	assert.Contains(t, first, showSettingsActionID, "first message carries the button")

	later := blocksJSON(t, s.buildUserMessageBlocks(noopCtx(t), sess, "again", nil, "default/my-session", false, noopLogger{}))
	assert.NotContains(t, later, showSettingsActionID, "subsequent messages must not carry the button")
}

// TestSettingsButtonSeen_OncePerSession: the shared seen-set hands out
// "include" exactly once per session ref, process-wide.
func TestSettingsButtonSeen_OncePerSession(t *testing.T) {
	k := &Kind{}
	seen := k.sharedSettingsButtonSeen()
	assert.False(t, seen.saw("ns/a"), "unseen session ⇒ first message")
	seen.mark("ns/a")
	assert.True(t, seen.saw("ns/a"), "marked session ⇒ no button")
	assert.False(t, seen.saw("ns/b"), "other sessions unaffected")
	assert.Same(t, seen, k.sharedSettingsButtonSeen(), "shared instance is process-wide per Kind")
}

// --- Send()-level wiring tests ---
//
// These exercise the first-message-only "Show settings" ordering through
// the real Send() path, not just buildUserMessageBlocks in isolation.
//
// Observation strategy: the Slack SDK's MsgOptionBlocks content is not
// recoverable from a captured MsgOption, so the button cannot be read back off
// the posted message. But Send() gates BOTH the button and the seen-set
// mark() on the SAME includeSettings value:
//
//	includeSettings := s.settingsSeen == nil || !s.settingsSeen.saw(sessRef)
//	blocks := s.buildUserMessageBlocks(..., includeSettings, ...)
//	...post...
//	if includeSettings && usedBlocks && s.settingsSeen != nil { s.settingsSeen.mark(sessRef) }
//
// So `seen.saw(sessRef)` flipping to true after a Send is equivalent to
// "includeSettings was true (button built into the blocks) AND the block
// post succeeded" — making the seen-set a faithful proxy for "the button
// was delivered on this send." That closes the loop with
// TestBuildUserMessageBlocks_SettingsButton_FirstMessageOnly, which
// proves includeSettings true⇒button / false⇒no-button.

// TestSend_SettingsButton_MarksOnlyAfterSuccessfulBlockPost verifies the
// once-guard is consumed only when the button was actually delivered:
// a successful first block post marks the session; a hard post failure
// and the invalid_blocks → plain-text degraded retry both leave it
// unmarked so the next real message still offers the button.
func TestSend_SettingsButton_MarksOnlyAfterSuccessfulBlockPost(t *testing.T) {
	invalidBlocks := slackapi.SlackErrorResponse{Err: "invalid_blocks"}
	cases := []struct {
		name       string
		postErrs   []error // FIFO per PostMessageContext call
		wantErr    bool
		wantMarked bool
		wantPosts  int
	}{
		{
			name:       "first successful block post: button delivered ⇒ session marked",
			postErrs:   nil,
			wantErr:    false,
			wantMarked: true,
			wantPosts:  1,
		},
		{
			name:       "hard post failure: not marked ⇒ button retried next message",
			postErrs:   []error{errors.New("network down")},
			wantErr:    true,
			wantMarked: false,
			wantPosts:  1,
		},
		{
			name:       "invalid_blocks degraded plain-text retry: delivered but not marked (button never carried)",
			postErrs:   []error{invalidBlocks, nil},
			wantErr:    false,
			wantMarked: false,
			wantPosts:  2,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := &fakeSlackClient{postMessageErrs: tc.postErrs}
			s := newSender(c)
			seen := newToolSessionSeen()
			s.settingsSeen = seen
			sess := sessionInfoForTest("default", "sess-1")

			_, err := s.Send(context.Background(), sess, userMessageEnvelope(t, "hello"))
			if tc.wantErr {
				require.Error(t, err, "Send should surface the post failure")
			} else {
				require.NoError(t, err, "Send")
			}
			assert.Equal(t, tc.wantMarked, seen.saw("default/sess-1"),
				"marked iff the button was delivered on a successful block post")
			assert.Len(t, c.postMessageCalls, tc.wantPosts, "postMessage call count")
		})
	}
}

// TestSend_SettingsButton_OmittedOnSubsequentMessages verifies the button
// is a first-message-only affordance: the first send marks the session,
// and a second send on the same session computes includeSettings=false
// (so buildUserMessageBlocks omits the button — see
// TestBuildUserMessageBlocks_SettingsButton_FirstMessageOnly) while still
// delivering the reply and leaving the mark idempotent.
func TestSend_SettingsButton_OmittedOnSubsequentMessages(t *testing.T) {
	c := &fakeSlackClient{}
	s := newSender(c)
	seen := newToolSessionSeen()
	s.settingsSeen = seen
	sess := sessionInfoForTest("default", "sess-2")

	_, err := s.Send(context.Background(), sess, userMessageEnvelope(t, "first"))
	require.NoError(t, err, "first Send")
	require.True(t, seen.saw("default/sess-2"), "first message marks the session (button delivered)")

	_, err = s.Send(context.Background(), sess, userMessageEnvelope(t, "second"))
	require.NoError(t, err, "second Send")
	assert.True(t, seen.saw("default/sess-2"), "session stays marked; the mark is idempotent")
	assert.Len(t, c.postMessageCalls, 2, "both messages delivered")
}

// --- clamp context line tests ---

func TestBuildUserMessageBlocks_ClampContextBlock_Present(t *testing.T) {
	// Session with a clamped budget → clamp warning line expected.
	eff := &spiceboxv1alpha1.EffectiveSettings{
		Provenance: map[string]string{"budget.maxTurns": "clamped"},
	}
	as := buildAgentSession("default", "my-session", eff)
	sch := settingsTestScheme(t)
	cli := fake.NewClientBuilder().WithScheme(sch).WithObjects(&as).Build()

	s := &slackSender{
		deps:         channelkinds.Deps{K8sClient: cli},
		planMessages: newPlanMessageMap(),
		statusCache:  make(map[string]cachedStatus),
	}
	sess := sessionInfoForTest("default", "my-session")
	blocks := s.buildUserMessageBlocks(noopCtx(t), sess, "Hello!", nil, "default/my-session", true, noopLogger{})

	txt := blocksText(blocks)
	assert.Contains(t, txt, "Budget capped by policy",
		"clamp context block must be present when budget is clamped")
	assert.True(t, hasActionBlockWithID(blocks, showSettingsActionID),
		"Show settings button must still be present alongside the clamp line")
}

func TestBuildUserMessageBlocks_ClampContextBlock_Absent(t *testing.T) {
	// Session with no clamp → no clamp warning line.
	eff := &spiceboxv1alpha1.EffectiveSettings{
		Provenance: map[string]string{"budget.maxTurns": "class"},
	}
	as := buildAgentSession("default", "my-session", eff)
	sch := settingsTestScheme(t)
	cli := fake.NewClientBuilder().WithScheme(sch).WithObjects(&as).Build()

	s := &slackSender{
		deps:         channelkinds.Deps{K8sClient: cli},
		planMessages: newPlanMessageMap(),
		statusCache:  make(map[string]cachedStatus),
	}
	sess := sessionInfoForTest("default", "my-session")
	blocks := s.buildUserMessageBlocks(noopCtx(t), sess, "Hello!", nil, "default/my-session", true, noopLogger{})

	txt := blocksText(blocks)
	assert.NotContains(t, txt, "Budget capped by policy",
		"clamp context block must be absent when budget is not clamped")
}

func TestBuildUserMessageBlocks_ClampLine_BestEffort_OnGetError(t *testing.T) {
	// K8sClient present but session doesn't exist — should not fail, clamp line skipped.
	sch := settingsTestScheme(t)
	cli := fake.NewClientBuilder().WithScheme(sch).Build() // no objects
	s := &slackSender{
		deps:         channelkinds.Deps{K8sClient: cli},
		planMessages: newPlanMessageMap(),
		statusCache:  make(map[string]cachedStatus),
	}
	sess := sessionInfoForTest("default", "nonexistent-session")
	// Must not panic or return an error.
	blocks := s.buildUserMessageBlocks(noopCtx(t), sess, "Hello!", nil, "default/nonexistent-session", true, noopLogger{})

	// Button still present; no clamp line.
	assert.True(t, hasActionBlockWithID(blocks, showSettingsActionID))
	txt := blocksText(blocks)
	assert.NotContains(t, txt, "Budget capped by policy")
}

// --- TestSettingsButtonJSON: structural sanity ---

func TestSettingsButtonJSON_Roundtrip(t *testing.T) {
	raw := encodeApprovalButtonValue(discShowSettings, "", "", "ns/name")
	var v approvalButtonValue
	require.NoError(t, json.Unmarshal([]byte(raw), &v))
	assert.Equal(t, discShowSettings, v.V)
	assert.Equal(t, "ns/name", v.S)
	assert.Empty(t, v.R)
	assert.Empty(t, v.D)
}
