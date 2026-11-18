package bootstrap

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestMintSetFingerprint_StableAndTracksTheMintSet(t *testing.T) {
	base := MintSetFingerprint()
	assert.Equal(t, base, MintSetFingerprint(), "a pure function of compiled-in values")

	orig := MemoryTokenSecretNames
	t.Cleanup(func() { MemoryTokenSecretNames = orig })
	MemoryTokenSecretNames = append(append([]string{}, orig...), "spicebox-newcomponent-memory-token")
	assert.NotEqual(t, base, MintSetFingerprint(), "adding a Secret to the mint set changes the fingerprint")
}

// TestMintedSecretNames_MatchesWhatMintAllCreates keeps the one list the
// fingerprint and `oap render --validate` derive from equal to what
// MintAllInternalSecrets really creates, for every skip combination. A Secret
// added to the mint without the list would leave the fingerprint unchanged, so
// the bootstrap Job would keep its name under a mutable tag and never re-run.
func TestMintedSecretNames_MatchesWhatMintAllCreates(t *testing.T) {
	for _, opts := range []MintOptions{
		{},
		{SkipSpiceDBToken: true},
		{SkipPostgresToken: true},
		{SkipSpiceDBToken: true, SkipPostgresToken: true},
	} {
		t.Run(fmt.Sprintf("%+v", opts), func(t *testing.T) {
			c := newFakeClient()
			require.NoError(t, MintAllInternalSecrets(context.Background(), c, c, opts, nil))
			var list corev1.SecretList
			require.NoError(t, c.List(context.Background(), &list, client.InNamespace(TokenSecretNamespace)))
			created := make([]string, 0, len(list.Items))
			for _, s := range list.Items {
				created = append(created, s.Name)
			}
			assert.ElementsMatch(t, created, MintedSecretNames(opts))
		})
	}
}
