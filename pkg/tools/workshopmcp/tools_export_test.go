package workshopmcp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/testfixtures"
	"github.com/authzed/openagentprimitives/pkg/platform/oap"
	"github.com/authzed/openagentprimitives/pkg/platform/oap/instance"
)

// fixtureWorkshopForApplyTestServer returns the Workshop CR
// soleAuthoredClassName/standinNames resolve for a Server built by
// newApplyTestServer (SessionNamespace "b", SessionName "x") — BLOCKER-1
// (fix round 2) made both read status.standins, and fail closed if the Get
// itself fails, so every handleExportDraft fixture below now needs one of
// these to exist. standins seeds status.standins with exactly the entries a
// given test needs already recorded; a test with no rehearsal stand-ins in
// play passes none.
func fixtureWorkshopForApplyTestServer(standins ...spiceboxv1alpha1.WorkshopStandin) *spiceboxv1alpha1.Workshop {
	return &spiceboxv1alpha1.Workshop{
		ObjectMeta: metav1.ObjectMeta{Namespace: "b", Name: spiceboxv1alpha1.WorkshopName("x")},
		Status:     spiceboxv1alpha1.WorkshopStatus{Standins: standins},
	}
}

// stubOperatorDraftStore spins up an httptest server standing in for the
// Task-6 export route, capturing the POSTed bundle bytes and answering with a
// fixed artifactRef/digest. Sets the two env vars storeDraft reads.
func stubOperatorDraftStore(t *testing.T) (gotBody *[]byte) {
	t.Helper()
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var readErr error
		body, readErr = io.ReadAll(r.Body)
		require.NoError(t, readErr)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"artifactRef": "mem://fake/workshop-draft/deadbeef.oap",
			"digest":      "deadbeef",
			"handle":      "ar-builder-x-0a1b2c",
			"artifactId":  "artifact-0123456789abcdef",
		})
	}))
	t.Cleanup(srv.Close)
	t.Setenv("OPERATOR_MEMORY_URL", srv.URL)
	t.Setenv("token", "fake-operator-bearer")
	return &body
}

// stubOperatorDraftStoreNoHandle mirrors stubOperatorDraftStore, but answers
// the way an operator that predates the draft render does: artifactRef and
// digest only, no handle or artifactId.
func stubOperatorDraftStoreNoHandle(t *testing.T) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"artifactRef": "mem://fake/workshop-draft/deadbeef.oap",
			"digest":      "deadbeef",
		})
	}))
	t.Cleanup(srv.Close)
	t.Setenv("OPERATOR_MEMORY_URL", srv.URL)
	t.Setenv("token", "fake-operator-bearer")
}

