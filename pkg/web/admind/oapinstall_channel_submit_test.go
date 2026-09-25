package admind

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr/testr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/wizardrun"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/platform/oap"

	// The bento kind is this file's fixture wizard: it declares five real
	// questions, honors the Channel name and AgentClass it is handed, derives
	// its Secret's name from the Channel's, and reaches no network from any of
	// its four methods. Registered the way internal/cmd/operator's main
	// registers every kind.
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/bento"

	// The fake kind is the second fixture, for the two properties bento
	// structurally cannot exhibit: its Result sets NO role (so the declared
	// one is the only thing that can reach the Channel), and it fixes its own
	// Channel name whatever it is seeded (so the declared name and the created
	// one differ). See rolelessKindBundle.
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/fake"
)

// --- fixtures ----------------------------------------------------------------

// noNetworkDialer is installed for the whole package's test run: nothing in
// this file may reach anything but loopback, and a route that quietly grew an
// outbound call would otherwise pass in CI and fail in an air-gapped one.
//
// It panics rather than erroring, so a swallowed error cannot hide the reach.
func init() {
	http.DefaultTransport.(*http.Transport).DialContext = func(_ context.Context, network, addr string) (net.Conn, error) {
		host, _, err := net.SplitHostPort(addr)
		if err != nil {
			panic("a test dialled " + addr + " (" + network + ") — no test in this package may reach the network")
		}
		if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
			panic("a test dialled " + addr + " — no test in this package may reach a non-loopback address")
		}
		return (&net.Dialer{}).DialContext(context.Background(), network, addr)
	}
}

// bentoBundle is a valid .oap declaring one bento channel and one required
// manifest question with no default — so a POST that answers nothing lands on
// the missing-questions 400 that mints the setup token.
func bentoBundle(t *testing.T) []byte {
	t.Helper()
	return packedChannelBundle(t,
		oap.RequiredChannel{
			Kind: "bento", Role: "input", Name: "demo-agent-bento",
			Purpose: "Where this agent's scheduled runs come from.",
		},
		// The delivery target the bento channel needs. B-R14 refuses a bundle
		// declaring an input channel and no role=output one — bento is a cron
		// trigger with no rendering surface, so a bento-only bundle describes
		// an agent that is woken up and can answer nowhere, and the install
		// route now refuses it before the form is ever built.
		//
		// `fake` rather than slack: it asks nothing and derives nothing, so the
		// row it adds cannot interact with what any test here submits. Every
		// helper below therefore selects the bento row BY NAME rather than by
		// index.
		deliveryChannelDecl)
}

// deliveryChannelDecl is the spare role=output declaration that satisfies
// B-R14 for a fixture whose subject is an input channel. See bentoBundle.
var deliveryChannelDecl = oap.RequiredChannel{
	Kind: "fake", Role: "output", Name: "demo-agent-out",
	Purpose: "Where this agent's work is delivered.",
}

// channelRowNamed picks one declared-channel row out of an install response,
// by name.
//
// By name and not by index, because more than one channel is declared now: an
// index would silently start asserting about the delivery sibling the moment
// declaration order changed, and the row it returned would be a valid row of
// the wrong channel — the kind of green test this branch has spent its review
// budget on.
func channelRowNamed(t *testing.T, body oapInstallMissingQuestionsResponse, name string) oapInstallChannel {
	t.Helper()
	for _, ch := range body.Channels {
		if ch.Name == name {
			return ch
		}
	}
	require.FailNowf(t, "no such declared channel", "no row named %q in %d rows", name, len(body.Channels))
	return oapInstallChannel{}
}

// installedCluster is the cluster as it stands when a channel is actually set
// up: the agent's own AgentClass is there, because the install that declared
// the channel has already run. bento's AgentClass question verifies its seeded
// class against the live listing, so a cluster without it is the wrong fixture
// — the route would fail for a reason the test did not intend.
func installedCluster(t *testing.T, extra ...client.Object) client.Client {
	t.Helper()
	objs := append([]client.Object{&spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-class", Namespace: "demo-ns"},
	}}, extra...)
	return ctrlfake.NewClientBuilder().WithScheme(channelTestScheme(t)).WithObjects(objs...).Build()
}

// manyAdminsChecker grants every permission to several canonical subjects, for
// the one property a single-admin fixture cannot express: two people who BOTH
// hold install_agent are indistinguishable to the permission check, so a
// pending setup's owner has to be checked separately.
type manyAdminsChecker struct{ canonical map[string]bool }

func (c manyAdminsChecker) CheckPlatformPermission(_ context.Context, _ string, id identity.CanonicalUserID, _ bool) (bool, error) {
	return c.canonical[id.String()], nil
}

func (c manyAdminsChecker) ListPlatformAdmins(context.Context) ([]string, error) { return nil, nil }

func (c manyAdminsChecker) CheckAgentIdentityUpdateCredential(context.Context, string, string, identity.CanonicalUserID) (bool, error) {
	return false, nil
}

// newChannelFormAdmindFor is newChannelFormAdmind with more than one admin.
func newChannelFormAdmindFor(t *testing.T, c client.Client, canonicals ...string) *Admind {
	t.Helper()
	allowed := make(map[string]bool, len(canonicals))
	for _, s := range canonicals {
		allowed[s] = true
	}
	a, err := New(Config{
		Mem:             memory.NewLocal(inmem.NewBackend()),
		K8s:             c,
		Checker:         manyAdminsChecker{canonical: allowed},
		Token:           "test-token",
		Logger:          testr.New(t),
		MetadataBaseURL: "http://127.0.0.1:1",
	})
	require.NoError(t, err)
	return a
}

// setupTokenFor drives the REAL install route to get the token the UI would
// have, rather than reaching into the store: the token's whole purpose is to
// join two requests, and a test that minted its own would prove nothing about
// the join.
func setupTokenFor(t *testing.T, a *Admind, packed []byte, name string) string {
	t.Helper()
	rec := postInstall(t, a, packed, map[string]string{"namespace": "demo-ns"})
	require.Equal(t, http.StatusBadRequest, rec.Code, "body: %s", rec.Body.String())

	var body oapInstallMissingQuestionsResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	row := channelRowNamed(t, body, name)
	require.Equal(t, channelFormAsk, row.Status, "reason: %s", row.Reason)
	require.NotEmpty(t, row.SetupToken,
		"a form the UI can render and cannot submit is the defect this route exists to close")
	return row.SetupToken
}

