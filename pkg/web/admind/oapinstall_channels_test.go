package admind

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-logr/logr/testr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/platform/oap"
	"github.com/authzed/openagentprimitives/pkg/platform/oap/channelplan"
	"github.com/authzed/openagentprimitives/pkg/platform/oap/install"
	"github.com/authzed/openagentprimitives/pkg/platform/oap/instance"

	// The kinds the bundles below declare. admind is hosted by
	// internal/cmd/operator, which blank-imports all six; this TEST binary has
	// no such main, so it registers the two it names, exactly as a real
	// consumer's main package would.
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/github"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/slack"
)

// --- fixtures ---------------------------------------------------------------

// allowChecker is the PlatformChecker the install route is gated on. It grants
// every permission to one canonical subject and nothing to anyone else, which
// is all this file needs: authorization itself is handler_test.go's subject.
type allowChecker struct{ canonical string }

func (c allowChecker) CheckPlatformPermission(_ context.Context, _ string, id identity.CanonicalUserID, _ bool) (bool, error) {
	return id.String() == c.canonical, nil
}

func (c allowChecker) ListPlatformAdmins(context.Context) ([]string, error) { return nil, nil }

func (c allowChecker) CheckAgentIdentityUpdateCredential(context.Context, string, string, identity.CanonicalUserID) (bool, error) {
	return false, nil
}

func channelTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	sch := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(sch))
	require.NoError(t, spiceboxv1alpha1.AddToScheme(sch))
	return sch
}

// newChannelFormAdmind builds an Admind over the supplied cluster. The
// controller-runtime fake client is enough for every read this file's paths
// make (a Channel Get per declared channel, one ConfigMap Get for the external
// base URL); nothing here installs anything, so no real apiserver is needed.
func newChannelFormAdmind(t *testing.T, c client.Client) *Admind {
	t.Helper()
	a, err := New(Config{
		Mem:     memory.NewLocal(inmem.NewBackend()),
		K8s:     c,
		Checker: allowChecker{canonical: "YWRtaW4"},
		Token:   "test-token",
		Logger:  testr.New(t),
		// A closed loopback port, so any cluster-kind detection fails fast
		// instead of reaching for a real metadata service.
		MetadataBaseURL: "http://127.0.0.1:1",
	})
	require.NoError(t, err)
	return a
}

// packedChannelBundle is a valid .oap declaring one channel and ONE required
// question with no default — so a POST that answers nothing lands on the
// missing-questions 400 this task extends.
func packedChannelBundle(t *testing.T, chans ...oap.RequiredChannel) []byte {
	t.Helper()
	m := &oap.Manifest{
		OapFormatVersion: "1",
		Agent:            oap.Agent{Name: "demo-agent", Version: "1.0.0"},
		Requires:         oap.Requires{Channels: chans},
		Questions: []oap.Question{{
			Name:    "greeting",
			Type:    oap.QString,
			Prompt:  "What should this agent open with?",
			Binding: []oap.Binding{{Target: "AgentClass/demo-class#spec.systemPrompt.inline"}},
		}},
	}
	b := &oap.Bundle{
		Manifest: m,
		Manifests: []byte("apiVersion: agentprimitives.authzed.com/v1alpha1\n" +
			"kind: AgentClass\n" +
			"metadata:\n  name: demo-class\n" +
			"spec:\n  systemPrompt:\n    inline: placeholder\n"),
	}
	require.NoError(t, b.Validate(), "the test's own bundle must be a valid one")
	packed, err := oap.Pack(b)
	require.NoError(t, err, "pack the fixture bundle")
	return packed
}

// packedBundleWithCredential is packedChannelBundle plus a bundled
// AgentIdentity whose one credential reads the named Secret.
//
// It exists because the credentials cross-check — the thing
// LintRequiredChannels was written for — is invisible to a fixture with no
// AgentIdentity in it: the check walks the bundled CRs, and a bundle carrying
// only an AgentClass gives it nothing to compare against, so a route that
// skipped the lint entirely would look identical to one that ran it.
func packedBundleWithCredential(t *testing.T, secretName string, chans ...oap.RequiredChannel) []byte {
	t.Helper()
	m := &oap.Manifest{
		OapFormatVersion: "1",
		Agent:            oap.Agent{Name: "demo-agent", Version: "1.0.0"},
		Requires:         oap.Requires{Channels: chans},
		Questions: []oap.Question{{
			Name:    "greeting",
			Type:    oap.QString,
			Prompt:  "What should this agent open with?",
			Binding: []oap.Binding{{Target: "AgentClass/demo-class#spec.systemPrompt.inline"}},
		}},
	}
	b := &oap.Bundle{
		Manifest: m,
		Manifests: []byte("apiVersion: agentprimitives.authzed.com/v1alpha1\n" +
			"kind: AgentClass\n" +
			"metadata:\n  name: demo-class\n" +
			"spec:\n  systemPrompt:\n    inline: placeholder\n" +
			"---\n" +
			"apiVersion: agentprimitives.authzed.com/v1alpha1\n" +
			"kind: AgentIdentity\n" +
			"metadata:\n  name: demo-agent-id\n" +
			"spec:\n  credentials:\n" +
			"    - name: channel-creds\n" +
			"      type: static\n" +
			"      static:\n" +
			"        secretRef:\n" +
			"          name: " + secretName + "\n" +
			"          key: token\n"),
	}
	require.NoError(t, b.Validate(), "the test's own bundle must be a valid one")
	packed, err := oap.Pack(b)
	require.NoError(t, err, "pack the fixture bundle")
	return packed
}

