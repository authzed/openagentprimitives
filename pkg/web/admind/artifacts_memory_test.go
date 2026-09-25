package admind_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/go-logr/logr/testr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/platform/artifacts"
	"github.com/authzed/openagentprimitives/pkg/web/admind"
)

// --- helpers --------------------------------------------------------------

func newTestAdmindWithMem(t *testing.T, k8s client.Client, mem *memory.Local) *admind.Admind {
	t.Helper()
	a, err := admind.New(admind.Config{
		Mem:     mem,
		K8s:     k8s,
		Checker: stubChecker{allow: map[string]bool{"YWRtaW4": true}},
		Token:   "test-token",
		Logger:  testr.New(t),
	})
	require.NoError(t, err)
	return a
}

// artifactRenderCR builds an ArtifactRender. When sessionName is non-empty it
// stamps the controller owner reference the runner sets at create time;
// artifactID, when non-empty, sets the logical-artifact grouping label.
func artifactRenderCR(ns, name, sessionName, artifactID, kind, phase string) *v1alpha1.ArtifactRender {
	ar := &v1alpha1.ArtifactRender{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec:       v1alpha1.ArtifactRenderSpec{Kind: kind},
		Status: v1alpha1.ArtifactRenderStatus{
			Phase:      v1alpha1.ArtifactRenderPhase(phase),
			OutputMIME: "text/html",
			OutputSize: 2048,
		},
	}
	if artifactID != "" {
		ar.Labels = map[string]string{artifacts.LabelArtifactID: artifactID}
	}
	if sessionName != "" {
		tval := true
		ar.OwnerReferences = []metav1.OwnerReference{{
			APIVersion: v1alpha1.SchemeBuilder.GroupVersion.String(), Kind: "AgentSession",
			Name: sessionName, UID: types.UID("uid-" + sessionName), Controller: &tval,
		}}
	}
	return ar
}

func memEntry(scopeID, kind, id string) memory.Entry {
	return memory.Entry{
		Scope: memory.Scope{Kind: "session", ID: scopeID},
		Kind:  kind, ID: id,
		Content: json.RawMessage(`{"toolName":"bash"}`),
	}
}

// --- T32 Artifacts --------------------------------------------------------