// postChannelSetup POSTs to the real route through the real handler chain,
// with the headers webd's proxy injects. subject "" omits the header
// altogether, which is what an unauthenticated caller looks like.
func postChannelSetup(t *testing.T, a *Admind, token, subject string, answers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(channelSetupRequest{SetupToken: token, Answers: answers})
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodPost, channelSetupPath, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer test-token")
	if subject != "" {
		req.Header.Set("X-Admin-Subject", subject)
	}
	rec := httptest.NewRecorder()
	a.Handler().ServeHTTP(rec, req)
	return rec
}

// noChannel asserts nothing of that name exists. Used wherever a refusal must
// be proved to have refused, not merely to have returned a non-200.
func noChannel(t *testing.T, c client.Client, namespace, name string) {
	t.Helper()
	var got spiceboxv1alpha1.Channel
	err := c.Get(context.Background(), client.ObjectKey{Namespace: namespace, Name: name}, &got)
	require.Error(t, err, "a refused setup must not have created Channel %q", name)
	assert.True(t, apierrors.IsNotFound(err), "unexpected error reading back Channel %q: %v", name, err)
}

// --- the submit path ----------------------------------------------------------

// TestChannelSetup_TheAnswersTheFormCollectedCreateTheChannel is this task's
// headline: the form the install response renders now has somewhere to be
// submitted, and submitting it produces the Channel and its Secret.
func TestChannelSetup_TheAnswersTheFormCollectedCreateTheChannel(t *testing.T) {
	c := installedCluster(t)
	a := newChannelFormAdmind(t, c)

	token := setupTokenFor(t, a, bentoBundle(t), "demo-agent-bento")
	rec := postChannelSetup(t, a, token, "user:YWRtaW4", map[string]string{
		"authzsubject": "service:demo-bot",
		"interval":     "@every 24h",
	})
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	var body channelSetupResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, "demo-agent-bento", body.Name)
	assert.Equal(t, "demo-ns", body.Namespace)
	assert.Equal(t, "bento", body.Kind)
	assert.Equal(t, "demo-class", body.AgentClass)
	assert.Equal(t, "demo-agent-bento-creds", body.SecretName)

	var got spiceboxv1alpha1.Channel
	require.NoError(t, c.Get(context.Background(),
		client.ObjectKey{Namespace: "demo-ns", Name: "demo-agent-bento"}, &got),
		"the Channel the bundle declared must exist under the declared name")
	assert.Equal(t, "bento", got.Spec.Kind)
	assert.Equal(t, "demo-class", got.Spec.AgentClass,
		"the AgentClass is the bundle's own, seeded server-side and never sent by the form")
	assert.Equal(t, "service:demo-bot", got.Spec.AuthzSubject, "the operator's answer must reach the manifests")
	require.NotNil(t, got.Spec.Bento)
	require.NotNil(t, got.Spec.Bento.Generate)
	assert.Equal(t, "@every 24h", got.Spec.Bento.Generate.Interval)
	assert.NotEmpty(t, got.Spec.Bento.Generate.Mapping,
		"an unanswered question with a Default must be preseeded exactly as the terminal preseeds it")

	var sec corev1.Secret
	require.NoError(t, c.Get(context.Background(),
		client.ObjectKey{Namespace: "demo-ns", Name: "demo-agent-bento-creds"}, &sec),
		"the credentials Secret the bundled AgentIdentity reads is named after the DECLARED channel")
	assert.Equal(t, "ap", got.Annotations[wizardrun.InstalledByAnnotation],
		"`oap clean` must recognise a Channel the admin UI created")
	// The field manager the apply claims is asserted where it is DECIDED —
	// wizardrun.TestApply_TheCapabilityPatchGoesLastAndUnderItsOwnManager, over
	// a recording applier. The controller-runtime fake client does not track
	// managedFields, so an assertion here would be one about the fake.
}

// TestChannelSetup_TheDeclaredNameIsNotTheFormsToChange is B-R13 made
// structural, and it is the reason this route takes a token rather than a
// namespace and a name.
//
// The dangling-credential defect it prevents is invisible to
// channelplan.LintRequiredChannels, which reads the BUNDLE: every bundled CR is
// written against "<declared-name>-creds", and a form field that could rename
// the Channel renames the Secret with it. Refused by name rather than ignored,
// because a silently-dropped value is indistinguishable from an accepted one.
func TestChannelSetup_TheDeclaredNameIsNotTheFormsToChange(t *testing.T) {
	cases := []struct {
		name string
		key  string
		val  string
	}{
		{name: "the Channel name: other bundled CRs already read <name>-creds", key: "name", val: "attacker-chosen"},
		{name: "the AgentClass: the bundle installs exactly one agent", key: "agentclass", val: "some-other-agent"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := installedCluster(t)
			a := newChannelFormAdmind(t, c)
			token := setupTokenFor(t, a, bentoBundle(t), "demo-agent-bento")

			rec := postChannelSetup(t, a, token, "user:YWRtaW4", map[string]string{
				"authzsubject": "service:demo-bot",
				tc.key:         tc.val,
			})
			require.Equal(t, http.StatusBadRequest, rec.Code, "body: %s", rec.Body.String())
			assert.Contains(t, rec.Body.String(), tc.key)

			noChannel(t, c, "demo-ns", tc.val)
			noChannel(t, c, "demo-ns", "demo-agent-bento")
		})
	}
}

