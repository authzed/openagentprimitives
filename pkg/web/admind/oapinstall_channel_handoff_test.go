package admind

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/registry"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/platform/oap"
	"github.com/authzed/openagentprimitives/pkg/x/externalurl"
)

// --- the fixture kind ---------------------------------------------------------

// The values the fixture's exchange mints. They stand in for github's App
// private key, webhook secret and App ID: what Complete returns, what the
// Secret must end up holding, and what no response body may carry.
const (
	fixtureAppID         = "APP-ID-99"
	fixturePrivateKey    = "-----BEGIN RSA PRIVATE KEY-----\nZmFrZS1rZXktbWF0ZXJpYWw=\n-----END RSA PRIVATE KEY-----"
	fixtureWebhookSecret = "whsec-fixture-0123456789"
)

// handoffCalls records what a fixture wizard's closures were asked to do, so a
// test can assert that a step which must NOT run did not run — rather than
// asserting only on a status code, which a handler that ran everything and then
// refused would also produce.
type handoffCalls struct {
	// mu guards every counter below. The concurrent-callback test drives the
	// exchange from several goroutines, and an unguarded ++ there is a race in
	// the FIXTURE — which -race reports indistinguishably from one in the code
	// under test.
	mu            sync.Mutex
	handoffCount  int
	beginCount    int
	beginAnswers  map[string]string
	beginCallback string
	completeCount int
	completeQuery url.Values
	guidanceCount int
}

// handoffWizard is a channel kind whose setup includes a browser round trip.
//
// It models github's shape faithfully in the two places this file depends on:
// Begin needs an up-front answer ("org") and receives the client's callback
// URL, and the exchange answers only SOME of the fallback questions — the
// installation ID is asked on every route, because the service redirects an
// App's creation back and an installation not. A fixture whose exchange
// answered everything would make the whole fallback path untested.
type handoffWizard struct {
	calls    *handoffCalls
	beginErr error
	beginURL string
	notes    []string
	onBegin  func()
	// completeErr, when set, is what the exchange fails with.
	completeErr error
	// guidance, when set, replaces the fixture's own FallbackGuidance, for the
	// tests about what guidance may carry.
	guidance func(map[string]string) (string, error)
	// guidanceErr, when set, makes FallbackGuidance fail — which must not end
	// the run.
	guidanceErr error
	// onComplete, when set, runs at the top of the exchange. A seam for forcing
	// an interleaving: a test that only STARTED two callbacks could pass because
	// the first finished before the second began.
	onComplete func()
	// noHandoff makes Handoff return nil, for the routes that must refuse a
	// kind with nothing to hand off.
	noHandoff bool
	skipWhen  func(map[string]string) string
	resolve   func(map[string]string) map[string]string
}

func (w handoffWizard) Inputs(context.Context, channelkinds.WizardInput) ([]oap.Question, error) {
	return []oap.Question{
		// Declared, and then seeded by the plan and dropped from the form —
		// which is also what carries its value into the answer map. A fixture
		// that omitted it would hand Result an empty AgentClass and make the
		// binding assertion below pass for the wrong reason.
		{Name: "agentclass", Type: oap.QString, Prompt: "Bind to AgentClass"},
		{Name: "org", Type: oap.QString, Prompt: "Organization"},
		{Name: "authzsubject", Type: oap.QString, Prompt: "SpiceDB subject", Default: "service:demo-bot"},
	}, nil
}

func (w handoffWizard) Handoff(context.Context, channelkinds.WizardInput) (*channelkinds.HandoffSpec, error) {
	w.calls.mu.Lock()
	w.calls.handoffCount++
	w.calls.mu.Unlock()
	if w.noHandoff {
		return nil, nil
	}
	return &channelkinds.HandoffSpec{
		Begin: func(answers map[string]string, callbackURL string) (channelkinds.HandoffStart, error) {
			if w.onBegin != nil {
				w.onBegin()
			}
			w.calls.mu.Lock()
			w.calls.beginCount++
			w.calls.beginAnswers = answers
			w.calls.beginCallback = callbackURL
			w.calls.mu.Unlock()
			if w.beginErr != nil {
				return channelkinds.HandoffStart{}, w.beginErr
			}
			beginURL := w.beginURL
			if beginURL == "" {
				beginURL = "https://service.invalid/organizations/" + answers["org"] + "/apps/new"
			}
			return channelkinds.HandoffStart{
				Explain:    "Opening the service to create an App under " + answers["org"] + ".",
				URL:        beginURL,
				FormFields: map[string]string{"manifest": `{"redirect_url":"` + callbackURL + `"}`},
			}, nil
		},
		Complete: func(_ context.Context, callback url.Values) (map[string]string, error) {
			if w.onComplete != nil {
				w.onComplete()
			}
			w.calls.mu.Lock()
			w.calls.completeCount++
			w.calls.completeQuery = callback
			w.calls.mu.Unlock()
			if w.completeErr != nil {
				return nil, w.completeErr
			}
			if strings.TrimSpace(callback.Get("code")) == "" {
				return nil, errors.New("the callback carried no exchange code")
			}
			return map[string]string{
				"app-id":         fixtureAppID,
				"private-key":    fixturePrivateKey,
				"webhook-secret": fixtureWebhookSecret,
			}, nil
		},
		FallbackInputs: []oap.Question{
			{Name: "app-id", Type: oap.QString, Prompt: "App ID"},
			{Name: "private-key-path", Type: oap.QString, Prompt: "Path to the App's key file"},
			{Name: "webhook-secret", Type: oap.QSecret, Prompt: "Webhook secret"},
			{Name: "installation-id", Type: oap.QString, Prompt: "Installation ID"},
		},
		// The exchange returns the KEY and the manual route asks for a PATH:
		// without this the happy path asks for a file that does not exist.
		SatisfiedBy: map[string]string{"private-key-path": "private-key"},
		FallbackGuidance: func(answers map[string]string) (string, error) {
			w.calls.mu.Lock()
			w.calls.guidanceCount++
			w.calls.mu.Unlock()
			if w.guidanceErr != nil {
				return "", w.guidanceErr
			}
			if w.guidance != nil {
				return w.guidance(answers)
			}
			return "Install the App on the organization, then paste its installation ID below.", nil
		},
		SkipWhen: w.skipWhen,
	}, nil
}

func (w handoffWizard) Resolve(_ context.Context, _ channelkinds.WizardInput, answers map[string]string) (map[string]string, error) {
	if w.resolve != nil {
		return w.resolve(answers), nil
	}
	return nil, nil
}

// Result puts every answer that matters onto the manifests, so a test can read
// back what the exchange produced from the objects rather than from a claim.
func (w handoffWizard) Result(in channelkinds.WizardInput, answers map[string]string) (channelkinds.WizardOutput, error) {
	name := answers["name"]
	return channelkinds.WizardOutput{
		SecretManifest: &corev1.Secret{
			TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
			ObjectMeta: metav1.ObjectMeta{Name: name + "-creds", Namespace: in.Namespace},
			// Data rather than StringData: an apiserver folds stringData into
			// data, and the controller-runtime fake client does not — so a
			// fixture using it would read back empty and the assertion that
			// the credential landed would be inert.
			Data: map[string][]byte{
				"private-key":    []byte(answers["private-key"]),
				"webhook-secret": []byte(answers["webhook-secret"]),
			},
		},
		ChannelManifest: &spiceboxv1alpha1.Channel{
			TypeMeta:   metav1.TypeMeta{APIVersion: "agentprimitives.authzed.com/v1alpha1", Kind: "Channel"},
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: in.Namespace},
			Spec: spiceboxv1alpha1.ChannelSpec{
				Kind:         "bento",
				AgentClass:   answers["agentclass"],
				AuthzSubject: answers["authzsubject"],
				CredentialsRef: spiceboxv1alpha1.ChannelCredentialsRef{
					SecretName: name + "-creds",
				},
				Bento: &spiceboxv1alpha1.BentoChannelConfig{
					Generate: &spiceboxv1alpha1.BentoGenerateConfig{
						// A carrier for the two fields this fixture wants
						// readable back off the Channel: the App this run
						// created, and where it was installed.
						Mapping:  "app=" + answers["app-id"] + " org=" + answers["org"],
						Interval: "installation=" + answers["installation-id"],
					},
				},
			},
		},
		Notes:   append([]string{"invite the App to the repositories it should watch"}, w.notes...),
		Summary: []channelkinds.SummaryNote{{Label: "App", Value: fixtureAppID}},
	}, nil
}