// TestHandleExportDraft_StripsPrefixAndLabel_StampsOapSource_StoresBundle is
// the KEY assertion for export_draft: the exported bundle's SpiceboxToolspec
// AND SpiceboxToolkit (both cluster-scoped, the only two kinds the ws-<id>-
// prefix and LabelWorkshopNamespace ever apply to) carry NEITHER the prefix
// NOR the label — otherwise plan-2's ReferencesWorkshopTool rule refuses the
// installed class — both cross-refs (the AgentClass's toolspecs list, and the
// toolspec's own toolkit ref) follow the rename, and the root AgentClass
// carries the workshop oap-source provenance.
func TestHandleExportDraft_StripsPrefixAndLabel_StampsOapSource_StoresBundle(t *testing.T) {
	workshopID := "ws-fake0123abcd"
	toolspecName := workshopID + "-mytool"
	toolkitName := workshopID + "-toolkit1"

	ac := &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-agent", Namespace: workshopID},
		Spec: spiceboxv1alpha1.AgentClassSpec{
			DisplayName: "Demo Agent",
			ToolBundles: []spiceboxv1alpha1.ToolBundle{
				{Name: "tools", Toolspecs: []string{toolspecName}},
			},
		},
	}
	ts := &spiceboxv1alpha1.SpiceboxToolspec{
		ObjectMeta: metav1.ObjectMeta{
			Name:   toolspecName,
			Labels: map[string]string{spiceboxv1alpha1.LabelWorkshopNamespace: workshopID},
		},
		Spec: spiceboxv1alpha1.SpiceboxToolspecSpec{
			Name:             "mytool",
			Toolkit:          spiceboxv1alpha1.ToolspecToolkitRef{Name: "custom-tool", Revision: "v1"},
			AllowSubcommands: []string{"run"},
		},
	}
	tk := &spiceboxv1alpha1.SpiceboxToolkit{
		ObjectMeta: metav1.ObjectMeta{
			Name:   toolkitName,
			Labels: map[string]string{spiceboxv1alpha1.LabelWorkshopNamespace: workshopID},
		},
		Spec: spiceboxv1alpha1.SpiceboxToolkitSpec{Name: "custom-tool", ToolkitRevision: "v1"},
	}
	c := fake.NewClientBuilder().WithScheme(testfixtures.NewScheme(t)).WithObjects(ac, ts, tk, fixtureWorkshopForApplyTestServer()).Build()
	s := newApplyTestServer(workshopID, c)

	gotBody := stubOperatorDraftStore(t)

	res, err := s.handleExportDraft(context.Background(), &mcp.CallToolRequest{})
	require.NoError(t, err)
	require.False(t, res.IsError, "a clean export must not be a tool error")
	body := decodeApplyResult(t, res)
	assert.Equal(t, "mem://fake/workshop-draft/deadbeef.oap", body["artifactRef"])
	assert.Equal(t, "deadbeef", body["digest"])
	assert.Equal(t, "ar-builder-x-0a1b2c", body["handle"])
	assert.Equal(t, "artifact-0123456789abcdef", body["artifactId"])

	require.NotEmpty(t, *gotBody, "storeDraft must have POSTed the packed bundle to the operator route")
	packed, err := oap.Unpack(*gotBody)
	require.NoError(t, err)
	crs, err := packed.CRs()
	require.NoError(t, err)

	var gotToolspec, gotToolkit, gotClass *unstructured.Unstructured
	for _, cr := range crs {
		switch cr.GetKind() {
		case "SpiceboxToolspec":
			gotToolspec = cr
		case "SpiceboxToolkit":
			gotToolkit = cr
		case "AgentClass":
			gotClass = cr
		}
	}
	require.NotNil(t, gotToolspec, "the toolspec must be present in the exported bundle")
	require.NotNil(t, gotToolkit, "the toolkit must be present in the exported bundle")
	require.NotNil(t, gotClass, "the AgentClass must be present in the exported bundle")

	assert.Equal(t, "mytool", gotToolspec.GetName(), "the ws-<id>- prefix must be stripped from the toolspec's name")
	assert.NotContains(t, gotToolspec.GetLabels(), spiceboxv1alpha1.LabelWorkshopNamespace,
		"LabelWorkshopNamespace must be stripped — otherwise ReferencesWorkshopTool refuses the installed class")
	assert.Equal(t, "toolkit1", gotToolkit.GetName(), "the ws-<id>- prefix must be stripped from the toolkit's name too")
	assert.NotContains(t, gotToolkit.GetLabels(), spiceboxv1alpha1.LabelWorkshopNamespace,
		"LabelWorkshopNamespace must be stripped from the toolkit as well")

	toolBundles, found, err := unstructured.NestedSlice(gotClass.Object, "spec", "toolBundles")
	require.NoError(t, err)
	require.True(t, found)
	require.Len(t, toolBundles, 1)
	elem, ok := toolBundles[0].(map[string]any)
	require.True(t, ok)
	specs, ok, err := unstructured.NestedStringSlice(elem, "toolspecs")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, []string{"mytool"}, specs, "the AgentClass's own toolspecs ref must follow the rename")

	// spec.toolkit.name is the toolkit's LOGICAL identity (matched against
	// SpiceboxToolkit.spec.name+spec.toolkitRevision by
	// source/cluster.go's walkToolkits — see ToolspecToolkitRef's own doc),
	// never the CR's metadata.name — so it must stay exactly as authored,
	// unaffected by the metadata.name strip applied above.
	toolkitRef, found, err := unstructured.NestedString(gotToolspec.Object, "spec", "toolkit", "name")
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, "custom-tool", toolkitRef, "spec.toolkit.name names the toolkit's spec.name, not a metadata.name — it must be untouched by the strip")

	annotations := gotClass.GetAnnotations()
	require.Contains(t, annotations, instance.AnnotationOapSource)
	src, err := instance.ParseOapSource(annotations[instance.AnnotationOapSource])
	require.NoError(t, err)
	assert.Equal(t, "workshop", src.SourceKind)
	assert.Equal(t, workshopID, src.Ref)
}