// TestChannelSetup_AnAnswerNoQuestionAsksForIsRefused mirrors the refusal
// `oap channel create` makes for an unknown --answer: a typo'd key that is
// quietly dropped looks exactly like one that was accepted.
func TestChannelSetup_AnAnswerNoQuestionAsksForIsRefused(t *testing.T) {
	c := installedCluster(t)
	a := newChannelFormAdmind(t, c)
	token := setupTokenFor(t, a, bentoBundle(t), "demo-agent-bento")

	rec := postChannelSetup(t, a, token, "user:YWRtaW4", map[string]string{
		"authzsubject": "service:demo-bot",
		"intervall":    "@every 24h",
	})
	require.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "intervall")
	assert.Contains(t, rec.Body.String(), "interval", "the refusal names what the kind DOES ask for")
	noChannel(t, c, "demo-ns", "demo-agent-bento")
}

// TestChannelSetup_ANameTakenSinceTheFormWasRenderedIsRefused: the verdict the
// install response reported was true when it was reported. Re-derived here
// because a Channel can appear, or be bound to another agent, between the two
// requests — and this apply is a server-side apply, which would merge over it.
func TestChannelSetup_ANameTakenSinceTheFormWasRenderedIsRefused(t *testing.T) {
	c := installedCluster(t)
	a := newChannelFormAdmind(t, c)
	token := setupTokenFor(t, a, bentoBundle(t), "demo-agent-bento")

	// Somebody else's Channel takes the name AFTER the form was rendered. Same
	// KIND, deliberately: the kind mismatch is checked first, so a slack
	// Channel here would pass this test on the wrong branch and never exercise
	// the cross-binding check (channelplan B-R11) that the wrong AGENT is.
	require.NoError(t, c.Create(context.Background(), &spiceboxv1alpha1.Channel{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-agent-bento", Namespace: "demo-ns"},
		Spec:       spiceboxv1alpha1.ChannelSpec{Kind: "bento", AgentClass: "some-other-agent"},
		TypeMeta:   metav1.TypeMeta{},
	}))

	rec := postChannelSetup(t, a, token, "user:YWRtaW4", map[string]string{"authzsubject": "service:demo-bot"})
	require.Equal(t, http.StatusConflict, rec.Code, "body: %s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "some-other-agent")

	var got spiceboxv1alpha1.Channel
	require.NoError(t, c.Get(context.Background(),
		client.ObjectKey{Namespace: "demo-ns", Name: "demo-agent-bento"}, &got))
	assert.Equal(t, "some-other-agent", got.Spec.AgentClass, "the other agent's Channel must be untouched")
	assert.Empty(t, got.Spec.AuthzSubject, "nothing this request answered may have reached it")
}

// TestChannelSetup_AChannelAlreadyOursIsANoOpNotAFailure: re-submitting the
// same form must be safe, which is what makes a UI that retries harmless.
func TestChannelSetup_AChannelAlreadyOursIsANoOpNotAFailure(t *testing.T) {
	c := installedCluster(t)
	a := newChannelFormAdmind(t, c)
	token := setupTokenFor(t, a, bentoBundle(t), "demo-agent-bento")

	require.NoError(t, c.Create(context.Background(), &spiceboxv1alpha1.Channel{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-agent-bento", Namespace: "demo-ns"},
		Spec:       spiceboxv1alpha1.ChannelSpec{Kind: "bento", AgentClass: "demo-class"},
	}))

	rec := postChannelSetup(t, a, token, "user:YWRtaW4", nil)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	var body channelSetupResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.True(t, body.AlreadyWired)
}

// TestChannelSetup_ARequiredAnswerNobodySuppliedIsReportedAsAForm: the same
// shape the install route reports a missing manifest question in, so the UI
// re-renders the fields rather than parsing prose.
//
// A STUB KIND rather than bento, because bento has no question this fixture can
// leave unanswered: the plan seeds its AgentClass, which makes its remaining
// defaults unambiguous, so every one of its five questions arrives with a
// Default. A test that used it would assert an empty list and pass whatever the
// route did.
func TestChannelSetup_ARequiredAnswerNobodySuppliedIsReportedAsAForm(t *testing.T) {
	c := installedCluster(t)
	a := newChannelFormAdmind(t, c)
	a.wizardFor = wizards(stubWizard{questions: []oap.Question{
		// Required is nil on the first two: oap.Question.IsRequired defaults to
		// true, and spelling it out would test a field this route does not read.
		{Name: "bot-token", Type: oap.QSecret, Prompt: "Bot token"},
		{Name: "channel-id", Type: oap.QString, Prompt: "Channel ID"},
		{Name: "optional-note", Type: oap.QString, Prompt: "A note", Required: new(false)},
	}})
	token := setupTokenFor(t, a, bentoBundle(t), "demo-agent-bento")

	// channel-id answered with the empty string, which the UI writes for a
	// field the operator cleared — it must count as unanswered, not as an
	// answer, or the blank reaches Result and lands on the manifests.
	rec := postChannelSetup(t, a, token, "user:YWRtaW4", map[string]string{"channel-id": ""})
	require.Equal(t, http.StatusBadRequest, rec.Code, "body: %s", rec.Body.String())

	var body oapInstallMissingQuestionsResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.NotEmpty(t, body.Questions, "the UI cannot re-render a field it was not told about")
	names := make([]string, 0, len(body.Questions))
	for _, q := range body.Questions {
		names = append(names, q.Name)
	}
	assert.Equal(t, []string{"bot-token", "channel-id"}, names)
	assert.NotContains(t, names, "optional-note", "an optional question is not owed an answer")
	noChannel(t, c, "demo-ns", "demo-agent-bento")
}

// TestChannelSetup_ARequiredQuestionTheAnswersGateOutIsNotOwed is the other
// side of the rule above: a required question a kind declares BUT DOES NOT ASK
// on the route these answers took is not an answer the operator owes.
//
// A branching question set — oap.Question.AskWhen — declares every route's
// questions in one batch so that each stays seedable, and lets the answers
// decide which are put to anyone. A form that read required-ness without the
// gate would demand, on this shape, exactly the credential the chosen route
// exists to mint: the operator says "create the app for me", and the route
// blocks them on the bot token they do not have and are not going to type.
//
// The stub's Result produces no manifests, so the 200 here means "the route
// asked for nothing more", which is precisely the claim.
func TestChannelSetup_ARequiredQuestionTheAnswersGateOutIsNotOwed(t *testing.T) {
	c := installedCluster(t)
	a := newChannelFormAdmind(t, c)
	a.wizardFor = wizards(stubWizard{questions: []oap.Question{
		{Name: "route", Type: oap.QEnum, Prompt: "Do you have an app?", Enum: []string{"have", "provision"}, Default: "have"},
		{Name: "bot-token", Type: oap.QSecret, Prompt: "Bot token",
			AskWhen: oap.AskWhen{Question: "route", In: []string{"have"}}},
		{Name: "config-token", Type: oap.QSecret, Prompt: "Configuration token",
			AskWhen: oap.AskWhen{Question: "route", In: []string{"provision"}}},
	}})
	token := setupTokenFor(t, a, bentoBundle(t), "demo-agent-bento")

	rec := postChannelSetup(t, a, token, "user:YWRtaW4", map[string]string{
		"route":        "provision",
		"config-token": "xoxe-demo-config-token",
	})

	require.Equal(t, http.StatusOK, rec.Code,
		"the bot token belongs to the route this operator did not take, so nothing is still owed; body: %s", rec.Body.String())
	assert.NotContains(t, rec.Body.String(), "bot-token",
		"a question the answers gate out must not come back as a field to fill in")
}

// --- the gate -----------------------------------------------------------------

// TestChannelSetup_IsGatedExactlyLikeTheInstallItServes. Creating the Channel
// an installed agent needs, and minting the Secret holding its credentials, is
// part of installing that agent — so it must not be reachable by anyone the
// install endpoint would refuse.
//
// Every arm asserts the cluster is UNTOUCHED as well as the status code: a
// route that wrote first and refused afterwards would return the same code.
func TestChannelSetup_IsGatedExactlyLikeTheInstallItServes(t *testing.T) {
	cases := []struct {
		name    string
		subject string
		want    int
	}{
		{
			// Two guards refuse this independently — `require`'s header check
			// and channelSetupOwner's — so this arm alone does NOT prove the
			// route is mounted behind the middleware. The arm below does: with
			// `require` removed it flips to 404, because the only thing left
			// refusing is the pending setup's owner check.
			name:    "no forwarded subject: 401, the same as the install endpoint",
			subject: "",
			want:    http.StatusUnauthorized,
		},
		{
			name:    "a subject without install_agent: 403, and nothing written",
			subject: "user:c29tZWJvZHk",
			want:    http.StatusForbidden,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := installedCluster(t)
			a := newChannelFormAdmind(t, c)
			token := setupTokenFor(t, a, bentoBundle(t), "demo-agent-bento")

			rec := postChannelSetup(t, a, token, tc.subject, map[string]string{"authzsubject": "service:demo-bot"})
			assert.Equal(t, tc.want, rec.Code, "body: %s", rec.Body.String())
			noChannel(t, c, "demo-ns", "demo-agent-bento")
		})
	}
}

