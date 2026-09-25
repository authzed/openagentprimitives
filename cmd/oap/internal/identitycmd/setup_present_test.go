package identitycmd

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/aptest"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/cli/tui"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/setup/builtins"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/setup/builtins/github_pat"
	"github.com/authzed/openagentprimitives/pkg/x/browser/browsertest"

	_ "github.com/authzed/openagentprimitives/pkg/platform/identity/authkind/loader"
	_ "github.com/authzed/openagentprimitives/pkg/platform/identity/setup/builtins/loader"
)

// wireGitHubPATFlow makes the github-pat builtin the flow `cli:gh` resolves to,
// with its browser opener replaced by a recorder, and returns the pages it
// opened. Registration is re-asserted because other tests in this package call
// builtins.Reset().
func wireGitHubPATFlow(t *testing.T) *[]string {
	t.Helper()
	var opened []string
	browsertest.Use(t, func(u string) error {
		opened = append(opened, u)
		return nil
	})
	if _, ok := builtins.Get("github-pat"); !ok {
		builtins.Register(github_pat.New())
		t.Cleanup(func() { builtins.Reset() })
	}
	return &opened
}

// validPAT is a fixture token of the shape the github-pat provider declares.
// Made up, and long enough to pass the declared format gate.
const validPAT = "ghp_abcdefghijklmnopqrstuvwxyz0123456789ABCDEF"

// assertNamesNoAnswerFlag rejects a refusal that tells the user to pre-supply
// an answer.
//
// `oap identity setup` has no flag that answers a setup question, by decision:
// every answer either IS a credential or mints one, and a flag value lands in
// shell history and in the process table. Any phrasing that suggests otherwise
// sends the reader after something that does not exist — the same defect as the
// fail-closed driver's stock "supply it via flags", just in a different voice.
//
// Applied to EVERY non-interactive refusal, including the ones whose wording is
// written a package away in a screen. Asserting only on the messages composed
// in this package is exactly what let an "as an answer" clause survive in
// flowscreens.Browser, on the two most-travelled paths.
func assertNamesNoAnswerFlag(t *testing.T, err error) {
	t.Helper()
	for _, phrase := range []string{"--answer", "as an answer", "supply it via flags", "supply the"} {
		assert.NotContainsf(t, strings.ToLower(err.Error()), phrase,
			"a refusal must not point at a way to pre-supply the answer; this command offers none")
	}
}

// TestIdentitySetup_ErrorsCarryNoInternalFraming pins what a user reads when a
// setup question is answered with something the provider will not accept.
//
// The sequencer wraps every screen error as `tui: apply screen "<id>": …` —
// right for a log, wrong for a terminal. Left in place, a mistyped token reads
// as `setup: github-token: tui: apply screen "token": …`, which puts two layers
// of this command's plumbing in front of the one sentence the user can act on.
//
// Asserted as the absence of the framing rather than the presence of the
// message, because every other assertion in this file uses Contains and would
// pass with any amount of framing in front of it.
func TestIdentitySetup_ErrorsCarryNoInternalFraming(t *testing.T) {
	wireGitHubPATFlow(t)
	c := aptest.IdentityClientBuilder(t).Build()

	var out strings.Builder
	err := runIdentitySetup(context.Background(), &out, io.Discard,
		strings.NewReader("not-a-github-token\n"), c, "default", "my-bot",
		[]string{"gh"}, nil, false, SetupOptions{})
	require.Error(t, err, "a token of the wrong shape must be refused; output:\n%s", out.String())

	assert.NotContains(t, err.Error(), "tui:",
		"the sequencer's own framing is plumbing, and the user is not debugging our sequencer")
	assert.NotContains(t, err.Error(), `screen "`,
		"a screen ID names the step to us, not to the person who mistyped a token")
	// The sentence the screen actually wrote must survive the strip — an error
	// with the framing removed AND the message removed would pass the two
	// assertions above while telling the user nothing.
	assert.Contains(t, err.Error(), "does not look like a valid github-pat token")
	assert.Contains(t, err.Error(), "github-token", "and still say which credential failed")
}