// postInstall POSTs a packed .oap to the real route, through the real handler
// chain, as the one subject allowChecker grants. The UPLOAD shape rather than
// the registry-ref one: a ref would have to be pulled, and no test here may
// reach the network.
func postInstall(t *testing.T, a *Admind, packed []byte, fields map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", "agent.oap")
	require.NoError(t, err)
	_, err = fw.Write(packed)
	require.NoError(t, err)
	for k, v := range fields {
		require.NoError(t, mw.WriteField(k, v))
	}
	require.NoError(t, mw.Close())

	req := httptest.NewRequest(http.MethodPost, "/admin/v1/agents/oap-install", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("X-Admin-Subject", "user:YWRtaW4")
	rec := httptest.NewRecorder()
	a.Handler().ServeHTTP(rec, req)
	return rec
}

// planFixture is a ChannelPlan built the way channelplan.PlanChannels builds
// one: three non-nil maps, and the three keys a real plan seeds. A fixture
// that left Seeded nil would make every "is this key seeded?" test pass for
// the wrong reason.
func planFixture(t *testing.T, rc oap.RequiredChannel, seeded map[string]string, source string) channelplan.ChannelPlan {
	t.Helper()
	p := channelplan.ChannelPlan{
		Required:    rc,
		WiringKnown: true,
		Seeded:      map[string]string{},
		SeededFrom:  map[string]string{},
		NotSeeded:   map[string]string{},
	}
	for k, v := range seeded {
		p.Seeded[k] = v
		p.SeededFrom[k] = source
	}
	return p
}

// stubWizard is a Wizard whose Inputs and Handoff answers are scripted, so a
// row can be built for a kind that misbehaves without registering one into the
// process-wide registry.
type stubWizard struct {
	questions   []oap.Question
	inputsErr   error
	resultErr   error
	resultOut   channelkinds.WizardOutput
	resultCalls *int
	handoff     *channelkinds.HandoffSpec
}

func (w stubWizard) Inputs(context.Context, channelkinds.WizardInput) ([]oap.Question, error) {
	return w.questions, w.inputsErr
}

func (w stubWizard) Handoff(context.Context, channelkinds.WizardInput) (*channelkinds.HandoffSpec, error) {
	return w.handoff, nil
}

func (w stubWizard) Resolve(context.Context, channelkinds.WizardInput, map[string]string) (map[string]string, error) {
	return nil, nil
}

func (w stubWizard) Result(channelkinds.WizardInput, map[string]string) (channelkinds.WizardOutput, error) {
	if w.resultCalls != nil {
		*w.resultCalls++
	}
	return w.resultOut, w.resultErr
}

// wizards returns a lookup that answers with w for every kind.
func wizards(w channelkinds.Wizard) channelWizardLookup {
	return func(string) (channelkinds.Wizard, bool) { return w, true }
}

func TestDeclaredChannelGraphUsesExactMappedCredentialName(t *testing.T) {
	const (
		namespace       = "test-channel-namespace"
		logicalChannel  = "transport"
		physicalChannel = "test-root-dependency-transport"
	)
	physicalCredential := strings.Repeat("c", 242) + "-0123456789"
	physicalAgent := strings.Repeat("a", 240)
	c := &apiIdentityClient{Client: ctrlfake.NewClientBuilder().WithScheme(channelTestScheme(t)).Build()}
	a := newChannelFormAdmind(t, c)
	a.wizardFor = wizards(stubWizard{resultOut: channelkinds.WizardOutput{
		SecretManifest: &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "wizard-default-creds"}},
		ChannelManifest: &spiceboxv1alpha1.Channel{
			ObjectMeta: metav1.ObjectMeta{Name: "wizard-default"},
			Spec:       spiceboxv1alpha1.ChannelSpec{CredentialsRef: spiceboxv1alpha1.ChannelCredentialsRef{SecretName: "wizard-default-creds"}},
		},
	}})
	bundle := &oap.Bundle{Manifest: &oap.Manifest{
		OapFormatVersion: "1", Agent: oap.Agent{Name: "test-root", Version: "1.0.0"},
		Requires: oap.Requires{Channels: []oap.RequiredChannel{{Kind: "test-kind", Role: "both", Name: logicalChannel}}},
	}}
	node := install.NodeContext{
		Path: oap.DependencyPath{"dependency"}, Bundle: bundle, RootInstallName: "test-root", PhysicalName: physicalAgent,
		Namespace: namespace, ResourceNames: instance.NameMap{
			"Channel/" + logicalChannel:           physicalChannel,
			"Secret/" + logicalChannel + "-creds": physicalCredential,
		},
	}
	owner := identity.CanonicalFromTrusted("YWRtaW4", "test fixture")
	base := &channelSetupBinding{RootDigest: "sha256:root", RootNamespace: namespace, RootInstall: "test-root"}

	_, rows, _, err := a.declaredChannelFormForNode(context.Background(), node, owner, base)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.NotEmpty(t, rows[0].SetupToken)
	setup := postChannelSetup(t, a, rows[0].SetupToken, "user:YWRtaW4", nil)
	require.Equal(t, http.StatusOK, setup.Code, "body: %s", setup.Body.String())

	pending, err := a.channelSetups.get(rows[0].SetupToken, owner)
	require.NoError(t, err)
	require.NotNil(t, pending.graphBinding)
	assert.Equal(t, physicalCredential, pending.graphBinding.CredentialSecret)
	require.NotNil(t, pending.staged.output.SecretManifest)
	assert.Equal(t, physicalCredential, pending.staged.output.SecretManifest.Name)
	require.NotNil(t, pending.staged.output.ChannelManifest)
	assert.Equal(t, physicalCredential, pending.staged.output.ChannelManifest.Spec.CredentialsRef.SecretName)

	planned, _, _, err := a.declaredChannelFormForNode(context.Background(), node, owner, base)
	require.NoError(t, err)
	require.NoError(t, planned.Resolve(context.Background()))
	require.NoError(t, planned.Apply(context.Background()))
	var secret corev1.Secret
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: namespace, Name: physicalCredential}, &secret))
	var channel spiceboxv1alpha1.Channel
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: namespace, Name: physicalChannel}, &channel))
	assert.Equal(t, physicalCredential, channel.Spec.CredentialsRef.SecretName)
}

