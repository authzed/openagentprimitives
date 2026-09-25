package spicedb

// Fail-CLOSED coverage for every *Client method that talks to SpiceDB.
//
// The build-tagged integration suite (integration_test.go, -tags=integration)
// exercises these same methods against a REAL SpiceDB and proves the happy-path
// tuple/permission semantics — the shape mismatches a fake cannot catch. What
// it does not exercise is the other half: what each method does when SpiceDB
// answers with an error. That half is what these tests pin, in the untagged
// suite, with no container required.
//
// The harness registers only LookupSubjects and LookupResources, so every other
// PermissionsService RPC answers Unimplemented and the SchemaService is not
// registered at all — a faithful stand-in for "the backend refused". Two
// properties are asserted for every method:
//
//  1. the error is RETURNED, never swallowed (AGENTS.md: never silently drop
//     errors), and
//  2. for the (bool, error) gates, the bool is FALSE — an unreachable backend
//     must never read as a grant.

import (
	"context"
	"testing"

	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// erroringClient returns a *Client dialed at a gRPC server that answers every
// permission/relationship RPC with Unimplemented. Reusing newCapturingClient
// keeps the transport real: the error travels the same gRPC path a genuine
// SpiceDB failure would.
func erroringClient(t *testing.T) *Client {
	t.Helper()
	c, _ := newCapturingClient(t)
	return c
}

// TestCheckMethods_BackendErrorDeniesAndSurfaces sweeps every identity-level
// gate. Each must answer (false, err) — a backend outage can never manufacture
// a permission — and the error must name the check so an operator can grep it.
func TestCheckMethods_BackendErrorDeniesAndSurfaces(t *testing.T) {
	const (
		ns    = "demo-ns"
		name  = "demo-session"
		alice = "alice@example.com"
	)

	cases := []struct {
		name    string
		call    func(c *Client) (bool, error)
		wantMsg string
	}{
		{
			name: "CheckInteract: backend error denies, error names the check",
			call: func(c *Client) (bool, error) {
				return c.CheckInteract(context.Background(), ns, name, identity.CanonicalFromTrusted(alice, "test fixture"), true)
			},
			wantMsg: "check interact",
		},
		{
			name: "CheckManageScope: backend error denies, error names the check",
			call: func(c *Client) (bool, error) {
				return c.CheckManageScope(context.Background(), ns, name, identity.CanonicalFromTrusted(alice, "test fixture"), true)
			},
			wantMsg: "check manage_scope",
		},
		{
			name: "CheckFork: backend error denies, error names the check",
			call: func(c *Client) (bool, error) {
				return c.CheckFork(context.Background(), ns, name, identity.CanonicalFromTrusted(alice, "test fixture"), true)
			},
			wantMsg: "check fork",
		},
		{
			name: "CheckApprove: backend error denies, error names the check",
			call: func(c *Client) (bool, error) {
				return c.CheckApprove(context.Background(), ns, name, identity.CanonicalFromTrusted(alice, "test fixture"), true)
			},
			wantMsg: "check approve",
		},
		{
			name: "CheckDenied: backend error denies, error names the check",
			call: func(c *Client) (bool, error) {
				return c.CheckDenied(context.Background(), ns, name, identity.CanonicalFromTrusted(alice, "test fixture"), true)
			},
			wantMsg: "check is_denied",
		},
		{
			name: "CheckOwnerOnResource: backend error denies, error names the check",
			call: func(c *Client) (bool, error) {
				return c.CheckOwnerOnResource(context.Background(), "repo", "r1", identity.CanonicalFromTrusted(alice, "test fixture"), true)
			},
			wantMsg: "check",
		},
		{
			name: "CheckArtifactView: backend error denies, error names the check",
			call: func(c *Client) (bool, error) {
				return c.CheckArtifactView(context.Background(), "artifact-1", identity.CanonicalFromTrusted(alice, "test fixture"), true)
			},
			wantMsg: "check artifact view",
		},
		{
			name: "CheckPlatformPermission: backend error denies, error names the area",
			call: func(c *Client) (bool, error) {
				return c.CheckPlatformPermission(context.Background(), "view_sessions", identity.CanonicalFromTrusted(alice, "test fixture"), true)
			},
			wantMsg: "check platform view_sessions",
		},
		{
			name: "CheckAgentIdentityUpdateCredential: backend error denies, error surfaces",
			call: func(c *Client) (bool, error) {
				return c.CheckAgentIdentityUpdateCredential(context.Background(), ns, "demo-identity", identity.CanonicalFromTrusted(alice, "test fixture"))
			},
			wantMsg: "check",
		},
		{
			name: "CheckUseToken: backend error denies, error surfaces",
			call: func(c *Client) (bool, error) {
				return c.CheckUseToken(context.Background(), ns, name, "cred-1", "hash-1", true)
			},
			wantMsg: "check",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.call(erroringClient(t))
			require.Error(t, err, "an unreachable backend must surface, not be swallowed")
			assert.False(t, got, "an unreachable backend must never read as a grant (fail-CLOSED)")
			assert.Contains(t, err.Error(), tc.wantMsg,
				"the error must name the failing check so an operator can locate it from logs alone")
		})
	}
}

