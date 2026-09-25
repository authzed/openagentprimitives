package channelwizard

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/github"
	"github.com/authzed/openagentprimitives/pkg/cli/tui"
	"github.com/authzed/openagentprimitives/pkg/platform/oap"
)

// ---------------------------------------------------------------------------
// fixtures
//
// Nothing here reaches the network or launches a browser: the "browser" is a
// function that fetches the loopback page runHandoff served and POSTs its
// form at an httptest server standing in for the external service.
// ---------------------------------------------------------------------------

var (
	formActionPattern = regexp.MustCompile(`action="([^"]+)"`)
	formFieldPattern  = regexp.MustCompile(`name="([^"]+)" value="([^"]*)"`)
)

// fakeService stands in for the external service the operator is sent to: it
// reads the callback out of the POSTed body (the way GitHub reads redirect_url
// out of the manifest) and redirects there with a code plus whatever state it
// was handed.
func fakeService(t *testing.T, code string) *httptest.Server {
	t.Helper()
	return fakeServiceReading(t, code, "callback")
}

// fakeServiceReading is fakeService with the manifest field the callback URL
// arrives in named explicitly. github calls it `redirect_url`, and a test
// driving github's real Begin has to be answered by a service that reads what
// that Begin actually writes.
func fakeServiceReading(t *testing.T, code, callbackField string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, r.ParseForm())
		var doc map[string]any
		require.NoError(t, json.Unmarshal([]byte(r.FormValue("manifest")), &doc))
		callback, _ := doc[callbackField].(string)
		require.NotEmptyf(t, callback, "the manifest must carry the client's callback URL under %q", callbackField)
		http.Redirect(w, r, callback+"?code="+url.QueryEscape(code)+"&state="+url.QueryEscape(r.URL.Query().Get("state")),
			http.StatusFound)
	}))
}

// fakeBrowser plays the part of a real browser loading runHandoff's
// self-submitting page: GET it, extract the form's action and hidden fields,
// POST them, and follow the redirect back to the loopback listener.
//
// rewriteHost points the form's action — which names the external service —
// at the httptest server standing in for it.
func fakeBrowser(t *testing.T, rewriteHost string, mutate func(u *url.URL)) func(string) error {
	t.Helper()
	return func(startURL string) error {
		resp, err := http.Get(startURL) //nolint:noctx // test-only stand-in for a browser
		if err != nil {
			return fmt.Errorf("fake browser: GET start page: %w", err)
		}
		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			return fmt.Errorf("fake browser: read start page: %w", err)
		}

		am := formActionPattern.FindSubmatch(body)
		if am == nil {
			return fmt.Errorf("fake browser: no form action in:\n%s", body)
		}
		action, err := url.Parse(html.UnescapeString(string(am[1])))
		if err != nil {
			return fmt.Errorf("fake browser: parse action: %w", err)
		}
		form := url.Values{}
		for _, m := range formFieldPattern.FindAllSubmatch(body, -1) {
			form.Set(html.UnescapeString(string(m[1])), html.UnescapeString(string(m[2])))
		}

		target, err := url.Parse(rewriteHost)
		if err != nil {
			return fmt.Errorf("fake browser: parse rewrite host: %w", err)
		}
		target.Path = action.Path
		target.RawQuery = action.RawQuery
		if mutate != nil {
			mutate(target)
		}

		resp2, err := http.PostForm(target.String(), form) //nolint:noctx // test-only
		if err != nil {
			return fmt.Errorf("fake browser: POST form: %w", err)
		}
		return resp2.Body.Close()
	}
}

// demoHandoff is a spec shaped like github's: a POST body carrying the
// client's callback, an exchange that turns the code into answers, and a
// fallback for when none of that works.
func demoHandoff(t *testing.T, serviceURL string) *channelkinds.HandoffSpec {
	t.Helper()
	return &channelkinds.HandoffSpec{
		Begin: func(answers map[string]string, callbackURL string) (channelkinds.HandoffStart, error) {
			return channelkinds.HandoffStart{
				Explain: "Opening " + answers["org"] + "'s app page in your browser.",
				URL:     serviceURL + "/organizations/" + answers["org"] + "/apps/new",
				FormFields: map[string]string{
					"manifest": `{"name":"demo-org-demo-channel","callback":` + jsonQuote(callbackURL) + `}`,
				},
			}, nil
		},
		Complete: func(_ context.Context, cb url.Values) (map[string]string, error) {
			code := cb.Get("code")
			if code == "" {
				return nil, errors.New("the callback carried no exchange code")
			}
			return map[string]string{"app-id": "from-" + code}, nil
		},
		FallbackInputs: []oap.Question{
			{Name: "app-id", Type: oap.QString, Prompt: "App ID"},
			{Name: "installation-id", Type: oap.QString, Prompt: "Installation ID"},
		},
	}
}

// jsonQuote quotes a string for the hand-built manifest above.
func jsonQuote(s string) string {
	raw, _ := json.Marshal(s)
	return string(raw)
}

// handoffParams is the common set every test below varies one thing in.
func handoffParams(t *testing.T, spec *channelkinds.HandoffSpec, open func(string) error, script string) (handoffRun, *tui.State) {
	t.Helper()
	st := tui.NewState()
	th := tui.NewTheme(tui.Caps{})
	return handoffRun{
		spec:    spec,
		answers: map[string]string{"org": "demo-org"},
		seeded:  map[string]string{},
		state:   st,
		opts:    tui.Options{Theme: th, Driver: tui.Plain(strings.NewReader(script), io.Discard, th)},
		// Short, and not the production five minutes: every test here either
		// completes the round trip or fails it outright, so reaching the
		// timeout at all means a regression — and one that hangs the whole
		// package for minutes is far worse to diagnose than one that fails.
		timeout:     15 * time.Second,
		openBrowser: open,
	}, st
}

