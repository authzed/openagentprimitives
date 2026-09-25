//go:build integration

// Integration tests for the guardian flow against a real SpiceDB instance:
// the full compose → grant → check round-trip, with the caveat-gated
// args-hash binding actually enforced by SpiceDB.
//
// Boot model: the same dockertest-based testspicedb harness pkg/authz/spicedb
// uses. Run with:
//
//	go test -tags=integration -count=1 ./pkg/authz/guardian/
//
// Each test takes a fresh datastore via UniqueToken (serve-testing keys
// datastores by token), then writes a SLICE-2 base schema (containing
// `use expiration` + `caveat check_hash`) before exercising compose +
// grant + check. We do NOT call testspicedb.WriteSchema (that writes the
// slice-1 schema which lacks the check_hash caveat).
package guardian_test

import (
	"context"
	"strings"
	"testing"
	"time"

	v1 "github.com/authzed/authzed-go/proto/authzed/api/v1"
	"github.com/authzed/grpcutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/structpb"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/guardian/grants"
	guardianschema "github.com/authzed/openagentprimitives/pkg/authz/guardian/schema"
	"github.com/authzed/openagentprimitives/pkg/authz/spicedb"
	"github.com/authzed/openagentprimitives/test/testspicedb"
)

// slice2BaseSchema is the pre-composition schema: declares the
// check_hash caveat the composed grant_* relations need, plus the
// github_repo and agentsession definitions the test exercises.
const slice2BaseSchema = `
use expiration

caveat check_hash(arguments_hash string, allowed_arguments_hash string) {
    arguments_hash == allowed_arguments_hash
}

definition user {}

definition github_repo {
    relation owner: user
    // any_user is the wildcard LEAF the composed permission
    //   check_admin_github_repo = grant_admin_github_repo->admin
    // resolves through. That arrow re-evaluates admin on the repo for
    // the SAME subject whose direct check failed, so without a leaf that
    // subject can satisfy, no grant tuple can ever make the composed
    // check pass. This is the production fragment shape rather than the
    // half-configured one; see TestIntegration_ComposeGrantCheck.
    relation any_user: user:*
    permission admin = owner + any_user
}

definition agentsession {
    relation started_by: user
    permission interact = started_by
}
`

// schemaIOOverConn implements guardianschema.SchemaIO directly against
// a raw grpc.ClientConn so the test can use the same token the rest of
// the SpiceDB ops in the test use. Equivalent to spicedb.SchemaIOFor
// but without going through *spicedb.Client.
//
// Note on `use expiration`: SpiceDB v1.52 ReadSchema does not echo the
// `use expiration` flag back in the rendered schema text. Compose
// emits relations with `with check_hash and expiration` syntax, which
// requires `use expiration` to be present, so this adapter
// re-prepends the directive on Read when missing. The production
// (operator) adapter in pkg/authz/spicedb.SchemaIOAdapter has the same
// concern; the controller test in pkg/controllers/guardian uses a
// fake SchemaIO that doesn't exhibit this stripping.
type schemaIOOverConn struct {
	cl v1.SchemaServiceClient
}

func (s schemaIOOverConn) ReadSchema(ctx context.Context) (string, error) {
	resp, err := s.cl.ReadSchema(ctx, &v1.ReadSchemaRequest{})
	if err != nil {
		return "", err
	}
	return ensureUseExpiration(resp.GetSchemaText()), nil
}

func (s schemaIOOverConn) WriteSchema(ctx context.Context, text string) error {
	_, err := s.cl.WriteSchema(ctx, &v1.WriteSchemaRequest{Schema: text})
	return err
}

// ensureUseExpiration prepends the `use expiration` directive if it's
// not already present. Necessary because SpiceDB ReadSchema strips it
// from the rendered text on round-trip, but Compose emits relations
// that depend on it being declared.
func ensureUseExpiration(s string) string {
	if strings.Contains(s, "use expiration") {
		return s
	}
	return "use expiration\n\n" + s
}

// useExpirationTolerant wraps any SchemaIO to ensure `use expiration`
// is present on Read. Same workaround as schemaIOOverConn but
// composable around the production *spicedb.Client adapter so the
// adapter-round-trip test can exercise the real production seam.
type useExpirationTolerant struct {
	inner guardianschema.SchemaIO
}

