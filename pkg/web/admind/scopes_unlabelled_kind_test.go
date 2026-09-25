//go:build !integration && !e2e

package admind_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/go-logr/logr/testr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/spicedb/relsource"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/platform/relsync"
	"github.com/authzed/openagentprimitives/pkg/web/admind"
)

// unlabelledSyncKind is a relsync.Kind that deliberately does NOT implement
// relsync.ScopeLabeler, so admind.New's type assertion has a negative case to
// take. Every SHIPPED kind resolves names now, which is why this cannot be a
// real one: pinning the branch to whichever kind happened to lack a labeler is
// exactly what made the previous version of that test need rewriting the first
// time a kind gained one.
//
// ListScopes and FetchScope are never reached — the test injects a
// fakeScopeReader, so nothing calls upstream — but the interface has to be
// satisfied to register.
//
// # Why this file is build-constrained, and why the TEST lives here too
//
// It registers into relsync's process-global registry from init(), and that
// registry is deliberately never reset (see relsync/kind.go). Unconstrained,
// the registration would be linked into this package's integration binary too,
// where it is not merely useless but a fixture kind visible to whatever else
// walks relsync.All(). It registers no relsource.Source for the same reason:
// its Source is a plain VALUE the adapter passes through to the fake reader,
// and nothing here consults the claim registry, so there is no claim on a
// definition schema.zed has never heard of to leak anywhere.
//
// The test that drives it lives in this file rather than in scopes_test.go,
// and that is not tidiness — it is the constraint leaking in the other
// direction. An UNCONSTRAINED test naming this fixture compiles fine under the
// default build and breaks `go test -tags=integration` with "undefined:
// unlabelledKindName", which `mage test:unit` never sees. A constrained
// fixture and its only caller belong in the same file.
type unlabelledSyncKind struct{}

// unlabelledKindName is the spec.kind a test CR sets to select the fixture.
// Namespaced with "fixture" so it can never collide with a real kind name.
const unlabelledKindName = "fixture-unlabelled"

func init() { relsync.Register(unlabelledSyncKind{}) }

func (unlabelledSyncKind) Name() string { return unlabelledKindName }

func (unlabelledSyncKind) Source() relsource.Source {
	return relsource.Source{Name: "fixtureunlabelledsync", DisplayName: "Fixture Directory"}
}

func (unlabelledSyncKind) ListScopes(context.Context, relsync.SourceParams, relsync.Cursor) (relsync.ScopePage, error) {
	return relsync.ScopePage{Complete: true}, nil
}

func (unlabelledSyncKind) FetchScope(context.Context, relsync.SourceParams, relsync.Scope) (relsync.ScopeContent, error) {
	return relsync.ScopeContent{}, nil
}

// A kind that does NOT implement relsync.ScopeLabeler passes nil bridges — no
// panic on the type assertion, and no invented bridge.
//
// Driven by the fixture above rather than by a shipped kind, and that is now
// the only way to reach this branch at all: every registered kind resolves
// names today. Pinning the branch to whichever kind happened to lack a labeler
// is what made this test need rewriting the moment Slack gained one, and a
// fixture cannot go stale that way.
func TestAdmindScopes_UnlabelledKindPassesNoBridges(t *testing.T) {
	src := &spiceboxv1alpha1.RelationshipSource{
		ObjectMeta: metav1.ObjectMeta{Name: "acme-unlabelled", Namespace: "default"},
		Spec:       spiceboxv1alpha1.RelationshipSourceSpec{Kind: unlabelledKindName},
	}
	k8s := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(src).Build()
	reader := &fakeScopeReader{}

	a, err := admind.New(admind.Config{
		Mem:     memory.NewLocal(inmem.NewBackend()),
		K8s:     k8s,
		Checker: stubChecker{allow: map[string]bool{"YWRtaW4": true}},
		Token:   "test-token",
		Logger:  testr.New(t),
		Scopes:  reader,
	})
	require.NoError(t, err)

	w := do(t, a.Handler(), http.MethodGet, "/admin/v1/config/directory/default/acme-unlabelled", "test-token", "user:YWRtaW4", "")
	require.Equal(t, http.StatusOK, w.Code)

	require.Equal(t, 1, reader.calls, "a kind with no labeler is still read for its scopes")
	assert.Empty(t, reader.gotBridges, "a kind that declares no labeler must declare no bridges either")
}
