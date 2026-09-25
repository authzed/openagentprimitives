package installcmd

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/kube"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/progress"
	"github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/cloud"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/passthroughlink"
)

// logTo returns a logf — the func(string, ...any) shape the ensure* helpers now
// narrate through (the install reporter's rep.Info in production) — that appends
// each formatted line to buf, so a test can assert on what the operator was
// told. The ensure* helpers were changed from writing straight to an io.Writer
// to narrating via logf so they don't corrupt the checklist's live region on a
// wizard install; see cmd/oap/internal/progress.
func logTo(buf *bytes.Buffer) func(string, ...any) {
	return func(f string, a ...any) { fmt.Fprintf(buf, f+"\n", a...) }
}

func TestEnsurePassthroughLinkSigningKeyIsIdempotent(t *testing.T) {
	ctx := context.Background()
	c := fake.NewClientBuilder().WithScheme(kube.Scheme).Build()

	require.NoError(t, ensurePassthroughLinkSigningKey(ctx, logTo(&bytes.Buffer{}), c), "first call must succeed")

	var first corev1.Secret
	require.NoError(t, c.Get(ctx, types.NamespacedName{
		Namespace: "agentprimitives-system", Name: v1alpha1.PassthroughLinkSigningKeySecret,
	}, &first), "Secret must exist after first call")

	firstKey := first.Data["key"]
	require.NotEmpty(t, firstKey, "key must be populated after first call")
	assert.Len(t, firstKey, 64, "key must be 64 hex chars (32 bytes)")

	require.NoError(t, ensurePassthroughLinkSigningKey(ctx, logTo(&bytes.Buffer{}), c), "second call must succeed")

	var second corev1.Secret
	require.NoError(t, c.Get(ctx, types.NamespacedName{
		Namespace: "agentprimitives-system", Name: v1alpha1.PassthroughLinkSigningKeySecret,
	}, &second), "Secret must still exist after second call")

	assert.True(t, bytes.Equal(firstKey, second.Data["key"]),
		"re-install must not rotate the link signing key")
}

