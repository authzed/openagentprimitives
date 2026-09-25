package workshopmcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/testfixtures"
	"github.com/authzed/openagentprimitives/pkg/tools/contract"
)

// TestValidateCandidate exercises validateCandidate directly — the shared
// helper handleValidate dispatches to — over the three real kinds and an
// unknown one. Each fixture round-trips through the SAME kind package's own
// ValidateFile the CLI uses (mirroring the fixture shapes each kind's own
// kind_test.go already proves valid/invalid against).
func TestValidateCandidate(t *testing.T) {
	cases := []struct {
		name  string
		kind  string
		body  string
		check func(t *testing.T, diags []contract.Diagnostic)
	}{
		{
			name: "valid SpiceboxToolspec: zero error diagnostics",
			kind: "SpiceboxToolspec",
			body: `
name: s
version: "1"
toolkit: {name: t, revision: "r"}
allowSubcommands: [say]
constraints:
  - cel: 'call.subcommand == "say"'
    message: ok
`,
			check: func(t *testing.T, diags []contract.Diagnostic) {
				assert.Empty(t, diags)
			},
		},
		{
			name: "invalid CEL constraint in SpiceboxToolspec: error diagnostic names constraints[0]",
			kind: "SpiceboxToolspec",
			body: `
name: s
version: "1"
toolkit: {name: t, revision: "r"}
allowSubcommands: []
constraints:
  - cel: 'call.subcommand =='
`,
			check: func(t *testing.T, diags []contract.Diagnostic) {
				require.Len(t, diags, 1)
				assert.Equal(t, "error", diags[0].Severity)
				assert.Contains(t, diags[0].Path, "constraints[0]")
			},
		},
		{
			// allowedFields is part of what makes this candidate valid, not
			// decoration: without it the argument gate refuses every call this
			// tool's own constraints are written to bound.
			name: "valid MCPServer: zero diagnostics",
			kind: "MCPServer",
			body: `
name: ok
version: "1"
server: { url: https://x, transport: streamable-http }
tools:
  - name: t1
    args:
      allowedFields: [q]
      constraints:
        - cel: 'args.q.size() < 100'
          message: 'q must have fewer than 100 items'
`,
			check: func(t *testing.T, diags []contract.Diagnostic) {
				assert.Empty(t, diags)
			},
		},
		{
			name: "malformed CEL constraint in MCPServer: error diagnostic",
			kind: "MCPServer",
			body: `
name: bad
version: "1"
server: { url: https://x, transport: streamable-http }
tools:
  - name: t
    args:
      allowedFields: [q]
      constraints:
        - cel: 'args.q.size( < 100'
`,
			check: func(t *testing.T, diags []contract.Diagnostic) {
				require.Len(t, diags, 1)
				assert.Equal(t, "error", diags[0].Severity)
			},
		},
		{
			name: "valid SidecarToolbox: zero diagnostics",
			kind: "SidecarToolbox",
			body: `
name: fixture-toolbox
version: "1"
source: { image: ghcr.io/fixture/toolbox:v1 }
sandbox: { class: sidecar-sandbox-default }
transport: { port: 8080 }
upstreamAuth: { provider: fixture-app }
tools: []
`,
			check: func(t *testing.T, diags []contract.Diagnostic) {
				assert.Empty(t, diags)
			},
		},
		{
			name: "SidecarToolbox with both source variants: error diagnostic",
			kind: "SidecarToolbox",
			body: `
name: fixture-toolbox
version: "1"
source:
  image: ghcr.io/fixture/toolbox:v1
  inline:
    baseImage: python:3.12-slim
    script: { configMapRef: { name: x, key: y.py } }
    entrypoint: ["python", "/app/y.py"]
sandbox: { class: sidecar-sandbox-default }
transport: { port: 8080 }
upstreamAuth: { provider: fixture-app }
tools: []
`,
			check: func(t *testing.T, diags []contract.Diagnostic) {
				require.Len(t, diags, 1)
				assert.Equal(t, "error", diags[0].Severity)
				assert.Contains(t, diags[0].Message, "exactly one of")
			},
		},
		{
			name: "unknown kind: one error diagnostic naming it, not a silent pass",
			kind: "TotallyMadeUpKind",
			body: `name: x`,
			check: func(t *testing.T, diags []contract.Diagnostic) {
				require.Len(t, diags, 1)
				assert.Equal(t, "error", diags[0].Severity)
				assert.Equal(t, "kind", diags[0].Path)
				assert.Contains(t, diags[0].Message, `"TotallyMadeUpKind"`)
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			diags := validateCandidate(tc.kind, []byte(tc.body))
			tc.check(t, diags)
		})
	}
}

