package permsurface

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewPermHandle_valid(t *testing.T) {
	h, err := NewPermHandle("write", "github_repo")
	require.NoError(t, err)
	assert.Equal(t, "perm:write:github_repo", h.String())
	assert.False(t, h.IsZero())
}

func TestNewToolHandle_valid(t *testing.T) {
	h, err := NewToolHandle("apply_workspace")
	require.NoError(t, err)
	assert.Equal(t, "tool:apply_workspace", h.String())
}

func TestNewPermHandle_namespacedResourceType(t *testing.T) {
	h, err := NewPermHandle("read", "acme/tracker_issue")
	require.NoError(t, err)
	assert.Equal(t, "perm:read:acme/tracker_issue", h.String())
}

// Every case must be REJECTED, never sanitized. Sanitizing would map distinct
// inputs onto one handle — the exact collision being defended against.
func TestConstructors_rejectMalformedComponents(t *testing.T) {
	permCases := []struct{ name, permission, resourceType string }{
		{"colon in permission: rejected", "wr:ite", "github_repo"},
		{"colon in resourceType: rejected", "write", "git:hub_repo"},
		{"empty permission: rejected", "", "github_repo"},
		{"empty resourceType: rejected", "write", ""},
		{"uppercase permission: rejected", "Write", "github_repo"},
		{"uppercase resourceType: rejected", "write", "GitHub_repo"},
		{"leading digit in permission: rejected", "1write", "github_repo"},
		{"space in permission: rejected", "wr ite", "github_repo"},
		{"newline in resourceType: rejected", "write", "github_repo\nApproved"},
		{"NUL in permission: rejected", "wr\x00ite", "github_repo"},
		{"cyrillic homoglyph in resourceType: rejected", "write", "githuб_repo"},
		{"zero-width joiner in permission: rejected", "wr‍ite", "github_repo"},
		{"RTL override in resourceType: rejected", "write", "github‮repo"},
		{"trailing slash in resourceType: rejected", "write", "github_repo/"},
		{"hyphen in resourceType: rejected", "write", "github-repo"},
	}
	for _, tc := range permCases {
		t.Run(tc.name, func(t *testing.T) {
			h, err := NewPermHandle(tc.permission, tc.resourceType)
			require.Error(t, err)
			assert.True(t, h.IsZero(), "rejected input must yield the zero Handle")
		})
	}

	toolCases := []struct{ name, toolName string }{
		{"colon in tool name: rejected", "perm:write:github_repo"},
		{"tool-prefixed name with colon: rejected", "tool:apply_workspace"},
		{"empty tool name: rejected", ""},
		{"space in tool name: rejected", "apply workspace"},
		{"newline in tool name: rejected", "apply\nworkspace"},
		{"NUL in tool name: rejected", "apply\x00workspace"},
		{"cyrillic homoglyph in tool name: rejected", "аpply_workspace"},
		{"overlong tool name (65 chars): rejected", string(make([]byte, 65))},
		{"dot in tool name: rejected", "apply.workspace"},
		{"slash in tool name: rejected", "apply/workspace"},
	}
	for _, tc := range toolCases {
		t.Run(tc.name, func(t *testing.T) {
			h, err := NewToolHandle(tc.toolName)
			require.Error(t, err)
			assert.True(t, h.IsZero(), "rejected input must yield the zero Handle")
		})
	}
}

// A bare "perm" IS a legal tool name; the tool: prefix keeps the namespaces
// disjoint, so this must succeed and must not collide with any perm: handle.
func TestNewToolHandle_bareKindWordIsLegalAndDisjoint(t *testing.T) {
	h, err := NewToolHandle("perm")
	require.NoError(t, err)
	assert.Equal(t, "tool:perm", h.String())
}

func TestParseHandle_roundTripsBothKinds(t *testing.T) {
	cases := []string{
		"perm:write:github_repo",
		"perm:read:acme/tracker_issue",
		"tool:apply_workspace",
		"tool:perm",
	}
	for _, s := range cases {
		t.Run(s+": round-trips", func(t *testing.T) {
			h, err := ParseHandle(s)
			require.NoError(t, err)
			assert.Equal(t, s, h.String())
		})
	}
}

func TestParseHandle_rejectsMalformed(t *testing.T) {
	cases := []struct{ name, input string }{
		{"no kind prefix: rejected", "write:github_repo"},
		{"unknown kind prefix: rejected", "cap:write:github_repo"},
		{"perm handle missing resourceType: rejected", "perm:write"},
		{"perm handle with extra segment: rejected", "perm:write:github:repo"},
		{"empty string: rejected", ""},
		{"prefix only: rejected", "perm:"},
		{"tool prefix only: rejected", "tool:"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, err := ParseHandle(tc.input)
			require.Error(t, err)
			assert.True(t, h.IsZero())
		})
	}
}

// The plan gate needs to know which resource types a phase's ceiling names, so
// it can tell whether a phase touching a declared slot type failed to name an
// instance. That fact is already IN the handle; without an accessor every
// consumer re-splits the wire form, and a consumer that splits it slightly
// differently is how a handle smuggles an unvalidated value back in.
func TestHandle_ResourceType(t *testing.T) {
	perm, err := NewPermHandle("push", "git_repo")
	require.NoError(t, err)
	assert.Equal(t, "git_repo", perm.ResourceType())

	tool, err := NewToolHandle("apply_workspace")
	require.NoError(t, err)
	assert.Empty(t, tool.ResourceType(),
		"a tool handle names no SpiceDB resource, and inventing one would make it look slot-bearing")

	assert.Empty(t, Handle{}.ResourceType(), "the zero handle names nothing")
}

// Permission is the half slot binding needs: a grant is written per (instance,
// permission), so an approval adding `read` must bind read and not whatever the
// slot happened to declare.
func TestHandle_Permission(t *testing.T) {
	h, err := NewPermHandle("read", "git_repo")
	require.NoError(t, err)
	assert.Equal(t, "read", h.Permission())
	assert.Equal(t, "git_repo", h.ResourceType(), "the two halves must not be transposed")

	tool, err := NewToolHandle("git")
	require.NoError(t, err)
	assert.Empty(t, tool.Permission(), "a tool handle names no permission")
	assert.Empty(t, Handle{}.Permission(), "the zero handle names none either")
}
