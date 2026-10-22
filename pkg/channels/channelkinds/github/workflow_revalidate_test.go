package github

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	api "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/github/checkruns"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/credkind/githubapp"
)

func TestWorkflowRevalidationRejectsChangedChannelAndNewAttempt(t *testing.T) {
	for _, change := range []string{"channel", "attempt", "class"} {
		t.Run(change, func(t *testing.T) {
			attempt := 1
			p := gateHTTPProvider(t, []int{7}, func(workflow string, _ int) []map[string]any {
				return []map[string]any{gateHTTPRun(17, attempt, "completed", "success", workflow)}
			})
			ch := gateHTTPChannel()
			ch.Name = "input"
			ch.Namespace = "default"
			ch.Generation = 1
			ch.UID = "channel-identity"
			ch.Spec.AgentClass = "worker"
			class := &api.AgentClass{}
			class.Name = "worker"
			class.Namespace = "default"
			class.Generation = 3
			class.UID = "class-identity"
			ready, err := admitWorkflow(context.Background(), ch, gateHTTPBody(t, gateHTTPRun(17, 1, "completed", "success", ".github/workflows/test.yml")), p, workflowClassIdentity{Generation: 3, UID: "class-identity"})
			require.NoError(t, err)
			require.Len(t, ready.Ready, 1)
			binding := &api.ChannelBinding{Key: ready.Ready[0].ChannelKey, External: ready.Ready[0].External}
			scheme := runtime.NewScheme()
			require.NoError(t, api.AddToScheme(scheme))
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ch, class).Build()
			require.NoError(t, revalidateWorkflowCohort(context.Background(), c, client.ObjectKeyFromObject(ch), binding, p), "unchanged cohort is publishable")
			if change == "channel" {
				ch.Generation = 2
				require.NoError(t, c.Update(context.Background(), ch))
			} else if change == "class" {
				class.Generation = 4
				require.NoError(t, c.Update(context.Background(), class))
			} else {
				attempt = 2
			}
			err = revalidateWorkflowCohort(context.Background(), c, client.ObjectKeyFromObject(ch), binding, p)
			require.Error(t, err, "superseded cohort must not publish")
		})
	}
}

type revalidationMinter struct{}

func (revalidationMinter) Mint(context.Context, githubapp.MintRequest) (githubapp.MintedToken, error) {
	return githubapp.MintedToken{}, nil
}
func TestTriggerConclusionRefusesStaleReportAndAllowsNeutralClosure(t *testing.T) {
	for _, tc := range []struct {
		name    string
		outcome channelkinds.TriggerOutcome
		url     string
		allowed bool
	}{
		{"report", channelkinds.TriggerOutcomeClean, "", false},
		{"neutral with report", channelkinds.TriggerOutcomeCouldNotFinish, "https://reports.invalid/report", false},
		{"neutral without report", channelkinds.TriggerOutcomeCouldNotFinish, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			writes := 0
			checked := false
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					fmt.Fprint(w, `{"total_count":1,"check_runs":[{"id":1,"name":"check","external_id":"cohort","head_sha":"source","status":"in_progress"}]}`)
				} else {
					writes++
					fmt.Fprint(w, `{"id":1}`)
				}
			}))
			defer server.Close()
			surface := &checkRunSurface{repo: checkruns.Repo{Owner: "synthetic", Name: "unit"}, number: 7, name: "check", revision: "source", cohort: "cohort", minter: revalidationMinter{}, api: checkruns.New(checkruns.WithBaseURL(server.URL)), revalidate: func(context.Context, string) error { checked = true; return fmt.Errorf("superseded") }}
			err := surface.Conclude(context.Background(), channelkinds.TriggerConclusion{Outcome: tc.outcome, Summary: "Synthetic result", DetailsURL: tc.url})
			require.True(t, checked)
			if tc.allowed {
				require.NoError(t, err)
				require.Equal(t, 1, writes)
			} else {
				require.Error(t, err)
				require.Zero(t, writes)
			}
		})
	}
}
