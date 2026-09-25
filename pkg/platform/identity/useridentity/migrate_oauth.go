package useridentity

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/pkg/platform/identity/refresh"
)

// redemptionKeys are the three RFC 6749 keys that turn a refresh_token into an
// independently usable credential: where to redeem it, as which client, with which
// authenticator. PutOAuthToken writes them to the sibling Secret, and
// MigrateRedemptionMaterial moves them off a master that still co-locates them.
var redemptionKeys = []string{"token_endpoint", "client_id", "client_secret"}

// MigrateRedemptionMaterial moves a master Secret's CO-LOCATED redemption
// material into its labeled sibling and strips it from the master, reporting
// whether it moved anything. Idempotent: a master carrying none of the keys is
// left untouched and reports false.
//
// The two-Secret split only buys anything if existing credentials get split too.
// PutOAuthToken strips the co-located copy on a re-link, but nothing re-links a
// WORKING credential — so without this pass a co-located master keeps handing
// every userPassthrough runner (granted `get` on it by name, and a Kubernetes read
// returns every key) a self-contained, offline-usable refresh grant for the user's
// upstream account. See refresh.MaterialSecretName.
//
// ORDER IS THE POINT. The sibling is written FIRST and the master stripped only
// after that write returns, so every interruption leaves the material readable
// somewhere: crash before the sibling write and nothing changed, crash between and
// both copies exist (the sibling wins at redemption, the master is stripped next
// pass). Stripping first would destroy the only durable copy of a DCR-minted
// client, leaving an unrefreshable credential recoverable only by a manual re-link.
//
// Failures are returned, never swallowed: a caller that cannot finish must leave
// the credential in its working co-located shape rather than half-migrated. A
// Secret at the sibling's derived name that is NOT this credential's refresh
// material surfaces as an error, and the master keeps its keys.
//
// A missing master is not an error (reports false): the Secret may not exist yet,
// which the validity reconciler reports as Valid=False/SecretMissing.
func MigrateRedemptionMaterial(ctx context.Context, c client.Client, namespace, masterName string) (bool, error) {
	var master corev1.Secret
	key := client.ObjectKey{Namespace: namespace, Name: masterName}
	if err := c.Get(ctx, key, &master); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("useridentity.MigrateRedemptionMaterial: get master secret %q: %w", masterName, err)
	}

	redemption := map[string][]byte{}
	for _, k := range redemptionKeys {
		if v, ok := master.Data[k]; ok && len(v) > 0 {
			redemption[k] = v
		}
	}
	if len(redemption) == 0 {
		return false, nil // already split, or never had material
	}

	if err := putRefreshSecret(ctx, c, &master, redemption); err != nil {
		return false, err
	}
	for _, k := range redemptionKeys {
		delete(master.Data, k)
	}
	if err := c.Update(ctx, &master); err != nil {
		// The sibling now holds the material and the master still does too, so
		// redemption keeps working (the sibling wins) and the next pass retries the
		// strip. Returning the error is what gets that next pass logged.
		return false, fmt.Errorf("useridentity.MigrateRedemptionMaterial: strip redemption material "+
			"from master secret %q (it is now also in %q; the credential still refreshes, but the "+
			"master's copy is still readable by a passthrough runner): %w",
			masterName, refresh.MaterialSecretName(masterName), err)
	}
	return true, nil
}