func (w useExpirationTolerant) ReadSchema(ctx context.Context) (string, error) {
	s, err := w.inner.ReadSchema(ctx)
	if err != nil {
		return "", err
	}
	return ensureUseExpiration(s), nil
}

func (w useExpirationTolerant) WriteSchema(ctx context.Context, text string) error {
	return w.inner.WriteSchema(ctx, text)
}

// grantWriterAdapter strips the variadic grpc.CallOption from the raw
// PermissionsServiceClient so it satisfies grants.Writer. Mirrors the
// same-named type in internal/cmd/runner/main.go.
type grantWriterAdapter struct {
	cl v1.PermissionsServiceClient
}

func (a grantWriterAdapter) WriteRelationships(ctx context.Context, req *v1.WriteRelationshipsRequest) (*v1.WriteRelationshipsResponse, error) {
	return a.cl.WriteRelationships(ctx, req)
}

func (a grantWriterAdapter) DeleteRelationships(ctx context.Context, req *v1.DeleteRelationshipsRequest) (*v1.DeleteRelationshipsResponse, error) {
	return a.cl.DeleteRelationships(ctx, req)
}

// newIntegrationConn returns a raw gRPC conn into a fresh datastore.
// Writes the slice-2 base schema before returning.
func newIntegrationConn(t *testing.T) (*grpc.ClientConn, string) {
	t.Helper()
	endpoint := testspicedb.SharedEndpoint(t)
	token := testspicedb.UniqueToken(t)
	conn, err := grpc.NewClient(endpoint,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpcutil.WithInsecureBearerToken(token),
	)
	require.NoError(t, err, "dial spicedb")
	t.Cleanup(func() { _ = conn.Close() })
	// Write base schema (slice-2 — includes `use expiration` + caveat).
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err = v1.NewSchemaServiceClient(conn).WriteSchema(ctx, &v1.WriteSchemaRequest{
		Schema: slice2BaseSchema,
	})
	require.NoError(t, err, "write base schema")
	return conn, token
}

