package meta_test

// One conformance sweep over every meta tool's DESCRIPTOR surface — Name,
// Kind, Description, InputSchema, Permission, PermissionVariants.
//
// Individually those are one-line constant returns and testing each in
// isolation would be busywork. Collectively they are not: they are the tool's
// contract with the model (Description and InputSchema are prompt — the model
// sees nothing else about the tool) and with the authorization pipeline
// (StateImpact decides whether the call is checked, approved, or dispatched
// unexamined). A typo'd or empty StateImpact silently falls through every
// switch in pkg/authz/hooks and the call dispatches with no gate at all, so
// "is it one of the five declared values" is a fail-closed guard, not a
// formality.
//
// Written as one table over all constructors, and the table is checked against
// the package's own source (TestMetaTools_EveryConstructorIsSwept), so a NEW
// meta tool is covered the moment it is written — its absence from the table is
// itself a failure, naming the constructor. The table used to be its own
// reminder and drifted nine constructors behind, return_result among them.

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/meta"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

// allMetaTools constructs every meta tool with zero-value config, save the one
// whose constructor gates on a non-zero field (see its row). Descriptor
// accessors must not touch collaborators, so a zero config is enough to ask
// each tool what it is — and a constructor that panicked here would itself be
// the finding.
//
// Keyed by CONSTRUCTOR name, not by tool name, because that is what
// TestMetaTools_EveryConstructorIsSwept can compare against the package's own
// source: a constructor added without a row here fails that test by name. The
// map replaced a bare slice after the list was found nine constructors short —
// return_result (terminal AND gate-carrying, so unguarded here was exactly the
// fail-closed StateImpact hole this file exists to close), both trigger-status
// tools, select_phase, complete_phase, delegate, reply_to_subagent, ask_parent
// and the mention lookup.
func allMetaTools(t *testing.T) map[string]tool.Tool {
	t.Helper()
	return map[string]tool.Tool{
		"NewAgentWorkComplete":     meta.NewAgentWorkComplete(meta.CompletionConfig{}),
		"NewApplyWorkspace":        meta.NewApplyWorkspace("", "", "", "", "", "", "", ""),
		"NewArtifactAwait":         meta.NewArtifactAwait(meta.ArtifactAwaitConfig{}),
		"NewArtifactHistory":       meta.NewArtifactHistory(meta.ArtifactHistoryConfig{}),
		"NewArtifactOfferView":     meta.NewArtifactOfferView(meta.ArtifactOfferViewConfig{}),
		"NewArtifactPrepare":       meta.NewArtifactPrepare(meta.ArtifactPrepareConfig{}),
		"NewAskParent":             meta.NewAskParent(meta.AskParentConfig{}),
		"NewRequestInput":          meta.NewRequestInput(meta.RequestInputConfig{}),
		"NewDeriveTag":             meta.NewDeriveTag(meta.DeriveTagConfig{}),
		"NewAwait":                 meta.NewAwait(meta.AwaitConfig{}),
		"NewClaimTriggerStatus":    meta.NewClaimTriggerStatus(meta.TriggerStatusConfig{}),
		"NewCompletePhase":         meta.NewCompletePhase(meta.CompletePhaseConfig{}),
		"NewConcludeTriggerStatus": meta.NewConcludeTriggerStatus(meta.TriggerStatusConfig{}),
		"NewCredentialUpdate":      meta.NewCredentialUpdate(meta.CredentialUpdateConfig{}),
		"NewDelegateTool":          meta.NewDelegateTool(meta.DelegateConfig{}),
		"NewIntrospect":            meta.NewIntrospect(meta.IntrospectConfig{}),
		"NewLoadSkill":             meta.NewLoadSkill(nil),
		// The one constructor a ZERO config does not produce a tool from: a nil
		// Kind is its wiring gate, and returning nil there is the behaviour
		// TestNewLookupUserForMention_NilIsTheWiringGate pins. Given the
		// smallest config that DOES yield a tool, so its descriptor is swept
		// like every other.
		"NewLookupUserForMention": meta.NewLookupUserForMention(meta.LookupUserForMentionConfig{
			Kind: &stubKind{supports: []channelkinds.MentionLookupKind{channelkinds.MentionLookupEmail}},
		}),
		"NewGetPreferences":       meta.NewGetPreferences(nil),
		"NewQueryKnowledge":       meta.NewQueryKnowledge(),
		"NewQueryMemory":          meta.NewQueryMemory(),
		"NewReadChannelHistory":   meta.NewReadChannelHistory(meta.ReadChannelHistoryConfig{}),
		"NewReadHistory":          meta.NewReadHistory(meta.ReadHistoryConfig{}),
		"NewReadView":             meta.NewReadView(meta.ReadViewConfig{}),
		"NewRecordObservation":    meta.NewRecordObservation(),
		"NewReturnResult":         meta.NewReturnResult(meta.CompletionConfig{}),
		"NewSearchMemory":         meta.NewSearchMemory(),
		"NewSelectPhase":          meta.NewSelectPhase(meta.SelectPhaseConfig{}),
		"NewSendInputTool":        meta.NewSendInputTool(meta.SendInputConfig{}),
		"NewSetPreference":        meta.NewSetPreference(nil, nil),
		"NewSetThreadTitle":       meta.NewSetThreadTitle(meta.SetThreadTitleConfig{}),
		"NewSetViewParams":        meta.NewSetViewParams(meta.SetViewParamsConfig{}),
		"NewShowAgentUI":          meta.NewShowAgentUI(meta.ShowAgentUIConfig{}),
		"NewShowAttachment":       meta.NewShowAttachment(meta.ShowAttachmentConfig{}),
		"NewSubagentReplyTool":    meta.NewSubagentReplyTool(meta.DelegateConfig{}),
		"NewSyncWorkspace":        meta.NewSyncWorkspace("", "", "", "", "", "", ""),
		"NewUpdateOpeningSummary": meta.NewUpdateOpeningSummary(meta.UpdateOpeningSummaryConfig{}),
		"NewUpdatePlan":           meta.NewUpdatePlan(meta.UpdatePlanConfig{}),
		"NewUpdateStatus":         meta.NewUpdateStatus(meta.UpdateStatusConfig{}),
		"NewUpdateView":           meta.NewUpdateView(meta.UpdateViewConfig{}),
		"New":                     meta.New(meta.RespondConfig{}),
	}
}

