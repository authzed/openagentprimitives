package grants

import (
	"context"
	"regexp"
	"testing"
	"time"

	v1 "github.com/authzed/authzed-go/proto/authzed/api/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testHashKey is the fixed per-session HMAC key used by every hashing
// test in this file. Production keys are 32 random bytes minted by the
// AgentSession reconciler.
var testHashKey = []byte("grants-test-key-0123456789abcdef")

// fakeWriter records WriteRelationships / DeleteRelationships calls for
// assertion in tests. Implements the Writer interface without touching
// the real spicedb client.
type fakeWriter struct {
	writes  []*v1.WriteRelationshipsRequest
	deletes []*v1.DeleteRelationshipsRequest
}

func (f *fakeWriter) WriteRelationships(_ context.Context, req *v1.WriteRelationshipsRequest) (*v1.WriteRelationshipsResponse, error) {
	f.writes = append(f.writes, req)
	return &v1.WriteRelationshipsResponse{}, nil
}

func (f *fakeWriter) DeleteRelationships(_ context.Context, req *v1.DeleteRelationshipsRequest) (*v1.DeleteRelationshipsResponse, error) {
	f.deletes = append(f.deletes, req)
	return &v1.DeleteRelationshipsResponse{}, nil
}

func TestArgsHash_OrderIndependence(t *testing.T) {
	cases := []struct {
		name string
		a, b map[string]any
		eq   bool
	}{
		{
			name: "top-level map key order doesn't change hash",
			a:    map[string]any{"alpha": 1, "beta": "two", "charlie": true},
			b:    map[string]any{"charlie": true, "alpha": 1, "beta": "two"},
			eq:   true,
		},
		{
			name: "nested map key order doesn't change hash",
			a: map[string]any{
				"outer": map[string]any{
					"x": 1,
					"y": 2,
					"z": map[string]any{"k1": "v1", "k2": "v2"},
				},
			},
			b: map[string]any{
				"outer": map[string]any{
					"z": map[string]any{"k2": "v2", "k1": "v1"},
					"y": 2,
					"x": 1,
				},
			},
			eq: true,
		},
		{
			name: "slice order is meaningful — reordering changes hash",
			a:    map[string]any{"list": []any{"x", "y", "z"}},
			b:    map[string]any{"list": []any{"z", "y", "x"}},
			eq:   false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ha := ArgsHash(testHashKey, tc.a)
			hb := ArgsHash(testHashKey, tc.b)
			if tc.eq {
				assert.Equal(t, ha, hb)
			} else {
				assert.NotEqual(t, ha, hb)
			}
		})
	}
}

func TestArgsHashFiltered_OnlyKeysAffectHash(t *testing.T) {
	// Filtering to [repo] should make calls that differ only in the
	// non-bound `pr` key hash identically.
	a := map[string]any{"repo": "foo/bar", "pr": 17}
	b := map[string]any{"repo": "foo/bar", "pr": 18}
	assert.Equal(t, ArgsHashFiltered(testHashKey, a, []string{"repo"}), ArgsHashFiltered(testHashKey, b, []string{"repo"}),
		"filtered hash should ignore non-bound keys")
	// And the unfiltered hashes MUST differ (sanity check that the
	// filter is doing work, not that the inputs were equal to begin
	// with).
	assert.NotEqual(t, ArgsHash(testHashKey, a), ArgsHash(testHashKey, b),
		"unfiltered hashes should differ when args differ")
}

func TestArgsHashFiltered_DifferentBoundKeyDiffers(t *testing.T) {
	// Same `pr`, different `repo` → filtered-by-[repo] hashes must differ.
	a := map[string]any{"repo": "foo/bar", "pr": 17}
	b := map[string]any{"repo": "foo/baz", "pr": 17}
	assert.NotEqual(t, ArgsHashFiltered(testHashKey, a, []string{"repo"}), ArgsHashFiltered(testHashKey, b, []string{"repo"}),
		"filtered hash should change when a bound key changes")
}

