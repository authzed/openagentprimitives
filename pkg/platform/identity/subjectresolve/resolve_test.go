package subjectresolve

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// mustEmailSubject computes the SAME canonical id the runner's own email path
// produces, so tests never hand-transcribe a base64 string.
func mustEmailSubject(t *testing.T, addr string) string {
	t.Helper()
	canon, err := identity.EmailReference(identity.Email(addr)).Canonical()
	require.NoError(t, err)
	return canon.String()
}

func TestResolve_Email(t *testing.T) {
	want := mustEmailSubject(t, "alice@example.com")
	cases := []struct {
		name string
		ref  string
	}{
		{name: "prefixed form", ref: "email:alice@example.com"},
		{name: "bare form", ref: "alice@example.com"},
		{name: "prefixed form is case-normalized same as the runner's path", ref: "email:Alice@Example.com"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := Resolve(t.Context(), tc.ref, Env{})
			require.NoError(t, err)
			assert.Equal(t, want, res.Subject)
			assert.Empty(t, res.Reason)
		})
	}
}

func TestResolve_EmailRejectsNonAtAddresses(t *testing.T) {
	cases := []string{"email:notanemail", "email:", "email:   "}
	for _, ref := range cases {
		t.Run(ref, func(t *testing.T) {
			res, err := Resolve(t.Context(), ref, Env{})
			require.NoError(t, err)
			assert.Empty(t, res.Subject)
			assert.NotEmpty(t, res.Reason)
		})
	}
}

func TestResolve_TriggerAuthor(t *testing.T) {
	t.Run("recorded owner resolves via the generic resource path", func(t *testing.T) {
		rel := &fakeRelations{subjects: map[string][]string{
			"github_user:4172237:sole_user": {"deadbeef"},
		}}
		env := Env{
			Relations: rel,
			SessionAnnotations: fakeAnnotations(map[string]string{
				spiceboxv1alpha1.AnnotationTriggerOwnerSubject: "github_user:4172237#user",
			}),
		}
		res, err := Resolve(t.Context(), "trigger-author", env)
		require.NoError(t, err)
		assert.Equal(t, "deadbeef", res.Subject)
		assert.Empty(t, res.Reason)
	})

	t.Run("missing annotation: unresolved with the fixed reason", func(t *testing.T) {
		env := Env{SessionAnnotations: fakeAnnotations(map[string]string{})}
		res, err := Resolve(t.Context(), "trigger-author", env)
		require.NoError(t, err)
		assert.Empty(t, res.Subject)
		assert.Equal(t, "this session was not opened by a trigger with a recorded owner", res.Reason)
	})

	t.Run("nil SessionAnnotations: same reason, fail-closed", func(t *testing.T) {
		res, err := Resolve(t.Context(), "trigger-author", Env{})
		require.NoError(t, err)
		assert.Empty(t, res.Subject)
		assert.Equal(t, "this session was not opened by a trigger with a recorded owner", res.Reason)
	})

	t.Run("SessionAnnotations error propagates", func(t *testing.T) {
		env := Env{SessionAnnotations: fakeAnnotationsErr(errFakeAnnotations)}
		_, err := Resolve(t.Context(), "trigger-author", env)
		require.ErrorIs(t, err, errFakeAnnotations)
	})

	t.Run("malformed recorded owner: unresolved, fail-closed", func(t *testing.T) {
		env := Env{
			Relations: &fakeRelations{},
			SessionAnnotations: fakeAnnotations(map[string]string{
				spiceboxv1alpha1.AnnotationTriggerOwnerSubject: "not a resource ref#user",
			}),
		}
		res, err := Resolve(t.Context(), "trigger-author", env)
		require.NoError(t, err)
		assert.Empty(t, res.Subject)
		assert.NotEmpty(t, res.Reason)
	})
}

// TestResolve_SubjectProven pins which resolvers VOUCH for their subject.
// A relation-backed resolution (a resource's sole_user, trigger-author's
// recursion into one) proves the platform links this subject to the
// reference; the email resolver canonicalizes in form only — any well-formed
// address yields a subject whether or not a platform user exists behind it —
// so its resolutions stay unproven and a consumer must apply its own
// existence bar before treating the subject as a real user. The zero value
// is unproven on purpose: a future resolver that forgets to claim proof gets
// the stricter treatment, not the looser one.
func TestResolve_SubjectProven(t *testing.T) {
	t.Run("email: resolved in form only, unproven", func(t *testing.T) {
		res, err := Resolve(t.Context(), "email:alice@example.com", Env{})
		require.NoError(t, err)
		require.NotEmpty(t, res.Subject)
		assert.False(t, res.SubjectProven,
			"an email canonicalizes unconditionally; it must never claim the platform knows this user")
	})

	t.Run("resource via sole_user: proven", func(t *testing.T) {
		rel := &fakeRelations{subjects: map[string][]string{
			"github_user:4172237:sole_user": {"deadbeef"},
		}}
		res, err := Resolve(t.Context(), "github_user:4172237", Env{Relations: rel})
		require.NoError(t, err)
		require.Equal(t, "deadbeef", res.Subject)
		assert.True(t, res.SubjectProven, "a sole_user edge IS the platform's own linkage authority")
	})

	t.Run("trigger-author via sole_user: proven", func(t *testing.T) {
		rel := &fakeRelations{subjects: map[string][]string{
			"github_user:4172237:sole_user": {"deadbeef"},
		}}
		env := Env{
			Relations: rel,
			SessionAnnotations: fakeAnnotations(map[string]string{
				spiceboxv1alpha1.AnnotationTriggerOwnerSubject: "github_user:4172237#user",
			}),
		}
		res, err := Resolve(t.Context(), "trigger-author", env)
		require.NoError(t, err)
		require.Equal(t, "deadbeef", res.Subject)
		assert.True(t, res.SubjectProven, "trigger-author recurses into the same sole_user authority")
	})
}