// TestIdentitySetup_NonInteractiveRefusesBeforeOpeningABrowser: a run told not
// to prompt must change nothing on its way to refusing.
//
// The browser step asks nothing, so a fail-closed driver never sees it: without
// the up-front check it opens a tab and only the NEXT screen refuses. Opening a
// page at somebody who is not there is the one visible effect of a run whose
// whole purpose was to do nothing and say so.
func TestIdentitySetup_NonInteractiveRefusesBeforeOpeningABrowser(t *testing.T) {
	opened := wireGitHubPATFlow(t)
	c := aptest.IdentityClientBuilder(t).Build()

	var out strings.Builder
	err := runIdentitySetup(context.Background(), &out, io.Discard,
		aptest.RefusingReader{T: t}, c, "default", "my-bot",
		[]string{"gh"}, nil, false, SetupOptions{nonInteractive: true})
	require.Error(t, err, "a credential that is not provisioned must fail a run that cannot ask; output:\n%s", out.String())

	assert.Empty(t, *opened, "a run told not to prompt must not open a browser on its way to refusing")
	assert.NotContains(t, err.Error(), "tui:")
	assert.Contains(t, err.Error(), "github-token", "the refusal must name the credential that stopped the run")
	// This is the message the two most-travelled non-interactive paths print, so
	// it is the one most able to send a user after something that does not exist.
	// The screen writing it lives a package away and did not get this assertion
	// the first time, which is how it kept an "answer" clause of its own.
	assertNamesNoAnswerFlag(t, err)

	var ai spiceboxv1alpha1.AgentIdentity
	if gerr := c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "my-bot"}, &ai); gerr == nil {
		assert.Empty(t, ai.Spec.Credentials, "a refused run must store nothing")
	}
}

// preProvisionedGitHubToken returns the AgentIdentity and Secret a credential
// already set up looks like: a static credential named for what the gh toolkit
// declares, resolving cleanly.
//
// Built by hand rather than by running setup first, so the control below tests
// one thing — that an already-provisioned credential passes a run that cannot
// ask — instead of two runs in sequence.
func preProvisionedGitHubToken() (*spiceboxv1alpha1.AgentIdentity, *corev1.Secret) {
	ai := &spiceboxv1alpha1.AgentIdentity{
		ObjectMeta: metav1.ObjectMeta{Name: "my-bot", Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentIdentitySpec{
			Credentials: []spiceboxv1alpha1.AgentCredential{{
				Name: "github-token",
				Type: "static",
				Static: &spiceboxv1alpha1.StaticCredentialSource{
					SecretRef: spiceboxv1alpha1.SecretKeyRef{Name: "github-token", Key: "token"},
				},
			}},
		},
	}
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "github-token", Namespace: "default"},
		Type:       corev1.SecretTypeOpaque,
		Data:       map[string][]byte{"token": []byte(validPAT)},
	}
	return ai, sec
}

// TestIdentitySetup_NonInteractiveOverAProvisionedCredentialSucceeds is the
// control for the two refusal tests above. Without a run that must SUCCEED,
// "refuse everything" passes every one of them.
//
// It is also the whole of what --non-interactive offers on this command: with
// no way to answer a question from a flag, the only run that can complete is
// the one with nothing left to ask. It must do that having opened no page and
// having spoken to nobody — the engine's idempotency check detects the
// credential by name and skips its flow before any of that could happen.
func TestIdentitySetup_NonInteractiveOverAProvisionedCredentialSucceeds(t *testing.T) {
	opened := wireGitHubPATFlow(t)
	refuseAnyProbe(t)
	ai, sec := preProvisionedGitHubToken()
	c := aptest.IdentityClientBuilder(t).WithObjects(ai, sec).Build()

	var out strings.Builder
	require.NoErrorf(t, runIdentitySetup(context.Background(), &out, io.Discard,
		aptest.RefusingReader{T: t}, c, "default", "my-bot",
		[]string{"gh"}, nil, false, SetupOptions{nonInteractive: true}),
		"an already-provisioned credential must pass a run that cannot ask; output:\n%s", out.String())

	assert.Empty(t, *opened, "there is nothing to mint, so there is no page to open")
	assert.Contains(t, out.String(), "already set up", "the run must say what it found")
	assert.NotContains(t, out.String(), validPAT, "the stored credential must not be echoed back")

	var got spiceboxv1alpha1.AgentIdentity
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "my-bot"}, &got))
	assert.Len(t, got.Spec.Credentials, 1, "a verifying run must not add or replace a credential")
}

