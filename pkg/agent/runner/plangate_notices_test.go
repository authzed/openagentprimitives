package runner

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/authz/permsurface"
	"github.com/authzed/openagentprimitives/pkg/authz/plangate"
	"github.com/authzed/openagentprimitives/pkg/memory"
)

// A dropped handle changes what the phase can reach — to nothing, if every
// handle is dropped. Logging it at INFO tells an operator reading logs later; it
// does not tell the AGENT, which proceeds believing it holds authority the gate
// never recorded. In enforcing mode that is a phase frozen with an empty ceiling
// and every subsequent call denied, with no feedback naming the cause.
//
// Observed live: an agent wrote raw tool names ("linear_list_projects") where
// handles ("tool:linear_list_projects") belong, and all ten were dropped
// silently.
func TestFreezeAndRecordPhases_returnsNoticesTheAgentCanAct_on(t *testing.T) {
	l := &Loop{
		SessionKey: memory.NamespacedName{Namespace: "ns", Name: "s"},
		PlanGateSurface: []permsurface.Descriptor{
			{Handle: mustHandle(t, "read", "linear_issue"), StateImpact: authz.Readonly},
		},
	}

	notices, err := l.FreezeAndRecordPhases(context.Background(), []plangate.AuthoredPhase{{
		Label:       "recon",
		Permissions: []plangate.AuthoredPermission{{Handle: "linear_list_projects", Why: "list them"}},
	}})
	require.NoError(t, err)

	require.NotEmpty(t, notices, "a dropped handle must come back to the agent, not only to the log")
	joined := strings.Join(notices, "\n")
	assert.Contains(t, joined, "linear_list_projects", "name what was dropped")
	assert.Contains(t, joined, "perm:read:linear_issue",
		"naming the handles that ARE available is what lets the agent self-correct")
}

// The case that produced the confusing session: nothing on this agent is gated,
// so every handle is dropped no matter what the agent writes. Saying that
// plainly is the difference between "my plan was rejected" and "there is
// nothing here to plan against".
func TestFreezeAndRecordPhases_anEmptySurfaceSaysSoRatherThanListingNothing(t *testing.T) {
	l := &Loop{SessionKey: memory.NamespacedName{Namespace: "ns", Name: "s"}}

	notices, err := l.FreezeAndRecordPhases(context.Background(), []plangate.AuthoredPhase{{
		Label:       "recon",
		Permissions: []plangate.AuthoredPermission{{Handle: "linear_list_projects", Why: "list them"}},
	}})
	require.NoError(t, err)

	joined := strings.Join(notices, "\n")
	assert.Contains(t, joined, "no gated tools",
		"an empty surface is a different situation from a mistyped handle and must read differently")
}

// A correct plan must stay quiet. A notice on every call would be noise the
// model learns to skip, which costs the mechanism its only chance to be heard.
func TestFreezeAndRecordPhases_aValidPhaseProducesNoNotices(t *testing.T) {
	l := &Loop{
		SessionKey: memory.NamespacedName{Namespace: "ns", Name: "s"},
		PlanGateSurface: []permsurface.Descriptor{
			{Handle: mustHandle(t, "read", "linear_issue"), StateImpact: authz.Readonly},
		},
	}

	notices, err := l.FreezeAndRecordPhases(context.Background(), []plangate.AuthoredPhase{{
		Label:       "recon",
		Permissions: []plangate.AuthoredPermission{{Handle: "perm:read:linear_issue", Why: "read them"}},
	}})
	require.NoError(t, err)
	assert.Empty(t, notices)
}

func mustHandle(t *testing.T, permission, resourceType string) permsurface.Handle {
	t.Helper()
	h, err := permsurface.NewPermHandle(permission, resourceType)
	require.NoError(t, err)
	return h
}

// slotLoop is a session whose class declares a git_repo slot and whose surface
// carries a git_repo-bearing handle — the codebot shape.
func slotLoop(t *testing.T, mode string) *Loop {
	t.Helper()
	return &Loop{
		SessionKey:   memory.NamespacedName{Namespace: "ns", Name: "s"},
		PlanGateMode: mode,
		PlanGateSurface: []permsurface.Descriptor{
			{Handle: mustHandle(t, "push", "git_repo"), StateImpact: authz.External},
		},
		PlanGateSlotTypes: []string{"git_repo"},
	}
}

func unnamedPhase() []plangate.AuthoredPhase {
	return []plangate.AuthoredPhase{{
		ID: "ship", Label: "Push branch and open PR",
		Permissions: []plangate.AuthoredPermission{{Handle: "perm:push:git_repo", Why: "to push"}},
	}}
}

// The WIRING, which is where the last three defects lived. MissingSlotDeclarations
// being correct says nothing about whether anything calls it before the frozen
// plan is stored.
//
// Under enforcing the update_plan call must FAIL, so the agent has to re-declare
// — an advisory notice is what the system prompt already was, and the agent
// ignored it while telling the user it had planned.
func TestFreezeAndRecordPhases_enforcingRefusesAPhaseThatNamesNoInstance(t *testing.T) {
	l := slotLoop(t, plangate.ModeEnforcing)

	_, err := l.FreezeAndRecordPhases(context.Background(), unnamedPhase())

	require.Error(t, err, "a suggestion is what did not work; this has to be a precondition")
	assert.Contains(t, err.Error(), "git_repo")
	assert.Contains(t, err.Error(), "Push branch and open PR", "name the phase to fix")
	assert.Contains(t, err.Error(), "NO id",
		"the refusal must state the deferral route, or the agent fabricates an id to get past it")

	// And the refused plan must NOT go on to govern anything.
	l.planGateMu.Lock()
	defer l.planGateMu.Unlock()
	assert.False(t, l.planGateHasPlan,
		"storing a plan that was just refused would gate calls against a ceiling the agent was told to replace")
}

// Logging runs every step EXCEPT blocking; its purpose is a dataset gathered
// without changing how sessions behave. Refusing there would change them.
func TestFreezeAndRecordPhases_loggingTellsTheAgentButDoesNotRefuse(t *testing.T) {
	l := slotLoop(t, "logging")

	notices, err := l.FreezeAndRecordPhases(context.Background(), unnamedPhase())

	require.NoError(t, err, "logging must not block")
	assert.Contains(t, strings.Join(notices, "\n"), "git_repo",
		"the agent is still told, so the dataset shows what it would have been asked to fix")

	l.planGateMu.Lock()
	defer l.planGateMu.Unlock()
	assert.True(t, l.planGateHasPlan, "and the plan still stands")
}

// The escape hatch, through the real entry point: declaring the type with no id
// is accepted, so an agent that cannot name its target is never forced to guess.
func TestFreezeAndRecordPhases_anExplicitlyDeferredTargetIsAccepted(t *testing.T) {
	l := slotLoop(t, plangate.ModeEnforcing)

	ph := unnamedPhase()
	ph[0].Slots = []plangate.AuthoredSlot{{Type: "git_repo", Why: "I must look before I can name it"}}

	_, err := l.FreezeAndRecordPhases(context.Background(), ph)
	require.NoError(t, err, "deferral is honest and must remain legal")
}

// A named target is the path this whole feature exists for.
func TestFreezeAndRecordPhases_aNamedTargetIsAccepted(t *testing.T) {
	l := slotLoop(t, plangate.ModeEnforcing)

	ph := unnamedPhase()
	ph[0].Slots = []plangate.AuthoredSlot{
		{Type: "git_repo", ID: "https://github.com/acme/app", Why: "the ticket names it"},
	}

	_, err := l.FreezeAndRecordPhases(context.Background(), ph)
	require.NoError(t, err)
}
