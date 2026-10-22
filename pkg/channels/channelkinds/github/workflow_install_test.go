package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/credkind/githubapp"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/sensitive"
	"github.com/stretchr/testify/require"
)

type installMinter struct{ t *testing.T }

func (m installMinter) Mint(_ context.Context, req githubapp.MintRequest) (githubapp.MintedToken, error) {
	require.Equal(m.t, "404", req.InstallationID)
	return githubapp.MintedToken{AccessToken: sensitive.NewSensitiveValue([]byte("install-token"))}, nil
}

func TestWorkflowInstallDiscoversAuthenticatedRepositoryPaths(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "Bearer install-token", r.Header.Get("Authorization"))
		require.Equal(t, "/repos/synthetic/widgets/actions/workflows", r.URL.Path)
		_, err := w.Write([]byte(`{"workflows":[{"id":101,"name":"Compile","path":".github/workflows/compile.yml","state":"active"},{"id":102,"name":"Retired","path":".github/workflows/retired.yml","state":"disabled_manually"}]}`))
		require.NoError(t, err)
	}))
	defer server.Close()
	wizard := &wizard{workflowMinter: installMinter{t}, workflowCatalog: httpWorkflowCatalog{baseURL: server.URL, client: server.Client()}}
	in := channelkinds.WizardInput{Config: json.RawMessage(`{"trigger":{"event":"workflow_run","actions":["completed"],"readiness":"all_completed"}}`)}
	answers := map[string]string{keyInstallationID: "404", keyPrivateKeyPEM: "test-key", keyWorkflows: `{"synthetic/widgets":[".github/workflows/compile.yml"]}`}
	_, err := wizard.Resolve(context.Background(), in, answers)
	require.NoError(t, err)
	answers[keyWorkflows] = `{"synthetic/widgets":[".github/workflows/missing.yml"]}`
	_, err = wizard.Resolve(context.Background(), in, answers)
	require.ErrorContains(t, err, "Compile (.github/workflows/compile.yml)")
	answers[keyWorkflows] = `{"synthetic/widgets":[".github/workflows/retired.yml"]}`
	_, err = wizard.Resolve(context.Background(), in, answers)
	require.ErrorContains(t, err, "does not have active workflow")
}