// ---------------------------------------------------------------------------
// the happy path
// ---------------------------------------------------------------------------

func TestRunHandoff_ExchangesTheCallbackForAnswers(t *testing.T) {
	svc := fakeService(t, "abc")
	t.Cleanup(svc.Close)

	run, _ := handoffParams(t, demoHandoff(t, svc.URL), fakeBrowser(t, svc.URL, nil), "67890\n")
	answers, err := runHandoff(context.Background(), run)
	require.NoError(t, err)

	assert.Equal(t, "from-abc", answers["app-id"],
		"the exchange's answers are what the run continues with")
	assert.Equal(t, "67890", answers["installation-id"],
		"a fallback question the exchange did not answer is still asked")
}

// TestRunHandoff_NamesTheDestinationInFullAndNeverTheNonce.
//
// A kind whose handoff needs a POST body cannot be reached by a link, so the
// browser is pointed at a local page that submits one — and the only address
// the operator ever sees is a loopback port. When the launch opens the wrong
// browser profile, or the tab is closed, or the page that arrives is not the
// one this run built, there is nothing on screen to go back to. So the SERVICE
// address is printed too, before the wait begins: after the callback returns is
// after the only moment anyone could have used it.
//
// It is printed on a line of its own, whole. A URL that is wrapped or elided is
// no longer something a terminal can turn into a click target or a reader can
// retype, which is the entire reason it is there.
//
// And it is the address WITHOUT the CSRF nonce. The nonce is the one secret in
// this flow whose job is to be unguessable to whoever delivers the callback,
// and a terminal's scrollback outlives the run.
func TestRunHandoff_NamesTheDestinationInFullAndNeverTheNonce(t *testing.T) {
	var mu sync.Mutex
	var roundTrippedNonce string

	svc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, r.ParseForm())
		var doc map[string]any
		require.NoError(t, json.Unmarshal([]byte(r.FormValue("manifest")), &doc))
		callback, _ := doc["callback"].(string)
		require.NotEmpty(t, callback, "the manifest must carry the client's callback URL")
		state := r.URL.Query().Get("state")
		mu.Lock()
		roundTrippedNonce = state
		mu.Unlock()
		http.Redirect(w, r, callback+"?code=abc&state="+url.QueryEscape(state), http.StatusFound)
	}))
	t.Cleanup(svc.Close)

	var printed bytes.Buffer
	run, _ := handoffParams(t, demoHandoff(t, svc.URL), fakeBrowser(t, svc.URL, nil), "67890\n")
	run.opts.Out = &printed
	_, err := runHandoff(context.Background(), run)
	require.NoError(t, err)

	got := printed.String()
	destination := svc.URL + "/organizations/demo-org/apps/new"
	assert.Contains(t, got, "\n"+destination+"\n",
		"the destination must be on a line of its own, unwrapped and unelided, so it stays legible")
	assert.Regexp(t, `\nhttp://127\.0\.0\.1:\d+/\n`, got,
		"the loopback page is named too, on a line of its own: re-opening IT is what re-POSTs the body")
	assert.Contains(t, got, "NOT a retry",
		"and the destination must say it is not the thing to open — see the control below for why")

	mu.Lock()
	nonce := roundTrippedNonce
	mu.Unlock()
	// The control on the sweep below. With an empty nonce a NotContains passes
	// against any output at all, which would pin nothing.
	require.NotEmpty(t, nonce, "the client sets the nonce on the address it sends the operator to")
	assert.NotContains(t, got, nonce, "the CSRF nonce must not reach the terminal")
	assert.NotContains(t, got, "state=",
		"and neither must the query parameter that carries it, whatever its value")
}

// bareURLLines returns every line of s that is nothing but a URL.
//
// The handoff prints each address on a line of its own precisely so it stays
// something a terminal can linkify and a reader can retype, which makes "the
// lines that are just a URL" the addresses this run offered — and their ORDER
// the order it offered them in.
func bareURLLines(s string) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "http://") || strings.HasPrefix(line, "https://") {
			out = append(out, line)
		}
	}
	return out
}

// TestRunHandoff_TheAddressOfferedAsTheRetryIsOneThatActuallyWorks is the
// control the printed-in-full assertion above was not: that one says both
// addresses reach the terminal whole, and says nothing about WHICH of them the
// operator is being told to open.
//
// They are not interchangeable and the difference is total. The loopback page
// re-serves the self-submitting form, so opening it again re-POSTs the
// manifest. The service address is the POST TARGET: the manifest travels in
// the form body, so navigating there by hand is a GET, and GitHub renders an
// empty form — every time, for as long as the operator keeps trying. Offering
// the second as the thing to click is a dead end that looks like a remedy,
// which is exactly how it shipped.
//
// So this does not assert on the wording. It takes the address the run offers
// FIRST — the framing contract being that the actionable one leads, ahead of
// the one that merely says where you end up — and drives a browser at THAT,
// requiring the handoff to complete from it. Under the defect the browser is
// pointed at the POST endpoint with no body and the run cannot finish.
func TestRunHandoff_TheAddressOfferedAsTheRetryIsOneThatActuallyWorks(t *testing.T) {
	svc := fakeService(t, "abc")
	t.Cleanup(svc.Close)

	var printed bytes.Buffer
	var offered []string

	// Ignores the address it is handed and opens whatever the TERMINAL told
	// the operator to open — which is the whole question under test. explain
	// runs before this, so the buffer already holds the text they would read.
	openWhatTheOperatorWasTold := func(string) error {
		offered = bareURLLines(printed.String())
		if len(offered) == 0 {
			return errors.New("the run offered the operator no address at all")
		}
		return fakeBrowser(t, svc.URL, nil)(offered[0])
	}

	run, _ := handoffParams(t, demoHandoff(t, svc.URL), openWhatTheOperatorWasTold, "67890\n")
	run.opts.Out = &printed
	answers, err := runHandoff(context.Background(), run)

	require.NoError(t, err,
		"the address this run tells the operator to open must be one the handoff can complete from")
	assert.Equal(t, "from-abc", answers["app-id"],
		"and completing from it must produce the exchange's answers, not the fallback's")

	require.Len(t, offered, 2, "both addresses are offered: the retry target and the destination")
	assert.Regexp(t, `^http://127\.0\.0\.1:\d+/$`, offered[0],
		"the address offered first must be the loopback page — the only one that re-sends the body")
	assert.Equal(t, svc.URL+"/organizations/demo-org/apps/new", offered[1],
		"and the POST target comes second, as where you end up rather than what to open")
}

