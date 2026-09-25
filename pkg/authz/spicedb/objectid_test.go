package spicedb

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAgentIdentityObjectID pins the gap between what Kubernetes accepts as an
// object name and what SpiceDB accepts as an object id.
//
// The gap is not theoretical: '.' is legal in a DNS-1123 subdomain and illegal
// in an object_id, no CRD pattern forbids it, and the resulting write fails
// PERMANENTLY while looking exactly like a transient SpiceDB error. Every row
// here is a name `kubectl create` accepts today.
func TestAgentIdentityObjectID(t *testing.T) {
	cases := []struct {
		name    string
		ns, obj string
		want    string // "" ⇒ must be refused as unrepresentable
	}{
		{name: "ordinary DNS-1123 label: composed as <ns>/<name>", ns: "default", obj: "support-bot", want: "default/support-bot"},
		{name: "digits and underscores are inside the charset", ns: "team-1", obj: "bot_2", want: "team-1/bot_2"},
		{name: "a dotted name is LEGAL in Kubernetes and refused here", ns: "default", obj: "support.bot"},
		{name: "a dot anywhere refuses, including a trailing one", ns: "default", obj: "bot."},
		{name: "an empty name would compose a dangling <ns>/ and is refused", ns: "default", obj: ""},
		{name: "an empty namespace is refused rather than silently rooted", ns: "", obj: "support-bot"},
		{name: "over the 1024-char ceiling: refused", ns: "default", obj: strings.Repeat("a", 1024)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := AgentIdentityObjectID(tc.ns, tc.obj)
			if tc.want == "" {
				require.Error(t, err, "an unrepresentable id must be refused BEFORE any RPC")
				assert.ErrorIs(t, err, ErrUnrepresentableObjectID,
					"and refused with the typed sentinel: the reconciler branches on it to stop retrying forever")
				assert.Empty(t, got, "a refused composition must not hand back a partial id a caller could use")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestAgentIdentityObjectID_AcceptsTheFullDeclaredCharset guards against a
// pattern tightened past what SpiceDB actually allows. Over-refusing is the
// quieter failure of the two: it would report "this identity can never be
// linked" about names that link fine.
func TestAgentIdentityObjectID_AcceptsTheFullDeclaredCharset(t *testing.T) {
	got, err := AgentIdentityObjectID("ns", "aZ0_|-=+")
	require.NoError(t, err, "every character SpiceDB's object_id grammar admits must be accepted")
	assert.Equal(t, "ns/aZ0_|-=+", got)
}
