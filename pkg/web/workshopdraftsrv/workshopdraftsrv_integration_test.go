//go:build integration

// Integration tests for the tuple-authorized draft-export route against a
// REAL apiserver (envtest) and a REAL SpiceDB (testspicedb).
// workshopdraftsrv_test.go's fake WorkshopBuildChecker proves the ROUTE
// re-checks a tuple rather than trusting the bearer; it says nothing about
// whether CheckWorkshopBuild itself, against a live backend, answers that
// check correctly. These tests wire the real
// (*pkg/authz/spicedb.Client).CheckWorkshopBuild end to end:
// EnsureWorkshopSubjects really writes the workshop:<id>#build tuple, the
// route really reads it back, and the no-tuple case proves the refusal holds
// against the real authorization backend, not a stand-in for it. Mirrors
// pkg/controllers/workshopprobe/controller_integration_test.go's shape.
package workshopdraftsrv_test

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/spicedb"
	"github.com/authzed/openagentprimitives/pkg/controllers/testenv"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/memory/tokens"
	"github.com/authzed/openagentprimitives/pkg/platform/artifactstore/blob"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/web/workshopdraftsrv"
	"github.com/authzed/openagentprimitives/test/testspicedb"
)

func TestMain(m *testing.M) {
	code := testenv.RunPackage(m)
	// These tests boot a shared SpiceDB container; stop it after the
	// apiserver so a package run leaves no container behind — mirrors
	// pkg/controllers/workshopprobe/controller_integration_test.go.
	testspicedb.StopShared()
	os.Exit(code)
}

func newIntegrationSpiceDBClient(t *testing.T) *spicedb.Client {
	t.Helper()
	endpoint := testspicedb.SharedEndpoint(t)
	token := testspicedb.UniqueToken(t)
	testspicedb.WriteSchema(t, endpoint, token)
	c, err := spicedb.NewClient(endpoint, token, true /* insecure */)
	require.NoError(t, err, "spicedb.NewClient")
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func integrationWorkshop(sessNS, sessName string) *spiceboxv1alpha1.Workshop {
	return &spiceboxv1alpha1.Workshop{
		ObjectMeta: metav1.ObjectMeta{
			Name:      spiceboxv1alpha1.WorkshopName(sessName),
			Namespace: sessNS,
		},
		Spec: spiceboxv1alpha1.WorkshopSpec{
			Session:        spiceboxv1alpha1.NamespacedRef{Namespace: sessNS, Name: sessName},
			SidecarToolbox: "workshop",
			Limits: spiceboxv1alpha1.WorkshopLimits{
				MaxAge:              metav1.Duration{Duration: time.Hour},
				MaxObjectsPerKind:   10,
				MaxObjects:          50,
				MaxConcurrentProbes: 2,
			},
		},
	}
}

// postDraftIntegration builds and issues the request; kept small since both
// cases below share it.
func postDraftIntegration(t *testing.T, srvURL, token string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, srvURL+workshopdraftsrv.Path, bytes.NewReader(draftBytes(t)))
	require.NoError(t, err, "NewRequest")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err, "Do")
	return resp
}

