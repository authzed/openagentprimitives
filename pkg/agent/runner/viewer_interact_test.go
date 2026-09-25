package runner

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// TestLoopCurrentSpeakerCanInteract covers every answer the check can give,
// and — for the two that reach SpiceDB — the ARGUMENTS the checker was handed.
// Asserting only the returned bool would pass just as well for a call that
// asked about the wrong person (the session's initiator instead of the current
// speaker) or asked with fullyConsistent=false, which would let a participant
// who joined seconds ago be refused a link they can in fact open.
func TestLoopCurrentSpeakerCanInteract(t *testing.T) {
	const (
		ns   = "demo-ns"
		name = "demo-session"
	)
	boom := errors.New("spicedb unavailable")

	cases := []struct {
		name string
		// checker nil means a TRUE nil InteractChecker is passed (see the
		// interface-typed local below) — never a typed-nil *fakeInteract,
		// which would satisfy != nil and panic instead of exercising the gate.
		checker    *fakeInteract
		speaker    identity.Subject
		wantOK     bool
		wantErr    bool
		wantCalled bool
	}{
		{
			name:       "an allowed speaker: (true, nil), checked fully consistently",
			checker:    &fakeInteract{allow: true},
			speaker:    identity.Subject("user:demo-user"),
			wantOK:     true,
			wantCalled: true,
		},
		{
			name:       "a denied speaker: (false, nil), and the denial is not an error",
			checker:    &fakeInteract{allow: false},
			speaker:    identity.Subject("user:demo-user"),
			wantCalled: true,
		},
		{
			name:       "a checker error: (false, err) with a cause, not a silent denial",
			checker:    &fakeInteract{err: boom},
			speaker:    identity.Subject("user:demo-user"),
			wantErr:    true,
			wantCalled: true,
		},
		{
			// The bool must not survive the error: an implementation that
			// returned the checker's own bool alongside the error would open
			// the gate on an answer the checker never actually made.
			name:       "a checker answering (true, err): still (false, err)",
			checker:    &fakeInteract{allow: true, err: boom},
			speaker:    identity.Subject("user:demo-user"),
			wantErr:    true,
			wantCalled: true,
		},
		{
			// A session woken by a schedule rather than by a message has
			// nobody to address an offer to.
			name:    "no speaker recorded yet: (false, err) without asking SpiceDB",
			checker: &fakeInteract{allow: true},
			speaker: "",
			wantErr: true,
		},
		{
			name:    "a non-user speaker subject: (false, err) without asking SpiceDB",
			checker: &fakeInteract{allow: true},
			speaker: identity.Subject("serviceaccount:demo-bot"),
			wantErr: true,
		},
		{
			// "user:" with nothing after it clears both earlier guards — not
			// Empty(), object type "user" — and would otherwise be asked about
			// SpiceDB with an empty subject id.
			name:    "a user subject with no id: (false, err) without asking SpiceDB",
			checker: &fakeInteract{allow: true},
			speaker: identity.Subject("user:"),
			wantErr: true,
		},
		{
			name:    "no checker wired: (false, err), fail-closed rather than assumed-allowed",
			checker: nil,
			speaker: identity.Subject("user:demo-user"),
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var ia InteractChecker // true nil interface unless a fake is assigned
			if tc.checker != nil {
				ia = tc.checker
			}
			l := &Loop{lastInboundAuthor: tc.speaker}

			ok, err := l.CurrentSpeakerCanInteract(context.Background(), ia, ns, name)

			// assert, not require: the verdict, the error and the
			// checker-was-called facts are independent, and a regression that
			// breaks two of them should report two failures rather than stop
			// at the first.
			assert.Equal(t, tc.wantOK, ok, "verdict")
			if tc.wantErr {
				assert.Error(t, err, "an indeterminate or unanswerable check must carry a cause")
			} else {
				assert.NoError(t, err, "a decided check must not carry an error")
			}
			if tc.checker != nil {
				assert.Equal(t, tc.wantCalled, tc.checker.calls > 0, "whether SpiceDB was consulted")
			}
		})
	}
}

// TestLoopCurrentSpeakerCanInteractAsksAboutTheSpeaker pins the three
// arguments that decide WHO and HOW MUCH FRESHNESS the check asked about.
// They are asserted here rather than as extra table columns because they are
// a claim about a single successful call, and because every one of them would
// stay green under the table's verdict assertions alone.
func TestLoopCurrentSpeakerCanInteractAsksAboutTheSpeaker(t *testing.T) {
	ia := &fakeInteract{allow: true}
	l := &Loop{lastInboundAuthor: identity.Subject("user:demo-user")}

	ok, err := l.CurrentSpeakerCanInteract(context.Background(), ia, "demo-ns", "demo-session")
	require.NoError(t, err, "the check must succeed for this fixture")
	require.True(t, ok, "the fixture's checker allows")

	assert.Equal(t, "demo-ns", ia.gotNS, "the check must name this session's namespace")
	assert.Equal(t, "demo-session", ia.gotName, "the check must name this session")
	assert.Equal(t, identity.CanonicalFromTrusted("demo-user", "test fixture"), ia.gotSubject,
		"the check must ask about the current speaker, not the session's initiator")
	assert.True(t, ia.gotFully,
		"the check must be fully consistent so a participant who joined seconds ago is reflected")
}
