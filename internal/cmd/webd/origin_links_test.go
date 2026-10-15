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
	ghLink := func() artifactview.OriginLink {
		return artifactview.OriginLink{Href: prURL, Label: "View the pull request", Icon: "github"}
	}
	slackLink := func() artifactview.OriginLink {
		return artifactview.OriginLink{Href: slackURL, Label: "Open the Slack thread", Icon: "slack"}
	}

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
			want: []artifactview.OriginLink{ghLink(), slackLink()},
		},
		{
			name: "interactive slack: input only, no output",
			in:   slackBinding(slackURL),
			out:  nil,
			want: []artifactview.OriginLink{slackLink()},
		},
		{
			name: "input==output same thread: de-duped to one link",
			in:   slackBinding(slackURL),
			out:  slackBinding(slackURL),
			want: []artifactview.OriginLink{slackLink()},
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
			// Fail-closed: no github.com fallback (GHES host is unknowable from
			// the binding). A session with no persisted origin_url contributes
			// no link rather than a possibly-wrong github.com one.
			name: "github without origin_url: omitted (no github.com fallback)",
			in:   ghBinding(""),
			out:  nil,
			want: nil,
		},
		{
			name: "slack output without permalink and no team_id: omitted",
			in:   ghBinding(prURL),
			out:  slackBinding(""),
			want: []artifactview.OriginLink{ghLink()},
		},
		{
			// External is user-writable; a non-https href (even if a kind let it
			// through) is dropped by the generic backstop in originLinksFrom.
			name: "non-https origin href: dropped by the scheme backstop",
			in:   ghBinding("javascript:alert(1)"),
			out:  nil,
			want: nil,
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

func TestIsHTTPSOriginHref(t *testing.T) {
	cases := []struct {
		name string
		href string
		want bool
	}{
		{"https with host", "https://github.com/x/y/pull/1", true},
		{"https slack", "https://demo.slack.com/archives/C1/p1", true},
		{"http not https", "http://github.com/x", false},
		{"javascript scheme", "javascript:alert(1)", false},
		{"data scheme", "data:text/html,<script>1</script>", false},
		{"relative ref", "/x/y", false},
		{"empty", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, isHTTPSOriginHref(tc.href))
		})
	}
}