// TestHandleExportDraft_OperatorAnswersNoHandle_OmitsHandleKeys pins the
// mixed-version case: an operator that predates the draft render answers
// artifactRef/digest only, and the tool result must OMIT the handle/
// artifactId keys rather than send the model an empty string it could pass
// straight into artifact_await("").
func TestHandleExportDraft_OperatorAnswersNoHandle_OmitsHandleKeys(t *testing.T) {
	workshopID := "ws-fake0123abcd"
	ac := &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-agent", Namespace: workshopID},
		Spec:       spiceboxv1alpha1.AgentClassSpec{DisplayName: "Demo Agent"},
	}
	c := fake.NewClientBuilder().WithScheme(testfixtures.NewScheme(t)).WithObjects(ac, fixtureWorkshopForApplyTestServer()).Build()
	s := newApplyTestServer(workshopID, c)

	stubOperatorDraftStoreNoHandle(t)

	res, err := s.handleExportDraft(context.Background(), &mcp.CallToolRequest{})
	require.NoError(t, err)
	require.False(t, res.IsError, "a clean export must not be a tool error")
	body := decodeApplyResult(t, res)
	assert.Equal(t, "mem://fake/workshop-draft/deadbeef.oap", body["artifactRef"])
	assert.Equal(t, "deadbeef", body["digest"])
	_, ok := body["handle"]
	assert.False(t, ok, "no handle key when the operator predates the draft render")
	_, ok = body["artifactId"]
	assert.False(t, ok, "no artifactId key when the operator predates the draft render")
}

// TestHandleExportDraft_NoAgentClass_ReturnsClearError: nothing built yet ->
// a clear tool error, never an empty/broken bundle.
func TestHandleExportDraft_NoAgentClass_ReturnsClearError(t *testing.T) {
	workshopID := "ws-fake0123abcd"
	c := fake.NewClientBuilder().WithScheme(testfixtures.NewScheme(t)).WithObjects(fixtureWorkshopForApplyTestServer()).Build()
	s := newApplyTestServer(workshopID, c)

	res, err := s.handleExportDraft(context.Background(), &mcp.CallToolRequest{})
	require.NoError(t, err)
	require.True(t, res.IsError)
	body := decodeApplyResult(t, res)
	assert.Contains(t, body["error"], "nothing to export yet")
}

// TestHandleExportDraft_MultipleAgentClasses_ReturnsClearError: more than one
// agent authored in the workshop is refused, naming both candidates, rather
// than silently exporting whichever one happened to sort first.
func TestHandleExportDraft_MultipleAgentClasses_ReturnsClearError(t *testing.T) {
	workshopID := "ws-fake0123abcd"
	ac1 := &spiceboxv1alpha1.AgentClass{ObjectMeta: metav1.ObjectMeta{Name: "agent-one", Namespace: workshopID}}
	ac2 := &spiceboxv1alpha1.AgentClass{ObjectMeta: metav1.ObjectMeta{Name: "agent-two", Namespace: workshopID}}
	c := fake.NewClientBuilder().WithScheme(testfixtures.NewScheme(t)).WithObjects(ac1, ac2, fixtureWorkshopForApplyTestServer()).Build()
	s := newApplyTestServer(workshopID, c)

	res, err := s.handleExportDraft(context.Background(), &mcp.CallToolRequest{})
	require.NoError(t, err)
	require.True(t, res.IsError)
	body := decodeApplyResult(t, res)
	assert.Contains(t, body["error"], "agent-one")
	assert.Contains(t, body["error"], "agent-two")
}

