package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/authzed/openagentprimitives/pkg/authz/adoptguard"
	"github.com/authzed/openagentprimitives/pkg/memory/provenance"
	"github.com/authzed/openagentprimitives/pkg/memory/publisherkeys"
)

// cmReader wraps a client as a guarded ConfigMapReader for these tests. The
// publisher-keys ConfigMap is allowlisted infra in production; here a permissive
// allowlist routes the read straight through to the underlying client so its
// NotFound / injected errors propagate exactly as installComponentPublisherKeys expects.
func cmReader(c client.Client) *adoptguard.ConfigMapReader {
	return adoptguard.NewConfigMapReader(c, c, adoptguard.Warn, func(types.NamespacedName) bool { return true })
}

func pubKeysScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(s))
	return s
}

// mintKeyEntry generates a real Ed25519 key and returns its content-address
// keyID plus the ConfigMap JSON for a single-entry publisher-keys map.
// Generating (rather than hardcoding) keeps the fixture valid under the
// content-addressing invariant the registry now enforces on load.
func mintKeyEntry(t *testing.T, publisher string) (keyID, keysJSON string) {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	keyID = provenance.KeyID(pub)
	body, err := json.Marshal([]publisherkeys.Entry{{
		Publisher: publisher, KeyID: keyID,
		PubKeyB64: base64.StdEncoding.EncodeToString(pub),
	}})
	require.NoError(t, err)
	return keyID, string(body)
}

func TestInstallComponentPublisherKeys_LoadsPersistedKeys(t *testing.T) {
	keyID, keysJSON := mintKeyEntry(t, "system:channelsd")
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: publisherKeysConfigMap, Namespace: "agentprimitives-system"},
		Data:       map[string]string{publisherKeysConfigMapField: keysJSON},
	}
	c := fake.NewClientBuilder().WithScheme(pubKeysScheme(t)).WithObjects(cm).Build()
	reg := publisherkeys.New()

	n, err := installComponentPublisherKeys(context.Background(), cmReader(c), "agentprimitives-system", reg, logr.Discard())
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	_, ok := reg.PublisherKey("system:channelsd", keyID)
	assert.True(t, ok, "the persisted channelsd key must be installed so verify-on-write accepts its appends")
}

func TestInstallComponentPublisherKeys_AbsentConfigMapIsNotAnError(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(pubKeysScheme(t)).Build()
	reg := publisherkeys.New()

	n, err := installComponentPublisherKeys(context.Background(), cmReader(c), "agentprimitives-system", reg, logr.Discard())
	require.NoError(t, err, "an absent ConfigMap means no components have registered yet — not a failure")
	assert.Equal(t, 0, n)
}

// TestInstallComponentPublisherKeys_ReadErrorPropagates is the regression for
// the outage: the operator read the publisher-keys ConfigMap with a cached
// client before the manager cache had started ("the cache is not started"),
// then SWALLOWED the error and ran with an empty registry — so verify-on-write
// 403'd every component append-only write ("Internal error processing your
// message"). The load must propagate the error so main fails closed (crash +
// restart), never continue degraded.
func TestInstallComponentPublisherKeys_ReadErrorPropagates(t *testing.T) {
	boom := errors.New("the cache is not started, can not read objects")
	c := interceptor.NewClient(
		fake.NewClientBuilder().WithScheme(pubKeysScheme(t)).Build(),
		interceptor.Funcs{
			Get: func(_ context.Context, _ client.WithWatch, _ client.ObjectKey, _ client.Object, _ ...client.GetOption) error {
				return boom
			},
		},
	)
	reg := publisherkeys.New()

	n, err := installComponentPublisherKeys(context.Background(), cmReader(c), "agentprimitives-system", reg, logr.Discard())
	require.Error(t, err, "a read failure must propagate — never swallowed into an empty registry")
	assert.ErrorIs(t, err, boom)
	assert.Equal(t, 0, n)
}
