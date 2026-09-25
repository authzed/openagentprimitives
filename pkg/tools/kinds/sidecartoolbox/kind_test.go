package sidecartoolbox_test

import (
	"fmt"
	"os"
	"strings"
	"testing"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/tools/kinds/sidecartoolbox"
)

func TestKind_Name(t *testing.T) {
	k := sidecartoolbox.New()
	if got, want := k.Name(), "SidecarToolbox"; got != want {
		t.Errorf("Name=%q want %q", got, want)
	}
}

func TestKind_DecodeYAML_PicksOnlySidecarToolbox(t *testing.T) {
	k := sidecartoolbox.New()
	other := []byte(`apiVersion: agentprimitives.authzed.com/v1alpha1
kind: MCPServer
metadata: { name: x }
spec: {}
`)
	obj, err := k.DecodeYAML(other)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if obj != nil {
		t.Fatalf("expected nil for non-SidecarToolbox kind, got %T", obj)
	}

	mine := []byte(`apiVersion: agentprimitives.authzed.com/v1alpha1
kind: SidecarToolbox
metadata: { name: redditro }
spec:
  name: redditro
  version: "1"
  source:
    image: ghcr.io/example/reddit-mcp:v1
  sandbox: { class: sidecar-sandbox-default }
  transport: { port: 8080 }
  upstreamAuth: { provider: reddit-app }
  tools: []
`)
	obj, err = k.DecodeYAML(mine)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	cr, ok := obj.(*spiceboxv1alpha1.SidecarToolbox)
	if !ok {
		t.Fatalf("expected *SidecarToolbox, got %T", obj)
	}
	if cr.Name != "redditro" {
		t.Errorf("Name=%q", cr.Name)
	}
}

func TestKind_ValidateFile_RejectsBothSourceVariants(t *testing.T) {
	k := sidecartoolbox.New()
	tmp := writeTempFile(t, `
name: redditro
version: "1"
source:
  image: ghcr.io/x:v1
  inline:
    baseImage: python:3.12-slim
    script: { configMapRef: { name: x, key: y.py } }
    entrypoint: ["python", "/app/y.py"]
sandbox: { class: sidecar-sandbox-default }
transport: { port: 8080 }
upstreamAuth: { provider: reddit-app }
tools: []
`)
	diags, err := k.ValidateFile(tmp)
	if err != nil {
		t.Fatalf("ValidateFile err=%v", err)
	}
	if len(diags) == 0 || !strings.Contains(diags[0].Message, "exactly one of") {
		t.Fatalf("expected source-discriminator diagnostic, got %+v", diags)
	}
}

func TestKind_ValidateFile_RejectsNeitherSourceVariant(t *testing.T) {
	k := sidecartoolbox.New()
	tmp := writeTempFile(t, `
name: redditro
version: "1"
source: {}
sandbox: { class: sidecar-sandbox-default }
transport: { port: 8080 }
upstreamAuth: { provider: reddit-app }
tools: []
`)
	diags, err := k.ValidateFile(tmp)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(diags) == 0 {
		t.Fatalf("expected diagnostic; got none")
	}
}

func TestKind_ValidateFile_CompilesCEL(t *testing.T) {
	k := sidecartoolbox.New()
	tmp := writeTempFile(t, `
name: redditro
version: "1"
source: { image: ghcr.io/x:v1 }
sandbox: { class: sidecar-sandbox-default }
transport: { port: 8080 }
upstreamAuth: { provider: reddit-app }
tools:
  - name: search
    args:
      constraints:
        - cel: 'this is not valid CEL'
`)
	diags, err := k.ValidateFile(tmp)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(diags) == 0 {
		t.Fatalf("expected CEL compile diagnostic")
	}
}

func TestKind_ValidateFile_AcceptsNoneUpstreamAuthProvider(t *testing.T) {
	k := sidecartoolbox.New()
	tmp := writeTempFile(t, `
name: workshop
version: "1"
source: { image: ap-workshop:dev }
sandbox: { class: workshop-sandbox }
transport: { port: 8080 }
upstreamAuth: { provider: none }
tools: []
`)
	diags, err := k.ValidateFile(tmp)
	if err != nil {
		t.Fatalf("ValidateFile err=%v", err)
	}
	for _, d := range diags {
		if strings.Contains(d.Message, "upstreamAuth.provider is required") {
			t.Fatalf(`provider: "none" must not trigger the "upstreamAuth.provider is required" diagnostic, got %+v`, diags)
		}
	}
}