// TestValidateCandidate_EmptyManifest_ErrorDiagnostic covers the empty-body
// edge case validateCandidate must fail closed on before it ever dispatches
// to a kind's ValidateFile (which would otherwise see an empty temp file and
// report its own, less clear, structural error).
func TestValidateCandidate_EmptyManifest_ErrorDiagnostic(t *testing.T) {
	diags := validateCandidate("SpiceboxToolspec", []byte("   \n"))
	require.Len(t, diags, 1)
	assert.Equal(t, "error", diags[0].Severity)
	assert.Equal(t, "manifest", diags[0].Path)
}

// callValidate drives handleValidate the way a real MCP call would: JSON
// arguments over req.Params.Arguments, exactly the flat (non-enveloped)
// shape decodeArgs expects for this leaf MCP server.
func callValidate(t *testing.T, s *Server, kind string, manifest json.RawMessage) *mcp.CallToolResult {
	t.Helper()
	raw, err := json.Marshal(validateArgs{Kind: kind, Manifest: manifest})
	require.NoError(t, err)
	res, err := s.handleValidate(context.Background(), &mcp.CallToolRequest{
		Params: &mcp.CallToolParamsRaw{Arguments: raw},
	})
	require.NoError(t, err, "handleValidate must not return a protocol-level error")
	return res
}

// decodeValidateResult unmarshals a handleValidate result's single text
// content block into a plain map for assertion.
func decodeValidateResult(t *testing.T, res *mcp.CallToolResult) map[string]any {
	t.Helper()
	require.Len(t, res.Content, 1)
	text, ok := res.Content[0].(*mcp.TextContent)
	require.True(t, ok, "result content block must be text")
	var body map[string]any
	require.NoError(t, json.Unmarshal([]byte(text.Text), &body), "result body must be valid JSON")
	return body
}

// TestHandleValidate_ValidManifestAsObject_ReportsValidTrue covers the
// common case: the builder passes the candidate's spec content as a JSON
// object (arrives as raw JSON bytes over the wire, which are already valid
// YAML input for the underlying loader).
func TestHandleValidate_ValidManifestAsObject_ReportsValidTrue(t *testing.T) {
	// A recording client that accepts the dry run: a clean local pass plus a
	// clean server pass is the only way to an empty diagnostics list.
	s, _ := newDryRunServer(t, "ws-demo123", nil)
	manifest, err := json.Marshal(map[string]any{
		"name":             "s",
		"version":          "1",
		"toolkit":          map[string]any{"name": "t", "revision": "r"},
		"allowSubcommands": []string{"say"},
	})
	require.NoError(t, err)

	res := callValidate(t, s, "SpiceboxToolspec", manifest)
	require.False(t, res.IsError, "a structurally valid candidate must not be a protocol-level tool error")
	body := decodeValidateResult(t, res)
	assert.Equal(t, true, body["valid"])
	assert.Empty(t, body["diagnostics"])
}

// TestHandleValidate_ManifestAsYAMLString_UnwrapsAndValidates covers the
// other accepted manifest shape: a raw YAML document passed as a JSON
// string, which manifestBytes must unwrap before validation rather than
// validating the string's own JSON quoting.
func TestHandleValidate_ManifestAsYAMLString_UnwrapsAndValidates(t *testing.T) {
	s := &Server{}
	manifest, err := json.Marshal("name: s\nversion: \"1\"\ntoolkit: {name: t, revision: r}\nallowSubcommands: []\n")
	require.NoError(t, err)

	res := callValidate(t, s, "SpiceboxToolspec", manifest)
	require.False(t, res.IsError)
	body := decodeValidateResult(t, res)
	assert.Equal(t, true, body["valid"])
}

// TestHandleValidate_InvalidCEL_ReportsValidFalseWithDiagnostic proves the
// end-to-end shape a caller actually sees for a bad candidate: not an
// IsError tool failure (the call itself succeeded), but a normal result
// carrying valid=false and the offending diagnostic.
func TestHandleValidate_InvalidCEL_ReportsValidFalseWithDiagnostic(t *testing.T) {
	s := &Server{}
	manifest, err := json.Marshal(map[string]any{
		"name":             "s",
		"version":          "1",
		"toolkit":          map[string]any{"name": "t", "revision": "r"},
		"allowSubcommands": []string{},
		"constraints":      []map[string]any{{"cel": "call.subcommand =="}},
	})
	require.NoError(t, err)

	res := callValidate(t, s, "SpiceboxToolspec", manifest)
	require.False(t, res.IsError, "a diagnostics-carrying result is still a successful tool call")
	body := decodeValidateResult(t, res)
	assert.Equal(t, false, body["valid"])
	diags, ok := body["diagnostics"].([]any)
	require.True(t, ok, "diagnostics must decode as a JSON array")
	require.Len(t, diags, 1)
}

