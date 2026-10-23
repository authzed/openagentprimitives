//go:build integration

package manifests_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/testenv"
	"github.com/authzed/openagentprimitives/pkg/platform/manifests"
)

const (
	operatorSAUser  = "system:serviceaccount:agentprimitives-system:spicebox-operator"
	channelsdSAUser = "system:serviceaccount:agentprimitives-system:spicebox-channelsd"
	untrustedUser   = "alice@example.com"
)

// impersonatingClient returns a client acting as username. The system:masters
// group spares the test an RBAC fixture per user: RBAC is not what is under
// test, and admission policies apply to masters exactly as to anyone else —
// which is the property this test relies on.
func impersonatingClient(t *testing.T, env *testenv.Env, username string) client.Client {
	t.Helper()
	cfg := rest.CopyConfig(env.Cfg)
	cfg.Impersonate = rest.ImpersonationConfig{UserName: username, Groups: []string{"system:masters", "system:authenticated"}}
	c, err := client.New(cfg, client.Options{Scheme: env.Scheme})
	require.NoError(t, err, "build impersonating client for %s", username)
	return c
}

func finalizerTestSession(name string, withFinalizer bool) *spiceboxv1alpha1.AgentSession {
	s := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentSessionSpec{
			Class:  "demo-agent",
			Prompt: spiceboxv1alpha1.PromptSource{Inline: "hello"},
		},
	}
	if withFinalizer {
		s.Finalizers = []string{spiceboxv1alpha1.FinalizerAgentSession}
	}
	return s
}

// installFinalizerPolicy creates the ValidatingAdmissionPolicy and binding
// exactly as the install bundle ships them, then waits for the apiserver to
// start enforcing — policy activation is eventually consistent, so a test that
// asserted immediately would race the policy's own load.
func installFinalizerPolicy(t *testing.T, ctx context.Context, env *testenv.Env) {
	t.Helper()
	docs, err := manifests.Split(manifests.Install)
	require.NoError(t, err, "split install bundle")
	installed := 0
	for _, d := range docs {
		switch d.GetKind() {
		case "ValidatingAdmissionPolicy", "ValidatingAdmissionPolicyBinding":
		default:
			continue
		}
		if d.GetName() != "agentsession-finalizer.agentprimitives.authzed.com" {
			continue
		}
		require.NoError(t, env.Client.Create(ctx, d), "create %s %s", d.GetKind(), d.GetName())
		installed++
	}
	require.Equal(t, 2, installed, "the bundle must ship the finalizer policy AND its binding")

	untrusted := impersonatingClient(t, env, untrustedUser)
	probe := finalizerTestSession("policy-activation-probe", true)
	deadline := time.Now().Add(30 * time.Second)
	for {
		// Dry-run: admission runs in full but nothing is persisted, so a probe
		// that lands before the policy is active leaves nothing behind.
		err := untrusted.Create(ctx, probe.DeepCopy(), client.DryRunAll)
		if apierrors.IsForbidden(err) {
			return
		}
		require.Truef(t, err == nil, "unexpected error while waiting for the policy to activate: %v", err)
		require.Truef(t, time.Now().Before(deadline), "the finalizer policy did not start enforcing within 30s")
		time.Sleep(200 * time.Millisecond)
	}
}

// TestAgentSessionFinalizerPolicy proves the shipped admission policy makes
// the operator's admission-sweep skip unforgeable: only the operator and
// channelsd service accounts may add the AgentSession finalizer, and writes
// that do not add it are untouched.
func TestAgentSessionFinalizerPolicy(t *testing.T) {
	env := testenv.Start(t)
	ctx := context.Background()
	installFinalizerPolicy(t, ctx, env)

	untrusted := impersonatingClient(t, env, untrustedUser)

	t.Run("untrusted CREATE with the finalizer: refused as Forbidden", func(t *testing.T) {
		err := untrusted.Create(ctx, finalizerTestSession("untrusted-prestamped", true))
		require.Error(t, err)
		assert.True(t, apierrors.IsForbidden(err), "expected Forbidden, got: %v", err)
		assert.Contains(t, err.Error(), "only the agentprimitives operator and channelsd may add")
	})

	t.Run("untrusted CREATE without the finalizer: allowed", func(t *testing.T) {
		assert.NoError(t, untrusted.Create(ctx, finalizerTestSession("untrusted-plain", false)))
	})

	for _, tc := range []struct {
		name, user string
	}{
		{name: "channelsd CREATE with the finalizer: allowed", user: channelsdSAUser},
		{name: "operator CREATE with the finalizer: allowed", user: operatorSAUser},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := impersonatingClient(t, env, tc.user)
			assert.NoError(t, c.Create(ctx, finalizerTestSession(fmt.Sprintf("trusted-%d", time.Now().UnixNano()), true)))
		})
	}

	t.Run("untrusted UPDATE adding the finalizer: refused; operator UPDATE adding it: allowed", func(t *testing.T) {
		sess := finalizerTestSession("update-adds", false)
		require.NoError(t, untrusted.Create(ctx, sess), "create without the finalizer")

		var fresh spiceboxv1alpha1.AgentSession
		require.NoError(t, untrusted.Get(ctx, client.ObjectKeyFromObject(sess), &fresh))
		fresh.Finalizers = append(fresh.Finalizers, spiceboxv1alpha1.FinalizerAgentSession)
		err := untrusted.Update(ctx, fresh.DeepCopy())
		require.Error(t, err, "an untrusted update adding the finalizer must be refused")
		assert.True(t, apierrors.IsForbidden(err), "expected Forbidden, got: %v", err)

		// The operator's EnsureFinalizer path: same write, trusted caller.
		operator := impersonatingClient(t, env, operatorSAUser)
		assert.NoError(t, operator.Update(ctx, fresh.DeepCopy()), "the operator adds the finalizer after its sweep")
	})

	t.Run("untrusted UPDATEs that keep or remove the finalizer: allowed", func(t *testing.T) {
		channelsd := impersonatingClient(t, env, channelsdSAUser)
		sess := finalizerTestSession("update-keeps", true)
		require.NoError(t, channelsd.Create(ctx, sess), "channelsd mint with the finalizer pre-stamped")

		var fresh spiceboxv1alpha1.AgentSession
		require.NoError(t, untrusted.Get(ctx, client.ObjectKeyFromObject(sess), &fresh))
		if fresh.Labels == nil {
			fresh.Labels = map[string]string{}
		}
		fresh.Labels["touched"] = "yes"
		require.NoError(t, untrusted.Update(ctx, &fresh), "an unrelated update leaving the finalizer in place must not be evaluated")

		fresh.Finalizers = nil
		assert.NoError(t, untrusted.Update(ctx, &fresh), "removing the finalizer is not adding it")
	})
}
