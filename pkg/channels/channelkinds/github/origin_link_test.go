package github

import (
	"testing"

	"github.com/stretchr/testify/assert"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

func TestKind_OriginLink(t *testing.T) {
	k := Kind{}
	cases := []struct {
		name    string
		binding *spiceboxv1alpha1.ChannelBinding
		wantURL string
		wantOK  bool
	}{
		{
			name: "persisted html_url wins (GHES host-correct)",
			binding: &spiceboxv1alpha1.ChannelBinding{Kind: "github", Key: "pr:demo-org/platform#42",
				External: map[string]string{"origin_url": "https://ghe.example.com/demo-org/platform/pull/42"}},
			wantURL: "https://ghe.example.com/demo-org/platform/pull/42", wantOK: true,
		},
		{
			name:    "no persisted url: reconstruct github.com from pr key",
			binding: &spiceboxv1alpha1.ChannelBinding{Kind: "github", Key: "pr:demo-org/platform#42"},
			wantURL: "https://github.com/demo-org/platform/pull/42", wantOK: true,
		},
		{
			name:    "unparseable key, no url: ok=false",
			binding: &spiceboxv1alpha1.ChannelBinding{Kind: "github", Key: "garbage"},
			wantOK:  false,
		},
		{name: "nil binding: ok=false", binding: nil, wantOK: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := k.OriginLink(tc.binding)
			assert.Equal(t, tc.wantOK, ok)
			assert.Equal(t, tc.wantURL, got)
		})
	}
}
