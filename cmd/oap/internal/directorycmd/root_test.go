package directorycmd_test

import (
	"bytes"
	"go/parser"
	"go/token"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic"
	k8stesting "k8s.io/client-go/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/directorycmd"
)

// runCfg is what runCmd's opts customize before the command runs.
type runCfg struct {
	dyn dynamic.Interface
	ns  string
}

// runOpt customizes one runCmd invocation.
type runOpt func(*runCfg)

// withDyn stubs the dynamic client `oap directory` resolves for this run.
// The namespace is "default" — the same one fakeDyn's fixtures (and its
// callers across this package's other test files) assume.
func withDyn(dyn dynamic.Interface) runOpt {
	return func(c *runCfg) { c.dyn = dyn }
}

// runCmd executes `oap directory <arg>` against this package's own subtree,
// stubbing directorycmd.ClientFactory so no cluster is needed — mirrors
// settingscmd's runSettings (cmd/oap/internal/settingscmd/helpers_test.go).
func runCmd(t *testing.T, arg string, opts ...runOpt) string {
	t.Helper()
	cfg := &runCfg{ns: "default"}
	for _, o := range opts {
		o(cfg)
	}

	prev := directorycmd.ClientFactory
	t.Cleanup(func() { directorycmd.ClientFactory = prev })
	// A nil controller-runtime client: `list` never writes a credential, and a
	// genuine nil interface (not a typed-nil pointer) is what NewDeps checks
	// to decide whether this run can create one at all.
	directorycmd.ClientFactory = func(*apcmd.Globals) (dynamic.Interface, client.Client, string, error) {
		return cfg.dyn, nil, cfg.ns, nil
	}

	cmd := directorycmd.NewCmd(&apcmd.Globals{})
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{arg})
	err := cmd.Execute()
	require.NoErrorf(t, err, "oap directory %s; out=%s", arg, buf.String())
	return buf.String()
}

// `list` is the read-only answer to "what is syncing here?" and must work
// before anything is configured.
func TestDirectoryList_ReportsNothingConfigured(t *testing.T) {
	out := runCmd(t, "list", withDyn(fakeDyn(t)))
	assert.Contains(t, out, "no directory sources")
}

// A kind with more than one RelationshipSource is a legitimate shape
// ExistingFor correctly refuses to guess at (github.com alongside a GHES
// host) — but `list` is read-only, and that per-kind refusal must not blank
// out every OTHER kind's perfectly good listing. Two github CRs plus one
// clean slack CR: the slack source must still appear.
func TestDirectoryList_AmbiguousKindStillListsOtherSources(t *testing.T) {
	dyn := fakeDyn(t,
		relationshipSource("gh-com", "github", withAuth("gh-id", "gh-cred")),
		relationshipSource("gh-ghes", "github", withAuth("gh-id", "gh-cred")),
		relationshipSource("slack-dir", "slack", withAuth("sl-id", "sl-cred")))

	out := runCmd(t, "list", withDyn(dyn))

	// Assert on a composed LINE, never on a kind name alone. runList always
	// prints every registered kind's bare name in its "Registered
	// directory-sync kinds" trailer, so Contains(out, "slack") holds even when
	// the configured line was never emitted at all — only text the trailer
	// cannot produce tells "listed" apart from "merely registered".
	assert.Contains(t, out, "- slack-dir (kind: slack, credential: sl-id/sl-cred)",
		"the clean slack source must still be listed")
	// Prefix only: AmbiguousSourceError.Names is in List order, which the fake
	// tracker does not promise to stabilize, so the names are checked
	// separately rather than pinned into one ordered string.
	assert.Contains(t, out, "- github: ambiguous (2 sources: ",
		"the ambiguous kind must report its ambiguity rather than vanish")
	assert.Contains(t, out, "gh-com", "the ambiguity notice must name the competing sources")
	assert.Contains(t, out, "gh-ghes")
}

