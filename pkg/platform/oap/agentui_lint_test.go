package oap

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/tool/mcp"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/tools/mcp/probe"
)

// countContaining returns how many Findings' String() contains substr. Used
// instead of asserting an exact total finding count, so a fixture that
// legitimately produces more than one finding still pins exactly one occurrence
// of the substring under test — the diagnostic property this table exists for.
func countContaining(findings []Finding, substr string) int {
	n := 0
	for _, f := range findings {
		if strings.Contains(f.String(), substr) {
			n++
		}
	}
	return n
}

// TestLintAgentUIsCatchesTheWaysTheThreeCRsCanDisagree is table-driven over
// pkg/platform/oap/testdata/agentui-lint/<dir>: each fixture is the "ok" bundle with
// exactly one authoring mistake introduced, so a fixture that fails for an
// unrelated reason would make its row meaningless. All names are fabricated
// (demo-console, demo-console-ui, demo-crm, prefix "demo") and distinct from
// any bundled example this repo ships.
func TestLintAgentUIsCatchesTheWaysTheThreeCRsCanDisagree(t *testing.T) {
	cases := []struct {
		dir      string
		wantNone bool
		want     string // substring that must appear in EXACTLY one Finding
	}{
		{dir: "ok", wantNone: true},
		{dir: "prefix-typo", want: `"deemo_list_rows"`},           // spec.tools names a prefix no AgentClass declares
		{dir: "not-granted", want: "not in the deployment grant"}, // absent from grantedTools
		{dir: "action-tool-unrequested", want: "Add each to spec.tools"},
		{dir: "visibility-unset", want: "app-visible"},               // tool would go to the LLM, never the browser
		{dir: "visibility-both", want: "app-visible"},                // app+model is NOT app-only either
		{dir: "app-tools-not-enabled", want: "mcpUiAppTools"},        // origin never opted in
		{dir: "allowed-fields-missing-key", want: `"stage"`},         // template sends a key the CR denies
		{dir: "allowed-fields-empty", want: "denies every argument"}, // empty is not allow-all
		{dir: "form-field-not-an-input", want: "which action"},       // a form field naming no action
		{dir: "upstream-tool-absent", want: "does not expose"},       // MCPServer.spec.tools has no such name
		{dir: "external-origin", want: "not carried by this bundle"}, // informational, still reported
		// A bundle with no AgentClass is rejected at load now (the one-class
		// rule — see TestBundleValidate_RejectsZeroOrMultipleAgentClasses), so it
		// can never reach LintAgentUIs; there is no "no-agentclass" fixture.
	}

	for _, tc := range cases {
		t.Run(tc.dir, func(t *testing.T) {
			b, err := FromFolder(filepath.Join("testdata", "agentui-lint", tc.dir))
			require.NoError(t, err, "FromFolder(%s)", tc.dir)

			findings, err := LintAgentUIs(b)
			require.NoError(t, err, "LintAgentUIs(%s)", tc.dir)

			if tc.wantNone {
				assert.Empty(t, findings, "findings: %v", findings)
				return
			}
			assert.Equal(t, 1, countContaining(findings, tc.want),
				"want exactly one finding containing %q; got findings: %v", tc.want, findings)
		})
	}
}

// visibilityFixtureManifests returns a minimal 3-CR bundle manifest stream
// (MCPServer + AgentClass + AgentUI) whose one MCP tool's `visibility` is
// spliced in verbatim as visYAML ("" leaves the key entirely unset, decoding to
// nil — as if a bundle author never wrote it). Trimmed to exactly what the
// spanning visibility test needs: no args template, no allowedFields, so the
// ONLY lint-only finding this fixture can produce is the app-visible one. Any
// other finding means the fixture itself is broken, not that visibility
// disagrees.
func visibilityFixtureManifests(visYAML string) []byte {
	return []byte(fmt.Sprintf(`
apiVersion: agentprimitives.authzed.com/v1alpha1
kind: MCPServer
metadata:
  name: demo-crm
spec:
  name: demo-crm
  version: v1
  intent: "Fixture MCP server for the visibility-parity spanning test."
  server:
    url: "http://127.0.0.1:1"
    transport: streamable-http
  mcpUiAppTools:
    enabled: true
  tools:
    - name: list_rows
      intent: "List demo rows."
      %s
      permission:
        stateImpact: readonly
---
apiVersion: agentprimitives.authzed.com/v1alpha1
kind: AgentClass
metadata:
  name: demo-console
spec:
  description: "Fixture AgentClass for the visibility-parity spanning test."
  systemPrompt:
    inline: "You are a fixture agent used only by this test."
  mcpServers:
    - name: demo
      ref: demo-crm
  agentUI:
    ref: demo-console-ui
    grantedTools: [demo_list_rows]
---
apiVersion: agentprimitives.authzed.com/v1alpha1
kind: AgentUI
metadata:
  name: demo-console-ui
spec:
  displayName: Demo console
  tools: [demo_list_rows]
  slots:
    - name: main
      default:
        component: ap:table
        props:
          columns:
            - {key: label, header: Label}
        bindings:
          rows:
            source: tool
            ref: demo_list_rows
`, visYAML))
}

