package toolspec

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// messagingbotGhAPICEL is a hand-maintained PARALLEL COPY of the `gh api`
// constraint on the messagingbot-gh-read SpiceboxToolspec shipped at
// examples/messagingbot/manifests/toolspec.yaml, pinned here as a Go constant
// for the same reason as ghReviewEndpointCEL: no test may read examples/. The
// copies are kept in sync BY HAND; this pins the expression's semantics, not
// that the shipped bundle still matches it.
//
// Unlike reviewbot's endpoint list, any repos/OWNER/NAME/... path is allowed:
// GET-only reads of an allowlisted repository are exactly the job. What the
// regex must still guarantee is that the endpoint NAMES a repository, because
// an endpoint that names none (search/code, orgs/X/repos, user) resolves no
// resource id and would slip past the repository allowlist as an ungated op.
const messagingbotGhAPICEL = `call.subcommand != 'api' || (!call.hasFlag('method') && !call.hasFlag('field') && !call.hasFlag('raw-field') && !call.hasFlag('input') && !call.hasFlag('hostname') && !call.positional['endpoint'].contains('..') && call.positional['endpoint'].matches('^repos/[^/]+/[^/?]+([/?].*)?$'))`

// messagingbotRepoViewCEL is the parallel copy of the toolspec's
// require-a-positional constraint on `gh repo view`.
const messagingbotRepoViewCEL = `call.subcommand != 'repo view' || 'repo' in call.positional`

// messagingbotAllowlistCEL is the parallel copy of the repository ring. Same
// shape as ghAllowlistCEL, over the config.readableRepos key.
const messagingbotAllowlistCEL = `call.resourceId == '' || config.readableRepos.exists(r, glob.match('https://github.com/' + r, call.resourceId))`

func TestMessagingbotGhAPIConstraint(t *testing.T) {
	cases := []struct {
		name     string
		endpoint string
		flags    map[string]any
		want     bool
	}{
		{name: "read a repo's README: allowed", endpoint: "repos/acme/design/readme", want: true},
		{name: "list a directory at a pinned ref: allowed",
			endpoint: "repos/anthropics/skills/contents/skills/theme-factory/themes?ref=dbd4588", want: true},
		{name: "read a nested file: allowed", endpoint: "repos/acme/design/contents/tokens/color.json", want: true},
		{name: "repo metadata (bare repos/OWNER/NAME): allowed", endpoint: "repos/acme/design", want: true},
		{name: "a jq filter on a read: allowed",
			endpoint: "repos/acme/design/contents/", flags: map[string]any{"jq": ".[].name"}, want: true},
		{name: "an explicit method: refused",
			endpoint: "repos/acme/design/contents/README.md", flags: map[string]any{"method": "PUT"}, want: false},
		{name: "a raw-field body flips gh to POST: refused",
			endpoint: "repos/acme/design/issues", flags: map[string]any{"raw-field": "title=x"}, want: false},
		{name: "a typed field body flips gh to POST: refused",
			endpoint: "repos/acme/design/issues", flags: map[string]any{"field": "title=x"}, want: false},
		{name: "a body read from a file: refused",
			endpoint: "repos/acme/design/issues", flags: map[string]any{"input": "/tmp/b.json"}, want: false},
		{name: "rerouting to another host: refused",
			endpoint: "repos/acme/design/readme", flags: map[string]any{"hostname": "ghe.attacker.example"}, want: false},
		{name: "code search names no repository: refused, would bypass the allowlist",
			endpoint: "search/code?q=secret", want: false},
		{name: "an org listing names no repository: refused", endpoint: "orgs/acme/repos", want: false},
		{name: "the token's own user: refused", endpoint: "user", want: false},
		{name: "owner only, no repository: refused", endpoint: "repos/acme", want: false},
		{name: "path traversal out of an allowlisted repo: refused",
			endpoint: "repos/acme/design/contents/../../../other/secret/contents", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			flags := tc.flags
			if flags == nil {
				flags = map[string]any{}
			}
			got := evalCEL(t, messagingbotGhAPICEL, map[string]any{
				"call": map[string]any{
					"subcommand": "api",
					"positional": map[string]any{"endpoint": tc.endpoint},
					"flags":      flags,
				},
			})
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestMessagingbotRepoViewConstraint(t *testing.T) {
	cases := []struct {
		name       string
		subcommand string
		positional map[string]any
		want       bool
	}{
		{"repo view naming the repo: allowed", "repo view", map[string]any{"repo": "acme/design"}, true},
		{"repo view from the working-directory remote: refused", "repo view", map[string]any{}, false},
		{"another subcommand is not gated by this rule", "auth status", map[string]any{}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := evalCEL(t, messagingbotRepoViewCEL, map[string]any{
				"call": map[string]any{"subcommand": tc.subcommand, "positional": tc.positional, "flags": map[string]any{}},
			})
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestMessagingbotRepoAllowlistCEL(t *testing.T) {
	shipped := []any{"anthropics/skills"}
	cases := []struct {
		name       string
		resourceID string
		readable   []any
		want       bool
	}{
		{"skills source readable with no brand repos", "https://github.com/anthropics/skills", shipped, true},
		{"brand repo appended at install: readable", "https://github.com/acme/design", []any{"anthropics/skills", "acme/design"}, true},
		{"unlisted repo refused", "https://github.com/acme/payroll", []any{"anthropics/skills", "acme/design"}, false},
		{"org glob covers its repos", "https://github.com/acme/messaging", []any{"acme/*"}, true},
		{"auth status names no repo: not gated", "", shipped, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := evalCEL(t, messagingbotAllowlistCEL, map[string]any{
				"call":   map[string]any{"resourceId": tc.resourceID},
				"config": map[string]any{"readableRepos": tc.readable},
			})
			assert.Equal(t, tc.want, got)
		})
	}
}
