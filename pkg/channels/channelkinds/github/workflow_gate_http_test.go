package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	api "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/stretchr/testify/require"
)

const gateHTTPHead = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
const gateHTTPBase = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

func gateHTTPChannel() *api.Channel {
	return &api.Channel{Spec: api.ChannelSpec{GitHub: &api.GitHubChannelConfig{Trigger: &api.GitHubTriggerConfig{Event: "workflow_run", Actions: []string{"completed"}, Readiness: "all_completed", Workflows: map[string][]string{"synthetic/unit": {".github/workflows/test.yml", ".github/workflows/lint.yml"}}}}}}
}
func gateHTTPRun(id int, attempt int, status, conclusion, workflow string) map[string]any {
	if id == 17 && strings.Contains(workflow, "lint.yml") {
		id = 18
	}
	return map[string]any{"id": id, "run_attempt": attempt, "head_sha": gateHTTPHead, "event": "pull_request", "path": workflow, "status": status, "conclusion": conclusion, "repository": map[string]any{"full_name": "synthetic/unit"}, "pull_requests": []any{map[string]any{"number": 7, "head": map[string]any{"sha": gateHTTPHead}, "base": map[string]any{"sha": gateHTTPBase}}}}
}
func gateHTTPBody(t *testing.T, run map[string]any) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{"action": "completed", "repository": map[string]any{"full_name": "synthetic/unit"}, "workflow_run": run})
	require.NoError(t, err)
	return b
}
func gateHTTPProvider(t *testing.T, prs []int, runs func(string, int) []map[string]any) *httpWorkflowProvider {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "Bearer fixture-token", r.Header.Get("Authorization"))
		var result any
		switch {
		case strings.Contains(r.URL.Path, "/actions/runs/") && strings.Contains(r.URL.Path, "/attempts/"):
			var id int
			var attempt int
			_, err := fmt.Sscanf(r.URL.Path, "/repos/synthetic/unit/actions/runs/%d/attempts/%d", &id, &attempt)
			require.NoError(t, err)
			// Only run 17 and the older run 1 are provider-authoritative fixtures.
			if id != 17 && id != 1 && id != 18 {
				http.NotFound(w, r)
				return
			}
			workflow := ".github/workflows/test.yml"
			if id == 18 {
				workflow = ".github/workflows/lint.yml"
			}
			result = gateHTTPRun(id, attempt, "completed", "success", workflow)
		case strings.Contains(r.URL.Path, "/commits/") && strings.HasSuffix(r.URL.Path, "/pulls"):
			entries := make([]map[string]any, 0, len(prs))
			for _, n := range prs {
				entries = append(entries, map[string]any{"number": n})
			}
			result = entries
		case strings.Contains(r.URL.Path, "/pulls/"):
			var n int
			_, err := fmt.Sscanf(r.URL.Path, "/repos/synthetic/unit/pulls/%d", &n)
			require.NoError(t, err)
			result = map[string]any{"number": n, "state": "open", "node_id": fmt.Sprintf("PR_%d", n), "head": map[string]any{"sha": gateHTTPHead, "repo": map[string]any{"full_name": "synthetic/unit"}}, "base": map[string]any{"sha": gateHTTPBase, "repo": map[string]any{"full_name": "synthetic/unit"}}, "user": map[string]any{"id": 41}}
		case strings.Contains(r.URL.Path, "/actions/workflows/"):
			require.Equal(t, gateHTTPHead, r.URL.Query().Get("head_sha"))
			require.Equal(t, "pull_request", r.URL.Query().Get("event"))
			var page int
			_, err := fmt.Sscanf(r.URL.Query().Get("page"), "%d", &page)
			require.NoError(t, err)
			workflow := ".github/workflows/test.yml"
			if strings.Contains(r.URL.Path, "lint.yml") {
				workflow = ".github/workflows/lint.yml"
			}
			items := runs(workflow, page)
			result = map[string]any{"workflow_runs": items, "total_count": len(items)}
		default:
			t.Errorf("unexpected provider endpoint %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		require.NoError(t, json.NewEncoder(w).Encode(result))
	}))
	t.Cleanup(server.Close)
	return &httpWorkflowProvider{token: "fixture-token", baseURL: server.URL, client: server.Client()}
}
func TestWorkflowGateHTTPPaginationSeesLatestQueuedAttempt(t *testing.T) {
	pages := 0
	p := gateHTTPProvider(t, []int{7}, func(workflow string, page int) []map[string]any {
		if workflow == ".github/workflows/lint.yml" {
			return []map[string]any{gateHTTPRun(501, 1, "completed", "success", workflow)}
		}
		pages++
		if page == 1 {
			items := make([]map[string]any, 100)
			for i := range items {
				items[i] = gateHTTPRun(i+1, 1, "completed", "success", workflow)
			}
			return items
		}
		return []map[string]any{gateHTTPRun(101, 2, "queued", "", workflow)}
	})
	body := gateHTTPBody(t, gateHTTPRun(1, 1, "completed", "success", ".github/workflows/test.yml"))
	result, err := admitWorkflow(context.Background(), gateHTTPChannel(), body, p)
	require.NoError(t, err)
	require.Equal(t, 2, pages)
	require.False(t, result.Done)
	require.Empty(t, result.Ready)
}
func TestWorkflowGateHTTPMissingPrerequisiteDefers(t *testing.T) {
	p := gateHTTPProvider(t, []int{7}, func(workflow string, _ int) []map[string]any {
		if strings.Contains(workflow, "lint") {
			return nil
		}
		return []map[string]any{gateHTTPRun(17, 1, "completed", "success", workflow)}
	})
	result, err := admitWorkflow(context.Background(), gateHTTPChannel(), gateHTTPBody(t, gateHTTPRun(17, 1, "completed", "success", ".github/workflows/test.yml")), p)
	require.NoError(t, err)
	require.False(t, result.Done)
	require.Empty(t, result.Ready)
}
func TestWorkflowGateHTTPRejectsMalformedLatestRun(t *testing.T) {
	for _, field := range []string{"event", "path", "run_attempt", "conclusion"} {
		t.Run(field, func(t *testing.T) {
			p := gateHTTPProvider(t, []int{7}, func(workflow string, _ int) []map[string]any {
				r := gateHTTPRun(17, 1, "completed", "success", workflow)
				if strings.Contains(workflow, "lint") {
					switch field {
					case "event":
						r[field] = ""
					case "path":
						r[field] = ".github/workflows/unselected.yml"
					case "run_attempt":
						r[field] = 0
					case "conclusion":
						r[field] = ""
					}
				}
				return []map[string]any{r}
			})
			result, err := admitWorkflow(context.Background(), gateHTTPChannel(), gateHTTPBody(t, gateHTTPRun(17, 1, "completed", "success", ".github/workflows/test.yml")), p)
			if err == nil {
				require.Empty(t, result.Ready, "malformed provider evidence must never admit an invocation")
			}
		})
	}
}
func TestWorkflowGateHTTPDoesNotFanRunOutToUnassociatedPR(t *testing.T) {
	p := gateHTTPProvider(t, []int{7, 8}, func(workflow string, _ int) []map[string]any {
		return []map[string]any{gateHTTPRun(17, 1, "completed", "success", workflow)}
	})
	result, err := admitWorkflow(context.Background(), gateHTTPChannel(), gateHTTPBody(t, gateHTTPRun(17, 1, "completed", "success", ".github/workflows/test.yml")), p)
	require.NoError(t, err)
	// Every provider run is associated only with PR 7. PR 8 has the same commit
	// but may have a different tested merge/base, so a SHA match is insufficient.
	require.LessOrEqual(t, len(result.Ready), 1)
	if len(result.Ready) == 1 {
		require.Contains(t, result.Ready[0].ChannelKey, "#7@")
	}
}
func TestWorkflowGateHTTPRejectsUnverifiedTriggerRunIdentity(t *testing.T) {
	p := gateHTTPProvider(t, []int{7}, func(workflow string, _ int) []map[string]any {
		return []map[string]any{gateHTTPRun(17, 1, "completed", "success", workflow)}
	})
	body := gateHTTPBody(t, gateHTTPRun(999, 42, "completed", "success", ".github/workflows/test.yml"))
	result, err := admitWorkflow(context.Background(), gateHTTPChannel(), body, p)
	if err == nil {
		require.Empty(t, result.Ready, "a trigger run absent from authenticated provider evidence cannot be accepted")
	}
}
func TestWorkflowGateHTTPRejectsPartialPagination(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, json.NewEncoder(w).Encode(map[string]any{"total_count": 101, "workflow_runs": []any{gateHTTPRun(17, 1, "completed", "success", ".github/workflows/test.yml")}}))
	}))
	defer srv.Close()
	p := httpWorkflowProvider{baseURL: srv.URL, client: srv.Client()}
	_, err := p.Runs(context.Background(), "synthetic/unit", ".github/workflows/test.yml", gateHTTPHead)
	require.Error(t, err, "partial list must not hide a newer queued attempt")
}
func TestNormalizeWorkflowPath(t *testing.T) {
	for _, raw := range []string{".github/workflows/unit.yml@main", ".github/workflows/unit.yml@refs/pull/7/merge", ".github/workflows/unit.yml"} {
		got, err := normalizeWorkflowPath(raw)
		require.NoError(t, err)
		require.Equal(t, ".github/workflows/unit.yml", got)
	}
	for _, raw := range []string{"../unit.yml", ".github/workflows/../unit.yml", ".github/workflows/unit.yml@", ".github/workflows/unit.yml@main@other", ".github/workflows/unit.yml@../main"} {
		_, err := normalizeWorkflowPath(raw)
		require.Error(t, err)
	}
}

