package memory_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/authzed/openagentprimitives/pkg/memory"
	_ "github.com/authzed/openagentprimitives/pkg/memory/kinds/all"
)

func TestKindAppendOnly(t *testing.T) {
	appendOnly := []string{
		"turn",
		"approval",
		"authz_decision",
		"lifecycle",
		"relwrites_audit",
		"scope_audit",
		"toolguard_audit",
		"metaagent_audit",
		"infoleakage_audit",
		"infoleakage_decision",
		"infoleakage_taint",
		"tool_dispatch_snapshot",
		"tool_session",
	}
	mutable := []string{
		"label",
		"session_scope",
		"extraction_state",
	}
	for _, name := range appendOnly {
		assert.True(t, memory.KindAppendOnly(name), "kind %q must be append-only", name)
	}
	for _, name := range mutable {
		assert.False(t, memory.KindAppendOnly(name), "kind %q must stay mutable", name)
	}
	assert.False(t, memory.KindAppendOnly("nonexistent-kind"), "unknown kind defaults to mutable")
}
