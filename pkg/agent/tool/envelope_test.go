package tool

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz"
)

type fakeTool struct {
	name string
	perm authz.Permission
}

func (f fakeTool) Name() string                 { return f.name }
func (f fakeTool) Kind() Kind                   { return KindMeta }
func (f fakeTool) Description() string          { return "fake" }
func (f fakeTool) InputSchema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (f fakeTool) Execute(context.Context, json.RawMessage, *SessionContext) (Result, error) {
	return Result{}, nil
}
func (f fakeTool) Permission() authz.Permission                  { return f.perm }
func (f fakeTool) PermissionVariants() []authz.PermissionVariant { return nil }

func readonlyOn(permission, resourceType string) authz.Permission {
	return authz.Permission{
		StateImpact: authz.Readonly,
		Check:       &authz.PermissionCheck{Permission: permission, ResourceType: resourceType},
	}
}

func TestValidateEnvelope_cleanEnvelopeHasNoProblems(t *testing.T) {
	probs := ValidateEnvelope([]Tool{
		fakeTool{name: "gh_pr_view", perm: readonlyOn("read", "github_repo")},
		fakeTool{name: "update_plan", perm: authz.Permission{StateImpact: authz.Passthrough}},
	})
	assert.Empty(t, probs)
}

// Silent last-wins shadowing is the live bug this closes.
func TestValidateEnvelope_duplicateNameIsFatal(t *testing.T) {
	probs := ValidateEnvelope([]Tool{
		fakeTool{name: "gh_pr_view", perm: readonlyOn("read", "github_repo")},
		fakeTool{name: "gh_pr_view", perm: readonlyOn("write", "github_repo")},
	})
	require.Len(t, probs, 1)
	assert.Equal(t, "gh_pr_view", probs[0].Tool)
	assert.True(t, probs[0].Fatal, "a duplicate tool name silently shadows and must be fatal")
	assert.Contains(t, probs[0].Detail, "duplicate")
}

// Unhandleable names are harmless until plan-gating exists: warn, don't kill
// a running cluster.
func TestValidateEnvelope_unhandleableNameWarnsButIsNotFatal(t *testing.T) {
	probs := ValidateEnvelope([]Tool{
		fakeTool{name: "bad name", perm: authz.Permission{StateImpact: authz.External}},
	})
	require.Len(t, probs, 1)
	assert.Equal(t, "bad name", probs[0].Tool)
	assert.False(t, probs[0].Fatal)
	assert.Contains(t, probs[0].Detail, "cannot mint a permission handle")
}

func TestValidateEnvelope_unhandleablePermissionComponentsWarn(t *testing.T) {
	probs := ValidateEnvelope([]Tool{
		fakeTool{name: "evil", perm: readonlyOn("wr:ite", "github_repo")},
	})
	require.Len(t, probs, 1)
	assert.False(t, probs[0].Fatal)
	assert.Contains(t, probs[0].Detail, "cannot mint a permission handle")
}

// stateless/passthrough tools are never on the surface, so their names are
// not handle components and must not be reported.
func TestValidateEnvelope_passthroughToolWithOddNameIsIgnored(t *testing.T) {
	probs := ValidateEnvelope([]Tool{
		fakeTool{name: "odd name", perm: authz.Permission{StateImpact: authz.Passthrough}},
	})
	assert.Empty(t, probs)
}

// TestFatalFor_UnhandleableBecomesFatalOnlyWhenTheSurfaceIsEnforced is the
// escalation three separate comments promised and nobody performed.
//
// A tool that cannot mint a handle is ABSENT FROM THE SURFACE YET STILL
// CALLABLE. While nothing enforces against the surface that is merely untidy.
// Once the plan gate denies, it becomes the one thing that walks past the gate
// everything else is measured by — the gate's no-handle branch records
// OutcomeAllow, so the call is not even flagged.
//
// Conditioned rather than unconditional because the original reasoning still
// holds for a cluster that gates nothing: it is unharmed, and refusing to start
// over a working name would be a pure regression there.
func TestFatalFor_UnhandleableBecomesFatalOnlyWhenTheSurfaceIsEnforced(t *testing.T) {
	unhandleable := ValidateEnvelope([]Tool{
		fakeTool{name: "bad name", perm: authz.Permission{StateImpact: authz.External}},
	})
	require.Len(t, unhandleable, 1)
	require.Equal(t, ProblemUnhandleable, unhandleable[0].Kind)

	assert.False(t, FatalFor(unhandleable, false),
		"with nothing enforcing against the surface, an unhandleable tool must stay a warning")
	assert.True(t, FatalFor(unhandleable, true),
		"once the gate denies, a tool absent from the surface yet callable is exactly what enforcement cannot tolerate")
}

// TestFatalFor_DuplicateIsFatalRegardless: a duplicate shadows at dispatch
// whatever the gate is doing. That defect predates permissions entirely.
func TestFatalFor_DuplicateIsFatalRegardless(t *testing.T) {
	dupes := ValidateEnvelope([]Tool{
		fakeTool{name: "same", perm: authz.Permission{StateImpact: authz.Readonly}},
		fakeTool{name: "same", perm: authz.Permission{StateImpact: authz.Readonly}},
	})
	require.NotEmpty(t, dupes)
	assert.Equal(t, ProblemDuplicateName, dupes[0].Kind)
	assert.True(t, FatalFor(dupes, false))
	assert.True(t, FatalFor(dupes, true))
}

// TestFatalFor_CleanEnvelopeIsNeverFatal keeps the escalation from becoming a
// blanket refusal: enforcing the surface must not stop an envelope that has
// nothing wrong with it.
func TestFatalFor_CleanEnvelopeIsNeverFatal(t *testing.T) {
	clean := ValidateEnvelope([]Tool{
		fakeTool{name: "grep", perm: authz.Permission{StateImpact: authz.Readonly}},
	})
	assert.False(t, FatalFor(clean, true))
	assert.False(t, FatalFor(clean, false))
}

// TestCallersBranchOnKindNotDetail pins why Kind exists: Detail is prose and
// free to be reworded, and a caller matching on its text would silently stop
// escalating the moment someone improved the wording.
func TestCallersBranchOnKindNotDetail(t *testing.T) {
	probs := ValidateEnvelope([]Tool{
		fakeTool{name: "evil", perm: readonlyOn("wr:ite", "github_repo")},
	})
	require.Len(t, probs, 1)
	assert.Equal(t, ProblemUnhandleable, probs[0].Kind,
		"the class must be a value, not something recovered from the message")
}