// --- fixtures -----------------------------------------------------------------

// publishedCluster is installedCluster plus the ConfigMap webd publishes the
// cluster's external URL in — the value a handoff's callback URL is built from,
// and without which no handoff can begin at all.
func publishedCluster(t *testing.T, extra ...client.Object) client.Client {
	t.Helper()
	return installedCluster(t, append(extra, &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      spiceboxv1alpha1.WebdExternalURLConfigMap,
			Namespace: externalurl.Namespace,
		},
		Data: map[string]string{spiceboxv1alpha1.WebdTrustedURLKey: "https://ap.demo.invalid"},
	})...)
}

// newHandoffAdmind wires the fixture kind in through the SAME lookup the
// install form and the setup route resolve through, so the row a test gets a
// token from and the wizard the handoff drives are one kind.
func newHandoffAdmind(t *testing.T, c client.Client, w handoffWizard) (*Admind, *handoffCalls) {
	t.Helper()
	calls := &handoffCalls{}
	w.calls = calls
	a := newChannelFormAdmind(t, c)
	a.wizardFor = wizards(w)
	return a, calls
}

// handoffTokenFor drives the real install route for a token, asserting the row
// came back marked as a handoff.
func handoffTokenFor(t *testing.T, a *Admind) string {
	t.Helper()
	rec := postInstall(t, a, bentoBundle(t), map[string]string{"namespace": "demo-ns"})
	require.Equal(t, http.StatusBadRequest, rec.Code, "body: %s", rec.Body.String())

	var body oapInstallMissingQuestionsResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	// By name: bentoBundle declares a delivery sibling too (B-R14 refuses an
	// input channel with no role=output one), and newHandoffAdmind resolves
	// EVERY kind to the fixture wizard — so both rows come back marked as a
	// handoff and an index would pick whichever was declared first.
	row := channelRowNamed(t, body, "demo-agent-bento")
	require.Equal(t, channelFormHandoff, row.Status, "reason: %s", row.Reason)
	require.NotEmpty(t, row.SetupToken)
	return row.SetupToken
}

func postChannelHandoff(t *testing.T, a *Admind, token, subject string, answers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(channelHandoffRequest{SetupToken: token, Answers: answers})
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodPost, channelHandoffPath, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer test-token")
	if subject != "" {
		req.Header.Set("X-Admin-Subject", subject)
	}
	rec := httptest.NewRecorder()
	a.Handler().ServeHTTP(rec, req)
	return rec
}

func TestChannelHandoffProviderSkippedAndBeginErrorsRedactEveryPostedAnswer(t *testing.T) {
	const short = "org-secret"
	const long = "org-secret-extended"
	tests := []struct {
		name   string
		wizard handoffWizard
		status int
	}{
		{
			name:   "skipped reason",
			status: http.StatusOK,
			wizard: handoffWizard{skipWhen: func(map[string]string) string {
				return "provider skipped " + long + " then " + short
			}},
		},
		{
			name: "begin error", status: http.StatusBadRequest,
			wizard: handoffWizard{beginErr: errors.New("provider rejected " + long + " then " + short)},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a, _ := newHandoffAdmind(t, publishedCluster(t), tc.wizard)
			token := handoffTokenFor(t, a)
			rec := postChannelHandoff(t, a, token, "user:YWRtaW4", map[string]string{"org": long, "authzsubject": short})
			require.Equal(t, tc.status, rec.Code, "body: %s", rec.Body.String())
			assert.NotContains(t, rec.Body.String(), long)
			assert.NotContains(t, rec.Body.String(), "then "+short)
			assert.Contains(t, rec.Body.String(), "redacted")
		})
	}
}

func TestGraphChannelHandoffSkippedReturnsGenericSetupActionAndCanStage(t *testing.T) {
	a, calls := newHandoffAdmind(t, publishedCluster(t), handoffWizard{
		skipWhen: func(map[string]string) string { return "use the existing application" },
		resolve: func(answers map[string]string) map[string]string {
			return map[string]string{"private-key": "key-from-" + answers["private-key-path"]}
		},
	})
	token := graphHandoffTokenFor(t, a)
	handoff := postChannelHandoff(t, a, token, "user:YWRtaW4", map[string]string{"org": "demo-org"})
	require.Equal(t, http.StatusOK, handoff.Code, "body: %s", handoff.Body.String())
	var transition channelHandoffResponse
	require.NoError(t, json.Unmarshal(handoff.Body.Bytes(), &transition))
	assert.Equal(t, "setup", transition.NextAction)
	assert.Empty(t, transition.URL)
	assert.Equal(t, 0, calls.beginCount, "a skipped handoff must not enter provider Begin")

	staged := postChannelSetup(t, a, token, "user:YWRtaW4", map[string]string{
		"org": "demo-org", "app-id": "existing-app", "private-key-path": "/safe/key.pem",
		"webhook-secret": "existing-secret", "installation-id": "4242",
	})
	require.Equal(t, http.StatusOK, staged.Code, "body: %s", staged.Body.String())
	var done channelSetupResponse
	require.NoError(t, json.Unmarshal(staged.Body.Bytes(), &done))
	assert.True(t, done.Staged)
}

func TestChannelHandoffMalformedBeginURLRedactsPostedAnswer(t *testing.T) {
	const secret = "org-secret-extended"
	a, _ := newHandoffAdmind(t, publishedCluster(t), handoffWizard{beginURL: "http://[" + secret})
	token := handoffTokenFor(t, a)
	rec := postChannelHandoff(t, a, token, "user:YWRtaW4", map[string]string{"org": secret})
	require.Equal(t, http.StatusBadRequest, rec.Code, "body: %s", rec.Body.String())
	assert.NotContains(t, rec.Body.String(), secret)
	assert.Contains(t, rec.Body.String(), "redacted")
}

func TestChannelHandoffDirectSetupRedactsNotesUsingAnswersAndSecretOutput(t *testing.T) {
	const answer = "notes-answer-secret"
	w := handoffWizard{notes: []string{"answer=" + answer, "credential=" + fixturePrivateKey}}
	a, _ := newHandoffAdmind(t, publishedCluster(t), w)
	token := handoffTokenFor(t, a)
	_, state := beginHandoff(t, a, token)
	cb := getCallback(t, a, "user:YWRtaW4", url.Values{"code": {"exchange-code"}, "state": {state}})
	require.Equal(t, http.StatusOK, cb.Code, "body: %s", cb.Body.String())
	done := postChannelSetup(t, a, token, "user:YWRtaW4", map[string]string{"installation-id": answer})
	require.Equal(t, http.StatusOK, done.Code, "body: %s", done.Body.String())
	assert.NotContains(t, done.Body.String(), answer)
	assert.NotContains(t, done.Body.String(), fixturePrivateKey)
	assert.Contains(t, done.Body.String(), "redacted")
}

// getCallback is the service's redirect, as a browser would deliver it: a GET
// carrying the query, through webd's proxy headers.
func getCallback(t *testing.T, a *Admind, subject string, q url.Values) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, channelHandoffCallbackPath+"?"+q.Encode(), nil)
	req.Header.Set("Authorization", "Bearer test-token")
	if subject != "" {
		req.Header.Set("X-Admin-Subject", subject)
	}
	rec := httptest.NewRecorder()
	a.Handler().ServeHTTP(rec, req)
	return rec
}

// beginHandoff runs the begin route and returns the `state` the response's URL
// carries — read off the URL rather than out of the store, because that is
// where the external service reads it from.
func beginHandoff(t *testing.T, a *Admind, token string) (channelHandoffResponse, string) {
	t.Helper()
	rec := postChannelHandoff(t, a, token, "user:YWRtaW4", map[string]string{"org": "demo-org"})
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	var body channelHandoffResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	u, err := url.Parse(body.URL)
	require.NoError(t, err)
	state := u.Query().Get("state")
	require.NotEmpty(t, state, "the client owns the nonce, and it rides on the address the operator is sent to")
	return body, state
}