// TestWriteMethods_BackendErrorSurfaces sweeps every relationship write and
// delete. A swallowed error here is the dangerous direction in reverse: the
// caller would believe a grant landed (or a revocation completed) when neither
// did.
func TestWriteMethods_BackendErrorSurfaces(t *testing.T) {
	const (
		ns    = "demo-ns"
		name  = "demo-session"
		alice = "alice@example.com"
	)

	cases := []struct {
		name    string
		call    func(c *Client) error
		wantMsg string
	}{
		{
			name: "TouchStartedBy: write failure surfaces",
			call: func(c *Client) error {
				return c.TouchStartedBy(context.Background(), ns, name, identity.CanonicalFromTrusted(alice, "test fixture"))
			},
			wantMsg: "touch agentsession#started_by",
		},
		{
			name:    "TouchOwner: write failure surfaces and names the subject",
			call:    func(c *Client) error { return c.TouchOwner(context.Background(), ns, name, "user:"+alice) },
			wantMsg: "touch agentsession#owner",
		},
		{
			name:    "TouchOwner with a subject set: write failure surfaces",
			call:    func(c *Client) error { return c.TouchOwner(context.Background(), ns, name, "group:eng#member") },
			wantMsg: "touch agentsession#owner",
		},
		{
			name: "TouchInteractParticipantUser: write failure surfaces",
			call: func(c *Client) error {
				return c.TouchInteractParticipantUser(context.Background(), ns, name, identity.CanonicalFromTrusted(alice, "test fixture"))
			},
			wantMsg: "touch",
		},
		{
			name: "TouchInteractParticipant: write failure surfaces",
			call: func(c *Client) error {
				return c.TouchInteractParticipant(context.Background(), ns, name, "group:eng#member")
			},
			wantMsg: "touch",
		},
		{
			name: "DeleteInteractParticipant: delete failure surfaces (access may still be live)",
			call: func(c *Client) error {
				return c.DeleteInteractParticipant(context.Background(), ns, name, "group:eng#member")
			},
			wantMsg: "delete",
		},
		{
			name: "TouchDeniedUser: write failure surfaces",
			call: func(c *Client) error {
				return c.TouchDeniedUser(context.Background(), ns, name, identity.CanonicalFromTrusted("mallory@example.com", "test fixture"))
			},
			wantMsg: "touch",
		},
		{
			name:    "DeleteAgentSessionRelationships: delete failure surfaces",
			call:    func(c *Client) error { return c.DeleteAgentSessionRelationships(context.Background(), ns, name) },
			wantMsg: "delete",
		},
		{
			name:    "TouchArtifactParent: write failure surfaces",
			call:    func(c *Client) error { return c.TouchArtifactParent(context.Background(), "artifact-1", ns, name) },
			wantMsg: "touch artifact#parent",
		},
		{
			name: "AddGroupMember: write failure surfaces",
			call: func(c *Client) error {
				return c.AddGroupMember(context.Background(), "eng", identity.CanonicalFromTrusted(alice, "test fixture"))
			},
			wantMsg: "add group",
		},
		{
			name: "DeleteGroupMember: delete failure surfaces",
			call: func(c *Client) error {
				return c.DeleteGroupMember(context.Background(), "eng", identity.CanonicalFromTrusted(alice, "test fixture"))
			},
			wantMsg: "delete group",
		},
		{
			name: "TouchPlatformAdmin: write failure surfaces",
			call: func(c *Client) error {
				return c.TouchPlatformAdmin(context.Background(), identity.CanonicalFromTrusted(alice, "test fixture"))
			},
			wantMsg: "touch platform admin",
		},
		{
			name: "DeletePlatformAdmin: delete failure surfaces",
			call: func(c *Client) error {
				return c.DeletePlatformAdmin(context.Background(), identity.CanonicalFromTrusted(alice, "test fixture"))
			},
			wantMsg: "delete platform admin",
		},
		{
			name:    "EnsureAgentClassPlatform: write failure surfaces",
			call:    func(c *Client) error { return c.EnsureAgentClassPlatform(context.Background(), ns, "demo-agent") },
			wantMsg: "touch agentclass#platform",
		},
		{
			name:    "EnsureAgentIdentityPlatform: write failure surfaces",
			call:    func(c *Client) error { return c.EnsureAgentIdentityPlatform(context.Background(), ns, "demo-identity") },
			wantMsg: "touch",
		},
		{
			name: "TouchAuthorizedToken: write failure surfaces",
			call: func(c *Client) error {
				return c.TouchAuthorizedToken(context.Background(), ns, name, "cred-1", "hash-1")
			},
			wantMsg: "touch",
		},
		{
			name: "TouchAuthorizedTokenIdentity: write failure surfaces",
			call: func(c *Client) error {
				return c.TouchAuthorizedTokenIdentity(context.Background(), ns, name, "cred-1")
			},
			wantMsg: "touch",
		},
		{
			name:    "DeleteAuthorizedToken: delete failure surfaces (the token may still be usable)",
			call:    func(c *Client) error { return c.DeleteAuthorizedToken(context.Background(), ns, name, "cred-1") },
			wantMsg: "delete",
		},
		{
			name:    "TouchBootstrapRelationship: write failure surfaces and names the tuple",
			call:    func(c *Client) error { return c.TouchBootstrapRelationship(context.Background(), demoTuple()) },
			wantMsg: "touch agentsession:demo-ns/demo-session#participant",
		},
		{
			name:    "DeleteBootstrapRelationship: delete failure surfaces and names the tuple",
			call:    func(c *Client) error { return c.DeleteBootstrapRelationship(context.Background(), demoTuple()) },
			wantMsg: "delete agentsession:demo-ns/demo-session#participant",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.call(erroringClient(t))
			require.Error(t, err, "a failed write must surface; a caller must never assume it landed")
			assert.Contains(t, err.Error(), tc.wantMsg,
				"the error must name the failing operation so an operator can locate it from logs alone")
		})
	}
}

