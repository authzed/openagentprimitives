//go:build darwin && arm64

package desktopcmd

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/browser"
)

// chatSession builds a chat-shaped AgentSession fixture: browser channel-kind
// label (what listChatSessions filters on), the given class/phase, a prompt,
// and a creation time offset (older = more negative) so ordering is testable.
func chatSession(name, class, phase, prompt string, ageOffset time.Duration) spiceboxv1alpha1.AgentSession {
	return spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         chatSessionNamespace,
			CreationTimestamp: metav1.NewTime(time.Now().Add(ageOffset)),
			Labels:            map[string]string{spiceboxv1alpha1.LabelChannelKind: browser.KindName},
		},
		Spec: spiceboxv1alpha1.AgentSessionSpec{
			Class:  class,
			Prompt: spiceboxv1alpha1.PromptSource{Inline: prompt},
		},
		Status: spiceboxv1alpha1.AgentSessionStatus{Phase: phase},
	}
}

func TestIsTerminalChatSessionPhase(t *testing.T) {
	cases := []struct {
		phase    string
		terminal bool
	}{
		{spiceboxv1alpha1.AgentSessionPhaseSucceeded, true},
		{spiceboxv1alpha1.AgentSessionPhaseFailed, true},
		{spiceboxv1alpha1.AgentSessionPhaseRunning, false},
		{spiceboxv1alpha1.AgentSessionPhasePending, false},
		{spiceboxv1alpha1.AgentSessionPhaseAwaitingDecision, false},
		{spiceboxv1alpha1.AgentSessionPhaseAwaitingCredentials, false},
		{"", false},
	}
	for _, tc := range cases {
		t.Run(tc.phase+" terminal="+boolStr(tc.terminal), func(t *testing.T) {
			assert.Equal(t, tc.terminal, isTerminalChatSessionPhase(tc.phase))
		})
	}
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func TestSelectChatSessions(t *testing.T) {
	t.Run("drops terminal, sorts newest-first, tiebreaks by name", func(t *testing.T) {
		in := []spiceboxv1alpha1.AgentSession{
			chatSession("pirate-private-aaaa1111", "pirate-private", spiceboxv1alpha1.AgentSessionPhaseRunning, "old one", -3*time.Minute),
			chatSession("pirate-private-cccc3333", "pirate-private", spiceboxv1alpha1.AgentSessionPhaseSucceeded, "finished", -1*time.Minute),
			chatSession("pirate-private-bbbb2222", "pirate-private", spiceboxv1alpha1.AgentSessionPhaseAwaitingDecision, "newest", -30*time.Second),
			chatSession("pirate-private-dddd4444", "pirate-private", spiceboxv1alpha1.AgentSessionPhaseFailed, "failed", -10*time.Second),
		}
		got := selectChatSessions(in, maxSessionMenuItems)
		require.Len(t, got, 2, "the two terminal sessions must be dropped")
		assert.Equal(t, "pirate-private-bbbb2222", got[0].Name, "newest live session first")
		assert.Equal(t, "pirate-private-aaaa1111", got[1].Name)
	})

	t.Run("caps to limit", func(t *testing.T) {
		in := make([]spiceboxv1alpha1.AgentSession, 0, 5)
		for i := 0; i < 5; i++ {
			in = append(in, chatSession(
				"pirate-private-"+string(rune('a'+i)), "pirate-private",
				spiceboxv1alpha1.AgentSessionPhaseRunning, "p", time.Duration(-i)*time.Minute))
		}
		got := selectChatSessions(in, 3)
		assert.Len(t, got, 3)
	})

	t.Run("equal creation times tiebreak by name ascending", func(t *testing.T) {
		at := -time.Minute
		in := []spiceboxv1alpha1.AgentSession{
			chatSession("pirate-private-zzzz", "pirate-private", spiceboxv1alpha1.AgentSessionPhaseRunning, "z", at),
			chatSession("pirate-private-aaaa", "pirate-private", spiceboxv1alpha1.AgentSessionPhaseRunning, "a", at),
		}
		// Force identical timestamps.
		in[1].CreationTimestamp = in[0].CreationTimestamp
		got := selectChatSessions(in, maxSessionMenuItems)
		require.Len(t, got, 2)
		assert.Equal(t, "pirate-private-aaaa", got[0].Name)
	})
}

func TestChatSessionLabel(t *testing.T) {
	t.Run("with inline prompt -> class: prompt", func(t *testing.T) {
		s := chatSession("pirate-private-abcd1234", "pirate-private", spiceboxv1alpha1.AgentSessionPhaseRunning, "translate hello to pirate", 0)
		assert.Equal(t, "pirate-private: translate hello to pirate", chatSessionLabel(s))
	})

	t.Run("multi-line prompt collapses to one line", func(t *testing.T) {
		s := chatSession("pirate-private-abcd1234", "pirate-private", spiceboxv1alpha1.AgentSessionPhaseRunning, "line one\n\nline two", 0)
		assert.Equal(t, "pirate-private: line one line two", chatSessionLabel(s))
	})

	t.Run("long prompt is truncated with an ellipsis", func(t *testing.T) {
		long := "this is a very long first message that should be cut off well before its natural end"
		s := chatSession("pirate-private-abcd1234", "pirate-private", spiceboxv1alpha1.AgentSessionPhaseRunning, long, 0)
		label := chatSessionLabel(s)
		assert.Contains(t, label, "…")
		assert.LessOrEqual(t, len([]rune(label)), len("pirate-private: ")+48+1)
	})

	t.Run("no prompt -> full session name", func(t *testing.T) {
		s := chatSession("pirate-private-abcd1234", "pirate-private", spiceboxv1alpha1.AgentSessionPhaseRunning, "", 0)
		assert.Equal(t, "pirate-private-abcd1234", chatSessionLabel(s))
	})

	t.Run("empty class, no prompt -> session name", func(t *testing.T) {
		s := chatSession("orphan-session", "", spiceboxv1alpha1.AgentSessionPhaseRunning, "", 0)
		assert.Equal(t, "orphan-session", chatSessionLabel(s))
	})
}

func TestTruncateLabel(t *testing.T) {
	assert.Equal(t, "abc", truncateLabel("abc", 10))
	assert.Equal(t, "abc", truncateLabel("abc", 3))
	assert.Equal(t, "ab…", truncateLabel("abcd", 2))
	assert.Equal(t, "abcd", truncateLabel("abcd", 0), "non-positive max returns input unchanged")
	// Rune-aware: cutting a multibyte string never splits a character.
	assert.Equal(t, "héllo…", truncateLabel("héllo world", 6))
}

func newChatSessionScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, spiceboxv1alpha1.AddToScheme(s))
	return s
}

func TestListChatSessions(t *testing.T) {
	ctx := context.Background()

	// A browser-labelled live session (kept), a browser-labelled terminal
	// session (filtered by phase), and a non-browser-labelled session
	// (filtered by the label selector) — only the first should come back.
	live := chatSession("pirate-private-live0001", "pirate-private", spiceboxv1alpha1.AgentSessionPhaseRunning, "hi", -time.Minute)
	done := chatSession("pirate-private-done0002", "pirate-private", spiceboxv1alpha1.AgentSessionPhaseSucceeded, "bye", -2*time.Minute)
	nonBrowser := chatSession("slackish-0003", "some-class", spiceboxv1alpha1.AgentSessionPhaseRunning, "x", -30*time.Second)
	nonBrowser.Labels = map[string]string{spiceboxv1alpha1.LabelChannelKind: "slack"}

	c := fake.NewClientBuilder().
		WithScheme(newChatSessionScheme(t)).
		WithObjects(&live, &done, &nonBrowser).
		Build()

	got, err := listChatSessions(ctx, c, maxSessionMenuItems)
	require.NoError(t, err)
	require.Len(t, got, 1, "only the live browser-labelled session should be returned")
	assert.Equal(t, "pirate-private-live0001", got[0].Name)
}
