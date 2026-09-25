package github_pat_test

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/cli/tui"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/authkind"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/provider"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/setup/builtins"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/setup/builtins/github_pat"
	"github.com/authzed/openagentprimitives/pkg/x/browser/browsertest"
)

// run drives the flow end to end over a scripted stdin, exactly as the CLI
// does: describe the screens, let the sequencer present them, then let the flow
// store what they collected. It returns what reached Store.
//
// script is one line per prompt the run reaches. Every prompt MUST have a line:
// huh's accessible renderer cannot report a read error, so a short script
// silently answers the remainder from their defaults and completes with a nil
// error. That is why every assertion below is on the STORED credential and
// never on err == nil.
func run(t *testing.T, req builtins.Request, script []string, seed map[string]string) (builtins.StoreValue, string, error) {
	t.Helper()

	var stored builtins.StoreValue
	storeCalls := 0
	req.Store = func(_ context.Context, v builtins.StoreValue) error {
		storeCalls++
		stored = v
		return nil
	}

	f := github_pat.New()
	screens, err := f.Screens(context.Background(), req)
	if err != nil {
		return builtins.StoreValue{}, "", err
	}

	st := tui.NewState()
	for k, v := range seed {
		st.Set(k, v)
	}
	in := ""
	if len(script) > 0 {
		in = strings.Join(script, "\n") + "\n"
	}
	var out bytes.Buffer
	st, err = tui.RunWith(context.Background(), screens, tui.Options{
		Theme: tui.NewTheme(tui.Caps{}),
		In:    strings.NewReader(in),
		Out:   &out,
	}, st)
	if err != nil {
		return builtins.StoreValue{}, out.String(), err
	}

	err = f.Result(context.Background(), req, st)
	if err == nil {
		assert.Equal(t, 1, storeCalls, "a successful Result must store exactly once")
	} else {
		assert.Zero(t, storeCalls, "a failing Result must not have stored anything")
	}
	return stored, out.String(), err
}

func testProvider() *provider.Provider {
	return &provider.Provider{
		ID: "github-pat", Builtin: "github-pat", Shape: "bearer",
		DocsURL: "https://docs.github.com/en/authentication/keeping-your-account-and-data-secure/managing-your-personal-access-tokens",
		Prompt:  "go open https://github.com/...",
		TokenShape: &provider.TokenShape{
			Pattern:     `^ghp_[A-Za-z0-9_]+$`,
			Description: "starts with ghp_",
		},
	}
}

func testRequest() builtins.Request {
	return builtins.Request{
		Provider:     testProvider(),
		Requirement:  authkind.CredentialRequirement{SuggestedName: "gh-pat", BindingEnv: map[string]string{"GITHUB_TOKEN": ""}},
		IdentityName: "demo-bot",
		Namespace:    "demo-ns",
	}
}

func TestFlowName(t *testing.T) {
	assert.Equal(t, "github-pat", github_pat.New().Name())
}

func TestScreensAreStableAndNamed(t *testing.T) {
	screens, err := github_pat.New().Screens(context.Background(), testRequest())
	require.NoError(t, err)

	var ids []string
	for _, s := range screens {
		ids = append(ids, s.ID())
	}
	assert.Equal(t, []string{"browser", "token"}, ids,
		"the page is opened before the token is asked for, so the user has somewhere to get one")
}

// TestPastedTokenIsStored is the happy path, asserted on the credential rather
// than on the absence of an error.
func TestPastedTokenIsStored(t *testing.T) {
	rec := browsertest.Record(t)

	stored, _, err := run(t, testRequest(), []string{"ghp_abcd1234efgh5678"}, nil)
	require.NoError(t, err, "flow run")
	assert.Equal(t, "ghp_abcd1234efgh5678", stored.Bearer)
	openedURL := rec.Last()
	assert.True(t, strings.HasPrefix(openedURL, "https://github.com/settings/personal-access-tokens/new"),
		"the browser is pointed at the token-creation page; got %q", openedURL)
	assert.Contains(t, openedURL, "name=demo-bot",
		"the page is pre-filled with the identity the token is for")
}

// TestRefusals covers every way this flow must decline to store, each of which
// would otherwise leave a credential that cannot authenticate.
func TestRefusals(t *testing.T) {
	browsertest.Record(t)

	cases := []struct {
		name      string
		script    []string
		errSubstr string
	}{
		{
			name: "malformed token: refused, naming the shape it should have had",
			// Two lines because the field's validator re-prompts a rejected
			// value; the second is what the run ends up carrying.
			script:    []string{"abcdef", "still-not-a-pat"},
			errSubstr: "starts with ghp_",
		},
		{
			name:      "no input at all: refused rather than storing an empty token",
			script:    nil,
			errSubstr: "nothing was supplied",
		},
		{
			name:      "a blank line: refused rather than storing an empty token",
			script:    []string{""},
			errSubstr: "nothing was supplied",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stored, _, err := run(t, testRequest(), tc.script, nil)
			require.Error(t, err, "the flow must refuse this input")
			assert.Contains(t, err.Error(), tc.errSubstr)
			assert.Empty(t, stored.Bearer, "nothing may be stored on a refusal")
		})
	}
}