func graphHandoffTokenFor(t *testing.T, a *Admind) string {
	t.Helper()
	owner := identity.CanonicalFromTrusted("YWRtaW4", "test fixture")
	binding := graphChannelBinding()
	binding.AgentClass = "demo-class"
	binding.Required = oap.RequiredChannel{Kind: "bento", Role: "input", Name: "demo-agent-bento"}
	token, err := a.channelSetups.put(&pendingChannelSetup{
		owner: owner, namespace: "demo-ns", agentClass: binding.AgentClass,
		required: binding.Required, graphBinding: &binding,
	})
	require.NoError(t, err)
	return token
}

func TestGraphChannelHandoffBeginIsExclusiveBeforeProviderBegin(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var first atomic.Bool
	a, calls := newHandoffAdmind(t, publishedCluster(t), handoffWizard{onBegin: func() {
		if first.CompareAndSwap(false, true) {
			close(entered)
			<-release
		}
	}})
	token := graphHandoffTokenFor(t, a)
	firstDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		firstDone <- postChannelHandoff(t, a, token, "user:YWRtaW4", map[string]string{"org": "demo-org"})
	}()
	<-entered
	second := postChannelHandoff(t, a, token, "user:YWRtaW4", map[string]string{"org": "demo-org"})
	close(release)
	firstResp := <-firstDone
	assert.Equal(t, http.StatusOK, firstResp.Code, "body: %s", firstResp.Body.String())
	assert.Equal(t, http.StatusConflict, second.Code, "body: %s", second.Body.String())
	assert.Equal(t, 1, calls.beginCount, "only the atomically claimed begin may enter provider Begin")
}

func TestGraphChannelHandoffBeginExcludesOrdinarySubmitBeforeProviderWork(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	a, calls := newHandoffAdmind(t, publishedCluster(t), handoffWizard{onBegin: func() {
		close(entered)
		<-release
	}})
	token := graphHandoffTokenFor(t, a)
	firstDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		firstDone <- postChannelHandoff(t, a, token, "user:YWRtaW4", map[string]string{"org": "demo-org"})
	}()
	<-entered
	before := calls.handoffCount
	submit := postChannelSetup(t, a, token, "user:YWRtaW4", map[string]string{"org": "demo-org"})
	close(release)
	<-firstDone
	assert.Equal(t, http.StatusConflict, submit.Code, "body: %s", submit.Body.String())
	assert.Equal(t, before, calls.handoffCount, "the losing submit must be refused before asking the provider for a handoff")
}

func TestGraphChannelHandoffMissingUpfrontAnswerPreservesTokenForRetry(t *testing.T) {
	a, calls := newHandoffAdmind(t, publishedCluster(t), handoffWizard{})
	token := graphHandoffTokenFor(t, a)

	missing := postChannelHandoff(t, a, token, "user:YWRtaW4", map[string]string{})
	require.Equal(t, http.StatusBadRequest, missing.Code, "body: %s", missing.Body.String())
	var missingBody oapInstallMissingQuestionsResponse
	require.NoError(t, json.Unmarshal(missing.Body.Bytes(), &missingBody))
	require.Len(t, missingBody.Questions, 1)
	assert.Equal(t, "org", missingBody.Questions[0].Name)

	retry := postChannelHandoff(t, a, token, "user:YWRtaW4", map[string]string{"org": "demo-org"})
	require.Equal(t, http.StatusOK, retry.Code, "the same graph-bound token must remain usable; body: %s", retry.Body.String())
	assert.Equal(t, 1, calls.beginCount, "missing input must be refused before provider Begin")
}

func TestGraphChannelHandoffFailedCompleteInvalidatesAndCannotBeginAgain(t *testing.T) {
	a, calls := newHandoffAdmind(t, publishedCluster(t), handoffWizard{completeErr: errors.New("exchange failed")})
	token := graphHandoffTokenFor(t, a)
	_, state := beginHandoff(t, a, token)
	failed := getCallback(t, a, "user:YWRtaW4", url.Values{"code": {"exchange-code"}, "state": {state}})
	require.Equal(t, http.StatusBadRequest, failed.Code, "body: %s", failed.Body.String())
	retry := postChannelHandoff(t, a, token, "user:YWRtaW4", map[string]string{"org": "demo-org"})
	assert.NotEqual(t, http.StatusOK, retry.Code, "a failed Complete invalidates the graph token")
	assert.Equal(t, 1, calls.beginCount)
	replay := getCallback(t, a, "user:YWRtaW4", url.Values{"code": {"exchange-code"}, "state": {state}})
	assert.Equal(t, http.StatusBadRequest, replay.Code)
	assert.Equal(t, 1, calls.completeCount)
}

func TestGraphChannelHandoffMissingFallbackAnswerPreservesTokenForRetry(t *testing.T) {
	a, calls := newHandoffAdmind(t, publishedCluster(t), handoffWizard{notes: []string{"Grant the fixture App access to the selected repository."}})
	token := graphHandoffTokenFor(t, a)
	_, state := beginHandoff(t, a, token)
	callback := getCallback(t, a, "user:YWRtaW4", url.Values{"code": {"exchange-code"}, "state": {state}})
	require.Equal(t, http.StatusOK, callback.Code, "body: %s", callback.Body.String())

	missing := postChannelSetup(t, a, token, "user:YWRtaW4", map[string]string{})
	require.Equal(t, http.StatusBadRequest, missing.Code, "body: %s", missing.Body.String())
	var missingBody oapInstallMissingQuestionsResponse
	require.NoError(t, json.Unmarshal(missing.Body.Bytes(), &missingBody))
	require.Len(t, missingBody.Questions, 1)
	assert.Equal(t, "installation-id", missingBody.Questions[0].Name)

	retry := postChannelSetup(t, a, token, "user:YWRtaW4", map[string]string{"installation-id": "4242"})
	require.Equal(t, http.StatusOK, retry.Code, "the same graph-bound token must remain usable; body: %s", retry.Body.String())
	var done channelSetupResponse
	require.NoError(t, json.Unmarshal(retry.Body.Bytes(), &done))
	assert.True(t, done.Staged)
	assert.Equal(t, []string{
		"invite the App to the repositories it should watch",
		"Grant the fixture App access to the selected repository.",
	}, done.NextSteps,
		"provider completion guidance must survive graph staging")
	assert.Equal(t, 1, calls.beginCount, "retry must not re-enter provider Begin")
	assert.Equal(t, 1, calls.completeCount, "retry must reuse the sealed provider output")
	owner := identity.CanonicalFromTrusted("YWRtaW4", "test fixture")
	rec, err := a.channelSetups.get(token, owner)
	require.NoError(t, err)
	assert.Equal(t, graphSetupComplete, rec.graphState)
}

// --- the round trip -------------------------------------------------------------