func TestWorkflowGateDefersUnknownNewerRunAssociation(t *testing.T) {
	for _, association := range []string{"missing", "ambiguous"} {
		t.Run(association, func(t *testing.T) {
			p := gateHTTPProvider(t, []int{7}, func(workflow string, _ int) []map[string]any {
				old := gateHTTPRun(17, 1, "completed", "success", workflow)
				if !strings.Contains(workflow, "lint") {
					return []map[string]any{old}
				}
				newer := gateHTTPRun(19, 1, "queued", "", workflow)
				newer["pull_requests"] = []any{}
				if association == "ambiguous" {
					newer["pull_requests"] = []any{
						map[string]any{"number": 7, "head": map[string]any{"sha": gateHTTPHead}, "base": map[string]any{"sha": gateHTTPBase}},
						map[string]any{"number": 8, "head": map[string]any{"sha": gateHTTPHead}, "base": map[string]any{"sha": gateHTTPBase}},
					}
				}
				return []map[string]any{old, newer}
			})
			result, err := admitWorkflow(context.Background(), gateHTTPChannel(), gateHTTPBody(t, gateHTTPRun(17, 1, "completed", "success", ".github/workflows/test.yml")), p)
			require.NoError(t, err)
			require.Empty(t, result.Ready, "unknown newer applicability cannot establish readiness")
			require.False(t, result.Done, "keep durable work for later association resolution")
		})
	}
}
