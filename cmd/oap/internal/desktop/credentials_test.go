package desktop_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/desktop"
	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/chatgpt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestCredentialRotationKeepsAccountsIsolatedAndClearsSignedOutSessions(t *testing.T) {
	ctx := context.Background()
	support := t.TempDir()
	dir := filepath.Join(support, "chatgpt")
	require.NoError(t, os.MkdirAll(dir, 0700))
	account := chatgpt.Account{ClientID: "oaiapp_one", Subject: "one", AccessToken: "new-access", RefreshToken: "refresh-stays-on-host", Scope: "chatgpt.tokens.use.direct", ExpiresAt: time.Now().Add(time.Hour)}
	writeAccount := func(a chatgpt.Account) {
		b, e := json.Marshal(map[string]any{"host_id": "urn:uuid:test", "accounts": map[string]chatgpt.Account{a.ClientID: a}})
		require.NoError(t, e)
		require.NoError(t, os.WriteFile(filepath.Join(dir, "accounts.json"), b, 0600))
	}
	writeAccount(account)
	central := desktop.AccountTokenSecretName(account.ClientID)
	source := &v1.NamespacedSecretKeyRef{Namespace: "agentprimitives-system", Name: central, Key: "token"}
	// A live session's settings may already reflect a new default. The durable
	// credential-source annotation, not the mutable default, owns its account.
	sess := &v1.AgentSession{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "session"}}
	sess.Status.EffectiveSettings = &v1.EffectiveSettings{}
	sess.Status.EffectiveSettings.ModelTokenSource = &v1.NamespacedSecretKeyRef{Namespace: "agentprimitives-system", Name: "new-default", Key: "token"}
	sess.Status.EffectiveSettings.Model.APIKey = v1.SecretKeyRef{Name: "session-secret", Key: "llm-api-key"}
	own := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "session-secret", Annotations: map[string]string{v1.ModelTokenSourceAnnotation: source.Namespace + "/" + source.Name + "/token"}}, Data: map[string][]byte{"llm-api-key": []byte("old-access"), "memory-token": []byte("unchanged")}}
	other := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "other-secret"}, Data: map[string][]byte{"llm-api-key": []byte("other-account")}}
	centralSecret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: source.Namespace, Name: central}, Data: map[string][]byte{"token": []byte("old-access")}}
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, v1.AddToScheme(scheme))
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(sess, own, other, centralSecret).Build()
	require.NoError(t, desktop.SyncAccountTokens(ctx, support, c))
	require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(own), own))
	assert.Equal(t, "new-access", string(own.Data["llm-api-key"]))
	assert.Equal(t, "unchanged", string(own.Data["memory-token"]))
	require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(other), other))
	assert.Equal(t, "other-account", string(other.Data["llm-api-key"]))
	assert.NotContains(t, string(own.Data["llm-api-key"]), "refresh-stays-on-host")
	account.AccessToken = ""
	account.RefreshToken = ""
	writeAccount(account)
	require.NoError(t, desktop.SyncAccountTokens(ctx, support, c))
	require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(own), own))
	assert.Empty(t, own.Data["llm-api-key"])
}