// TestRunHandoff_ADirectNavigationOffersNoRetryRatherThanLeakingTheNonce is the
// OTHER shape of handoff — a kind whose Begin declares no FormFields, so the
// browser is pointed straight at the service and no loopback page stands in
// front of it.
//
// There is no address this run can honestly offer there. The only one that
// works is the one carrying the CSRF nonce, and a terminal's scrollback
// outlives the run; a nonce-stripped copy would be worse than useless, because
// following it produces a callback the constant-time compare rejects and sends
// the run down its fallback for a reason the operator caused by doing as they
// were told. So it says the step cannot be re-opened by hand, names where the
// browser is going, and prints no nonce.
//
// No kind ships this shape today. It is covered because the branch exists and
// because what it must NOT print is a secret — a combination that is exactly
// how a leak reaches a release unnoticed.
func TestRunHandoff_ADirectNavigationOffersNoRetryRatherThanLeakingTheNonce(t *testing.T) {
	var mu sync.Mutex
	var opened string

	spec := &channelkinds.HandoffSpec{
		Begin: func(map[string]string, string) (channelkinds.HandoffStart, error) {
			return channelkinds.HandoffStart{
				Explain: "Opening the service to authorize this channel.",
				URL:     "https://service.demo.test/authorize",
			}, nil
		},
		Complete: func(context.Context, url.Values) (map[string]string, error) {
			return map[string]string{"app-id": "direct"}, nil
		},
		FallbackInputs: []oap.Question{{Name: "app-id", Type: oap.QString, Prompt: "App ID"}},
	}

	var printed bytes.Buffer
	run, _ := handoffParams(t, spec, func(u string) error {
		mu.Lock()
		opened = u
		mu.Unlock()
		return errors.New("no browser here: this test is about what was PRINTED, not the round trip")
	}, "12345\n")
	run.opts.Out = &printed
	// The browser fails, so the run takes its detour and the scripted answer
	// above completes it — which is the documented behaviour of a handoff that
	// did not happen, and not what is under test. What matters is the text
	// written before that.
	answers, err := runHandoff(context.Background(), run)
	require.NoError(t, err, "a failed handoff with a fallback is a detour, not the end of the run")
	assert.Equal(t, "12345", answers["app-id"], "the detour is what supplied the answer")

	got := printed.String()
	assert.Contains(t, got, "\nhttps://service.demo.test/authorize\n",
		"where the browser is being sent is still named, in full and on a line of its own")
	assert.Contains(t, got, "cannot be re-opened by hand",
		"and the operator is told there is nothing here for them to click, rather than being handed a dud")

	mu.Lock()
	sent := opened
	mu.Unlock()
	// The control on the sweep: the address actually opened DOES carry the
	// nonce, so a NotContains against it is a claim about something real.
	require.Contains(t, sent, "state=", "precondition: the address the browser is sent to carries the nonce")
	nonce := strings.TrimPrefix(sent, "https://service.demo.test/authorize?state=")
	require.NotEmpty(t, nonce)
	assert.NotContains(t, got, nonce, "the CSRF nonce must not reach the terminal on this branch either")
	assert.NotContains(t, got, "state=", "nor the parameter carrying it")
}