func TestIntegration_TuplePresent_StoresAndSetsExport(t *testing.T) {
	env := testenv.Shared(t)
	spdb := newIntegrationSpiceDBClient(t)
	ctx := context.Background()

	const (
		sessNS      = "default"
		sessName    = "builder-int-ok"
		wsNamespace = "ws-int-ok-1"
	)

	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Namespace: sessNS, Name: sessName},
		Spec: spiceboxv1alpha1.AgentSessionSpec{
			Class:  "demo-class",
			Prompt: spiceboxv1alpha1.PromptSource{Inline: "draft an agent"},
		},
	}
	require.NoError(t, env.Client.Create(ctx, sess), "create builder AgentSession")

	ws := integrationWorkshop(sessNS, sessName)
	require.NoError(t, env.Client.Create(ctx, ws), "create Workshop")
	ws.Status.Namespace = wsNamespace
	ws.Status.Phase = spiceboxv1alpha1.WorkshopPhaseReady
	require.NoError(t, env.Client.Status().Update(ctx, ws), "mark Workshop Ready")

	require.NoError(t, spdb.EnsureWorkshopSubjects(ctx, wsNamespace, sessNS, sessName, identity.CanonicalUserID{}), "seed the workshop#build tuple")

	mem := memory.NewLocal(inmem.NewBackend())
	store := blob.NewMem()
	reg := tokens.NewRegistry()
	reg.Set(memory.NamespacedName{Namespace: sessNS, Name: spiceboxv1alpha1.WorkshopName(sessName)}, "tok-int-ok", "")

	srv := httptest.NewServer(workshopdraftsrv.NewHandler(env.Client, mem, store, reg, spdb))
	t.Cleanup(srv.Close)

	resp := postDraftIntegration(t, srv.URL, "tok-int-ok")
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode, "a genuinely-held tuple lets the store through")

	var got spiceboxv1alpha1.Workshop
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: sessNS, Name: spiceboxv1alpha1.WorkshopName(sessName)}, &got))
	require.NotNil(t, got.Status.Export, "status.export set")
	assert.NotEmpty(t, got.Status.Export.ArtifactRef)
	assert.NotEmpty(t, got.Status.Export.Digest)

	out := decodeDraftResponse(t, resp)
	var cr spiceboxv1alpha1.ArtifactRender
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: sessNS, Name: out.Handle}, &cr), "get the created ArtifactRender")
	require.Len(t, cr.OwnerReferences, 1)
	assert.Equal(t, sess.UID, cr.OwnerReferences[0].UID, "owned by the real, apiserver-assigned builder session UID")
}

// TestIntegration_TupleAbsent_DeniesAndStoresNothing is the load-bearing
// refusing-direction integration test: a real, Ready Workshop with a
// correctly-registered bearer is still denied when the workshop:<id>#build
// tuple was never written in the real SpiceDB backend.
func TestIntegration_TupleAbsent_DeniesAndStoresNothing(t *testing.T) {
	env := testenv.Shared(t)
	spdb := newIntegrationSpiceDBClient(t)
	ctx := context.Background()

	const (
		sessNS      = "default"
		sessName    = "builder-int-deny"
		wsNamespace = "ws-int-deny-1"
	)

	ws := integrationWorkshop(sessNS, sessName)
	require.NoError(t, env.Client.Create(ctx, ws), "create Workshop")
	ws.Status.Namespace = wsNamespace
	ws.Status.Phase = spiceboxv1alpha1.WorkshopPhaseReady
	require.NoError(t, env.Client.Status().Update(ctx, ws), "mark Workshop Ready")

	// Deliberately NOT calling EnsureWorkshopSubjects: no workshop#build tuple
	// exists in this test's SpiceDB datastore.

	mem := memory.NewLocal(inmem.NewBackend())
	store := blob.NewMem()
	reg := tokens.NewRegistry()
	reg.Set(memory.NamespacedName{Namespace: sessNS, Name: spiceboxv1alpha1.WorkshopName(sessName)}, "tok-int-deny", "")

	srv := httptest.NewServer(workshopdraftsrv.NewHandler(env.Client, mem, store, reg, spdb))
	t.Cleanup(srv.Close)

	resp := postDraftIntegration(t, srv.URL, "tok-int-deny")
	defer resp.Body.Close()
	assert.Equal(t, http.StatusForbidden, resp.StatusCode, "no tuple in the real backend ⇒ denied")

	var got spiceboxv1alpha1.Workshop
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: sessNS, Name: spiceboxv1alpha1.WorkshopName(sessName)}, &got))
	assert.Nil(t, got.Status.Export, "status.export never set on denial")
}