// TestHandleValidate_MissingKind_ToolError covers the malformed-request
// path (no kind at all) — distinct from an unknown kind, which is reported
// as an invalid candidate rather than a protocol error.
func TestHandleValidate_MissingKind_ToolError(t *testing.T) {
	s := &Server{}
	res := callValidate(t, s, "", json.RawMessage(`{"name":"x"}`))
	assert.True(t, res.IsError, "a missing kind argument is a tool error, not a diagnostic")
}

// TestHandleValidate_MissingManifest_ToolError mirrors the missing-kind
// case for the other required argument.
func TestHandleValidate_MissingManifest_ToolError(t *testing.T) {
	s := &Server{}
	res := callValidate(t, s, "SpiceboxToolspec", nil)
	assert.True(t, res.IsError, "a missing manifest argument is a tool error, not a diagnostic")
}

// TestHandleValidate_UnknownKind_ReportsInvalidNotToolError proves the
// fail-closed behavior end to end: an unknown kind never silently passes,
// but it also isn't a protocol-level failure — it's a candidate the caller
// asked about and got told "invalid", same as a bad CEL expression would.
func TestHandleValidate_UnknownKind_ReportsInvalidNotToolError(t *testing.T) {
	s := &Server{}
	res := callValidate(t, s, "NotARealKind", json.RawMessage(`{"name":"x"}`))
	require.False(t, res.IsError)
	body := decodeValidateResult(t, res)
	assert.Equal(t, false, body["valid"])
	diags, ok := body["diagnostics"].([]any)
	require.True(t, ok)
	require.Len(t, diags, 1)
}

// dryRunRecorder is a fake client whose Patch records what validate_spec sent
// and answers with a canned error, so a test can assert the dry run reached
// the apiserver with the right object, the right options, and NOTHING else —
// a dry run that forgot its DryRun option would create the candidate, which
// is exactly the approval-gated write validate_spec exists to spare.
type dryRunRecorder struct {
	obj  *unstructured.Unstructured
	opts client.PatchOptions
	err  error
}

func newDryRunServer(t *testing.T, ns string, answer error) (*Server, *dryRunRecorder) {
	t.Helper()
	rec := &dryRunRecorder{err: answer}
	c := fake.NewClientBuilder().WithScheme(testfixtures.NewScheme(t)).WithInterceptorFuncs(interceptor.Funcs{
		Patch: func(_ context.Context, _ client.WithWatch, obj client.Object, _ client.Patch, opts ...client.PatchOption) error {
			u, ok := obj.(*unstructured.Unstructured)
			require.True(t, ok, "the dry run must patch an unstructured candidate")
			rec.obj = u.DeepCopy()
			rec.opts = client.PatchOptions{}
			rec.opts.ApplyOptions(opts)
			return rec.err
		},
	}).Build()
	return &Server{K8s: c, Identity: WorkshopIdentity{Namespace: ns}, FieldOwner: defaultFieldOwner}, rec
}

// The live loop on oap-desktop 2026-09-13: AgentIdentity has no local
// validator, so the builder discovered its schema by workshop_apply — one
// human approval per guessed field. validate_spec now dry-runs the exact
// apply for every kind the workshop can author, so the same schema answer
// arrives with nothing created and nobody asked.
func TestHandleValidate_DryRunsTheApplyForKindsWithoutALocalValidator(t *testing.T) {
	ns := "ws-demo123"
	schemaErr := apierrors.NewBadRequest(".spec.credentials[0].authKind: field not declared in schema")
	s, rec := newDryRunServer(t, ns, schemaErr)

	res := callValidate(t, s, "AgentIdentity", json.RawMessage(`{"credentials":[{"name":"github","authKind":"oauth-mcp"}]}`))
	require.False(t, res.IsError, "a schema refusal is a diagnostic, not a tool error")
	body := decodeValidateResult(t, res)
	assert.Equal(t, false, body["valid"])
	assert.Contains(t, fmt.Sprint(body["diagnostics"]), "authKind: field not declared in schema")

	require.NotNil(t, rec.obj, "the candidate must reach the apiserver")
	assert.Equal(t, []string{metav1.DryRunAll}, rec.opts.DryRun, "the apply must be a dry run — nothing may be created")
	assert.Equal(t, "AgentIdentity", rec.obj.GetKind())
	assert.Equal(t, spiceboxv1alpha1.SchemeGroupVersion.String(), rec.obj.GetAPIVersion(), "the group is stamped from the kind, as apply does")
	assert.Equal(t, ns, rec.obj.GetNamespace(), "always the workshop's own namespace")
	assert.NotEmpty(t, rec.obj.GetName(), "SSA needs a name; spec-only content gets a placeholder")
	creds, _, _ := unstructured.NestedSlice(rec.obj.Object, "spec", "credentials")
	assert.Len(t, creds, 1, "spec-only content is wrapped under spec")
}

