package spicedb

// The agentclass#starter grant writers — the standing override for the
// session-start gate ("this guest may start sessions of this class without
// per-session admin approval"). The write/delete round-trip against a live
// SpiceDB is covered by the integration suite; these unit tests pin the
// fail-before-RPC contract shared with every other AgentClassObjectID caller.

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// TestAgentClassStarter_UnrepresentableClassRefusedBeforeRPC: a class name
// SpiceDB cannot express (a dotted DNS name) must be refused with the typed
// sentinel BEFORE any RPC — the nil cl would panic if a request were built.
func TestAgentClassStarter_UnrepresentableClassRefusedBeforeRPC(t *testing.T) {
	c := &Client{} // cl is nil: any RPC attempt panics
	alice := identity.CanonicalFromTrusted("YWxpY2VAZXhhbXBsZS5jb20", "test fixture")

	t.Run("TouchAgentClassStarter", func(t *testing.T) {
		err := c.TouchAgentClassStarter(context.Background(), "default", "demo.agent", alice)
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrUnrepresentableObjectID)
	})
	t.Run("DeleteAgentClassStarter", func(t *testing.T) {
		err := c.DeleteAgentClassStarter(context.Background(), "default", "demo.agent", alice)
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrUnrepresentableObjectID)
	})
	t.Run("ListAgentClassStarters", func(t *testing.T) {
		_, err := c.ListAgentClassStarters(context.Background(), "default", "demo.agent")
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrUnrepresentableObjectID)
	})
}