// TestRunHandoff_AsksNothingItAlreadyHasTheAnswerTo is driven against GITHUB'S
// OWN SPEC, not a local double, and that is the point of it.
//
// A double whose fallback question is named exactly what its exchange returns
// cannot express the case this filter exists for. github's can, and does: its
// manual route asks for `private-key-path` while a completed exchange returns
// `private-key`, so a filter keyed on the question's own name leaves the
// operator being asked for a .pem file that does not exist — on the primary
// happy path of the only kind with a handoff — and, in a scripted run, eats
// the answer meant for the next question.
//
// Only Complete is substituted, because the real one would reach
// api.github.com. Everything the filter reads — FallbackInputs, SatisfiedBy —
// is the kind's own, so a change to either reddens this.
func TestRunHandoff_AsksNothingItAlreadyHasTheAnswerTo(t *testing.T) {
	svc := fakeServiceReading(t, "abc", "redirect_url")
	t.Cleanup(svc.Close)

	spec := githubHandoffSpec(t)
	// Exactly the four keys github's Complete returns, from the same fixture
	// shape its Conversion carries.
	spec.Complete = func(context.Context, url.Values) (map[string]string, error) {
		return map[string]string{
			"app-id":         "55555",
			"slug":           "demo-org-demo-reviewbot-gh",
			"private-key":    "-----BEGIN RSA PRIVATE KEY-----\nZmFrZQ==\n-----END RSA PRIVATE KEY-----\n",
			"webhook-secret": "whsec_faketestwebhooksecretvalue",
		}, nil
	}

	assert.Equal(t, []string{"installation-id"},
		questionNamesOf(spec.Unanswered(map[string]string{
			"app-id":         "55555",
			"slug":           "demo-org-demo-reviewbot-gh",
			"private-key":    "-----BEGIN RSA PRIVATE KEY-----\nZmFrZQ==\n-----END RSA PRIVATE KEY-----\n",
			"webhook-secret": "whsec_faketestwebhooksecretvalue",
		})),
		"after a successful exchange the ONLY thing left to ask is the installation ID, "+
			"which GitHub does not redirect back with")

	// And end to end, with a script of ONE line: a run that also asked for the
	// key file would consume "67890" as the path and then fail closed on the
	// installation ID it never reached. github's REAL Begin runs here, so the
	// stand-in service has to read the callback out of `redirect_url`, which
	// is where GitHub's manifest flow carries it.
	//
	// TWO ADDRESSES ARE OPENED ON THIS ROUTE and only one of them is a round
	// trip, so the browser is split accordingly. The loopback page is the
	// manifest POST and is driven by the fake browser; the second is github's
	// fire-and-forget installation page (HandoffSpec.FallbackOpenURL), which
	// is a REAL github.com address — driving the fake browser at it would put
	// a network call in this suite, which is the reason it is recorded rather
	// than fetched.
	var visited []string
	run, _ := handoffParams(t, spec, func(u string) error {
		if strings.HasPrefix(u, "http://127.0.0.1:") {
			return fakeBrowser(t, svc.URL, nil)(u)
		}
		visited = append(visited, u)
		return nil
	}, "67890\n")
	run.answers = map[string]string{
		"owner-type":        "organization",
		"org":               "demo-org",
		"external-base-url": "https://ap.demo.test",
		"name":              "demo-reviewbot-gh",
	}
	answers, err := runHandoff(context.Background(), run)
	require.NoError(t, err)
	assert.Equal(t, "55555", answers["app-id"])
	assert.Equal(t, "67890", answers["installation-id"])
	assert.NotContains(t, answers, "private-key-path",
		"a question the exchange answered under another name must not have been asked")

	// The App now exists, so the operator is put on the page that installs it
	// — the page whose "Configure" link carries the installation ID the very
	// next question asks for. It is opened and NOT waited on: GitHub redirects
	// an installation back only to an App declaring a Setup URL, and this
	// App's manifest declares none, which is why the ID is still asked above.
	assert.Equal(t, []string{"https://github.com/apps/demo-org-demo-reviewbot-gh/installations/new"}, visited,
		"the installation page is opened once the exchange has produced the App's slug")
}

// TestRunHandoff_SatisfiedByNamingNoQuestionIsRefused: the field exists to stop
// a question being asked after the exchange answered it under another name, so
// a key that names no question does nothing at all — silently, and in exactly
// the direction the field was added to fix.
func TestRunHandoff_SatisfiedByNamingNoQuestionIsRefused(t *testing.T) {
	svc := fakeService(t, "abc")
	t.Cleanup(svc.Close)

	spec := demoHandoff(t, svc.URL)
	spec.SatisfiedBy = map[string]string{"privat-key-path": "private-key"}

	run, _ := handoffParams(t, spec, func(string) error {
		t.Fatal("a malformed spec must be refused before a browser is opened")
		return nil
	}, "")
	_, err := runHandoff(context.Background(), run)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "privat-key-path")
}

// githubHandoffSpec is the real kind's handoff, for the tests that must not be
// able to pass against a double shaped to suit them.
func githubHandoffSpec(t *testing.T) *channelkinds.HandoffSpec {
	t.Helper()
	w, ok := github.Kind{}.Wizard().(channelkinds.Wizard)
	require.True(t, ok, "github must answer the wizard contract")

	spec, err := w.Handoff(context.Background(), channelkinds.WizardInput{Namespace: "default"})
	require.NoError(t, err)
	require.NotNil(t, spec, "an unseeded run has an App to create")
	return spec
}

func questionNamesOf(qs []oap.Question) []string {
	out := make([]string, 0, len(qs))
	for _, q := range qs {
		out = append(out, q.Name)
	}
	return out
}

// TestRunHandoff_SeededFallbackAnswersAreNotAsked: a --answer that named a
// fallback question is written into the run's State and read back, not
// re-prompted.
func TestRunHandoff_SeededFallbackAnswersAreNotAsked(t *testing.T) {
	svc := fakeService(t, "abc")
	t.Cleanup(svc.Close)

	run, st := handoffParams(t, demoHandoff(t, svc.URL), fakeBrowser(t, svc.URL, nil), "")
	run.seeded = map[string]string{"installation-id": "seeded-67890"}

	answers, err := runHandoff(context.Background(), run)
	require.NoError(t, err, "nothing was left to ask, so the empty script is not a truncated one")
	assert.Equal(t, "seeded-67890", answers["installation-id"])
	assert.Equal(t, "seeded-67890", st.Get("installation-id"),
		"a seeded answer lands in State the same way a typed one does")
}

// TestRunHandoff_RecordsWhatTheExchangeProducedOnTheRunsState: a handoff is
// the one step of a channel-create run that leaves something behind in the
// world. If the run then fails, Result is never reached and its Summary never
// exists — the run's State is all that is left to print, and an identifier
// that reached neither is an object the operator cannot name.
func TestRunHandoff_RecordsWhatTheExchangeProducedOnTheRunsState(t *testing.T) {
	svc := fakeService(t, "abc")
	t.Cleanup(svc.Close)

	run, st := handoffParams(t, demoHandoff(t, svc.URL), fakeBrowser(t, svc.URL, nil), "67890\n")
	_, err := runHandoff(context.Background(), run)
	require.NoError(t, err)

	assert.Contains(t, notesText(st), "app-id=from-abc",
		"the identifier the exchange minted has to survive a run that fails after it")
}