// TestChannelSetup_ATokenIsNotTransferableBetweenOperators. Two people can both
// hold install_agent, so the platform permission cannot tell them apart — and a
// pending setup is one operator's capability, carrying a declaration and (for a
// handoff) a live CSRF nonce.
func TestChannelSetup_ATokenIsNotTransferableBetweenOperators(t *testing.T) {
	c := installedCluster(t)
	a := newChannelFormAdmindFor(t, c, "YWRtaW4", "YWRtaW4y")
	token := setupTokenFor(t, a, bentoBundle(t), "demo-agent-bento")

	rec := postChannelSetup(t, a, token, "user:YWRtaW4y", map[string]string{"authzsubject": "service:demo-bot"})
	require.Equal(t, http.StatusNotFound, rec.Code,
		"the second admin holds install_agent and must still not drive the first admin's pending setup")
	noChannel(t, c, "demo-ns", "demo-agent-bento")

	// The owner's own token still works, so the refusal above is about WHO
	// asked and not about the token having been consumed.
	ok := postChannelSetup(t, a, token, "user:YWRtaW4", map[string]string{"authzsubject": "service:demo-bot"})
	require.Equal(t, http.StatusOK, ok.Code, "body: %s", ok.Body.String())
}

// TestChannelSetup_AnUnknownOrExpiredTokenAnswersIdentically. A token that
// never existed and one that has aged out must be indistinguishable: telling a
// holder which of the two it is tells them something only a legitimate holder
// should know, and the action is the same either way.
func TestChannelSetup_AnUnknownOrExpiredTokenAnswersIdentically(t *testing.T) {
	c := installedCluster(t)
	a := newChannelFormAdmind(t, c)
	token := setupTokenFor(t, a, bentoBundle(t), "demo-agent-bento")

	// Age the store past its TTL without sleeping.
	a.channelSetups.now = func() time.Time { return time.Now().Add(2 * channelSetupTTL) }

	expired := postChannelSetup(t, a, token, "user:YWRtaW4", map[string]string{"authzsubject": "service:demo-bot"})
	unknown := postChannelSetup(t, a, "not-a-token", "user:YWRtaW4", map[string]string{"authzsubject": "service:demo-bot"})

	assert.Equal(t, http.StatusNotFound, expired.Code)
	assert.Equal(t, http.StatusNotFound, unknown.Code)
	assert.Equal(t, expired.Body.String(), unknown.Body.String(),
		"the two must be one answer, or the difference is an oracle")
	noChannel(t, c, "demo-ns", "demo-agent-bento")
}

// --- the store ----------------------------------------------------------------

// TestChannelSetupStore_ARepeatedFormRenderReusesOneToken. The install form is
// re-rendered on every 400 the operator corrects a manifest answer on; a token
// per render would grow the store without any of them being a distinct thing to
// complete.
func TestChannelSetupStore_ARepeatedFormRenderReusesOneToken(t *testing.T) {
	a := newChannelFormAdmind(t, installedCluster(t))
	packed := bentoBundle(t)

	first := setupTokenFor(t, a, packed, "demo-agent-bento")
	second := setupTokenFor(t, a, packed, "demo-agent-bento")
	assert.Equal(t, first, second)
	assert.Len(t, a.channelSetups.byToken, 2,
		"one per declared channel — the bento one and its delivery sibling — and no more, however many times the form is rendered")
}

