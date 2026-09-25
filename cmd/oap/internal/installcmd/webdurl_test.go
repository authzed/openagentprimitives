package installcmd

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/kube"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/publicendpoint"
	"github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/cloud"
)

// TestPublicEndpointLocalURLMatchesTheConfigMapSeedByteForByte is the reason
// webdLocalSeedURL is a constant. The controller publishes spec.localURL into
// the very ConfigMap install seeded moments earlier; if the two strings differ
// by so much as a trailing slash, the controller's first apply is an update
// rather than a no-op, bumping the resourceVersion of an object webd polls.
func TestPublicEndpointLocalURLMatchesTheConfigMapSeedByteForByte(t *testing.T) {
	ctx := context.Background()
	c := fake.NewClientBuilder().WithScheme(kube.Scheme).Build()

	// managedExternal=false is the tunnel-kind shape: no --trusted-hostname, so
	// the seed is the genuinely-correct loopback value rather than empty.
	require.NoError(t, ensureWebdExternalURLConfigMap(ctx, logTo(&bytes.Buffer{}), c, false))
	require.NoError(t, publicendpoint.EnsureWebd(ctx, &bytes.Buffer{}, c, cloud.MustFor(cloud.KeyLocal), webdLocalSeedURL, nil))

	var cm corev1.ConfigMap
	require.NoError(t, c.Get(ctx, types.NamespacedName{
		Namespace: cloud.WebdServiceNamespace, Name: v1alpha1.WebdExternalURLConfigMap,
	}, &cm))
	var pe v1alpha1.PublicEndpoint
	require.NoError(t, c.Get(ctx, types.NamespacedName{Name: publicendpoint.WebdName}, &pe))

	assert.Equal(t, cm.Data[v1alpha1.WebdTrustedURLKey], pe.Spec.LocalURL,
		"the controller's first write must be byte-identical to what install seeded")
	assert.Equal(t, cm.Data[v1alpha1.WebdSandboxURLKey], pe.Spec.LocalURL,
		"both keys, since the controller writes both")
}

// TestWebdExternalURLHasAnotherOwner enumerates who owns the external-URL
// ConfigMap, because the answer decides whether install creates a
// PublicEndpoint at all. A tunnel endpoint on a cluster whose real ingress
// install just configured hands the Gateway's two keys to a controller that
// rewrites them, under ForceOwnership, to a loopback address.
func TestWebdExternalURLHasAnotherOwner(t *testing.T) {
	cases := []struct {
		name string
		opts WebdRoutingOpts
		want bool
	}{
		{
			name: "no routing flags: unclaimed, so a PublicEndpoint may own it",
			opts: WebdRoutingOpts{},
			want: false,
		},
		{
			name: "--trusted-hostname: install patches the real https hosts itself",
			opts: WebdRoutingOpts{trustedHostname: "webd.example.com", sandboxHostname: "sandbox.example.com"},
			want: true,
		},
		{
			name: "--manual-webd-routing: install promised the operator owns it",
			opts: WebdRoutingOpts{manualWebdRouting: true},
			want: true,
		},
		{
			name: "--disable-artifact-viewer alone claims nothing",
			opts: WebdRoutingOpts{disableViewer: true},
			want: false,
		},
		{
			name: "the unattended desktop install claims nothing",
			opts: UnattendedRoutingOpts(),
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, webdExternalURLHasAnotherOwner(tc.opts))
		})
	}
}

// TestInstallCreatesWebdPublicEndpoint crosses the two inputs of the decision
// install actually makes. The row that matters most is local + a trusted
// hostname: a supported install (local installs Envoy Gateway for it) where
// gating on the cluster kind alone would create an endpoint whose controller
// then rewrites the Gateway's own https:// URLs to a loopback address on every
// reconcile, under ForceOwnership.
func TestInstallCreatesWebdPublicEndpoint(t *testing.T) {
	withHostname := WebdRoutingOpts{trustedHostname: "webd.example.com", sandboxHostname: "sandbox.example.com"}
	manual := WebdRoutingOpts{manualWebdRouting: true}

	cases := []struct {
		name string
		opts WebdRoutingOpts
		kind string
		want bool
	}{
		{
			name: "local, no routing flags: install creates the endpoint",
			kind: cloud.KeyLocal, opts: WebdRoutingOpts{}, want: true,
		},
		{
			name: "local + --trusted-hostname: the Gateway owns the URL, so no endpoint",
			kind: cloud.KeyLocal, opts: withHostname, want: false,
		},
		{
			name: "local + --manual-webd-routing: the operator owns the URL, so no endpoint",
			kind: cloud.KeyLocal, opts: manual, want: false,
		},
		{
			name: "desktop, no routing flags: OnDemand, so install creates nothing",
			kind: cloud.KeyDesktop, opts: WebdRoutingOpts{}, want: false,
		},
		{
			name: "gke, no routing flags: real ingress, so install creates nothing",
			kind: cloud.KeyGKE, opts: WebdRoutingOpts{}, want: false,
		},
		{
			name: "default, no routing flags: real ingress, so install creates nothing",
			kind: cloud.KeyDefault, opts: WebdRoutingOpts{}, want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, installCreatesWebdPublicEndpoint(tc.opts, cloud.MustFor(tc.kind)))
		})
	}
}