func TestHandleValidate_DryRunKeepsAFullManifestsOwnName(t *testing.T) {
	s, rec := newDryRunServer(t, "ws-demo123", nil)
	res := callValidate(t, s, "AgentUI", json.RawMessage(`{"kind":"AgentUI","apiVersion":"oap.dev/v1","metadata":{"name":"my-page"},"spec":{"view":{"component":"ap:card"}}}`))
	require.False(t, res.IsError)
	assert.Equal(t, true, decodeValidateResult(t, res)["valid"])
	require.NotNil(t, rec.obj)
	assert.Equal(t, "my-page", rec.obj.GetName())
	assert.Equal(t, spiceboxv1alpha1.SchemeGroupVersion.String(), rec.obj.GetAPIVersion(), "a guessed apiVersion is overwritten, never trusted")
}

// The local validators decode leniently, so a field the CRD does not declare
// passes them (observed: spec.server.auth on an MCPServer) and failed only at
// the real apply. The dry run runs AFTER a clean local pass and catches it.
func TestHandleValidate_DryRunFollowsACleanLocalPass(t *testing.T) {
	s, rec := newDryRunServer(t, "ws-demo123", apierrors.NewBadRequest(".spec.server.auth: field not declared in schema"))
	res := callValidate(t, s, "MCPServer", json.RawMessage(`{"name":"github","version":"v1","server":{"url":"https://example.test/mcp","transport":"streamable-http","auth":{"type":"federated"}},"tools":[{"name":"get_pull_request"}]}`))
	require.False(t, res.IsError)
	body := decodeValidateResult(t, res)
	assert.Equal(t, false, body["valid"])
	assert.Contains(t, fmt.Sprint(body["diagnostics"]), "server.auth: field not declared in schema")
	require.NotNil(t, rec.obj)
	assert.Equal(t, "MCPServer", rec.obj.GetKind())
}

// A Server built with no cluster client (the local-only validator tests) still
// answers, and says out loud that the server-side half did not run.
func TestHandleValidate_NoClusterClient_WarnsInsteadOfPretending(t *testing.T) {
	s := &Server{}
	res := callValidate(t, s, "AgentIdentity", json.RawMessage(`{"credentials":[]}`))
	require.False(t, res.IsError)
	body := decodeValidateResult(t, res)
	assert.Equal(t, true, body["valid"], "a warning is not an error")
	assert.Contains(t, fmt.Sprint(body["diagnostics"]), "dry run skipped")
}

// TestValidateCandidate_MCPServerArgumentGate is the live mistake, caught
// before apply: the builder authored a tool with args.constraints and no
// args.allowedFields, which the argument gate reads as "refuse every
// argument" — so every call the agent made was denied, after the tool had
// already been approved and installed. Nothing told the builder, because
// nothing it could call knew the rule.
func TestValidateCandidate_MCPServerArgumentGate(t *testing.T) {
	cases := []struct {
		name string
		body string
		// wantFlagged are the tool names the diagnostics must name; empty means
		// no argument-gate diagnostic at all.
		wantFlagged []string
	}{
		{
			name: "constraints but no allowedFields: flagged, because every call is refused",
			body: `
name: demo-connector
version: "1"
server: { url: https://x, transport: streamable-http }
tools:
  - name: pull_request_read
    args:
      constraints:
        - cel: 'args.q.size() < 100'
          message: 'q must have fewer than 100 items'
`,
			wantFlagged: []string{"pull_request_read"},
		},
		{
			name: "allowedFields listed: no diagnostic",
			body: `
name: demo-connector
version: "1"
server: { url: https://x, transport: streamable-http }
tools:
  - name: pull_request_read
    args:
      allowedFields: [owner, repo, pullNumber]
`,
		},
		{
			name: "unconstrainedArgs set: no diagnostic, the opt-out is explicit",
			body: `
name: demo-connector
version: "1"
server: { url: https://x, transport: streamable-http }
tools:
  - name: search
    args:
      unconstrainedArgs: true
`,
		},
		{
			name: "every tool bare: each one is named, so a fix list is complete",
			body: `
name: demo-connector
version: "1"
server: { url: https://x, transport: streamable-http }
tools:
  - name: first_tool
  - name: second_tool
    args:
      allowedFields: [q]
  - name: third_tool
`,
			wantFlagged: []string{"first_tool", "third_tool"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			diags := validateCandidate("MCPServer", []byte(tc.body))
			var flagged []string
			for _, d := range diags {
				if !strings.Contains(d.Message, "allowedFields") {
					continue
				}
				assert.Equal(t, "warning", d.Severity,
					"an arg-less tool is legitimately bare, so this must not fail the candidate")
				assert.Contains(t, d.Message, "unconstrainedArgs", "the diagnostic must name both ways out")
				for _, name := range []string{"first_tool", "second_tool", "third_tool", "pull_request_read", "search"} {
					if strings.Contains(d.Message, name) {
						flagged = append(flagged, name)
					}
				}
			}
			assert.ElementsMatch(t, tc.wantFlagged, flagged)
		})
	}
}