// declaredStateImpacts is the closed set from pkg/authz. A value outside it
// (including the empty string) makes needsApproval and CheckRequired both
// answer false, and the call dispatches ungated.
var declaredStateImpacts = map[authz.StateImpact]bool{
	authz.Stateless:   true,
	authz.Passthrough: true,
	authz.Readonly:    true,
	authz.Readwrite:   true,
	authz.External:    true,
}

func TestMetaTools_DescriptorConformance(t *testing.T) {
	for _, tl := range allMetaTools(t) {
		require.NotNil(t, tl, "every constructor in the list must yield a tool")

		t.Run(tl.Name()+": descriptor is well-formed", func(t *testing.T) {
			assert.NotEmpty(t, tl.Name(), "an unnamed tool cannot be dispatched or looked up")
			assert.Equal(t, tool.KindMeta, tl.Kind(), "every tool in this package is a meta tool")

			// Description is prompt: it is all the model is told about the tool.
			assert.NotEmpty(t, tl.Description(),
				"an empty description leaves the model guessing what the tool does")

			var schema map[string]any
			require.NoError(t, json.Unmarshal(tl.InputSchema(), &schema),
				"InputSchema must be valid JSON — a provider rejects the whole tool list otherwise")
			assert.Equal(t, "object", schema["type"],
				"an LLM tool schema's top level must be an object")

			p := tl.Permission()
			assert.True(t, declaredStateImpacts[p.StateImpact],
				"StateImpact %q is outside the declared set; it would fall through every authz switch "+
					"and dispatch the call ungated", p.StateImpact)

			// A Check that names no resource or no permission checks nothing.
			if p.Check != nil {
				assert.NotEmpty(t, p.Check.ResourceType, "a Check must name the SpiceDB definition it evaluates")
				assert.NotEmpty(t, p.Check.Permission, "a Check must name the permission it evaluates")
			}

			for i, v := range tl.PermissionVariants() {
				assert.NotEmpty(t, v.When, "variant %d has an empty When and can never match deterministically", i)
				assert.True(t, declaredStateImpacts[v.Check.StateImpact],
					"variant %d declares StateImpact %q, outside the declared set", i, v.Check.StateImpact)
			}
		})
	}
}

