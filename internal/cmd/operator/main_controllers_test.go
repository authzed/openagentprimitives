package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"sort"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestOperatorRegistersExpectedControllers walks internal/cmd/operator/main.go
// and verifies that every controller we expect to be wired into the
// manager is actually present at a SetupWithManager(mgr) call site.
//
// This is the regression for "added a new controller, forgot to wire
// it." A controller that fails to register doesn't crash the manager
// at startup — it just silently never reconciles. The slice-2
// ArtifactRender bug had a related shape: the controller WAS wired,
// but its CRD wasn't shipped, so the informer crashed during cache
// sync. The CRD-side regression is in
// TestKustomizationCoversEveryCRDFile; this test covers the wire-up
// side.
//
// Implementation: AST-walk main.go and count two things:
//
//  1. The number of CallExpr nodes whose selector ends in
//     "SetupWithManager" with a single "mgr" argument — the call
//     style every controller registration uses in this file.
//  2. The value of every log.Info("registered controller", "name", "X")
//     call's "X" string — the controller's human name.
//
// We assert both: (a) the count of SetupWithManager calls equals
// expectedRegistrations, and (b) the set of registered-controller
// log names equals expectedControllerNames. Drift in either
// direction (extra controller registered without a log; controller
// name in the log without a SetupWithManager) trips this test.
//
// Why AST vs. building a real ctrl.Manager and inspecting it: every
// controller in main has a different dependency set (executor, store,
// registry, gatewayEndpoint, memTokens, memStore, runnerImage, etc.)
// and an unguarded "build them all with fakes" registerControllers()
// helper would have to import every package and reproduce ~150 lines
// of main. The static check is cheap and catches the same regression
// (a SetupWithManager call removed, a controller added without a log).
func TestOperatorRegistersExpectedControllers(t *testing.T) {
	expectedControllerNames := map[string]bool{
		"AccessToken":                true,
		"SpiceboxClass":              true,
		"SpiceboxSession":            true,
		"SpiceboxToolchain":          true,
		"SpiceboxToolkit":            true,
		"SpiceboxToolspec":           true,
		"MCPServer":                  true,
		"AgentIdentity":              true,
		"AgentIdentityRefresh":       true,
		"ToolCall":                   true,
		"ArtifactRender":             true,
		"AgentClass":                 true,
		"Channel":                    true,
		"AgentSession":               true,
		"GuardianAgentSessionGrants": true,
		"MonitoringWatchers":         true,
		"UserIdentity":               true,
		"UserIdentityRefresh":        true,
		"SidecarToolbox":             true,
		"ClusterAgentSettings":       true,
		"AgentSettings":              true,
		"ClusterSkillSource":         true,
		"SkillSource":                true,
		"ClusterSkill":               true,
		"Skill":                      true,
		"ClusterIdentityProvider":    true,
		"WorkspaceSource":            true,
		"WorkspaceVolumeJanitor":     true,
		"CredentialUpdateRequest":    true,
		"AgentUI":                    true,
		"PublicEndpoint":             true,
		"SessionHold":                true,
		"SubagentRequest":            true,
		"Workshop":                   true,
		"WorkshopProbe":              true,
		"RelationshipSource":         true,
	}
	// MonitoringWatchers uses monitoringctrl.Register(mgr, ...) rather
	// than a direct SetupWithManager(mgr) call in main.go, so the
	// SetupWithManager count is one fewer than the full name set.
	expectedRegistrations := len(expectedControllerNames) - 1

	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "main.go", nil, parser.AllErrors)
	require.NoError(t, err, "parse main.go")

	var setupCalls int
	loggedNames := map[string]bool{}

	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel == nil {
			return true
		}

		// (1) Count `.SetupWithManager(mgr)` calls.
		if sel.Sel.Name == "SetupWithManager" && len(call.Args) == 1 {
			if id, ok := call.Args[0].(*ast.Ident); ok && id.Name == "mgr" {
				setupCalls++
			}
			return true
		}

		// (2) Capture `<x>.Info("registered controller", "name", "X", ...)`
		// — main uses both `log.Info` and (in branches) variations with
		// extra k/v pairs after the name (e.g. ToolCall, AgentClass).
		if sel.Sel.Name != "Info" || len(call.Args) < 3 {
			return true
		}
		msgLit, ok := call.Args[0].(*ast.BasicLit)
		if !ok || msgLit.Kind != token.STRING {
			return true
		}
		msg, err := strconv.Unquote(msgLit.Value)
		if err != nil || msg != "registered controller" {
			return true
		}
		keyLit, ok := call.Args[1].(*ast.BasicLit)
		if !ok || keyLit.Kind != token.STRING {
			return true
		}
		key, err := strconv.Unquote(keyLit.Value)
		if err != nil || key != "name" {
			return true
		}
		valLit, ok := call.Args[2].(*ast.BasicLit)
		if !ok || valLit.Kind != token.STRING {
			return true
		}
		name, err := strconv.Unquote(valLit.Value)
		if err != nil {
			return true
		}
		loggedNames[name] = true
		return true
	})

	// (1) SetupWithManager count.
	assert.Equal(t, expectedRegistrations, setupCalls,
		"found %d SetupWithManager(mgr) calls in main.go, expected %d. "+
			"A controller was likely added without a corresponding "+
			"registration, or one was removed without updating this test's "+
			"expectedControllerNames set.",
		setupCalls, expectedRegistrations)

	// (2) Names match exactly.
	missing := []string{}
	for want := range expectedControllerNames {
		if !loggedNames[want] {
			missing = append(missing, want)
		}
	}
	extra := []string{}
	for got := range loggedNames {
		if !expectedControllerNames[got] {
			extra = append(extra, got)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)

	assert.Empty(t, missing,
		"expected controllers not registered in main.go: %v — either the "+
			"SetupWithManager call was removed, or the log.Info(\"registered "+
			"controller\", \"name\", %q) line was renamed. Restore it or "+
			"update this test.",
		missing, missing)
	assert.Empty(t, extra,
		"unexpected controllers registered in main.go: %v — a new "+
			"controller was added without updating this test's "+
			"expectedControllerNames. Add it to the set so future removals "+
			"are caught.", extra)
}

