package pinnedmessage_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/authzed/openagentprimitives/pkg/agent/pinnedmessage"
	v1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

func TestDerive(t *testing.T) {
	cases := []struct {
		name       string
		hasOpening bool
		concluded  bool
		outcome    string
		body, link string
		want       *v1alpha1.PinnedMessageStatus
	}{
		{name: "no opening -> nil projection", hasOpening: false, want: nil},
		{
			name:       "running, no body -> in_progress",
			hasOpening: true,
			want:       &v1alpha1.PinnedMessageStatus{Badge: v1alpha1.OpeningBadgeInProgress},
		},
		{
			name:       "running with body -> in_progress + body, no link yet",
			hasOpening: true, body: "1 finding so far",
			want: &v1alpha1.PinnedMessageStatus{Badge: v1alpha1.OpeningBadgeInProgress, Body: "1 finding so far"},
		},
		{
			name:       "concluded problems_found -> outcome badge + body + link",
			hasOpening: true, concluded: true, outcome: "problems_found", body: "2 findings", link: "https://v/1",
			want: &v1alpha1.PinnedMessageStatus{Badge: v1alpha1.OpeningBadgeProblemsFound, Body: "2 findings", Link: "https://v/1"},
		},
		{
			name:       "concluded clean -> clean badge",
			hasOpening: true, concluded: true, outcome: "clean",
			want: &v1alpha1.PinnedMessageStatus{Badge: v1alpha1.OpeningBadgeClean},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, pinnedmessage.Derive(tc.hasOpening, tc.concluded, tc.outcome, tc.body, tc.link))
		})
	}
}