// --- the endpoint ------------------------------------------------------------

// TestOapInstall_MissingChannelInputsAreReturnedForRendering is the task's
// headline: a bundle declaring a channel gets that channel's own inputs on the
// same 400 the manifest questions come back on, so the UI can render one form.
func TestOapInstall_MissingChannelInputsAreReturnedForRendering(t *testing.T) {
	a := newChannelFormAdmind(t, ctrlfake.NewClientBuilder().WithScheme(channelTestScheme(t)).Build())

	rec := postInstall(t, a,
		packedChannelBundle(t, oap.RequiredChannel{
			Kind: "slack", Role: "output", Name: "demo-agent-slack",
			Purpose: "Where this agent posts its reviews.",
		}),
		map[string]string{"namespace": "demo-ns"})
	require.Equal(t, http.StatusBadRequest, rec.Code, "body: %s", rec.Body.String())

	var body oapInstallMissingQuestionsResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.NotEmpty(t, body.Questions, "the manifest question is still what makes this a 400")
	require.Len(t, body.Channels, 1)
	ch := body.Channels[0]
	assert.Equal(t, "demo-agent-slack", ch.Name)
	assert.Equal(t, "slack", ch.Kind)
	assert.Equal(t, "output", ch.Role)
	assert.Equal(t, "Where this agent posts its reviews.", ch.Purpose)
	assert.Equal(t, channelFormAsk, ch.Status)
	require.NotEmpty(t, ch.Questions, "the UI cannot render a form from nothing")

	// The same safety property the bundle questions have — asserted on the
	// STRUCTURE, not as a substring sweep for a token prefix.
	//
	// A sweep is the wrong instrument here and would fail a correct
	// implementation: slack's own question is literally prompted "Bot User
	// OAuth Token (xoxb-)", so the prefix appears in this body BECAUSE the
	// body carries the field's shape, which is the property under test. It
	// would also pass an implementation that leaked a credential under any
	// other prefix. What must hold is that no ANSWER travels: a secret-typed
	// question carries no value, and a seeded answer for one is redacted
	// (TestDeclaredChannelRow_ASeededSecretIsReportedWithoutItsValue puts a
	// real token-shaped value through that path and proves it does not come
	// out).
	secrets := 0
	for _, q := range ch.Questions {
		if q.Type != oap.QSecret {
			continue
		}
		secrets++
		assert.Nil(t, q.Default, "a secret question's SHAPE must not carry a value")
	}
	assert.Positive(t, secrets, "slack asks for two tokens; a run with none would make the check above vacuous")
	for _, s := range ch.Seeded {
		assert.False(t, s.Redacted && s.Value != "", "a redacted seed must not also carry its value")
	}
}

// TestOapInstall_SeededChannelAnswersAreReportedNotAsked pins B-R13 END TO END,
// against the real slack wizard and a real plan: the two answers the bundle
// itself determines are reported as decided, with provenance, and are NOT in
// the form the operator can edit.
func TestOapInstall_SeededChannelAnswersAreReportedNotAsked(t *testing.T) {
	a := newChannelFormAdmind(t, ctrlfake.NewClientBuilder().WithScheme(channelTestScheme(t)).Build())

	rec := postInstall(t, a,
		packedChannelBundle(t, oap.RequiredChannel{Kind: "slack", Role: "output", Name: "demo-agent-slack"}),
		map[string]string{"namespace": "demo-ns"})
	require.Equal(t, http.StatusBadRequest, rec.Code, "body: %s", rec.Body.String())

	var body oapInstallMissingQuestionsResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Len(t, body.Channels, 1)
	ch := body.Channels[0]

	// Non-empty first: the loop below asserts nothing at all over an empty
	// list, which is precisely the state a broken builder would produce.
	require.NotEmpty(t, ch.Questions)
	for _, q := range ch.Questions {
		assert.NotEqual(t, "name", q.Name,
			"the Channel name is the bundle's, and other bundled CRs already read <name>-creds")
		assert.NotEqual(t, "agentclass", q.Name,
			"the bundle installs exactly one agent; there is nothing to choose")
	}

	require.Len(t, ch.Seeded, 2)
	assert.Equal(t, []oapInstallChannelSeed{
		{Key: "agentclass", Value: "demo-class", Source: "the AgentClass this bundle installs"},
		{Key: "name", Value: "demo-agent-slack", Source: "this bundle's requires.channels declaration"},
	}, ch.Seeded, "seeded values are REPORTED with their provenance, never offered as an editable default")

	// The AgentClass is the bundled CR's own name, not the manifest's agent
	// name — the fixture deliberately differs on the two, so a builder reading
	// the wrong one cannot pass the assertion above.
	assert.NotEqual(t, "demo-agent", ch.Seeded[0].Value)
}

