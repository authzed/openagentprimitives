package sessions

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/web/webui"
)

// goldenShellPropsPath is the ONE artifact both halves of the /sessions
// Go->React bootstrap seam read. This file asserts shellPageBuild's own
// output marshals to it; ui/SessionShell.test.tsx imports the same file and
// (a) renders it through the REAL SessionShell — the viewer's identity, the
// session-origin disclosure, both halves of the sibling-view switch and their
// hrefs, the selected view's declaration, and the requested chrome state all
// come off this file — and
// (b) reads golden.startableClasses and the nested offersAgentUI directly off
// the parsed JSON, so a renamed or dropped key fails the TS suite too. See
// shellProps' own doc comment (page.go) for exactly which protection each
// field gets. See pkg/web/webui/agentui's own props.golden.json
// (props_golden_internal_test.go) for the shape this mirrors and the
// json-tag-rename defect class it exists to catch — a rename here fails this
// test; regenerating the golden to match then fails the TS side instead.
// There is no edit to one side alone that keeps both suites green.
//
// It is a SEPARATE file from props.golden.json on purpose: merging them would
// make an edit to either contract churn both, and the two are produced by
// different builders (shellPageBuild here, ViewFor there).
const goldenShellPropsPath = "ui/testdata/shell.golden.json"

// goldenShellSubjectDisplay/goldenShellSubjectCanonical are a genuinely
// base64-encoded canonical-subject pair — unlike a bare "user:alice" literal,
// whose failed decode falls back to the input unchanged (identity.
// DecodeForDisplay's own doc comment) and so cannot tell a forwarded
// canonical subject apart from a properly decoded one. This pair's canonical
// form decodes to a DIFFERENT string than itself, so the golden can tell the
// two apart.
const goldenShellSubjectDisplay = "alice@example.com"

var goldenShellSubjectCanonical = "user:" + base64.RawURLEncoding.EncodeToString([]byte(goldenShellSubjectDisplay))

// goldenShellUIName is the AgentUI referenced by "demo-agent" (below) — the
// class both golden-listed sessions (alpha, beta) belong to, and the one
// buildGoldenShellProps selects via `?session=demo-ns/alpha`, so the golden
// exercises the list join AND a populated `selected` agent-ui view in one
// fixture.
const goldenShellUIName = "demo-ui"

// goldenShellFixtureDeps builds the SAME kind of fixture list_test.go's
// buildSessionList tests use (newListDeps), populated so every field this
// task fills — Sessions, Notices, StartableClasses, AND Selected — carries
// real, non-empty content in the golden: two joinable sessions (one Active,
// one waiting on the viewer), one lookup hit with no Kubernetes object, one
// unrepresentable id (so notices.unavailable is non-zero too), and an
// AgentClass/AgentUI pair so the selected session (alpha) resolves a real
// agent-ui view rather than falling back to chat — a chat-view selection
// would leave `selected.view.ui`/`.chrome` entirely unpinned, which is
// exactly the gap C2 exists to close. The AgentUI additionally requests
// collapsed chrome, so `selected.chrome.initialState` is pinned NON-empty —
// see I2's own note on the `omitempty` golden trap. See this file's own top
// comment and page.go's shellProps doc comment for why a golden pinning only
// empty values would not be coverage.
func goldenShellFixtureDeps(t *testing.T) *listFixtureDeps {
	t.Helper()
	older := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	newer := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	return newListDeps(t,
		lookupReturns(ref("demo-ns", "alpha"), ref("demo-ns", "beta"), ref("demo-ns", "ghost")),
		lookupUnrepresentable("not-a-valid-ref"),
		// So canStartSessions is pinned TRUE. A golden carrying `false` for a
		// boolean pins nothing — a builder that hardcoded false would match it
		// exactly, which is the same "a golden that pins an absent/zero value is
		// not coverage" trap the chrome.initialState note above records.
		withStartCollaborator(),
		// So startableClasses[].startable is pinned TRUE. Same reasoning as
		// withStartCollaborator directly above: a golden carrying a boolean's
		// zero value pins nothing, and the browser gates the submit on this one.
		withStartableNamespaces("demo-ns"),
		withSession("demo-ns", "alpha", classNamed("demo-agent"), startedAt(newer)), // phase "" -> Active
		withSession("demo-ns", "beta", classNamed("demo-agent"),
			inPhase(spiceboxv1alpha1.AgentSessionPhaseAwaitingApproval), startedAt(older)),
		withObjects(
			&spiceboxv1alpha1.AgentClass{
				ObjectMeta: metav1.ObjectMeta{Namespace: "demo-ns", Name: "demo-agent"},
				Spec: spiceboxv1alpha1.AgentClassSpec{
					DisplayName: "Demo Agent", // withClass's own field — kept so sessions[*].title is unchanged
					AgentUI:     &spiceboxv1alpha1.AgentClassUIGrant{Ref: goldenShellUIName},
				},
			},
			&spiceboxv1alpha1.AgentUI{
				ObjectMeta: metav1.ObjectMeta{Namespace: "demo-ns", Name: goldenShellUIName},
				Spec: spiceboxv1alpha1.AgentUISpec{
					Chrome: &spiceboxv1alpha1.AgentUIChrome{InitialState: "collapsed"},
					Slots: []spiceboxv1alpha1.AgentUISlot{{
						Name:    "root",
						Default: &apiextensionsv1.JSON{Raw: []byte(`{"component":"ap:heading","props":{"text":"Fleet Console","level":1}}`)},
					}},
				},
				Status: spiceboxv1alpha1.AgentUIStatus{Conditions: []metav1.Condition{{
					Type:   spiceboxv1alpha1.AgentUIConditionValid,
					Status: metav1.ConditionTrue,
					Reason: "Test",
				}}},
			},
		),
	)
}

