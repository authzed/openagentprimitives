package identityd

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// consumedLinksSecretName is the single object holding every not-yet-expired
// link digest. One object rather than one per link: the set is small (bounded
// by the link TTL times the click rate), and a single object makes the
// record-if-absent step one atomic update instead of a create-and-race.
const consumedLinksSecretName = "spicebox-consumed-links"

// secretConsumedLinkBackend persists consumed-link digests in a Secret in
// IdentitiesNamespace.
//
// A Secret, not a ConfigMap: webd already holds get/create/update/delete on
// Secrets in exactly this namespace (config/webd/role.yaml), so this needs no
// RBAC change, and digests of bearer-shaped credential links belong with the
// credential-link material rather than in a world-readable-by-default
// ConfigMap — even though a digest is not itself a credential.
type secretConsumedLinkBackend struct {
	k8s client.Client
}

func newSecretConsumedLinkBackend(k8s client.Client) *secretConsumedLinkBackend {
	return &secretConsumedLinkBackend{k8s: k8s}
}

// markConsumed records key if absent and reports whether THIS call recorded it.
//
// Atomicity comes from the apiserver: the read-modify-write runs under
// RetryOnConflict, so two callers racing the same link serialize on
// resourceVersion and exactly one is told it was first. The whole single-use
// guarantee rests on that — never relax it into a blind overwrite.
func (b *secretConsumedLinkBackend) markConsumed(ctx context.Context, key string, at time.Time, ttl time.Duration) (bool, error) {
	first := false
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		first = false // reset per attempt; a retried attempt re-decides
		var sec corev1.Secret
		getErr := b.k8s.Get(ctx, client.ObjectKey{
			Namespace: spiceboxv1alpha1.IdentitiesNamespace,
			Name:      consumedLinksSecretName,
		}, &sec)

		switch {
		case apierrors.IsNotFound(getErr):
			// First consumed link since install. Create carries the entry, so
			// the create IS the record — no follow-up update to lose.
			sec = corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      consumedLinksSecretName,
					Namespace: spiceboxv1alpha1.IdentitiesNamespace,
				},
				Data: map[string][]byte{key: []byte(at.UTC().Format(time.RFC3339))},
			}
			if cerr := b.k8s.Create(ctx, &sec); cerr != nil {
				// A concurrent creator won. Surface as a conflict so
				// RetryOnConflict re-reads and takes the update path.
				if apierrors.IsAlreadyExists(cerr) {
					return apierrors.NewConflict(
						corev1.Resource("secrets"), consumedLinksSecretName, cerr)
				}
				return fmt.Errorf("create %s: %w", consumedLinksSecretName, cerr)
			}
			first = true
			return nil

		case getErr != nil:
			return fmt.Errorf("get %s: %w", consumedLinksSecretName, getErr)
		}

		if sec.Data == nil {
			sec.Data = map[string][]byte{}
		}
		pruned := pruneExpired(sec.Data, at, ttl)
		if _, already := sec.Data[key]; already {
			// Already consumed — settled. Write back ONLY if pruning removed
			// something: replay is the hot path, and updating on every attempt
			// would let anyone holding a spent link generate unbounded
			// apiserver writes.
			if pruned == 0 {
				return nil
			}
			return b.k8s.Update(ctx, &sec)
		}
		sec.Data[key] = []byte(at.UTC().Format(time.RFC3339))
		if uerr := b.k8s.Update(ctx, &sec); uerr != nil {
			return uerr // conflicts retry; anything else surfaces below
		}
		first = true
		return nil
	})
	if err != nil {
		return false, err
	}
	return first, nil
}

// pruneExpired drops digests older than ttl and reports how many it removed —
// what keeps the object bounded without a background sweeper. A non-positive
// ttl expires everything. An unparseable timestamp counts as expired: it cannot
// be reasoned about, and keeping it would grow the object without bound.
func pruneExpired(data map[string][]byte, now time.Time, ttl time.Duration) int {
	cutoff := now.Add(-ttl)
	removed := 0
	for k, v := range data {
		t, err := time.Parse(time.RFC3339, string(v))
		if err != nil || !t.After(cutoff) {
			delete(data, k)
			removed++
		}
	}
	return removed
}