// TestOapInstall_AKindNeedingABrowserRoundTripGetsItsUpFrontForm: github's
// setup is three Go closures (HandoffSpec) that no JSON body can carry, so the
// SPEC stays server-side — but the questions the round trip is described FROM
// are ordinary fields, and the row must carry them.
//
// It is marked channelFormHandoff and not channelFormAsk because the answers go
// somewhere else: to the route that begins the browser step, not to the one
// that creates the Channel outright.
func TestOapInstall_AKindNeedingABrowserRoundTripGetsItsUpFrontForm(t *testing.T) {
	a := newChannelFormAdmind(t, ctrlfake.NewClientBuilder().WithScheme(channelTestScheme(t)).Build())

	rec := postInstall(t, a,
		// The delivery sibling B-R14 requires beside an input declaration: an
		// input channel with no role=output one is refused before the form is
		// built, so a github-only bundle could not reach this route at all.
		packedChannelBundle(t,
			oap.RequiredChannel{Kind: "github", Role: "input", Name: "demo-agent-gh"},
			deliveryChannelDecl),
		map[string]string{"namespace": "demo-ns"})
	require.Equal(t, http.StatusBadRequest, rec.Code, "body: %s", rec.Body.String())

	var body oapInstallMissingQuestionsResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	ch := channelRowNamed(t, body, "demo-agent-gh")
	assert.Equal(t, channelFormHandoff, ch.Status)
	assert.Contains(t, ch.Reason, "browser")
	assert.Contains(t, ch.Remedy, channelHandoffPath, "the row must name the route its answers go to")
	assert.NotEmpty(t, ch.SetupToken, "a row with no token is a form with no submit button")

	names := make([]string, 0, len(ch.Questions))
	for _, q := range ch.Questions {
		names = append(names, q.Name)
	}
	assert.Contains(t, names, "org",
		"the organization goes into the address the browser is sent to; the round trip cannot be described without it")
	assert.NotContains(t, names, "private-key-path",
		"a fallback question is asked AFTER the callback, and only when the exchange did not answer it")
	assert.NotContains(t, names, "app-id", "same: the exchange mints it")
	assert.NotContains(t, names, "name", "the declared Channel name is seeded, not asked (B-R13)")
}

