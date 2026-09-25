//go:build !integration && !e2e

package credkind_test

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/platform/identity/credkind"
	// Registers the static/oauth/federated credkind.Kinds. Without this blank
	// import registry.Keys()/All() are empty in THIS test binary regardless of
	// what any other package imports — a test binary is its own process, and
	// another package's blank import of credkind/imports does not leak in.
	// Every test below that ranges over the registry needs a real one to walk;
	// each also carries its own require.NotEmpty(registry.Keys()) precondition
	// so dropping this import fails loudly instead of returning every guard to
	// a vacuous, always-green loop over zero kinds.
	_ "github.com/authzed/openagentprimitives/pkg/platform/identity/credkind/imports"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/credkind/registry"
)

// enumMarkerRE finds a `+kubebuilder:validation:Enum=a;b;c` marker line and
// captures the semicolon-separated value list.
var enumMarkerRE = regexp.MustCompile(`\+kubebuilder:validation:Enum=([A-Za-z0-9_;-]+)`)

// enumValues reads src and returns the union of every Enum marker's values,
// so a registered kind missing from the actual `.Type` enum is caught even
// if its name happens to appear elsewhere in the file (a comment, a doc
// string) — plain substring Contains would miss that distinction.
func enumValues(t *testing.T, path string) map[string]bool {
	t.Helper()
	src, err := os.ReadFile(path)
	require.NoError(t, err, "reading %s", path)

	values := map[string]bool{}
	for _, m := range enumMarkerRE.FindAllStringSubmatch(string(src), -1) {
		for _, v := range strings.Split(m[1], ";") {
			values[v] = true
		}
	}
	require.NotEmpty(t, values, "no +kubebuilder:validation:Enum marker found in %s; "+
		"this test's assumption about where the CredentialSource/AgentCredential .Type enum "+
		"lives is stale", path)
	return values
}

// TestEveryRegisteredKindIsInTheCRDEnum pins the one thing the registry
// cannot enforce for itself: the kubebuilder Enum marker is static text, so a
// kind registered in Go but missing from the marker would be rejected by the
// apiserver at apply time, far from its cause. Both markers carry the same
// enum independently (CredentialSource.Type on ToolCall's resolved
// descriptor, AgentCredential.Type on the identity CRs) and must both list
// every registered kind.
func TestEveryRegisteredKindIsInTheCRDEnum(t *testing.T) {
	require.NotEmpty(t, registry.Keys(),
		"registry is empty — this test binary is missing its blank import of "+
			"credkind/imports, and every assertion below would iterate nothing")

	files := []string{
		"../../../apis/v1alpha1/credential_descriptor.go", // CredentialSource.Type
		"../../../apis/v1alpha1/agentidentity_types.go",   // AgentCredential.Type
	}
	for _, f := range files {
		values := enumValues(t, f)
		for _, key := range registry.Keys() {
			assert.True(t, values[key],
				"credkind %q is registered but absent from the Enum marker in %s; "+
					"add it and re-run `mage gen:api && mage manifests`", key, f)
		}
	}
}

// TestEveryCRDEnumValueHasARegisteredKind is the reverse of
// TestEveryRegisteredKindIsInTheCRDEnum: it catches an Enum marker value with
// NO registered Kind behind it, not just a registered Kind missing from the
// marker. That gap matters because Plan 2's own rollout is two commits — add
// the value to the Enum marker, then register the Kind — and this is exactly
// the window between them: the apiserver would accept a spec.credentials[].type
// or CredentialSource.type it should not, and every consumer would then fail
// closed on registry.Get far from the apply that let it through.
func TestEveryCRDEnumValueHasARegisteredKind(t *testing.T) {
	require.NotEmpty(t, registry.Keys(),
		"registry is empty — this test binary is missing its blank import of "+
			"credkind/imports, and every assertion below would iterate nothing")

	registered := map[string]bool{}
	for _, key := range registry.Keys() {
		registered[key] = true
	}

	files := []string{
		"../../../apis/v1alpha1/credential_descriptor.go", // CredentialSource.Type
		"../../../apis/v1alpha1/agentidentity_types.go",   // AgentCredential.Type
	}
	for _, f := range files {
		for value := range enumValues(t, f) {
			assert.True(t, registered[value],
				"the +kubebuilder:validation:Enum marker in %s accepts %q but no credkind.Kind "+
					"is registered for it; the apiserver will accept type=%s and every consumer "+
					"will fail closed on registry.Get until a Kind is registered", f, value, value)
		}
	}
}