// TestEnsurePassthroughLinkSigningKeyRepairsUnusableKey covers the repair path
// the function's own doc comment promises: a Secret that already exists but
// carries no usable key must be filled in, not left alone.
//
// This is reachable in practice because the Secret is routinely provisioned by
// something other than `oap install` — External Secrets Operator, a GitOps
// apply, a hand-written manifest — which can land an existing object with an
// empty, absent, short, or non-hex "key". Leaving it is not inert: every
// production reader decodes the Secret through passthroughlink.DecodeHexKey,
// which enforces MinKeyLen and hex-ness, so anything it refuses is fatal in
// webd (readHexKey at startup) and silently disables channelsd's passthrough
// signer — while install printed "Secret exists with key, skipping" and
// reported success.
//
// "Usable" is therefore DecodeHexKey's verdict, not len > 0: a 16-byte key (32
// hex chars) or a 64-character non-hex string both satisfy len > 0 and are both
// refused by all three readers.
func TestEnsurePassthroughLinkSigningKeyRepairsUnusableKey(t *testing.T) {
	// goodKey is a well-formed 32-byte key in the exact shape install writes.
	goodKey := []byte(hex.EncodeToString(bytes.Repeat([]byte{0xab}, passthroughlink.MinKeyLen)))

	cases := []struct {
		name string
		// seeded is the Secret's Data map before the call.
		seeded map[string][]byte
		// wantPreserved, when non-nil, is the exact key the call must leave
		// untouched. nil means "the key must have been regenerated".
		wantPreserved []byte
		wantLog       string
	}{
		{
			name:    "existing Secret with empty key: repaired in place",
			seeded:  map[string][]byte{"key": []byte("")},
			wantLog: "regenerated",
		},
		{
			name:    "existing Secret with no key entry at all: repaired in place",
			seeded:  map[string][]byte{},
			wantLog: "regenerated",
		},
		{
			name:    "existing Secret with a nil Data map: repaired in place",
			seeded:  nil,
			wantLog: "regenerated",
		},
		{
			// 16 bytes — half the floor. len > 0, so the old gate skipped it and
			// install reported success; DecodeHexKey refuses it, so webd is fatal
			// at startup and channelsd disables the signer.
			name: "existing Secret with a key below MinKeyLen: repaired in place (every reader refuses it)",
			seeded: map[string][]byte{
				"key": []byte(hex.EncodeToString(bytes.Repeat([]byte{0x01}, passthroughlink.MinKeyLen/2))),
			},
			wantLog: "regenerated",
		},
		{
			// Right length, wrong alphabet — the shape a hand-written manifest or
			// a base64-instead-of-hex provisioner lands.
			name:    "existing Secret with a non-hex key of the right length: repaired in place",
			seeded:  map[string][]byte{"key": bytes.Repeat([]byte("z"), passthroughlink.MinKeyLen*2)},
			wantLog: "regenerated",
		},
		{
			// The other half of the contract: never rotate a good key. Rotating
			// invalidates every in-flight passthrough link (channelsd just minted
			// one; identityd is mid-verify).
			name:          "existing Secret with a usable key: left byte-identical (rotation invalidates in-flight links)",
			seeded:        map[string][]byte{"key": goodKey},
			wantPreserved: goodKey,
			wantLog:       "skipping",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			existing := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      v1alpha1.PassthroughLinkSigningKeySecret,
					Namespace: "agentprimitives-system",
					// A label a third-party provisioner would own; the repair
					// must not clobber the caller's other metadata.
					Labels: map[string]string{"app.kubernetes.io/managed-by": "external-provisioner"},
				},
				Type: corev1.SecretTypeOpaque,
				Data: tc.seeded,
			}
			c := fake.NewClientBuilder().WithScheme(kube.Scheme).WithObjects(existing).Build()

			var log bytes.Buffer
			require.NoError(t, ensurePassthroughLinkSigningKey(ctx, logTo(&log), c),
				"an existing Secret with no usable key must be repaired, not error")

			var got corev1.Secret
			require.NoError(t, c.Get(ctx, types.NamespacedName{
				Namespace: "agentprimitives-system", Name: v1alpha1.PassthroughLinkSigningKeySecret,
			}, &got), "Secret must still exist")

			// The contract is the readers' contract, not a length: after install
			// reports success, DecodeHexKey — the single gate webd, channelsd and
			// `oap` all decode through — must accept what is in the Secret.
			_, decErr := passthroughlink.DecodeHexKey(got.Data["key"])
			assert.NoError(t, decErr,
				"install must leave a key every production reader accepts (webd readHexKey, channelsd loadPassthroughSigner, oap buildChatViewMinter)")

			if tc.wantPreserved != nil {
				assert.Equal(t, tc.wantPreserved, got.Data["key"],
					"a usable key must never be rotated by a re-install")
			} else {
				assert.NotEqual(t, tc.seeded["key"], got.Data["key"],
					"an unusable key must actually be replaced")
			}
			assert.Equal(t, "external-provisioner", got.Labels["app.kubernetes.io/managed-by"],
				"repair must preserve metadata owned by whoever provisioned the Secret")
			assert.Contains(t, log.String(), tc.wantLog, "the outcome must be reported to the operator")
		})
	}
}

