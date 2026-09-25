package agentclass

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// Slot auto-fill sets args[ArgName] on the TOP-LEVEL args object, which is the
// right shape for an MCP tool. A sandbox tool's are not: it receives
// {operation_id, _reason, args: [argv]}, so the fill lands beside an untouched
// argv, the tool runs the agent's original command, and the bound instance is
// ignored. Nothing errors — and the check misses it too, because a sandbox
// tool's named args come from the toolkit parser's view of argv, not from the
// JSON object.
//
// Silently binding nothing is the failure this refuses. Declaring autoFillArgs
// is opting in to a behaviour, and an opt-in that cannot be honoured makes the
// class not-ready rather than quietly doing nothing.
func TestValidateSlotAutofill_refusesAPatternThatMatchesASandboxTool(t *testing.T) {
	ac := classWithSlotAutofill("gitlike_*")

	reason, msg := validateSlotAutofill(ac, []string{"gitlike_git"})

	assert.Equal(t, spiceboxv1alpha1.ReasonAgentClassSlotAutofillUnfillable, reason)
	assert.Contains(t, msg, "gitlike_git", "name the tool so the author can see which one")
	assert.Contains(t, msg, "git_repo", "and the slot it came from")
	assert.Contains(t, msg, "remote", "and the arg that cannot be filled")
}

// An empty pattern means "every tool", which includes every sandbox tool. This
// is the case most likely to be written by accident — the field is optional, so
// omitting it reads as "no constraint" rather than "match the argv tools too".
func TestValidateSlotAutofill_refusesAnEmptyPatternWhenASandboxToolExists(t *testing.T) {
	ac := classWithSlotAutofill("")

	reason, msg := validateSlotAutofill(ac, []string{"gitlike_git"})

	assert.Equal(t, spiceboxv1alpha1.ReasonAgentClassSlotAutofillUnfillable, reason)
	assert.Contains(t, msg, "matches every tool",
		"an omitted pattern is the accidental case; say why it matched")
}

// The cases that must stay valid, or the guard is worse than the bug it fixes.
func TestValidateSlotAutofill_allowsWhatItShould(t *testing.T) {
	cases := []struct {
		name             string
		pattern          string
		sandboxToolNames []string
	}{
		{
			name:             "pattern matches only MCP tools: valid",
			pattern:          "list_*",
			sandboxToolNames: []string{"gitlike_git"},
		},
		{
			name:             "class has no sandbox tools at all: valid",
			pattern:          "",
			sandboxToolNames: nil,
		},
		{
			name:             "pattern names a bundle that does not exist: valid",
			pattern:          "otherbundle_*",
			sandboxToolNames: []string{"gitlike_git"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reason, msg := validateSlotAutofill(classWithSlotAutofill(tc.pattern), tc.sandboxToolNames)
			assert.Empty(t, reason, "unexpected refusal: %s", msg)
		})
	}
}

// A slot with no autoFillArgs is the common case and declares no intent to fill
// anything, so nothing to refuse — the instance axis still works for sandbox
// tools through grants and checks, which is what the git-repo-url-* bundles pin.
func TestValidateSlotAutofill_ignoresASlotThatDeclaresNoAutofill(t *testing.T) {
	ac := classWithSlotAutofill("gitlike_*")
	ac.Spec.Authz.Slots[0].AutoFillArgs = nil

	reason, _ := validateSlotAutofill(ac, []string{"gitlike_git"})

	assert.Empty(t, reason)
}

func classWithSlotAutofill(pattern string) *spiceboxv1alpha1.AgentClass {
	return &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-agent", Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentClassSpec{
			Authz: &spiceboxv1alpha1.AuthzBlock{
				Slots: []spiceboxv1alpha1.AuthzSlot{{
					ResourceType: "git_repo",
					Description:  "a git repository",
					Permission:   "fetch",
					AutoFillArgs: []spiceboxv1alpha1.AuthzSlotAutoFillArg{{
						ArgName:         "remote",
						ToolNamePattern: pattern,
					}},
				}},
			},
		},
	}
}

// Guards the helper the validator leans on: filepath.Match's semantics are not
// obvious, and an empty pattern matching everything is load-bearing here.
func TestAutofillPatternMatches(t *testing.T) {
	cases := []struct {
		pattern, toolName string
		want              bool
	}{
		{"", "gitlike_git", true},
		{"gitlike_*", "gitlike_git", true},
		{"gitlike_git", "gitlike_git", true},
		{"list_*", "gitlike_git", false},
		{"gitlike_*", "otherbundle_git", false},
		{"[", "gitlike_git", false}, // malformed: must not panic, must not match
	}
	for _, tc := range cases {
		t.Run(tc.pattern+"/"+tc.toolName, func(t *testing.T) {
			require.Equal(t, tc.want, autofillPatternMatches(tc.pattern, tc.toolName))
		})
	}
}
