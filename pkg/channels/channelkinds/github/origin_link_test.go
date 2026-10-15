package github

import (
	"testing"

	"github.com/stretchr/testify/assert"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

func TestKind_OriginLink(t *testing.T) {
	k := Kind{}
	cases := []struct {
		name     string
		binding  *spiceboxv1alpha1.ChannelBinding
		wantLink channelkinds.OriginLink
		wantOK   bool
	}{
		{
			name: "persisted html_url wins (GHES host-correct)",
			binding: &spiceboxv1alpha1.ChannelBinding{Kind: "github", Key: "pr:demo-org/platform#42",
				External: map[string]string{"origin_url": "https://ghe.example.com/demo-org/platform/pull/42"}},
			wantLink: channelkinds.OriginLink{URL: "https://ghe.example.com/demo-org/platform/pull/42",
				Label: "View the pull request", Icon: "github"},
			wantOK: true,
		},
		{
			// Fail-closed: there is no github.com fallback. On GHES a
			// reconstructed github.com URL would leak internal org/repo names
			// and could point at a squatted repo; the binding carries no host,
			// so a missing link is the only safe answer.
			name:    "no persisted url: ok=false (no github.com fallback)",
			binding: &spiceboxv1alpha1.ChannelBinding{Kind: "github", Key: "pr:demo-org/platform#42"},
			wantOK:  false,
		},
		{
			name: "non-https origin_url: ok=false",
			binding: &spiceboxv1alpha1.ChannelBinding{Kind: "github",
				External: map[string]string{"origin_url": "javascript:alert(1)"}},
			wantOK: false,
		},
		{
			name: "http (not https) origin_url: ok=false",
			binding: &spiceboxv1alpha1.ChannelBinding{Kind: "github",
				External: map[string]string{"origin_url": "http://ghe.example.com/demo-org/platform/pull/42"}},
			wantOK: false,
		},
		{name: "nil binding: ok=false", binding: nil, wantOK: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := k.OriginLink(tc.binding)
			assert.Equal(t, tc.wantOK, ok)
			assert.Equal(t, tc.wantLink, got)
		})
	}
}