// expiredOAuthCredential returns the MCPServer, AgentIdentity and Secret that
// make up a credential which resolves but has run out: an OAuth token whose
// expires_at is in the past.
//
// Distinct from an absent credential, and that distinction is the whole point —
// the engine detects it by name, finds it, and only then discovers it is dead.
func expiredOAuthCredential() (*spiceboxv1alpha1.MCPServer, *spiceboxv1alpha1.AgentIdentity, *corev1.Secret) {
	srv := &spiceboxv1alpha1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-mcp", Namespace: "default"},
		Spec: spiceboxv1alpha1.MCPServerSpec{
			Server: spiceboxv1alpha1.MCPServerServer{URL: "https://mcp.example.test/sse"},
			Auth:   spiceboxv1alpha1.MCPServerAuth{Provider: "oauth-mcp", Credential: "demo-mcp"},
		},
	}
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "my-bot-demo-mcp", Namespace: "default"},
		Data: map[string][]byte{
			"access_token": []byte("expired-token"),
			"expires_at":   []byte(time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)),
		},
	}
	ai := &spiceboxv1alpha1.AgentIdentity{
		ObjectMeta: metav1.ObjectMeta{Name: "my-bot", Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentIdentitySpec{
			Credentials: []spiceboxv1alpha1.AgentCredential{{
				Name: "demo-mcp", Type: "oauth",
				OAuth: &spiceboxv1alpha1.OAuthCredentialSource{
					SecretRef: spiceboxv1alpha1.SecretRef{Name: "my-bot-demo-mcp"},
				},
			}},
		},
	}
	return srv, ai, sec
}

// TestIdentitySetup_ExpiredCredentialIsACautionInteractivelyAndARefusalWhenAsserting
// pins the one state that is neither "provisioned" nor "absent".
//
// An expired OAuth token resolves — it is present, and it is dead. Interactively
// that is a nudge: the run says which faster path to take (`oap identity refresh`)
// and carries on, because the user is right there and can act on it.
//
// Under --non-interactive it cannot be a nudge, because there is nobody to nudge
// and the exit code IS the answer. The flag's whole contract on this command is
// the assertion "everything this agent needs is ready"; exiting 0 on a token
// that stopped working is that assertion made falsely, and a CI gate built on
// it would wave through an agent that cannot authenticate.
//
// The two rows are each other's control: make it a refusal always and the
// interactive row fails; leave it a caution always and the asserting row does.
func TestIdentitySetup_ExpiredCredentialIsACautionInteractivelyAndARefusalWhenAsserting(t *testing.T) {
	cases := []struct {
		name           string
		nonInteractive bool
		wantErr        string
	}{
		{
			name: "interactive: a nudge toward the faster path, and the run carries on",
		},
		{
			name:           "asserting readiness: a refusal, because an expired token is not ready",
			nonInteractive: true,
			wantErr:        "demo-mcp",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			refuseAnyProbe(t)
			srv, ai, sec := expiredOAuthCredential()
			c := aptest.IdentityClientBuilder(t).WithObjects(srv, ai, sec).Build()

			var out strings.Builder
			err := runIdentitySetup(context.Background(), &out, io.Discard,
				aptest.RefusingReader{T: t}, c, "default", "my-bot",
				nil, []string{"demo-mcp"}, false,
				SetupOptions{nonInteractive: tc.nonInteractive})

			if tc.wantErr == "" {
				require.NoErrorf(t, err, "output:\n%s", out.String())
				assert.Contains(t, out.String(), "oap identity refresh",
					"a human at the keyboard is told the faster way to fix it")
				return
			}
			require.Error(t, err, "a run asserting readiness must not pass on an expired credential; output:\n%s", out.String())
			assert.Contains(t, err.Error(), tc.wantErr, "the refusal must name the credential that failed the assertion")
			assert.Contains(t, err.Error(), "expired", "and say what is wrong with it")
			assertNamesNoAnswerFlag(t, err)
			assert.NotContains(t, err.Error(), "tui:")
		})
	}
}