func TestEnsureSlackOAuthSecretIsIdempotent(t *testing.T) {
	cases := []struct {
		name           string
		preSeed        func(ctx context.Context, c *fake.ClientBuilder) *fake.ClientBuilder
		wantClientID   []byte // nil means "check it's empty string / zero-len from create"
		wantTeamID     []byte
		wantIdempotent bool
	}{
		{
			name:           "first-run: Secret created with empty keys including team_id",
			preSeed:        func(_ context.Context, b *fake.ClientBuilder) *fake.ClientBuilder { return b },
			wantClientID:   []byte(""),
			wantTeamID:     []byte(""),
			wantIdempotent: false,
		},
		{
			name: "already-exists with operator-populated client_id + team_id: not overwritten",
			preSeed: func(ctx context.Context, b *fake.ClientBuilder) *fake.ClientBuilder {
				sec := &corev1.Secret{}
				sec.Namespace = "agentprimitives-system"
				sec.Name = v1alpha1.SlackOAuthSecret
				sec.Data = map[string][]byte{
					"client_id":     []byte("T12345"),
					"client_secret": []byte("xoxb-secret"),
					"team_id":       []byte("T_HOME"),
				}
				return b.WithObjects(sec)
			},
			wantClientID:   []byte("T12345"),
			wantTeamID:     []byte("T_HOME"),
			wantIdempotent: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			builder := tc.preSeed(ctx, fake.NewClientBuilder().WithScheme(kube.Scheme))
			c := builder.Build()

			require.NoError(t, ensureSlackOAuthSecret(ctx, logTo(&bytes.Buffer{}), c), "call must succeed")

			var sec corev1.Secret
			require.NoError(t, c.Get(ctx, types.NamespacedName{
				Namespace: "agentprimitives-system", Name: v1alpha1.SlackOAuthSecret,
			}, &sec), "Secret must exist")

			assert.True(t, bytes.Equal(tc.wantClientID, sec.Data["client_id"]),
				"client_id must match expected value")
			_, hasTeamID := sec.Data["team_id"]
			assert.True(t, hasTeamID, "team_id key must be present in the Secret skeleton")
			assert.True(t, bytes.Equal(tc.wantTeamID, sec.Data["team_id"]),
				"team_id must match expected value")

			if tc.wantIdempotent {
				// Second call must also succeed and must not wipe the populated fields.
				require.NoError(t, ensureSlackOAuthSecret(ctx, logTo(&bytes.Buffer{}), c), "second call must succeed")
				var sec2 corev1.Secret
				require.NoError(t, c.Get(ctx, types.NamespacedName{
					Namespace: "agentprimitives-system", Name: v1alpha1.SlackOAuthSecret,
				}, &sec2), "Secret must still exist after second call")
				assert.True(t, bytes.Equal(tc.wantClientID, sec2.Data["client_id"]),
					"re-install must not overwrite operator-populated client_id")
				assert.True(t, bytes.Equal(tc.wantTeamID, sec2.Data["team_id"]),
					"re-install must not overwrite operator-populated team_id")
			}
		})
	}
}

func TestEnsureWebdExternalURLConfigMapIsIdempotent(t *testing.T) {
	cases := []struct {
		name            string
		managedExternal bool
		preSeed         func(ctx context.Context, b *fake.ClientBuilder) *fake.ClientBuilder
		wantURL         string
	}{
		{
			name:            "unmanaged first-run: ConfigMap seeded with localhost (genuinely-correct local value)",
			managedExternal: false,
			preSeed:         func(_ context.Context, b *fake.ClientBuilder) *fake.ClientBuilder { return b },
			wantURL:         "http://localhost:8080",
		},
		{
			name:            "managed first-run: ConfigMap seeded EMPTY (fail closed; oap patches the real URL later)",
			managedExternal: true,
			preSeed:         func(_ context.Context, b *fake.ClientBuilder) *fake.ClientBuilder { return b },
			wantURL:         "",
		},
		{
			name:            "already-exists with operator URL: not overwritten",
			managedExternal: false,
			preSeed: func(ctx context.Context, b *fake.ClientBuilder) *fake.ClientBuilder {
				cm := &corev1.ConfigMap{}
				cm.Namespace = "agentprimitives-system"
				cm.Name = v1alpha1.WebdExternalURLConfigMap
				cm.Data = map[string]string{
					v1alpha1.WebdTrustedURLKey: "https://webd.example.com",
					v1alpha1.WebdSandboxURLKey: "https://webd.example.com",
				}
				return b.WithObjects(cm)
			},
			wantURL: "https://webd.example.com",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			builder := tc.preSeed(ctx, fake.NewClientBuilder().WithScheme(kube.Scheme))
			c := builder.Build()

			require.NoError(t, ensureWebdExternalURLConfigMap(ctx, logTo(&bytes.Buffer{}), c, tc.managedExternal), "call must succeed")

			var cm corev1.ConfigMap
			require.NoError(t, c.Get(ctx, types.NamespacedName{
				Namespace: "agentprimitives-system", Name: v1alpha1.WebdExternalURLConfigMap,
			}, &cm), "ConfigMap must exist")

			assert.Equal(t, tc.wantURL, cm.Data[v1alpha1.WebdTrustedURLKey], "trusted-url key must match expected value")
			// The managed (empty) seed must set BOTH keys empty — a stray
			// localhost on the sandbox key would re-introduce the bug.
			assert.Equal(t, tc.wantURL, cm.Data[v1alpha1.WebdSandboxURLKey], "sandbox-url key must match expected value")

			// Second call is always a no-op; url must not change.
			require.NoError(t, ensureWebdExternalURLConfigMap(ctx, logTo(&bytes.Buffer{}), c, tc.managedExternal), "second call must succeed")
			var cm2 corev1.ConfigMap
			require.NoError(t, c.Get(ctx, types.NamespacedName{
				Namespace: "agentprimitives-system", Name: v1alpha1.WebdExternalURLConfigMap,
			}, &cm2), "ConfigMap must still exist after second call")
			assert.Equal(t, tc.wantURL, cm2.Data[v1alpha1.WebdTrustedURLKey],
				"re-install must not overwrite the url in the ConfigMap")
		})
	}
}