// TestChannelSetupStore_TheBoundRefusesRatherThanEvictingSomebodysLiveSetup.
// Evicting under pressure would let one operator's form-rendering loop silently
// invalidate another's in-flight handoff — an App already created, with no
// record of it anywhere. A loud refusal on a bound no real install approaches is
// the better failure.
func TestChannelSetupStore_TheBoundRefusesRatherThanEvictingSomebodysLiveSetup(t *testing.T) {
	s := newChannelSetupStore()
	s.max = 2

	for i := range s.max {
		_, err := s.put(&pendingChannelSetup{
			owner: identity.CanonicalFromTrusted("YWRtaW4", "test fixture"), namespace: "demo-ns", agentClass: "demo-class",
			required: oap.RequiredChannel{Kind: "bento", Name: fmt.Sprintf("demo-channel-%d", i)},
		})
		require.NoError(t, err)
	}
	held := make([]string, 0, len(s.byToken))
	for tok := range s.byToken {
		held = append(held, tok)
	}

	_, err := s.put(&pendingChannelSetup{
		owner: identity.CanonicalFromTrusted("YWRtaW4", "test fixture"), namespace: "demo-ns", agentClass: "demo-class",
		required: oap.RequiredChannel{Kind: "bento", Name: "demo-channel-overflow"},
	})
	require.ErrorIs(t, err, errTooManyPendingSetups)
	for _, tok := range held {
		_, err := s.get(tok, identity.CanonicalFromTrusted("YWRtaW4", "test fixture"))
		assert.NoError(t, err, "an existing setup must survive the refusal of a new one")
	}
}

// TestChannelSetupStore_AnExpiredEntryFreesItsSlot: the bound is on LIVE
// setups, so a store full of aged-out entries must not refuse a new one.
func TestChannelSetupStore_AnExpiredEntryFreesItsSlot(t *testing.T) {
	s := newChannelSetupStore()
	s.max = 1
	_, err := s.put(&pendingChannelSetup{
		owner: identity.CanonicalFromTrusted("YWRtaW4", "test fixture"), namespace: "demo-ns", agentClass: "demo-class",
		required: oap.RequiredChannel{Kind: "bento", Name: "demo-channel-old"},
	})
	require.NoError(t, err)

	s.now = func() time.Time { return time.Now().Add(2 * channelSetupTTL) }
	_, err = s.put(&pendingChannelSetup{
		owner: identity.CanonicalFromTrusted("YWRtaW4", "test fixture"), namespace: "demo-ns", agentClass: "demo-class",
		required: oap.RequiredChannel{Kind: "bento", Name: "demo-channel-new"},
	})
	require.NoError(t, err)
	assert.Len(t, s.byToken, 1)
}

func graphChannelBinding() channelSetupBinding {
	return channelSetupBinding{
		RootDigest: "sha256:root", RootNamespace: "agents", RootInstall: "captain", AgentPath: "reviewer",
		AgentClass: "captain-reviewer",
		Required:   oap.RequiredChannel{Kind: "bento", Role: "output", Name: "captain-reviewer-bento"},
	}
}

func completeGraphForTest(t *testing.T, s *channelSetupStore, token string, owner identity.CanonicalUserID, binding channelSetupBinding, staged stagedChannelSetup) {
	t.Helper()
	_, err := s.beginGraphResolve(token, owner)
	require.NoError(t, err)
	require.NoError(t, s.completeGraph(token, owner, binding, staged))
}

func TestChannelSetupStore_CompletedGraphSetupRequiresExactOwnerAndBinding(t *testing.T) {
	s := newChannelSetupStore()
	owner := identity.CanonicalFromTrusted("YWRtaW4", "test fixture")
	other := identity.CanonicalFromTrusted("b3RoZXI", "test fixture")
	binding := graphChannelBinding()
	token, err := s.put(&pendingChannelSetup{owner: owner, graphBinding: &binding})
	require.NoError(t, err)
	completeGraphForTest(t, s, token, owner, binding, stagedChannelSetup{})

	_, err = s.claimGraph(token, other, binding)
	require.ErrorIs(t, err, errNoPendingSetup)
	mismatch := binding
	mismatch.AgentPath = "other-child"
	_, err = s.claimGraph(token, owner, mismatch)
	require.ErrorIs(t, err, errNoPendingSetup)
	mismatch = binding
	mismatch.RootNamespace = "other-namespace"
	_, err = s.claimGraph(token, owner, mismatch)
	require.ErrorIs(t, err, errNoPendingSetup)

	claimed, err := s.claimGraph(token, owner, binding)
	require.NoError(t, err)
	assert.Equal(t, binding, claimed.binding)
}

func TestChannelSetupStore_IncompleteAndExpiredGraphSetupsCannotBeClaimed(t *testing.T) {
	owner := identity.CanonicalFromTrusted("YWRtaW4", "test fixture")
	binding := graphChannelBinding()

	t.Run("incomplete", func(t *testing.T) {
		s := newChannelSetupStore()
		token, err := s.put(&pendingChannelSetup{owner: owner, graphBinding: &binding})
		require.NoError(t, err)
		_, err = s.claimGraph(token, owner, binding)
		require.ErrorIs(t, err, errPendingSetupIncomplete)
	})

	t.Run("expired cleans prerequisite", func(t *testing.T) {
		s := newChannelSetupStore()
		var expire func()
		s.schedule = func(_ time.Duration, fn func()) func() {
			expire = fn
			return func() {}
		}
		var cleanups atomic.Int32
		token, err := s.put(&pendingChannelSetup{owner: owner, graphBinding: &binding})
		require.NoError(t, err)
		completeGraphForTest(t, s, token, owner, binding, stagedChannelSetup{
			rollbackPrerequisite: func(context.Context) error { cleanups.Add(1); return nil },
		})
		require.NotNil(t, expire)
		expire()
		require.Eventually(t, func() bool { return cleanups.Load() == 1 }, time.Second, time.Millisecond)
		_, err = s.claimGraph(token, owner, binding)
		require.ErrorIs(t, err, errNoPendingSetup)
		assert.Equal(t, int32(1), cleanups.Load())
	})

	t.Run("expired cleanup failure is surfaced", func(t *testing.T) {
		s := newChannelSetupStore()
		var expire func()
		s.schedule = func(_ time.Duration, fn func()) func() {
			expire = fn
			return func() {}
		}
		reported := make(chan error, 1)
		s.onCleanupError = func(err error) { reported <- err }
		const cleanupSecret = "cleanup-provider-secret"
		cleanupErr := errors.New("guarded prerequisite cleanup failed for " + cleanupSecret)
		token, err := s.put(&pendingChannelSetup{owner: owner, graphBinding: &binding})
		require.NoError(t, err)
		completeGraphForTest(t, s, token, owner, binding, stagedChannelSetup{
			sensitiveValues:      []string{cleanupSecret},
			rollbackPrerequisite: func(context.Context) error { return cleanupErr },
		})
		expire()
		select {
		case got := <-reported:
			assert.NotContains(t, got.Error(), cleanupSecret)
			assert.Contains(t, got.Error(), "redacted")
			assert.NotErrorIs(t, got, cleanupErr, "cleanup causes are severed because a provider may retain plaintext answers")
		case <-time.After(time.Second):
			t.Fatal("expiry cleanup error was silently dropped")
		}
	})
}

