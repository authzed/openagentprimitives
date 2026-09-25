package provider_test

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/platform/identity/provider"
	"github.com/authzed/openagentprimitives/toolkits"
)

// TestShippedToolkitProvidersResolve is the general guard for a whole defect
// class, not one instance of it: a toolkit env var may name a `provider:`
// that does not exist in /providers/, and NOTHING rejects it. toolkit load
// validation checks that a sensitive env declares a `credential:` but never
// that its `provider:` resolves (pkg/tools/toolspec/toolkit/types.go's Provider
// field is an unvalidated string).
//
// A dangling reference is not cosmetic. `pkg/platform/identity/setup/engine.go` hard-
// errors with "requirement references unknown provider", so the `oap` setup
// flow for that credential fails outright; the identityd paste form renders
// no instructions and no docs URL; ValidateToken has no tokenShape to gate
// on, so a mis-pasted token is accepted and fails later as an opaque 401;
// and AuthFailureOrigins.RecordProvider drops the origin, leaving it
// permanently uncorroborable.
//
// This test is deliberately written over toolkits.All() rather than against
// a named provider, so it catches the NEXT dangling reference whichever
// toolkit introduces it.
func TestShippedToolkitProvidersResolve(t *testing.T) {
	tks := toolkits.All()
	require.NotEmpty(t, tks, "no shipped toolkits loaded; this test would pass vacuously")

	// Positive control: this test only proves anything if it actually
	// examined some provider references. If a future edit drops every
	// `provider:` from the catalog (or the field is renamed and silently
	// stops unmarshaling), the loop below would iterate over nothing and
	// pass while checking nothing at all.
	checked := 0

	for _, tk := range tks {
		for _, env := range tk.Env.Allowed {
			if env.Provider == "" {
				// The catalog's idiom for "no provider" is to omit the key
				// (toolkits/git.yaml's GIT_TOKEN). That is legitimate: the
				// credential is captured without a provider-specific flow.
				continue
			}
			checked++
			_, ok := provider.ByID(env.Provider)
			assert.Truef(t, ok,
				"toolkit %q env %q names provider %q, which does not exist in /providers/ — "+
					"`oap` setup for this credential hard-errors, the paste form renders no "+
					"instructions, and there is no tokenShape gate",
				tk.Name, env.Name, env.Provider)
		}
	}

	require.Positivef(t, checked,
		"examined %d toolkit provider references; expected at least one, otherwise this test asserts nothing",
		checked)
}

// TestShippedToolkitProviderReferences_Enumerated records WHICH references
// exist, so a reviewer reading a failure sees the specific (toolkit, env,
// provider) triple rather than only an aggregate count. Subtests are named
// per reference, so `go test -run` can target one.
func TestShippedToolkitProviderReferences_Enumerated(t *testing.T) {
	for _, tk := range toolkits.All() {
		for _, env := range tk.Env.Allowed {
			if env.Provider == "" {
				continue
			}
			name := fmt.Sprintf("%s/%s -> provider %s resolves", tk.Name, env.Name, env.Provider)
			t.Run(name, func(t *testing.T) {
				p, ok := provider.ByID(env.Provider)
				require.Truef(t, ok, "provider %q not found in /providers/", env.Provider)
				assert.Equalf(t, env.Provider, p.ID, "provider %q must report its own id", env.Provider)
			})
		}
	}
}