func TestKind_ValidateFile_EmptyUpstreamAuthProviderStillErrors(t *testing.T) {
	k := sidecartoolbox.New()
	tmp := writeTempFile(t, `
name: workshop
version: "1"
source: { image: ap-workshop:dev }
sandbox: { class: workshop-sandbox }
transport: { port: 8080 }
upstreamAuth: { provider: "" }
tools: []
`)
	diags, err := k.ValidateFile(tmp)
	if err != nil {
		t.Fatalf("ValidateFile err=%v", err)
	}
	var found bool
	for _, d := range diags {
		if strings.Contains(d.Message, "upstreamAuth.provider is required") {
			found = true
		}
	}
	if !found {
		t.Fatalf(`empty provider must still trigger "upstreamAuth.provider is required", got %+v`, diags)
	}
}

// adapterConfigFixture is a syntactically valid apiadapter.Config with one
// operation, get_account — mirrors the fixture the admission webhook's own
// tests use for the same contract (plan 7b task 3), so the two checks are
// exercised against an equivalent shape. Kept as a plain multi-line string
// and embedded into a spec YAML document via %q (a double-quoted YAML
// scalar), the same style the invalid-config test below already uses —
// avoids the reindentation a literal block scalar ("config: |") would need.
const adapterConfigFixture = "baseURL: https://api.example.test\nauth: { type: none }\noperations:\n  - name: get_account\n    method: GET\n    path: /account\n"

// TestKind_ValidateFile_AdapterImage_ValidConfigMatchingTools — MINOR-2: the
// builder's "validate before apply" step must accept a config that WOULD be
// admitted, not just defer to the webhook for every case.
func TestKind_ValidateFile_AdapterImage_ValidConfigMatchingTools(t *testing.T) {
	k := sidecartoolbox.New()
	tmp := writeTempFile(t, fmt.Sprintf(`
name: adapter
version: "1"
source: { image: ap-api-adapter:dev }
sandbox: { class: sidecar-sandbox-default }
transport: { port: 8080 }
upstreamAuth: { provider: none }
config: %q
tools:
  - name: get_account
`, adapterConfigFixture))
	diags, err := k.ValidateFile(tmp)
	if err != nil {
		t.Fatalf("ValidateFile err=%v", err)
	}
	for _, d := range diags {
		if d.Path == "config" || d.Path == "tools" {
			t.Fatalf("unexpected adapter-config diagnostic for a matching config: %+v", diags)
		}
	}
}

// TestKind_ValidateFile_AdapterImage_MissingConfig — MINOR-2: the same "a
// config is required" refusal the webhook gives, produced locally.
func TestKind_ValidateFile_AdapterImage_MissingConfig(t *testing.T) {
	k := sidecartoolbox.New()
	tmp := writeTempFile(t, `
name: adapter
version: "1"
source: { image: ap-api-adapter:dev }
sandbox: { class: sidecar-sandbox-default }
transport: { port: 8080 }
upstreamAuth: { provider: none }
tools: []
`)
	diags, err := k.ValidateFile(tmp)
	if err != nil {
		t.Fatalf("ValidateFile err=%v", err)
	}
	var found bool
	for _, d := range diags {
		if d.Path == "config" && strings.Contains(d.Message, "spec.config is required") {
			found = true
		}
	}
	if !found {
		t.Fatalf(`expected "spec.config is required" diagnostic, got %+v`, diags)
	}
}