// TestChannelHandoff_TheWholeRoundTripCreatesTheChannel: begin, the service's
// redirect back, then the one fallback answer the exchange could not supply.
func TestChannelHandoff_TheWholeRoundTripCreatesTheChannel(t *testing.T) {
	c := publishedCluster(t)
	a, calls := newHandoffAdmind(t, c, handoffWizard{})
	token := handoffTokenFor(t, a)

	begin, state := beginHandoff(t, a, token)
	assert.Equal(t, 1, calls.beginCount)
	assert.Contains(t, begin.URL, "demo-org", "the up-front answer reaches the address the browser goes to")
	assert.NotContains(t, begin.Explain, "demo-org", "provider explanation text cannot echo an answer onto the response wire")
	assert.Contains(t, begin.Explain, "redacted")
	require.Contains(t, begin.FormFields, "manifest",
		"a POST body cannot be expressed as a link; the UI submits a form")
	assert.Contains(t, begin.FormFields["manifest"], calls.beginCallback,
		"the service reads the callback out of the POSTed body, not off a named field")

	cb := getCallback(t, a, "user:YWRtaW4", url.Values{"code": {"exchange-code"}, "state": {state}})
	require.Equal(t, http.StatusOK, cb.Code, "body: %s", cb.Body.String())
	var cbBody channelHandoffCallbackResponse
	require.NoError(t, json.Unmarshal(cb.Body.Bytes(), &cbBody))
	assert.True(t, cbBody.Exchanged)
	assert.Equal(t, 1, calls.completeCount)

	// Only what the exchange did NOT answer is still owed. app-id and
	// webhook-secret came back under their own names; private-key-path is
	// satisfied under another name (SatisfiedBy), which is the rule a client
	// that filtered on its own would get wrong.
	names := make([]string, 0, len(cbBody.Questions))
	for _, q := range cbBody.Questions {
		names = append(names, q.Name)
	}
	assert.Equal(t, []string{"installation-id"}, names)
	assert.NotEmpty(t, cbBody.Guidance, "the operator cannot answer these without being told what to do")
	assert.Equal(t, 1, calls.guidanceCount)

	done := postChannelSetup(t, a, cbBody.SetupToken, "user:YWRtaW4", map[string]string{"installation-id": "4242"})
	require.Equal(t, http.StatusOK, done.Code, "body: %s", done.Body.String())

	var ch spiceboxv1alpha1.Channel
	require.NoError(t, c.Get(context.Background(),
		client.ObjectKey{Namespace: "demo-ns", Name: "demo-agent-bento"}, &ch))
	assert.Equal(t, "demo-class", ch.Spec.AgentClass)
	require.NotNil(t, ch.Spec.Bento)
	require.NotNil(t, ch.Spec.Bento.Generate)
	assert.Equal(t, "app="+fixtureAppID+" org=demo-org", ch.Spec.Bento.Generate.Mapping,
		"the exchange's answer AND the answer given before the browser left must both reach Result")
	assert.Equal(t, "installation=4242", ch.Spec.Bento.Generate.Interval)

	var sec corev1.Secret
	require.NoError(t, c.Get(context.Background(),
		client.ObjectKey{Namespace: "demo-ns", Name: "demo-agent-bento-creds"}, &sec))
	assert.Equal(t, fixturePrivateKey, string(sec.Data["private-key"]),
		"the credential the exchange minted lands in the Secret, which is the only place it belongs")
	assert.Equal(t, fixtureWebhookSecret, string(sec.Data["webhook-secret"]))
}

// TestChannelHandoff_CompleteRunsServerSideAndNothingItMintedReachesTheBrowser.
//
// Complete is placed server-side precisely so the App's credentials never enter
// a browser. Asserted STRUCTURALLY — every value the exchange returned is
// checked against the whole body — rather than by sweeping for one credential's
// header line, which would miss the webhook secret entirely and would fail a
// correct implementation whose prompt legitimately names a prefix.
func TestChannelHandoff_CompleteRunsServerSideAndNothingItMintedReachesTheBrowser(t *testing.T) {
	c := publishedCluster(t)
	a, _ := newHandoffAdmind(t, c, handoffWizard{})
	token := handoffTokenFor(t, a)
	_, state := beginHandoff(t, a, token)

	cb := getCallback(t, a, "user:YWRtaW4", url.Values{"code": {"exchange-code"}, "state": {state}})
	require.Equal(t, http.StatusOK, cb.Code)

	body := cb.Body.String()
	for label, minted := range map[string]string{
		"the App's private key": fixturePrivateKey,
		"the webhook secret":    fixtureWebhookSecret,
		"the App id":            fixtureAppID,
	} {
		assert.NotContains(t, body, minted,
			"%s reached the browser; Complete runs server-side so that it cannot", label)
	}
	// And the exchange code itself is not echoed either: it is single-use, and
	// a body that carried it would hand a replay to anything reading the page.
	assert.NotContains(t, body, "exchange-code")
}

// --- the state check ------------------------------------------------------------

// TestChannelHandoff_AStateMismatchIsRefusedBeforeTheCodeIsRead mirrors the
// CLI's own control, and its load-bearing assertion is the same one: Complete
// NEVER RAN.
//
// A callback delivered by a page that does not know this run's nonce is not
// this run's callback, and spending its code is exactly what the check exists
// to stop. Asserting only the status code would pass an implementation that
// exchanged first and refused afterwards — which is the failure, not a
// cosmetic ordering preference.
func TestChannelHandoff_AStateMismatchIsRefusedBeforeTheCodeIsRead(t *testing.T) {
	cases := []struct {
		name  string
		state func(real string) string
	}{
		{name: "a forged state: refused, and no code is spent", state: func(string) string { return "forged" }},
		{name: "no state at all: refused, and no code is spent", state: func(string) string { return "" }},
		{
			name: "a state one byte short of this run's: refused, and no code is spent",
			// A prefix of the real nonce, which is what a byte-at-a-time guess
			// looks like — and what a non-constant-time compare would leak the
			// progress of.
			state: func(real string) string { return real[:len(real)-1] },
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := publishedCluster(t)
			a, calls := newHandoffAdmind(t, c, handoffWizard{})
			token := handoffTokenFor(t, a)
			_, state := beginHandoff(t, a, token)

			cb := getCallback(t, a, "user:YWRtaW4",
				url.Values{"code": {"exchange-code"}, "state": {tc.state(state)}})

			assert.Equal(t, http.StatusBadRequest, cb.Code,
				"the callback is a public endpoint; an unverified state lets anyone drive the exchange")
			assert.Zero(t, calls.completeCount,
				"the exchange must not have run: verifying the state AFTER reading the code spends the code")
			noChannel(t, c, "demo-ns", "demo-agent-bento")

			// The real state still works, so the refusals above are about the
			// state and not about the run having been torn down.
			ok := getCallback(t, a, "user:YWRtaW4", url.Values{"code": {"exchange-code"}, "state": {state}})
			require.Equal(t, http.StatusOK, ok.Code, "body: %s", ok.Body.String())
			assert.Equal(t, 1, calls.completeCount)
		})
	}
}

// TestChannelHandoff_ANonceIsSpentOnce: a replayed redirect — a refresh, a
// prefetch, a link someone kept — must not run a second exchange against the
// same run.
func TestChannelHandoff_ANonceIsSpentOnce(t *testing.T) {
	c := publishedCluster(t)
	a, calls := newHandoffAdmind(t, c, handoffWizard{})
	token := handoffTokenFor(t, a)
	_, state := beginHandoff(t, a, token)

	first := getCallback(t, a, "user:YWRtaW4", url.Values{"code": {"exchange-code"}, "state": {state}})
	require.Equal(t, http.StatusOK, first.Code)

	replay := getCallback(t, a, "user:YWRtaW4", url.Values{"code": {"exchange-code"}, "state": {state}})
	assert.Equal(t, http.StatusBadRequest, replay.Code)
	assert.Equal(t, 1, calls.completeCount, "the second delivery must not have reached the exchange")
}

// TestChannelHandoff_ACallbackIsNotDrivableByAnotherOperator: the nonce travels
// through a browser, a referrer and the service's own logs. Both admins hold
// install_agent, so the permission check cannot tell them apart.
func TestChannelHandoff_ACallbackIsNotDrivableByAnotherOperator(t *testing.T) {
	c := publishedCluster(t)
	calls := &handoffCalls{}
	a := newChannelFormAdmindFor(t, c, "YWRtaW4", "YWRtaW4y")
	a.wizardFor = wizards(handoffWizard{calls: calls})

	token := handoffTokenFor(t, a)
	_, state := beginHandoff(t, a, token)

	other := getCallback(t, a, "user:YWRtaW4y", url.Values{"code": {"exchange-code"}, "state": {state}})
	assert.Equal(t, http.StatusBadRequest, other.Code)
	assert.Zero(t, calls.completeCount)

	own := getCallback(t, a, "user:YWRtaW4", url.Values{"code": {"exchange-code"}, "state": {state}})
	require.Equal(t, http.StatusOK, own.Code, "body: %s", own.Body.String())
	assert.Equal(t, 1, calls.completeCount)
}

// --- the gate -------------------------------------------------------------------