// TestSeededTokenSkipsTheBrowser pins the reason the browser is a screen of its
// own: a token supplied ahead of time is a run with nothing to generate, so
// opening a tab would be pure noise.
func TestSeededTokenSkipsTheBrowser(t *testing.T) {
	rec := browsertest.Record(t)

	stored, _, err := run(t, testRequest(), nil, map[string]string{github_pat.KeyToken: "ghp_seeded000"})
	require.NoError(t, err)
	assert.Equal(t, "ghp_seeded000", stored.Bearer, "the seeded token is what gets stored")
	assert.Zero(t, rec.Count(), "a run that already has its token must not open a browser")
}

// TestSeededTokenIsStillChecked: supplying an answer ahead of time skips the
// question, not the format gate.
func TestSeededTokenIsStillChecked(t *testing.T) {
	browsertest.Record(t)

	stored, _, err := run(t, testRequest(), nil, map[string]string{github_pat.KeyToken: "sk-ant-wrong-provider"})
	require.Error(t, err, "a seeded token of the wrong shape must be refused")
	assert.Contains(t, err.Error(), "starts with ghp_")
	assert.Empty(t, stored.Bearer)
}

// TestBrowserFailureIsSurfacedNotSwallowed: a browser that will not open is not
// fatal, but the user must be told where to go instead — on the screen where
// they need it, not in a log.
func TestBrowserFailureIsSurfacedNotSwallowed(t *testing.T) {
	browsertest.Fail(t, errors.New("no browser on this machine"))

	stored, out, err := run(t, testRequest(), []string{"ghp_abcd1234efgh5678"}, nil)
	require.NoError(t, err, "a browser that will not open must not fail the run")
	assert.Equal(t, "ghp_abcd1234efgh5678", stored.Bearer)
	assert.Contains(t, out, "no browser on this machine", "the reason the browser did not open must reach the user")
	assert.Contains(t, out, "https://github.com/settings/personal-access-tokens/new",
		"the address must be shown so the user can open it themselves")
}

// TestGuidanceFitsTheNoteWidth guards the address in particular: huh wraps an
// over-long note line at the form's column budget with no sign the halves
// belong together, which turns a URL the user must copy into two useless
// fragments.
//
// The browser's own failure reason is exempt. It comes from the operating
// system, so its length is not ours to bound — which is exactly why it is
// composed onto a line of its own, and why this test proves it drags nothing
// else over with it.
func TestGuidanceFitsTheNoteWidth(t *testing.T) {
	const reason = "an unusually long explanation of why no browser could be started here"
	browsertest.Fail(t, errors.New(reason))

	for _, intent := range []string{"", "read the repo", "write to the repo and create PRs"} {
		t.Run("intent="+intent, func(t *testing.T) {
			req := testRequest()
			req.UserIntent = intent
			_, out, err := run(t, req, []string{"ghp_abcd1234efgh5678"}, nil)
			require.NoError(t, err)
			for _, line := range tui.RailedNoteBudget().Overflows(out) {
				assert.Contains(t, line, reason,
					"only the operating system's own failure text may exceed the note width; %q (%d columns) did too",
					line, len([]rune(line)))
			}
			assert.Contains(t, out, "Page: https://github.com/settings/personal-access-tokens/new",
				"the address must appear whole, on one line")
		})
	}
}

func TestVerifyDelegatesToHTTPBearer(t *testing.T) {
	builtins.SetVerifyHTTPClient(func() *http.Client { return &http.Client{} })
	t.Cleanup(func() { builtins.SetVerifyHTTPClient(nil) })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "token ghp_abc", r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"login":"octocat"}`))
	}))
	t.Cleanup(srv.Close)

	prov := &provider.Provider{
		ID: "github-pat", Title: "GitHub", Shape: "bearer",
		Verify: &provider.VerifyConfig{Endpoint: srv.URL, AuthScheme: "token", SubjectField: "login"},
	}
	res, err := github_pat.New().Verify(context.Background(), builtins.VerifyRequest{
		Provider: prov,
		Value:    builtins.StoreValue{Bearer: "ghp_abc"},
	})
	require.NoError(t, err)
	assert.Equal(t, builtins.VerifyValid, res.Status)
	assert.Equal(t, "octocat", res.Subject)
}
