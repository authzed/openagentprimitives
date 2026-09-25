//go:build integration

// Integration coverage for SyncArtifactOrgViewer against a real SpiceDB: the
// schema-semantics suite (pkg/authz/spicedb/schema) proves what the schema
// MEANS; this file proves the CLIENT writes tuples in the shape the schema
// consumes — the Touch-shape-vs-Check-shape drift a fake cannot catch.
package spicedb

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/test/testspicedb"
)

// TestIntegration_SyncArtifactOrgViewer_NoSchemaIsNoOp pins the pre-first-compose
// case: the agentsession definition is written to SpiceDB only when the guardian
// composes the schema (driven by a class becoming Valid). A session whose class
// was NEVER valid reconciles against an instance with NO schema, and the sync
// runs on every reconcile before the class-valid gate. A delete against an
// absent object definition is a no-op — no schema means no session ever ran,
// hence no artifacts to widen or revoke — so it must NOT fail the reconcile.
// Enable is the same: nothing to grant yet, and the class watch re-levels once
// the schema lands.
func TestIntegration_SyncArtifactOrgViewer_NoSchemaIsNoOp(t *testing.T) {
	endpoint := testspicedb.Endpoint(t)
	token := testspicedb.UniqueToken(t) // fresh datastore, no WriteSchema — schema is absent
	c, err := NewClient(endpoint, token, true)
	require.NoError(t, err, "NewClient")
	t.Cleanup(func() { _ = c.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	assert.NoError(t, c.SyncArtifactOrgViewer(ctx, "default", "never-valid-sess", false),
		"disable against a schema-less instance must be a no-op, not a reconcile-failing error")
	assert.NoError(t, c.SyncArtifactOrgViewer(ctx, "default", "never-valid-sess", true),
		"enable against a schema-less instance must be a no-op — nothing has run, so nothing to grant yet")
}

// TestIntegration_SyncArtifactOrgViewer_LevelsTheWildcard drives the full
// level-triggered cycle a reconciler performs: disabled-when-absent (the hot
// path for every never-opted-in session) must be a clean no-op, enable must
// open artifact#view to an unrelated user, disable must revoke it again.
func TestIntegration_SyncArtifactOrgViewer_LevelsTheWildcard(t *testing.T) {
	c := newIntegrationClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	ns := "integration-test"
	sessName := uniq(t, "artifact-org-sess-")
	artifactID := uniq(t, "artifact-org-")
	starter := uniqCanon(t, "starter-")
	orgUser := uniqCanon(t, "org-user-") // holds NO standing on the session

	t.Cleanup(func() { _ = c.DeleteAgentSessionRelationships(context.Background(), ns, sessName) })

	require.NoError(t, c.TouchStartedBy(ctx, ns, sessName, starter), "TouchStartedBy")
	require.NoError(t, c.TouchOwner(ctx, ns, sessName, "user:"+starter.String()), "TouchOwner")
	require.NoError(t, c.TouchArtifactParent(ctx, artifactID, ns, sessName), "TouchArtifactParent")

	// Disabled while already absent: the default level for every session whose
	// class never opted in, re-applied on each reconcile — must not error.
	require.NoError(t, c.SyncArtifactOrgViewer(ctx, ns, sessName, false),
		"leveling to disabled with no tuple present must be an idempotent no-op")

	off, err := c.CheckArtifactView(ctx, artifactID, orgUser, true)
	require.NoError(t, err)
	assert.False(t, off, "before opt-in an unrelated user must NOT view the artifact")

	require.NoError(t, c.SyncArtifactOrgViewer(ctx, ns, sessName, true), "enable")
	on, err := c.CheckArtifactView(ctx, artifactID, orgUser, true)
	require.NoError(t, err)
	assert.True(t, on, "after opt-in any user subject must view the artifact")

	interactOK, err := c.CheckInteract(ctx, ns, sessName, orgUser, true)
	require.NoError(t, err)
	assert.False(t, interactOK, "the wildcard must never widen interact")

	require.NoError(t, c.SyncArtifactOrgViewer(ctx, ns, sessName, false), "disable (revoke)")
	revoked, err := c.CheckArtifactView(ctx, artifactID, orgUser, true)
	require.NoError(t, err)
	assert.False(t, revoked, "flipping off must revoke org-wide view (level-triggered, not frozen)")

	// The session's own people are untouched by the flip.
	starterOK, err := c.CheckArtifactView(ctx, artifactID, starter, true)
	require.NoError(t, err)
	assert.True(t, starterOK, "the starter's view via parent->interact must survive the opt-out")
}