// crossCheckMCPServer builds an MCPServer CR carrying one auth shape, for a
// fake workshop namespace the cross-check reads.
func crossCheckMCPServer(ns, name, authType, credential, provider string) *spiceboxv1alpha1.MCPServer {
	return &spiceboxv1alpha1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: spiceboxv1alpha1.MCPServerSpec{
			Name:   name,
			Server: spiceboxv1alpha1.MCPServerServer{URL: "https://example.test/mcp", Transport: "streamable-http"},
			Auth:   spiceboxv1alpha1.MCPServerAuth{Type: authType, Credential: credential, Provider: provider},
		},
	}
}

// crossCheckAgentClass builds an AgentClass CR with one identity mode that
// references each named MCPServer.
func crossCheckAgentClass(ns, name, mode string, refs ...string) *spiceboxv1alpha1.AgentClass {
	ac := &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec:       spiceboxv1alpha1.AgentClassSpec{IdentityMode: mode},
	}
	for _, r := range refs {
		ac.Spec.MCPServers = append(ac.Spec.MCPServers, spiceboxv1alpha1.AgentClassMCPServerRef{Name: r, Ref: r})
	}
	return ac
}

// mcpServerCandidateJSON renders the flat (spec-content-only) MCPServer
// document validate_spec's local half accepts — the shape `oap tools validate`
// reads from a file — with only its auth stanza varying. An auth field left
// empty is omitted entirely, which is the shape under test: an auth stanza
// that names a type and nothing else.
func mcpServerCandidateJSON(t *testing.T, name, authType, credential, provider string) json.RawMessage {
	t.Helper()
	auth := map[string]any{}
	if authType != "" {
		auth["type"] = authType
	}
	if credential != "" {
		auth["credential"] = credential
	}
	if provider != "" {
		auth["provider"] = provider
	}
	raw, err := json.Marshal(map[string]any{
		"name":    name,
		"version": "1",
		"server":  map[string]any{"url": "https://example.test/mcp", "transport": "streamable-http"},
		"auth":    auth,
		"tools":   []any{map[string]any{"name": "read_item", "args": map[string]any{"allowedFields": []string{"id"}}}},
	})
	require.NoError(t, err)
	return raw
}

// newCrossCheckServer builds a Server whose fake cluster holds objs and whose
// dry run accepts every candidate, so the only diagnostics a validate call can
// carry are the local half's and the cross-check's.
func newCrossCheckServer(t *testing.T, ns string, objs ...client.Object) *Server {
	t.Helper()
	c := fake.NewClientBuilder().
		WithScheme(testfixtures.NewScheme(t)).
		WithObjects(objs...).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(_ context.Context, _ client.WithWatch, _ client.Object, _ client.Patch, _ ...client.PatchOption) error {
				return nil
			},
		}).Build()
	return &Server{K8s: c, Identity: WorkshopIdentity{Namespace: ns}, FieldOwner: defaultFieldOwner}
}

// diagnosticsUnderPath returns every diagnostic in a validate result whose
// Path starts with prefix — the cross-check's own paths — so a case's
// expectation is about the per-person rule and not about whatever else the
// local half or the dry run had to say.
func diagnosticsUnderPath(t *testing.T, body map[string]any, prefix string) []contract.Diagnostic {
	t.Helper()
	raw, err := json.Marshal(body["diagnostics"])
	require.NoError(t, err)
	var all []contract.Diagnostic
	require.NoError(t, json.Unmarshal(raw, &all))
	var out []contract.Diagnostic
	for _, d := range all {
		if strings.HasPrefix(d.Path, prefix) {
			out = append(out, d)
		}
	}
	return out
}