// When oap manages external access (--trusted-hostname), the hint must NOT tell
// the operator to set the URLs by hand (setupWebdExternalAccess sets them at the
// end of the install) — that confused a GKE install that showed "defaults to
// localhost" mid-run. When unmanaged, the set-them-yourself hint stays.
func TestEnsureWebdExternalURLConfigMap_HintMatchesOwnership(t *testing.T) {
	cases := []struct {
		name            string
		managedExternal bool
		wantContains    string
		wantAbsent      string
	}{
		{name: "managed (--trusted-hostname): says oap will set them, not set-yourself",
			managedExternal: true, wantContains: "oap will set", wantAbsent: "externally reachable"},
		{name: "unmanaged: hints to set the URLs yourself",
			managedExternal: false, wantContains: "externally reachable", wantAbsent: "oap will set"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			c := fake.NewClientBuilder().WithScheme(kube.Scheme).Build()
			var buf bytes.Buffer
			require.NoError(t, ensureWebdExternalURLConfigMap(ctx, logTo(&buf), c, tc.managedExternal))
			assert.Contains(t, buf.String(), tc.wantContains)
			assert.NotContains(t, buf.String(), tc.wantAbsent)
		})
	}
}

// TestPrintInstallCompletionSummaryDeclinedHostnameChange asserts the four
// paths printInstallCompletionSummary takes: external access skipped for any
// reason (must NOT print the URL), declined hostname change (must NOT print
// the new-hostname URL), fresh/accepted with addr ready (URL only), and no
// hostname set (generic next-step hint). This is the primary guard for the
// bug where declining a hostname change caused the completion message to
// advertise the new (unapplied) URL, and for the skip path when external
// access is not available.
func TestPrintInstallCompletionSummaryDeclinedHostnameChange(t *testing.T) {
	cases := []struct {
		name                  string
		trustedHostname       string
		gatewayAddrReady      bool
		hostnameDeclined      bool
		externalAccessSkipped bool
		wantContains          string
		wantNotContains       string
	}{
		{
			name:                  "external access skipped: skip message, no URL",
			trustedHostname:       "webd.example.com",
			gatewayAddrReady:      false,
			hostnameDeclined:      false,
			externalAccessSkipped: true,
			wantContains:          "webd external access was skipped",
			wantNotContains:       "webd.example.com",
		},
		{
			name:             "declined hostname change: unchanged message, no new URL",
			trustedHostname:  "webd.example.com",
			gatewayAddrReady: false,
			hostnameDeclined: true,
			wantContains:     "webd external access unchanged",
			wantNotContains:  "webd.example.com",
		},
		{
			name:             "accepted change, addr ready: live URL printed",
			trustedHostname:  "webd.example.com",
			gatewayAddrReady: true,
			hostnameDeclined: false,
			wantContains:     "https://webd.example.com",
			wantNotContains:  "once DNS resolves",
		},
		{
			name:             "accepted change, addr not ready: URL with DNS caveat",
			trustedHostname:  "webd.example.com",
			gatewayAddrReady: false,
			hostnameDeclined: false,
			wantContains:     "https://webd.example.com",
			wantNotContains:  "webd external access unchanged",
		},
		{
			name:             "no hostname: generic next-step hint",
			trustedHostname:  "",
			gatewayAddrReady: false,
			hostnameDeclined: false,
			wantContains:     "oap --help",
			wantNotContains:  "open the web UI",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			rep := progress.New(&buf, bytes.NewReader(nil), true)
			printInstallCompletionSummary(rep, tc.trustedHostname, tc.gatewayAddrReady, tc.hostnameDeclined, tc.externalAccessSkipped)
			out := buf.String()
			assert.Contains(t, out, tc.wantContains, "completion output must contain expected text")
			assert.NotContains(t, out, tc.wantNotContains, "completion output must not contain excluded text")
		})
	}
}

