package artifactref_test

import (
	"bytes"
	"context"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/toolguard"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/platform/artifacts"
	"github.com/authzed/openagentprimitives/pkg/web/uibindings"
	"github.com/authzed/openagentprimitives/pkg/web/uibindings/artifactref"
)

// fakeDeps' artifacts field is typed as *artifacts.Service — a concrete
// pointer, so the zero value's Artifacts() carries an honest nil, unlike
// memory.Memory's interface. fetch is a func type for the same reason (see
// uibindings.Deps' doc): the zero value's ArtifactRenderBytes() is likewise
// a genuine nil, no typed-nil trap possible for either.
type fakeDeps struct {
	artifacts *artifacts.Service
	fetch     uibindings.ArtifactRenderBytesFunc
}

func (f fakeDeps) Memory() memory.Memory                                   { return nil }
func (f fakeDeps) Artifacts() *artifacts.Service                           { return f.artifacts }
func (f fakeDeps) NATSRequest() channelevents.RequestFunc                  { return nil }
func (f fakeDeps) ArtifactRenderBytes() uibindings.ArtifactRenderBytesFunc { return f.fetch }
func (f fakeDeps) Logger() logr.Logger                                     { return logr.Discard() }

// renderedCR builds a minimal Ready ArtifactRender CR sufficient for
// artifacts.Service.FinalizeRevision to seed a resolvable head+revision —
// mirrors pkg/platform/artifacts/service_test.go's helper of the same shape (that one
// is unexported in a different test package and can't be imported).
func renderedCR(name, headID string, uid types.UID) *spiceboxv1alpha1.ArtifactRender {
	return &spiceboxv1alpha1.ArtifactRender{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "default", UID: uid,
			Labels:      map[string]string{artifacts.LabelArtifactID: headID},
			Annotations: map[string]string{artifacts.AnnoChangeDescription: "fixture"},
		},
		Spec: spiceboxv1alpha1.ArtifactRenderSpec{Kind: "html"},
		Status: spiceboxv1alpha1.ArtifactRenderStatus{
			Phase:      spiceboxv1alpha1.ArtifactRenderPhaseReady,
			OutputRef:  "mem://" + name,
			OutputMIME: "text/html",
			OutputSize: 10,
		},
	}
}

// depsWithArtifacts builds a uibindings.Deps whose Artifacts() is an
// artifacts.NewService over an in-memory memory.Memory seeded so that
// "artifact-fixture" resolves to render name "ar-fixture" in scope
// demo-ns/demo-session, and "artifact-elsewhere" resolves to a render only
// in a DIFFERENT session's scope — present in the backend, but invisible
// from demo-ns/demo-session, which is what proves scope-bounded resolution
// rather than a mere authorization filter. fetch becomes the deps'
// ArtifactRenderBytes(); nil is a valid argument for the fail-closed cases.
func depsWithArtifacts(t *testing.T, fetch uibindings.ArtifactRenderBytesFunc) uibindings.Deps {
	t.Helper()
	mem := memory.NewLocal(inmem.NewBackend())
	svc := artifacts.NewService(mem, nil)
	ctx := memory.WithSystemApproval(context.Background(), "test")

	fixtureScope := memory.Scope{Kind: "session", ID: "demo-ns/demo-session"}
	_, err := svc.FinalizeRevision(ctx, fixtureScope, renderedCR("ar-fixture", "artifact-fixture", types.UID("uid-fixture")))
	require.NoError(t, err, "seed artifact-fixture")

	elsewhereScope := memory.Scope{Kind: "session", ID: "other-ns/other-session"}
	_, err = svc.FinalizeRevision(ctx, elsewhereScope, renderedCR("ar-elsewhere", "artifact-elsewhere", types.UID("uid-elsewhere")))
	require.NoError(t, err, "seed artifact-elsewhere in a different session")

	return fakeDeps{artifacts: svc, fetch: fetch}
}

