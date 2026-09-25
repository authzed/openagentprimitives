package authz_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz"
)

func repoAutofillEntities() []authz.BoundEntitySpec {
	return []authz.BoundEntitySpec{
		{
			ResourceType: "github_repo",
			AutoFillArgs: []authz.AutoFillArgSpec{
				{ArgName: "repo", ToolNamePattern: "gh_*"},
			},
		},
		{
			ResourceType: "linear_team",
			AutoFillArgs: []authz.AutoFillArgSpec{
				{ArgName: "teamId", ToolNamePattern: "linear_*"},
			},
		},
	}
}

// stubLister returns a fixed set of bound instances, or an error.
type stubLister struct {
	granted []authz.SlotBinding
	err     error
	calls   int
	gotNS   string
	gotName string
}

func (s *stubLister) ListSlotGrants(_ context.Context, ns, name string) ([]authz.SlotBinding, error) {
	s.calls++
	s.gotNS, s.gotName = ns, name
	return s.granted, s.err
}

func autofillSession() authz.SessionRef {
	return authz.SessionRef{Namespace: "ns", Name: "n"}
}

func TestFillToolArgs(t *testing.T) {
	cases := []struct {
		name     string
		granted  []authz.SlotBinding
		toolName string
		input    string
		expected string
	}{
		{
			name:     "one bound instance fills the empty arg",
			granted:  []authz.SlotBinding{{ResourceType: "github_repo", ResourceID: authz.TrustedObjectID("demo-org/demo-repo")}},
			toolName: "gh_repo_view",
			input:    `{}`,
			expected: `{"repo":"demo-org/demo-repo"}`,
		},
		{
			name: "two instances of one type: ambiguous, left unset rather than guessed",
			granted: []authz.SlotBinding{
				{ResourceType: "github_repo", ResourceID: authz.TrustedObjectID("a/b")},
				{ResourceType: "github_repo", ResourceID: authz.TrustedObjectID("c/d")},
			},
			toolName: "gh_repo_view",
			input:    `{}`,
			expected: `{}`,
		},
		{
			name:     "agent-supplied arg wins over the bound instance",
			granted:  []authz.SlotBinding{{ResourceType: "github_repo", ResourceID: authz.TrustedObjectID("demo-org/demo-repo")}},
			toolName: "gh_repo_view",
			input:    `{"repo":"other/repo"}`,
			expected: `{"repo":"other/repo"}`,
		},
		{
			name:     "tool-name pattern mismatch: no fill",
			granted:  []authz.SlotBinding{{ResourceType: "github_repo", ResourceID: authz.TrustedObjectID("demo-org/demo-repo")}},
			toolName: "linear_list_issues",
			input:    `{}`,
			expected: `{}`,
		},
		{
			name: "several types bound, only the matching tool's arg fills",
			granted: []authz.SlotBinding{
				{ResourceType: "github_repo", ResourceID: authz.TrustedObjectID("demo-org/demo-repo")},
				{ResourceType: "linear_team", ResourceID: authz.TrustedObjectID("platform")},
			},
			toolName: "gh_repo_view",
			input:    `{}`,
			expected: `{"repo":"demo-org/demo-repo"}`,
		},
		{
			name:     "nothing granted: input unchanged",
			granted:  nil,
			toolName: "gh_repo_view",
			input:    `{}`,
			expected: `{}`,
		},
		{
			name:     "a granted type nothing declares: input unchanged",
			granted:  []authz.SlotBinding{{ResourceType: "crm_company", ResourceID: authz.TrustedObjectID("acme")}},
			toolName: "gh_repo_view",
			input:    `{}`,
			expected: `{}`,
		},
		{
			name:     "non-object input: unchanged",
			granted:  []authz.SlotBinding{{ResourceType: "github_repo", ResourceID: authz.TrustedObjectID("demo-org/demo-repo")}},
			toolName: "gh_repo_view",
			input:    `"not an object"`,
			expected: `"not an object"`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lister := &stubLister{granted: tc.granted}
			got, err := authz.FillToolArgs(context.Background(), lister, autofillSession(),
				repoAutofillEntities(), tc.toolName, json.RawMessage(tc.input))
			require.NoError(t, err)
			assert.JSONEq(t, tc.expected, string(got))
		})
	}
}

// TestFillToolArgs_ReadsTheSessionItWasGiven guards the coordinate the lookup
// is keyed on: a slot grant is per-session, so filling from the wrong session's
// grants would hand the agent an instance nobody approved for this session.
func TestFillToolArgs_ReadsTheSessionItWasGiven(t *testing.T) {
	lister := &stubLister{granted: []authz.SlotBinding{{ResourceType: "github_repo", ResourceID: authz.TrustedObjectID("demo-org/demo-repo")}}}
	_, err := authz.FillToolArgs(context.Background(), lister, authz.SessionRef{Namespace: "team-a", Name: "sess-7"},
		repoAutofillEntities(), "gh_repo_view", json.RawMessage(`{}`))
	require.NoError(t, err)
	assert.Equal(t, "team-a", lister.gotNS)
	assert.Equal(t, "sess-7", lister.gotName)
}

// TestFillToolArgs_LookupFailure_SurfacesTheError keeps the failure visible.
// The caller falls back to the agent's raw args either way, so swallowing this
// would leave "my repo wasn't filled in" with no trace anywhere.
func TestFillToolArgs_LookupFailure_SurfacesTheError(t *testing.T) {
	lister := &stubLister{err: errors.New("spicedb unreachable")}
	got, err := authz.FillToolArgs(context.Background(), lister, autofillSession(),
		repoAutofillEntities(), "gh_repo_view", json.RawMessage(`{"a":1}`))
	require.Error(t, err, "a lookup failure must reach the caller")
	assert.Contains(t, err.Error(), "spicedb unreachable")
	assert.JSONEq(t, `{"a":1}`, string(got), "args must be returned untouched on failure")
}

func TestFillToolArgs_NoEntities_SkipsTheLookupEntirely(t *testing.T) {
	lister := &stubLister{granted: []authz.SlotBinding{{ResourceType: "github_repo", ResourceID: authz.TrustedObjectID("demo-org/demo-repo")}}}
	got, err := authz.FillToolArgs(context.Background(), lister, autofillSession(), nil, "gh_repo_view", json.RawMessage(`{}`))
	require.NoError(t, err)
	assert.JSONEq(t, `{}`, string(got))
	assert.Zero(t, lister.calls, "a class with no slots must not pay for a lookup on every dispatch")
}

func TestFillToolArgs_NilLister_NoOp(t *testing.T) {
	got, err := authz.FillToolArgs(context.Background(), nil, autofillSession(),
		repoAutofillEntities(), "gh_repo_view", json.RawMessage(`{}`))
	require.NoError(t, err)
	assert.JSONEq(t, `{}`, string(got))
}
