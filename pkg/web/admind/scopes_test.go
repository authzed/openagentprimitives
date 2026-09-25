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
	"github.com/authzed/openagentprimitives/pkg/authz/resourcedisplay"
	"github.com/authzed/openagentprimitives/pkg/authz/spicedb"
	"github.com/authzed/openagentprimitives/pkg/authz/spicedb/relsource"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/web/admind"
)

// fakeScopeReader is a scripted admind.SourceScopeReader that records the
// relsource.Source it was actually called with — the point being to catch
// admind.New's own spec.kind -> relsync.Kind -> Source() resolution getting
// it wrong, a failure the section-builder unit tests in
// pkg/web/admind/config/projectors/directoryscopes_test.go cannot see: they
// drive directoryScopesSection directly with an explicit slice/error, never
// through admind.New's wrapper.
type fakeScopeReader struct {
	calls      int
	gotSrc     relsource.Source
	gotBridges []spicedb.ScopeLabelBridge
	gotCap     int
	scopes     spicedb.SourceScopes
	err        error
}

func (f *fakeScopeReader) ListSourceScopes(_ context.Context, src relsource.Source, bridges []spicedb.ScopeLabelBridge, capPerDefinition int) (spicedb.SourceScopes, error) {
	f.calls++
	f.gotSrc = src
	f.gotBridges = bridges
	f.gotCap = capPerDefinition
	return f.scopes, f.err
}

