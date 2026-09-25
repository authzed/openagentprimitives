package agentui

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/web/uicomponents"
)

// goldenBindingsPath is the ONE artifact both halves of the data-binding seam
// read, in the same shape testdata/props.golden.json already pins the
// bootstrap seam. It carries four fields, and every one is load-bearing on
// both sides:
//
//	declaration — the AgentUI page tree this file installs (as spec.view) into
//	              its fixture and the frontend suite renders. Neither side
//	              writes its own copy. It carries a hook AND bindings outside
//	              every hook, so the region half of the binding-path rule is
//	              pinned by the shared artifact rather than by a per-language
//	              literal. Its "data" hook's table's rows binding carries a
//	              `select`, which is what makes this golden ALSO the J2 pin for
//	              "the server extracts, the browser does not": the value that
//	              ends up in `response` below is the value AFTER
//	              pkg/web/uiselect ran, never the raw envelope golden_envelope
//	              answers with.
//	request     — the POST body the browser sends. Decoded here through
//	              bindingsRequestBody (so a json-tag rename yields an empty
//	              params map and fails), asserted byte-for-byte as the body the
//	              browser actually POSTs on the TS side.
//	paths       — uicomponents.WalkBindings' output for `declaration`, in walk
//	              order. Re-derived independently on the TS side through
//	              @ap/agentui's bindingPath.
//	response    — the REAL bindingsHandler's response body for `request`,
//	              AFTER extraction: the two paths in the region outside every
//	              hook carry the ordinary tool-result values selectors don't
//	              touch, and data/0#rows — inside the "data" hook, whose
//	              subtree the region rule numbers from zero again — carries
//	              golden_envelope's `results[].properties`, with
//	              `total`/`paging` gone — exactly the shape resolveOneBinding
//	              produces, never the raw tool result. Fed verbatim to a
//	              stubbed fetch on the TS side and read through useBindings'
//	              BindingsResponse.
//
// TestTheGoldenActuallyCarriesASelector guards the claim above: Binding.Select
// is `omitempty`, so a regenerated golden that silently dropped the field
// would still look like a pin — every other assertion in this file would
// still pass against a `data` hook with no selector at all. Do not trust the
// prose above without that test passing.
//
// # Why this exists
//
// Two seams here were pinned only by two independent copies of the same
// literal, one per language, and a review proved by mutation that a
// coordinated change on the Go side left BOTH suites green while every binding
// in the shipped page stopped resolving:
//
//   - BindingPath's format ("<region>/<dotted node index>#<prop>"). Change the
//     separator in walk.go and sweep the Go test literals with it, and the
//     browser's lookups all miss: every bound prop falls back to its declared
//     placeholder and stays there, on a clean 200, forever.
//   - bindingsResponseBody's json tags and status literals. The Go tests
//     unmarshal the handler's output back into the producing struct, so
//     renaming `value` or `"ok"` round-trips invisibly; in the browser every
//     entry becomes an error card, or — renaming `bindings` — a silent page of
//     placeholders with no error at all.
//
// With this file in place there is no edit to one side alone that keeps both
// suites green: change the Go format and this test fails; regenerate the
// golden to match and the TS rows fail instead.
//
// To change either contract deliberately: update the Go code, this file's
// expectations, the golden, AND the TS mirrors (@ap/agentui's bindingPath,
// ui/useBindings.ts's BindingsResponse) together.
const goldenBindingsPath = "ui/testdata/bindings.golden.json"

// goldenBindings is the golden's own shape. `declaration` and `response` stay
// json.RawMessage: this file must compare the handler's bytes against the
// file's bytes, and decoding either through a Go struct on the way would
// launder exactly the tag renames the comparison exists to catch.
type goldenBindings struct {
	Declaration json.RawMessage `json:"declaration"`
	Request     json.RawMessage `json:"request"`
	Paths       []string        `json:"paths"`
	Response    json.RawMessage `json:"response"`
}

func readBindingsGolden(t *testing.T) goldenBindings {
	t.Helper()
	raw, err := os.ReadFile(filepath.Clean(goldenBindingsPath))
	require.NoError(t, err, "the golden the frontend suite renders must exist")
	var g goldenBindings
	require.NoError(t, json.Unmarshal(raw, &g), "the golden must be well-formed JSON")
	require.NotEmpty(t, g.Paths, "a golden with no bound props would pin nothing")
	return g
}

