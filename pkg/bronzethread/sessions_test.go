package bronzethread_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	bt "github.com/authzed/openagentprimitives/pkg/bronzethread"
)

// multiSessionBundle is the minimum runnable bundle with two named threads, a
// step scoped to one of them, admission on and one ordering claim.
func multiSessionBundle() bt.Bundle {
	b := runnableBundle()
	b.UserTurns = []bt.UserTurn{
		{Text: "review pull request 1", Thread: "a1"},
		{Text: "summarize the notes", Thread: "b", Channel: "notes-fake"},
		{Text: "wrap up", Thread: "a1", WaitFor: []bt.TurnBarrier{{Thread: "a1", Until: bt.UntilAwaitingInput}}},
	}
	b.LLM = []bt.LLMStep{{Thread: "a1", Reply: []bt.ReplyPart{{Text: "none"}}}}
	b.Admission = &bt.Admission{CapacitySessions: 1}
	b.Assert.Admission = &bt.AdmissionAssertions{
		AdmittedBefore: []bt.ThreadOrder{{First: "a1", Then: "b"}},
	}
	return b
}

// TestBundle_ValidateSessions covers the preconditions a multi-session bundle
// adds: every turn routed, every scope and barrier naming a real thread, and
// admission claims only where admission is on.
func TestBundle_ValidateSessions(t *testing.T) {
	cases := []struct {
		name    string
		doctor  func(b bt.Bundle) bt.Bundle
		wantErr string // empty means the bundle must validate
	}{
		{
			name:   "a multi-session admission bundle validates",
			doctor: func(b bt.Bundle) bt.Bundle { return b },
		},
		{
			name:    "one turn names no thread while others do: refused",
			doctor:  func(b bt.Bundle) bt.Bundle { b.UserTurns[1].Thread = ""; return b },
			wantErr: "every turn must",
		},
		{
			name: "a channel with no thread: refused",
			doctor: func(b bt.Bundle) bt.Bundle {
				b.UserTurns = []bt.UserTurn{{Text: "hi", Channel: "notes-fake"}}
				b.LLM[0].Thread = ""
				b.Assert.Admission = nil
				return b
			},
			wantErr: "names a channel but no thread",
		},
		{
			name: "a barrier naming an unknown thread: refused",
			doctor: func(b bt.Bundle) bt.Bundle {
				b.UserTurns[2].WaitFor[0].Thread = "zz"
				return b
			},
			wantErr: "which no user turn opens",
		},
		{
			name: "a barrier with an unknown state: refused",
			doctor: func(b bt.Bundle) bt.Bundle {
				b.UserTurns[2].WaitFor[0].Until = "sleeping"
				return b
			},
			wantErr: "until is \"sleeping\"",
		},
		{
			name:    "a step scoped to an unknown thread: refused",
			doctor:  func(b bt.Bundle) bt.Bundle { b.LLM[0].Thread = "zz"; return b },
			wantErr: "llm[0] is scoped to thread \"zz\"",
		},
		{
			name: "one thread's opening text contains another's: refused",
			doctor: func(b bt.Bundle) bt.Bundle {
				b.UserTurns[1].Text = "please review pull request 1 too"
				return b
			},
			wantErr: "cannot be told apart",
		},
		{
			name:    "admission with no capacity: refused",
			doctor:  func(b bt.Bundle) bt.Bundle { b.Admission.CapacitySessions = 0; return b },
			wantErr: "capacitySessions must be at least 1",
		},
		{
			name:    "admission claims with admission off: refused",
			doctor:  func(b bt.Bundle) bt.Bundle { b.Admission = nil; return b },
			wantErr: "turns admission on nowhere",
		},
		{
			name: "an admission claim naming an unknown thread: refused",
			doctor: func(b bt.Bundle) bt.Bundle {
				b.Assert.Admission.CompletedBefore = []bt.ThreadOrder{{First: "a1", Then: "zz"}}
				return b
			},
			wantErr: "each must be a thread a user turn opens",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.doctor(multiSessionBundle()).Validate()
			if tc.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err, "this bundle must not be runnable")
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

// Threads lists named threads once each, in first-appearance order, and
// OpeningText is the thread's FIRST turn — the message every request of that
// session starts from.
func TestBundle_ThreadsAndOpeningText(t *testing.T) {
	b := multiSessionBundle()
	assert.Equal(t, []string{"a1", "b"}, b.Threads())
	assert.Equal(t, "review pull request 1", b.OpeningText("a1"), "the first turn, not the follow-up")
	assert.Empty(t, b.OpeningText("zz"))
	assert.Empty(t, runnableBundle().Threads(), "a single-session bundle names no threads")
}
