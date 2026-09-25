package toolscmd

import (
	"bytes"
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	authzspicedb "github.com/authzed/openagentprimitives/pkg/authz/spicedb"
	"github.com/authzed/openagentprimitives/pkg/web/browsersession"
	"github.com/authzed/openagentprimitives/pkg/x/externalurl"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/aptest"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// setAgentClassValid marks ac Valid=True the same way pkg/web/browsersession's
// own tests do (browsersession_test.go's demoAgentClass): a direct
// Status.Conditions write. apcmd.AgentClassIsValid and
// browsersession.AgentClassReady both read this same condition, so one write
// satisfies both readers.
func setAgentClassValid(t *testing.T, ac *spiceboxv1alpha1.AgentClass) {
	t.Helper()
	ac.Status.Conditions = []metav1.Condition{{
		Type:   spiceboxv1alpha1.AgentClassConditionValid,
		Status: metav1.ConditionTrue,
		Reason: "Valid",
	}}
}

// validBuilderClass returns an agent-builder AgentClass marked Valid=True.
func validBuilderClass(t *testing.T) *spiceboxv1alpha1.AgentClass {
	t.Helper()
	ac := &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: "agent-builder", Namespace: "agentprimitives-system", UID: "builder-uid"},
	}
	setAgentClassValid(t, ac)
	return ac
}

// webdURLConfigMap seeds webd's external-URL ConfigMap so ComposeAgentUIURL
// yields a non-empty URL.
func webdURLConfigMap() *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: externalurl.Namespace, Name: spiceboxv1alpha1.WebdExternalURLConfigMap},
		Data:       map[string]string{spiceboxv1alpha1.WebdTrustedURLKey: "https://oap.example.test"},
	}
}

// installSeams stubs the three I/O boundaries and restores them on cleanup.
// createBrowserSession records the Params it was called with; openBrowser
// records the URL (empty = not called).
func installSeams(t *testing.T, createErr error) (gotParams *browsersession.Params, gotURL *string) {
	t.Helper()
	gotParams = &browsersession.Params{}
	gotURL = new(string)
	called := new(bool)

	origDial, origCreate, origOpen := dialSpiceDB, createBrowserSession, openBrowser
	t.Cleanup(func() { dialSpiceDB, createBrowserSession, openBrowser = origDial, origCreate, origOpen })

	// A non-nil zero-value client: its methods are never called because
	// createBrowserSession is stubbed, but it must be non-nil so the Deps'
	// Granter/StartChecker are honest interfaces (typed-nil rule).
	dialSpiceDB = func(_ context.Context, _ *kubeBundle) (*authzspicedb.Client, func(), error) {
		return &authzspicedb.Client{}, func() {}, nil
	}
	createBrowserSession = func(_ context.Context, _ browsersession.Deps, p browsersession.Params) (browsersession.Created, error) {
		*called = true
		*gotParams = p
		if createErr != nil {
			return browsersession.Created{}, createErr
		}
		return browsersession.Created{Session: &spiceboxv1alpha1.AgentSession{
			ObjectMeta: metav1.ObjectMeta{Name: "agent-builder-abcd1234", Namespace: p.Namespace},
		}}, nil
	}
	openBrowser = func(url string) error { *gotURL = url; return nil }
	return gotParams, gotURL
}

func TestToolsGen_HappyPath_CreatesSessionAndOpensWorkshop(t *testing.T) {
	aptest.FakeIdentityConfigDir(t)
	params, gotURL := installSeams(t, nil)
	b := aptest.NewBundle(t, validBuilderClass(t), webdURLConfigMap())
	g := aptest.GlobalsFor(b)

	var out bytes.Buffer
	require.NoError(t, runToolsGen(context.Background(), g, &out, "agentprimitives-system", false))

	assert.Equal(t, "agent-builder", params.AgentClass)
	assert.Equal(t, "agentprimitives-system", params.Namespace)
	assert.Equal(t, builderOpeningPrompt, params.Prompt, "opens with the builder intake turn, not the wait-style UI prompt")
	assert.NotEmpty(t, params.Subject, "attributed to the CLI user")
	assert.Contains(t, *gotURL, "/sessions?session=agentprimitives-system%2Fagent-builder-abcd1234")
	assert.Contains(t, out.String(), *gotURL)
}

func TestToolsGen_NoBrowser_PrintsURLAndDoesNotOpen(t *testing.T) {
	aptest.FakeIdentityConfigDir(t)
	_, gotURL := installSeams(t, nil)
	b := aptest.NewBundle(t, validBuilderClass(t), webdURLConfigMap())
	g := aptest.GlobalsFor(b)

	var out bytes.Buffer
	require.NoError(t, runToolsGen(context.Background(), g, &out, "agentprimitives-system", true))
	assert.Empty(t, *gotURL, "--no-browser must not open a browser")
	assert.Contains(t, out.String(), "/sessions?session=")
}

func TestToolsGen_BuilderNotInstalled_FriendlyErrorNoSession(t *testing.T) {
	aptest.FakeIdentityConfigDir(t)
	params, gotURL := installSeams(t, nil)
	b := aptest.NewBundle(t /* no AgentClass seeded */)
	g := aptest.GlobalsFor(b)

	var out bytes.Buffer
	err := runToolsGen(context.Background(), g, &out, "agentprimitives-system", false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "isn't installed")
	assert.Empty(t, params.AgentClass, "no session creation attempted when the builder is absent")
	assert.Empty(t, *gotURL)
}

func TestToolsGen_NotAnAllowedStarter_FriendlyError(t *testing.T) {
	aptest.FakeIdentityConfigDir(t)
	_, gotURL := installSeams(t, browsersession.ErrNotAnAllowedStarter)
	b := aptest.NewBundle(t, validBuilderClass(t), webdURLConfigMap())
	g := aptest.GlobalsFor(b)

	var out bytes.Buffer
	err := runToolsGen(context.Background(), g, &out, "agentprimitives-system", false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "allowed-starters")
	assert.Empty(t, *gotURL, "a refused starter never opens a browser")
}