// TestOapInstall_NamedInstallOfAChannelDeclaringBundleIsRefused: admind's
// install path HAS an instance name, and --name plus a declared channel is the
// combination channelplan.RefuseNamedInstall exists to stop. It is refused
// before anything is applied and in that function's own words, not a second
// copy of them.
//
// EVERY QUESTION IS ANSWERED HERE, deliberately. The refusal must not be
// conditional on anything else about the request: reached only through the
// path that builds the channel form, it would refuse this combination when a
// question happens to be unanswered and install it when they all are. With the
// manifest question answered, the refusal is the only thing between this
// request and a write.
func TestOapInstall_NamedInstallOfAChannelDeclaringBundleIsRefused(t *testing.T) {
	c := ctrlfake.NewClientBuilder().WithScheme(channelTestScheme(t)).Build()
	a := newChannelFormAdmind(t, c)

	rec := postInstall(t, a,
		packedChannelBundle(t, oap.RequiredChannel{Kind: "slack", Role: "output", Name: "demo-agent-slack"}),
		map[string]string{
			"namespace": "demo-ns",
			"name":      "second",
			"values":    `{"greeting":"hello"}`,
		})
	require.Equal(t, http.StatusBadRequest, rec.Code, "body: %s", rec.Body.String())

	// Compared to the shared function's own output, not to a phrase copied out
	// of it: two wordings that could drift is the thing RefuseNamedInstall
	// being one function prevents.
	want := channelplan.RefuseNamedInstall("second",
		[]oap.RequiredChannel{{Kind: "slack", Role: "output", Name: "demo-agent-slack"}})
	require.Error(t, want, "the fixture must be a combination that IS refused")
	var body struct {
		Error string `json:"error"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, want.Error(), body.Error)

	var got spiceboxv1alpha1.AgentClass
	err := c.Get(context.Background(), client.ObjectKey{Namespace: "demo-ns", Name: "second-demo-class"}, &got)
	assert.Error(t, err, "nothing may be applied by a refused install")
}

// TestOapInstall_ABundleDeclaringNoChannelCarriesNoChannelBlock keeps the
// addition invisible to every bundle written before requires.channels existed.
func TestOapInstall_ABundleDeclaringNoChannelCarriesNoChannelBlock(t *testing.T) {
	a := newChannelFormAdmind(t, ctrlfake.NewClientBuilder().WithScheme(channelTestScheme(t)).Build())

	rec := postInstall(t, a, packedChannelBundle(t), map[string]string{"namespace": "demo-ns"})
	require.Equal(t, http.StatusBadRequest, rec.Code, "body: %s", rec.Body.String())

	var body oapInstallMissingQuestionsResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.NotEmpty(t, body.Questions)
	assert.Empty(t, body.Channels)
	assert.NotContains(t, rec.Body.String(), `"channels"`)
}

// TestOapInstall_AChannelAlreadyHeldBySomebodyElseIsReportedAsAConflict: the
// planner reports a Channel of the declared name bound to another agent, and
// the form must say so rather than show fields for a channel this install can
// never create (channelplan B-R11).
func TestOapInstall_AChannelAlreadyHeldBySomebodyElseIsReportedAsAConflict(t *testing.T) {
	existing := &spiceboxv1alpha1.Channel{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-agent-slack", Namespace: "demo-ns"},
		Spec:       spiceboxv1alpha1.ChannelSpec{Kind: "slack", AgentClass: "some-other-agent"},
	}
	a := newChannelFormAdmind(t, ctrlfake.NewClientBuilder().
		WithScheme(channelTestScheme(t)).WithObjects(existing).Build())

	rec := postInstall(t, a,
		packedChannelBundle(t, oap.RequiredChannel{Kind: "slack", Role: "output", Name: "demo-agent-slack"}),
		map[string]string{"namespace": "demo-ns"})
	require.Equal(t, http.StatusBadRequest, rec.Code, "body: %s", rec.Body.String())

	var body oapInstallMissingQuestionsResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Len(t, body.Channels, 1)
	ch := body.Channels[0]
	assert.Equal(t, channelFormConflict, ch.Status)
	assert.Empty(t, ch.Questions)
	assert.Contains(t, ch.Reason, "some-other-agent")
	// The remedy has to be a command that reproduces what this form would have
	// created, and the declared role is part of that: `oap channel create`
	// without --role leaves the Channel on ChannelSpec.Role's `both` default,
	// which is deliberately not an output-binding candidate. The row RENDERS
	// role: output right beside this string, so a remedy that could not express
	// it would be telling the operator to create something other than what is
	// on screen.
	assert.Equal(t, "output", ch.Role, "the fixture must declare the role this assertion is about")
	assert.Contains(t, ch.Remedy, "oap channel create --kind slack --name demo-agent-slack --namespace demo-ns --role output")
}

// TestOapInstall_ACredentialReadingAnUndeclaredChannelsSecretIsRefused is the
// motivating defect of the whole feature, on the path that had stopped
// checking for it.
//
// This endpoint called channelplan.RefuseNamedInstall and nothing else, so the
// credentials cross-check — the reason LintRequiredChannels exists — ran only
// under `oap agent lint` and `oap agent install`. A bundle whose AgentIdentity
// reads "demo-agent-github-creds" while requires.channels declares
// "demo-agent-gh" therefore installed cleanly from the console, rendered a
// form, wired whatever the operator answered, and left the agent reading a
// Secret nothing produces: every CR healthy, no token on the first inbound.
//
// The assertion that carries it is the pair of names in the body plus the
// ABSENCE of the AgentClass: a refusal that fired after install.Install would
// also return 400.
func TestOapInstall_ACredentialReadingAnUndeclaredChannelsSecretIsRefused(t *testing.T) {
	c := ctrlfake.NewClientBuilder().WithScheme(channelTestScheme(t)).Build()
	a := newChannelFormAdmind(t, c)

	rec := postInstall(t, a,
		// The delivery sibling keeps B-R14 out of this: the dangling
		// credential must be the ONLY thing wrong, or the assertions below
		// could be satisfied by a different finding.
		packedBundleWithCredential(t, "demo-agent-github-creds",
			oap.RequiredChannel{Kind: "github", Role: "input", Name: "demo-agent-gh"},
			deliveryChannelDecl),
		// Every question answered, so nothing but this check stands between
		// the request and a write.
		map[string]string{"namespace": "demo-ns", "values": `{"greeting":"hello"}`})

	require.Equal(t, http.StatusBadRequest, rec.Code, "body: %s", rec.Body.String())
	var body struct {
		Error string `json:"error"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Contains(t, body.Error, "demo-agent-github-creds",
		"the finding must name the Secret the credential reads")
	assert.Contains(t, body.Error, "demo-agent-gh",
		"and the channel name the manifest declares, because either one could be the typo")

	var got spiceboxv1alpha1.AgentClass
	err := c.Get(context.Background(), client.ObjectKey{Namespace: "demo-ns", Name: "demo-class"}, &got)
	assert.Error(t, err, "the refusal must precede the first cluster write")
}

// TestOapInstall_EveryDeclarationFindingIsRefusedHereNotOnlyTheNameOne covers
// the rest of what this endpoint had been skipping, as one table: each row is a
// declaration that `oap agent lint` has always rejected and this route used to
// install.
func TestOapInstall_EveryDeclarationFindingIsRefusedHereNotOnlyTheNameOne(t *testing.T) {
	cases := []struct {
		name     string
		declared []oap.RequiredChannel
		wantIn   string
	}{{
		name:     "role: monitoring, which binds to no agent (B-R12)",
		declared: []oap.RequiredChannel{{Kind: "slack", Role: "monitoring", Name: "demo-agent-slack"}},
		wantIn:   "cannot be declared in requires.channels",
	}, {
		name: "two declarations claiming one name, which would collapse to one Channel",
		declared: []oap.RequiredChannel{
			{Kind: "slack", Role: "output", Name: "demo-agent-slack"},
			{Kind: "slack", Role: "output", Name: "demo-agent-slack"},
		},
		wantIn: "is already declared by requires.channels[0]",
	}, {
		name:     "a name Kubernetes will not accept, which would fail at the apply",
		declared: []oap.RequiredChannel{{Kind: "slack", Role: "output", Name: "Demo_Agent_Slack"}},
		wantIn:   "is not a usable Channel name",
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := ctrlfake.NewClientBuilder().WithScheme(channelTestScheme(t)).Build()
			a := newChannelFormAdmind(t, c)

			rec := postInstall(t, a, packedChannelBundle(t, tc.declared...),
				map[string]string{"namespace": "demo-ns", "values": `{"greeting":"hello"}`})

			require.Equal(t, http.StatusBadRequest, rec.Code, "body: %s", rec.Body.String())
			assert.Contains(t, rec.Body.String(), tc.wantIn)

			var got spiceboxv1alpha1.AgentClass
			err := c.Get(context.Background(), client.ObjectKey{Namespace: "demo-ns", Name: "demo-class"}, &got)
			assert.Error(t, err, "the refusal must precede the first cluster write")
		})
	}
}