// TestWebdURLClaimFlag names which flag claimed the URL, because the remedy in
// the refusal differs by flag: --trusted-hostname is dropped to keep the
// tunnel, while --manual-webd-routing means the operator meant to own the keys
// themselves. The empty answer belongs to the installs that never reach the
// refusal at all.
func TestWebdURLClaimFlag(t *testing.T) {
	cases := []struct {
		name string
		opts WebdRoutingOpts
		want string
	}{
		{
			name: "--trusted-hostname: install patches the real https hosts itself",
			opts: WebdRoutingOpts{trustedHostname: "webd.demo.test", sandboxHostname: "sandbox.demo.test"},
			want: "--trusted-hostname",
		},
		{
			name: "--manual-webd-routing: install promised the operator owns the keys",
			opts: WebdRoutingOpts{manualWebdRouting: true},
			want: "--manual-webd-routing",
		},
		{
			name: "both: the hostname is what install actually acts on, so it is what is named",
			opts: WebdRoutingOpts{trustedHostname: "webd.demo.test", manualWebdRouting: true},
			want: "--trusted-hostname",
		},
		{
			name: "no routing flags: nothing claims it, and this install never asks",
			opts: WebdRoutingOpts{},
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, webdURLClaimFlag(tc.opts))
			// The two answers are one decision: anything that names a flag must
			// also report the URL as claimed, or the refusal is unreachable for
			// a case that needs it.
			assert.Equal(t, tc.want != "", webdExternalURLHasAnotherOwner(tc.opts),
				"a flag worth naming is a flag that claims the URL")
		})
	}
}

// TestReInstallWithATrustedHostnameIsRefusedWhileAnEndpointOwnsTheURL walks the
// exact two-install sequence, in order, over one cluster: install once with no
// routing flags (which creates the endpoint), then install again with
// --trusted-hostname.
//
// The second run used to succeed. installCreatesWebdPublicEndpoint answered
// false — correctly, its own question being "do I create one" — and nothing
// asked whether one was already there, so install provisioned a Gateway, wrote
// the real hostnames, reported success, and had them re-applied over by the
// first run's controller on its next reconcile.
func TestReInstallWithATrustedHostnameIsRefusedWhileAnEndpointOwnsTheURL(t *testing.T) {
	ctx := context.Background()
	c := fake.NewClientBuilder().WithScheme(kube.Scheme).Build()

	// Install #1: a local cluster with no routing flags.
	first := WebdRoutingOpts{}
	require.True(t, installCreatesWebdPublicEndpoint(first, cloud.MustFor(cloud.KeyLocal)),
		"precondition: the first install is the one that creates the endpoint")
	require.NoError(t, publicendpoint.EnsureWebd(ctx, &bytes.Buffer{}, c,
		cloud.MustFor(cloud.KeyLocal), webdLocalSeedURL, nil))

	// Install #2: the same cluster, now with a real hostname.
	second := WebdRoutingOpts{trustedHostname: "webd.demo.test", sandboxHostname: "sandbox.demo.test"}
	require.False(t, installCreatesWebdPublicEndpoint(second, cloud.MustFor(cloud.KeyLocal)),
		"the creation decision stands down — and that is precisely what it cannot see past")
	require.True(t, webdExternalURLHasAnotherOwner(second), "so the second question is the one that runs")

	err := publicendpoint.RefuseWebdEndpointWhenURLIsClaimed(ctx, &bytes.Buffer{}, c, webdURLClaimFlag(second))

	require.Error(t, err, "an install that would be silently overwritten must not report success")
	assert.Contains(t, err.Error(), publicendpoint.WebdName)
	assert.Contains(t, err.Error(), "--trusted-hostname")
}

// TestInstallAsksWhoOwnsTheWebdURLBeforeProvisioningExternalAccess guards the
// wiring of the refusal above, which runInstall makes inline and which no unit
// test can reach without a cluster, a Gateway controller and a load balancer.
//
// TWO PROPERTIES, and the ordering one is the load-bearing half: refusing after
// the Gateway and certificate are provisioned would leave that work half-done
// on a cluster the operator was told to fix by hand. Same shape and same reason
// as internal/cmd/webd's sharedOriginOK wiring guard.
func TestInstallAsksWhoOwnsTheWebdURLBeforeProvisioningExternalAccess(t *testing.T) {
	b, err := os.ReadFile("install.go")
	require.NoError(t, err, "read cmd/oap/internal/installcmd/install.go")
	src := string(b)

	refusal := strings.Index(src, "publicendpoint.RefuseWebdEndpointWhenURLIsClaimed(")
	require.NotEqual(t, -1, refusal,
		"runInstall must ask whether an existing PublicEndpoint owns webd's URL; "+
			"installCreatesWebdPublicEndpoint answers a different question and cannot see one that already exists")

	external := strings.Index(src, "setupWebdExternalAccess(ctx, recheckCtx")
	require.NotEqual(t, -1, external, "the external-access call site must still be findable for this guard to mean anything")

	assert.Less(t, refusal, external,
		"the refusal must run BEFORE external access: refusing afterwards leaves a Gateway and a certificate "+
			"provisioned for an install that then failed")
}