// TestMetaTools_NamesAreUnique guards the dispatcher: tool.LookupByName resolves
// duplicates FIRST-WINS, so two tools sharing a name means one is unreachable
// and which one wins depends on slice order.
func TestMetaTools_NamesAreUnique(t *testing.T) {
	seen := map[string]bool{}
	for _, tl := range allMetaTools(t) {
		name := tl.Name()
		assert.False(t, seen[name], "duplicate meta tool name %q: one of the two would be unreachable", name)
		seen[name] = true
	}
}

// TestMetaTools_DescriptorsAreStable pins the accessors as pure: the runner
// reads them once to build the tool list and again on later lookups, so a
// descriptor that changed between calls would let the model's schema and the
// dispatcher's gate disagree about the same tool.
func TestMetaTools_DescriptorsAreStable(t *testing.T) {
	for _, tl := range allMetaTools(t) {
		t.Run(tl.Name()+": repeated reads agree", func(t *testing.T) {
			assert.Equal(t, tl.Name(), tl.Name())
			assert.Equal(t, tl.Description(), tl.Description())
			assert.JSONEq(t, string(tl.InputSchema()), string(tl.InputSchema()))
			assert.Equal(t, tl.Permission(), tl.Permission())
		})
	}
}

// TestMetaTools_EveryConstructorIsSwept is what makes the sweep above a GUARD
// rather than a list.
//
// The header says "a NEW meta tool is covered the moment it is added to the
// list — and the list itself is the reminder". A list is a poor reminder: this
// one drifted nine constructors behind the package, and the one it most needed
// (return_result, which is terminal AND carries the completion gate) was among
// them, so the fail-closed StateImpact check never ran on it. That is precisely
// the hole this file was written to close, sitting open behind a green test.
//
// So the roster is checked against the package's OWN SOURCE: every exported
// top-level function in package meta that returns a tool.Tool must have a row
// in allMetaTools. A constructor added tomorrow fails HERE, by name, with
// nothing to remember.
//
// Parsing the directory rather than using reflection because a constructor is
// only reachable by reflection once something calls it — which is the very
// thing being asserted. `go test` runs with the package directory as its
// working directory, so "." is package meta's source.
func TestMetaTools_EveryConstructorIsSwept(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi os.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	require.NoError(t, err, "parsing package meta's own source")

	pkg, ok := pkgs["meta"]
	require.True(t, ok, "package meta not found in the working directory; go test's cwd is the package dir")

	found := map[string]bool{}
	for _, f := range pkg.Files {
		for _, decl := range f.Decls {
			fn, isFunc := decl.(*ast.FuncDecl)
			if !isFunc || fn.Recv != nil || !fn.Name.IsExported() {
				continue // methods and unexported helpers are not constructors
			}
			if !isToolConstructor(fn) {
				continue
			}
			found[fn.Name.Name] = true
		}
	}
	require.NotEmpty(t, found,
		"no tool.Tool constructors found by parsing — the walk itself is broken, and an empty "+
			"set would make the comparison below vacuously pass")

	swept := allMetaTools(t)
	for name := range found {
		assert.Containsf(t, swept, name,
			"meta.%s returns a tool.Tool but has no row in allMetaTools, so its descriptor — "+
				"Description and InputSchema (the model's whole view of it) and StateImpact (whether "+
				"the call is gated at all) — is unasserted", name)
	}
	for name := range swept {
		assert.Truef(t, found[name],
			"allMetaTools has a row for meta.%s, which package meta no longer exports as a "+
				"tool.Tool constructor; drop the row", name)
	}
}