// --- the row builder ----------------------------------------------------------

// TestDeclaredChannelRow_NotSeededReasonsReachTheRowThatAsksTheQuestion proves
// the NotSeeded half reaches the form: when the cluster has published no
// external base URL, the reason is the whole of what the operator gets beside
// the question they are about to be asked. A reason for a key this kind never
// asks about is dropped — it would be noise on every response.
func TestDeclaredChannelRow_NotSeededReasonsReachTheRowThatAsksTheQuestion(t *testing.T) {
	p := planFixture(t, oap.RequiredChannel{Kind: "demo", Role: "input", Name: "demo-agent-x"}, nil, "")
	p.NotSeeded["external-base-url"] = "there is no ConfigMap ap-system/spicebox-webd-external-url"
	p.NotSeeded["never-asked"] = "a key no kind declares"

	row := declaredChannelRow(context.Background(), p, "demo-ns", wizards(stubWizard{questions: []oap.Question{
		{Name: "external-base-url", Type: oap.QString, Prompt: "Cluster's external base URL"},
	}}))

	require.Equal(t, channelFormAsk, row.Status)
	assert.Equal(t, []oapInstallChannelNote{
		{Key: "external-base-url", Reason: "there is no ConfigMap ap-system/spicebox-webd-external-url"},
	}, row.NotSeeded, "only a key this kind actually asks about is worth a reason")
}

// TestDeclaredChannelRow_SeededQuestionsAreDroppedNotPreFilled is the B-R13
// control at the unit level: the seeded question must not survive into
// Questions in ANY form — not as a Default either.
func TestDeclaredChannelRow_SeededQuestionsAreDroppedNotPreFilled(t *testing.T) {
	p := planFixture(t,
		oap.RequiredChannel{Kind: "demo", Role: "input", Name: "demo-agent-x"},
		map[string]string{"name": "demo-agent-x"},
		"this bundle's requires.channels declaration")

	row := declaredChannelRow(context.Background(), p, "demo-ns", wizards(stubWizard{questions: []oap.Question{
		{Name: "name", Type: oap.QString, Prompt: "Channel resource name", Default: "whatever-the-kind-guessed"},
		{Name: "channel-id", Type: oap.QString, Prompt: "Channel ID"},
	}}))

	require.Equal(t, channelFormAsk, row.Status)
	require.Len(t, row.Questions, 1)
	assert.Equal(t, "channel-id", row.Questions[0].Name,
		"a seeded question is DROPPED, exactly as channelwizard's renderer drops it — a Default is editable, and the declared name is not the operator's to change")
	assert.Equal(t, []oapInstallChannelSeed{
		{Key: "name", Value: "demo-agent-x", Source: "this bundle's requires.channels declaration"},
	}, row.Seeded)
}

