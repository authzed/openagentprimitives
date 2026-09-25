package spicedb

// Coverage for parseSubjectRef — the optional-relation subject parser behind
// TouchOwner. Distinct from ParseSubject (tested in client_test.go), which
// REQUIRES the #relation segment. TouchOwner writes the one relation every
// session gate resolves through (interact / manage_scope / fork / approve), so
// a subject parsed into the wrong shape silently hands the session to the
// wrong principal — or to nobody.

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseSubjectRef(t *testing.T) {
	cases := []struct {
		name     string
		in       string
		objType  string
		objID    string
		relation string
		wantErr  bool
	}{
		{
			name: "direct user subject: relation left empty", in: "user:alice@example.com",
			objType: "user", objID: "alice@example.com",
		},
		{
			name: "subject-set ref: relation captured", in: "group:eng#member",
			objType: "group", objID: "eng", relation: "member",
		},
		{
			name: "id containing a slash is preserved whole", in: "team:acme/platform#member",
			objType: "team", objID: "acme/platform", relation: "member",
		},
		{
			name: "trailing hash with empty relation parses as a direct subject", in: "user:alice#",
			objType: "user", objID: "alice",
		},
		{
			name: "only the FIRST colon separates type from id", in: "channel:C9:thread#member",
			objType: "channel", objID: "C9:thread", relation: "member",
		},
		{
			name: "only the FIRST hash separates id from relation", in: "group:eng#member#extra",
			objType: "group", objID: "eng", relation: "member#extra",
		},
		{name: "missing colon: refused", in: "alice", wantErr: true},
		{name: "empty type: refused", in: ":alice", wantErr: true},
		{name: "trailing colon with no id: refused", in: "user:", wantErr: true},
		{name: "empty string: refused", in: "", wantErr: true},
		{name: "hash immediately after the colon leaves an empty id: refused", in: "user:#member", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			objType, objID, rel, err := parseSubjectRef(tc.in)
			if tc.wantErr {
				require.Error(t, err, "malformed subject ref %q must be refused", tc.in)
				assert.Empty(t, objType, "a refused parse must not hand back partial parts")
				assert.Empty(t, objID)
				assert.Empty(t, rel)
				return
			}
			require.NoError(t, err, "parseSubjectRef(%q)", tc.in)
			assert.Equal(t, tc.objType, objType, "objType")
			assert.Equal(t, tc.objID, objID, "objID")
			assert.Equal(t, tc.relation, rel, "relation")
		})
	}
}

// TestTouchOwner_RefusesMalformedSubjectBeforeAnyRPC pins the ordering: a
// subject ref that cannot be parsed must be refused before the client is
// touched. The receiver's inner client is nil, so reaching the RPC panics —
// passing proves the parse guard runs first, and that the caller learns the
// owner was never written rather than assuming it landed.
func TestTouchOwner_RefusesMalformedSubjectBeforeAnyRPC(t *testing.T) {
	c := &Client{} // cl is nil: any RPC attempt panics

	for _, bad := range []string{"", "alice", ":alice", "user:", "user:#member"} {
		t.Run("malformed subject "+bad+": refused, nothing written", func(t *testing.T) {
			err := c.TouchOwner(context.Background(), "demo-ns", "demo-session", bad)
			require.Error(t, err, "a malformed owner subject must not reach SpiceDB")
			assert.Contains(t, err.Error(), "touch agentsession#owner",
				"the error must name the operation that failed, not just the parse")
		})
	}
}
