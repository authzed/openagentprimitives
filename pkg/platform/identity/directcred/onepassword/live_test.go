package onepassword_test

import (
	"context"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/platform/identity/directcred"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/directcred/onepassword"
)

// TestLiveVerifyAndList exercises the real SDK (wasm core and all) against a
// live 1Password service account. It is gated on OP_SERVICE_ACCOUNT_TOKEN and
// OP_TEST_VAULT and skips with a reason otherwise, so it is excluded from the
// ship gate by construction -- the unit suite builds it but never runs its
// body without those env vars.
func TestLiveVerifyAndList(t *testing.T) {
	token := os.Getenv("OP_SERVICE_ACCOUNT_TOKEN")
	vaultName := os.Getenv("OP_TEST_VAULT")
	if token == "" || vaultName == "" {
		t.Skipf("set OP_SERVICE_ACCOUNT_TOKEN and OP_TEST_VAULT to run the live 1Password round-trip")
	}

	src := onepassword.New()
	ctx := context.Background()
	cfg := directcred.Config{
		SecretData: map[string][]byte{"token": []byte(token)},
		Vault:      vaultName,
	}

	pin, err := src.Verify(ctx, cfg)
	require.NoError(t, err, "Verify against the live vault")
	assert.Equal(t, vaultName, pin.VaultName)
	require.NotEmpty(t, pin.VaultID)

	cfg.VaultID = pin.VaultID
	refs, err := src.List(ctx, cfg)
	require.NoError(t, err, "List against the live vault")
	for _, r := range refs {
		assert.NotEmpty(t, r.ID, "every Ref must carry an id")
		for _, h := range r.Hosts {
			assert.NotEmpty(t, h.Name, "every host must be non-empty and canonical")
		}
	}
	t.Logf("live vault %q (%s): %d login refs", pin.VaultName, pin.VaultID, len(refs))
}
