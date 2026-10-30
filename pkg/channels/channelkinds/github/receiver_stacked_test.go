package github

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/utils/ptr"

	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

// lowerTip stands in for the tip of the branch a stacked PR was based on:
// the base commit an "edited" retarget reports it moved FROM. It differs from
// the fixture's own base.sha, as a real retarget's does once the lower PR has
// merged into the default branch.
const lowerTip = "1111111111111111111111111111111111111111"

// stackedBody rewrites the opened fixture into the delivery under test: the
// action, the PR's base branch, the repository's default branch and, when
// retargetedFrom is set, an "edited" base change from that branch whose
// previous base commit is lowerTip. mods adjust the result further.
func stackedBody(t *testing.T, action, baseRef, defaultBranch, retargetedFrom string, mods ...func(m map[string]any)) []byte {
	t.Helper()
	var m map[string]any
	require.NoError(t, json.Unmarshal(fixtureBody(t, "pull_request_opened.json"), &m))
	m["action"] = action
	pr := m["pull_request"].(map[string]any)
	pr["base"].(map[string]any)["ref"] = baseRef
	m["repository"].(map[string]any)["default_branch"] = defaultBranch
	if retargetedFrom != "" {
		m["changes"] = map[string]any{"base": map[string]any{
			"ref": map[string]any{"from": retargetedFrom},
			"sha": map[string]any{"from": lowerTip},
		}}
	}
	for _, mod := range mods {
		mod(m)
	}
	b, err := json.Marshal(m)
	require.NoError(t, err)
	return b
}

func asDraft(m map[string]any) { m["pull_request"].(map[string]any)["draft"] = true }

// baseCommitUnchanged makes the base change keep the base commit, as a
// default-branch rename does.
func baseCommitUnchanged(m map[string]any) {
	sha := m["pull_request"].(map[string]any)["base"].(map[string]any)["sha"]
	m["changes"].(map[string]any)["base"].(map[string]any)["sha"] = map[string]any{"from": sha}
}

func withoutPreviousBaseCommit(m map[string]any) {
	delete(m["changes"].(map[string]any)["base"].(map[string]any), "sha")
}