// TestAppVisibilityMeansTheSameThingToTheLintAndTheRuntime spans two packages:
// it proves the lint's appVisible predicate (agentui_lint.go) and the runtime
// split mcp.Synthesize applies agree, by deriving BOTH verdicts from the SAME
// fixture Visibility value on every row rather than from two hand-picked cases
// that could each pass while the copies drifted apart.
//
// Every row passes on the unmodified tree; that is the point. The two packages
// carry independent COPIES of the same boolean expression
// (Contains("app") && !Contains("model")) with no shared symbol between them,
// so today's agreement is coincidental — this test is what makes a future edit
// to either copy alone fail loudly instead of drifting silently.
func TestAppVisibilityMeansTheSameThingToTheLintAndTheRuntime(t *testing.T) {
	cases := []struct {
		name       string
		visibility []string // nil for "unset"; passed to the RUNTIME half verbatim
		visYAML    string   // spliced into the LINT half's fixture; "" omits the key
		wantApp    bool     // both the lint and the runtime must agree on this
	}{
		{name: "unset -> model-visible only", visibility: nil, visYAML: "", wantApp: false},
		{name: "[] -> model-visible only", visibility: []string{}, visYAML: "visibility: []", wantApp: false},
		{name: `["app"] -> browser-callable`, visibility: []string{"app"}, visYAML: "visibility: [app]", wantApp: true},
		{name: `["model"] -> model-visible only`, visibility: []string{"model"}, visYAML: "visibility: [model]", wantApp: false},
		{name: `["app","model"] -> model-visible only, NOT browser-callable`, visibility: []string{"app", "model"}, visYAML: "visibility: [app, model]", wantApp: false},
		{name: `["app","unrecognized"] -> browser-callable (unrecognized entries are ignored, same as ["app"])`, visibility: []string{"app", "unrecognized"}, visYAML: "visibility: [app, unrecognized]", wantApp: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// (a) the lint's verdict: run the real LintAgentUIs over a bundle
			// whose MCPServerTool.Visibility is tc.visibility (spliced in as YAML).
			b := &Bundle{Manifests: visibilityFixtureManifests(tc.visYAML)}
			findings, err := LintAgentUIs(b)
			require.NoError(t, err, "LintAgentUIs")
			notAppVisible := countContaining(findings, "is not app-visible")
			require.LessOrEqual(t, notAppVisible, 1, "at most one app-visible finding; findings: %v", findings)
			lintSaysApp := notAppVisible == 0
			assert.Equal(t, tc.wantApp, lintSaysApp, "lint verdict; findings: %v", findings)
			// No OTHER lint-only finding may appear — a fixture producing one
			// would mean this row is testing something other than visibility.
			assert.Empty(t, countContaining(findings, "mcpUiAppTools")+countContaining(findings, "allowedFields"),
				"the fixture must be clean apart from the app-visible finding under test; findings: %v", findings)

			// (b) the runtime's verdict: run the real mcp.Synthesize over an
			// MCPServer spec carrying the SAME value, opt-in enabled, and check
			// whether the tool lands in AppTools.
			cr := &spiceboxv1alpha1.MCPServer{}
			cr.Name = "demo-crm"
			cr.Spec.Server.URL = "https://x"
			cr.Spec.MCPUIAppTools = &spiceboxv1alpha1.MCPUIAppToolsSpec{Enabled: true}
			cr.Spec.Tools = []spiceboxv1alpha1.MCPServerTool{
				{Name: "list_rows", Visibility: tc.visibility, Permission: &authz.Permission{StateImpact: authz.Readonly}},
			}
			live := []probe.Tool{{Name: "list_rows"}}
			res, err := mcp.Synthesize(cr, live)
			require.NoError(t, err, "Synthesize")
			runtimeSaysApp := len(res.AppTools) > 0

			assert.Equal(t, tc.wantApp, runtimeSaysApp, "runtime verdict")
			assert.Equal(t, lintSaysApp, runtimeSaysApp, "the lint and the runtime must agree with EACH OTHER, not just with wantApp")
		})
	}
}

// TestLintAgentUIsExternalOriginFindingIsInformational pins that the ONE
// lint-only finding that is advisory rather than a hard error — an AgentUI
// naming an origin the bundle legitimately expects to find pre-installed on
// the target cluster — is actually marked Informational, since cmd/oap's
// wiring depends on that flag (not just the message text) to decide whether
// to fail the command.
func TestLintAgentUIsExternalOriginFindingIsInformational(t *testing.T) {
	b, err := FromFolder(filepath.Join("testdata", "agentui-lint", "external-origin"))
	require.NoError(t, err)

	findings, err := LintAgentUIs(b)
	require.NoError(t, err)
	require.NotEmpty(t, findings)

	for _, f := range findings {
		if strings.Contains(f.Reason, "not carried by this bundle") {
			assert.True(t, f.Informational, "finding: %v", f)
			return
		}
	}
	t.Fatal("no finding mentioned \"not carried by this bundle\"")
}

// TestFindingString pins Finding.String()'s exact rendering, since
// cmd/oap's `agent lint` prints it verbatim as author-facing output.
func TestFindingString(t *testing.T) {
	f := Finding{UI: "demo-console-ui", Path: "spec.tools", Reason: "something is wrong"}
	assert.Equal(t, "AgentUI/demo-console-ui spec.tools: something is wrong", f.String())
	assert.Equal(t, fmt.Sprintf("AgentUI/%s %s: %s", f.UI, f.Path, f.Reason), f.String())
}

// TestLintAgentUIsNilBundle pins the fail-loud contract: a caller passing a
// nil bundle gets an error, never a clean-looking nil slice.
func TestLintAgentUIsNilBundle(t *testing.T) {
	findings, err := LintAgentUIs(nil)
	require.Error(t, err)
	assert.Nil(t, findings)
}

// TestLintAgentUIsNoAgentUI pins that a bundle carrying no AgentUI at all
// (an ordinary bundle with no UI) lints clean rather than erroring — LintAgentUIs
// has nothing to check, which is a legitimate, common case, not a defect.
func TestLintAgentUIsNoAgentUI(t *testing.T) {
	b, err := FromFolder(filepath.Join("testdata", "agentui-lint", "no-agentui"))
	require.NoError(t, err)

	findings, err := LintAgentUIs(b)
	require.NoError(t, err)
	assert.Empty(t, findings)
}
