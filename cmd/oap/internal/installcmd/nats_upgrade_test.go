package installcmd

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/kube"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	apnats "github.com/authzed/openagentprimitives/pkg/platform/nats"
	"github.com/authzed/openagentprimitives/pkg/platform/nats/natstest"
)

const natsSystemNS = "agentprimitives-system"

// seededCluster returns a fake cluster with the NATS identity and TLS Secrets
// already written — the state ensureNATSClientCredsWithClient requires.
func seededCluster(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	c := fake.NewClientBuilder().WithScheme(kube.Scheme).WithObjects(objs...).Build()
	ctx := context.Background()
	require.NoError(t, ensureNATSIdentityWithClient(ctx, c), "seed identity")
	require.NoError(t, ensureNATSTLSWithClient(ctx, c), "seed tls")
	return c
}

// storedIdentity reads back the account keys ensureNATSIdentityWithClient
// wrote, so a test can mint an "older release's" credential signed by the same
// account the installer will compare against.
func storedIdentity(t *testing.T, c client.Client) *apnats.Identity {
	t.Helper()
	var sec corev1.Secret
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{
		Namespace: natsSystemNS, Name: "spicebox-nats-identity",
	}, &sec), "read the seeded identity Secret")
	return &apnats.Identity{
		AccountPublicKey:   string(sec.Data["account-public-key"]),
		AccountSigningSeed: sec.Data["account-signing-seed"],
	}
}

// currentNATSCA is the CA ensureNATSTLSWithClient just wrote — what a healthy
// creds Secret carries, so a test can isolate grant drift from CA drift.
func currentNATSCA(t *testing.T, c client.Client) []byte {
	t.Helper()
	var tls corev1.Secret
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{
		Namespace: natsSystemNS, Name: "spicebox-nats-tls",
	}, &tls), "read the seeded TLS Secret")
	require.NotEmpty(t, tls.Data["ca.crt"])
	return tls.Data["ca.crt"]
}

func natsCreds(t *testing.T, c client.Client, name string) string {
	t.Helper()
	var sec corev1.Secret
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{
		Namespace: natsSystemNS, Name: name,
	}, &sec), "read %s", name)
	return string(sec.Data["nats.creds"])
}

// webdGrantBeforeWebhookInbound is webd's grant as an OLDER release shipped
// it: everything the current one has, minus the webhook-inbound publish.
// Derived by subtraction from the live grant rather than transcribed, so it
// stays a faithful "one release behind" as the real grant grows.
func webdGrantBeforeWebhookInbound() apnats.UserGrant {
	old := apnats.UserGrant{Name: webdNATSGrant.Name, SubAllow: webdNATSGrant.SubAllow}
	for _, s := range webdNATSGrant.PubAllow {
		if s == channelevents.WebhookInboundSubject {
			continue
		}
		old.PubAllow = append(old.PubAllow, s)
	}
	return old
}

// webdDeployment is a stand-in for the shipped webd Deployment: what matters
// here is only that its pod template mounts the creds Secret, which is how the
// installer decides who to roll.
func webdDeployment(mountedSecret string) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: natsSystemNS, Name: "spicebox-webd"},
		Spec: appsv1.DeploymentSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "webd", Image: "webd:test"}},
					Volumes: []corev1.Volume{{
						Name:         "nats-creds",
						VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: mountedSecret}},
					}},
				},
			},
		},
	}
}

// unrelatedDeployment mounts nothing of interest — the control proving the
// roll is targeted rather than "restart everything in the namespace".
func unrelatedDeployment() *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: natsSystemNS, Name: "spicebox-authzd"},
		Spec: appsv1.DeploymentSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "authzd", Image: "authzd:test"}}},
			},
		},
	}
}

func restartedAt(t *testing.T, c client.Client, name string) string {
	t.Helper()
	var d appsv1.Deployment
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: natsSystemNS, Name: name}, &d))
	return d.Spec.Template.Annotations["ap.authzed.com/restartedAt"]
}

