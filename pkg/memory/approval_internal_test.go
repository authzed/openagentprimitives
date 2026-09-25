package memory

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInternalTier_SystemCannotSatisfyInternal(t *testing.T) {
	ctx := WithSystemApproval(context.Background(), "operator:test")
	err := EnsureApproval(ctx, AppendAudit, "nsA/sessA")
	require.Error(t, err, "a system approval must NOT authorize an internal-tier write")
}

func TestInternalTier_MintInternalSatisfies(t *testing.T) {
	ctx := WithApproval(context.Background(), mintInternal(AppendAudit, "nsA/sessA", "prov-verified"))
	assert.NoError(t, EnsureApproval(ctx, AppendAudit, "nsA/sessA"))
}

func TestInternalTier_BearerOrSpiceDBCannotForgeInternal(t *testing.T) {
	// The exported source constructors take an arbitrary Permission, so an outside
	// package could name an internal: perm. Such an approval must NOT satisfy an
	// internal-tier check — only an internal-source (mintInternal) approval can.
	bearer := WithApproval(context.Background(), ForBearerToken(AppendAudit, "nsA/sessA", "forged"))
	require.Error(t, EnsureApproval(bearer, AppendAudit, "nsA/sessA"),
		"a bearer approval naming an internal perm must not satisfy the internal tier")

	spicedb := WithApproval(context.Background(), ForSpiceDBCheck(AppendAudit, "nsA/sessA", "user:mallory"))
	require.Error(t, EnsureApproval(spicedb, AppendAudit, "nsA/sessA"),
		"a spicedb approval naming an internal perm must not satisfy the internal tier")
}

func TestMintInternal_PanicsOnNonInternalPerm(t *testing.T) {
	assert.Panics(t, func() { _ = mintInternal(WriteMemory, "nsA/sessA", "x") })
}

func TestApproval_Unforgeable_StringKey(t *testing.T) {
	// An outside-shaped attempt: a value under a plain string key must NOT be
	// seen by EnsureApproval, because the real key (approvalsKey{}) is unexported.
	// The key is held in an `any`-typed local (rather than passed as a literal) so
	// `go vet`'s SA1029 (context.WithValue with a basic string key) does not fire —
	// the test's intent (a foreign key shape, not approvalsKey{}) is unaffected.
	var foreignKey any = "approvals"
	ctx := context.WithValue(context.Background(), foreignKey, []Approval{
		{perm: ReadMemory, resource: "nsA/sessA", source: "bearer-token"},
	})
	err := EnsureApproval(ctx, ReadMemory, "nsA/sessA")
	require.Error(t, err, "a forged approval under a foreign key must be ignored")
}