// TestRunHandoff_NeverRecordsACredentialTheExchangeReturned is the other half,
// and the security one: an exchange returns credentials alongside identifiers
// — github's returns the App's private key and its webhook secret next to its
// id — and a note reaches plain scrollback verbatim, where it outlives the
// run. This client cannot tell one from the other except by what the kind
// declared, so anything not declared readable contributes its NAME only.
func TestRunHandoff_NeverRecordsACredentialTheExchangeReturned(t *testing.T) {
	const rawKey = "-----BEGIN RSA PRIVATE KEY-----\nZmFrZQ==\n-----END RSA PRIVATE KEY-----\n"
	const rawSecret = "whsec_faketestwebhooksecretvalue"

	svc := fakeService(t, "abc")
	t.Cleanup(svc.Close)

	spec := demoHandoff(t, svc.URL)
	// A secret the kind DECLARED as one, and a second the kind never declared
	// at all — the two ways a value can arrive whose readability this client
	// has no business assuming.
	spec.FallbackInputs = append(spec.FallbackInputs,
		oap.Question{Name: "webhook-secret", Type: oap.QSecret, Prompt: "Webhook secret"})
	spec.Complete = func(_ context.Context, cb url.Values) (map[string]string, error) {
		return map[string]string{
			"app-id":         "from-" + cb.Get("code"),
			"webhook-secret": rawSecret,
			"private-key":    rawKey,
		}, nil
	}

	run, st := handoffParams(t, spec, fakeBrowser(t, svc.URL, nil), "67890\n")
	_, err := runHandoff(context.Background(), run)
	require.NoError(t, err)

	notes := notesText(st)
	assert.Contains(t, notes, "app-id=from-abc", "the identifier is still recorded")
	assert.Contains(t, notes, "webhook-secret", "and a secret is named, so the record is not merely silent")
	assert.NotContains(t, notes, rawSecret, "but a declared secret's VALUE must never reach scrollback")
	assert.NotContains(t, notes, rawKey, "and neither must a value no question declared at all")
}

// ---------------------------------------------------------------------------
// CSRF
// ---------------------------------------------------------------------------

// TestRunHandoff_AStateMismatchIsRefusedBeforeTheCodeIsRead is the CSRF gate:
// a callback that does not carry the nonce this run generated — a stale tab, a
// request forged by another page in the same browser — must not have its code
// exchanged. Asserting that Complete never RAN is the half that matters: an
// implementation that compared the state after reading the code would still
// have spent it.
func TestRunHandoff_AStateMismatchIsRefusedBeforeTheCodeIsRead(t *testing.T) {
	svc := fakeService(t, "abc")
	t.Cleanup(svc.Close)

	completed := false
	spec := demoHandoff(t, svc.URL)
	inner := spec.Complete
	spec.Complete = func(ctx context.Context, cb url.Values) (map[string]string, error) {
		completed = true
		return inner(ctx, cb)
	}

	// The "browser" reaches the service with a state nobody generated.
	forge := fakeBrowser(t, svc.URL, func(u *url.URL) {
		q := u.Query()
		q.Set("state", "not-the-state-this-run-made")
		u.RawQuery = q.Encode()
	})

	// Both fallback questions are asked, because the exchange answered
	// neither: the handoff failing is a detour, not the end of the run.
	run, st := handoffParams(t, spec, forge, "11111\n67890\n")
	answers, err := runHandoff(context.Background(), run)
	require.NoError(t, err, "a refused callback falls through to the questions that always work")

	assert.False(t, completed, "a callback whose state did not match must never be exchanged")
	assert.Equal(t, "11111", answers["app-id"])
	assert.Equal(t, "67890", answers["installation-id"])

	assert.Contains(t, notesText(st), "state",
		"the run's summary must say why the browser step was not used")
}

// ---------------------------------------------------------------------------
// the detour
// ---------------------------------------------------------------------------

func TestRunHandoff_FallsThroughToTheQuestionsWhenTheHandoffFails(t *testing.T) {
	cases := []struct {
		name    string
		spec    func(t *testing.T, svc *httptest.Server) *channelkinds.HandoffSpec
		open    func(t *testing.T, svc *httptest.Server) func(string) error
		wantSay string
	}{
		{
			name: "the browser will not open",
			spec: func(t *testing.T, svc *httptest.Server) *channelkinds.HandoffSpec {
				return demoHandoff(t, svc.URL)
			},
			open: func(*testing.T, *httptest.Server) func(string) error {
				return func(string) error { return errors.New("no browser on this host") }
			},
			wantSay: "no browser on this host",
		},
		{
			name: "the exchange is rejected",
			spec: func(t *testing.T, svc *httptest.Server) *channelkinds.HandoffSpec {
				s := demoHandoff(t, svc.URL)
				s.Complete = func(context.Context, url.Values) (map[string]string, error) {
					return nil, errors.New("the service rejected the exchange: status 422")
				}
				return s
			},
			open: func(t *testing.T, svc *httptest.Server) func(string) error {
				return fakeBrowser(t, svc.URL, nil)
			},
			wantSay: "status 422",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc := fakeService(t, "abc")
			t.Cleanup(svc.Close)

			run, st := handoffParams(t, tc.spec(t, svc), tc.open(t, svc), "11111\n67890\n")
			answers, err := runHandoff(context.Background(), run)
			require.NoError(t, err, "the manual questions are the floor that always works")
			assert.Equal(t, "11111", answers["app-id"])
			assert.Equal(t, "67890", answers["installation-id"])
			assert.Contains(t, notesText(st), tc.wantSay,
				"the reason the automated path was not used is recorded, not swallowed")
		})
	}
}