func TestChannelSetupStore_CleanupErrorsUseTokenLongestFirstRedactorAndSeverCause(t *testing.T) {
	s := newChannelSetupStore()
	owner := identity.CanonicalFromTrusted("YWRtaW4", "test fixture")
	binding := graphChannelBinding()
	token, err := s.put(&pendingChannelSetup{owner: owner, graphBinding: &binding})
	require.NoError(t, err)
	rec, err := s.beginGraphResolve(token, owner)
	require.NoError(t, err)
	rec.redactor.register(map[string]string{"short": "token", "long": "token-extended"})
	cause := errors.New("cleanup echoed token-extended and token")
	require.NoError(t, s.completeGraph(token, owner, binding, stagedChannelSetup{
		rollbackPrerequisite: func(context.Context) error { return cause },
	}))

	err = s.abandon(context.Background(), token, owner)
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "token-extended")
	assert.NotContains(t, err.Error(), " and token")
	assert.Contains(t, err.Error(), `<redacted id="2"/>`)
	assert.Contains(t, err.Error(), `<redacted id="1"/>`)
	assert.NotErrorIs(t, err, cause)
}

func TestChannelSetupStore_ConcurrentGraphClaimIsSingleUse(t *testing.T) {
	s := newChannelSetupStore()
	owner := identity.CanonicalFromTrusted("YWRtaW4", "test fixture")
	binding := graphChannelBinding()
	token, err := s.put(&pendingChannelSetup{owner: owner, graphBinding: &binding})
	require.NoError(t, err)
	completeGraphForTest(t, s, token, owner, binding, stagedChannelSetup{})

	var successes atomic.Int32
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, claimErr := s.claimGraph(token, owner, binding); claimErr == nil {
				successes.Add(1)
			}
		}()
	}
	wg.Wait()
	assert.Equal(t, int32(1), successes.Load())
}

func TestChannelSetupStore_GraphFailureCleansAndSuccessConsumesExactlyOnce(t *testing.T) {
	owner := identity.CanonicalFromTrusted("YWRtaW4", "test fixture")
	binding := graphChannelBinding()

	t.Run("failure invalidates and cleans once", func(t *testing.T) {
		s := newChannelSetupStore()
		var cleanups atomic.Int32
		token, err := s.put(&pendingChannelSetup{owner: owner, graphBinding: &binding})
		require.NoError(t, err)
		completeGraphForTest(t, s, token, owner, binding, stagedChannelSetup{
			rollbackPrerequisite: func(context.Context) error { cleanups.Add(1); return nil },
		})
		claimed, err := s.claimGraph(token, owner, binding)
		require.NoError(t, err)
		require.NoError(t, claimed.invalidate(context.Background()))
		require.NoError(t, claimed.invalidate(context.Background()))
		assert.Equal(t, int32(1), cleanups.Load())
		_, err = s.claimGraph(token, owner, binding)
		require.ErrorIs(t, err, errNoPendingSetup)
	})

	t.Run("success consumes without cleanup", func(t *testing.T) {
		s := newChannelSetupStore()
		var cleanups atomic.Int32
		token, err := s.put(&pendingChannelSetup{owner: owner, graphBinding: &binding})
		require.NoError(t, err)
		completeGraphForTest(t, s, token, owner, binding, stagedChannelSetup{
			rollbackPrerequisite: func(context.Context) error { cleanups.Add(1); return nil },
		})
		claimed, err := s.claimGraph(token, owner, binding)
		require.NoError(t, err)
		require.NoError(t, claimed.consume())
		require.Error(t, claimed.consume())
		assert.Zero(t, cleanups.Load())
		_, err = s.claimGraph(token, owner, binding)
		require.ErrorIs(t, err, errNoPendingSetup)
	})

	t.Run("abandonment cleans once and invalidates", func(t *testing.T) {
		s := newChannelSetupStore()
		var cleanups atomic.Int32
		token, err := s.put(&pendingChannelSetup{owner: owner, graphBinding: &binding})
		require.NoError(t, err)
		completeGraphForTest(t, s, token, owner, binding, stagedChannelSetup{
			rollbackPrerequisite: func(context.Context) error { cleanups.Add(1); return nil },
		})
		require.NoError(t, s.abandon(context.Background(), token, owner))
		require.ErrorIs(t, s.abandon(context.Background(), token, owner), errNoPendingSetup)
		assert.Equal(t, int32(1), cleanups.Load())
		_, err = s.claimGraph(token, owner, binding)
		require.ErrorIs(t, err, errNoPendingSetup)
	})
}