// TestEnsureNATSClientCreds_ReMintsAGrantThatGainedASubject is the upgrade
// this blocker is about.
//
// A cluster installed before ap.channel.webhook_inbound joined webdNATSGrant
// carries a webd JWT without it. `oap install` used to see the Secret, return
// early, and leave the stale JWT in place forever — and the resulting failure
// is invisible: nats-server reports a publish permission violation
// asynchronously, so channelwebhook's Publish returns nil and the handler
// answers 202 Accepted for a delivery that was dropped.
//
// The assertion is a server-side permission DECISION, not a substring of the
// JWT: the grant read back out of the re-minted credential is put through a
// real nats-server, which is the only thing that actually decides whether the
// publish lands.
func TestEnsureNATSClientCreds_ReMintsAGrantThatGainedASubject(t *testing.T) {
	ctx := context.Background()
	dep := webdDeployment("spicebox-webd-nats-creds")
	c := seededCluster(t, dep, unrelatedDeployment())

	// Stand up the "installed one release ago" state: a real, correctly signed
	// webd credential that simply predates the webhook-inbound subject.
	//
	// The CA is the CURRENT one on purpose. Seeding a stale CA alongside would
	// re-mint via the CA branch and the grant assertion below would pass
	// without the grant comparison ever running — the test would prove
	// nothing. The obsolete grant must be the only thing wrong here.
	stale, err := apnats.MintUser(storedIdentity(t, c), webdGrantBeforeWebhookInbound())
	require.NoError(t, err, "mint the pre-upgrade webd creds")
	require.NoError(t, c.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: natsSystemNS, Name: "spicebox-webd-nats-creds"},
		Type:       corev1.SecretTypeOpaque,
		Data:       map[string][]byte{"nats.creds": []byte(stale), "ca.crt": currentNATSCA(t, c)},
	}), "seed the pre-upgrade webd creds Secret")

	h := natstest.New(t)
	staleGrant, err := apnats.GrantFromCreds(stale)
	require.NoError(t, err)
	require.False(t, h.PublishAllowed(t, staleGrant, channelevents.WebhookInboundSubject),
		"precondition: the pre-upgrade credential must NOT permit the webhook-inbound publish, or this test proves nothing")

	var narrated []string
	require.NoError(t, ensureNATSClientCredsWithClient(ctx, c, func(f string, a ...any) {
		narrated = append(narrated, fmt.Sprintf(f, a...))
	}), "the upgrade run must succeed")

	freshGrant, err := apnats.GrantFromCreds(natsCreds(t, c, "spicebox-webd-nats-creds"))
	require.NoError(t, err, "the re-minted creds must be readable")
	assert.True(t, h.PublishAllowed(t, freshGrant, channelevents.WebhookInboundSubject),
		"after the upgrade the stored credential must permit the webhook-inbound publish, "+
			"or every GitHub delivery is silently dropped behind a 202")

	// The rotation must be reported: nothing else in the system ever would.
	var sawWhy bool
	for _, line := range narrated {
		if strings.Contains(line, "re-minting spicebox-webd-nats-creds") &&
			strings.Contains(line, channelevents.WebhookInboundSubject) {
			sawWhy = true
		}
	}
	assert.Truef(t, sawWhy, "the run must say what it rotated and why; got %v", narrated)

	// And the rotation must actually reach the process holding the old JWT.
	assert.NotEmpty(t, restartedAt(t, c, "spicebox-webd"),
		"a running webd read its creds file at connect time; without a roll the stale JWT stays on the wire")
	assert.Empty(t, restartedAt(t, c, "spicebox-authzd"),
		"only workloads that mount the rotated Secret may be rolled")
}

// TestEnsureNATSClientCreds_LeavesAnUpToDateCredentialAlone is the other half:
// re-minting is non-deterministic (a fresh user nkey every call), so an
// unconditional write would rotate every principal and disconnect live clients
// on every re-run.
func TestEnsureNATSClientCreds_LeavesAnUpToDateCredentialAlone(t *testing.T) {
	ctx := context.Background()
	c := seededCluster(t, webdDeployment("spicebox-webd-nats-creds"))

	require.NoError(t, ensureNATSClientCredsWithClient(ctx, c, nil), "first install")
	before := natsCreds(t, c, "spicebox-webd-nats-creds")
	require.NotEmpty(t, before)

	var narrated []string
	require.NoError(t, ensureNATSClientCredsWithClient(ctx, c, func(f string, a ...any) {
		narrated = append(narrated, fmt.Sprintf(f, a...))
	}), "second install")

	assert.Equal(t, before, natsCreds(t, c, "spicebox-webd-nats-creds"),
		"a credential already carrying the shipped grant must not be rotated")
	assert.Empty(t, narrated, "a healthy re-run has nothing to report")
	assert.Empty(t, restartedAt(t, c, "spicebox-webd"),
		"a re-run that rotates nothing must not restart anything")
}

// TestEnsureNATSClientCreds_RefreshesAStaleCA covers the other stale half of
// the same Secret: the creds file is fine but the CA no longer matches the
// server's, which fails the TLS handshake rather than a permission check.
func TestEnsureNATSClientCreds_RefreshesAStaleCA(t *testing.T) {
	ctx := context.Background()
	c := seededCluster(t, webdDeployment("spicebox-cli-nats-creds"))

	require.NoError(t, ensureNATSClientCredsWithClient(ctx, c, nil), "first install")
	creds := natsCreds(t, c, "spicebox-cli-nats-creds")

	var sec corev1.Secret
	key := types.NamespacedName{Namespace: natsSystemNS, Name: "spicebox-cli-nats-creds"}
	require.NoError(t, c.Get(ctx, key, &sec))
	sec.Data["ca.crt"] = []byte("a CA from a previous NATS install")
	require.NoError(t, c.Update(ctx, &sec))

	require.NoError(t, ensureNATSClientCredsWithClient(ctx, c, nil), "second install")

	require.NoError(t, c.Get(ctx, key, &sec))
	var tls corev1.Secret
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: natsSystemNS, Name: "spicebox-nats-tls"}, &tls))
	assert.Equal(t, tls.Data["ca.crt"], sec.Data["ca.crt"],
		"a creds Secret carrying a CA the server no longer presents cannot connect at all")
	assert.NotEqual(t, creds, natsCreds(t, c, "spicebox-cli-nats-creds"),
		"the credential is re-minted on the same write, so the whole Secret is consistent")
	assert.NotEmpty(t, restartedAt(t, c, "spicebox-webd"),
		"the Deployment mounting the rotated Secret must be rolled")
}