func TestAdmindArtifacts(t *testing.T) {
	// Two renders of one logical artifact (owned by s1) + one render owned by s2.
	a1 := artifactRenderCR("default", "ar-s1-aaa", "s1", "art-1", "html", "Ready")
	a2 := artifactRenderCR("default", "ar-s1-bbb", "s1", "art-1", "html", "Failed")
	a3 := artifactRenderCR("default", "ar-s2-ccc", "s2", "art-2", "image", "Rendering")
	k8s := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(a1, a2, a3).Build()
	a := newTestAdmind(t, k8s)
	h := a.Handler()

	// view_audit gates it: a non-admin subject is forbidden.
	w := do(t, h, http.MethodGet, "/admin/v1/artifacts", "test-token", "user:bm9wZQ", "")
	assert.Equal(t, http.StatusForbidden, w.Code, "artifacts needs view_audit")

	w = do(t, h, http.MethodGet, "/admin/v1/artifacts", "test-token", "user:YWRtaW4", "")
	require.Equal(t, http.StatusOK, w.Code)

	var rows []struct {
		Name      string `json:"name"`
		Namespace string `json:"namespace"`
		Session   string `json:"session"`
		Kind      string `json:"kind"`
		Phase     string `json:"phase"`
		MIME      string `json:"mime"`
		Size      int64  `json:"size"`
		Revisions int    `json:"revisions"`
		Created   string `json:"created"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &rows))
	require.Len(t, rows, 3)

	byName := map[string]int{}
	for i, r := range rows {
		byName[r.Name] = i
	}
	r1 := rows[byName["ar-s1-aaa"]]
	assert.Equal(t, "default/s1", r1.Session, "owner AgentSession ref → ns/name")
	assert.Equal(t, "html", r1.Kind)
	assert.Equal(t, "Ready", r1.Phase, "status.phase surfaces")
	assert.Equal(t, "text/html", r1.MIME)
	assert.Equal(t, int64(2048), r1.Size)
	assert.Equal(t, 2, r1.Revisions, "two renders share artifact-id label art-1")

	r3 := rows[byName["ar-s2-ccc"]]
	assert.Equal(t, "default/s2", r3.Session)
	assert.Equal(t, "Rendering", r3.Phase)
	assert.Equal(t, 1, r3.Revisions, "art-2 has a single render")
}

func TestAdmindArtifacts_Empty(t *testing.T) {
	a := newTestAdmind(t, fake.NewClientBuilder().WithScheme(newScheme(t)).Build())
	w := do(t, a.Handler(), http.MethodGet, "/admin/v1/artifacts", "test-token", "user:YWRtaW4", "")
	require.Equal(t, http.StatusOK, w.Code)
	var rows []any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &rows))
	assert.Empty(t, rows, "no ArtifactRenders → empty array, not null/500")
}

// artifactDetailResp mirrors the GET /admin/v1/artifacts/{ns}/{name} payload.
type artifactDetailResp struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	Session   string `json:"session"`
	Kind      string `json:"kind"`
	Phase     string `json:"phase"`
	MIME      string `json:"mime"`
	Size      int64  `json:"size"`
	Created   string `json:"created"`
	Revisions []struct {
		Name    string `json:"name"`
		Phase   string `json:"phase"`
		MIME    string `json:"mime"`
		Size    int64  `json:"size"`
		Created string `json:"created"`
	} `json:"revisions"`
	ViewPath  string `json:"viewPath"`
	OutputRef string `json:"outputRef"`
}

func TestAdmindArtifactDetail(t *testing.T) {
	// Two renders of one logical artifact (art-1) with distinct create times, plus
	// an unrelated art-2 render that must NOT bleed into art-1's revisions.
	older := artifactRenderCR("default", "ar-s1-old", "s1", "art-1", "html", "Ready")
	older.CreationTimestamp = metav1.NewTime(time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC))
	older.Status.OutputRef = "artifactstore://ref-old"
	newer := artifactRenderCR("default", "ar-s1-new", "s1", "art-1", "html", "Rendering")
	newer.CreationTimestamp = metav1.NewTime(time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC))
	other := artifactRenderCR("default", "ar-s2-other", "s2", "art-2", "image", "Ready")

	k8s := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(older, newer, other).Build()
	a := newTestAdmind(t, k8s)
	h := a.Handler()

	// view_audit gates it: a non-admin subject is forbidden.
	w := do(t, h, http.MethodGet, "/admin/v1/artifacts/default/ar-s1-old", "test-token", "user:bm9wZQ", "")
	assert.Equal(t, http.StatusForbidden, w.Code, "artifact detail needs view_audit")

	// Unknown artifact → 404.
	w = do(t, h, http.MethodGet, "/admin/v1/artifacts/default/missing", "test-token", "user:YWRtaW4", "")
	assert.Equal(t, http.StatusNotFound, w.Code, "unknown artifact → 404")

	// Detail for the older render surfaces its own metadata + BOTH art-1 renders
	// as revisions, newest-first, excluding the art-2 render.
	w = do(t, h, http.MethodGet, "/admin/v1/artifacts/default/ar-s1-old", "test-token", "user:YWRtaW4", "")
	require.Equal(t, http.StatusOK, w.Code)

	var d artifactDetailResp
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &d))
	assert.Equal(t, "ar-s1-old", d.Name)
	assert.Equal(t, "default", d.Namespace)
	assert.Equal(t, "default/s1", d.Session, "owner AgentSession ref → ns/name")
	assert.Equal(t, "html", d.Kind)
	assert.Equal(t, "Ready", d.Phase)
	assert.Equal(t, "text/html", d.MIME)
	assert.Equal(t, int64(2048), d.Size)
	assert.Equal(t, "artifactstore://ref-old", d.OutputRef, "status.outputRef surfaces for download")
	assert.Equal(t, "/artifact-view?artifactId=art-1&sessionRef=default%2Fs1", d.ViewPath,
		"admin viewer path carries the logical artifact id (label) + owning-session ref as unsigned params")

	require.Len(t, d.Revisions, 2, "both art-1 renders are revisions; art-2 excluded")
	assert.Equal(t, "ar-s1-new", d.Revisions[0].Name, "newest revision first")
	assert.Equal(t, "Rendering", d.Revisions[0].Phase)
	assert.Equal(t, "ar-s1-old", d.Revisions[1].Name, "older revision second")
}

func TestAdmindArtifactDetail_Unlabeled(t *testing.T) {
	// An ArtifactRender with no artifact-id label is its own sole revision.
	solo := artifactRenderCR("default", "ar-solo", "s1", "", "html", "Ready")
	k8s := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(solo).Build()
	a := newTestAdmind(t, k8s)

	w := do(t, a.Handler(), http.MethodGet, "/admin/v1/artifacts/default/ar-solo", "test-token", "user:YWRtaW4", "")
	require.Equal(t, http.StatusOK, w.Code)
	var d artifactDetailResp
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &d))
	require.Len(t, d.Revisions, 1, "unlabeled render is its own sole revision")
	assert.Equal(t, "ar-solo", d.Revisions[0].Name)
	assert.Equal(t, "/artifact-view?artifactId=ar-solo&sessionRef=default%2Fs1", d.ViewPath,
		"unlabeled render's viewer path falls back to the render name as the artifact id")
}

// --- T33 Memory browser ---------------------------------------------------

func TestAdmindMemoryRollups(t *testing.T) {
	a := newTestAdmind(t, fake.NewClientBuilder().WithScheme(newScheme(t)).Build())
	// Three approval entries across two distinct session scopes.
	for _, e := range []memory.Entry{
		memEntry("default/s1", "approval", "approval-1"),
		memEntry("default/s1", "approval", "approval-2"),
		memEntry("default/s2", "approval", "approval-3"),
	} {
		_, err := a.Memory().Put(context.Background(), e)
		require.NoError(t, err)
	}
	h := a.Handler()

	// view_audit gates it.
	w := do(t, h, http.MethodGet, "/admin/v1/memory", "test-token", "user:bm9wZQ", "")
	assert.Equal(t, http.StatusForbidden, w.Code, "memory needs view_audit")

	w = do(t, h, http.MethodGet, "/admin/v1/memory", "test-token", "user:YWRtaW4", "")
	require.Equal(t, http.StatusOK, w.Code)

	var resp struct {
		Rollups []struct {
			Kind       string `json:"kind"`
			Entries    int    `json:"entries"`
			Scopes     int    `json:"scopes"`
			LastWrite  string `json:"lastWrite"`
			AppendOnly bool   `json:"appendOnly"`
		} `json:"rollups"`
		Entries   []map[string]any `json:"entries"`
		Truncated bool             `json:"truncated"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Len(t, resp.Rollups, 1, "only the approval kind has entries")
	roll := resp.Rollups[0]
	assert.Equal(t, "approval", roll.Kind)
	assert.Equal(t, 3, roll.Entries, "three approval entries")
	assert.Equal(t, 2, roll.Scopes, "across two distinct scopes")
	assert.True(t, roll.AppendOnly, "approval kind is append-only")
	assert.Empty(t, resp.Entries, "no ?kind= → no entry list")
}

func TestAdmindMemoryKindDrillIn(t *testing.T) {
	a := newTestAdmind(t, fake.NewClientBuilder().WithScheme(newScheme(t)).Build())
	for _, e := range []memory.Entry{
		memEntry("default/s1", "approval", "approval-1"),
		memEntry("default/s2", "approval", "approval-2"),
	} {
		_, err := a.Memory().Put(context.Background(), e)
		require.NoError(t, err)
	}

	w := do(t, a.Handler(), http.MethodGet, "/admin/v1/memory?kind=approval", "test-token", "user:YWRtaW4", "")
	require.Equal(t, http.StatusOK, w.Code)
	var resp struct {
		Rollups []map[string]any `json:"rollups"`
		Entries []struct {
			Kind  string `json:"kind"`
			Scope string `json:"scope"`
			ID    string `json:"id"`
		} `json:"entries"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Len(t, resp.Entries, 2, "?kind= returns that kind's recent entries")
	for _, e := range resp.Entries {
		assert.Equal(t, "approval", e.Kind)
	}
}

// stubSearcher is a fixed-result Searcher for the ?q= path.
type stubSearcher struct{ entries []memory.ScoredEntry }

func (s stubSearcher) Search(context.Context, memory.SearchRequest) (memory.MergedSearchResult, error) {
	return memory.MergedSearchResult{Entries: s.entries}, nil
}

func TestAdmindMemorySearch(t *testing.T) {
	hits := []memory.ScoredEntry{
		{Entry: memEntry("default/s1", "approval", "approval-1"), Score: 0.91, Source: "postgres"},
		{Entry: memEntry("default/s2", "transcript", "transcript-7"), Score: 0.42, Source: "postgres"},
	}
	mem := memory.NewLocal(inmem.NewBackend(), memory.WithSearcher(stubSearcher{entries: hits}))
	a := newTestAdmindWithMem(t, fake.NewClientBuilder().WithScheme(newScheme(t)).Build(), mem)

	w := do(t, a.Handler(), http.MethodGet, "/admin/v1/memory?q=deploy", "test-token", "user:YWRtaW4", "")
	require.Equal(t, http.StatusOK, w.Code)
	var resp struct {
		Entries []struct {
			Kind  string  `json:"kind"`
			Scope string  `json:"scope"`
			ID    string  `json:"id"`
			Score float64 `json:"score"`
		} `json:"entries"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Len(t, resp.Entries, 2, "search returns ranked hits")
	assert.Equal(t, "approval", resp.Entries[0].Kind)
	assert.Equal(t, "default/s1", resp.Entries[0].Scope)
	assert.InDelta(t, 0.91, resp.Entries[0].Score, 0.001, "relevance score surfaces")
}

func TestAdmindMemorySearch_NoProvidersIsEmptyNot500(t *testing.T) {
	// newTestAdmind builds a searcher-less facade — ?q= must degrade to an empty
	// result, not a 500 (search is an optional capability).
	a := newTestAdmind(t, fake.NewClientBuilder().WithScheme(newScheme(t)).Build())
	w := do(t, a.Handler(), http.MethodGet, "/admin/v1/memory?q=anything", "test-token", "user:YWRtaW4", "")
	require.Equal(t, http.StatusOK, w.Code)
	var resp struct {
		Entries []any `json:"entries"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Empty(t, resp.Entries)
}