// TestChannelHandoff_BothRoutesAreGatedLikeTheInstallTheyServe. A public
// callback that drives a credential exchange must not be less protected than
// the thing it serves — and the begin route creates a real App at the far end.
func TestChannelHandoff_BothRoutesAreGatedLikeTheInstallTheyServe(t *testing.T) {
	cases := []struct {
		name    string
		subject string
		want    int
	}{
		{name: "no forwarded subject: 401", subject: "", want: http.StatusUnauthorized},
		{name: "a subject without install_agent: 403", subject: "user:c29tZWJvZHk", want: http.StatusForbidden},
	}
	for _, tc := range cases {
		t.Run("begin: "+tc.name, func(t *testing.T) {
			c := publishedCluster(t)
			a, calls := newHandoffAdmind(t, c, handoffWizard{})
			token := handoffTokenFor(t, a)

			rec := postChannelHandoff(t, a, token, tc.subject, map[string]string{"org": "demo-org"})
			assert.Equal(t, tc.want, rec.Code, "body: %s", rec.Body.String())
			assert.Zero(t, calls.beginCount, "Begin creates a real App; it must not run for a refused caller")
		})
		t.Run("callback: "+tc.name, func(t *testing.T) {
			c := publishedCluster(t)
			a, calls := newHandoffAdmind(t, c, handoffWizard{})
			token := handoffTokenFor(t, a)
			_, state := beginHandoff(t, a, token)

			rec := getCallback(t, a, tc.subject, url.Values{"code": {"exchange-code"}, "state": {state}})
			assert.Equal(t, tc.want, rec.Code, "body: %s", rec.Body.String())
			assert.Zero(t, calls.completeCount, "a refused caller must not spend this run's code")
		})
	}
}

// --- refusals in front of Begin ---------------------------------------------------

// TestChannelHandoff_ANameAlreadyTakenIsRefusedBeforeAnyAppIsCreated is P5-R21
// on this route. Begin is where the irreversible half happens: an operator sent
// to create an App for a Channel name that cannot be created loses the run AND
// is left with an App whose key was minted for nothing.
func TestChannelHandoff_ANameAlreadyTakenIsRefusedBeforeAnyAppIsCreated(t *testing.T) {
	c := publishedCluster(t)
	a, calls := newHandoffAdmind(t, c, handoffWizard{})
	token := handoffTokenFor(t, a)

	// A Channel of this name, of this kind, bound to nobody in particular:
	// planning reports it as ours-or-not depending on the class, so this one
	// takes the name via the plain collision net rather than the conflict
	// branch. Either way Begin must not run.
	require.NoError(t, c.Create(context.Background(), &spiceboxv1alpha1.Channel{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-agent-bento", Namespace: "demo-ns"},
		Spec:       spiceboxv1alpha1.ChannelSpec{Kind: "bento", AgentClass: "some-other-agent"},
	}))

	rec := postChannelHandoff(t, a, token, "user:YWRtaW4", map[string]string{"org": "demo-org"})
	assert.Equal(t, http.StatusConflict, rec.Code, "body: %s", rec.Body.String())
	assert.Zero(t, calls.beginCount, "no App may be created for a Channel that cannot be")
}

// TestChannelHandoff_ABeginFailureEndsTheRunRatherThanFallingBack.
//
// Begin and FallbackGuidance are built by the same kind from the same answers,
// so a Begin that could not describe the round trip is a FallbackGuidance that
// cannot describe the manual route either. Falling back would hand the operator
// bare prompts plus a note saying the browser step did not finish, with the
// actual reason demoted to a footnote — which is the bug the CLI shipped.
func TestChannelHandoff_ABeginFailureEndsTheRunRatherThanFallingBack(t *testing.T) {
	c := publishedCluster(t)
	a, calls := newHandoffAdmind(t, c, handoffWizard{
		beginErr: errors.New(`"demo org" is not a valid organization login`),
	})
	token := handoffTokenFor(t, a)

	rec := postChannelHandoff(t, a, token, "user:YWRtaW4", map[string]string{"org": "demo org"})
	require.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "not a valid organization login",
		"the kind's own sentence is the one line that says what to fix")
	assert.Zero(t, calls.guidanceCount,
		"the fallback detour must not be entered: its guidance is built from the answers Begin just refused")

	// No nonce was recorded either, so nothing can be matched against a run
	// that never started. Read off the store rather than by probing the
	// callback with an empty state, which is refused unconditionally and would
	// pass whatever the begin route had left behind.
	rec2, err := a.channelSetups.get(token, identity.CanonicalFromTrusted("YWRtaW4", "test fixture"))
	require.NoError(t, err, "a refused begin leaves the setup itself usable")
	assert.Empty(t, rec2.handoffState)
	assert.Nil(t, rec2.handoff)
}

// TestChannelHandoff_ACallbackTheClusterCannotBeReachedAtIsRefusedUpFront: the
// service reads the callback out of what Begin produced, so beginning with no
// reachable address creates an App that redirects nowhere. The refusal quotes
// the planner's own reason for the absence.
func TestChannelHandoff_ACallbackTheClusterCannotBeReachedAtIsRefusedUpFront(t *testing.T) {
	// installedCluster, deliberately: no external-URL ConfigMap.
	c := installedCluster(t)
	a, calls := newHandoffAdmind(t, c, handoffWizard{})
	token := handoffTokenFor(t, a)

	rec := postChannelHandoff(t, a, token, "user:YWRtaW4", map[string]string{"org": "demo-org"})
	require.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), spiceboxv1alpha1.WebdExternalURLConfigMap,
		"the operator is owed the planner's reason, not a generic refusal")
	assert.Zero(t, calls.beginCount, "no App may be created against a callback that does not exist")
}

// TestChannelHandoff_AKindWithNothingToHandOffIsSentToTheSetupRoute keeps the
// two routes from silently accepting each other's work.
func TestChannelHandoff_AKindWithNothingToHandOffIsSentToTheSetupRoute(t *testing.T) {
	c := publishedCluster(t)
	a, _ := newHandoffAdmind(t, c, handoffWizard{noHandoff: true})

	// The row is a plain "ask" for a kind with no handoff, so the token comes
	// from the ordinary path.
	token := setupTokenFor(t, a, bentoBundle(t), "demo-agent-bento")
	rec := postChannelHandoff(t, a, token, "user:YWRtaW4", map[string]string{"org": "demo-org"})
	require.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), channelSetupPath)
}

// TestChannelHandoff_TheSetupRouteRefusesAKindWhoseBrowserStepHasNotRun is the
// mirror: a handoff kind must not be completed from a body alone, because the
// answers the round trip supplies would be missing and Result would build
// manifests from blanks.
func TestChannelHandoff_TheSetupRouteRefusesAKindWhoseBrowserStepHasNotRun(t *testing.T) {
	c := publishedCluster(t)
	a, _ := newHandoffAdmind(t, c, handoffWizard{})
	token := handoffTokenFor(t, a)

	rec := postChannelSetup(t, a, token, "user:YWRtaW4", map[string]string{"org": "demo-org"})
	require.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), channelHandoffPath)
	noChannel(t, c, "demo-ns", "demo-agent-bento")
}

// --- the detour -------------------------------------------------------------------

// TestChannelHandoff_GuidanceThatCannotBeBuiltIsReportedAndNotFatal: the
// questions are the floor that always works, and an operator who knows the
// values must not lose them because the prose could not be rendered — but the
// failure is stated where the questions are, not swallowed.
func TestChannelHandoff_GuidanceThatCannotBeBuiltIsReportedAndNotFatal(t *testing.T) {
	c := publishedCluster(t)
	a, _ := newHandoffAdmind(t, c, handoffWizard{guidanceErr: errors.New("could not write key " + fixturePrivateKey)})
	token := handoffTokenFor(t, a)
	_, state := beginHandoff(t, a, token)

	cb := getCallback(t, a, "user:YWRtaW4", url.Values{"code": {"exchange-code"}, "state": {state}})
	require.Equal(t, http.StatusOK, cb.Code, "body: %s", cb.Body.String())

	var body channelHandoffCallbackResponse
	require.NoError(t, json.Unmarshal(cb.Body.Bytes(), &body))
	assert.True(t, body.Exchanged)
	assert.NotEmpty(t, body.Questions, "the questions still have to be asked")
	assert.Empty(t, body.Guidance)
	assert.Contains(t, body.GuidanceError, "could not write key")
	assert.NotContains(t, body.GuidanceError, fixturePrivateKey)
	assert.Contains(t, body.GuidanceError, "redacted")
}