// A PR stacked on another open PR waits for it: every action that would
// normally start a review is dropped while the PR's base is another branch,
// and the review arrives with the retarget onto the default branch that
// follows the lower PR's merge. An unset skipStacked (a Channel written before
// the field existed) keeps reviewing into any base.
func TestTranslate_StackedPullRequests(t *testing.T) {
	on := ptr.To(true)
	const stack1, stack2 = "demo-contributor/stack-layer-1", "demo-contributor/stack-layer-2"
	cases := []struct {
		name        string
		skipStacked *bool
		events      []string // nil keeps testChannel's opened+synchronize
		repos       []string
		body        []byte
		wantReview  bool
		wantPrompt  bool // the retarget prompt, with the previous base commit
	}{
		{name: "opened on the default branch: reviewed",
			skipStacked: on, body: stackedBody(t, "opened", "main", "main", ""), wantReview: true},
		{name: "opened stacked on another PR's branch: deferred",
			skipStacked: on, body: stackedBody(t, "opened", stack1, "main", "")},
		{name: "pushed to while stacked: deferred",
			skipStacked: on, body: stackedBody(t, "synchronize", stack1, "main", "")},
		{name: "marked ready while stacked: deferred",
			skipStacked: on, events: []string{"opened", "ready_for_review"},
			body: stackedBody(t, "ready_for_review", stack1, "main", "")},
		{name: "reopened while stacked: deferred",
			skipStacked: on, events: []string{"opened", "reopened"},
			body: stackedBody(t, "reopened", stack1, "main", "")},
		{name: "retargeted onto the default branch after the lower PR merged: reviewed, prompt says why",
			skipStacked: on, body: stackedBody(t, "edited", "main", "main", stack1), wantReview: true, wantPrompt: true},
		{name: "retarget whose payload omits the previous base commit: reviewed on the branch names",
			skipStacked: on, body: stackedBody(t, "edited", "main", "main", stack1, withoutPreviousBaseCommit), wantReview: true},
		{name: "retarget of a draft: skipped like any draft",
			skipStacked: on, body: stackedBody(t, "edited", "main", "main", stack1, asDraft)},
		{name: "retargeted from one stack branch to another: still deferred",
			skipStacked: on, body: stackedBody(t, "edited", stack2, "main", stack1)},
		{name: "default branch renamed (base commit unchanged): not a retarget, ignored",
			skipStacked: on, body: stackedBody(t, "edited", "main", "main", "master", baseCommitUnchanged)},
		{name: "edited without a base change (title/body edit): ignored",
			skipStacked: on, body: stackedBody(t, "edited", "main", "main", "")},
		{name: "retarget on a Channel that does not review opened PRs: ignored",
			skipStacked: on, events: []string{"ready_for_review"},
			body: stackedBody(t, "edited", "main", "main", stack1)},
		{name: "retarget in a repository outside the allowlist: ignored",
			skipStacked: on, repos: []string{"demo-org/elsewhere"},
			body: stackedBody(t, "edited", "main", "main", stack1)},
		{name: "no default branch in the payload: fail open, reviewed",
			skipStacked: on, body: stackedBody(t, "opened", stack1, "", ""), wantReview: true},
		{name: "base change with no default branch in the payload: not a retarget, ignored",
			skipStacked: on, body: stackedBody(t, "edited", stack1, "", "main")},
		{name: "skipStacked unset (existing Channel), opened stacked: reviewed as before",
			body: stackedBody(t, "opened", stack1, "main", ""), wantReview: true},
		{name: "skipStacked unset, retarget is not a listed event: ignored",
			body: stackedBody(t, "edited", "main", "main", stack1)},
		{name: "skipStacked=false, opened into a release branch: reviewed",
			skipStacked: ptr.To(false), body: stackedBody(t, "opened", "release-1.4", "main", ""), wantReview: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ch := testChannel(t)
			ch.Spec.GitHub.SkipStacked = tc.skipStacked
			if tc.events != nil {
				ch.Spec.GitHub.Events = tc.events
			}
			ch.Spec.GitHub.Repositories = tc.repos

			got, err := receiver{}.Translate(context.Background(), ch,
				channelkinds.WebhookRequest{Headers: prHeaders("pull_request"), Body: tc.body})
			require.NoError(t, err)
			if !tc.wantReview {
				assert.Nil(t, got, "no review session for this delivery")
				return
			}
			require.NotNil(t, got, "this delivery starts a review")
			if tc.wantPrompt {
				assert.Contains(t, got.MessageText, "retargeted onto the default branch")
				assert.NotContains(t, got.MessageText, "was edited", "a retarget must not read as an edit")
				assert.Contains(t, got.MessageText, "Previous base commit: "+lowerTip,
					"the lower PR's tip bounds the review to this PR's own commits")
				assert.Contains(t, got.MessageText, "Previous base branch: "+stack1,
					"the previous base is named, inside the untrusted block")
			}
		})
	}
}

// The deferral log names a PR someone is waiting on, so it must not fire for
// a repository the Channel never reviews.
func TestTranslate_StackedDeferralLogOnlyForReviewedRepositories(t *testing.T) {
	cases := []struct {
		name    string
		repos   []string
		wantLog bool
	}{
		{name: "repository in the allowlist: deferral logged", repos: []string{"demo-org/platform"}, wantLog: true},
		{name: "repository outside the allowlist: nothing logged", repos: []string{"demo-org/elsewhere"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var logged []string
			ctx := logr.NewContext(context.Background(), funcr.New(func(_, args string) { logged = append(logged, args) }, funcr.Options{}))
			ch := testChannel(t)
			ch.Spec.GitHub.SkipStacked = ptr.To(true)
			ch.Spec.GitHub.Repositories = tc.repos

			got, err := receiver{}.Translate(ctx, ch, channelkinds.WebhookRequest{
				Headers: prHeaders("pull_request"),
				Body:    stackedBody(t, "opened", "demo-contributor/stack-layer-1", "main", ""),
			})
			require.NoError(t, err)
			assert.Nil(t, got)
			assert.Equal(t, tc.wantLog, strings.Contains(strings.Join(logged, "\n"), "deferring review of a stacked pull request"))
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