func TestArgsHashBackCompat_NilKeysHashesAll(t *testing.T) {
	// ArgsHashFiltered(key, args, nil) MUST equal ArgsHash(key, args) —
	// the runner's pre-slice-2 callsites pass nil and the existing tuple
	// shape must be preserved.
	args := map[string]any{"alpha": 1, "beta": "two", "charlie": true}
	assert.Equal(t, ArgsHash(testHashKey, args), ArgsHashFiltered(testHashKey, args, nil))
}

func TestArgsHashFiltered_EmptyKeysHashesNothing(t *testing.T) {
	// An empty (but non-nil) key set binds the grant to "every call
	// regardless of args" — every input hashes the same.
	a := map[string]any{"x": 1, "y": 2}
	b := map[string]any{"x": 99, "z": "different"}
	empty := []string{}
	assert.Equal(t, ArgsHashFiltered(testHashKey, a, empty), ArgsHashFiltered(testHashKey, b, empty),
		"empty-keys-but-non-nil should hash to a constant across inputs")
}

func TestArgsHashFiltered_KeyOrderIrrelevant(t *testing.T) {
	// keys is a set; reordering must not change the hash.
	args := map[string]any{"a": 1, "b": 2, "c": 3}
	h1 := ArgsHashFiltered(testHashKey, args, []string{"a", "b"})
	h2 := ArgsHashFiltered(testHashKey, args, []string{"b", "a"})
	assert.Equal(t, h1, h2, "filter-key order should not affect hash")
}

func TestArgsHashIsHex64(t *testing.T) {
	h := ArgsHash(testHashKey, map[string]any{"k": "v"})
	require.Len(t, h, 64, "expected 64-char hash")
	assert.Regexp(t, regexp.MustCompile(`^[0-9a-f]{64}$`), h)
}

func TestArgsHash_KeyedBinding(t *testing.T) {
	args := map[string]any{"repo": "acme/site", "pr": float64(7)}

	h1 := ArgsHash(testHashKey, args)
	h2 := ArgsHash(testHashKey, map[string]any{"pr": float64(7), "repo": "acme/site"})
	assert.Equal(t, h1, h2, "same key + same args (any map order) → same hash")

	other := ArgsHash([]byte("a-different-session-key-32bytes!"), args)
	assert.NotEqual(t, h1, other, "different key → different hash: the binding is unforgeable without the session key")

	unkeyedShape := ArgsHash(nil, args)
	assert.NotEqual(t, h1, unkeyedShape, "keyed hash differs from nil-key hash")
	assert.Len(t, h1, 64, "hex-encoded HMAC-SHA256")
}

func TestWriteToolGrantBuildsRelationshipUpdate(t *testing.T) {
	fw := &fakeWriter{}
	g := Grant{
		SessionRef:   "default/sess-1",
		Permission:   "admin",
		ResourceType: "github_repo",
		ResourceID:   "octo/widgets",
		ArgsHash:     "deadbeef",
		TTL:          0,
	}
	require.NoError(t, WriteToolGrant(context.Background(), fw, g))
	require.Len(t, fw.writes, 1)
	require.Len(t, fw.writes[0].Updates, 1)
	upd := fw.writes[0].Updates[0]
	assert.Equal(t, v1.RelationshipUpdate_OPERATION_TOUCH, upd.Operation)
	rel := upd.Relationship
	assert.Equal(t, AgentSessionDefinition, rel.Resource.ObjectType)
	assert.Equal(t, "default/sess-1", rel.Resource.ObjectId)
	assert.Equal(t, "grant_admin_github_repo", rel.Relation)
	assert.Equal(t, "github_repo", rel.Subject.Object.ObjectType)
	assert.Equal(t, "octo/widgets", rel.Subject.Object.ObjectId)
	require.NotNil(t, rel.OptionalCaveat, "expected caveat")
	assert.Equal(t, CheckHashCaveat, rel.OptionalCaveat.CaveatName)
	assert.Equal(t, "deadbeef", rel.OptionalCaveat.Context.Fields["allowed_arguments_hash"].GetStringValue())
	// Schema requires expiration on every grant tuple (the relation
	// declares `with check_hash and expiration`). For TTL=0 we apply
	// the DefaultSessionGrantTTL backstop so the write doesn't get
	// rejected; "indefinite session-wide grant" isn't actually
	// expressible under the schema.
	require.NotNil(t, rel.OptionalExpiresAt, "TTL=0 must apply DefaultSessionGrantTTL backstop, not nil")
	gotExp := rel.OptionalExpiresAt.AsTime()
	wantMin := time.Now().Add(DefaultSessionGrantTTL).Add(-2 * time.Second)
	wantMax := time.Now().Add(DefaultSessionGrantTTL).Add(2 * time.Second)
	assert.WithinRange(t, gotExp, wantMin, wantMax,
		"TTL=0 expiration is now()+DefaultSessionGrantTTL")
}