// goldenBindingsUI builds the AgentUI carrying the golden's own declaration
// as spec.view — the golden's `declaration` IS the page tree — so the fixture
// and the page the browser renders are the same bytes.
func goldenBindingsUI(t *testing.T, g goldenBindings) *spiceboxv1alpha1.AgentUI {
	t.Helper()
	var decl struct {
		View json.RawMessage `json:"view"`
	}
	require.NoError(t, json.Unmarshal(g.Declaration, &decl))
	require.NotEmpty(t, decl.View, "the golden's declaration must carry a view tree")

	return &spiceboxv1alpha1.AgentUI{
		ObjectMeta: metav1.ObjectMeta{Name: bindingsUIName, Namespace: bindingsNS},
		Spec:       spiceboxv1alpha1.AgentUISpec{View: &apiextensionsv1.JSON{Raw: decl.View}},
		Status: spiceboxv1alpha1.AgentUIStatus{Conditions: []metav1.Condition{{
			Type: spiceboxv1alpha1.AgentUIConditionValid, Status: metav1.ConditionTrue, Reason: "Test",
		}}},
	}
}

// TestTheGoldenActuallyCarriesASelector guards against the golden looking
// like a J2 pin without being one. Binding.Select is `omitempty`: a
// regenerated golden that silently dropped the field would still parse,
// still walk to the same paths, and — if response bytes were hand-edited to
// match rather than produced by resolving through a real select — still pass
// TestBindingsResponseMatchesTheGoldenTheBrowserParses. This is the one
// assertion that fails if nobody's `select` field survived a regeneration.
func TestTheGoldenActuallyCarriesASelector(t *testing.T) {
	g := readBindingsGolden(t)

	decl, err := uicomponents.ParseDeclaration(g.Declaration)
	require.NoError(t, err, "the golden's declaration must parse")

	found := false
	for _, b := range uicomponents.WalkBindings(decl) {
		if b.Binding.Select != "" {
			found = true
			break
		}
	}
	require.True(t, found, "%s must carry at least one non-empty Binding.Select, or it pins nothing about extraction", goldenBindingsPath)
}

// TestBindingPathsMatchTheGoldenTheBrowserLooksUp pins the BindingPath format
// itself: the golden's `paths` are what the REAL WalkBindings produces for the
// golden's own declaration, and the TS suite re-derives the same list through
// @ap/agentui's bindingPath. Asserted in walk ORDER, not as a set, because the
// order is documented as deterministic (one tree, props sorted, children
// depth-first) and the TS mirror reproduces it.
func TestBindingPathsMatchTheGoldenTheBrowserLooksUp(t *testing.T) {
	g := readBindingsGolden(t)

	decl, err := uicomponents.ParseDeclaration(g.Declaration)
	require.NoError(t, err, "the golden's declaration must parse")

	got := make([]string, 0, len(g.Paths))
	for _, b := range uicomponents.WalkBindings(decl) {
		got = append(got, b.Path)
	}
	assert.Equal(t, g.Paths, got,
		"BindingPath's format changed: update walk.go, %s, and @ap/agentui's bindingPath together", goldenBindingsPath)
}

// TestBindingsResponseMatchesTheGoldenTheBrowserParses pins the response
// envelope — the json tags AND the status literals — by asserting the real
// handler's bytes against the file the TS suite feeds to a stubbed fetch. It
// also pins the request envelope in the one direction a Go test can: decoding
// the golden's request body through bindingsRequestBody must still yield the
// parameter the declaration declares.
func TestBindingsResponseMatchesTheGoldenTheBrowserParses(t *testing.T) {
	registerFakeToolResolver(t)
	g := readBindingsGolden(t)

	// The request half. Decoding the golden's bytes (not re-marshalling the
	// struct) is what makes a `params` tag rename visible: the field would
	// simply not be populated.
	var body bindingsRequestBody
	require.NoError(t, json.Unmarshal(g.Request, &body), "the golden's request must decode")
	assert.Equal(t, map[string]string{"span": "7d"}, body.Params,
		"bindingsRequestBody's shape changed: update bindings.go, %s, and useBindings.ts together", goldenBindingsPath)

	objs := []client.Object{bindingsFixtureSession(), bindingsFixtureClass(), goldenBindingsUI(t, g)}
	d := &bindingsFakeDeps{interactOK: true, k8s: newBindingsFakeK8s(t, objs...), origin: bindingsOrigin}
	rec := postBindings(t, d, bindingsSubject, bindingsOrigin, string(g.Request))
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	assert.JSONEq(t, string(g.Response), rec.Body.String(),
		"the bindings response envelope changed: update bindings.go, %s, and useBindings.ts's BindingsResponse together", goldenBindingsPath)
}