// TestIntegration_ComposeGrantCheck exercises the full slice-2
// round-trip end-to-end against a real SpiceDB, for the ONLY subject the
// approval flow ever asks about: one who does NOT already hold the
// resource permission. (A prior version of this test seeded
// `github_repo:foo/bar#owner@user:alice` before checking, which made the
// composed check pass for a reason unrelated to the grant — it would
// have passed with no grant tuple at all, and did not notice that the
// production path was denying every approved call.)
//
//  1. Compose one pair (github_repo, admin) into the agentsession
//     definition via guardianschema.Run.
//  2. Read back the schema; verify `grant_admin_github_repo` relation
//     and `check_admin_github_repo` permission are now present.
//  3. Write a grant tuple
//     `agentsession:ns/sess#grant_admin_github_repo@github_repo:foo/bar`
//     with caveat `check_hash{allowed_arguments_hash: "deadbeef"}` via
//     grants.WriteToolGrant.
//  4. NEGATIVE CONTROL — grant tuple present, wildcard leaf tuple ABSENT:
//     NO_PERMISSION. This is the production defect in miniature: the
//     `grant_admin_github_repo->admin` arrow re-derives `admin` on the
//     repo for user:alice, which is the check that denied in the first
//     place. Approval alone cannot authorize the call.
//  5. Write the wildcard leaf tuple `github_repo:foo/bar#any_user@user:*`.
//  6. CheckPermission `check_admin_github_repo` for user:alice with
//     caveat context {arguments_hash: "deadbeef"} → HAS_PERMISSION. alice
//     holds no `owner` tuple, so the grant + leaf pair is the sole reason
//     this passes.
//  7. NEGATIVE CONTROL — wrong `arguments_hash`: NO_PERMISSION (the
//     caveat binds the grant to the exact call).
//  8. NEGATIVE CONTROL — a different session with no grant tuple, wildcard
//     leaf still present: NO_PERMISSION (the leaf does not grant on its
//     own; the grant is load-bearing).
//
// This is the test that locks the contract between the operator
// (composed schema text) and the runner (grant tuple shape + caveat
// context) — if either side drifts, this test fails.
func TestIntegration_ComposeGrantCheck(t *testing.T) {
	conn, _ := newIntegrationConn(t)
	schemaCli := v1.NewSchemaServiceClient(conn)
	permCli := v1.NewPermissionsServiceClient(conn)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// 1. Compose one pair.
	io := schemaIOOverConn{cl: schemaCli}
	pair := guardianschema.GrantPair{ResourceType: "github_repo", Permission: "admin"}
	res, err := guardianschema.Run(ctx, io, []guardianschema.GrantPair{pair})
	require.NoError(t, err, "guardianschema.Run")
	require.True(t, res.Changed, "Run.Changed = false; want true (the pair is new)")
	require.Empty(t, res.SkippedPairs, "unexpected SkippedPairs")

	// 2. Read schema back and verify composed lines are present.
	readResp, err := schemaCli.ReadSchema(ctx, &v1.ReadSchemaRequest{})
	require.NoError(t, err, "ReadSchema")
	for _, want := range []string{
		"relation grant_admin_github_repo: github_repo with check_hash and expiration",
		"permission check_admin_github_repo = grant_admin_github_repo->admin",
	} {
		assert.Contains(t, readResp.GetSchemaText(), want,
			"composed schema missing %q", want)
	}

	// 3. Write the grant tuple with caveat context.
	const (
		sessionRef = "ns/sess"
		repoID     = "foo/bar"
		argsHash   = "deadbeef"
	)
	// TTL must be > 0: the composed relation type is
	// `github_repo with check_hash and expiration`, so SpiceDB requires
	// every tuple on that relation to carry an OptionalExpiresAt. (The
	// TTL=0 path is only valid against schemas that opt out of the
	// `and expiration` qualifier, which Compose always emits.)
	require.NoError(t, grants.WriteToolGrant(ctx, grantWriterAdapter{cl: permCli}, grants.Grant{
		SessionRef:   sessionRef,
		Permission:   "admin",
		ResourceType: "github_repo",
		ResourceID:   repoID,
		ArgsHash:     argsHash,
		TTL:          10 * time.Minute,
	}), "grants.WriteToolGrant")

	// checkComposed runs the composed session-grant permission for
	// user:alice against the given session with the given args-hash.
	// alice deliberately holds NO `owner` tuple on the repo anywhere in
	// this test — that is what makes the assertions below about the
	// grant load-bearing rather than incidental.
	checkComposed := func(t *testing.T, session, hash string) v1.CheckPermissionResponse_Permissionship {
		t.Helper()
		cavCtx, err := structpb.NewStruct(map[string]any{"arguments_hash": hash})
		require.NoError(t, err, "structpb.NewStruct")
		resp, err := permCli.CheckPermission(ctx, &v1.CheckPermissionRequest{
			Consistency: &v1.Consistency{Requirement: &v1.Consistency_FullyConsistent{FullyConsistent: true}},
			Resource:    &v1.ObjectReference{ObjectType: "agentsession", ObjectId: session},
			Permission:  pair.PermissionName(), // check_admin_github_repo
			Subject:     &v1.SubjectReference{Object: &v1.ObjectReference{ObjectType: "user", ObjectId: "alice"}},
			Context:     cavCtx,
		})
		require.NoError(t, err, "CheckPermission")
		return resp.GetPermissionship()
	}

	// 4. NEGATIVE CONTROL: grant tuple written, wildcard leaf absent.
	// The arrow re-derives `admin` on the repo for user:alice — the very
	// check that denied and triggered the approval — so the grant cannot
	// help. If this ever starts passing, the composed permission's shape
	// changed and the wildcard-leaf admission invariant is moot.
	assert.NotEqual(t, v1.CheckPermissionResponse_PERMISSIONSHIP_HAS_PERMISSION,
		checkComposed(t, sessionRef, argsHash),
		"grant tuple without a wildcard leaf must NOT authorize: the composed arrow re-evaluates admin for the same subject.\nComposed schema:\n%s",
		readResp.GetSchemaText())

	// 5. Write the wildcard leaf the production fragment JIT-writes.
	_, err = permCli.WriteRelationships(ctx, &v1.WriteRelationshipsRequest{
		Updates: []*v1.RelationshipUpdate{{
			Operation: v1.RelationshipUpdate_OPERATION_TOUCH,
			Relationship: &v1.Relationship{
				Resource: &v1.ObjectReference{ObjectType: "github_repo", ObjectId: repoID},
				Relation: "any_user",
				Subject: &v1.SubjectReference{Object: &v1.ObjectReference{
					ObjectType: "user", ObjectId: "*",
				}},
			},
		}},
	})
	require.NoError(t, err, "seed github_repo#any_user@user:*")

	// 6. Grant tuple + wildcard leaf + matching args-hash → HAS_PERMISSION.
	assert.Equal(t, v1.CheckPermissionResponse_PERMISSIONSHIP_HAS_PERMISSION,
		checkComposed(t, sessionRef, argsHash),
		"slice-2 happy-path contract: composed schema + grant tuple + wildcard leaf + matching caveat ctx must grant a subject who does NOT hold the base permission.\nComposed schema:\n%s",
		readResp.GetSchemaText())

	// 7. NEGATIVE CONTROL: mismatching args-hash. The caveat binds the
	// grant to the exact approved call.
	assert.NotEqual(t, v1.CheckPermissionResponse_PERMISSIONSHIP_HAS_PERMISSION,
		checkComposed(t, sessionRef, "wronghash"),
		"wrong hash: caveat must reject mismatched args hashes")

	// 8. NEGATIVE CONTROL: a session with no grant tuple, wildcard leaf
	// still in place. Proves the leaf alone authorizes nothing through
	// the composed permission — the grant is what allows.
	assert.NotEqual(t, v1.CheckPermissionResponse_PERMISSIONSHIP_HAS_PERMISSION,
		checkComposed(t, "ns/ungranted-sess", argsHash),
		"a session with no grant tuple must be denied even with the wildcard leaf present")
}