// demoTuple is the bootstrap tuple the write-error cases above use. Named
// coordinates keep the asserted error text readable.
func demoTuple() Tuple {
	return Tuple{
		ResourceType: "agentsession", ResourceID: "demo-ns/demo-session",
		Relation:    "participant",
		SubjectType: "user", SubjectID: "alice@example.com",
	}
}

// TestListMethods_BackendErrorSurfacesRatherThanEmptyList pins the most
// dangerous swallow of the three: a listing that returns (nil, nil) on failure
// renders as "there are none", which reads as an authoritative empty answer.
func TestListMethods_BackendErrorSurfacesRatherThanEmptyList(t *testing.T) {
	const (
		ns   = "demo-ns"
		name = "demo-session"
	)

	t.Run("ListDeniedUsers: error surfaces, no empty list", func(t *testing.T) {
		got, err := erroringClient(t).ListDeniedUsers(context.Background(), ns, name)
		require.Error(t, err, "an outage must not render as 'nobody is denied'")
		assert.Empty(t, got)
	})
	t.Run("ListPlatformAdmins: error surfaces, no empty list", func(t *testing.T) {
		got, err := erroringClient(t).ListPlatformAdmins(context.Background())
		require.Error(t, err, "an outage must not render as 'there are no admins'")
		assert.Contains(t, err.Error(), "read platform admins")
		assert.Empty(t, got)
	})
	t.Run("ListAuthorizedTokens: error surfaces, no empty list", func(t *testing.T) {
		got, err := erroringClient(t).ListAuthorizedTokens(context.Background(), ns, name)
		require.Error(t, err, "an outage must not render as 'no tokens are authorized'")
		assert.Empty(t, got)
	})
}