// TestIdentitySetup_RefusalNamesTheCredentialNotTheScreen: reaching a question
// in a run that cannot ask proves one thing — this credential is not set up —
// and that is what the user has to be told.
//
// The fail-closed driver's own wording is wrong twice over here: it names a
// screen ID, which is our vocabulary rather than theirs, and it points at flags
// that deliberately do not exist on this command.
func TestIdentitySetup_RefusalNamesTheCredentialNotTheScreen(t *testing.T) {
	c := aptest.IdentityClientBuilder(t).Build()

	// A Claude Code token is generated by a command the user runs themselves,
	// so this flow has no browser step to refuse up front — its refusal comes
	// from the fail-closed driver, which is the path being re-worded.
	err := runIdentitySetup(context.Background(), io.Discard, io.Discard,
		aptest.RefusingReader{T: t}, c, "default", "my-bot",
		[]string{"claude-oauth"}, nil, false, SetupOptions{nonInteractive: true})
	require.Error(t, err)
	assert.ErrorIs(t, err, tui.ErrUnanswered,
		"the sentinel must survive the reframing, or callers can no longer tell a missing answer from a fault")
	assert.Contains(t, err.Error(), "anthropic-oauth is not set up yet",
		"the refusal must name the credential in the words the user's manifests use")
	assertNamesNoAnswerFlag(t, err)
	assert.NotContains(t, err.Error(), "tui:")
	assert.NotContains(t, err.Error(), `screen "`)
}

// TestIdentitySetup_NonInteractiveRefusesAnAuthorizationOnlyAHumanCanGive
// covers the other shape of "this cannot happen without you": an OAuth consent
// page that has to be approved by the account holder, with the token minted BY
// that approval.
//
// The flow declares this itself; this test is what makes the declaration do
// something. Left unconsulted, the run reaches the authorizing screen and calls
// out to the MCP server before failing — a network round trip on behalf of a
// run that was told to touch nothing.
func TestIdentitySetup_NonInteractiveRefusesAnAuthorizationOnlyAHumanCanGive(t *testing.T) {
	srv := &spiceboxv1alpha1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-mcp", Namespace: "default"},
		Spec: spiceboxv1alpha1.MCPServerSpec{
			Server: spiceboxv1alpha1.MCPServerServer{URL: "https://mcp.example.test/sse"},
			Auth:   spiceboxv1alpha1.MCPServerAuth{Provider: "oauth-mcp", Credential: "demo-mcp"},
		},
	}
	c := aptest.IdentityClientBuilder(t).WithObjects(srv).Build()

	err := runIdentitySetup(context.Background(), io.Discard, io.Discard,
		aptest.RefusingReader{T: t}, c, "default", "my-bot",
		nil, []string{"demo-mcp"}, false, SetupOptions{nonInteractive: true})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cannot be automated",
		"the refusal must be the flow's own words about why nothing can stand in for a human here")
	assertNamesNoAnswerFlag(t, err)
	assert.NotContains(t, err.Error(), "tui:")
}