// TestKind_ValidateFile_AdapterImage_InvalidConfig_SurfacesParserMessage —
// MINOR-2: a config that fails apiadapter.Parse must surface the parser's
// own words, the same contract the webhook honors (denied with the parser's
// own message, never a re-derived summary).
func TestKind_ValidateFile_AdapterImage_InvalidConfig_SurfacesParserMessage(t *testing.T) {
	k := sidecartoolbox.New()
	tmp := writeTempFile(t, `
name: adapter
version: "1"
source: { image: ap-api-adapter:dev }
sandbox: { class: sidecar-sandbox-default }
transport: { port: 8080 }
upstreamAuth: { provider: none }
config: "baseURL: https://api.example.test\nauth: {type: bogus}\noperations: []"
tools: []
`)
	diags, err := k.ValidateFile(tmp)
	if err != nil {
		t.Fatalf("ValidateFile err=%v", err)
	}
	var found bool
	for _, d := range diags {
		// Distinctive fragment proving the diagnostic carries the parser's own
		// message verbatim (config.go's "apiadapter:" error convention),
		// mirroring the webhook's equivalent test.
		if d.Path == "config" && strings.Contains(d.Message, "apiadapter: auth.type") {
			found = true
		}
	}
	if !found {
		t.Fatalf(`expected a diagnostic carrying the parser's own message, got %+v`, diags)
	}
}

// TestKind_ValidateFile_AdapterImage_ToolsMismatch — MINOR-2: spec.tools
// naming something other than the config's operations must be caught here,
// via the SAME apiadapter.Config.ToolNamesMatch the webhook calls — not a
// re-implemented comparison that could disagree with it.
func TestKind_ValidateFile_AdapterImage_ToolsMismatch(t *testing.T) {
	k := sidecartoolbox.New()
	tmp := writeTempFile(t, fmt.Sprintf(`
name: adapter
version: "1"
source: { image: ap-api-adapter:dev }
sandbox: { class: sidecar-sandbox-default }
transport: { port: 8080 }
upstreamAuth: { provider: none }
config: %q
tools:
  - name: get_account
  - name: extra_tool
`, adapterConfigFixture))
	diags, err := k.ValidateFile(tmp)
	if err != nil {
		t.Fatalf("ValidateFile err=%v", err)
	}
	var found bool
	for _, d := range diags {
		if d.Path == "tools" && strings.Contains(d.Message, "extra_tool") {
			found = true
		}
	}
	if !found {
		t.Fatalf(`expected a "tools" diagnostic naming extra_tool, got %+v`, diags)
	}
}

// authedAdapterConfigFixture is adapterConfigFixture's auth-required sibling:
// same one operation, but auth.type "bearer" with an explicit envVar — so a
// mismatch between it and spec.upstreamAuth.envVar has something to
// disagree about (adapterConfigFixture's auth.type "none" never checks
// upstreamAuth.envVar at all, per apiadapter.Config.UpstreamEnvVarMismatch).
const authedAdapterConfigFixture = "baseURL: https://api.example.test\nauth: { type: bearer, envVar: UPSTREAM_TOKEN }\noperations:\n  - name: get_account\n    method: GET\n    path: /account\n"

// TestKind_ValidateFile_AdapterImage_UpstreamEnvVarMatches — MINOR-2: a
// config whose auth.envVar agrees with spec.upstreamAuth.envVar must not be
// flagged.
func TestKind_ValidateFile_AdapterImage_UpstreamEnvVarMatches(t *testing.T) {
	k := sidecartoolbox.New()
	tmp := writeTempFile(t, fmt.Sprintf(`
name: adapter
version: "1"
source: { image: ap-api-adapter:dev }
sandbox: { class: sidecar-sandbox-default }
transport: { port: 8080 }
upstreamAuth: { provider: some-provider, envVar: UPSTREAM_TOKEN }
config: %q
tools:
  - name: get_account
`, authedAdapterConfigFixture))
	diags, err := k.ValidateFile(tmp)
	if err != nil {
		t.Fatalf("ValidateFile err=%v", err)
	}
	for _, d := range diags {
		if d.Path == "upstreamAuth.envVar" {
			t.Fatalf("unexpected upstreamAuth.envVar diagnostic for a matching env var: %+v", diags)
		}
	}
}