// TestDeclaredChannelRow_ASeededSecretIsReportedWithoutItsValue keeps the
// "this body is logged" property STRUCTURAL rather than a claim about what
// today's planner happens to seed.
func TestDeclaredChannelRow_ASeededSecretIsReportedWithoutItsValue(t *testing.T) {
	p := planFixture(t,
		oap.RequiredChannel{Kind: "demo", Role: "input", Name: "demo-agent-x"},
		map[string]string{"bot-token": "xoxb-not-a-real-token"},
		"somewhere")

	row := declaredChannelRow(context.Background(), p, "demo-ns", wizards(stubWizard{questions: []oap.Question{
		{Name: "bot-token", Type: oap.QSecret, Prompt: "Bot token"},
	}}))

	require.Len(t, row.Seeded, 1)
	assert.Equal(t, "bot-token", row.Seeded[0].Key)
	assert.Empty(t, row.Seeded[0].Value, "a secret-typed answer never reaches a body that is logged")
	assert.True(t, row.Seeded[0].Redacted)
	assert.Equal(t, "somewhere", row.Seeded[0].Source, "the provenance is still worth having")

	blob, err := json.Marshal(row)
	require.NoError(t, err)
	assert.NotContains(t, string(blob), "xoxb-")
}

// TestDeclaredChannelRow_ASeededKeyNoKindAsksAboutIsNotReported: the plan
// seeds the external base URL for every declared channel, and most kinds never
// want it. "Answered for you, so you were not asked" about a question that was
// never going to be asked is a claim about a run that did not happen.
func TestDeclaredChannelRow_ASeededKeyNoKindAsksAboutIsNotReported(t *testing.T) {
	p := planFixture(t,
		oap.RequiredChannel{Kind: "demo", Role: "output", Name: "demo-agent-x"},
		map[string]string{"name": "demo-agent-x", "external-base-url": "https://demo.example"},
		"a source")

	row := declaredChannelRow(context.Background(), p, "demo-ns", wizards(stubWizard{questions: []oap.Question{
		{Name: "name", Type: oap.QString, Prompt: "Channel resource name"},
	}}))

	require.Len(t, row.Seeded, 1)
	assert.Equal(t, "name", row.Seeded[0].Key)
}

// TestDeclaredChannelRow_TheGuardOrderIsTheCLIsOwn pins that a verdict a form
// could not change is reached BEFORE the kind is ever consulted — the same
// order agentcmd.wireOne states, and for the same reason: a conflict rendered
// as a form is the silent cross-binding B-R11 exists to refuse.
func TestDeclaredChannelRow_TheGuardOrderIsTheCLIsOwn(t *testing.T) {
	rc := oap.RequiredChannel{Kind: "demo", Role: "input", Name: "demo-agent-x"}
	// A wizard whose Inputs REFUSES. Consulting it would land the row on
	// channelFormUnavailable carrying that sentence, so "no questions" cannot
	// pass here for the wrong reason — the Status assertion catches it.
	never := wizards(stubWizard{inputsErr: errors.New("the wizard must not be consulted here")})

	cases := []struct {
		name   string
		mutate func(p *channelplan.ChannelPlan)
		want   string
		reason string
	}{
		{
			name: "a Channel of this name that is not ours: conflict, not a form",
			mutate: func(p *channelplan.ChannelPlan) {
				p.Conflict = &channelplan.ChannelConflict{
					Kind: "slack", AgentClass: "other", Reason: "bound to agent \"other\"",
				}
			},
			want:   channelFormConflict,
			reason: `bound to agent "other"`,
		},
		{
			name:   "already ours: nothing to ask",
			mutate: func(p *channelplan.ChannelPlan) { p.AlreadyWired = true },
			want:   channelFormAlreadyWired,
		},
		{
			name:   "the planner never got to look: say so rather than offer to create one",
			mutate: func(p *channelplan.ChannelPlan) { p.WiringKnown = false },
			want:   channelFormUnknown,
			reason: "could not determine",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := planFixture(t, rc, map[string]string{"name": "demo-agent-x"}, "the declaration")
			tc.mutate(&p)
			row := declaredChannelRow(context.Background(), p, "demo-ns", never)
			assert.Equal(t, tc.want, row.Status)
			assert.Empty(t, row.Questions)
			assert.Empty(t, row.Seeded,
				"nothing was going to be asked, so nothing was answered on the operator's behalf")
			if tc.reason != "" {
				assert.Contains(t, row.Reason, tc.reason)
			}
		})
	}
}

// TestDeclaredChannelRow_AKindThisBuildDoesNotHaveIsNamed: the registry is the
// only authority on which kinds exist, and a declaration naming one this build
// never linked must be reported, never silently dropped from the form.
func TestDeclaredChannelRow_AKindThisBuildDoesNotHaveIsNamed(t *testing.T) {
	p := planFixture(t, oap.RequiredChannel{Kind: "nosuch", Role: "input", Name: "demo-agent-x"}, nil, "")
	row := declaredChannelRow(context.Background(), p, "demo-ns",
		func(string) (channelkinds.Wizard, bool) { return nil, false })

	assert.Equal(t, channelFormUnavailable, row.Status)
	assert.Contains(t, row.Reason, `"nosuch"`)
	assert.Empty(t, row.Questions)
}