func TestResolve_GenericResource(t *testing.T) {
	t.Run("sole_user exactly one: resolved", func(t *testing.T) {
		rel := &fakeRelations{subjects: map[string][]string{
			"github_user:12345:sole_user": {"canon-abc"},
		}}
		res, err := Resolve(t.Context(), "github_user:12345", Env{Relations: rel})
		require.NoError(t, err)
		assert.Equal(t, "canon-abc", res.Subject)
	})

	t.Run("lone user-relation claimant: NOT resolved, user relation never queried", func(t *testing.T) {
		// #user is session membership, legitimately multi-claimant and
		// possibly stale mid-derivation — durable authority traverses
		// #sole_user only. A resource whose only linked user sits on #user
		// must stay unresolved.
		rel := &fakeRelations{subjects: map[string][]string{
			"github_user:12345:user": {"canon-xyz"},
		}}
		res, err := Resolve(t.Context(), "github_user:12345", Env{Relations: rel})
		require.NoError(t, err)
		assert.Empty(t, res.Subject)
		assert.Equal(t, "github_user:12345 has no linked platform user", res.Reason)
		assert.Equal(t, []string{"github_user:12345:sole_user"}, rel.calls,
			"resolution consults sole_user only, never the user relation")
	})

	t.Run("sole_user empty: unresolved naming the resource", func(t *testing.T) {
		rel := &fakeRelations{}
		res, err := Resolve(t.Context(), "github_user:12345", Env{Relations: rel})
		require.NoError(t, err)
		assert.Empty(t, res.Subject)
		assert.Equal(t, "github_user:12345 has no linked platform user", res.Reason)
	})

	t.Run("sole_user ambiguous: unresolved, fail-closed", func(t *testing.T) {
		rel := &fakeRelations{subjects: map[string][]string{
			"github_user:12345:sole_user": {"a", "b"},
		}}
		res, err := Resolve(t.Context(), "github_user:12345", Env{Relations: rel})
		require.NoError(t, err)
		assert.Empty(t, res.Subject)
		assert.Equal(t, "github_user:12345 has no unambiguous linked platform user", res.Reason)
		assert.Equal(t, []string{"github_user:12345:sole_user"}, rel.calls,
			"an ambiguous sole_user is an invariant violation; nothing else is consulted")
	})

	t.Run("sole_user RelationReader error propagates", func(t *testing.T) {
		rel := &fakeRelations{errs: map[string]error{
			"github_user:12345:sole_user": errFakeRelations,
		}}
		_, err := Resolve(t.Context(), "github_user:12345", Env{Relations: rel})
		require.ErrorIs(t, err, errFakeRelations)
	})

	t.Run("nil RelationReader on a well-formed reference: error, not a silent unresolved", func(t *testing.T) {
		_, err := Resolve(t.Context(), "github_user:12345", Env{})
		require.Error(t, err)
	})
}

// TestResolve_ReasonsBoundTheEchoedRef proves the agent-supplied reference is
// truncated before being echoed into a Reason, in both places that echo one:
// the unsupported-form catch-all and the email resolver's rejection.
func TestResolve_ReasonsBoundTheEchoedRef(t *testing.T) {
	long := strings.Repeat("x", 5000)
	cases := []struct {
		name string
		ref  string
	}{
		{name: "unsupported form", ref: long},
		{name: "invalid email", ref: "email:" + long},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := Resolve(t.Context(), tc.ref, Env{})
			require.NoError(t, err)
			assert.Empty(t, res.Subject)
			assert.Contains(t, res.Reason, "…", "a truncated echo must say it was truncated")
			assert.Less(t, utf8.RuneCountInString(res.Reason), 400,
				"the reason must stay bounded no matter how long the supplied ref is")
		})
	}
}

func TestResolve_UnsupportedForms(t *testing.T) {
	cases := []string{
		"",
		"garbage",
		"slack:...",      // '.' is outside the SpiceDB object-id charset
		"a:b:c",          // more than one colon
		":noleadingtype", // empty type
		"UPPER:lower",    // type must be lowercase
		"github_user:has space",
	}
	for _, ref := range cases {
		t.Run(ref, func(t *testing.T) {
			rel := &fakeRelations{}
			res, err := Resolve(t.Context(), ref, Env{Relations: rel})
			require.NoError(t, err)
			assert.Empty(t, res.Subject)
			assert.NotEmpty(t, res.Reason)
			assert.Empty(t, rel.calls, "shape-invalid references must never reach SpiceDB")
		})
	}
}
