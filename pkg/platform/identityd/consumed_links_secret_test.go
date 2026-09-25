package identityd

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

func consumedLinksSecret(t *testing.T, c client.Client) *corev1.Secret {
	t.Helper()
	var sec corev1.Secret
	err := c.Get(context.Background(), client.ObjectKey{
		Namespace: spiceboxv1alpha1.IdentitiesNamespace,
		Name:      consumedLinksSecretName,
	}, &sec)
	require.NoError(t, err, "the consumed-links Secret must exist")
	return &sec
}

// TestSecretConsumedLinkBackend_RecordsAndRejectsReplay covers the durable half
// end to end against a client: the first call creates the Secret and reports
// first, the second reads it back and reports not-first. The second call is the
// one that only works because the record is outside the process.
func TestSecretConsumedLinkBackend_RecordsAndRejectsReplay(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).Build()
	b := newSecretConsumedLinkBackend(c)
	now := time.Now()

	first, err := b.markConsumed(context.Background(), "digest-a", now, time.Hour)
	require.NoError(t, err)
	assert.True(t, first, "the first call records the digest")

	first, err = b.markConsumed(context.Background(), "digest-a", now, time.Hour)
	require.NoError(t, err)
	assert.False(t, first, "a replay of the same digest must not be reported as first")

	sec := consumedLinksSecret(t, c)
	assert.Contains(t, sec.Data, "digest-a")
}

// TestSecretConsumedLinkBackend_DistinctDigestsCoexist: recording one link must
// not consume another.
func TestSecretConsumedLinkBackend_DistinctDigestsCoexist(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).Build()
	b := newSecretConsumedLinkBackend(c)
	now := time.Now()

	for _, d := range []string{"digest-a", "digest-b"} {
		first, err := b.markConsumed(context.Background(), d, now, time.Hour)
		require.NoError(t, err)
		assert.True(t, first, "%s is independently first", d)
	}
	sec := consumedLinksSecret(t, c)
	assert.Len(t, sec.Data, 2)
}

// TestSecretConsumedLinkBackend_PrunesExpired keeps the object bounded: an entry
// older than ttl is dropped, which both frees the digest for reuse (matching the
// link's own expiry) and stops the Secret growing without limit. Without this
// there is no sweeper anywhere.
func TestSecretConsumedLinkBackend_PrunesExpired(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).Build()
	b := newSecretConsumedLinkBackend(c)
	start := time.Now()

	first, err := b.markConsumed(context.Background(), "old", start, time.Hour)
	require.NoError(t, err)
	require.True(t, first)

	// Two hours later, with a one-hour ttl, the entry is past its window.
	later := start.Add(2 * time.Hour)
	first, err = b.markConsumed(context.Background(), "new", later, time.Hour)
	require.NoError(t, err)
	assert.True(t, first)

	sec := consumedLinksSecret(t, c)
	assert.NotContains(t, sec.Data, "old", "an expired digest must be pruned")
	assert.Contains(t, sec.Data, "new")
}

// TestPruneExpired_TreatsUnparseableAsExpired: a corrupt timestamp must not pin
// an entry in the Secret forever, since nothing else would ever remove it.
func TestPruneExpired_TreatsUnparseableAsExpired(t *testing.T) {
	now := time.Now()
	data := map[string][]byte{
		"good":    []byte(now.Format(time.RFC3339)),
		"corrupt": []byte("not-a-timestamp"),
	}

	removed := pruneExpired(data, now, time.Hour)

	assert.Equal(t, 1, removed)
	assert.Contains(t, data, "good")
	assert.NotContains(t, data, "corrupt")
}

// TestSecretConsumedLinkBackend_ReplayDoesNotWrite guards against a spent link
// being turned into an unbounded apiserver write generator: once nothing is
// left to prune, replaying a consumed digest must be read-only.
func TestSecretConsumedLinkBackend_ReplayDoesNotWrite(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).Build()
	b := newSecretConsumedLinkBackend(c)
	now := time.Now()

	_, err := b.markConsumed(context.Background(), "digest-a", now, time.Hour)
	require.NoError(t, err)
	before := consumedLinksSecret(t, c).ResourceVersion

	for i := 0; i < 3; i++ {
		first, rerr := b.markConsumed(context.Background(), "digest-a", now, time.Hour)
		require.NoError(t, rerr)
		require.False(t, first)
	}

	assert.Equal(t, before, consumedLinksSecret(t, c).ResourceVersion,
		"replaying a spent link with nothing to prune must not write")
}