// agentIdentityCELExclusionRE pulls `c.type == 'x'` literals out of the
// AgentIdentitySpec XValidation rule text — the apiserver-level mirror of
// ValidOn() excluding ScopeAgentIdentity. It is read-only: this test must
// NEVER rewrite the marker. gofmt turns straight quotes into smart quotes in
// doc comments and silently breaks CEL markers in a way envtest skips rather
// than reports.
var agentIdentityCELExclusionRE = regexp.MustCompile(`c\.type == '([^']+)'`)

// TestAgentIdentityCELExclusionsMatchValidOn asserts the two sources of
// truth for "which credential types are rejected on an AgentIdentity" agree:
// the CEL XValidation rule on AgentIdentitySpec (apiserver-enforced, static
// text) and credkind.Kind.ValidOn() (Go-enforced, per Task 11's registry).
// Guard 1 parses Go ASTs and will never see a CEL string, so nothing else in
// this suite would catch the two drifting apart.
func TestAgentIdentityCELExclusionsMatchValidOn(t *testing.T) {
	require.NotEmpty(t, registry.Keys(),
		"registry is empty — this test binary is missing its blank import of "+
			"credkind/imports, and every assertion below would iterate nothing")

	src, err := os.ReadFile("../../../apis/v1alpha1/agentidentity_types.go")
	require.NoError(t, err)

	excludedByCEL := map[string]bool{}
	for _, m := range agentIdentityCELExclusionRE.FindAllStringSubmatch(string(src), -1) {
		excludedByCEL[m[1]] = true
	}
	require.NotEmpty(t, excludedByCEL,
		"no c.type == '...' exclusion found in the AgentIdentitySpec XValidation rule; "+
			"this test's assumption about where that CEL rule lives is stale")

	for _, k := range registry.All() {
		excludedByGo := !credkind.ValidOnScope(k, credkind.ScopeAgentIdentity)
		if !excludedByGo {
			continue
		}
		assert.True(t, excludedByCEL[k.Type()],
			"credkind %q excludes ScopeAgentIdentity from ValidOn() but the AgentIdentitySpec "+
				"XValidation rule does not reject c.type == %q; the apiserver would accept what "+
				"Go-level validation rejects. Add the exclusion to the CEL rule in "+
				"pkg/apis/v1alpha1/agentidentity_types.go — do NOT let gofmt touch that line, it "+
				"rewrites straight quotes to smart quotes and silently breaks the marker.", k.Type(), k.Type())
	}
}

// TestMintedKindsAreSubjectExclusiveAndStoreNothing sweeps every registered
// kind rather than naming them, so a kind added later cannot skip the check.
// It does NOT assert that a minted kind stamps an expiry — that invariant is
// enforced once, at the broker's single choke point
// (pkg/platform/identity/broker/inproc/broker.go's resolveOneSource), not
// per-kind here. What this test does assert: a minted kind serves either a
// human subject (UserIdentity/SessionUserIdentity) or a bot
// (AgentIdentity) but never both, and that "minted" implies "nothing is
// stored" — NeedsRefresh is false and BuildCredential always errors.
func TestMintedKindsAreSubjectExclusiveAndStoreNothing(t *testing.T) {
	require.NotEmpty(t, registry.Keys(),
		"registry is empty — this test binary is missing its blank import of "+
			"credkind/imports, and every assertion below would iterate nothing")

	for _, k := range registry.All() {
		if !k.Minted() {
			continue
		}
		t.Run(k.Type(), func(t *testing.T) {
			assert.False(t, credkind.ValidOnScope(k, credkind.ScopeUserIdentity) &&
				credkind.ValidOnScope(k, credkind.ScopeAgentIdentity),
				"a minted kind serves either a human subject or a bot, never both")
			assert.False(t, k.NeedsRefresh(),
				"a minted kind stores nothing, so there is nothing to refresh")
			_, err := k.BuildCredential("c", "s", "k")
			assert.Error(t, err,
				"a minted kind has no stored value for the setup flow to write")
		})
	}
}