func TestChannelSetupStore_StagedGraphOutputIsCopiedAndNeverSerialized(t *testing.T) {
	s := newChannelSetupStore()
	owner := identity.CanonicalFromTrusted("YWRtaW4", "test fixture")
	binding := graphChannelBinding()
	token, err := s.put(&pendingChannelSetup{owner: owner, graphBinding: &binding})
	require.NoError(t, err)
	const secret = "stage-only-secret-value"
	out := channelkinds.WizardOutput{SecretManifest: &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "demo-ns", Name: "channel-creds"},
		Data:       map[string][]byte{"token": []byte(secret)},
	}}
	completeGraphForTest(t, s, token, owner, binding, stagedChannelSetup{
		output: out, sensitiveValues: []string{secret},
	})

	out.SecretManifest.Data["token"] = []byte("mutated-after-staging")
	claimed, err := s.claimGraph(token, owner, binding)
	require.NoError(t, err)
	assert.Equal(t, secret, string(claimed.staged.output.SecretManifest.Data["token"]),
		"the store must own an immutable copy of the sealed wizard result")
	wire, err := json.Marshal(claimed)
	require.NoError(t, err)
	assert.NotContains(t, string(wire), secret)
	assert.NotContains(t, string(wire), "mutated-after-staging")
}

func TestChannelSetup_GraphFinishErrorRedactsPostedSecret(t *testing.T) {
	c := ctrlfake.NewClientBuilder().WithScheme(channelTestScheme(t)).Build()
	a := newChannelFormAdmind(t, c)
	const secret = "graph-provider-super-secret"
	a.wizardFor = wizards(stubWizard{
		questions: []oap.Question{{Name: "bot-token", Type: oap.QSecret, Prompt: "Bot token"}},
		resultErr: fmt.Errorf("provider rejected %s", secret),
	})
	owner := identity.CanonicalFromTrusted("YWRtaW4", "test fixture")
	binding := graphChannelBinding()
	token, err := a.channelSetups.put(&pendingChannelSetup{
		owner: owner, namespace: "demo-ns", agentClass: binding.AgentClass,
		required: binding.Required, graphBinding: &binding,
	})
	require.NoError(t, err)

	rec := postChannelSetup(t, a, token, "user:YWRtaW4", map[string]string{"bot-token": secret})
	require.Equal(t, http.StatusBadRequest, rec.Code, "body: %s", rec.Body.String())
	assert.NotContains(t, rec.Body.String(), secret)
	_, err = a.channelSetups.get(token, owner)
	assert.ErrorIs(t, err, errNoPendingSetup, "a failed resolver token is invalidated instead of being rerun")
}

func TestChannelSetup_CompletedGraphTokenRefusesBeforeRunningWizardAgain(t *testing.T) {
	c := ctrlfake.NewClientBuilder().WithScheme(channelTestScheme(t)).Build()
	a := newChannelFormAdmind(t, c)
	calls := 0
	a.wizardFor = wizards(stubWizard{resultCalls: &calls})
	owner := identity.CanonicalFromTrusted("YWRtaW4", "test fixture")
	binding := graphChannelBinding()
	token, err := a.channelSetups.put(&pendingChannelSetup{
		owner: owner, namespace: "demo-ns", agentClass: binding.AgentClass,
		required: binding.Required, graphBinding: &binding,
	})
	require.NoError(t, err)

	first := postChannelSetup(t, a, token, "user:YWRtaW4", nil)
	require.Equal(t, http.StatusOK, first.Code, "body: %s", first.Body.String())
	replay := postChannelSetup(t, a, token, "user:YWRtaW4", nil)
	require.Equal(t, http.StatusConflict, replay.Code, "body: %s", replay.Body.String())
	assert.Equal(t, 1, calls, "a completed one-use token must refuse before provider resolution or Result runs again")
}

type blockingResultWizard struct {
	calls   atomic.Int32
	entered chan struct{}
	release chan struct{}
}

func (w *blockingResultWizard) Inputs(context.Context, channelkinds.WizardInput) ([]oap.Question, error) {
	return nil, nil
}
func (w *blockingResultWizard) Handoff(context.Context, channelkinds.WizardInput) (*channelkinds.HandoffSpec, error) {
	return nil, nil
}
func (w *blockingResultWizard) Resolve(context.Context, channelkinds.WizardInput, map[string]string) (map[string]string, error) {
	return nil, nil
}
func (w *blockingResultWizard) Result(channelkinds.WizardInput, map[string]string) (channelkinds.WizardOutput, error) {
	w.calls.Add(1)
	close(w.entered)
	<-w.release
	return channelkinds.WizardOutput{}, nil
}