// TestIntegration_SchemaIOAdapterRoundTrip verifies that the
// production adapter exposed by pkg/authz/spicedb.SchemaIOFor reads back
// the exact bytes it wrote — i.e. the *spicedb.Client → SchemaIO seam
// the operator wires up in internal/cmd/operator/main.go works end to end.
func TestIntegration_SchemaIOAdapterRoundTrip(t *testing.T) {
	conn, token := newIntegrationConn(t)
	_ = conn // base schema already written

	endpoint := testspicedb.SharedEndpoint(t)
	cli, err := spicedb.NewClient(endpoint, token, true /* insecure */)
	require.NoError(t, err, "spicedb.NewClient")
	t.Cleanup(func() { _ = cli.Close() })

	adapter := spicedb.SchemaIOFor(cli)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	got, err := adapter.ReadSchema(ctx)
	require.NoError(t, err, "adapter.ReadSchema")
	// Note: SpiceDB strips `use expiration` from the round-trip — we
	// don't assert its presence here, only that the rest of the schema
	// shape survived.
	for _, want := range []string{"caveat check_hash(", "definition agentsession"} {
		assert.Contains(t, got, want, "adapter.ReadSchema missing %q", want)
	}

	// Compose adds a pair, write through a use-expiration-tolerant
	// wrapper around the adapter, read again, assert.
	pair := guardianschema.GrantPair{ResourceType: "github_repo", Permission: "admin"}
	res, err := guardianschema.Run(ctx, useExpirationTolerant{adapter}, []guardianschema.GrantPair{pair})
	require.NoError(t, err, "guardianschema.Run via adapter")
	require.True(t, res.Changed, "Run.Changed = false; want true")
	after, err := adapter.ReadSchema(ctx)
	require.NoError(t, err, "adapter.ReadSchema (after)")
	assert.Contains(t, after, "relation grant_admin_github_repo",
		"schema after compose via adapter missing grant_admin_github_repo")
}