// TestChannelHandoff_AFailedExchangeSaysSoAndCreatesNothing.
func TestChannelHandoff_AFailedExchangeSaysSoAndCreatesNothing(t *testing.T) {
	c := publishedCluster(t)
	a, _ := newHandoffAdmind(t, c, handoffWizard{completeErr: errors.New("the service rejected exchange-code")})
	token := handoffTokenFor(t, a)
	_, state := beginHandoff(t, a, token)

	cb := getCallback(t, a, "user:YWRtaW4", url.Values{"code": {"exchange-code"}, "state": {state}})
	require.Equal(t, http.StatusBadRequest, cb.Code)
	assert.Contains(t, cb.Body.String(), "the service rejected")
	assert.NotContains(t, cb.Body.String(), "exchange-code")
	assert.Contains(t, cb.Body.String(), "redacted")
	noChannel(t, c, "demo-ns", "demo-agent-bento")
}

// --- the callback address ------------------------------------------------------------

// TestBrowserCallbackURL_IsAdmindsOwnRouteUnderTheProxyPrefix asserts that the
// two halves of the derivation compose back into admind's own mounted route:
// strip the browser prefix, put the API prefix back, and the result must be
// exactly the pattern Handler() registers. A drift between them produces an App
// that redirects to a 404 — created, and unreachable.
//
// It asserts NOTHING about pkg/web/adminui, which owns the other end of the
// rewrite; an earlier version of this comment claimed it did, which was the
// transcribed-list defect wearing the clothes of a guard. Both constants are
// exported now and adminui's proxyHandler builds its rewrite out of them, so
// the two packages cannot disagree by construction — and
// TestEveryProxyPatternUsesTheSharedPrefix, over in adminui, pins that route
// table's own pattern strings against the same constant.
func TestBrowserCallbackURL_IsAdmindsOwnRouteUnderTheProxyPrefix(t *testing.T) {
	rec := &pendingChannelSetup{seeded: map[string]string{"external-base-url": "https://ap.demo.invalid"}}
	a := &Admind{}
	got, err := a.channelHandoffCallbackURL(rec)
	require.NoError(t, err)

	u, err := url.Parse(got)
	require.NoError(t, err)
	assert.Equal(t, "https", u.Scheme)
	assert.Equal(t, "ap.demo.invalid", u.Host)

	// Undo the proxy's rewrite and the result must be the route admind mounts.
	require.True(t, strings.HasPrefix(u.Path, BrowserAPIPathPrefix), "path %q", u.Path)
	assert.Equal(t, channelHandoffCallbackPath,
		APIPathPrefix+strings.TrimPrefix(u.Path, BrowserAPIPathPrefix))
}

// TestBrowserCallbackURL_TrailingSlashesAndQueriesOnTheBaseAreNormalised: the
// published external URL is operator-supplied cluster state, and a doubled
// slash or a stray query on the callback is a redirect the service will not
// match.
func TestBrowserCallbackURL_TrailingSlashesAndQueriesOnTheBaseAreNormalised(t *testing.T) {
	a := &Admind{}
	want, err := a.channelHandoffCallbackURL(&pendingChannelSetup{
		seeded: map[string]string{"external-base-url": "https://ap.demo.invalid"},
	})
	require.NoError(t, err)

	for _, base := range []string{
		"https://ap.demo.invalid/",
		"https://ap.demo.invalid?next=%2Fadmin",
		"https://ap.demo.invalid/#frag",
	} {
		got, err := a.channelHandoffCallbackURL(&pendingChannelSetup{
			seeded: map[string]string{"external-base-url": base},
		})
		require.NoError(t, err, "base %q", base)
		assert.Equal(t, want, got, "base %q", base)
	}
}

// --- what the begin step leaves behind ------------------------------------------

// logEntry is one line a handler logged, with its structured context.
type logEntry struct {
	msg string
	kv  map[string]string
}

// recordingSink collects what admind logged, so a test can assert on the
// structured context of one line rather than on a substring of a stream.
type recordingSink struct {
	mu      sync.Mutex
	entries []logEntry
}

func (s *recordingSink) Init(logr.RuntimeInfo)                {}
func (s *recordingSink) Enabled(int) bool                     { return true }
func (s *recordingSink) WithValues(...any) logr.LogSink       { return s }
func (s *recordingSink) WithName(string) logr.LogSink         { return s }
func (s *recordingSink) Error(_ error, msg string, kv ...any) { s.record(msg, kv) }
func (s *recordingSink) Info(_ int, msg string, kv ...any)    { s.record(msg, kv) }

func (s *recordingSink) record(msg string, kv []any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := logEntry{msg: msg, kv: map[string]string{}}
	for i := 0; i+1 < len(kv); i += 2 {
		e.kv[fmt.Sprint(kv[i])] = fmt.Sprint(kv[i+1])
	}
	s.entries = append(s.entries, e)
}

// find returns the one entry whose message contains want, failing if there is
// not exactly one — a test that silently matched the first of several would
// assert about a line it did not mean.
func (s *recordingSink) find(t *testing.T, want string) logEntry {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	var got []logEntry
	for _, e := range s.entries {
		if strings.Contains(e.msg, want) {
			got = append(got, e)
		}
	}
	require.Len(t, got, 1, "expected exactly one logged line containing %q", want)
	return got[0]
}

// values is every message and every logged value, for a leak sweep.
func (s *recordingSink) values() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, e := range s.entries {
		out = append(out, e.msg)
		for _, v := range e.kv {
			out = append(out, v)
		}
	}
	return out
}

// TestChannelHandoff_TheBeginLogNamesWhatIsAboutToBeCreatedWithoutAnswers.
//
// The next thing that happens after this response is a real App on somebody's
// organization, and the pending setup that could match its callback lives only
// in this process's memory. A restart in between orphans that App, and no API
// lists a user's apps afterwards — so this log line is the only durable record
// that it was created, and it has to say what and where.
//
// It must name those in the KIND's own words. admind does not know what an
// "org" is: reading answers["org"] here would be the `if kind == "github"` this
// repo refuses, spelled as a map key. So the assertion is that the kind's own
// destination address and Explain reached the line — which is what carries the
// organization for github, and would carry whatever the next kind's equivalent
// is without this package learning a second vocabulary.
func TestChannelHandoff_TheBeginLogNamesWhatIsAboutToBeCreated(t *testing.T) {
	c := publishedCluster(t)
	a, _ := newHandoffAdmind(t, c, handoffWizard{})
	sink := &recordingSink{}
	a.cfg.Logger = logr.New(sink)

	token := handoffTokenFor(t, a)
	_, nonce := beginHandoff(t, a, token)

	got := sink.find(t, "channel handoff begun")
	assert.Equal(t, "demo-ns", got.kv["namespace"])
	assert.Equal(t, "demo-agent-bento", got.kv["channel"])
	assert.Equal(t, "bento", got.kv["kind"])
	assert.Equal(t, "YWRtaW4", got.kv["subject"])
	assert.Equal(t, token, got.kv["setupToken"],
		"the token is what correlates this line with the exchange and creation lines after it")
	assert.NotContains(t, got.kv["destination"], "demo-org")
	assert.NotContains(t, got.kv["explain"], "demo-org")
	assert.Contains(t, got.kv["destination"], "redacted",
		"a pluggable kind's destination is diagnostic text and must not echo submitted answers into logs")
	assert.Contains(t, got.kv["explain"], "redacted")
	assert.Contains(t, got.kv["callback"], "/agents/channel-handoff/callback",
		"where it was supposed to come back to is half of what makes an orphan diagnosable")

	// The nonce is the secret whose whole job is to be unguessable to whoever
	// delivers the callback. Swept across EVERY value of EVERY line, not just
	// this one: the begin URL carries it, and logging that URL anywhere would
	// hand it to any log reader.
	require.NotEmpty(t, nonce)
	for _, v := range sink.values() {
		assert.NotContains(t, v, nonce, "the CSRF nonce must not reach the logs")
	}
}

