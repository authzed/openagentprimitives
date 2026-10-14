package main

import (
	"testing"

	"github.com/stretchr/testify/assert"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/web/webui/artifactview"
)

func ghBinding(url string) *spiceboxv1alpha1.ChannelBinding {
	b := &spiceboxv1alpha1.ChannelBinding{Name: "demo-reviewbot-gh", Kind: "github", Key: "pr:demo-org/platform#42"}
	if url != "" {
		b.External = map[string]string{"origin_url": url}
	}
	return b
}

func slackBinding(permalink string) *spiceboxv1alpha1.ChannelBinding {
	b := &spiceboxv1alpha1.ChannelBinding{Name: "demo-reviewbot-slack", Kind: "slack",
		External: map[string]string{"channel_id": "C1", "thread_ts": "1700.1"}}
	if permalink != "" {
		b.External["permalink"] = permalink
	}
	return b
}

// TestOriginLinksFrom exercises the walk over input+output bindings, the
// registry dispatch to each kind's OriginLinkProvider, and de-duplication —
// the registry is populated by webd's own blank imports (github, slack).
func TestOriginLinksFrom(t *testing.T) {
	const prURL = "https://github.com/demo-org/platform/pull/42"
	const slackURL = "https://demo.slack.com/archives/C1/p17001"

	cases := []struct {
		name string
		in   *spiceboxv1alpha1.ChannelBinding
		out  *spiceboxv1alpha1.ChannelBinding
		want []artifactview.OriginLink
	}{
		{
			name: "reviewbot: github input + slack output, input first",
			in:   ghBinding(prURL),
			out:  slackBinding(slackURL),
			want: []artifactview.OriginLink{
				{Href: prURL, ChannelKind: "github"},
				{Href: slackURL, ChannelKind: "slack"},
			},
		},
		{
			name: "interactive slack: input only, no output",
			in:   slackBinding(slackURL),
			out:  nil,
			want: []artifactview.OriginLink{{Href: slackURL, ChannelKind: "slack"}},
		},
		{
			name: "input==output same thread: de-duped to one link",
			in:   slackBinding(slackURL),
			out:  slackBinding(slackURL),
			want: []artifactview.OriginLink{{Href: slackURL, ChannelKind: "slack"}},
		},
		{
			name: "kubectl-driven: no bindings, empty",
			in:   nil,
			out:  nil,
			want: nil,
		},
		{
			name: "input kind without an OriginLinkProvider (browser): contributes nothing",
			in:   &spiceboxv1alpha1.ChannelBinding{Name: "b", Kind: "browser"},
			out:  nil,
			want: nil,
		},
		{
			name: "github reconstructs from key when origin_url absent",
			in:   ghBinding(""),
			out:  nil,
			want: []artifactview.OriginLink{{Href: prURL, ChannelKind: "github"}},
		},
		{
			name: "slack output without permalink: omitted",
			in:   ghBinding(prURL),
			out:  slackBinding(""),
			want: []artifactview.OriginLink{{Href: prURL, ChannelKind: "github"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sess := &spiceboxv1alpha1.AgentSession{}
			sess.Spec.InputChannel = tc.in
			sess.Spec.OutputChannel = tc.out
			assert.Equal(t, tc.want, originLinksFrom(sess))
		})
	}
}
