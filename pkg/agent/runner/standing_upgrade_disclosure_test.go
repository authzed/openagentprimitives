package runner

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
)

// TestToolApprovalFields_DisclosesStandingUpgrade pins the card-copy half of the
// standing-upgrade disclosure: a JIT ask that will be rebound to session length
// on approve (upgradeDeclaredSlotGrantToStanding) must say so, naming the host,
// so the human is not told "one call" while consenting to the whole session.
func TestToolApprovalFields_DisclosesStandingUpgrade(t *testing.T) {
	fields := toolApprovalFields(toolApprovalCopy{
		Justification:         "because",
		What:                  "Act on the host",
		ResourceLabel:         "aa.com",
		StandingAfterApproval: true,
	})

	var note string
	for _, f := range fields {
		if f.Label == "Note" {
			note = f.Value
		}
	}
	assert.Contains(t, note, "aa.com", "the disclosure must name the host being approved")
	assert.Contains(t, strings.ToLower(note), "rest of this session",
		"the disclosure must say the approval lasts the session, not one call")
}

// TestToolApprovalFields_NoStandingNoticeByDefault: an ordinary one-call ask (no
// standing upgrade, no changed-page reprompt) carries no Note row.
func TestToolApprovalFields_NoStandingNoticeByDefault(t *testing.T) {
	fields := toolApprovalFields(toolApprovalCopy{Justification: "because", What: "do it"})
	for _, f := range fields {
		assert.NotEqual(t, "Note", f.Label, "a plain one-call ask must show no standing-upgrade note")
	}
}

// TestWillUpgradeDeclaredSlotToStanding mirrors the exact condition
// toolCallPostApprove gates the declared-slot standing upgrade on, so the card
// discloses standing exactly when (and only when) the upgrade will truly run.
func TestWillUpgradeDeclaredSlotToStanding(t *testing.T) {
	wired := func() *Loop {
		return &Loop{
			AgentName:           "demo-agent",
			PlanGateSlotTypes:   []string{"web_host"},
			GuardianGrantWriter: noopGuardianGrantWriter{},
			Mem:                 memory.NewLocal(inmem.NewBackend()),
			SlotBinder:          newRecordingSlotBinder(),
		}
	}
	host := func(l *Loop) *runnerHost {
		return newRunnerHost(l, hostSession{Namespace: "ns", Name: "s", Class: "demo-agent"})
	}
	const external = string(authz.External)

	t.Run("declared slot, external, empty web_action key, writers wired: true", func(t *testing.T) {
		assert.True(t, host(wired()).willUpgradeDeclaredSlotToStanding("", external, "web_host", "aa=2Ecom"))
	})
	t.Run("non-empty web_action key (a per-action mutate): false", func(t *testing.T) {
		assert.False(t, host(wired()).willUpgradeDeclaredSlotToStanding("some-key", external, "web_host", "aa=2Ecom"))
	})
	t.Run("not external: false", func(t *testing.T) {
		assert.False(t, host(wired()).willUpgradeDeclaredSlotToStanding("", string(authz.Readwrite), "web_host", "aa=2Ecom"))
	})
	t.Run("undeclared slot type: false", func(t *testing.T) {
		assert.False(t, host(wired()).willUpgradeDeclaredSlotToStanding("", external, "github_repo", "demo/widgets"))
	})
	t.Run("empty resource id: false", func(t *testing.T) {
		assert.False(t, host(wired()).willUpgradeDeclaredSlotToStanding("", external, "web_host", ""))
	})
	t.Run("grant writer not wired: false (do not promise standing we cannot grant)", func(t *testing.T) {
		l := wired()
		l.SlotBinder = nil
		assert.False(t, host(l).willUpgradeDeclaredSlotToStanding("", external, "web_host", "aa=2Ecom"))
	})
}