// isToolConstructor reports whether fn CONSTRUCTS a meta tool: a single result
// of the qualified type tool.Tool, and a New* name.
//
// The name half is what separates a constructor from a lookup — LookupForTest
// also returns a tool.Tool, but it returns one somebody else already built and
// registered, so sweeping its descriptor would be sweeping a row that is
// already in the map under its own constructor. Every constructor in this
// package is named New or New<Thing>; one that is not would be the more
// surprising thing, and renaming it out of this guard's sight is not a mistake
// a reader could make by accident.
func isToolConstructor(fn *ast.FuncDecl) bool {
	if !strings.HasPrefix(fn.Name.Name, "New") {
		return false
	}
	if fn.Type.Results == nil || len(fn.Type.Results.List) != 1 {
		return false
	}
	sel, ok := fn.Type.Results.List[0].Type.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Tool" {
		return false
	}
	ident, ok := sel.X.(*ast.Ident)
	return ok && ident.Name == "tool"
}

// TestApplyWorkspace_IsExternalSoEveryCallIsApproved pins the single most
// consequential StateImpact declaration in the package. apply_workspace pushes
// the agent's edits back to the source origin; External is what makes
// needsApproval return true unconditionally, including under a permissive
// AgentClass toolAuthMode. Weakening it to Passthrough or Readwrite would let
// the agent write to the origin with no human in the loop.
func TestApplyWorkspace_IsExternalSoEveryCallIsApproved(t *testing.T) {
	tl := meta.NewApplyWorkspace("git", "example.com/demo-org/demo-repo", "main", "/workspace", "pvc", "", "", "")

	assert.Equal(t, authz.External, tl.Permission().StateImpact,
		"apply_workspace must route through human approval on every call")
	assert.True(t, tl.Permission().StateImpact.CheckRequired(),
		"External is in the check-required class")
}

// TestSyncWorkspace_IsPassthroughAndNotApprovalGated is the counterpart: sync
// only pulls into the session's own overlay, so it is deliberately exempt.
// Pinned so the two cannot be transposed.
func TestSyncWorkspace_IsPassthroughAndNotApprovalGated(t *testing.T) {
	tl := meta.NewSyncWorkspace("git", "example.com/demo-org/demo-repo", "main", "/workspace", "pvc", "", "")

	assert.Equal(t, authz.Passthrough, tl.Permission().StateImpact)
	assert.False(t, tl.Permission().StateImpact.CheckRequired(),
		"a pull into the session's own overlay needs no resource check")
}

// TestNewLookupUserForMention_NilIsTheWiringGate covers the one constructor
// that returns nil by design. The runner appends only non-nil tools, so the nil
// must be a GENUINE nil interface — a typed-nil pointer would compare non-nil,
// be appended, and panic on the first descriptor read (AGENTS.md, "Nil
// interfaces: never assign a typed-nil pointer directly").
func TestNewLookupUserForMention_NilIsTheWiringGate(t *testing.T) {
	t.Run("no bound channel kind: genuine nil interface, not a typed nil", func(t *testing.T) {
		got := meta.NewLookupUserForMention(meta.LookupUserForMentionConfig{})
		assert.Nil(t, got, "a nil Kind must gate the tool out of the toolset entirely")
	})
	t.Run("a kind advertising no lookups: genuine nil interface", func(t *testing.T) {
		got := meta.NewLookupUserForMention(meta.LookupUserForMentionConfig{Kind: &stubKind{supports: nil}})
		assert.Nil(t, got, "a kind with no supported lookups must gate the tool out")
	})
	t.Run("a kind advertising a lookup: the tool is wired in and conforms", func(t *testing.T) {
		got := meta.NewLookupUserForMention(meta.LookupUserForMentionConfig{
			Kind: &stubKind{supports: []channelkinds.MentionLookupKind{channelkinds.MentionLookupEmail}},
		})
		require.NotNil(t, got, "a kind that supports a lookup must yield the tool")
		assert.NotEmpty(t, got.Name())
		assert.NotEmpty(t, got.Description())
		assert.True(t, declaredStateImpacts[got.Permission().StateImpact])
	})
}
