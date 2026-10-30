package github

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/utils/ptr"

	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

// stackedBody rewrites the opened fixture into the delivery under test: the
// action, the PR's base branch, the repository's default branch and, when
// retargetedFrom is set, an "edited" base change from that branch.
func stackedBody(t *testing.T, action, baseRef, defaultBranch, retargetedFrom string) []byte {
	t.Helper()
	var m map[string]any
	require.NoError(t, json.Unmarshal(fixtureBody(t, "pull_request_opened.json"), &m))
	m["action"] = action
	pr := m["pull_request"].(map[string]any)
	pr["base"].(map[string]any)["ref"] = baseRef
	m["repository"].(map[string]any)["default_branch"] = defaultBranch
	if retargetedFrom != "" {
		m["changes"] = map[string]any{"base": map[string]any{"ref": map[string]any{"from": retargetedFrom}}}
	}
	b, err := json.Marshal(m)
	require.NoError(t, err)
	return b
}

// A PR stacked on another open PR waits for it: every action that would
// normally start a review is dropped while the PR's base is another branch,
// and the review arrives with the retarget onto the default branch that
// follows the lower PR's merge. skipStacked=false restores reviewing into any
// base, for repositories that review into release branches.
func TestTranslate_StackedPullRequests(t *testing.T) {
	cases := []struct {
		name        string
		skipStacked *bool
		body        []byte
		wantReview  bool
		wantPrompt  string
	}{
		{name: "opened on the default branch: reviewed",
			body: stackedBody(t, "opened", "main", "main", ""), wantReview: true},
		{name: "opened stacked on another PR's branch: deferred (nil means skip)",
			body: stackedBody(t, "opened", "demo-contributor/stack-layer-1", "main", "")},
		{name: "pushed to while stacked: deferred",
			body: stackedBody(t, "synchronize", "demo-contributor/stack-layer-1", "main", "")},
		{name: "retargeted onto the default branch after the lower PR merged: reviewed, prompt says why",
			body: stackedBody(t, "edited", "main", "main", "demo-contributor/stack-layer-1"), wantReview: true,
			wantPrompt: "retargeted onto the default branch"},
		{name: "retargeted from one stack branch to another: still deferred",
			body: stackedBody(t, "edited", "demo-contributor/stack-layer-2", "main", "demo-contributor/stack-layer-1")},
		{name: "edited without a base change (title/body edit): ignored",
			body: stackedBody(t, "edited", "main", "main", "")},
		{name: "no default branch in the payload: fail open, reviewed",
			body: stackedBody(t, "opened", "demo-contributor/stack-layer-1", "", ""), wantReview: true},
		{name: "base change with no default branch in the payload: not a retarget, ignored",
			body: stackedBody(t, "edited", "demo-contributor/stack-layer-1", "", "main")},
		{name: "skipStacked=false, opened into a release branch: reviewed",
			skipStacked: ptr.To(false), body: stackedBody(t, "opened", "release-1.4", "main", ""), wantReview: true},
		{name: "skipStacked=false, retarget is not a listed event: ignored",
			skipStacked: ptr.To(false), body: stackedBody(t, "edited", "main", "main", "release-1.4")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ch := testChannel(t)
			ch.Spec.GitHub.SkipStacked = tc.skipStacked

			got, err := receiver{}.Translate(context.Background(), ch,
				channelkinds.WebhookRequest{Headers: prHeaders("pull_request"), Body: tc.body})
			require.NoError(t, err)
			if !tc.wantReview {
				assert.Nil(t, got, "no review session for this delivery")
				return
			}
			require.NotNil(t, got, "this delivery starts a review")
			if tc.wantPrompt != "" {
				assert.Contains(t, got.MessageText, tc.wantPrompt)
				assert.NotContains(t, got.MessageText, "was edited", "a retarget must not read as an edit")
				assert.Contains(t, got.MessageText, "Previous base branch: demo-contributor/stack-layer-1",
					"the previous base is named, inside the untrusted block")
			}
		})
	}
}

// The opening line names the retarget instead of the raw "edited".
func TestDescribeDelivery_Retarget(t *testing.T) {
	body := stackedBody(t, "edited", "main", "main", "demo-contributor/stack-layer-1")
	var ev prEvent
	require.NoError(t, json.Unmarshal(body, &ev))
	_, facts := describeDelivery(formatChannelKey(ev.Repository.FullName, ev.Number), "pull_request", body)
	assert.Contains(t, facts, "retargeted onto main")
	assert.NotContains(t, facts, "edited")
}
