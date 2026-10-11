//go:build !integration && !e2e

package directcred_test

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/platform/identity/directcred"
	// Register the two real sources. Without these blank imports the registry
	// is empty in THIS test binary regardless of what the operator imports — a
	// test binary is its own process, and internal/cmd/operator's blank imports
	// do not leak in. The snapshot below carries its own require.NotEmpty
	// precondition so dropping either import fails loudly instead of returning
	// both guards to a vacuous, always-green loop over zero sources.
	_ "github.com/authzed/openagentprimitives/pkg/platform/identity/directcred/onepassword"
	_ "github.com/authzed/openagentprimitives/pkg/platform/identity/directcred/secretsource"
)

// sourcesAtStartup snapshots the registered source kinds at package-var-init
// time — after the blank imports' init() registrations, BEFORE any test body
// runs. The sibling registry tests (directcred_test.go) Reset() the
// process-wide registry as part of their own fixtures, so reading
// directcred.All() inside a test body would make these guards order-dependent
// on which file's tests ran first; the snapshot is not.
var sourcesAtStartup = func() map[string]bool {
	kinds := map[string]bool{}
	for _, s := range directcred.All() {
		kinds[s.Kind()] = true
	}
	return kinds
}()

// enumMarkerRE finds a `+kubebuilder:validation:Enum=a;b;c` marker line and
// captures the semicolon-separated value list. Same shape as the credkind
// enum guard (pkg/platform/identity/credkind/enum_invariant_test.go), whose
// window this test mirrors for directcred sources.
var enumMarkerRE = regexp.MustCompile(`\+kubebuilder:validation:Enum=([A-Za-z0-9_;-]+)`)

// directCredentialsTypesFile holds the DirectCredentialsSource.Source enum
// marker this guard reads.
const directCredentialsTypesFile = "../../../apis/v1alpha1/directcredentials_types.go"

// enumValues reads the types file and returns the union of every Enum
// marker's values (today there is exactly one marker: Source's).
func enumValues(t *testing.T) map[string]bool {
	t.Helper()
	src, err := os.ReadFile(directCredentialsTypesFile)
	require.NoError(t, err, "reading %s", directCredentialsTypesFile)

	values := map[string]bool{}
	for _, m := range enumMarkerRE.FindAllStringSubmatch(string(src), -1) {
		for _, v := range strings.Split(m[1], ";") {
			values[v] = true
		}
	}
	require.NotEmpty(t, values, "no +kubebuilder:validation:Enum marker found in %s; "+
		"this test's assumption about where the DirectCredentialsSource.Source enum lives is stale",
		directCredentialsTypesFile)
	return values
}

// TestEveryEnumSourceValueHasARegisteredSource is the directcred twin of
// credkind's TestEveryCRDEnumValueHasARegisteredKind: it catches an Enum
// marker value with NO registered Source behind it. That gap matters because
// adding a backend is two steps — add the value to the Enum marker, register
// the Source — and this is exactly the window between them: the apiserver
// would accept a source the binary cannot drive, and the AgentIdentity
// reconciler would then fail CLOSED (Valid=False/DirectCredentialInvalid,
// see verifyDirectCredential) on every identity using it, far from the
// commit that opened the window.
func TestEveryEnumSourceValueHasARegisteredSource(t *testing.T) {
	require.NotEmpty(t, sourcesAtStartup,
		"registry snapshot is empty — this test file is missing its blank imports of "+
			"directcred/onepassword and directcred/secretsource, and every assertion below "+
			"would iterate nothing")

	for value := range enumValues(t) {
		assert.True(t, sourcesAtStartup[value],
			"the +kubebuilder:validation:Enum marker in %s accepts source=%q but no "+
				"directcred.Source is registered for it; the apiserver will accept it and the "+
				"AgentIdentity reconciler will fail closed (DirectCredentialInvalid) on every "+
				"identity that uses it until the source's package is written and blank-imported",
			directCredentialsTypesFile, value)
	}
}

// TestEveryRegisteredSourceIsInTheEnum is the reverse direction: a Source
// registered in Go but absent from the static Enum marker would be rejected
// by the apiserver at apply time, far from its cause.
func TestEveryRegisteredSourceIsInTheEnum(t *testing.T) {
	require.NotEmpty(t, sourcesAtStartup,
		"registry snapshot is empty — this test file is missing its blank imports of "+
			"directcred/onepassword and directcred/secretsource, and every assertion below "+
			"would iterate nothing")

	values := enumValues(t)
	for kind := range sourcesAtStartup {
		assert.True(t, values[kind],
			"directcred source %q is registered but absent from the Enum marker in %s; "+
				"add it and re-run `mage gen:api && mage manifests`", kind, directCredentialsTypesFile)
	}
}