// testCtx stands in for the read authority Deps.Artifacts() already carries
// in production through its own token: webd's real artifacts.Service wraps
// memory/httpclient.Client, which authenticates a Query by bearer token
// rather than a ctx-borne capability, so the resolver itself mints nothing.
// This test drives artifacts.Service over the in-process memory.Local
// facade directly (depsWithArtifacts), which DOES enforce its capability
// door from ctx — minting a system approval here isolates this test from
// that door's own behavior (covered separately by pkg/memory's door tests),
// rather than accidentally re-testing it.
func testCtx() context.Context {
	return memory.WithSystemApproval(context.Background(), "test")
}

func TestArtifactResolver(t *testing.T) {
	r := artifactref.New()
	assert.Equal(t, "artifact", r.Source())

	t.Run("JSON content is bound as JSON", func(t *testing.T) {
		fetch := func(_ context.Context, ns, sess, render string) ([]byte, string, error) {
			assert.Equal(t, "demo-ns", ns)
			assert.Equal(t, "demo-session", sess)
			assert.Equal(t, "ar-fixture", render)
			return []byte(`[{"stage":"new","count":3}]`), "application/json", nil
		}
		res, err := r.Resolve(testCtx(), depsWithArtifacts(t, fetch), uibindings.Request{
			Namespace: "demo-ns", Session: "demo-session", Ref: "artifact-fixture",
		})
		require.NoError(t, err)
		assert.JSONEq(t, `[{"stage":"new","count":3}]`, string(res.Value))
	})

	t.Run("non-JSON content is bound as a JSON string for ap:markdown", func(t *testing.T) {
		fetch := func(context.Context, string, string, string) ([]byte, string, error) {
			return []byte("# Leads\n\nnothing yet"), "text/markdown", nil
		}
		res, err := r.Resolve(testCtx(), depsWithArtifacts(t, fetch), uibindings.Request{
			Namespace: "demo-ns", Session: "demo-session", Ref: "artifact-fixture",
		})
		require.NoError(t, err)
		assert.JSONEq(t, `"# Leads\n\nnothing yet"`, string(res.Value))
	})

	t.Run("a handle from another session is unresolvable, not merely unauthorized", func(t *testing.T) {
		fetch := func(context.Context, string, string, string) ([]byte, string, error) {
			t.Fatal("fetch must not be reached for an unresolvable handle")
			return nil, "", nil
		}
		_, err := r.Resolve(testCtx(), depsWithArtifacts(t, fetch), uibindings.Request{
			Namespace: "demo-ns", Session: "demo-session", Ref: "artifact-elsewhere",
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not available")
	})

	t.Run("content over the UI ceiling is refused", func(t *testing.T) {
		fetch := func(context.Context, string, string, string) ([]byte, string, error) {
			return bytes.Repeat([]byte("x"), int(toolguard.DefaultUIIngressBytes)+1), "text/plain", nil
		}
		_, err := r.Resolve(testCtx(), depsWithArtifacts(t, fetch), uibindings.Request{
			Namespace: "demo-ns", Session: "demo-session", Ref: "artifact-fixture",
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "too large")
	})

	t.Run("a nil fetch fails closed", func(t *testing.T) {
		_, err := r.Resolve(testCtx(), depsWithArtifacts(t, nil), uibindings.Request{
			Namespace: "demo-ns", Session: "demo-session", Ref: "artifact-fixture",
		})
		require.Error(t, err)
	})

	t.Run("a nil Artifacts fails closed", func(t *testing.T) {
		fetch := func(context.Context, string, string, string) ([]byte, string, error) {
			t.Fatal("fetch must not be reached when Artifacts is unconfigured")
			return nil, "", nil
		}
		_, err := r.Resolve(context.Background(), fakeDeps{fetch: fetch}, uibindings.Request{
			Namespace: "demo-ns", Session: "demo-session", Ref: "artifact-fixture",
		})
		require.Error(t, err)
	})
}