// TestRunHandoff_ASetupFailureEndsTheRunInsteadOfTakingTheDetour is the
// counterpart to the table above, and the line between them is what the
// fallback route means.
//
// A round trip that STARTED and did not come back is what the fallback is for:
// the address was fine, the browser or the service was not, and the operator
// can still supply by hand what the exchange would have produced. A kind that
// could not say where to send them in the first place is a different failure —
// it read the same answers the fallback's own guidance is built from, so
// entering the detour asks the kind to render instructions it has just proved
// it cannot. github's live case is a mistyped organization login: every
// question accepted it, Begin refused it, and the old behaviour turned that
// into five bare prompts for an App the operator must now create entirely by
// hand, with the real reason demoted to a note headed "the browser step did
// not finish".
//
// Both rows assert the same three things: the run ENDS, the message is the
// kind's own verbatim, and no fallback question was asked — the scripted
// answers are deliberately present, so a run that took the detour would
// consume them and return them rather than failing.
func TestRunHandoff_ASetupFailureEndsTheRunInsteadOfTakingTheDetour(t *testing.T) {
	cases := []struct {
		name    string
		begin   func(map[string]string, string) (channelkinds.HandoffStart, error)
		wantSay string
	}{
		{
			name: "Begin refuses a malformed answer: the run ends naming the field",
			begin: func(map[string]string, string) (channelkinds.HandoffStart, error) {
				return channelkinds.HandoffStart{}, errors.New(`github app manifest: org: "demo org" does not look like a GitHub organization login`)
			},
			wantSay: `org: "demo org" does not look like a GitHub organization login`,
		},
		{
			name: "Begin names no address at all: the run ends rather than detouring",
			begin: func(map[string]string, string) (channelkinds.HandoffStart, error) {
				return channelkinds.HandoffStart{}, nil
			},
			wantSay: "named no address to send the operator to",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc := fakeService(t, "abc")
			t.Cleanup(svc.Close)

			spec := demoHandoff(t, svc.URL)
			spec.Begin = tc.begin
			open := func(string) error {
				t.Fatal("no browser may be opened when the kind could not build the address")
				return nil
			}

			run, st := handoffParams(t, spec, open, "11111\n67890\n")
			answers, err := runHandoff(context.Background(), run)
			require.Error(t, err, "a setup failure must end the run, not spend the fallback route on it")
			assert.Contains(t, err.Error(), tc.wantSay,
				"the kind's own sentence is what the operator has to act on, so nothing may reword it")
			assert.Empty(t, answers, "a run that ended must not hand back half an answer set")
			assert.NotContains(t, notesText(st), "Browser handoff",
				"there is no detour to explain: the run stopped before one was taken")
		})
	}
}

// TestRunHandoff_TimesOutWhenTheCallbackNeverArrives: a browser that opened
// and was then abandoned — or one that never opened despite reporting success,
// which is every launcher that shells out — must not park the terminal
// forever.
func TestRunHandoff_TimesOutWhenTheCallbackNeverArrives(t *testing.T) {
	svc := fakeService(t, "abc")
	t.Cleanup(svc.Close)

	run, st := handoffParams(t, demoHandoff(t, svc.URL), func(string) error { return nil }, "11111\n67890\n")
	run.timeout = 50 * time.Millisecond

	answers, err := runHandoff(context.Background(), run)
	require.NoError(t, err, "a timeout is a detour into the questions, not the end of the run")
	assert.Equal(t, "11111", answers["app-id"])
	assert.Contains(t, notesText(st), "timed out")
}

// TestDeliverCallback_DoesNotBlockOnAFullChannel is the guard for a duplicate
// delivery — a page refresh, a second tab, a browser prefetch — arriving after
// the select has already consumed the first. A plain send would block that
// handler goroutine forever (nothing will ever receive a second value from a
// buffer-of-one already holding one), which in turn stalls the deferred
// shutdown of the listener.
//
// Proven against the helper rather than through the HTTP round trip, so a hang
// here fails in two seconds instead of wedging the package.
func TestDeliverCallback_DoesNotBlockOnAFullChannel(t *testing.T) {
	callbacks := make(chan handoffCallback, 1)
	deliverCallback(callbacks, handoffCallback{values: url.Values{"code": {"first"}}})

	done := make(chan struct{})
	go func() {
		deliverCallback(callbacks, handoffCallback{values: url.Values{"code": {"duplicate"}}})
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("deliverCallback blocked on a full channel — a duplicate callback would wedge the handler goroutine")
	}
	assert.Equal(t, "first", (<-callbacks).values.Get("code"),
		"a duplicate must be dropped, never overwrite the result already in hand")
}

// TestRunHandoff_AMandatoryHandoffThatFailsEndsTheRun: an empty FallbackInputs
// means the kind has no manual route, so a failure is the end of it rather
// than a detour into questions that do not exist.
func TestRunHandoff_AMandatoryHandoffThatFailsEndsTheRun(t *testing.T) {
	svc := fakeService(t, "abc")
	t.Cleanup(svc.Close)

	spec := demoHandoff(t, svc.URL)
	spec.FallbackInputs = nil

	run, _ := handoffParams(t, spec, func(string) error { return errors.New("no browser on this host") }, "")
	_, err := runHandoff(context.Background(), run)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no browser on this host")
}