// TestConfirmWebdHostnameChange verifies that re-running oap install with a
// changed hostname surfaces a warning and that the proceed flag is correct for
// all relevant cases (fresh install, same hostname, changed hostname with
// --yes, changed hostname non-interactively).
//
// The fresh-install (no-routes) case is only reached after EnsureGatewayController
// has guaranteed the Gateway API is served, so the fake-client IsNotFound the
// function receives for a missing HTTPRoute matches production (a missing route,
// not a missing CRD). No assertion change is needed — the test still validates
// the hostname-change comparison logic.
func TestConfirmWebdHostnameChange(t *testing.T) {
	ctx := context.Background()

	// seedRoute seeds a fake HTTPRoute carrying the given hostname so the
	// function under test can detect a pre-existing external-access config.
	seedRoute := func(b *fake.ClientBuilder, name, hostname string) *fake.ClientBuilder {
		route := &gatewayv1.HTTPRoute{
			ObjectMeta: metav1.ObjectMeta{Namespace: cloud.WebdServiceNamespace, Name: name},
			Spec: gatewayv1.HTTPRouteSpec{
				Hostnames: []gatewayv1.Hostname{gatewayv1.Hostname(hostname)},
			},
		}
		return b.WithObjects(route)
	}

	cases := []struct {
		name        string
		seed        func(*fake.ClientBuilder) *fake.ClientBuilder
		opts        WebdRoutingOpts
		isTTY       bool
		wantProceed bool
		wantWarn    bool
	}{
		{
			name:        "fresh install (no routes): proceed with no warning",
			seed:        func(b *fake.ClientBuilder) *fake.ClientBuilder { return b },
			opts:        WebdRoutingOpts{trustedHostname: "webd.example.com", sandboxHostname: "sandbox.example.com", assumeYes: true},
			isTTY:       false,
			wantProceed: true,
			wantWarn:    false,
		},
		{
			name: "existing route with same hostname: proceed with no warning",
			seed: func(b *fake.ClientBuilder) *fake.ClientBuilder {
				return seedRoute(b, webdTrustedRouteName, "webd.example.com")
			},
			opts:        WebdRoutingOpts{trustedHostname: "webd.example.com", assumeYes: true},
			isTTY:       false,
			wantProceed: true,
			wantWarn:    false,
		},
		{
			name: "trusted hostname changed + --yes: warn but proceed automatically",
			seed: func(b *fake.ClientBuilder) *fake.ClientBuilder {
				return seedRoute(b, webdTrustedRouteName, "old.example.com")
			},
			opts:        WebdRoutingOpts{trustedHostname: "webd.example.com", assumeYes: true},
			isTTY:       false,
			wantProceed: true,
			wantWarn:    true,
		},
		{
			name: "trusted hostname changed + non-interactive (no --yes, no TTY): warn but proceed",
			seed: func(b *fake.ClientBuilder) *fake.ClientBuilder {
				return seedRoute(b, webdTrustedRouteName, "old.example.com")
			},
			// assumeYes=false, isTTY=false — the function takes the non-interactive
			// warn-only path (don't block automation) and returns proceed=true.
			opts:        WebdRoutingOpts{trustedHostname: "webd.example.com", assumeYes: false},
			isTTY:       false,
			wantProceed: true,
			wantWarn:    true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := tc.seed(fake.NewClientBuilder().WithScheme(kube.Scheme))
			c := b.Build()
			var buf bytes.Buffer
			rep := progress.New(&buf, bytes.NewReader(nil), true)
			proceed, err := confirmWebdHostnameChange(ctx, c, tc.opts, rep, bytes.NewReader(nil), &buf, tc.isTTY)
			require.NoError(t, err, "confirmWebdHostnameChange must not return an error")
			assert.Equal(t, tc.wantProceed, proceed, "proceed flag must match expected")
			if tc.wantWarn {
				assert.Contains(t, buf.String(), "webd external-access settings are changing",
					"warning must mention the hostname change")
			} else {
				assert.NotContains(t, buf.String(), "webd external-access settings are changing",
					"no warning expected for this case")
			}
		})
	}
}
