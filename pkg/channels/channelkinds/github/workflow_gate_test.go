package github

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	api "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/stretchr/testify/require"
)

type gateFixture struct {
	prs  []prEvent
	runs map[string][]workflowRun
}

func (f *gateFixture) Pulls(context.Context, string, string) ([]prEvent, error) { return f.prs, nil }
func (f *gateFixture) Runs(_ context.Context, _ string, path, _ string) ([]workflowRun, error) {
	return f.runs[path], nil
}
func (f *gateFixture) Run(_ context.Context, repo string, id int64, attempt int) (workflowRun, error) {
	for path, runs := range f.runs {
		for _, r := range runs {
			if r.ID == id {
				r.RunAttempt = attempt
				r.Path = path
				r.Event = "pull_request"
				r.Status = "completed"
				r.Conclusion = "success"
				r.Repository.FullName = repo
				r.PullRequests = []workflowPR{{Number: 7}}
				r.PullRequests[0].Head.SHA = "abc"
				r.PullRequests[0].Base.SHA = "base"
				return r, nil
			}
		}
	}
	return workflowRun{}, fmt.Errorf("unknown provider run")
}
func TestWorkflowGateWaitsAndSeparatesAttempts(t *testing.T) {
	ch := &api.Channel{Spec: api.ChannelSpec{GitHub: &api.GitHubChannelConfig{Trigger: &api.GitHubTriggerConfig{Event: "workflow_run", Actions: []string{"completed"}, Readiness: "all_completed", Workflows: map[string][]string{"acme/widget": {".github/workflows/test.yml", ".github/workflows/lint.yml"}}}}}}
	var pr prEvent
	require.NoError(t, json.Unmarshal([]byte(`{"number":7,"state":"open","head":{"sha":"abc","repo":{"full_name":"acme/widget"}},"base":{"sha":"base"},"node_id":"PR_7","user":{"id":19}}`), &pr.PullRequest))
	pr.Number = 7
	pr.Repository.FullName = "acme/widget"
	f := &gateFixture{prs: []prEvent{pr}, runs: map[string][]workflowRun{
		".github/workflows/test.yml": {{ID: 1, RunAttempt: 1, HeadSHA: "abc", Status: "completed", Conclusion: "success"}},
		".github/workflows/lint.yml": {{ID: 2, RunAttempt: 1, HeadSHA: "abc", Status: "in_progress"}},
	}}
	for path, runs := range f.runs {
		for i := range runs {
			runs[i].Event = "pull_request"
			runs[i].Path = path
			runs[i].PullRequests = []workflowPR{{Number: 7}}
			runs[i].PullRequests[0].Head.SHA = "abc"
			runs[i].PullRequests[0].Base.SHA = "base"
		}
		f.runs[path] = runs
	}
	body := []byte(`{"action":"completed","repository":{"full_name":"acme/widget"},"workflow_run":{"id":1,"head_sha":"abc","run_attempt":1,"path":".github/workflows/test.yml"}}`)
	result, err := admitWorkflow(context.Background(), ch, body, f)
	require.NoError(t, err)
	require.False(t, result.Done)
	require.Empty(t, result.Ready)
	f.runs[".github/workflows/lint.yml"][0].Status = "completed"
	f.runs[".github/workflows/lint.yml"][0].Conclusion = "success"
	result, err = admitWorkflow(context.Background(), ch, body, f)
	require.NoError(t, err)
	require.Len(t, result.Ready, 1)
	first := result.Ready[0]
	require.Equal(t, "abc", first.External["trigger_revision"])
	f.runs[".github/workflows/test.yml"][0].RunAttempt = 2
	result, err = admitWorkflow(context.Background(), ch, body, f)
	require.NoError(t, err)
	require.Len(t, result.Ready, 1)
	require.NotEqual(t, first.ChannelKey, result.Ready[0].ChannelKey)
	f.prs[0].PullRequest.Head.SHA = "new"
	result, err = admitWorkflow(context.Background(), ch, body, f)
	require.NoError(t, err)
	require.True(t, result.Done)
	require.Empty(t, result.Ready)
}

type mergeGateFixture struct {
	*gateFixture
	verified   workflowRun
	queriedSHA string
}

func (f *mergeGateFixture) Run(context.Context, string, int64, int) (workflowRun, error) {
	return f.verified, nil
}
func (f *mergeGateFixture) Runs(ctx context.Context, repo, path, sha string) ([]workflowRun, error) {
	f.queriedSHA = sha
	return f.gateFixture.Runs(ctx, repo, path, sha)
}
func TestWorkflowGateDistinguishesSourceHeadAndTestedMerge(t *testing.T) {
	ch := gateHTTPChannel()
	var pr prEvent
	require.NoError(t, json.Unmarshal([]byte(`{"state":"open","head":{"sha":"`+gateHTTPHead+`","repo":{"full_name":"synthetic/unit"}},"base":{"sha":"`+gateHTTPBase+`"},"user":{"id":41}}`), &pr.PullRequest))
	pr.Number = 7
	pr.Repository.FullName = "synthetic/unit"
	var verified workflowRun
	require.NoError(t, json.Unmarshal(gateHTTPBodyRun(t, gateHTTPRun(17, 1, "completed", "success", ".github/workflows/test.yml")), &verified))
	mergeSHA := "cccccccccccccccccccccccccccccccccccccccc"
	verified.HeadSHA = mergeSHA
	f := &mergeGateFixture{gateFixture: &gateFixture{prs: []prEvent{pr}, runs: map[string][]workflowRun{}}, verified: verified}
	for _, path := range ch.Spec.GitHub.Trigger.Workflows["synthetic/unit"] {
		run := verified
		run.Path = path
		f.runs[path] = []workflowRun{run}
	}
	body := gateHTTPBody(t, map[string]any{"id": 17, "run_attempt": 1, "head_sha": mergeSHA, "path": ".github/workflows/test.yml"})
	result, err := admitWorkflow(context.Background(), ch, body, f)
	require.NoError(t, err)
	require.Len(t, result.Ready, 1)
	require.Equal(t, gateHTTPHead, result.Ready[0].External["trigger_revision"])
	require.Equal(t, mergeSHA, f.queriedSHA)
	var snapshot struct {
		TestedSHA string `json:"run_head_sha"`
		HeadSHA   string `json:"head_sha"`
		BaseSHA   string `json:"base_sha"`
	}
	require.NoError(t, json.Unmarshal([]byte(result.Ready[0].External["workflow_runs"]), &snapshot))
	require.Equal(t, mergeSHA, snapshot.TestedSHA)
	require.Equal(t, gateHTTPHead, snapshot.HeadSHA)
	require.Equal(t, gateHTTPBase, snapshot.BaseSHA)
	firstKey := result.Ready[0].ChannelKey
	ch.Generation++
	result, err = admitWorkflow(context.Background(), ch, body, f)
	require.NoError(t, err)
	require.Len(t, result.Ready, 1)
	require.NotEqual(t, firstKey, result.Ready[0].ChannelKey, "handler reconfiguration must create a new CI cohort")
	f.prs[0].PullRequest.Base.SHA = "new-base"
	result, err = admitWorkflow(context.Background(), ch, body, f)
	require.NoError(t, err)
	require.Empty(t, result.Ready, "old tested base cannot publish on a newer base association")
}
func gateHTTPBodyRun(t *testing.T, r map[string]any) []byte {
	t.Helper()
	b, err := json.Marshal(r)
	require.NoError(t, err)
	return b
}
