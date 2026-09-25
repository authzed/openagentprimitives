package spicedb

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	guardianschema "github.com/authzed/openagentprimitives/pkg/authz/guardian/schema"
)

// TestSchemaIOFor_NilClientReports captures the slice-2 silent-crash
// defensive guard from AGENTS.md "Nil interfaces: never assign a
// typed-nil pointer directly". SchemaIOFor(nil) deliberately returns
// an adapter wrapping a nil *Client (the function intentionally does
// not return a nil interface, because the call site is expected to
// have already nil-checked the *Client). What this test asserts is
// the defensive IsNil() guard: callers that wrap the result in a
// guardianschema.SchemaIO interface can detect "wraps nil" before
// passing the interface to code that would NPE on first call.
//
// The negative case — wrapping a NON-nil *Client — confirms IsNil
// reports false. We can't construct a real authzed.Client here
// without dialing, so the test instantiates SchemaIOAdapter directly
// for the non-nil branch.
func TestSchemaIOFor_NilClientReportsViaIsNil(t *testing.T) {
	adapter := SchemaIOFor(nil)
	assert.True(t, adapter.IsNil(), "SchemaIOFor(nil).IsNil()")

	// The adapter still satisfies the interface STRUCTURALLY even
	// though calling its methods would NPE — this is the typed-nil
	// trap. The test asserts the trap is present and that IsNil is
	// the documented escape hatch.
	var iface guardianschema.SchemaIO = adapter
	assert.NotNil(t, iface, "interface wrapping typed-nil adapter is non-nil (typed-nil trap)")

	// Non-nil branch: a non-nil pointer is wrapped intact.
	nonNil := &Client{} // cl field stays nil; that's fine for the IsNil check
	adapter2 := SchemaIOFor(nonNil)
	assert.False(t, adapter2.IsNil(), "SchemaIOFor(non-nil).IsNil()")
}

func TestParseSubject(t *testing.T) {
	cases := []struct {
		name      string
		in        string
		objType   string
		objID     string
		relation  string
		shouldErr bool
	}{
		{name: "group:engineering#member parses cleanly", in: "group:engineering#member", objType: "group", objID: "engineering", relation: "member"},
		{name: "team with slash in id parses cleanly", in: "team:authzed/spicedb#member", objType: "team", objID: "authzed/spicedb", relation: "member"},
		{name: "missing #relation errors", in: "user:abc", shouldErr: true},
		{name: "missing type:id errors", in: "#member", shouldErr: true},
		{name: "empty string errors", in: "", shouldErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			objType, objID, rel, err := ParseSubject(tc.in)
			if tc.shouldErr {
				assert.Error(t, err, "expected error for %q", tc.in)
				return
			}
			require.NoError(t, err, "ParseSubject(%q)", tc.in)
			assert.Equal(t, tc.objType, objType, "objType")
			assert.Equal(t, tc.objID, objID, "objID")
			assert.Equal(t, tc.relation, rel, "relation")
		})
	}
}

func TestSplitObject(t *testing.T) {
	cases := []struct {
		name      string
		in        string
		objType   string
		objID     string
		shouldErr bool
	}{
		{name: "user:alice parses cleanly", in: "user:alice", objType: "user", objID: "alice"},
		{name: "repo with slash in id", in: "github_repo:authzed/spicedb", objType: "github_repo", objID: "authzed/spicedb"},
		{name: "missing colon errors", in: "alice", shouldErr: true},
		{name: "empty type errors", in: ":alice", shouldErr: true},
		{name: "empty id errors", in: "user:", shouldErr: true},
		{name: "empty string errors", in: "", shouldErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			objType, objID, err := SplitObject(tc.in)
			if tc.shouldErr {
				assert.Error(t, err, "expected error for %q", tc.in)
				return
			}
			require.NoError(t, err, "SplitObject(%q)", tc.in)
			assert.Equal(t, tc.objType, objType, "objType")
			assert.Equal(t, tc.objID, objID, "objID")
		})
	}
}