// TestIntegration_RunAllConvergesAgainstRealSpiceDB proves the guardian's
// reconcile loop reaches a fixed point against a REAL server.
//
// The unit-level convergence test canonicalizes with the same compile+render
// the fix uses, so it cannot prove that form matches what SpiceDB actually
// returns — a self-consistent but wrong normalization would satisfy it. Only a
// real server settles that, and the consequence of getting it wrong is not
// cosmetic: a comparison that never reports "equal" makes the controller
// rewrite the whole schema on every reconcile, forever, on an idle cluster.
func TestIntegration_RunAllConvergesAgainstRealSpiceDB(t *testing.T) {
	conn, token := newIntegrationConn(t)
	_ = conn // base schema already written

	endpoint := testspicedb.SharedEndpoint(t)
	cli, err := spicedb.NewClient(endpoint, token, true /* insecure */)
	require.NoError(t, err, "spicedb.NewClient")
	t.Cleanup(func() { _ = cli.Close() })

	io := useExpirationTolerant{spicedb.SchemaIOFor(cli)}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// A fragment-declared resource plus a grant pair over it — the same shape
	// the AgentClass → AgentSessionGrants path produces in production.
	fragment := &spiceboxv1alpha1.SpiceDBSchemaFragment{
		Resources: []spiceboxv1alpha1.SpiceDBResource{{
			Name:        "conv_doc",
			Permissions: []spiceboxv1alpha1.SpiceDBPermission{{Name: "read", Expr: "self"}},
			Relations:   []spiceboxv1alpha1.SpiceDBRelation{{Name: "self", SubjectType: "user"}},
		}},
	}
	pairs := []guardianschema.GrantPair{{ResourceType: "conv_doc", Permission: "read"}}
	frags := []guardianschema.IdentifiedFragment{{Fragment: fragment}}

	first, err := guardianschema.RunAll(ctx, io, frags, nil, pairs, nil)
	require.NoError(t, err, "RunAll pass 1")
	assert.True(t, first.Changed, "pass 1 must write: the grant relation is not in the base schema yet")

	live, err := io.ReadSchema(ctx)
	require.NoError(t, err, "ReadSchema after pass 1")
	require.Contains(t, live, "grant_read_conv_doc", "pass 1 must have written the grant relation")

	// Re-run with identical inputs. Every pass after the first must be a no-op
	// against what the server actually returns.
	for pass := 2; pass <= 4; pass++ {
		res, err := guardianschema.RunAll(ctx, io, frags, nil, pairs, nil)
		require.NoErrorf(t, err, "RunAll pass %d", pass)
		assert.Falsef(t, res.Changed,
			"pass %d rewrote the schema; the loop never converges and the controller rewrites SpiceDB on every reconcile", pass)
	}
}