// TestChannelHandoff_TheSpecIsNotReAskedAfterTheExchange.
//
// Wizard.Handoff may do I/O and nothing in the contract says it is idempotent,
// so calling it again on the submit that follows a callback is a second chance
// to answer differently — and the answer that matters is already held: the
// stored spec is the one whose Complete produced the derived answers, closures
// and captured state included. A fresh one would know nothing about the App
// that now exists.
func TestChannelHandoff_TheSpecIsNotReAskedAfterTheExchange(t *testing.T) {
	c := publishedCluster(t)
	a, calls := newHandoffAdmind(t, c, handoffWizard{})
	token := handoffTokenFor(t, a)
	_, state := beginHandoff(t, a, token)

	cb := getCallback(t, a, "user:YWRtaW4", url.Values{"code": {"exchange-code"}, "state": {state}})
	require.Equal(t, http.StatusOK, cb.Code, "body: %s", cb.Body.String())
	before := calls.handoffCount
	require.Positive(t, before, "the fixture must have been asked at least once, or this counts nothing")

	done := postChannelSetup(t, a, token, "user:YWRtaW4", map[string]string{"installation-id": "4242"})
	require.Equal(t, http.StatusOK, done.Code, "body: %s", done.Body.String())
	assert.Equal(t, before, calls.handoffCount,
		"the submit after a callback must reuse the spec the exchange ran against, not ask for a new one")

	// And the run still finished THROUGH that spec: the credential only the
	// stored Complete produced is on the Secret.
	var sec corev1.Secret
	require.NoError(t, c.Get(context.Background(),
		client.ObjectKey{Namespace: "demo-ns", Name: "demo-agent-bento-creds"}, &sec))
	assert.Equal(t, fixturePrivateKey, string(sec.Data["private-key"]))
}

// TestChannelHandoff_ABegunRunThatNeverCameBackReAsksTheKind is the other half
// of the rule above, and the reason it is scoped to "after the exchange" rather
// than "whenever a spec is stored": nothing has been exchanged, so the kind is
// entitled to answer again against the current seeds.
func TestChannelHandoff_ABegunRunThatNeverCameBackReAsksTheKind(t *testing.T) {
	c := publishedCluster(t)
	a, calls := newHandoffAdmind(t, c, handoffWizard{})
	token := handoffTokenFor(t, a)

	_, _ = beginHandoff(t, a, token)
	before := calls.handoffCount

	// The browser never came back; the operator starts over.
	_, _ = beginHandoff(t, a, token)
	assert.Greater(t, calls.handoffCount, before)
}

// --- concurrency on one pending setup ---------------------------------------------

// TestChannelHandoff_ConcurrentCallbacksSpendTheCodeExactlyOnce.
//
// A browser delivers one redirect more than once in ordinary use: a prefetch
// plus the navigation the operator actually made, a refresh, a link opened
// twice. Both arrive on the same nonce, at the same time.
//
// TWO PROPERTIES, and the second is why this test runs under -race in CI:
//
//   - Exactly ONE exchange. Matching the nonce and spending it are one
//     operation inside the store, so the loser finds nothing to claim. Split
//     into a lookup followed by an assignment, both callers match before either
//     clears and both spend the code at the external service.
//   - No data race, and in particular no concurrent write to handoffAnswers.
//     Two goroutines merging into one map is `fatal error: concurrent map
//     writes` — not a flaky wrong answer but an unrecoverable crash that takes
//     the operator process, and admind with it.
//
// The fixture's Complete blocks on a gate until both requests are in flight, so
// the interleaving is forced rather than hoped for.
func TestChannelHandoff_ConcurrentCallbacksSpendTheCodeExactlyOnce(t *testing.T) {
	c := publishedCluster(t)

	const callers = 8
	var entered sync.WaitGroup
	entered.Add(1)
	release := make(chan struct{})
	var completes atomic.Int64

	w := handoffWizard{onComplete: func() {
		// The first exchange to arrive holds the door open until every other
		// caller has had its chance to claim, so a serialised run cannot pass
		// this test by accident.
		if completes.Add(1) == 1 {
			entered.Done()
			<-release
		}
	}}
	a, calls := newHandoffAdmind(t, c, w)
	token := handoffTokenFor(t, a)
	_, state := beginHandoff(t, a, token)

	codes := make(chan int, callers)
	var running sync.WaitGroup
	for range callers {
		running.Add(1)
		go func() {
			defer running.Done()
			rec := getCallback(t, a, "user:YWRtaW4", url.Values{"code": {"exchange-code"}, "state": {state}})
			codes <- rec.Code
		}()
	}
	// Let the first exchange finish only once it has actually started, so the
	// others race the claim rather than queueing behind a completed one.
	entered.Wait()
	close(release)
	running.Wait()
	close(codes)

	ok, refused := 0, 0
	for code := range codes {
		switch code {
		case http.StatusOK:
			ok++
		case http.StatusBadRequest:
			refused++
		default:
			t.Fatalf("unexpected callback status %d", code)
		}
	}
	assert.Equal(t, 1, ok, "exactly one delivery may complete the round trip")
	assert.Equal(t, callers-1, refused, "every other delivery must be refused, not silently dropped")
	assert.Equal(t, 1, calls.completeCount,
		"the exchange code is single-use at the external service; spending it twice is the failure")
}

// TestChannelSetupStore_ConcurrentUseOfOneSetupIsSerialised drives the store's
// own operations from many goroutines at once.
//
// It is the guard for the fields the HTTP test above does not reach — the seeds
// a submit refreshes and the answers a callback merges — and it exists because
// the record used to be handed out BY POINTER while the store's own mutex
// guarded the map it lived in. Every read and write off that pointer was
// unsynchronised; only -race says so.
func TestChannelSetupStore_ConcurrentUseOfOneSetupIsSerialised(t *testing.T) {
	s := newChannelSetupStore()
	token, err := s.put(&pendingChannelSetup{
		owner: identity.CanonicalFromTrusted("YWRtaW4", "test fixture"), namespace: "demo-ns", agentClass: "demo-class",
		required:       oap.RequiredChannel{Kind: "bento", Name: "demo-channel"},
		seeded:         map[string]string{"name": "demo-channel"},
		handoffAnswers: map[string]string{},
	})
	require.NoError(t, err)

	var wg sync.WaitGroup
	for i := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			switch i % 4 {
			case 0:
				got, err := s.get(token, identity.CanonicalFromTrusted("YWRtaW4", "test fixture"))
				assert.NoError(t, err)
				// Written to, to prove the copy is the caller's own: a shared
				// map here is the crash this test exists for.
				got.handoffAnswers["mine"] = "yes"
			case 1:
				_, err := s.update(token, identity.CanonicalFromTrusted("YWRtaW4", "test fixture"), func(p *pendingChannelSetup) {
					p.handoffAnswers[fmt.Sprint(i)] = "v"
				})
				assert.NoError(t, err)
			case 2:
				_, _ = s.claimHandoffState("never-set", identity.CanonicalFromTrusted("YWRtaW4", "test fixture"))
			default:
				_, err := s.update(token, identity.CanonicalFromTrusted("YWRtaW4", "test fixture"), func(p *pendingChannelSetup) {
					p.seeded = map[string]string{"name": "demo-channel"}
				})
				assert.NoError(t, err)
			}
		}()
	}
	wg.Wait()

	got, err := s.get(token, identity.CanonicalFromTrusted("YWRtaW4", "test fixture"))
	require.NoError(t, err)
	assert.NotContains(t, got.handoffAnswers, "mine",
		"a caller writing into what get() returned must not reach the record the store still owns")
}

// TestChannelSetupStore_TheBoundIsPerOperator.
//
// The refusal has always said "on this operator" and the count was global, so
// one admin's form-rendering loop denied EVERY other admin for up to a TTL,
// with a message pointing them at setups that were not theirs. The old test
// used a single owner and structurally could not see it.
func TestChannelSetupStore_TheBoundIsPerOperator(t *testing.T) {
	s := newChannelSetupStore()
	s.max = 2

	for i := range s.max {
		_, err := s.put(&pendingChannelSetup{
			owner: identity.CanonicalFromTrusted("YWRtaW4", "test fixture"), namespace: "demo-ns", agentClass: "demo-class",
			required: oap.RequiredChannel{Kind: "bento", Name: fmt.Sprintf("demo-channel-%d", i)},
		})
		require.NoError(t, err)
	}
	_, err := s.put(&pendingChannelSetup{
		owner: identity.CanonicalFromTrusted("YWRtaW4", "test fixture"), namespace: "demo-ns", agentClass: "demo-class",
		required: oap.RequiredChannel{Kind: "bento", Name: "demo-channel-overflow"},
	})
	require.ErrorIs(t, err, errTooManyPendingSetups, "the owner at its own ceiling is refused")

	// A DIFFERENT operator is unaffected. This is the assertion the single-owner
	// fixture could not make.
	other, err := s.put(&pendingChannelSetup{
		owner: identity.CanonicalFromTrusted("YWRtaW4y", "test fixture"), namespace: "demo-ns", agentClass: "demo-class",
		required: oap.RequiredChannel{Kind: "bento", Name: "demo-channel-theirs"},
	})
	require.NoError(t, err, "one operator's pending work must not deny another's")
	assert.NotEmpty(t, other)
}