// A genuine API failure (the cluster unreachable, a List call erroring for
// a reason that has nothing to do with ambiguity) must still abort the
// whole listing — silently omitting a kind because the API call failed
// would be worse than a hard error.
func TestDirectoryList_AbortsOnGenuineAPIFailure(t *testing.T) {
	dyn := fakeDyn(t)
	dyn.PrependReactor("list", "relationshipsources", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apiFailure
	})

	prev := directorycmd.ClientFactory
	t.Cleanup(func() { directorycmd.ClientFactory = prev })
	directorycmd.ClientFactory = func(*apcmd.Globals) (dynamic.Interface, client.Client, string, error) {
		return dyn, nil, "default", nil
	}

	cmd := directorycmd.NewCmd(&apcmd.Globals{})
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"list"})
	err := cmd.Execute()
	require.Error(t, err, "a genuine API failure must abort the listing, not be swallowed")
}

// apiFailure stands in for a genuine cluster/API error — deliberately not
// wrapping directorycmd.AmbiguousSourceError, so a caller that only checks
// for that sentinel correctly treats this as fatal.
var apiFailure = assert.AnError

// TestDirectoryRootGo_BlankImportsEveryRelsyncKind is the task-7 ruling's
// kind-coverage guard — moved here as a STRUCTURAL check on root.go's own
// source, after two behavioral attempts were each proven, by overlay, unable
// to ever go red:
//
//  1. Iterating relsync.All() inside this package's own test binary: this
//     directory's test binary links `package directorycmd` and
//     `package directorycmd_test` together (Go compiles every `_test.go` in
//     one directory into one binary), and wizard_test.go's own blank imports
//     of github/onepassword/slack (needed for ITS tests, which exercise
//     those kinds by name) keep all three registered here regardless of
//     what root.go imports.
//  2. Driving the real assembled `oap` binary (NewRootCmd(), or
//     `go list -deps ./cmd/oap`) and checking its registered kinds or its
//     `oap directory list` output: cmd/oap/main.go already blank-imports
//     pkg/authz/spicedb/relsource/imports for a wholly unrelated reason
//     (completing the SpiceDB relsource claim table so guarded writes like
//     `oap memory share` work), and THAT package blank-imports
//     github/onepassword/slack too — every relsync.Kind's Source() must
//     carry real Claims (relsync.Kind's own doc), so every relsync kind ends
//     up listed there as well. Commenting out root.go's own onepassword
//     import and running `go list -deps ./cmd/oap | grep onepassword`
//     confirmed it: the package was still reported as a dependency, and a
//     behavioral test driven through NewRootCmd() stayed green.
//
// Both of those check whether the KIND ENDS UP REGISTERED somewhere in a
// given binary, which — thanks to relsource/imports's unrelated but total
// coverage — is true in the real `oap` binary no matter what root.go itself
// imports. The only way to catch root.go regressing away from being the
// explicit, locally-legible wiring site (the convention
// internal/cmd/operator/main.go's own comment establishes: an explicit
// import "not an accident of" a transitive one) is to check root.go's own
// import block directly, at the source level — which is what this does:
// parses root.go, collects its blank ("_") imports, and asserts the three
// expected channelkinds packages are named there BY ROOT.GO ITSELF, not
// merely present somewhere in whatever binary happens to run this test.
//
// Verified against the same overlay that defeated the two behavioral
// attempts: commenting out root.go's onepassword blank import made this
// test fail with "root.go must blank-import
// .../channelkinds/onepassword directly"; restoring the import made it pass
// again.
func TestDirectoryRootGo_BlankImportsEveryRelsyncKind(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "root.go", nil, parser.ImportsOnly)
	require.NoError(t, err, "parse root.go's own import block")

	blank := map[string]bool{}
	for _, imp := range f.Imports {
		if imp.Name == nil || imp.Name.Name != "_" {
			continue // not a blank import; irrelevant to registration
		}
		path, err := strconv.Unquote(imp.Path.Value)
		require.NoErrorf(t, err, "unquote import path %s", imp.Path.Value)
		blank[path] = true
	}

	for _, kind := range []string{"github", "onepassword", "slack"} {
		path := "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/" + kind
		assert.Truef(t, blank[path],
			"root.go must blank-import %s directly — the real `oap` binary "+
				"happens to link every relsync kind transitively via "+
				"relsource/imports today, so nothing behavioral would catch "+
				"this import being dropped from root.go itself", path)
	}
}
