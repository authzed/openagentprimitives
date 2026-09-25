package relwrites

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// captureWriter records every batch of tuples passed to
// WriteRelationships so the override-writer tests can assert on what
// flowed through the override. (Separate from `fakeWriter` in
// relwrites_test.go which has a different shape.)
type captureWriter struct {
	calls [][]ResolvedTuple
}

func (c *captureWriter) WriteRelationships(_ context.Context, tuples []ResolvedTuple) error {
	cp := make([]ResolvedTuple, len(tuples))
	copy(cp, tuples)
	c.calls = append(c.calls, cp)
	return nil
}

// TestUserSubjectOverrideWriter_RewritesUserSubjects pins the
// substitution contract: only `user:` subjects get rewritten, every
// other subject type passes through unchanged.
func TestUserSubjectOverrideWriter_RewritesUserSubjects(t *testing.T) {
	const overrideCanon = "YWxpY2VAZXhhbXBsZS5jb20" // base64-url of "alice@example.com" (illustrative)

	cases := []struct {
		name     string
		override string
		in       []ResolvedTuple
		want     []ResolvedTuple
	}{
		{
			name:     "user subject is rewritten to the override canonical",
			override: overrideCanon,
			in: []ResolvedTuple{
				{Resource: "hubspot_owner:42", Relation: "user", Subject: "user:Ym9iQGV4YW1wbGUuY29t"},
			},
			want: []ResolvedTuple{
				{Resource: "hubspot_owner:42", Relation: "user", Subject: "user:" + overrideCanon},
			},
		},
		{
			name:     "non-user subjects pass through unchanged",
			override: overrideCanon,
			in: []ResolvedTuple{
				{Resource: "crm_company:99", Relation: "hubspot_owner_id_ref", Subject: "hubspot_owner:42"},
				{Resource: "agentsession:abc", Relation: "participant", Subject: "group:eng#member"},
			},
			want: []ResolvedTuple{
				{Resource: "crm_company:99", Relation: "hubspot_owner_id_ref", Subject: "hubspot_owner:42"},
				{Resource: "agentsession:abc", Relation: "participant", Subject: "group:eng#member"},
			},
		},
		{
			name:     "mixed batch — only user subjects are rewritten",
			override: overrideCanon,
			in: []ResolvedTuple{
				{Resource: "crm_company:99", Relation: "hubspot_owner_id_ref", Subject: "hubspot_owner:42"},
				{Resource: "hubspot_owner:42", Relation: "user", Subject: "user:Ym9iQGV4YW1wbGUuY29t"},
			},
			want: []ResolvedTuple{
				{Resource: "crm_company:99", Relation: "hubspot_owner_id_ref", Subject: "hubspot_owner:42"},
				{Resource: "hubspot_owner:42", Relation: "user", Subject: "user:" + overrideCanon},
			},
		},
		{
			name:     "empty override behaves as a pure pass-through",
			override: "",
			in: []ResolvedTuple{
				{Resource: "hubspot_owner:42", Relation: "user", Subject: "user:original"},
			},
			want: []ResolvedTuple{
				{Resource: "hubspot_owner:42", Relation: "user", Subject: "user:original"},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			inner := &captureWriter{}
			w := &UserSubjectOverrideWriter{
				Inner:             inner,
				OverrideCanonical: identity.CanonicalFromTrusted(tc.override, "test fixture"),
			}
			require.NoError(t, w.WriteRelationships(context.Background(), tc.in))
			require.Len(t, inner.calls, 1)
			assert.Equal(t, tc.want, inner.calls[0])
		})
	}
}

// TestUserSubjectOverrideWriter_LogsOnRewrite verifies the per-write
// log fires for every actual rewrite (giving operators a grep target
// to confirm the override is active) and stays silent for
// pass-through tuples.
func TestUserSubjectOverrideWriter_LogsOnRewrite(t *testing.T) {
	const overrideCanon = "YWxpY2VAZXhhbXBsZS5jb20"
	var logged []string
	w := &UserSubjectOverrideWriter{
		Inner:             &captureWriter{},
		OverrideCanonical: identity.CanonicalFromTrusted(overrideCanon, "test fixture"),
		LogFn: func(msg string, _ ...any) {
			logged = append(logged, msg)
		},
	}
	in := []ResolvedTuple{
		{Resource: "crm_company:99", Relation: "hubspot_owner_id_ref", Subject: "hubspot_owner:42"}, // skipped
		{Resource: "hubspot_owner:42", Relation: "user", Subject: "user:Ym9iQGV4YW1wbGUuY29t"},      // rewritten
		{Resource: "hubspot_owner:99", Relation: "user", Subject: "user:" + overrideCanon},          // already match → no rewrite log
	}
	require.NoError(t, w.WriteRelationships(context.Background(), in))
	assert.Len(t, logged, 1, "exactly one rewrite log (only the actual substitution)")
}