// TestHandleExportDraft_RosterNamingAStandin_ExcludesTheStandinFromTheBundle
// is the test the agent-builder plan 9b Task-1 fix brief asked for BY NAME,
// not by grep: it exercises the REAL export path (handleExportDraft, not a
// unit test of the ref-walk table alone) against a workshop holding BOTH a
// builder-authored draft AND a rehearsal stand-in it delegates to — the
// exact shape a real workshop using this feature ends up in.
//
// Two facts make the corrected bare-name design (Ruling C,
// pkg/web/workshopprojectsrv's package doc) safe, and this test is what
// confirms the second one BY RUNNING THE CODE, per the fix brief's own
// instruction, rather than trusting the ref-walk table's shape alone:
// pkg/platform/oap/source's refDescriptors (cluster_refs.go) has no row for
// AgentClass.spec.subagents at all, so the export ref-walk never follows a
// roster entry to fetch (and bundle) whatever it names — a stand-in
// projected under the roster's bare name rides along in W but never in the
// exported bytes.
//
// Also proves soleAuthoredClassName's stand-in exclusion (this file's own
// fix, right above) actually lets export SUCCEED with both objects present
// — before that fix, this exact fixture 500'd export outright with "more
// than one agent exists in this workshop".
func TestHandleExportDraft_RosterNamingAStandin_ExcludesTheStandinFromTheBundle(t *testing.T) {
	workshopID := "ws-fake0123abcd"
	draft := &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-agent", Namespace: workshopID},
		Spec: spiceboxv1alpha1.AgentClassSpec{
			DisplayName: "Demo Agent",
			Subagents:   []string{"support-class"},
		},
	}
	// standin is exactly what pkg/web/workshopprojectsrv.projectStandin
	// creates: an ordinary, BARE-named AgentClass in W, carrying the
	// provenance annotation and a stand-in-first description.
	standin := &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "support-class",
			Namespace: workshopID,
			Annotations: map[string]string{
				spiceboxv1alpha1.AnnotationStandinSource: "default/support-class",
			},
		},
		Spec: spiceboxv1alpha1.AgentClassSpec{
			Description:  "STAND-IN for default/support-class — holds no credentials, tools, or authorization; for delegation rehearsal only.",
			SystemPrompt: spiceboxv1alpha1.PromptSource{Inline: "you are a helpful support agent"},
		},
	}
	// status.standins is what ACTUALLY excludes it now (BLOCKER-1, fix round
	// 2) — the annotation above is stamped alongside purely as a
	// human-readable label and is no longer what soleAuthoredClassName
	// trusts; see TestHandleExportDraft_ForgedAnnotationStillCountsAsAuthored
	// below for the break-it-first proof that the annotation ALONE is not
	// enough.
	ws := fixtureWorkshopForApplyTestServer(spiceboxv1alpha1.WorkshopStandin{
		Name: "support-class", SourceNamespace: "default", SourceName: "support-class",
	})
	c := fake.NewClientBuilder().WithScheme(testfixtures.NewScheme(t)).WithObjects(draft, standin, ws).Build()
	s := newApplyTestServer(workshopID, c)

	gotBody := stubOperatorDraftStore(t)

	res, err := s.handleExportDraft(context.Background(), &mcp.CallToolRequest{})
	require.NoError(t, err)
	require.False(t, res.IsError, "export must succeed even though a stand-in coexists with the draft in W")

	require.NotEmpty(t, *gotBody, "storeDraft must have POSTed the packed bundle")
	packed, err := oap.Unpack(*gotBody)
	require.NoError(t, err)
	crs, err := packed.CRs()
	require.NoError(t, err)

	var classes []*unstructured.Unstructured
	for _, cr := range crs {
		if cr.GetKind() == "AgentClass" {
			classes = append(classes, cr)
		}
	}
	require.Len(t, classes, 1, "the export ref-walk does not follow spec.subagents, so the roster-named stand-in must NOT ride along in the bundle")
	assert.Equal(t, "demo-agent", classes[0].GetName(), "only the draft the builder authored is in the bundle")

	subagents, found, err := unstructured.NestedStringSlice(classes[0].Object, "spec", "subagents")
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, []string{"support-class"}, subagents,
		"the roster entry is carried through UNCHANGED — its bare name resolves to the stand-in inside W and to the real class after install, with no rewriting anywhere")
}