func TestChannelSetup_ConcurrentGraphSubmitClaimsBeforeWizardWork(t *testing.T) {
	c := ctrlfake.NewClientBuilder().WithScheme(channelTestScheme(t)).Build()
	a := newChannelFormAdmind(t, c)
	wiz := &blockingResultWizard{entered: make(chan struct{}), release: make(chan struct{})}
	a.wizardFor = wizards(wiz)
	owner := identity.CanonicalFromTrusted("YWRtaW4", "test fixture")
	binding := graphChannelBinding()
	token, err := a.channelSetups.put(&pendingChannelSetup{
		owner: owner, namespace: "demo-ns", agentClass: binding.AgentClass,
		required: binding.Required, graphBinding: &binding,
	})
	require.NoError(t, err)

	post := func() *httptest.ResponseRecorder {
		body, marshalErr := json.Marshal(channelSetupRequest{SetupToken: token})
		require.NoError(t, marshalErr)
		req := httptest.NewRequest(http.MethodPost, channelSetupPath, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer test-token")
		req.Header.Set("X-Admin-Subject", "user:YWRtaW4")
		rec := httptest.NewRecorder()
		a.Handler().ServeHTTP(rec, req)
		return rec
	}

	firstDone := make(chan *httptest.ResponseRecorder, 1)
	go func() { firstDone <- post() }()
	<-wiz.entered
	second := post()
	require.Equal(t, http.StatusConflict, second.Code, "body: %s", second.Body.String())
	close(wiz.release)
	first := <-firstDone
	require.Equal(t, http.StatusOK, first.Code, "body: %s", first.Body.String())
	assert.Equal(t, int32(1), wiz.calls.Load(), "the losing request must be refused before any wizard callback")
}

// --- what a response may carry -------------------------------------------------

// TestChannelSetup_NoAnswerValueIsEchoedBack asserts the property
// STRUCTURALLY: every value that went in, and everything a kind decided, is
// checked against the body — rather than sweeping for one credential's
// prefix, which would both miss another shape and fail a correct
// implementation whose question prompts legitimately name one.
//
// The Channel's own name is excluded because it is an identifier the response
// exists to state, not an answer the operator supplied.
func TestChannelSetup_NoAnswerValueIsEchoedBack(t *testing.T) {
	c := installedCluster(t)
	a := newChannelFormAdmind(t, c)
	token := setupTokenFor(t, a, bentoBundle(t), "demo-agent-bento")

	answers := map[string]string{
		"authzsubject": "service:demo-bot-secret-subject",
		"interval":     "@every 72h",
		"mapping":      `root = "a mapping nobody should read back"`,
	}
	rec := postChannelSetup(t, a, token, "user:YWRtaW4", answers)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	body := rec.Body.String()
	for k, v := range answers {
		assert.NotContains(t, body, v,
			"answer %q reached the response body; this body is logged and rendered, and the next kind's answer is a token", k)
	}

	// The one place a kind states what it decided is WizardOutput.Summary,
	// which is masked only by each kind's own discipline. It is logged and not
	// returned, so this route's safety does not depend on every future kind.
	var resp channelSetupResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.NotEmpty(t, resp.NextSteps, "the kind's post-setup guidance is what the operator still needs")
	for _, n := range resp.NextSteps {
		assert.NotContains(t, strings.ToLower(n), "service:demo-bot-secret-subject")
	}
}

// --- the declared role, and the name the Channel actually took ----------------

// rolelessKindBundle declares one channel of the `fake` kind with the given
// role.
//
// THE KIND IS THE POINT OF THIS FIXTURE, not an arbitrary choice. Every other
// test in this file drives bento, whose Result hardcodes role=input — so a
// route that stamped the declared role and a route that stamped nothing
// produced byte-identical Channels, and the class of defect below was
// invisible here while a CLI test for the same property passed. `fake` sets no
// role at all, which is the production shape too: slack's non-monitoring
// Result sets none, so its Channel takes ChannelSpec.Role's `both` default —
// and `both` is deliberately not an output-binding candidate.
//
// It also fixes its own Channel name ("fake-channel") whatever it is seeded,
// which is the other property below: the declared name is what every bundled
// CR reads "<name>-creds" against.
func rolelessKindBundle(t *testing.T, role string) []byte {
	t.Helper()
	return packedChannelBundle(t, oap.RequiredChannel{
		Kind: "fake", Role: role, Name: "demo-agent-fake",
		Purpose: "Where this agent's work is delivered.",
	})
}

// TestChannelSetup_TheDeclaredRoleReachesTheChannel is the console half of a
// property the CLI already had (TestWireChannels_StampsTheDeclaredRole) and
// this route did not.
//
// No kind's wizard asks for a role, so it reaches the Channel from the
// declaration or not at all — and "not at all" is not "unset": the CRD
// defaults it to `both`, which outputbind refuses as an output target. A
// bundle declaring `role: output` whose Channel came out `both` therefore left
// the agent's OTHER Channel reporting
// Valid=False/ChannelOutputBindingUnresolvable, with no supported command to
// fix it — installed cleanly, delivered nothing.
func TestChannelSetup_TheDeclaredRoleReachesTheChannel(t *testing.T) {
	c := installedCluster(t)
	a := newChannelFormAdmind(t, c)

	token := setupTokenFor(t, a, rolelessKindBundle(t, spiceboxv1alpha1.ChannelRoleOutput), "demo-agent-fake")
	rec := postChannelSetup(t, a, token, "user:YWRtaW4", nil)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	var got spiceboxv1alpha1.Channel
	require.NoError(t, c.Get(context.Background(),
		client.ObjectKey{Namespace: "demo-ns", Name: "fake-channel"}, &got))
	assert.Equal(t, spiceboxv1alpha1.ChannelRoleOutput, got.Spec.Role,
		"a bundle that declared role: output must not get a Channel the output binding will not match")
}

// The other half of that property — that the stamp is not a blanket rewrite
// over a role the KIND set — is pinned one layer down, in
// wizardrun.TestFinish_NoDeclaredRoleLeavesTheKindsOwnAnswerAlone, because it
// cannot be reached from here: LintRequiredChannels defaults an omitted
// declared role to ChannelSpec.Role's own `both` default and refuses it for
// every kind that does not serve one (bento, the kind whose Result sets a
// role, serves input alone). "Declared nothing" therefore never reaches this
// route at all for such a kind, and a test here would be asserting against a
// combination the install refuses.

// TestChannelSetup_AChannelThatDidNotTakeTheDeclaredNameIsReportedAsSuch: the
// response used to state the DECLARED name unconditionally, so a kind whose
// manifests fix their own name produced a 200 naming a Channel that does not
// exist — and said nothing about the "<declared>-creds" Secret the rest of the
// bundle reads and will not find. The CLI has carried this warning since the
// wiring pass was written; this is the same sentence, from the same function.
func TestChannelSetup_AChannelThatDidNotTakeTheDeclaredNameIsReportedAsSuch(t *testing.T) {
	c := installedCluster(t)
	a := newChannelFormAdmind(t, c)

	// role=output, not input: this test's subject is the NAME, and an
	// input-only declaration would be refused by B-R14 before a form exists to
	// submit. Either role exercises the name property identically.
	token := setupTokenFor(t, a, rolelessKindBundle(t, spiceboxv1alpha1.ChannelRoleOutput), "demo-agent-fake")
	rec := postChannelSetup(t, a, token, "user:YWRtaW4", nil)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	var body channelSetupResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, "fake-channel", body.Name,
		"the response must name the Channel that exists, not the one that was asked for")
	require.Len(t, body.Warnings, 1, "a Channel under an unexpected name is not a silent success")
	assert.Contains(t, body.Warnings[0], "demo-agent-fake-creds",
		"the warning must name the Secret the rest of the bundle is reading")

	noChannel(t, c, "demo-ns", "demo-agent-fake")
}