// severities lists each diagnostic's severity, in order, for a whole-set
// assertion — "exactly one error and nothing else" is the claim, not "an
// error appears somewhere".
func severities(diags []contract.Diagnostic) []string {
	var out []string
	for _, d := range diags {
		out = append(out, d.Severity)
	}
	return out
}

// The live miss on oap-desktop, 2026-09-13: the builder authored an MCPServer
// whose auth named a type and neither auth.credential nor auth.provider, under
// a userPassthrough class. passthrough.Resolve skips exactly that shape, so
// nobody was ever asked to link an account and every call went out
// unauthenticated — and validate_spec said the candidate was fine. The rule
// needs the CLASS's identity mode, which is why it lives here and not on the
// MCPServer reconciler: the same server under an agent-mode class is correct.
func TestHandleValidate_MCPServerNobodyCanConnect(t *testing.T) {
	const ns = "ws-demo123"
	cases := []struct {
		name string
		objs []client.Object
		// auth shape of the candidate under validation.
		authType, credential, provider string
		wantSeverities                 []string
	}{
		{
			name:           "oauth naming neither field under a userPassthrough class: error",
			objs:           []client.Object{crossCheckAgentClass(ns, "demo-agent", spiceboxv1alpha1.IdentityModeUserPassthrough, "demo-connector")},
			authType:       "oauth",
			wantSeverities: []string{"error"},
		},
		{
			name:           "static naming neither field under an ask class: warning, the person may still choose themselves",
			objs:           []client.Object{crossCheckAgentClass(ns, "demo-agent", spiceboxv1alpha1.IdentityModeAsk, "demo-connector")},
			authType:       "static",
			wantSeverities: []string{"warning"},
		},
		{
			name:       "oauth naming a credential under a userPassthrough class: clean",
			objs:       []client.Object{crossCheckAgentClass(ns, "demo-agent", spiceboxv1alpha1.IdentityModeUserPassthrough, "demo-connector")},
			authType:   "oauth",
			credential: "demo-connector-token",
		},
		{
			name:     "oauth naming neither field under an agent-mode class: clean, the credential comes from the agent identity",
			objs:     []client.Object{crossCheckAgentClass(ns, "demo-agent", spiceboxv1alpha1.IdentityModeAgent, "demo-connector")},
			authType: "oauth",
		},
		{
			name:     "federated naming neither field under a userPassthrough class: clean, the credential is minted not linked",
			objs:     []client.Object{crossCheckAgentClass(ns, "demo-agent", spiceboxv1alpha1.IdentityModeUserPassthrough, "demo-connector")},
			authType: "federated",
		},
		{
			name:     "oauth naming neither field with no class referencing it: clean",
			objs:     []client.Object{crossCheckAgentClass(ns, "demo-agent", spiceboxv1alpha1.IdentityModeUserPassthrough, "other-connector")},
			authType: "oauth",
		},
		{
			name: "two classes, one of them per-person: one error",
			objs: []client.Object{
				crossCheckAgentClass(ns, "demo-agent", spiceboxv1alpha1.IdentityModeUserPassthrough, "demo-connector"),
				crossCheckAgentClass(ns, "other-agent", spiceboxv1alpha1.IdentityModeAgent, "demo-connector"),
			},
			authType:       "oauth",
			wantSeverities: []string{"error"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newCrossCheckServer(t, ns, tc.objs...)
			res := callValidate(t, s, "MCPServer", mcpServerCandidateJSON(t, "demo-connector", tc.authType, tc.credential, tc.provider))
			require.False(t, res.IsError, "a cross-check finding is a diagnostic, not a protocol error")
			body := decodeValidateResult(t, res)
			diags := diagnosticsUnderPath(t, body, "spec.auth")
			assert.Equal(t, tc.wantSeverities, severities(diags))
			for _, d := range diags {
				assert.Contains(t, d.Message, "demo-connector", "the finding must name the server it is about")
				assert.Contains(t, d.Message, "auth.credential", "the finding must name the field that fixes it")
				assert.Contains(t, d.Message, "auth.provider", "the finding must name the field that fixes it")
			}
		})
	}
}

// An error diagnostic from the cross-check must reach the caller as
// valid=false — the whole point is that the candidate is refused before it is
// applied and a person is left with an agent that cannot authenticate.
func TestHandleValidate_MCPServerNobodyCanConnect_ReportsValidFalse(t *testing.T) {
	const ns = "ws-demo123"
	s := newCrossCheckServer(t, ns, crossCheckAgentClass(ns, "demo-agent", spiceboxv1alpha1.IdentityModeUserPassthrough, "demo-connector"))
	res := callValidate(t, s, "MCPServer", mcpServerCandidateJSON(t, "demo-connector", "oauth", "", ""))
	body := decodeValidateResult(t, res)
	assert.Equal(t, false, body["valid"])
	assert.Contains(t, fmt.Sprint(body["diagnostics"]), "identityMode userPassthrough")
}