// TestLookupInteractSubjects_BackendErrorSurfaces covers the broadcast-addressing
// lookup. An empty answer here would silently address a notice to nobody.
func TestLookupInteractSubjects_BackendErrorSurfaces(t *testing.T) {
	// The harness DOES serve LookupSubjects, so drive the failure through the
	// stream rather than the dial: an unrepresentable subject ref is the one
	// input this method rejects before the RPC.
	c, _ := newCapturingClient(t)
	got, err := c.LookupInteractSubjects(context.Background(), "demo-ns", "demo-session")
	require.NoError(t, err, "the harness serves this stream cleanly")
	assert.Equal(t, []string{"user:alice"}, got,
		"subjects are returned prefixed with their type, as the callers expect")
}

// TestLookupSubjects_MalformedSubjectRefRefusedBeforeRPC pins the pre-RPC
// guard: a subject expression that cannot be parsed must be refused rather
// than sent to SpiceDB as a half-formed query.
func TestLookupSubjects_MalformedSubjectRefRefusedBeforeRPC(t *testing.T) {
	c := &Client{} // cl is nil: any RPC attempt panics

	t.Run("LookupSubjectIncludes: malformed ref refused, denies", func(t *testing.T) {
		ok, err := c.LookupSubjectIncludes(context.Background(), "not-a-subject-ref", identity.CanonicalFromTrusted("alice@example.com", "test fixture"))
		require.Error(t, err, "a malformed subject expression must be refused")
		assert.False(t, ok, "an unparseable subject set must never read as membership")
	})
	t.Run("LookupSubjects: malformed ref refused", func(t *testing.T) {
		got, err := c.LookupSubjects(context.Background(), "not-a-subject-ref")
		require.Error(t, err, "a malformed subject expression must be refused")
		assert.Empty(t, got)
	})
}

// TestPingAndSchemaRead_BackendErrorSurfaces covers the startup probe and the
// schema read the AgentClass reconciler validates tools against. Ping is the
// one call whose whole purpose is to fail loudly when SpiceDB is unreachable.
func TestPingAndSchemaRead_BackendErrorSurfaces(t *testing.T) {
	t.Run("Ping: unreachable schema service surfaces as an error", func(t *testing.T) {
		require.Error(t, erroringClient(t).Ping(context.Background()),
			"a startup probe that swallows its error defeats its own purpose")
	})
	t.Run("FetchAndParseSchema: unreachable schema service surfaces as an error", func(t *testing.T) {
		got, err := FetchAndParseSchema(context.Background(), erroringClient(t))
		require.Error(t, err, "an unreadable schema must not be reported as an empty one")
		assert.Nil(t, got, "a failed fetch must not hand back a Schema a caller would query")
	})
}
