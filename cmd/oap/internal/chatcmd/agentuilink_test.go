// The two pure pieces of the TUI's agent-UI
// wiring: the shared webd base-URL read both chat minters are built from, and
// the URL the minter composes from it. The assignment into
// local.HostConfig.Deps in startSession is NOT covered here — it needs a live
// bubbletea program — so a break there surfaces only as a missing button.
package chatcmd

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/kube"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/x/externalurl"
)

// webdURLBundle builds a kube.Bundle whose controller client holds the webd
// external-URL ConfigMap with the given data, or nothing at all when data is
// nil — the state before the webd install task has run.
func webdURLBundle(t *testing.T, data map[string]string) *kube.Bundle {
	t.Helper()
	builder := fake.NewClientBuilder().WithScheme(newAgentChatScheme(t))
	if data != nil {
		builder = builder.WithObjects(&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: externalurl.Namespace,
				Name:      spiceboxv1alpha1.WebdExternalURLConfigMap,
			},
			Data: data,
		})
	}
	return &kube.Bundle{Controller: builder.Build()}
}

// TestReadWebdTrustedBaseURL covers the three states the one shared read can
// be in, and the distinction that matters: an absent ConfigMap is a reason
// (the caller declines to build a minter), while a present-but-unpopulated
// one is a successful read of an empty URL (the caller still builds one, and
// the sender reports "not configured yet" only if an offer ever needs it).
func TestReadWebdTrustedBaseURL(t *testing.T) {
	cases := []struct {
		name       string
		data       map[string]string
		wantBase   string
		wantReason bool
	}{
		{
			name:     "populated ConfigMap: the trusted base URL, no reason",
			data:     map[string]string{spiceboxv1alpha1.WebdTrustedURLKey: "https://webd.example.test"},
			wantBase: "https://webd.example.test",
		},
		{
			name:     "ConfigMap present but key unset: an empty URL is a successful read",
			data:     map[string]string{},
			wantBase: "",
		},
		{
			name:       "no ConfigMap at all: a reason, and no URL to build on",
			data:       nil,
			wantBase:   "",
			wantReason: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base, reason := readWebdTrustedBaseURL(context.Background(), webdURLBundle(t, tc.data))

			assert.Equal(t, tc.wantBase, base)
			if !tc.wantReason {
				assert.Empty(t, reason, "a readable ConfigMap must not report a failure")
				return
			}
			assert.NotEmpty(t, reason, "an unreadable source must name a cause the TUI can print")
			// The reason is rendered into the TUI timeline, so it must carry
			// no cluster-internal identifier. Asserted per-token rather than
			// as one big string match so a partial regression still fails.
			for _, leak := range []string{"ConfigMap", "configmap", spiceboxv1alpha1.WebdExternalURLConfigMap, externalurl.Namespace} {
				assert.NotContains(t, reason, leak,
					"user-facing copy must not name %q — the reader cannot act on a cluster object", leak)
			}
		})
	}
}

// TestBuildChatAgentUIMinterKeepsAnEmptyURLUsable pins the sibling semantics
// buildChatSessionViewMinter established: only an unreadable ConfigMap
// declines. A ConfigMap that exists but carries no URL still yields a minter,
// so the "not configured yet" answer is given by the sender at offer time
// rather than by a startup notice about a page the session may never use.
func TestBuildChatAgentUIMinterKeepsAnEmptyURLUsable(t *testing.T) {
	minter, reason := buildChatAgentUIMinter(context.Background(), webdURLBundle(t, map[string]string{}))

	require.NotNil(t, minter, "a readable-but-unpopulated ConfigMap must still produce a minter")
	assert.Empty(t, reason, "an unpopulated URL is not a build failure")

	got, err := minter.MintAgentUILink("demo-ns/demo-session")
	require.NoError(t, err, "an empty base URL is a clean skip, not an error")
	assert.Empty(t, got, "with no webd URL there is no link to offer")
}

// TestBuildChatAgentUIMinterDeclinesWhenWebdIsUnreadable is the other half:
// the nil here must be a TRUE nil interface, which is why the builder returns
// the untyped nil rather than a typed *tuiAgentUIMinter.
func TestBuildChatAgentUIMinterDeclinesWhenWebdIsUnreadable(t *testing.T) {
	minter, reason := buildChatAgentUIMinter(context.Background(), webdURLBundle(t, nil))

	assert.Nil(t, minter, "an unreadable ConfigMap must leave the Deps field a true nil interface")
	assert.NotEmpty(t, reason, "and must name the cause, or the TUI prints nothing at all")
}

// TestTUIAgentUIMinterComposesTheShellURL is the one that distinguishes this
// minter from its session-view sibling: the agent-UI shell takes the session
// as a QUERY parameter, where the session-view page takes it as a path. A
// minter pointed at the wrong composer produces a URL that looks plausible
// and opens the wrong page.
func TestTUIAgentUIMinterComposesTheShellURL(t *testing.T) {
	m := &tuiAgentUIMinter{webdBaseURL: "https://webd.example.test"}

	got, err := m.MintAgentUILink("demo-ns/demo-session")
	require.NoError(t, err, "a well-formed sessionRef must compose")
	assert.Equal(t, "https://webd.example.test/sessions?session=demo-ns%2Fdemo-session", got,
		"the TUI must link to the agent-UI shell, not to the session-view page")
}

// TestTUIAgentUIMinterRejectsAMalformedSessionRef: a ref with no "ns/name"
// split is a caller bug and must surface as an error rather than a link that
// opens the shell on nothing.
func TestTUIAgentUIMinterRejectsAMalformedSessionRef(t *testing.T) {
	m := &tuiAgentUIMinter{webdBaseURL: "https://webd.example.test"}

	_, err := m.MintAgentUILink("no-slash")
	require.Error(t, err, "a sessionRef without ns/name must be rejected")
	assert.Contains(t, err.Error(), "sessionRef", "the error must name what was malformed")
}
