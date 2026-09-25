package main

import (
	"testing"

	"github.com/go-logr/logr/funcr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/browser"
	channelregistry "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/registry"
	"github.com/authzed/openagentprimitives/pkg/web/webui/agentui"
	"github.com/authzed/openagentprimitives/pkg/web/webui/chat"
)

// newViewerDepsForTest builds a *artifactViewDeps that satisfies chat.Deps and
// agentui.Deps by TYPE (the methods exist) while carrying NONE of the
// underlying collaborators (no NATS, no SpiceDB, no operator URL).
//
// The cast succeeding is what makes the test below discriminating: if the cast
// itself were the reason chat.Routes returned nil, the prerequisite gate could
// be deleted outright and the assertion would still pass, by falling through to
// a different nil-returning path.
func newViewerDepsForTest(t *testing.T, logged *[]string) *artifactViewDeps {
	t.Helper()
	capLogger := funcr.New(func(_, args string) {
		*logged = append(*logged, args)
	}, funcr.Options{})

	scheme := runtime.NewScheme()
	require.NoError(t, spiceboxv1alpha1.AddToScheme(scheme), "add spicebox types to scheme")
	k8s := fake.NewClientBuilder().WithScheme(scheme).Build()

	return &artifactViewDeps{
		webdDeps: &webdDeps{
			k8s:             k8s,
			externalBaseURL: func() string { return "https://trusted.example" },
		},
		// logger MUST be set here, on *artifactViewDeps itself, not on the
		// embedded *webdDeps: artifactViewDeps.Logger() is its own method
		// (returning THIS field), which shadows the promoted webdDeps.Logger()
		// for any Deps cast — including chat.Deps and agentui.Deps. Setting it
		// only on webdDeps silently discards every log line chat.Routes and
		// ensureRegistry emit, which would make the log assertion below pass
		// vacuously no matter what the code under test does.
		logger: capLogger,
	}
}

// TestChatMountIsGatedOnPrerequisitesAndSaysSo pins what decides whether the
// conversation routes mount: this process's own collaborators, per plugin, not
// the shape of the deployment.
//
// The deps here carry no NATS connection, so the plugin refuses — and the
// refusal must LOG which prerequisite was missing. That log is the whole point
// of the assertion: an operator whose conversation routes are absent has
// nothing else to read, and a refusal that says nothing is indistinguishable
// from a plugin that was never registered.
//
// The second half is the counterpart: on the very same deps, the browser
// channel kind resolves with its capabilities intact and the agent-UI plugin
// mounts. Those two never depended on the transcript plane's collaborators and
// must not start to.
func TestChatMountIsGatedOnPrerequisitesAndSaysSo(t *testing.T) {
	var logged []string
	deps := newViewerDepsForTest(t, &logged)

	assert.Empty(t, chat.New().Routes(deps),
		"the conversation routes must not mount when NATS is unconfigured")
	require.NotEmpty(t, logged, "an unmet prerequisite must be logged, never a silent absence")
	assert.Contains(t, logged[0], "NATS is not configured",
		"the log must name the prerequisite that was missing, not merely that something was")

	k, ok := channelregistry.Get(browser.KindName)
	require.True(t, ok, "the browser channel kind must be registered in webd")
	assert.NotEmpty(t, k.Capabilities())
	assert.NotEmpty(t, agentui.New().Routes(deps), "the agent-UI plugin mounts on its own per-request authorization")
}
