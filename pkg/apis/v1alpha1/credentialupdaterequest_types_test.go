package v1alpha1

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestIsCredentialUpdateRequestTerminal(t *testing.T) {
	cases := []struct {
		name  string
		phase string
		want  bool
	}{
		{name: "Pending is not terminal: the meta tool must keep polling", phase: CredentialUpdateRequestPhasePending, want: false},
		{name: "Open is not terminal: a card is live and awaiting a human", phase: CredentialUpdateRequestPhaseOpen, want: false},
		{name: "Fulfilled is terminal: credential replaced, session resumes", phase: CredentialUpdateRequestPhaseFulfilled, want: true},
		{name: "Refused is terminal: determination blocked the ask", phase: CredentialUpdateRequestPhaseRefused, want: true},
		{name: "Expired is terminal: nobody clicked before the idle TTL", phase: CredentialUpdateRequestPhaseExpired, want: true},
		{name: "empty phase is not terminal: a freshly created CR has no phase yet", phase: "", want: false},
		{name: "unknown phase is not terminal: never strand the poller on a typo", phase: "Bogus", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, IsCredentialUpdateRequestTerminal(tc.phase))
		})
	}
}