// TestHandleExportDraft_ForgedAnnotationStillCountsAsAuthored is BLOCKER-1's
// break-it-first test (i)'s workshopmcp half: a class the workshop's
// builder genuinely authored, carrying a FORGED
// spiceboxv1alpha1.AnnotationStandinSource (a builder's own apply tool can
// server-side-apply arbitrary annotations into W; the admission webhook
// never inspects them), must still be treated as AUTHORED by
// soleAuthoredClassName/export_draft — because Workshop.status.standins,
// the ONLY authority BLOCKER-1 established, never recorded it. Before this
// fix, the mere PRESENCE of the annotation excluded it, so export_draft
// would have reported "nothing to export yet" for a workshop that
// genuinely held authored work — or worse, silently dropped it from a
// multi-candidate error, hiding it from the builder entirely.
func TestHandleExportDraft_ForgedAnnotationStillCountsAsAuthored(t *testing.T) {
	workshopID := "ws-fake0123abcd"
	forged := &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "demo-agent",
			Namespace: workshopID,
			Annotations: map[string]string{
				spiceboxv1alpha1.AnnotationStandinSource: "some-other-namespace/some-other-class",
			},
		},
		Spec: spiceboxv1alpha1.AgentClassSpec{DisplayName: "Genuinely Authored, Forged Marker"},
	}
	// status.standins deliberately EMPTY: nothing was ever actually
	// projected here — the annotation above is a forgery, never recorded.
	ws := fixtureWorkshopForApplyTestServer()
	c := fake.NewClientBuilder().WithScheme(testfixtures.NewScheme(t)).WithObjects(forged, ws).Build()
	s := newApplyTestServer(workshopID, c)

	name, err := s.soleAuthoredClassName(context.Background())
	require.NoError(t, err, "a forged annotation must not make export_draft refuse a genuinely authored class")
	assert.Equal(t, "demo-agent", name, "a forged AnnotationStandinSource must not exclude a class status never recorded as a stand-in")
}

// toUnstructuredCRs converts typed fixture objects into GVK-stamped
// unstructured CRs, mirroring pkg/platform/oap/source/cluster.go's own
// toSanitizedUnstructured (unexported there) for the small, fixed set of
// kinds this test package's fixtures use.
func toUnstructuredCRs(t *testing.T, objs ...client.Object) []*unstructured.Unstructured {
	t.Helper()
	out := make([]*unstructured.Unstructured, 0, len(objs))
	for _, o := range objs {
		var kind string
		switch o.(type) {
		case *spiceboxv1alpha1.AgentClass:
			kind = "AgentClass"
		case *spiceboxv1alpha1.SpiceboxToolspec:
			kind = "SpiceboxToolspec"
		default:
			t.Fatalf("toUnstructuredCRs: unhandled fixture type %T", o)
		}
		o.GetObjectKind().SetGroupVersionKind(spiceboxv1alpha1.SchemeGroupVersion.WithKind(kind))
		m, err := runtime.DefaultUnstructuredConverter.ToUnstructured(o)
		require.NoError(t, err)
		out = append(out, &unstructured.Unstructured{Object: m})
	}
	return out
}

// buildFixtureOapBytes packs a minimal two-object .oap: an AgentClass
// referencing a SpiceboxToolspec, BOTH already unprefixed — exactly the
// shape a real stored draft has, since export_draft strips the workshop
// prefix before ever storing it. load_draft's job is to re-key this back to
// whichever workshop it is currently running in.
func buildFixtureOapBytes(t *testing.T) []byte {
	t.Helper()
	ac := &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: "resumed-agent"},
		Spec: spiceboxv1alpha1.AgentClassSpec{
			DisplayName: "Resumed Agent",
			ToolBundles: []spiceboxv1alpha1.ToolBundle{
				{Name: "tools", Toolspecs: []string{"mytool"}},
			},
		},
	}
	ts := &spiceboxv1alpha1.SpiceboxToolspec{
		ObjectMeta: metav1.ObjectMeta{Name: "mytool"},
		Spec: spiceboxv1alpha1.SpiceboxToolspecSpec{
			Name:             "mytool",
			Toolkit:          spiceboxv1alpha1.ToolspecToolkitRef{Name: "cat", Revision: "2026-04-25"},
			AllowSubcommands: []string{"run"},
		},
	}

	manifests, err := marshalCRStream(toUnstructuredCRs(t, ac, ts))
	require.NoError(t, err)

	bundle := &oap.Bundle{
		Manifest: &oap.Manifest{
			OapFormatVersion: "1",
			Agent:            oap.Agent{Name: "resumed-agent", Version: "0.1.0"},
		},
		Manifests: manifests,
		Assets:    map[string][]byte{},
	}
	packed, err := oap.Pack(bundle)
	require.NoError(t, err)
	return packed
}

