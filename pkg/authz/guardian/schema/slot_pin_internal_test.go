package schema

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func baseSchemaFor(rt string) string {
	return "definition agentsession {}\n\ndefinition " + rt + " {\n    relation viewer: agentsession\n}\n"
}

func TestComposeOneSlot_EmitsNonExpiringSlotPinRelation(t *testing.T) {
	for _, rt := range []string{"git_repo", "crm_company", "github_pull_request"} {
		t.Run(rt, func(t *testing.T) {
			out, changed, skip, err := composeOneSlot(baseSchemaFor(rt), SlotPair{ResourceType: rt, Permission: "read"})
			require.NoError(t, err)
			require.False(t, skip, "the base schema defines %s; the slot must not be skipped", rt)
			require.True(t, changed)
			assert.Equal(t, 1, strings.Count(out, "relation slot_pin: agentsession"))
			assert.NotContains(t, out, "slot_pin: agentsession with expiration",
				"the pin must never carry an expiration")
		})
	}
}

func TestComposeOneSlot_SecondPermissionDoesNotDuplicatePin(t *testing.T) {
	out, _, _, err := composeOneSlot(baseSchemaFor("git_repo"), SlotPair{ResourceType: "git_repo", Permission: "push"})
	require.NoError(t, err)
	out2, _, _, err := composeOneSlot(out, SlotPair{ResourceType: "git_repo", Permission: "read"})
	require.NoError(t, err)
	assert.Equal(t, 1, strings.Count(out2, "relation slot_pin: agentsession"))
}