// TestAdmindScopes_ReaderReceivesTheResolvedRelsourceSource proves admind.New
// resolves RelationshipSource.Spec.Kind through relsync.Get before calling
// ListSourceScopes, rather than passing the bare kind string through — which
// ListSourceScopes cannot even accept, its signature takes a
// relsource.Source. Getting the resolution wrong (the wrong kind, or a
// zero-value Source) would make the Scopes panel silently probe nothing for
// every RelationshipSource, forever, with every other test in this task still
// green. "github" resolves for real here because oapinstall_channels_test.go
// (this package's own internal test file) blank-imports
// pkg/channels/channelkinds/github, registering its relsync.Kind into this
// binary's shared registry.
func TestAdmindScopes_ReaderReceivesTheResolvedRelsourceSource(t *testing.T) {
	src := &spiceboxv1alpha1.RelationshipSource{
		ObjectMeta: metav1.ObjectMeta{Name: "acme-github", Namespace: "default"},
		Spec:       spiceboxv1alpha1.RelationshipSourceSpec{Kind: "github"},
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

	w := do(t, a.Handler(), http.MethodGet, "/admin/v1/config/directory/default/acme-github", "test-token", "user:YWRtaW4", "")
	require.Equal(t, http.StatusOK, w.Code)

	require.Equal(t, 1, reader.calls, "the reader must be called exactly once for this detail read")
	assert.Equal(t, "githubdirectorysync", reader.gotSrc.Name,
		"admind.New must resolve spec.kind through relsync.Get(...).Source(), not pass the bare kind string through")
	assert.Positive(t, reader.gotCap, "the cap must be a real bound, not the zero value")
}

// The same resolution, for the other half admind.New owns: a kind that
// implements relsync.ScopeLabeler must have its bridges carried through to the
// reader. Getting THIS wrong is silent in a way the panel cannot show — every
// row renders its raw id, which is exactly what a kind with no bridges is
// supposed to look like, so a dropped type assertion here would leave the
// console indistinguishable from the state this whole change set out to fix.
func TestAdmindScopes_ReaderReceivesTheKindsDeclaredLabelBridges(t *testing.T) {
	src := &spiceboxv1alpha1.RelationshipSource{
		ObjectMeta: metav1.ObjectMeta{Name: "acme-github", Namespace: "default"},
		Spec:       spiceboxv1alpha1.RelationshipSourceSpec{Kind: "github"},
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

	w := do(t, a.Handler(), http.MethodGet, "/admin/v1/config/directory/default/acme-github", "test-token", "user:YWRtaW4", "")
	require.Equal(t, http.StatusOK, w.Code)

	require.Len(t, reader.gotBridges, 1, "the github kind declares exactly one scope-label bridge")
	b := reader.gotBridges[0]
	assert.Equal(t, "github_repo", b.ScopeDefinition, "the numeric-id definition is the one that needs naming")
	assert.Equal(t, "github_repo_url", b.BridgeDefinition, "the bridge is the URL-keyed type the sync already writes")
	assert.Equal(t, "repo", b.BridgeRelation)
	assert.Equal(t, resourcedisplay.DecoderB64URL, b.Decoder)
}

// The Slack kind's own bridge, through the same adapter, in the shape that
// distinguishes it from GitHub's: the scope is the tuple's RESOURCE and the
// name rides on the subject. Asserted here rather than only in the kind's own
// package because the adapter is what carries Shape across, and a Shape
// dropped in transit degrades SILENTLY — the read would run against the wrong
// definition, resolve nothing, and produce a page of raw ids that looks
// exactly like the state this change set out to fix.
func TestAdmindScopes_ReaderReceivesAStoredNameBridgeWithItsShape(t *testing.T) {
	src := &spiceboxv1alpha1.RelationshipSource{
		ObjectMeta: metav1.ObjectMeta{Name: "acme-slack", Namespace: "default"},
		Spec:       spiceboxv1alpha1.RelationshipSourceSpec{Kind: "slack"},
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

	w := do(t, a.Handler(), http.MethodGet, "/admin/v1/config/directory/default/acme-slack", "test-token", "user:YWRtaW4", "")
	require.Equal(t, http.StatusOK, w.Code)

	require.Len(t, reader.gotBridges, 1, "the slack kind declares exactly one scope-label bridge")
	b := reader.gotBridges[0]
	assert.Equal(t, "slack_channel", b.ScopeDefinition)
	assert.Equal(t, "string", b.BridgeDefinition, "a stored name rides the permission-less string type")
	assert.Equal(t, "label", b.BridgeRelation)
	assert.Equal(t, spicedb.ShapeNameOnSubject, b.Shape,
		"the scope is the resource and the name is the subject; the bridged shape would read the wrong definition")
	assert.Equal(t, resourcedisplay.DecoderB64Text, b.Decoder,
		"a directory-authored name decodes to a title and never to an href")
}

// The no-labeler branch of that same assertion lives in
// scopes_unlabelled_kind_test.go, with the fixture kind it needs: every
// SHIPPED kind resolves names now, so reaching it requires a registered
// fixture, and a registration into relsync's never-reset global registry must
// not be linked into the integration binary.

// A kind with no registered relsync.Kind (a typo, or a kind whose package was
// never linked into this binary) must surface as an unavailable read, not a
// panic and not a silently empty one — and the injected reader must never be
// called with a meaningless zero-value Source.
func TestAdmindScopes_UnregisteredKindRendersUnavailable(t *testing.T) {
	src := &spiceboxv1alpha1.RelationshipSource{
		ObjectMeta: metav1.ObjectMeta{Name: "mystery-source", Namespace: "default"},
		Spec:       spiceboxv1alpha1.RelationshipSourceSpec{Kind: "no-such-relsync-kind"},
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

	w := do(t, a.Handler(), http.MethodGet, "/admin/v1/config/directory/default/mystery-source", "test-token", "user:YWRtaW4", "")
	require.Equal(t, http.StatusOK, w.Code)

	assert.Equal(t, 0, reader.calls, "an unregistered kind must never reach the injected reader")
	assert.Contains(t, w.Body.String(), "unavailable")
}

// An admind built with no Scopes reader (the every-build-until-wired-in
// state) shows no Scopes tab at all, the same degrade-not-fail treatment
// Identities gets.
func TestAdmindScopes_NilConfigOmitsTheSection(t *testing.T) {
	src := &spiceboxv1alpha1.RelationshipSource{
		ObjectMeta: metav1.ObjectMeta{Name: "acme-github", Namespace: "default"},
		Spec:       spiceboxv1alpha1.RelationshipSourceSpec{Kind: "github"},
	}
	k8s := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(src).Build()

	a := newTestAdmind(t, k8s)

	w := do(t, a.Handler(), http.MethodGet, "/admin/v1/config/directory/default/acme-github", "test-token", "user:YWRtaW4", "")
	require.Equal(t, http.StatusOK, w.Code)
	assert.NotContains(t, w.Body.String(), "directoryscopes", "no reader installed -> no tab")
}