// TestHandleLoadDraft_AppliesEveryObjectViaApplyCR_ReturnsOneSummary is the
// KEY assertion for load_draft: every object in the bundle is actually
// re-applied through applyCR (proven by fetching them back from the fake
// client afterward), the cluster-scoped toolspec is RE-PREFIXED for the
// CURRENT workshop (and its cross-ref on the AgentClass follows), and the
// tool returns exactly ONE summary for the whole bundle, never one per
// object.
func TestHandleLoadDraft_AppliesEveryObjectViaApplyCR_ReturnsOneSummary(t *testing.T) {
	shrinkApplyPollForTest(t)
	workshopID := "ws-fresh98765432"
	c := fake.NewClientBuilder().WithScheme(testfixtures.NewScheme(t)).Build()
	s := newApplyTestServer(workshopID, c)

	raw, err := json.Marshal(loadDraftArgs{Oap: buildFixtureOapBytes(t)})
	require.NoError(t, err)

	res, err := s.handleLoadDraft(context.Background(), &mcp.CallToolRequest{
		Params: &mcp.CallToolParamsRaw{Arguments: raw},
	})
	require.NoError(t, err)
	require.False(t, res.IsError, "a clean load must not be a tool error")
	body := decodeApplyResult(t, res)

	summary, ok := body["summary"].(string)
	require.True(t, ok, "result must carry a single summary STRING, not one entry per object")
	assert.NotEmpty(t, summary)
	assert.Equal(t, float64(2), body["objects"], "both bundled objects were applied")

	var gotClass spiceboxv1alpha1.AgentClass
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: workshopID, Name: "resumed-agent"}, &gotClass),
		"the AgentClass must have been applied into the CURRENT workshop namespace")

	var gotToolspec spiceboxv1alpha1.SpiceboxToolspec
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: workshopID + "-mytool"}, &gotToolspec),
		"the toolspec must have been re-prefixed for THIS workshop before being applied")
	assert.Equal(t, workshopID, gotToolspec.Labels[spiceboxv1alpha1.LabelWorkshopNamespace],
		"LabelWorkshopNamespace must be re-added, naming the current workshop")

	require.Len(t, gotClass.Spec.ToolBundles, 1)
	assert.Equal(t, []string{workshopID + "-mytool"}, gotClass.Spec.ToolBundles[0].Toolspecs,
		"the applied AgentClass's own toolspecs ref must follow the re-prefix")
}

// TestHandleLoadDraft_RefusesAdmissionDenial_SurfacesVerbatim proves a
// denial mid-bundle surfaces exactly as workshop_apply's own denial does —
// never re-worded, never claimed as a partial success.
func TestHandleLoadDraft_RefusesAdmissionDenial_SurfacesVerbatim(t *testing.T) {
	workshopID := "ws-fresh98765432"
	denyErr := apierrors.NewForbidden(
		schema.GroupResource{Group: spiceboxv1alpha1.GroupName, Resource: "agentclasses"},
		"resumed-agent", errors.New("admission webhook \"validate-workshop-object\" denied the request: workshop policy forbids this"))

	c := fake.NewClientBuilder().WithScheme(testfixtures.NewScheme(t)).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(ctx context.Context, cl client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				return denyErr
			},
		}).Build()
	s := newApplyTestServer(workshopID, c)

	raw, err := json.Marshal(loadDraftArgs{Oap: buildFixtureOapBytes(t)})
	require.NoError(t, err)

	res, err := s.handleLoadDraft(context.Background(), &mcp.CallToolRequest{
		Params: &mcp.CallToolParamsRaw{Arguments: raw},
	})
	require.NoError(t, err)
	require.True(t, res.IsError, "a denied load must be an error result")
	body := decodeApplyResult(t, res)
	assert.Equal(t, true, body["denied"])
	assert.Equal(t, denyErr.Error(), body["message"], "the apiserver's own denial text must be surfaced VERBATIM")
}

// TestHandleLoadDraft_MissingOapArg_ReturnsClearError.
func TestHandleLoadDraft_MissingOapArg_ReturnsClearError(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testfixtures.NewScheme(t)).Build()
	s := newApplyTestServer("ws-fake0123abcd", c)

	res, err := s.handleLoadDraft(context.Background(), &mcp.CallToolRequest{})
	require.NoError(t, err)
	require.True(t, res.IsError)
	body := decodeApplyResult(t, res)
	assert.Contains(t, body["error"], "oap is required")
}
