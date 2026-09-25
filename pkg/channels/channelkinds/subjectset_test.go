package channelkinds_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

// plainKind implements no optional describer — the common case, and the one
// that must degrade to a correct answer rather than a wrong one.
type plainKind struct{}

// describingKind supplies a label, and optionally a size.
type describingKind struct {
	label string
	size  int
	ok    bool
}

func (k describingKind) DescribeSubjectSet(ref string) (channelkinds.SubjectSetDescription, bool) {
	if !k.ok {
		return channelkinds.SubjectSetDescription{}, false
	}
	return channelkinds.SubjectSetDescription{Label: k.label, Size: k.size}, true
}

func TestDescribeApprovers(t *testing.T) {
	cases := []struct {
		name  string
		kind  any
		ref   string
		names []string
		size  int
		want  string
	}{
		{
			name:  "a single named user: renders the address",
			kind:  plainKind{},
			names: []string{"dev@example.com"},
			size:  1,
			want:  "dev@example.com",
		},
		{
			name:  "small enough to list: names them",
			kind:  plainKind{},
			names: []string{"a@example.com", "b@example.com", "c@example.com"},
			size:  3,
			want:  "a@example.com, b@example.com, c@example.com",
		},
		{
			name: "large, kind describes it fully: the channel's own words",
			kind: describingKind{label: "#eng", size: 23, ok: true},
			ref:  "slack_channel:C0421#member",
			size: 23,
			want: "anyone in #eng (23 people)",
		},
		{
			name: "large, label but no size: no fabricated count",
			kind: describingKind{label: "#eng", size: 0, ok: true},
			ref:  "slack_channel:C0421#member",
			want: "anyone in #eng",
		},
		{
			name: "large, size but no label: falls back to the permission",
			kind: plainKind{},
			ref:  "slack_channel:C0421#member",
			size: 23,
			want: "anyone with approve on this session (23 people)",
		},
		{
			// Not hypothetical: this is what Slack renders today, because the
			// app requests neither channels:read nor groups:read.
			name: "large, neither label nor size: the honest floor",
			kind: plainKind{},
			ref:  "slack_channel:C0421#member",
			want: "anyone with approve on this session",
		},
		{
			name: "kind implements the interface but declines this ref",
			kind: describingKind{ok: false},
			ref:  "slack_channel:C0421#member",
			size: 12,
			want: "anyone with approve on this session (12 people)",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := channelkinds.DescribeApprovers(channelkinds.ApproverPopulation{
				Kind:              tc.kind,
				Ref:               tc.ref,
				Names:             tc.names,
				Size:              tc.size,
				MaxNamedApprovers: 5,
			})
			assert.Equal(t, tc.want, got)
		})
	}
}

// Past the threshold the population is described, not enumerated: a 200-member
// channel listed on a card is worse than useless, and the enumeration itself
// costs a lookup.
func TestDescribeApprovers_stopsNamingPastTheThreshold(t *testing.T) {
	names := []string{"a@example.com", "b@example.com", "c@example.com", "d@example.com", "e@example.com", "f@example.com"}

	got := channelkinds.DescribeApprovers(channelkinds.ApproverPopulation{
		Kind: describingKind{label: "#eng", size: 6, ok: true},
		Ref:  "slack_channel:C0421#member", Names: names, Size: 6, MaxNamedApprovers: 5,
	})

	assert.Equal(t, "anyone in #eng (6 people)", got)
	for _, n := range names {
		assert.NotContains(t, got, n)
	}
}

// A zero threshold must not accidentally mean "name everyone".
func TestDescribeApprovers_zeroThresholdNamesNobody(t *testing.T) {
	got := channelkinds.DescribeApprovers(channelkinds.ApproverPopulation{
		Kind: plainKind{}, Names: []string{"a@example.com"}, Size: 1, MaxNamedApprovers: 0,
	})
	assert.NotContains(t, got, "a@example.com")
}

// The describer is display-only and must never be the reason an approval cannot
// be rendered. With nothing at all to say, it still returns the floor.
func TestDescribeApprovers_emptyPopulationStillRendersSomething(t *testing.T) {
	got := channelkinds.DescribeApprovers(channelkinds.ApproverPopulation{})
	assert.NotEmpty(t, got)
}
