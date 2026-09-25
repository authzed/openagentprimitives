package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sha256hex mirrors the runner's commitment: hex(sha256(token)).
func sha256hex(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// TestRegistryClaim_HashCommitment asserts the registry stores only the
// SHA-256 hash of the stream token: a Claim presenting the correct raw token
// succeeds (the registry hashes it and compares against the stored hash); a
// Claim presenting a wrong token is rejected. The raw token never lives in the
// ActiveStream — only TokenHash does.
func TestRegistryClaim_HashCommitment(t *testing.T) {
	const rawToken = "the-real-token-preimage"

	cases := []struct {
		name      string
		presented string
		wantOK    bool
	}{
		{name: "correct raw token: Claim succeeds", presented: rawToken, wantOK: true},
		{name: "wrong token: Claim rejected with token mismatch", presented: "not-the-token", wantOK: false},
		{name: "empty token: Claim rejected", presented: "", wantOK: false},
		{name: "the stored hash presented as the token: Claim rejected", presented: sha256hex(rawToken), wantOK: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reg := NewRegistry()
			require.NoError(t, reg.Register(&ActiveStream{
				Namespace:    "default",
				ToolCallName: "tc-1",
				TokenHash:    sha256hex(rawToken),
			}), "Register must succeed")

			active, release, err := reg.Claim("default", "tc-1", tc.presented)
			if tc.wantOK {
				require.NoError(t, err, "Claim with the correct raw token must succeed")
				require.NotNil(t, active, "claimed stream returned")
				assert.NotNil(t, release, "release func returned")
				assert.Equal(t, sha256hex(rawToken), active.TokenHash,
					"registry holds only the hash, never the raw token")
			} else {
				require.Error(t, err, "Claim with a wrong token must be rejected")
				assert.Contains(t, err.Error(), "token mismatch")
				assert.Nil(t, active, "no stream returned on a rejected claim")
			}
		})
	}
}

// TestRegistryClaim_NoStream asserts Claim on an unknown key fails before any
// token comparison.
func TestRegistryClaim_NoStream(t *testing.T) {
	reg := NewRegistry()
	active, _, err := reg.Claim("default", "missing", "anything")
	require.Error(t, err, "Claim on an unregistered key must fail")
	assert.Contains(t, err.Error(), "no active stream")
	assert.Nil(t, active)
}

// TestRegistryClaim_SingleUse asserts a stream can be claimed at most once
// (no resume, no concurrent connections): the correct token claims it, a
// second claim with the same token is refused as already claimed.
//
// Single-use must NOT be implemented by deleting the entry. The claiming
// client is the runner's own bridge, and the operator still has to reach that
// stream after the claim to end the session — see
// TestRegistry_IdleTeardownAfterClaim_CancelsTheExec.
func TestRegistryClaim_SingleUse(t *testing.T) {
	const rawToken = "single-use-token"
	reg := NewRegistry()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	require.NoError(t, reg.Register(&ActiveStream{
		Namespace:    "default",
		ToolCallName: "tc-once",
		TokenHash:    sha256hex(rawToken),
		Cancel:       cancel,
	}))

	_, _, err := reg.Claim("default", "tc-once", rawToken)
	require.NoError(t, err, "first claim with the correct token succeeds")

	_, _, err = reg.Claim("default", "tc-once", rawToken)
	require.Error(t, err, "second claim must be refused — a stream is claimable at most once")
	assert.Contains(t, err.Error(), "already claimed")

	// The refusal must not have come from the entry having vanished: the
	// operator still has to be able to end this session.
	reg.CancelAndUnregister("default", "tc-once")
	assert.Error(t, ctx.Err(), "a claimed stream stays reachable by the controller's deletion path")
}

// TestRegistry_IdleTeardownAfterClaim_CancelsTheExec drives an interactive
// session through its DOCUMENTED NORMAL teardown and asserts the exec actually
// stops.
//
// The ordering is the only one production ever has: reconcileStreaming
// registers the stream with the exec's CancelFunc, the runner's bridge is the
// sole client and claims it the moment it connects, then the bridge goes idle,
// deletes the ToolCall, and the controller's deletion branch calls
// CancelAndUnregister — which is what pkg/agent/tool/sandbox/interactive_tool.go
// documents as the thing that stops the exec and unblocks the bridge's Recv
// loop. If the claim leaves the registry unable to find the stream, every
// interactive session's idle teardown silently fails and the tool runs on with
// nobody attached until the 4h interactive safety ceiling.
func TestRegistry_IdleTeardownAfterClaim_CancelsTheExec(t *testing.T) {
	const rawToken = "idle-teardown-token"
	reg := NewRegistry()
	execCtx, cancelExec := context.WithCancel(context.Background())
	t.Cleanup(cancelExec)

	require.NoError(t, reg.Register(&ActiveStream{
		Namespace:    "default",
		ToolCallName: "tc-idle",
		TokenHash:    sha256hex(rawToken),
		Cancel:       cancelExec,
	}), "reconcileStreaming registers the interactive stream")

	_, _, err := reg.Claim("default", "tc-idle", rawToken)
	require.NoError(t, err, "the bridge claims the stream when it connects")

	// OnIdle deleted the ToolCall; this is the controller's deletion branch.
	reg.CancelAndUnregister("default", "tc-idle")

	assert.Error(t, execCtx.Err(),
		"idle teardown must cancel the exec; a claimed stream the registry cannot find leaves the session running with no client")
}
