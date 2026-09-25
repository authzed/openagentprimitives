//go:build integration

// Semantic tests for the opt-in org-wide artifact audience:
//
//	agentsession#artifact_org_viewer: user:*
//	agentsession#artifact_org_view = artifact_org_viewer - denied
//	artifact#view = parent->interact + parent->artifact_org_view + platform->view_audit
//
// The wildcard tuple widens artifact#view — and ONLY artifact#view — to any
// user subject. "Any user" is scoped to the corp directory one layer up: webd
// only mints subjects for IdP-verified logins. What the schema itself must
// hold, and what this file pins: interact stays closed (no mirrors, no send),
// denied still beats the wildcard, delete is untouched, and no tuple means no
// widening.
package schema_test

import (
	"testing"

	v1 "github.com/authzed/authzed-go/proto/authzed/api/v1"
	"github.com/stretchr/testify/assert"
)

func TestArtifactOrgViewerWildcard(t *testing.T) {
	const (
		sess = "agentsession:default/s1"
		art  = "artifact:artifact-abc"
	)
	base := []string{
		art + "#parent@" + sess,
		art + "#platform@platform:platform",
		sess + "#owner@user:alice",
	}
	optedIn := append(append([]string{}, base...), sess+"#artifact_org_viewer@user:*")

	t.Run("wildcard tuple grants artifact view to an unrelated user", func(t *testing.T) {
		got := checkOnCanonicalSchema(t, optedIn, art, "view", "user:zoe")
		assert.Equal(t, v1.CheckPermissionResponse_PERMISSIONSHIP_HAS_PERMISSION, got,
			"an opted-in session's artifact must be viewable by any user subject")
	})

	t.Run("without the tuple an unrelated user stays denied", func(t *testing.T) {
		got := checkOnCanonicalSchema(t, base, art, "view", "user:zoe")
		assert.Equal(t, v1.CheckPermissionResponse_PERMISSIONSHIP_NO_PERMISSION, got,
			"absent tuple must mean today's behavior exactly — no widening")
	})

	t.Run("the wildcard does not widen session interact", func(t *testing.T) {
		got := checkOnCanonicalSchema(t, optedIn, sess, "interact", "user:zoe")
		assert.Equal(t, v1.CheckPermissionResponse_PERMISSIONSHIP_NO_PERMISSION, got,
			"org viewers get the artifact only — never the conversation/plan mirrors or send standing")
	})

	t.Run("denied beats the wildcard", func(t *testing.T) {
		rels := append(append([]string{}, optedIn...), sess+"#denied@user:mallory")
		got := checkOnCanonicalSchema(t, rels, art, "view", "user:mallory")
		assert.Equal(t, v1.CheckPermissionResponse_PERMISSIONSHIP_NO_PERMISSION, got,
			"the session's deny list must stay authoritative over the org-wide grant")
	})

	t.Run("artifact delete is not widened", func(t *testing.T) {
		got := checkOnCanonicalSchema(t, optedIn, art, "delete", "user:zoe")
		assert.Equal(t, v1.CheckPermissionResponse_PERMISSIONSHIP_NO_PERMISSION, got,
			"the wildcard must reach view and nothing else")
	})

	t.Run("owner keeps view with or without the tuple", func(t *testing.T) {
		got := checkOnCanonicalSchema(t, base, art, "view", "user:alice")
		assert.Equal(t, v1.CheckPermissionResponse_PERMISSIONSHIP_HAS_PERMISSION, got,
			"the parent->interact arm must be preserved untouched")
	})
}