// buildGoldenShellProps runs the REAL Page.Build closure so the golden is
// production output, not a hand-built literal. Deps is a populated fixture
// (goldenShellFixtureDeps), not nil: shellPageBuild calls buildSessionList,
// which reaches LookupInteractableSessions and K8s() unconditionally, so a nil
// Deps here would be an immediate nil-pointer panic rather than a genuine,
// honest value. The request carries `?session=demo-ns/alpha`, so Selected is
// populated — see goldenShellFixtureDeps' own doc comment for why.
func buildGoldenShellProps(t *testing.T) []byte {
	t.Helper()
	build := shellPageBuild(goldenShellFixtureDeps(t))

	r := httptest.NewRequest(http.MethodGet, "/sessions?session=demo-ns/alpha", nil)
	ctx := webui.WithSubjectForTest(context.Background(), goldenShellSubjectCanonical)

	props, _, err := build(ctx, r.WithContext(ctx))
	require.NoError(t, err, "shellPageBuild must not error on its happy path")

	out, err := json.Marshal(props)
	require.NoError(t, err, "props must marshal — the framework marshals them the same way")
	return out
}

func TestShellPageBuild_MatchesGoldenTheBrowserRenders(t *testing.T) {
	got := buildGoldenShellProps(t)
	want, err := os.ReadFile(filepath.Clean(goldenShellPropsPath))
	require.NoError(t, err, "the golden the frontend suite renders must exist")

	assert.JSONEq(t, string(want), string(got),
		"page.go's props no longer match %s, which SessionShell.test.tsx renders", goldenShellPropsPath)
}

// TestShellPageBuild_GoldenCarriesSelected is C2's Go-side half: the golden
// FILE itself (not just shellPageBuild's live output, which the JSONEq test
// above already covers) must carry the `selected` key and its `view.kind` —
// otherwise a regenerated golden that silently dropped Selected (e.g. an
// `omitempty` on a field that should never be empty here) still looks like a
// pin. Mirrors props_golden_internal_test.go's own jsonKeys/mustField
// pattern for the identical reason.
func TestShellPageBuild_GoldenCarriesSelected(t *testing.T) {
	raw, err := os.ReadFile(filepath.Clean(goldenShellPropsPath))
	require.NoError(t, err)

	var obj map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(raw, &obj))
	selectedRaw, ok := obj["selected"]
	require.True(t, ok, "the golden must carry a populated \"selected\" key")

	var selected map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(selectedRaw, &selected))
	viewRaw, ok := selected["view"]
	require.True(t, ok, "selected must carry a \"view\" key")

	var view map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(viewRaw, &view))
	var kind string
	require.NoError(t, json.Unmarshal(view["kind"], &kind))
	assert.Equal(t, "agent-ui", kind)

	// canStartSessions must be carried AND true. A golden holding a boolean's
	// zero value pins nothing: a builder that hardcoded false would match it
	// exactly, and the start control's whole gate reads this field.
	canStartRaw, ok := obj["canStartSessions"]
	require.True(t, ok, "the golden must carry a \"canStartSessions\" key")
	var canStart bool
	require.NoError(t, json.Unmarshal(canStartRaw, &canStart))
	assert.True(t, canStart, "the golden fixture wires a start collaborator, so this must be pinned TRUE, not at its zero value")

	var chrome map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(selected["chrome"], &chrome))
	var initialState string
	require.NoError(t, json.Unmarshal(chrome["initialState"], &initialState))
	assert.Equal(t, "collapsed", initialState, "chrome.initialState must be pinned NON-empty (I2)")
}

// TestRegenerateShellGoldenScratch is a scratch generator, not part of this
// package's real coverage: it exists only so ui/testdata/shell.golden.json
// can be regenerated from shellPageBuild's own real output when the
// bootstrap contract changes. Gated behind REGEN_GOLDEN so it never runs as
// part of the normal suite — mirrors pkg/web/webui/agentui's own
// TestRegenerateGoldensScratch (props_golden_internal_test.go).
func TestRegenerateShellGoldenScratch(t *testing.T) {
	if os.Getenv("REGEN_GOLDEN") == "" {
		t.Skip("set REGEN_GOLDEN=1 to regenerate")
	}
	var buf bytes.Buffer
	require.NoError(t, json.Indent(&buf, buildGoldenShellProps(t), "", "  "))
	buf.WriteByte('\n')
	require.NoError(t, os.WriteFile(goldenShellPropsPath, buf.Bytes(), 0o644))
}