// TestAgentIdentityReconcilerIsWiredWithAPlatformLinker pins the ONE production
// assignment of the AgentIdentity reconciler's PlatformLinker.
//
// Nothing else asserts it. Every test of the link write injects its own linker,
// so the whole feature can be correct at every tier and still be dead in a real
// cluster if this field is dropped here -- and the failure is SILENT in exactly
// the way the schema's own doc warns about: the reconciler logs "PlatformLinker
// not configured" and continues, the identity converges Valid=True, the card
// publishes, the button renders, and every admin's click is refused. That is
// the same shape as the two shipped incidents this repo's static-wiring test
// already exists to catch (a controller registered without its CRD, a
// controller never registered at all), so it belongs in the same file.
//
// AST rather than a constructed manager, for the reason this file's other test
// documents: main's dependency set is enormous and a "build them all with
// fakes" helper would reproduce ~150 lines of main.
//
// The assertion is deliberately on the IDENTIFIER as well as the field: a
// PlatformLinker set to anything other than the real client (a nil variable, a
// placeholder) is the typed-nil hazard AGENTS.md has a production incident
// from, and "the key is present" alone would not catch it.
func TestAgentIdentityReconcilerIsWiredWithAPlatformLinker(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "main.go", nil, parser.AllErrors)
	require.NoError(t, err, "parse main.go")

	found := false
	var linkerValue string

	ast.Inspect(f, func(n ast.Node) bool {
		lit, ok := n.(*ast.CompositeLit)
		if !ok {
			return true
		}
		sel, ok := lit.Type.(*ast.SelectorExpr)
		if !ok || sel.Sel == nil || sel.Sel.Name != "Reconciler" {
			return true
		}
		pkg, ok := sel.X.(*ast.Ident)
		if !ok || pkg.Name != "agentidentity" {
			return true
		}
		found = true
		for _, el := range lit.Elts {
			kv, ok := el.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			key, ok := kv.Key.(*ast.Ident)
			if !ok || key.Name != "PlatformLinker" {
				continue
			}
			if id, ok := kv.Value.(*ast.Ident); ok {
				linkerValue = id.Name
			} else {
				linkerValue = "<not a plain identifier>"
			}
		}
		return true
	})

	require.True(t, found,
		"no agentidentity.Reconciler{...} literal in main.go — the operator no longer registers it, "+
			"or it moved out of this file and this guard needs to move with it")
	require.NotEmpty(t, linkerValue,
		"agentidentity.Reconciler is constructed WITHOUT PlatformLinker. agentidentity#update_credential is then "+
			"unsatisfiable for every identity in the cluster and the failure is silent: the reconciler logs a skip, "+
			"the identity is Valid, the credential-update card publishes, and every admin's click is refused")
	assert.Equal(t, "spiceDBClient", linkerValue,
		"PlatformLinker must be the real *spicedb.Client main constructs (and exits on failure to construct), "+
			"not a variable that may still be nil: a typed-nil pointer in an interface field is a NON-nil interface "+
			"that panics on first call rather than taking the nil branch")
}