func TestWriteToolGrantWithTTLStampsExpiresAt(t *testing.T) {
	fw := &fakeWriter{}
	g := Grant{
		SessionRef:   "default/sess-1",
		Permission:   "admin",
		ResourceType: "github_repo",
		ResourceID:   "octo/widgets",
		ArgsHash:     "deadbeef",
		TTL:          5 * time.Minute,
	}
	before := time.Now()
	require.NoError(t, WriteToolGrant(context.Background(), fw, g))
	rel := fw.writes[0].Updates[0].Relationship
	require.NotNil(t, rel.OptionalExpiresAt, "TTL>0 must stamp OptionalExpiresAt")
	gotExp := rel.OptionalExpiresAt.AsTime()
	wantMin := before.Add(5 * time.Minute).Add(-1 * time.Second)
	wantMax := time.Now().Add(5 * time.Minute).Add(1 * time.Second)
	assert.WithinRange(t, gotExp, wantMin, wantMax)
}

func TestWriteToolGrantEmptyArgsHashErrors(t *testing.T) {
	fw := &fakeWriter{}
	g := Grant{
		SessionRef:   "default/sess-1",
		Permission:   "admin",
		ResourceType: "github_repo",
		ResourceID:   "octo/widgets",
		ArgsHash:     "",
	}
	require.Error(t, WriteToolGrant(context.Background(), fw, g))
	assert.Empty(t, fw.writes, "expected no writes on validation error")
}

// TestArgsHash_KnownAnswer pins the exact construction — HMAC-SHA256
// over the canonical [k0,v0,...] JSON — so silent drift in normalize()
// or the algorithm cannot invalidate grants already persisted in
// SpiceDB across a runner restart.
func TestArgsHash_KnownAnswer(t *testing.T) {
	got := ArgsHash(testHashKey, map[string]any{"b": "2", "a": float64(1)})
	// hex(HMAC-SHA256(testHashKey, `["a",1,"b","2"]`))
	assert.Equal(t, "25abc56d266127866b9f5bc58eaf5ed360e37023736510ba41fcf312f046023e", got)
}

func TestDeleteToolGrantBuildsFilter(t *testing.T) {
	fw := &fakeWriter{}
	k := GrantKey{
		SessionRef:   "default/sess-1",
		Permission:   "admin",
		ResourceType: "github_repo",
		ResourceID:   "octo/widgets",
	}
	require.NoError(t, DeleteToolGrant(context.Background(), fw, k))
	require.Len(t, fw.deletes, 1)
	f := fw.deletes[0].RelationshipFilter
	assert.Equal(t, AgentSessionDefinition, f.ResourceType)
	assert.Equal(t, "default/sess-1", f.OptionalResourceId)
	assert.Equal(t, "grant_admin_github_repo", f.OptionalRelation)
	require.NotNil(t, f.OptionalSubjectFilter, "expected subject filter")
	assert.Equal(t, "github_repo", f.OptionalSubjectFilter.SubjectType)
	assert.Equal(t, "octo/widgets", f.OptionalSubjectFilter.OptionalSubjectId)
}