// ---------------------------------------------------------------------------
// guidance
// ---------------------------------------------------------------------------

// TestRunHandoff_GuidanceIsShownAboveTheFirstQuestionAsked: the kind's
// fallback questions are unanswerable on their own — the operator filling in
// the service's form by hand has to be told what to put in it — and the
// guidance is derived from answers, so it cannot travel on the static
// questions.
func TestRunHandoff_GuidanceIsShownAboveTheFirstQuestionAsked(t *testing.T) {
	svc := fakeService(t, "abc")
	t.Cleanup(svc.Close)

	spec := demoHandoff(t, svc.URL)
	var gotAnswers map[string]string
	spec.FallbackGuidance = func(answers map[string]string) (string, error) {
		gotAnswers = answers
		return "CREATE-IT-BY-HAND-AT " + answers["org"], nil
	}

	th := tui.NewTheme(tui.Caps{})
	var shown strings.Builder
	run, _ := handoffParams(t, spec, fakeBrowser(t, svc.URL, nil), "67890\n")
	run.opts = tui.Options{Theme: th, Driver: tui.Plain(strings.NewReader("67890\n"), &shown, th)}

	_, err := runHandoff(context.Background(), run)
	require.NoError(t, err)

	assert.Contains(t, shown.String(), "CREATE-IT-BY-HAND-AT demo-org",
		"the guidance must be on screen while the question it explains is being answered")
	assert.Equal(t, "from-abc", gotAnswers["app-id"],
		"the guidance is composed from every answer known by then, including the handoff's own")
}

// TestRunHandoff_GuidanceIsNotBuiltWhenNothingWillBeAsked: FallbackGuidance
// promises it runs only when a question is actually going to be put to
// somebody, and a kind may do real work inside it — github WRITES its
// reference manifest to disk from there. A run whose every remaining question
// came from a flag has no screen to carry the text and nobody to read it, so
// building it would be a file dropped in the operator's working directory for
// a prompt that never appeared.
func TestRunHandoff_GuidanceIsNotBuiltWhenNothingWillBeAsked(t *testing.T) {
	svc := fakeService(t, "abc")
	t.Cleanup(svc.Close)

	spec := demoHandoff(t, svc.URL)
	called := false
	spec.FallbackGuidance = func(map[string]string) (string, error) {
		called = true
		return "CREATE-IT-BY-HAND", nil
	}

	// The exchange answers app-id; the flag answers the only other question.
	run, _ := handoffParams(t, spec, fakeBrowser(t, svc.URL, nil), "")
	run.seeded = map[string]string{"installation-id": "seeded-67890"}

	answers, err := runHandoff(context.Background(), run)
	require.NoError(t, err)
	assert.Equal(t, "seeded-67890", answers["installation-id"])
	assert.False(t, called, "no question was rendered, so nothing had guidance to carry")
}

// TestRunHandoff_AGuidanceFailureDoesNotEndTheRun: the guidance is text, and
// losing it must not cost an operator the questions they can still answer —
// but it is reported rather than dropped, and reported WHERE THE QUESTIONS
// ARE.
//
// A note alone is not enough. The post-run summary is printed after the run
// ends, so an operator who was relying on the guidance to answer these
// questions reads about its absence only once they are behind them. These
// questions are routinely unanswerable without it — github's carry the webhook
// URL, the permissions and the events for an App being created by hand — so
// silence here is the difference between "I cannot answer this" and "I did not
// know anything was missing".
func TestRunHandoff_AGuidanceFailureDoesNotEndTheRun(t *testing.T) {
	svc := fakeService(t, "abc")
	t.Cleanup(svc.Close)

	spec := demoHandoff(t, svc.URL)
	spec.FallbackGuidance = func(map[string]string) (string, error) {
		return "", errors.New("could not render the reference")
	}

	th := tui.NewTheme(tui.Caps{})
	var shown strings.Builder
	run, st := handoffParams(t, spec, fakeBrowser(t, svc.URL, nil), "67890\n")
	run.opts = tui.Options{Theme: th, Driver: tui.Plain(strings.NewReader("67890\n"), &shown, th)}

	answers, err := runHandoff(context.Background(), run)
	require.NoError(t, err)
	assert.Equal(t, "67890", answers["installation-id"],
		"the questions are the floor that always works")
	assert.Contains(t, notesText(st), "could not render the reference",
		"the failure is recorded on the run's State for the summary")
	assert.Contains(t, shown.String(), "could not render the reference",
		"and stated on screen, above the questions it was supposed to explain")
}

// ---------------------------------------------------------------------------
// unattended
// ---------------------------------------------------------------------------

// TestRunHandoff_NonInteractiveNeverOpensABrowser: nobody is watching, so
// there is nothing for a browser to be opened in front of. The fallback
// questions are what a flag can answer, and the run fails closed on the ones
// it cannot.
func TestRunHandoff_NonInteractiveNeverOpensABrowser(t *testing.T) {
	svc := fakeService(t, "abc")
	t.Cleanup(svc.Close)

	run, _ := handoffParams(t, demoHandoff(t, svc.URL), func(string) error {
		t.Fatal("an unattended run must not launch a browser")
		return nil
	}, "")
	run.opts = tui.Options{Theme: tui.NewTheme(tui.Caps{}), NonInteractive: true}
	run.seeded = map[string]string{"app-id": "11111", "installation-id": "67890"}

	answers, err := runHandoff(context.Background(), run)
	require.NoError(t, err)
	assert.Equal(t, "11111", answers["app-id"])
	assert.Equal(t, "67890", answers["installation-id"])
}