// TestIdentitySetup_NonInteractiveRefusesTheAssistedSetupAgent: a credential
// with no built-in flow can only be set up by the LLM-driven agent, which works
// BY asking — it opens pages, poses questions, and reads what is pasted back.
//
// It asks through its own tools, so the screen sequencer never sees it and the
// fail-closed driver cannot refuse it for us. Left alone it would spend an LLM
// budget opening tabs at an empty chair and then fail anyway.
func TestIdentitySetup_NonInteractiveRefusesTheAssistedSetupAgent(t *testing.T) {
	wireGitHubPATFlow(t)
	refuseAnyProbe(t)
	c := aptest.IdentityClientBuilder(t).Build()

	var out strings.Builder
	// The claude toolkit names a provider this build ships no builtin for.
	err := runIdentitySetup(context.Background(), &out, io.Discard,
		aptest.RefusingReader{T: t}, c, "default", "my-bot",
		[]string{"claude"}, nil, false, SetupOptions{nonInteractive: true})
	require.Error(t, err, "output:\n%s", out.String())
	assert.Contains(t, err.Error(), "assisted setup agent")
	assertNamesNoAnswerFlag(t, err)
	assert.NotContains(t, out.String(), "Store it anyway?",
		"a run told not to prompt must not put a question on the terminal")
}

// refuseAnyProbe fails the test if the live-verification probe is called at
// all. Used by runs that must complete, or refuse, without speaking to a
// provider — an assertion no output check could make, since a probe that
// happened to succeed leaves nothing behind to look for.
func refuseAnyProbe(t *testing.T) {
	t.Helper()
	builtins.SetVerifyHTTPClient(func() *http.Client {
		return &http.Client{Transport: aptest.RoundTripFunc(func(r *http.Request) (*http.Response, error) {
			t.Errorf("this run must speak to nobody, but it probed %s", r.URL.Host)
			return nil, errors.New("probe refused by the test")
		})}
	})
	t.Cleanup(func() { builtins.SetVerifyHTTPClient(nil) })
}

// failingWriter fails every write, standing in for the stdout of a run whose
// pipe was closed (`oap identity setup | head`) or whose disk filled up.
type failingWriter struct{ err error }

func (w failingWriter) Write([]byte) (int, error) { return 0, w.err }

// TestIdentitySetup_SummaryFailureDoesNotUnreportAStoredCredential pins the
// ordering hazard in the presenter: the summary is rendered AFTER Commit has
// written the credential to the AgentIdentity's Secret, so it is past the point
// of no return.
//
// Letting its error propagate would make the run report `setup: <credential>:
// …` for a credential that is durably stored — and because that error is none
// of the engine's sentinels, the retry loop turns it straight into a hard
// failure. The user then re-runs, or goes hunting for a credential they already
// have. A summary that could not be printed is a presentation problem; it is
// surfaced on stderr and the run succeeds, because it did.
func TestIdentitySetup_SummaryFailureDoesNotUnreportAStoredCredential(t *testing.T) {
	wireGitHubPATFlow(t)
	aptest.InstallVerifyHTTPStub(t)
	c := aptest.IdentityClientBuilder(t).Build()

	var stderr strings.Builder
	require.NoError(t, runIdentitySetup(context.Background(),
		failingWriter{err: errors.New("broken pipe")}, &stderr,
		strings.NewReader(validPAT+"\n"), c, "default", "my-bot",
		[]string{"gh"}, nil, false, SetupOptions{}),
		"a summary that could not print must not fail the requirement")

	// The credential really is stored — which is what makes reporting a failure
	// the wrong answer rather than merely a noisy one.
	var ai spiceboxv1alpha1.AgentIdentity
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "my-bot"}, &ai))
	require.NotEmpty(t, ai.Spec.Credentials, "the credential must have been stored")

	// Surfaced, not swallowed: stderr, because stdout is the stream that failed.
	assert.Contains(t, stderr.String(), "the credential was stored")
	assert.Contains(t, stderr.String(), "broken pipe",
		"the reason the summary could not print must reach the operator")
	assert.NotContains(t, stderr.String(), validPAT, "not even a failure notice may carry the credential")
}