// A full resource (kind/metadata/spec) is the other accepted candidate shape,
// and its CR name — the one an AgentClass ref names — comes from
// metadata.name, not from the spec's own server name.
func TestHandleValidate_MCPServerNobodyCanConnect_FullResourceUsesMetadataName(t *testing.T) {
	const ns = "ws-demo123"
	s := newCrossCheckServer(t, ns, crossCheckAgentClass(ns, "demo-agent", spiceboxv1alpha1.IdentityModeUserPassthrough, "demo-connector"))
	manifest := json.RawMessage(`{"kind":"MCPServer","metadata":{"name":"demo-connector"},"spec":{"name":"tool-prefix","version":"1","server":{"url":"https://example.test/mcp","transport":"streamable-http"},"auth":{"type":"oauth"},"tools":[{"name":"read_item"}]}}`)
	res := callValidate(t, s, "MCPServer", manifest)
	require.False(t, res.IsError)
	diags := diagnosticsUnderPath(t, decodeValidateResult(t, res), "spec.auth")
	require.Equal(t, []string{"error"}, severities(diags))
	assert.Contains(t, diags[0].Message, "demo-connector")
}

// The gate is order-independent: whichever of the two resources is validated
// second sees the other. Validating the CLASS reads each server it references.
func TestHandleValidate_AgentClassReferencingAServerNobodyCanConnect(t *testing.T) {
	const ns = "ws-demo123"
	cases := []struct {
		name           string
		objs           []client.Object
		mode           string
		wantSeverities []string
	}{
		{
			name:           "userPassthrough class referencing an unconnectable oauth server: error on the ref",
			objs:           []client.Object{crossCheckMCPServer(ns, "demo-connector", "oauth", "", "")},
			mode:           spiceboxv1alpha1.IdentityModeUserPassthrough,
			wantSeverities: []string{"error"},
		},
		{
			name:           "dynamic class referencing an unconnectable static server: warning on the ref",
			objs:           []client.Object{crossCheckMCPServer(ns, "demo-connector", "static", "", "")},
			mode:           spiceboxv1alpha1.IdentityModeDynamic,
			wantSeverities: []string{"warning"},
		},
		{
			name: "userPassthrough class whose server is not created yet: clean, the dangling ref is the dry run's finding",
			mode: spiceboxv1alpha1.IdentityModeUserPassthrough,
		},
		{
			name: "userPassthrough class referencing a credentialed server: clean",
			objs: []client.Object{crossCheckMCPServer(ns, "demo-connector", "oauth", "demo-connector-token", "")},
			mode: spiceboxv1alpha1.IdentityModeUserPassthrough,
		},
		{
			name: "agent-mode class referencing an unconnectable oauth server: clean",
			objs: []client.Object{crossCheckMCPServer(ns, "demo-connector", "oauth", "", "")},
			mode: spiceboxv1alpha1.IdentityModeAgent,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newCrossCheckServer(t, ns, tc.objs...)
			manifest, err := json.Marshal(map[string]any{
				"identityMode": tc.mode,
				"mcpServers":   []any{map[string]any{"name": "demo-connector", "ref": "demo-connector"}},
			})
			require.NoError(t, err)
			res := callValidate(t, s, "AgentClass", manifest)
			require.False(t, res.IsError)
			diags := diagnosticsUnderPath(t, decodeValidateResult(t, res), "spec.mcpServers[0]")
			assert.Equal(t, tc.wantSeverities, severities(diags))
			for _, d := range diags {
				assert.Contains(t, d.Message, "demo-connector")
				assert.Contains(t, d.Message, "auth.credential")
			}
		})
	}
}

