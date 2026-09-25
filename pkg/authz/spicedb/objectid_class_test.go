package spicedb

// Coverage for the agentclass half of objectid.go. AgentClassObjectID is the
// structural twin of AgentIdentityObjectID (tested in objectid_test.go) and
// carries the same contract, but it guards a different downstream gate —
// agentclass#start_session and the browser's agent picker — so its refusals
// need their own pinning rather than being assumed to follow from the sibling.

import (
	"context"
	"strings"
	"testing"

	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAgentClassObjectID pins the gap between what Kubernetes accepts as an
// object name and what SpiceDB accepts as an object id. A '.' is legal in a
// DNS-1123 subdomain and absent from SpiceDB's object_id grammar, so a class
// named "support.bot" would produce a write SpiceDB rejects PERMANENTLY while
// the error looks transient — a requeueing reconciler then spins forever.
func TestAgentClassObjectID(t *testing.T) {
	cases := []struct {
		name    string
		ns, obj string
		want    string // "" ⇒ must be refused as unrepresentable
	}{
		{name: "ordinary DNS-1123 label: composed as <ns>/<name>", ns: "default", obj: "demo-agent", want: "default/demo-agent"},
		{name: "digits and underscores are inside the charset", ns: "team-1", obj: "agent_2", want: "team-1/agent_2"},
		{name: "a dotted name is LEGAL in Kubernetes and refused here", ns: "default", obj: "demo.agent"},
		{name: "a dot anywhere refuses, including a trailing one", ns: "default", obj: "demo-agent."},
		{name: "a dotted namespace is refused too", ns: "team.one", obj: "demo-agent"},
		{name: "an empty name would compose a dangling <ns>/ and is refused", ns: "default", obj: ""},
		{name: "an empty namespace is refused rather than silently rooted", ns: "", obj: "demo-agent"},
		{name: "both halves empty: refused", ns: "", obj: ""},
		{name: "over the 1024-char ceiling: refused", ns: "default", obj: strings.Repeat("a", 1024)},
		{name: "a space is outside the charset: refused", ns: "default", obj: "demo agent"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := AgentClassObjectID(tc.ns, tc.obj)
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

// TestAgentClassObjectID_AcceptsTheFullDeclaredCharset guards against a pattern
// tightened past what SpiceDB actually allows. Over-refusing is the quieter
// failure: it would hide classes from the picker that link perfectly well.
func TestAgentClassObjectID_AcceptsTheFullDeclaredCharset(t *testing.T) {
	got, err := AgentClassObjectID("ns", "aZ0_|-=+")
	require.NoError(t, err, "every character SpiceDB's object_id grammar admits must be accepted")
	assert.Equal(t, "ns/aZ0_|-=+", got)
}

// TestAgentClassObjectID_BoundaryLength pins the ceiling as inclusive: exactly
// maxObjectIDLen characters must be accepted, one more refused. An off-by-one
// here silently makes a whole band of legal names unlinkable.
func TestAgentClassObjectID_BoundaryLength(t *testing.T) {
	const ns = "ns"
	fill := maxObjectIDLen - len(ns) - 1 // one char for the "/" separator

	t.Run("exactly at the ceiling: accepted", func(t *testing.T) {
		got, err := AgentClassObjectID(ns, strings.Repeat("a", fill))
		require.NoError(t, err)
		assert.Len(t, got, maxObjectIDLen)
	})
	t.Run("one character over the ceiling: refused", func(t *testing.T) {
		_, err := AgentClassObjectID(ns, strings.Repeat("a", fill+1))
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrUnrepresentableObjectID)
	})
}

// TestEnsureAgentClassPlatform_RefusesUnrepresentableBeforeAnyRPC is the
// property that makes the refusal useful: the check must happen before the
// client is touched. The receiver here has a nil inner client, so reaching the
// RPC would panic — passing proves the guard runs first.
func TestEnsureAgentClassPlatform_RefusesUnrepresentableBeforeAnyRPC(t *testing.T) {
	c := &Client{} // cl is nil: any RPC attempt panics

	err := c.EnsureAgentClassPlatform(context.Background(), "default", "demo.agent")
	require.Error(t, err, "an unrepresentable class name must be refused, not written")
	assert.ErrorIs(t, err, ErrUnrepresentableObjectID,
		"the caller branches on the sentinel to stop requeueing a permanently impossible write")
}

// TestEnsureAgentIdentityPlatform_RefusesUnrepresentableBeforeAnyRPC is the
// identity-side mirror of the same ordering guarantee.
func TestEnsureAgentIdentityPlatform_RefusesUnrepresentableBeforeAnyRPC(t *testing.T) {
	c := &Client{} // cl is nil: any RPC attempt panics

	err := c.EnsureAgentIdentityPlatform(context.Background(), "default", "demo.identity")
	require.Error(t, err, "an unrepresentable identity name must be refused, not written")
	assert.ErrorIs(t, err, ErrUnrepresentableObjectID)
}

// TestCheckAgentIdentityUpdateCredential_UnrepresentableDenies is the
// fail-CLOSED half: when the object id cannot be composed there is no object to
// check, and the only safe answer is "no".
func TestCheckAgentIdentityUpdateCredential_UnrepresentableDenies(t *testing.T) {
	c := &Client{} // cl is nil: any RPC attempt panics

	ok, err := c.CheckAgentIdentityUpdateCredential(context.Background(), "default", "demo.identity", identity.CanonicalFromTrusted("alice@example.com", "test fixture"))
	require.Error(t, err, "the impossibility must surface rather than being swallowed")
	assert.ErrorIs(t, err, ErrUnrepresentableObjectID)
	assert.False(t, ok, "an uncheckable object must never answer 'permitted'")
}
