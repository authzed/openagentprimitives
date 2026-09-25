package all_test

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/memory"
	_ "github.com/authzed/openagentprimitives/pkg/memory/kinds/all"
)

// sessionWritable is the reviewed answer to "which Kinds may an agent session
// author with its own credential?".
//
// It is NOT the enforcement path — memory.Local reads each Kind's own
// WriteAuthority(), so nothing here can grant or deny anything. It is a review
// gate: widening a session's authorship surface is a security decision, and
// pinning the set here means it cannot be made silently inside a Kind's package
// where a reviewer looking at a one-line diff sees "SessionWritten" with no
// context for what else already is.
//
// Adding a Kind whose writer is the runner? Add it here in the same change. If
// you find yourself adding a Kind that a COMPONENT reads to make an
// authorization decision, that is the case to stop and reconsider.
var sessionWritable = map[string]string{
	"approval":             "runner: its own tool-call approval requests and outcomes",
	"artifact":             "runner: the agent's artifacts, via pkg/platform/artifacts",
	"artifact_revision":    "runner: written alongside its artifact head",
	"authz_decision":       "runner: the authorization outcome of its own tool calls",
	"contentguard_audit":   "runner: its own content-guard verdicts",
	"infoleakage_audit":    "runner: its own info-leakage events",
	"infoleakage_decision": "runner: the audience decisions its own hooks produce",
	"infoleakage_taint":    "runner: taint marks on content its own tools read",
	"label":                "runner: resource labels resolved during its own dispatches",
	"lifecycle":            "runner: its own phase transitions (operator/authzd append on other doors)",
	"observation":          "runner: notes the agent writes into a resource-scoped pool it holds a write_memory slot on; nothing reads one to authorize anything, a later session's own model reads it like any other tool result",
	"observed_fact":        "runner: facts it derives from its own tool results via a declared observes block",
	"plan_gate_audit":      "runner: the PlanGate hook's own gate/allow/deny/card records, constructed in-process via hooks.NewPlanGate (pkg/agent/runner/hooks_dataplane.go)",
	"relwrites_audit":      "runner: relationship writes its own MCP dispatches performed",
	"session_scope":        "runner: BindClassDefaults — see the residual on the Kind",
	"system_prompt":        "runner: the composed system prompt for its own turn, via systemprompt.Record in internal/cmd/runner/main.go",
	"tool_catalog":         "runner: the tool catalog it actually offered on each Send, via toolcatalog.Record in pkg/agent/runner/loop.go",
	"tool_session":         "runner: its own tool-session bookkeeping",
	"toolguard_audit":      "runner: its own toolguard verdicts",
	"turn":                 "runner + oap + channelsd: the transcript",
	"ui_action":            "runner: the UI-action lifecycle it drives (display projection, never an authz input)",
	"ui_view_model":        "runner: the agent's own Tier-1 view-model, via update_view",
	// Never an authorization input, which is the question this pin asks. The
	// stored keys are admitted only if the CURRENT declaration's own controls
	// drive them (uicomponents.ParamKeys) and the browser re-filters them
	// against the declaration it renders, so a forged key names a control that
	// does not exist and is dropped. The VALUES reach tool args — but by
	// exactly the route a viewer's own control does, through the same
	// per-binding authorization on every resolve. Setting one buys the agent
	// nothing it could not do by calling the tool itself.
	"ui_view_params": "runner: the agent's own binding-parameter choices, via set_view_params",
}

// TestEveryKind_DeclaresAKnownWriteAuthority is the drift guard.
//
// The declaration itself is compile-enforced — WriteAuthority is a method on
// memory.Kind, so a new Kind cannot be registered without answering. What a
// compiler cannot check is that the answer is a value the enforcement path
// recognizes: memory.Local permits only an exact SessionWritten, so any other
// value is silently ComponentWritten. That is the safe direction, but a Kind
// meant to be session-writable and holding, say, WriteAuthority(2) would look
// declared and behave denied.
func TestEveryKind_DeclaresAKnownWriteAuthority(t *testing.T) {
	kinds := memory.RegisteredKinds()
	require.NotEmpty(t, kinds, "no Kinds registered — every assertion below would be vacuous")

	for _, k := range kinds {
		t.Run(k.Name()+": declares a known WriteAuthority", func(t *testing.T) {
			w := k.WriteAuthority()
			assert.Contains(t, []memory.WriteAuthority{memory.ComponentWritten, memory.SessionWritten}, w,
				"WriteAuthority() returned %s, which the write door treats as component-written; "+
					"return one of the declared constants", w)
		})
	}
}

// TestSessionWritableSet_MatchesTheReviewedPin fails in BOTH directions: a Kind
// that became session-writable without being listed, and a listed Kind that is
// no longer session-writable. Either way the change is visible in this file's
// diff, which is the point.
func TestSessionWritableSet_MatchesTheReviewedPin(t *testing.T) {
	got := map[string]bool{}
	for _, k := range memory.RegisteredKinds() {
		if k.WriteAuthority() == memory.SessionWritten {
			got[k.Name()] = true
		}
	}
	require.NotEmpty(t, got, "no session-writable Kind at all would mean the runner cannot write its own transcript")

	for name := range got {
		assert.Contains(t, sessionWritable, name,
			"Kind %q declares itself session-writable but is not in the reviewed set. A session credential "+
				"is driven by model output and by whatever content its tools pulled in; if any component "+
				"reads this Kind to make an authorization decision, it must be ComponentWritten. If the "+
				"runner genuinely authors it, add it to sessionWritable with the writer named.", name)
	}
	for name, why := range sessionWritable {
		k, ok := memory.LookupKind(name)
		if !assert.Truef(t, ok, "sessionWritable names %q (%s), which is not a registered Kind; drop the stale row", name, why) {
			continue
		}
		assert.Equalf(t, memory.SessionWritten, k.WriteAuthority(),
			"sessionWritable lists %q (%s) but the Kind now declares %s; if that tightening is intended, "+
				"remove the row — and check the writer it names has moved", name, why, k.WriteAuthority())
	}
}