// TestDeclaredChannelRow_AWizardThatRefusesIsReportedInItsOwnWords: a refusal
// reaches the operator as a reason, not as an empty form, from EITHER of the
// two calls this builder makes — which are separate branches and would
// otherwise be one tested and one not.
func TestDeclaredChannelRow_AWizardThatRefusesIsReportedInItsOwnWords(t *testing.T) {
	cases := []struct {
		name   string
		wiz    channelkinds.Wizard
		reason string
	}{
		{
			// UnavailableWizard refuses from Handoff, which is the first call.
			name:   "a kind whose Channels are built by another process: its own sentence",
			wiz:    channelkinds.UnavailableWizard("this kind's Channels are built by its own host process"),
			reason: "built by its own host process",
		},
		{
			name:   "a kind whose Inputs refuses: its own sentence",
			wiz:    stubWizard{inputsErr: errors.New("namespace is required")},
			reason: "namespace is required",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := planFixture(t, oap.RequiredChannel{Kind: "demo", Role: "input", Name: "demo-agent-x"}, nil, "")
			row := declaredChannelRow(context.Background(), p, "demo-ns", wizards(tc.wiz))

			assert.Equal(t, channelFormUnavailable, row.Status)
			assert.Contains(t, row.Reason, tc.reason)
			assert.Empty(t, row.Questions)
			assert.NotEmpty(t, row.Remedy, "a refusal with nothing to do about it is half an answer")
		})
	}
}

// TestDeclaredChannelRow_AnUnrenderableQuestionIsRefusedNotShipped: the same
// contract check channelwizard's renderer runs. A Binding is a bundle-install
// concept every renderer silently ignores, which is worse than absent.
func TestDeclaredChannelRow_AnUnrenderableQuestionIsRefusedNotShipped(t *testing.T) {
	p := planFixture(t, oap.RequiredChannel{Kind: "demo", Role: "input", Name: "demo-agent-x"}, nil, "")

	row := declaredChannelRow(context.Background(), p, "demo-ns", wizards(stubWizard{questions: []oap.Question{
		{Name: "q", Type: oap.QString, Prompt: "Q", Binding: []oap.Binding{{Target: "AgentClass/x#spec.y"}}},
	}}))

	assert.Equal(t, channelFormUnavailable, row.Status)
	assert.Contains(t, row.Reason, "binding")
	assert.Empty(t, row.Questions)
}

// TestDeclaredChannelRow_AKindThatOffersNoWizardIsReportedNotDereferenced:
// Kind.Wizard() returns an INTERFACE, and a kind that answered it with nothing
// would be dereferenced on the very next line. Reported instead.
//
// This covers a genuine nil interface. The neighbouring hazard — a TYPED-nil
// pointer, which compares non-nil here and panics anyway — is closed at the
// producer, by channelkinds.UnavailableWizard returning a value rather than a
// pointer; no consumer-side comparison can catch it.
func TestDeclaredChannelRow_AKindThatOffersNoWizardIsReportedNotDereferenced(t *testing.T) {
	p := planFixture(t, oap.RequiredChannel{Kind: "demo", Role: "input", Name: "demo-agent-x"}, nil, "")
	row := declaredChannelRow(context.Background(), p, "demo-ns",
		func(string) (channelkinds.Wizard, bool) { return nil, true })

	assert.Equal(t, channelFormUnavailable, row.Status)
	assert.Contains(t, row.Reason, "no setup flow")
}

// --- the AgentClass the channels bind to -------------------------------------

// TestBundleAgentClassName reads the bundled CR's own name, which is what the
// Channel binds to — NOT the manifest's agent.name, which is metadata and
// routinely differs.
func TestBundleAgentClassName(t *testing.T) {
	b := &oap.Bundle{
		Manifest: &oap.Manifest{Agent: oap.Agent{Name: "demo-agent"}},
		Manifests: []byte("apiVersion: agentprimitives.authzed.com/v1alpha1\n" +
			"kind: AgentClass\nmetadata:\n  name: demo-class\nspec: {}\n"),
	}
	got, err := bundleAgentClassName(b)
	require.NoError(t, err)
	assert.Equal(t, "demo-class", got)

	none := &oap.Bundle{
		Manifest:  &oap.Manifest{Agent: oap.Agent{Name: "demo-agent"}},
		Manifests: []byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: x\n"),
	}
	_, err = bundleAgentClassName(none)
	require.Error(t, err, "a channel with no agent to bind to must be loud, not bound to \"\"")
	assert.Contains(t, err.Error(), "AgentClass")
}

// TestDeclaredChannelForm_AFailedPlanIsSurfacedNotSwallowed: a cluster read
// this endpoint cannot make must reach the operator as a warning on the very
// response they are waiting for.
func TestDeclaredChannelForm_AFailedPlanIsSurfacedNotSwallowed(t *testing.T) {
	a := newChannelFormAdmind(t, ctrlfake.NewClientBuilder().WithScheme(channelTestScheme(t)).Build())
	b := &oap.Bundle{
		Manifest: &oap.Manifest{
			OapFormatVersion: "1",
			Agent:            oap.Agent{Name: "demo-agent", Version: "1.0.0"},
			Requires: oap.Requires{Channels: []oap.RequiredChannel{
				{Kind: "slack", Role: "output", Name: "demo-agent-slack"},
			}},
		},
		Manifests: []byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: x\n"),
	}

	rows, warnings := a.declaredChannelForm(context.Background(), b, "demo-ns", "", identity.CanonicalFromTrusted("YWRtaW4", "test fixture"))
	assert.Empty(t, rows)
	require.Len(t, warnings, 1)
	assert.Contains(t, warnings[0], "AgentClass")
}