// A read that fails for any reason other than "not found" must not pass the
// candidate silently: the cross-check says out loud that it did not run, as a
// warning — a transient read must never fail a correct candidate either.
func TestHandleValidate_CrossCheckReadFailure_WarnsRatherThanPassing(t *testing.T) {
	const ns = "ws-demo123"
	c := fake.NewClientBuilder().WithScheme(testfixtures.NewScheme(t)).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(_ context.Context, _ client.WithWatch, _ client.ObjectList, _ ...client.ListOption) error {
				return apierrors.NewServiceUnavailable("the apiserver is having a moment")
			},
			Patch: func(_ context.Context, _ client.WithWatch, _ client.Object, _ client.Patch, _ ...client.PatchOption) error {
				return nil
			},
		}).Build()
	s := &Server{K8s: c, Identity: WorkshopIdentity{Namespace: ns}, FieldOwner: defaultFieldOwner}

	res := callValidate(t, s, "MCPServer", mcpServerCandidateJSON(t, "demo-connector", "oauth", "", ""))
	require.False(t, res.IsError)
	body := decodeValidateResult(t, res)
	assert.Equal(t, true, body["valid"], "a read that could not run must not fail a candidate")
	diags := diagnosticsUnderPath(t, body, "spec.auth")
	require.Equal(t, []string{"warning"}, severities(diags))
	assert.NotContains(t, diags[0].Message, "apiserver", "the wording a person may be shown carries no infrastructure words")
}

// A Server with no cluster client (the local-only validator tests build one)
// cannot run the cross-check and says so, rather than pretending the candidate
// passed a check that never happened.
func TestHandleValidate_CrossCheckNoClusterClient_WarnsInsteadOfPretending(t *testing.T) {
	s := &Server{}
	res := callValidate(t, s, "MCPServer", mcpServerCandidateJSON(t, "demo-connector", "oauth", "", ""))
	require.False(t, res.IsError)
	diags := diagnosticsUnderPath(t, decodeValidateResult(t, res), "spec.auth")
	require.Equal(t, []string{"warning"}, severities(diags))
}

// Two per-person classes on ONE server are two findings, and each names the
// class it is about.
//
// Without the class name they are the same sentence twice, which reads as one
// finding reported in duplicate — the builder relaying it to a person would
// say "this server has a problem" rather than "these two agents each have
// one", and fixing the agent it happened to be thinking of would leave the
// other exactly as broken. The server-side check is the only one of the two
// that can say this: it is the end that walks a SET of classes.
func TestHandleValidate_MCPServerNobodyCanConnect_NamesEachReferencingClass(t *testing.T) {
	const ns = "ws-demo123"
	s := newCrossCheckServer(t, ns,
		crossCheckAgentClass(ns, "demo-agent", spiceboxv1alpha1.IdentityModeUserPassthrough, "demo-connector"),
		crossCheckAgentClass(ns, "demo-reporter", spiceboxv1alpha1.IdentityModeUserPassthrough, "demo-connector"),
	)

	res := callValidate(t, s, "MCPServer", mcpServerCandidateJSON(t, "demo-connector", "oauth", "", ""))
	require.False(t, res.IsError)
	diags := diagnosticsUnderPath(t, decodeValidateResult(t, res), "spec.auth")
	require.Equal(t, []string{"error", "error"}, severities(diags),
		"one finding per referencing class, not one per server")

	var named []string
	for _, d := range diags {
		assert.Contains(t, d.Message, `server "demo-connector", referenced by `,
			"the finding must name the server and then the agent that references it")
		for _, class := range []string{"demo-agent", "demo-reporter"} {
			if strings.Contains(d.Message, strconv.Quote(class)) {
				named = append(named, class)
			}
		}
	}
	assert.ElementsMatch(t, []string{"demo-agent", "demo-reporter"}, named,
		"each finding names its own class, so two agents read as two problems")
}

// The class side's own no-cluster-client branch, the mirror of
// TestHandleValidate_CrossCheckNoClusterClient_WarnsInsteadOfPretending.
//
// Both ends of this rule reach the cluster and both can find it absent, and
// the two branches are separate code — so a Server built without a client
// (every local-only validator test builds one) has to say the comparison did
// not happen from EITHER end. Untested, the class-side branch could return nil
// and report a candidate clean on a check that never ran, and nothing else
// here would notice: the local validation passes, so `valid` stays true.
func TestHandleValidate_AgentClassCrossCheckNoClusterClient_WarnsInsteadOfPretending(t *testing.T) {
	s := &Server{}
	manifest, err := json.Marshal(map[string]any{
		"identityMode": spiceboxv1alpha1.IdentityModeUserPassthrough,
		"mcpServers":   []any{map[string]any{"name": "demo-connector", "ref": "demo-connector"}},
	})
	require.NoError(t, err)

	res := callValidate(t, s, "AgentClass", manifest)
	require.False(t, res.IsError)
	diags := diagnosticsUnderPath(t, decodeValidateResult(t, res), "spec.mcpServers")
	require.Equal(t, []string{"warning"}, severities(diags),
		"a check that could not run is a warning: it is not evidence against the candidate")
	assert.Equal(t, crossCheckUnavailable, diags[0].Message,
		"and it says so in the fixed wording, with the cause logged rather than relayed")
}