// --- what the guidance may carry ---------------------------------------------------

// TestChannelHandoff_GuidanceCarryingAMintedCredentialIsWithheld.
//
// Complete runs server-side precisely so the App's private key and webhook
// secret never enter a browser — and the guidance is then built from those same
// answers and put in an HTTP response. This package already refuses to echo
// WizardOutput.Summary for exactly this reason ("echoing it would make this
// route's safety a property of every kind's future edits"); the same obligation
// applies here, and over HTTP the exposure is new — the CLI hands the same
// guidance to a terminal.
//
// The probe kind's guidance names the PEM, which no fallback question declares.
// The response must carry the refusal, not the key.
func TestChannelHandoff_GuidanceCarryingAMintedCredentialIsWithheld(t *testing.T) {
	c := publishedCluster(t)
	a, _ := newHandoffAdmind(t, c, handoffWizard{
		guidance: func(answers map[string]string) (string, error) {
			return "Finish by hand using these values:\n  private-key = " + answers["private-key"], nil
		},
	})
	token := handoffTokenFor(t, a)
	_, state := beginHandoff(t, a, token)

	cb := getCallback(t, a, "user:YWRtaW4", url.Values{"code": {"exchange-code"}, "state": {state}})
	require.Equal(t, http.StatusOK, cb.Code, "the questions are the floor and must still be asked")

	var body channelHandoffCallbackResponse
	require.NoError(t, json.Unmarshal(cb.Body.Bytes(), &body))
	assert.Empty(t, body.Guidance)
	assert.Contains(t, body.GuidanceError, "private-key", "the operator is owed the reason, naming the answer")
	assert.NotEmpty(t, body.Questions, "withholding prose must not withhold the questions")

	// Structural, over the WHOLE body: not a sweep for one header line, which
	// would miss the webhook secret and would fail a correct implementation
	// whose prompts legitimately name a prefix.
	whole := cb.Body.String()
	for label, minted := range map[string]string{
		"the App's private key": fixturePrivateKey,
		"the webhook secret":    fixtureWebhookSecret,
	} {
		assert.NotContains(t, whole, minted, "%s reached the browser through the guidance", label)
	}
}

func TestChannelHandoff_GuidanceFromProviderCannotEchoDerivedAnswers(t *testing.T) {
	c := publishedCluster(t)
	a, _ := newHandoffAdmind(t, c, handoffWizard{
		guidance: func(answers map[string]string) (string, error) {
			// app-id is declared as a QString fallback question, so the kind has
			// said it is a value an operator reads and types.
			return "Install the App at https://service.invalid/apps/" + answers["app-id"] + "/installations/new", nil
		},
	})
	token := handoffTokenFor(t, a)
	_, state := beginHandoff(t, a, token)

	cb := getCallback(t, a, "user:YWRtaW4", url.Values{"code": {"exchange-code"}, "state": {state}})
	require.Equal(t, http.StatusOK, cb.Code)

	var body channelHandoffCallbackResponse
	require.NoError(t, json.Unmarshal(cb.Body.Bytes(), &body))
	assert.Empty(t, body.GuidanceError)
	assert.NotContains(t, body.Guidance, fixtureAppID)
	assert.Contains(t, body.Guidance, "redacted")
}

// TestGuidanceLeak_ReadsTheKindsOwnDeclaration exercises the rule directly, so
// the classification is pinned without a round trip per case.
func TestGuidanceLeak_ReadsTheKindsOwnDeclaration(t *testing.T) {
	spec := &channelkinds.HandoffSpec{
		FallbackInputs: []oap.Question{
			{Name: "app-id", Type: oap.QString, Prompt: "App ID"},
			{Name: "webhook-secret", Type: oap.QSecret, Prompt: "Webhook secret"},
			{Name: "private-key-path", Type: oap.QString, Prompt: "Path to the key file"},
		},
	}
	derived := map[string]string{
		"app-id":         "APP-42",
		"slug":           "demo-app",
		"webhook-secret": "whsec-abc",
		"private-key":    "-----BEGIN RSA PRIVATE KEY-----x",
		"blank":          "",
	}

	cases := []struct {
		name     string
		guidance string
		want     string
	}{
		{name: "a plain declared question's value: publishable", guidance: "install APP-42 now", want: ""},
		{name: "a QSecret question's value: withheld, named", guidance: "secret is whsec-abc", want: "webhook-secret"},
		{
			name:     "a derived key with no question at all: withheld, named",
			guidance: "key: -----BEGIN RSA PRIVATE KEY-----x",
			want:     "private-key",
		},
		{
			// The kind's manual route asks for a PATH; the exchange returns the
			// KEY. Nothing declares "slug" here either, so it is not publishable
			// in THIS spec — the rule reads the declaration, not a global list.
			name:     "a derived key the spec does not declare: withheld even though it looks harmless",
			guidance: "https://service.invalid/apps/demo-app", want: "slug",
		},
		{name: "clean guidance", guidance: "nothing sensitive here", want: ""},
		{
			name:     "an empty derived value never matches",
			guidance: "anything at all",
			want:     "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, guidanceLeak(spec, derived, tc.guidance))
		})
	}
}

// TestGuidanceLeak_TheRealGithubKindsPostExchangeGuidanceSurvives.
//
// The one production kind with a handoff, against the real check. Its
// post-exchange guidance IS the install URL for the App just created, built
// around the App's SLUG — a derived answer. A blanket "no derived value may
// appear" would blank the only instruction the operator needs on the happy
// path, so this asserts the rule lets it through while the PEM and the webhook
// secret it was handed alongside stay out.
//
// Against the registered kind, not a fixture: the point is that the RULE and
// this KIND agree, and a fixture restating github's declaration would only
// prove the rule agrees with itself.
func TestGuidanceLeak_TheRealGithubKindsPostExchangeGuidanceSurvives(t *testing.T) {
	kind, ok := registry.Get("github")
	require.True(t, ok, "the github kind must be registered in this test binary")

	// WorkingDir is empty, which is admind's own answer and what keeps this
	// test from writing a reference manifest to disk.
	in := channelkinds.WizardInput{Namespace: "demo-ns"}
	spec, err := kind.Wizard().Handoff(context.Background(), in)
	require.NoError(t, err)
	require.NotNil(t, spec)
	require.NotNil(t, spec.FallbackGuidance)

	derived := map[string]string{
		"app-id":         "424242",
		"slug":           "demo-agent-gh",
		"private-key":    fixturePrivateKey,
		"webhook-secret": fixtureWebhookSecret,
	}
	answers := map[string]string{
		"org":               "demo-org",
		"name":              "demo-agent-gh",
		"external-base-url": "https://ap.demo.invalid",
	}
	maps.Copy(answers, derived)

	guidance, err := spec.FallbackGuidance(answers)
	require.NoError(t, err)
	require.NotEmpty(t, guidance)
	require.Contains(t, guidance, "demo-agent-gh",
		"the fixture must be the POST-EXCHANGE branch, whose text embeds the slug — "+
			"otherwise this asserts nothing about the case that matters")

	assert.Empty(t, guidanceLeak(spec, derived, guidance),
		"the real kind's happy-path guidance must survive the check; blanking it would "+
			"delete the install URL, which is the whole instruction")
	assert.NotContains(t, guidance, fixturePrivateKey)
	assert.NotContains(t, guidance, fixtureWebhookSecret)
}