// TestAll_ImportsEveryKindPackage guards the registry membership itself.
//
// parked_prompt was missing from all.go, so the OPERATOR — the process that
// serves the memory API, and whose registry is the only one that decides
// whether a Put is storable — never registered it, and every channelsd
// parked-prompt write was answered 400 "unknown Kind". No test noticed, because
// channelsd's own binary registers the Kind through a direct import and the
// Kind's package tests use a local facade. The blank-import list is a
// hand-maintained mirror of a directory; this asserts the mirror is complete
// in BOTH directions.
//
// "Complete" is DERIVED from source, not from a hand-picked directory-name
// allowlist: a directory under kinds/ requires the blank import iff one of
// its .go files calls memory.RegisterKind. An early version of this test
// instead special-cased `case "factcontent": continue` — factcontent shares
// no Kind of its own, only the Content struct envelopefact and observedfact
// both use — but a literal exclusion is exactly the failure mode this guard
// exists to catch: nothing would have stopped factcontent from later
// growing a RegisterKind call the guard had been told to skip, the same way
// parked_prompt was once silently skipped. Deriving from the RegisterKind
// call itself means the guard notices that on its own, the day it happens,
// rather than trusting a name on a list to still be accurate.
//
// "internal" stays a literal exclusion, and that is not the same shortcut:
// Go's own import rule makes it structurally unreachable from any binary
// outside pkg/memory/kinds/, so it can never be the thing a binary "forgets"
// to blank-import — there is no failure mode here for a heuristic to paper
// over. That rule is also why factcontent cannot simply move out from under
// kinds/ to sidestep this guard the way an ordinary pkg/ package could: it
// needs kinds/internal/undecodable's helpers, and moving factcontent to,
// say, pkg/memory/factcontent would make that import unreachable too. Do
// not re-attempt that move; derive the exclusion instead, as this test now
// does.
func TestAll_ImportsEveryKindPackage(t *testing.T) {
	dirs, err := os.ReadDir("..")
	require.NoError(t, err, "read pkg/memory/kinds")

	imported := importedPackages(t, "all.go")
	require.NotEmpty(t, imported, "parsed no imports out of all.go — the guard would be vacuous")
	importedSet := make(map[string]bool, len(imported))
	for _, p := range imported {
		importedSet[p] = true
	}

	var checked int
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		switch d.Name() {
		case "all":
			continue // itself
		case "internal":
			continue // shared accessor helpers; Go's import rule makes this structurally not-a-Kind
		}
		checked++

		if dirRegistersAKind(t, d.Name()) {
			assert.Containsf(t, importedSet, d.Name(),
				"pkg/memory/kinds/%s calls memory.RegisterKind but is not blank-imported by kinds/all, so no "+
					"binary that relies on this bundle registers its Kind — writes of it are answered 400 "+
					"\"unknown Kind\" by the operator",
				d.Name())
		} else {
			assert.NotContainsf(t, importedSet, d.Name(),
				"pkg/memory/kinds/%s is blank-imported by kinds/all but calls no memory.RegisterKind — either "+
					"the import is dead weight in the bundle, or the registration moved somewhere this "+
					"directory-local scan cannot see; either way the import needs a reason or removal",
				d.Name())
		}
	}
	require.NotZero(t, checked, "found no Kind package directories — the guard would be vacuous")
}

// dirRegistersAKind reports whether any NON-TEST .go file directly inside
// pkg/memory/kinds/<name> calls memory.RegisterKind.
//
// A textual scan, not an AST walk: the only question is "does this package
// register a Kind", and every Kind in this codebase answers it the same way
// — `func init() { memory.RegisterKind(Kind{}) }`, un-wrapped, with the
// `memory` import never shadowed or aliased — so a substring match is exact
// for every case that exists today and fails loud (a missed Contains/
// NotContains below) rather than silent if that convention is ever broken.
//
// _test.go files are excluded, and skipping that exclusion defeats the guard
// outright. Tests register Kinds into their own process for their own
// fixtures — kgingestion/hooks_test.go and hooks_approval_test.go both call
// memory.RegisterKind today — but a registration a test performs is invisible
// to every BINARY, which is the only thing this guard is about. Counting one
// would mean deleting kgingestion/kind.go's own init left the guard green
// with the Kind unregistered in the operator: precisely the parked_prompt
// failure the test exists to catch, hidden by the test file that happens to
// sit beside it.
func dirRegistersAKind(t *testing.T, name string) bool {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join("..", name))
	require.NoError(t, err, "read pkg/memory/kinds/%s", name)

	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		b, err := os.ReadFile(filepath.Join("..", name, e.Name()))
		require.NoError(t, err, "read %s/%s", name, e.Name())
		if strings.Contains(string(b), "memory.RegisterKind(") {
			return true
		}
	}
	return false
}

// importedPackages returns the last path segment of every import in file.
func importedPackages(t *testing.T, file string) []string {
	t.Helper()
	parsed, err := parser.ParseFile(token.NewFileSet(), file, nil, parser.ImportsOnly)
	require.NoError(t, err, "parse %s", file)

	var out []string
	for _, imp := range parsed.Imports {
		path, uerr := strconv.Unquote(imp.Path.Value)
		require.NoError(t, uerr, "unquote import path %s", imp.Path.Value)
		if !strings.Contains(path, "/memory/kinds/") {
			continue
		}
		out = append(out, path[strings.LastIndex(path, "/")+1:])
	}
	return out
}