// TestIntegration_SpiceDBBootstrap_OrderingInvariant uses a real
// SpiceDB to verify the DELETE-before-WriteSchema ordering invariant
// the SpiceDBBootstrap reconciler depends on. A naïve (WriteSchema
// then DeleteRelationships) order would fail SpiceDB's "relation in
// use" check; this test fails fast if that ordering is regressed.
//
// The test runs two passes against fresh datastores (unique bearer
// tokens — serve-testing keys datastores by token):
//
//  1. Positive: seed schema + tuple, then DELETE the tuple, then
//     WriteSchema without the relation. Both calls must succeed.
//  2. Negative: same seed, but flip the order — WriteSchema first.
//     SpiceDB must reject this with "relation in use", proving the
//     invariant is load-bearing (not an accident of our test setup).
//
// (We don't run the actual reconciler against real SpiceDB here — the
// controller test covers the reconciler's call order against a fake.
// THIS test proves the invariant the reconciler enforces is real.)
func TestIntegration_SpiceDBBootstrap_OrderingInvariant(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	endpoint := testspicedb.SharedEndpoint(t)

	// Use a fresh datastore (unique bearer token) so we don't collide
	// with the slice-2 tests above.
	token := testspicedb.UniqueToken(t)
	conn, err := grpc.NewClient(endpoint,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpcutil.WithInsecureBearerToken(token),
	)
	require.NoError(t, err, "dial spicedb")
	t.Cleanup(func() { _ = conn.Close() })
	pc := v1.NewPermissionsServiceClient(conn)
	sc := v1.NewSchemaServiceClient(conn)

	// Initial schema: includes a `team` definition with a `lead`
	// relation. We'll seed a tuple on that relation, then attempt to
	// remove the relation from the schema — first the right way
	// (DELETE then WriteSchema), then the wrong way (WriteSchema
	// first) against a clean datastore.
	initialSchema := `
use expiration

caveat check_hash(arguments_hash string, allowed_arguments_hash string) {
    arguments_hash == allowed_arguments_hash
}

definition user {}

definition team {
    relation lead: user
}

definition agentsession {
    relation started_by: user
    permission interact = started_by
}
`
	updatedSchema := strings.ReplaceAll(initialSchema, "    relation lead: user\n", "")

	_, err = sc.WriteSchema(ctx, &v1.WriteSchemaRequest{Schema: initialSchema})
	require.NoError(t, err, "initial WriteSchema")
	_, err = pc.WriteRelationships(ctx, &v1.WriteRelationshipsRequest{
		Updates: []*v1.RelationshipUpdate{{
			Operation: v1.RelationshipUpdate_OPERATION_TOUCH,
			Relationship: &v1.Relationship{
				Resource: &v1.ObjectReference{ObjectType: "team", ObjectId: "alpha"},
				Relation: "lead",
				Subject: &v1.SubjectReference{
					Object: &v1.ObjectReference{ObjectType: "user", ObjectId: "bob-canon"},
				},
			},
		}},
	})
	require.NoError(t, err, "seed tuple")

	// Sanity-check the seed.
	requireTupleExists(t, ctx, pc, "team", "alpha", "lead", "user", "bob-canon")

	// Positive pass: DELETE the tuple, THEN WriteSchema without the
	// `lead` relation. Both must succeed — this is the order the
	// SpiceDBBootstrap reconciler enforces.
	_, err = pc.DeleteRelationships(ctx, &v1.DeleteRelationshipsRequest{
		RelationshipFilter: &v1.RelationshipFilter{
			ResourceType:       "team",
			OptionalResourceId: "alpha",
			OptionalRelation:   "lead",
			OptionalSubjectFilter: &v1.SubjectFilter{
				SubjectType:       "user",
				OptionalSubjectId: "bob-canon",
			},
		},
	})
	require.NoError(t, err, "DELETE before WriteSchema must succeed")

	_, err = sc.WriteSchema(ctx, &v1.WriteSchemaRequest{Schema: updatedSchema})
	require.NoError(t, err, "WriteSchema after DELETE must succeed")

	// Negative pass: a clean datastore, same seed, but flip the order
	// — WriteSchema first. SpiceDB must reject this. This is what
	// proves the DELETE-first ordering is load-bearing rather than
	// just our convention.
	token2 := testspicedb.UniqueToken(t)
	conn2, err := grpc.NewClient(endpoint,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpcutil.WithInsecureBearerToken(token2),
	)
	require.NoError(t, err, "dial spicedb (clean datastore)")
	t.Cleanup(func() { _ = conn2.Close() })
	sc2 := v1.NewSchemaServiceClient(conn2)
	pc2 := v1.NewPermissionsServiceClient(conn2)
	_, err = sc2.WriteSchema(ctx, &v1.WriteSchemaRequest{Schema: initialSchema})
	require.NoError(t, err, "initial WriteSchema (clean datastore)")
	_, err = pc2.WriteRelationships(ctx, &v1.WriteRelationshipsRequest{
		Updates: []*v1.RelationshipUpdate{{
			Operation: v1.RelationshipUpdate_OPERATION_TOUCH,
			Relationship: &v1.Relationship{
				Resource: &v1.ObjectReference{ObjectType: "team", ObjectId: "alpha"},
				Relation: "lead",
				Subject:  &v1.SubjectReference{Object: &v1.ObjectReference{ObjectType: "user", ObjectId: "bob-canon"}},
			},
		}},
	})
	require.NoError(t, err, "seed tuple (clean datastore)")
	_, err = sc2.WriteSchema(ctx, &v1.WriteSchemaRequest{Schema: updatedSchema})
	assert.Error(t, err, "WriteSchema BEFORE DELETE must fail — proves the ordering is load-bearing")
	if err != nil {
		t.Logf("expected error from WriteSchema-before-DELETE: %v", err)
	}
}

// requireTupleExists reads the named relationship back from SpiceDB
// and fails the test if it is not present. Local to this file —
// integration-test scaffolding only.
func requireTupleExists(t *testing.T, ctx context.Context, pc v1.PermissionsServiceClient, resType, resID, relation, subjType, subjID string) {
	t.Helper()
	stream, err := pc.ReadRelationships(ctx, &v1.ReadRelationshipsRequest{
		// FullyConsistent: this helper runs immediately after a
		// WriteRelationships in the same test. Without it the read may be
		// served from a snapshot predating the write and return zero rows
		// (stream.Recv → io.EOF), which the caller surfaces as a spurious
		// "tuple to exist" failure. Matches the consistency the caveat
		// check earlier in this file already uses for read-after-write.
		Consistency: &v1.Consistency{Requirement: &v1.Consistency_FullyConsistent{FullyConsistent: true}},
		RelationshipFilter: &v1.RelationshipFilter{
			ResourceType:       resType,
			OptionalResourceId: resID,
			OptionalRelation:   relation,
			OptionalSubjectFilter: &v1.SubjectFilter{
				SubjectType:       subjType,
				OptionalSubjectId: subjID,
			},
		},
	})
	require.NoError(t, err, "ReadRelationships")
	_, err = stream.Recv()
	require.NoError(t, err, "expected tuple %s:%s#%s@%s:%s to exist", resType, resID, relation, subjType, subjID)
}
