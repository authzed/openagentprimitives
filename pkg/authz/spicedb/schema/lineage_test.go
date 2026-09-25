package schema

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSchema_LineageDeclarations(t *testing.T) {
	require.NotEmpty(t, Schema, "embedded schema must be non-empty")

	cases := []struct {
		name string
		want string
	}{
		{"parent relation", "relation parent: agentsession"},
		{"child relation", "relation child: agentsession"},
		{"ancestor closure", "permission ancestor = parent + parent->ancestor"},
		// converse is the agent-to-agent inbound gate. Both terms are pinned as
		// one string because dropping either silences one DIRECTION of a
		// conversational delegation while the other keeps working — a failure
		// that presents as "the child stopped answering", not as a broken gate.
		{"converse admits both ends of one delegation hop", "permission converse = parent + child"},
		{"read_transcript denies last", "permission read_transcript = interact + parent + parent->read_transcript - denied"},
		{"approve inherits, denies last", "permission approve = owner + parent->approve - denied"},
		{"memory_entry reads transcript", "permission read = session->read_transcript"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Contains(t, Schema, tc.want)
		})
	}
}

// A parent arm that subtracts denied INSIDE the arrow would let a user denied on
// the child inherit through the parent. Guard the shape, not just the substring.
//
// Scoped to the agentsession definition, not the whole schema: `denied` is a
// relation on agentsession only, so "must end in `- denied`" is only a coherent
// requirement for permissions declared inside that block. artifact#view also
// contains a `parent->` arrow (`parent->interact`) but belongs to a different
// definition with no `denied` relation of its own to subtract — appending one
// would not compile. Its safety instead comes from `interact` itself already
// subtracting denied, which TestSchemaHasCriticalDefinitions pins separately.
func TestSchema_DeniedIsSubtractedAfterEveryParentArrow(t *testing.T) {
	start := strings.Index(Schema, "definition agentsession {")
	require.GreaterOrEqual(t, start, 0, "definition agentsession not found in schema")
	end := strings.Index(Schema[start:], "\n}\n")
	require.GreaterOrEqual(t, end, 0, "closing brace of definition agentsession not found")
	block := Schema[start : start+end]

	for _, line := range strings.Split(block, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "permission ") || !strings.Contains(trimmed, "parent->") {
			continue
		}
		if strings.Contains(trimmed, "ancestor") {
			continue // ancestor is a session-subject closure; denied does not apply
		}
		assert.True(t, strings.HasSuffix(trimmed, "- denied"),
			"permission with a parent arrow must end in `- denied`: %q", trimmed)
	}
}