// TestKind_ValidateFile_AdapterImage_UpstreamEnvVarMismatch — MINOR-2: a
// config's auth.envVar disagreeing with spec.upstreamAuth.envVar must be
// caught here, locally, rather than only at sidecar boot.
func TestKind_ValidateFile_AdapterImage_UpstreamEnvVarMismatch(t *testing.T) {
	k := sidecartoolbox.New()
	tmp := writeTempFile(t, fmt.Sprintf(`
name: adapter
version: "1"
source: { image: ap-api-adapter:dev }
sandbox: { class: sidecar-sandbox-default }
transport: { port: 8080 }
upstreamAuth: { provider: some-provider, envVar: WRONG_VAR_NAME }
config: %q
tools:
  - name: get_account
`, authedAdapterConfigFixture))
	diags, err := k.ValidateFile(tmp)
	if err != nil {
		t.Fatalf("ValidateFile err=%v", err)
	}
	var found bool
	for _, d := range diags {
		if d.Path == "upstreamAuth.envVar" && strings.Contains(d.Message, "UPSTREAM_TOKEN") && strings.Contains(d.Message, "WRONG_VAR_NAME") {
			found = true
		}
	}
	if !found {
		t.Fatalf(`expected an "upstreamAuth.envVar" diagnostic naming both env vars, got %+v`, diags)
	}
}

// TestKind_ValidateFile_ProviderNoneWithEnvVar_Refused mirrors the controller's
// providerCheck guard: the sentinel means the credential is controller-issued,
// so nothing ever resolves a library credential into an env var, and naming one
// is a contradiction. Caught here so validate_spec refuses it before an
// approval card rather than letting it dead-end at session boot on advice
// (`oap agent setup-identity`) that cannot fix a "none" provider.
func TestKind_ValidateFile_ProviderNoneWithEnvVar_Refused(t *testing.T) {
	k := sidecartoolbox.New()
	tmp := writeTempFile(t, `
name: contradictory
version: "1"
source: { image: ap-workshop:dev }
sandbox: { class: workshop-sandbox }
transport: { port: 8080 }
upstreamAuth: { provider: none, envVar: UPSTREAM_TOKEN }
tools: []
`)
	diags, err := k.ValidateFile(tmp)
	if err != nil {
		t.Fatalf("ValidateFile err=%v", err)
	}
	var found bool
	for _, d := range diags {
		if d.Path == "upstreamAuth.envVar" && d.Severity == "error" && strings.Contains(d.Message, "must be empty") {
			found = true
		}
	}
	if !found {
		t.Fatalf(`expected an "upstreamAuth.envVar" error refusing the sentinel+envVar pairing, got %+v`, diags)
	}
}

// TestKind_ValidateFile_NonAdapterImage_ConfigNotChecked — the adapter-config
// check must only fire for the adapter image; a non-adapter first-party
// image (ap-workshop) with no config must not gain a spurious "spec.config
// is required" diagnostic.
func TestKind_ValidateFile_NonAdapterImage_ConfigNotChecked(t *testing.T) {
	k := sidecartoolbox.New()
	tmp := writeTempFile(t, `
name: workshop
version: "1"
source: { image: ap-workshop:dev }
sandbox: { class: workshop-sandbox }
transport: { port: 8080 }
upstreamAuth: { provider: none }
tools: []
`)
	diags, err := k.ValidateFile(tmp)
	if err != nil {
		t.Fatalf("ValidateFile err=%v", err)
	}
	for _, d := range diags {
		if d.Path == "config" {
			t.Fatalf("unexpected adapter-config diagnostic for a non-adapter image: %+v", diags)
		}
	}
}

func TestKind_MatchFlat_DetectsByTopLevelFields(t *testing.T) {
	k := sidecartoolbox.New()
	flat := []byte(`name: redditro
version: "1"
source: { image: ghcr.io/x:v1 }
sandbox: { class: sidecar-sandbox-default }
transport: { port: 8080 }
upstreamAuth: { provider: reddit-app }
tools: []
`)
	if !k.MatchFlat(flat) {
		t.Fatalf("expected MatchFlat=true")
	}
	if k.MatchFlat([]byte(`server: { url: https://x }`)) {
		t.Fatalf("MatchFlat must not claim flat-MCP specs")
	}
}

func writeTempFile(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	p := dir + "/spec.yaml"
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	return p
}
