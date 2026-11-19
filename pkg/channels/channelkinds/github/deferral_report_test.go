package github_test

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/utils/ptr"

	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/github"
)

// deferralBody rewrites the opened fixture (demo-org/platform#42, the same
// pull request fakeGitHub answers for) into a delivery with the given action
// and base branch, on a repository whose default branch is main.
func deferralBody(t *testing.T, action, baseRef string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "pull_request_opened.json"))
	require.NoError(t, err)
	var m map[string]any
	require.NoError(t, json.Unmarshal(raw, &m))
	m["action"] = action
	m["pull_request"].(map[string]any)["base"].(map[string]any)["ref"] = baseRef
	m["repository"].(map[string]any)["default_branch"] = "main"
	b, err := json.Marshal(m)
	require.NoError(t, err)
	return b
}

func eventHeader(event string) http.Header {
	h := http.Header{}
	h.Set("X-GitHub-Event", event)
	return h
}

func deferralReporter(t *testing.T) channelkinds.WebhookDeferralReporter {
	t.Helper()
	rep, ok := github.Kind{}.WebhookReceiver(channelkinds.Deps{}).(channelkinds.WebhookDeferralReporter)
	require.True(t, ok, "the github receiver reports its deferrals")
	return rep
}

// A deferred stacked PR gets a QUEUED check run saying why — the only trace of
// the deferral a person looking at the PR sees. Queued, not a neutral
// conclusion: a neutral run would satisfy a required check and let the PR
// merge unreviewed if the retarget delivery never arrives.
func TestReportDeferral(t *testing.T) {
	const stack1 = "demo-contributor/stack-layer-1"
	cases := []struct {
		name        string
		skipStacked bool
		action      string
		baseRef     string
		event       string
		existing    []map[string]any
		wantQueued  bool
	}{
		{name: "stacked PR deferred: queued check run naming the base", skipStacked: true,
			action: "opened", baseRef: stack1, event: "pull_request", wantQueued: true},
		{name: "push to a stacked PR deferred: queued check run", skipStacked: true,
			action: "synchronize", baseRef: stack1, event: "pull_request", wantQueued: true},
		{name: "PR on the default branch: not deferred, nothing written", skipStacked: true,
			action: "opened", baseRef: "main", event: "pull_request"},
		{name: "skipStacked off: not deferred, nothing written",
			action: "opened", baseRef: stack1, event: "pull_request"},
		{name: "unlisted action on a stacked PR: ignored, not deferred, nothing written", skipStacked: true,
			action: "labeled", baseRef: stack1, event: "pull_request"},
		{name: "not a pull_request event: nothing written", skipStacked: true,
			action: "opened", baseRef: stack1, event: "push"},
		{name: "head already carries a run: left untouched", skipStacked: true,
			action: "synchronize", baseRef: stack1, event: "pull_request",
			existing: []map[string]any{{"id": 4242, "name": fxAppSlug, "status": "completed", "conclusion": "success"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := newFakeGitHub(t)
			g.existing = tc.existing
			ch := fixtureChannel()
			ch.Spec.GitHub.Events = []string{"opened", "synchronize"}
			ch.Spec.GitHub.SkipStacked = ptr.To(tc.skipStacked)

			err := deferralReporter(t).ReportDeferral(context.Background(), ch, fixtureSecrets(t),
				channelkinds.WebhookRequest{Headers: eventHeader(tc.event), Body: deferralBody(t, tc.action, tc.baseRef)},
				g.srv.URL)
			require.NoError(t, err)

			writes := g.writes()
			if !tc.wantQueued {
				assert.Empty(t, writes)
				return
			}
			require.Len(t, writes, 1, "one queued check run")
			w := writes[0]
			assert.Equal(t, http.MethodPost, w.Method)
			assert.Equal(t, "/repos/"+fxOwner+"/"+fxRepo+"/check-runs", w.Path)
			assert.Equal(t, "queued", w.Body["status"], "queued blocks a required check; neutral would pass it")
			assert.NotContains(t, w.Body, "conclusion", "a deferral is not a verdict")
			assert.Equal(t, fxAppSlug, w.Body["name"], "the same run Claim adopts when the review starts")
			assert.Equal(t, fxHeadSHA, w.Body["head_sha"])
			out, ok := w.Body["output"].(map[string]any)
			require.True(t, ok, "the run explains itself")
			assert.Contains(t, out["summary"], "`"+stack1+"`", "names the base it is stacked on")
			assert.Contains(t, out["summary"], "Automatically delete head branches", "says what ends the wait")
			assert.Contains(t, out["summary"], "closed without merging", "says what to do when the wait cannot end on its own")
		})
	}
}

// A provider failure is returned, so the route can log that the deferral is
// not visible, rather than reported as success.
func TestReportDeferral_ProviderFailureIsReturned(t *testing.T) {
	ch := fixtureChannel()
	ch.Spec.GitHub.Events = []string{"opened"}
	ch.Spec.GitHub.SkipStacked = ptr.To(true)
	err := deferralReporter(t).ReportDeferral(context.Background(), ch, fixtureSecrets(t),
		channelkinds.WebhookRequest{Headers: eventHeader("pull_request"), Body: deferralBody(t, "opened", "stack-layer-1")},
		"http://127.0.0.1:1") // nothing listens
	require.Error(t, err)
}

// When the review a deferral promised starts, Claim adopts the queued run and
// marks it in progress, rather than leaving "deferred" on the PR or opening a
// second run beside it.
func TestTriggerSurface_ClaimAdoptsADeferralNotice(t *testing.T) {
	g := newFakeGitHub(t)
	g.existing = []map[string]any{{"id": 4242, "name": fxAppSlug, "status": "queued"}}

	claim, err := newSurface(t, g).Claim(context.Background())
	require.NoError(t, err)
	assert.False(t, claim.Concluded)
	assert.Contains(t, claim.Ref, "4242", "the claim names the deferral's run")

	writes := g.writes()
	require.Len(t, writes, 1)
	assert.Equal(t, http.MethodPatch, writes[0].Method, "the queued run is updated, not duplicated")
	assert.Equal(t, "/repos/"+fxOwner+"/"+fxRepo+"/check-runs/4242", writes[0].Path)
	assert.Equal(t, "in_progress", writes[0].Body["status"])
}