func TestRunHandoff_NonInteractiveWithAnUnseededFallbackFailsClosed(t *testing.T) {
	svc := fakeService(t, "abc")
	t.Cleanup(svc.Close)

	run, _ := handoffParams(t, demoHandoff(t, svc.URL), func(string) error {
		t.Fatal("an unattended run must not launch a browser")
		return nil
	}, "")
	run.opts = tui.Options{Theme: tui.NewTheme(tui.Caps{}), NonInteractive: true}

	_, err := runHandoff(context.Background(), run)
	require.Error(t, err, "a question nobody can answer must not be silently skipped")
}

// ---------------------------------------------------------------------------
// the listener
// ---------------------------------------------------------------------------

// TestRunHandoff_ShutsTheListenerDownOnEveryExitPath: the loopback listener is
// a live HTTP server on the operator's machine that accepts an exchange code.
// Leaving one behind after the run means a port that answers to whatever
// navigates to it, for as long as the process lives.
func TestRunHandoff_ShutsTheListenerDownOnEveryExitPath(t *testing.T) {
	svc := fakeService(t, "abc")
	t.Cleanup(svc.Close)

	cases := []struct {
		name string
		open func(t *testing.T, svc *httptest.Server) func(string) error
	}{
		{
			name: "the exchange completed",
			open: func(t *testing.T, svc *httptest.Server) func(string) error { return fakeBrowser(t, svc.URL, nil) },
		},
		{
			name: "the browser never opened",
			open: func(*testing.T, *httptest.Server) func(string) error {
				return func(string) error { return errors.New("no browser on this host") }
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var startURL string
			open := tc.open(t, svc)
			run, _ := handoffParams(t, demoHandoff(t, svc.URL), func(u string) error {
				startURL = u
				return open(u)
			}, "11111\n67890\n")

			_, err := runHandoff(context.Background(), run)
			require.NoError(t, err)
			require.NotEmpty(t, startURL, "the listener's address is what a browser would have been sent to")

			resp, err := http.Get(startURL) //nolint:noctx // test-only liveness probe
			if err == nil {
				_ = resp.Body.Close()
				t.Fatalf("the loopback listener at %s is still serving after the run", startURL)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// misuse
// ---------------------------------------------------------------------------

// TestRunHandoff_RefusesAMalformedSpec: a kind that describes a handoff it
// cannot actually run is a programming error, and the client calls Begin and
// Complete with no nil check on the paths below — so it is refused by name
// rather than reaching a nil dereference.
func TestRunHandoff_RefusesAMalformedSpec(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*channelkinds.HandoffSpec)
		wantSay string
	}{
		{
			name:    "no Begin: nowhere to send the operator",
			mutate:  func(s *channelkinds.HandoffSpec) { s.Begin = nil },
			wantSay: "Begin",
		},
		{
			name:    "no Complete: nothing to do with the callback",
			mutate:  func(s *channelkinds.HandoffSpec) { s.Complete = nil },
			wantSay: "Complete",
		},
		{
			name: "an unrenderable fallback question",
			mutate: func(s *channelkinds.HandoffSpec) {
				s.FallbackInputs = []oap.Question{{Name: "app-id", Type: oap.QEnum, Prompt: "App ID"}}
			},
			wantSay: "enum",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc := fakeService(t, "abc")
			t.Cleanup(svc.Close)

			spec := demoHandoff(t, svc.URL)
			tc.mutate(spec)
			run, _ := handoffParams(t, spec, func(string) error {
				t.Fatal("a malformed spec must be refused before a browser is opened")
				return nil
			}, "")

			_, err := runHandoff(context.Background(), run)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantSay)
		})
	}
}

// TestFormPage_EscapesAttributeContent is the one place the self-submitting
// page's escaping is checked directly.
//
// Both values it interpolates are attacker-adjacent: the action URL is built
// from an org login the operator typed, and a field value is routinely a JSON
// document (github's App manifest), which by definition contains the double
// quotes that would close an HTML attribute early. Built with fmt.Sprintf
// instead of html/template, the remainder would be parsed as markup in a page
// a real browser executes.
//
// The assertion is a ROUND TRIP rather than a match against one expected
// escaping: what matters is that a browser recovers exactly the value the
// kind supplied, not which of the several valid escapings html/template
// happened to choose.
func TestFormPage_EscapesAttributeContent(t *testing.T) {
	const (
		action   = `https://github.com/organizations/demo-org/settings/apps/new?state=abc123&x=1`
		manifest = `{"name":"demo-org-demo-reviewbot-gh","url":"https://ap.demo.test"}`
	)

	page, err := formPage(action, map[string]string{"manifest": manifest})
	require.NoError(t, err)

	assert.NotContains(t, page, `value="`+manifest+`"`,
		"the raw JSON must not sit verbatim in the attribute; its own quotes would close it early")
	assert.NotContains(t, page, `action="`+action+`"`,
		"the raw URL must not sit verbatim in the attribute either — its & is an entity there")

	a := formActionPattern.FindStringSubmatch(page)
	require.Len(t, a, 2, "the form must carry an action")
	assert.Equal(t, action, html.UnescapeString(a[1]))

	f := formFieldPattern.FindStringSubmatch(page)
	require.Len(t, f, 3, `the form must carry the name="manifest" hidden field`)
	assert.Equal(t, "manifest", f[1])
	assert.Equal(t, manifest, html.UnescapeString(f[2]))
}

// notesText flattens a run's recorded summary so a test can assert what it
// says without depending on the order the lines were written in.
func notesText(st *tui.State) string {
	var b strings.Builder
	for _, n := range st.Notes() {
		b.WriteString(n.Label)
		b.WriteString(": ")
		b.WriteString(n.Value)
		b.WriteString("\n")
	}
	return b.String()
}
