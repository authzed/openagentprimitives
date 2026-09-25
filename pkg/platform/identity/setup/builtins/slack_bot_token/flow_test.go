package slack_bot_token_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelfeatures"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/slack"
	"github.com/authzed/openagentprimitives/pkg/cli/tui"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/provider"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/setup/builtins"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/setup/builtins/slack_bot_token"
)

// noBrowser is the opener handed to a Flow whose screens this test never
// presents, so nothing ever calls it. Named rather than nil so every
// construction in this file reads the same way.
func noBrowser(string) error { return nil }

// noManifestDir is the directory handed to a run that is not testing the copy
// on disk: empty means "this run has nowhere to write", so no run in this file
// touches the filesystem unless it says so with a t.TempDir().
const noManifestDir = ""

const goodToken = "xoxb-1234567890123-1234567890123-abcdefghijklmnop"

// seedRoute answers the app-route question ahead of the run.
//
// Used by every test that is about the TOKEN rather than about the route: a
// seeded key is never asked (tui.Question.Prepare), so the scripted token line
// cannot be silently eaten by a question the test forgot was there — which is
// exactly the failure a short script produces, since huh's accessible renderer
// answers from the default and reports nothing.
func seedRoute(route string) map[string]string {
	return map[string]string{slack_bot_token.KeyAppRoute: route}
}

// run drives the flow end to end over a scripted stdin with a browser that
// opens nothing and nowhere to write a manifest, and returns what reached
// Store.
//
// script is one line per prompt the run reaches. Every prompt MUST have a
// line: huh's accessible renderer cannot report a read error, so a short
// script silently answers the remainder from their defaults and completes with
// a nil error. That is why every assertion below is on the STORED credential
// and never on err == nil.
func run(t *testing.T, req builtins.Request, script []string, seed map[string]string) (builtins.StoreValue, string, error) {
	t.Helper()
	return runWithBrowser(t, func(string) error { return nil }, req, script, seed)
}

// runWithBrowser is run with the browser opener the caller wants — one that
// records the URL, or one that fails.
func runWithBrowser(
	t *testing.T, open func(string) error,
	req builtins.Request, script []string, seed map[string]string,
) (builtins.StoreValue, string, error) {
	t.Helper()
	return runIn(t, noManifestDir, open, req, script, seed)
}

// runIn is the full drive: the browser opener the caller wants, and the
// directory the generated manifest's copy is written into.
//
// Every run in this package builds its Flow with both injected, which is why
// none of them can reach a real browser and none of them can leave a file in
// this package's own directory: each is a constructor argument rather than a
// package-level default with a setter, so the safe path is the one you get by
// writing the test at all, not the one you get by remembering to opt out of
// the unsafe one.
func runIn(
	t *testing.T, dir string, open func(string) error,
	req builtins.Request, script []string, seed map[string]string,
) (builtins.StoreValue, string, error) {
	t.Helper()

	var stored builtins.StoreValue
	storeCalls := 0
	req.Store = func(_ context.Context, v builtins.StoreValue) error {
		storeCalls++
		stored = v
		return nil
	}

	f := slack_bot_token.New(open, dir)
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

// testRequest mirrors the catalog entry this flow serves. Built by hand rather
// than loaded so a change to providers/slack-bot-token.yaml shows up as a
// failing assertion here rather than as a test that silently follows it.
func testRequest() builtins.Request {
	return builtins.Request{
		Provider: &provider.Provider{
			ID: "slack-bot-token", Builtin: "slack-bot-token", Shape: "bearer",
			Title:   "Slack",
			DocsURL: "https://api.slack.com/authentication/token-types#bot",
			TokenShape: &provider.TokenShape{
				Pattern:     `^xoxb-`,
				Description: "a Slack bot token starting with xoxb-",
			},
		},
		IdentityName: "demo-bot",
		Namespace:    "demo-ns",
		UserIntent:   "read-only access to workspace channels and their members for directory sync",
	}
}

func screenIDs(t *testing.T, f *slack_bot_token.Flow, req builtins.Request) []string {
	t.Helper()
	screens, err := f.Screens(context.Background(), req)
	require.NoError(t, err)
	var ids []string
	for _, s := range screens {
		ids = append(ids, s.ID())
	}
	return ids
}

func TestFlowName(t *testing.T) {
	assert.Equal(t, "slack-bot-token", slack_bot_token.New(noBrowser, noManifestDir).Name())
}

// TestScreensAreStableAndNamed pins both sequences: the one an ordinary
// directory-sync run gets, and the shorter one a run this flow cannot generate
// an app for gets.
func TestScreensAreStableAndNamed(t *testing.T) {
	f := slack_bot_token.New(noBrowser, noManifestDir)

	assert.Equal(t, []string{"app", "browser", "manifest", "token"}, screenIDs(t, f, testRequest()),
		"where the app comes from is asked first, because it decides what the last screen has to say; "+
			"the app list is opened before the token is asked for, so the user has somewhere to make one")

	writing := testRequest()
	writing.UserIntent = "read channels and post messages as the agent"
	assert.Equal(t, []string{"browser", "token"}, screenIDs(t, f, writing),
		"a run the generated app would not cover is never offered it, and is the flow it always was")
}

// TestPastedTokenIsStored is the happy path on BOTH routes, asserted on the
// credential rather than on the absence of an error.
func TestPastedTokenIsStored(t *testing.T) {
	for _, route := range []string{slack_bot_token.RouteManifest, slack_bot_token.RouteExisting} {
		t.Run("route="+route, func(t *testing.T) {
			openedURL := ""
			stored, out, err := runWithBrowser(t,
				func(u string) error { openedURL = u; return nil },
				testRequest(), []string{goodToken}, seedRoute(route))
			require.NoError(t, err)
			assert.Equal(t, goodToken, stored.Bearer)
			assert.Equal(t, "https://api.slack.com/apps", openedURL,
				"the flow must open the page the app is made on")
			assert.NotContains(t, out, goodToken,
				"the token must never be echoed into the run's own output")
		})
	}
}

// TestWrongSlackTokenIsRefused covers the mistake this provider's format gate
// exists for: a Slack app hands out four tokens from adjacent pages, and only
// the xoxb- one authenticates as the bot.
func TestWrongSlackTokenIsRefused(t *testing.T) {
	cases := []struct {
		name  string
		token string
	}{
		{name: "app-level token (xapp-): refused, names the xoxb- prefix", token: "xapp-1-A0123-456-deadbeef"},
		{name: "user token (xoxp-): refused, names the xoxb- prefix", token: "xoxp-1234567890123-abcdef"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			seed := seedRoute(slack_bot_token.RouteExisting)
			seed[slack_bot_token.KeyToken] = tc.token
			_, _, err := run(t, testRequest(), nil, seed)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "xoxb-",
				"the refusal must name the prefix the field wants, or it does not say which token to paste")
		})
	}
}

// TestBlankAnswerIsRefused is the fail-closed half of the flow contract,
// exercised here with the provider's own format gate REMOVED so the emptiness
// guard is the only thing that can refuse it.
func TestBlankAnswerIsRefused(t *testing.T) {
	req := testRequest()
	req.Provider = nil

	var storeCalls int
	req.Store = func(context.Context, builtins.StoreValue) error { storeCalls++; return nil }

	st := tui.NewState()
	st.Set(slack_bot_token.KeyToken, "   ")
	require.Error(t, slack_bot_token.New(noBrowser, noManifestDir).Result(context.Background(), req, st))
	assert.Zero(t, storeCalls, "a blank answer must never reach Store")
}

// TestGeneratedAppRequestsTheEmailScope is what the generate-the-app route
// exists to get right, and the assertion to break first when checking that
// these tests bite.
//
// users:read.email is load-bearing and it fails QUIETLY: Slack does not refuse
// users.info without it, it answers and omits profile.email, so the sync drops
// every member as a join miss and reports success having written nothing.
// Automating app creation from a manifest short of it would be strictly worse
// than the scope list it replaced — an app that installs cleanly and does
// nothing is harder to diagnose than one that was never made.
//
// The four scopes are asserted literally, not as "whatever the feature
// declares", so the claim is about the app an operator actually gets.
func TestGeneratedAppRequestsTheEmailScope(t *testing.T) {
	_, out, err := run(t, testRequest(), []string{goodToken}, seedRoute(slack_bot_token.RouteManifest))
	require.NoError(t, err)

	for _, scope := range []string{"channels:read", "groups:read", "users:read", "users:read.email"} {
		assert.Containsf(t, out, "- "+scope+"\n", "the generated manifest must request %s", scope)
	}
	assert.Contains(t, out, "display_information:", "the manifest itself must be on the screen, not just its scopes")
	assert.Contains(t, out, "Create the app from this manifest:",
		"the operator must be told what to do with it")
	assert.Contains(t, out, "copy the bot token (xoxb-) shown",
		"and how to get the token back out of the app they just made")
}

// TestGeneratedAppIsTheOneTheSyncDeclares proves the screen renders the shared
// generator rather than a second copy of the same YAML — the copy being where
// a scope added to the feature declaration would fail to arrive.
func TestGeneratedAppIsTheOneTheSyncDeclares(t *testing.T) {
	req := testRequest()
	_, out, err := run(t, req, []string{goodToken}, seedRoute(slack_bot_token.RouteManifest))
	require.NoError(t, err)

	want := slack.BotTokenAppManifestFor(req.IdentityName, []channelfeatures.Feature{channelfeatures.DirectorySync})
	for _, line := range strings.Split(strings.TrimSpace(want), "\n") {
		assert.Containsf(t, out, line, "the rendered manifest is missing the line %q", line)
	}
	assert.Contains(t, out, "name: demo-bot", "the app is named after the identity the credential is for")
}

// TestGeneratedManifestIsLeftOnDisk is why the copy exists at all: the
// manifest is shown on a note in scrollback that the next command scrolls
// away, and Slack's import wants it pasted whole.
func TestGeneratedManifestIsLeftOnDisk(t *testing.T) {
	dir := t.TempDir()
	req := testRequest()
	_, out, err := runIn(t, dir, noBrowser, req, []string{goodToken}, seedRoute(slack_bot_token.RouteManifest))
	require.NoError(t, err)

	path := filepath.Join(dir, "slack-app-manifest-demo-bot.yaml")
	got, readErr := os.ReadFile(path)
	require.NoError(t, readErr, "the run must leave the manifest beside it")
	assert.Equal(t,
		slack.BotTokenAppManifestFor(req.IdentityName, []channelfeatures.Feature{channelfeatures.DirectorySync}),
		string(got),
		"the file must be the same document the screen showed, byte for byte — it is the one that gets pasted")

	assert.Contains(t, out, "A copy is saved at", "the operator must be told the file is there")
	assert.Contains(t, out, path, "and where")
}

// TestNowhereToWriteStillShowsTheManifest: a surface that offered no directory
// loses the file and nothing else. The manifest is the product of the screen,
// and it is on the screen either way.
func TestNowhereToWriteStillShowsTheManifest(t *testing.T) {
	_, out, err := runIn(t, noManifestDir, noBrowser, testRequest(),
		[]string{goodToken}, seedRoute(slack_bot_token.RouteManifest))
	require.NoError(t, err)
	assert.Contains(t, out, "display_information:", "the manifest must still be shown")
	assert.NotContains(t, out, "A copy is saved at",
		"nothing was written, so nothing may claim it was")
}

// TestAFailedSaveIsReportedAndDoesNotEndTheRun is the no-silent-errors half:
// the write can fail for reasons that have nothing to do with the credential
// (a read-only directory, a full disk), and ending the run there would take
// the manifest with it — the screen holds the only other copy.
func TestAFailedSaveIsReportedAndDoesNotEndTheRun(t *testing.T) {
	// A FILE where a directory is expected, so the write fails the way a
	// permission problem would, without needing to chmod anything.
	blocked := filepath.Join(t.TempDir(), "not-a-directory")
	require.NoError(t, os.WriteFile(blocked, []byte("x"), 0o600))

	stored, out, err := runIn(t, blocked, noBrowser, testRequest(),
		[]string{goodToken}, seedRoute(slack_bot_token.RouteManifest))
	require.NoError(t, err, "a manifest that could not be saved must not cost the operator the run")
	assert.Equal(t, goodToken, stored.Bearer)
	assert.Contains(t, out, "This manifest is not saved to",
		"the failure must be said out loud, beside the only copy that is left")
	assert.Contains(t, out, "display_information:", "and that copy must still be there")
}

// TestSilenceTakesTheGenerateRoute pins what an exhausted input script
// answers the route question with. The generate route is the floor that always
// works; the other answer claims an app exists, and an operator who has none
// is then handed a bare "paste the token" with nowhere to get one.
//
// Asserted on the manifest reaching the screen, not on the absence of an
// error: this run stores nothing, because the token screen runs out of input
// too and Result refuses — which is the fail-closed behavior, and is why the
// error is required here rather than tolerated.
func TestSilenceTakesTheGenerateRoute(t *testing.T) {
	_, out, err := run(t, testRequest(), nil, nil)
	require.Error(t, err, "an exhausted script supplies no token, and a flow that stored one anyway would be storing nothing")
	assert.Contains(t, out, "display_information:",
		"silence must land on the route that generates the app, not the one that assumes one exists")
}

// TestScriptedChoicePicksTheExistingAppRoute drives the route question itself
// rather than seeding past it, so the option ORDER is pinned by something: a
// reordering that made option 2 the generate route would otherwise be
// invisible.
func TestScriptedChoicePicksTheExistingAppRoute(t *testing.T) {
	stored, out, err := run(t, testRequest(), []string{"2", goodToken}, nil)
	require.NoError(t, err)
	assert.Equal(t, goodToken, stored.Bearer)
	assert.Contains(t, out, "Suggested bot token scopes:",
		"the second option is the app-you-already-have route, which recommends scopes instead of generating them")
	assert.NotContains(t, out, "display_information:",
		"and generates no app")
}

// TestScopeGuidanceFollowsTheIntent pins the recommendation itself, on the
// route that gives one. The read set is what a Slack directory sync actually
// calls; recommending write scopes to a read-only intent would hand the
// operator a token broader than anything the sync does with it.
func TestScopeGuidanceFollowsTheIntent(t *testing.T) {
	cases := []struct {
		name     string
		intent   string
		contains []string
		absent   []string
	}{
		{
			name:     "read-only intent: the four read scopes, no chat:write",
			intent:   "read-only access to workspace channels and their members for directory sync",
			contains: []string{"channels:read", "groups:read", "users:read", "users:read.email"},
			absent:   []string{"chat:write"},
		},
		{
			name:     "writing intent: chat:write added on top of the read scopes",
			intent:   "read channels and post messages as the agent",
			contains: []string{"channels:read", "users:read.email", "chat:write"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := testRequest()
			req.UserIntent = tc.intent
			_, out, err := run(t, req, []string{goodToken}, seedRoute(slack_bot_token.RouteExisting))
			require.NoError(t, err)
			for _, want := range tc.contains {
				assert.Contains(t, out, want)
			}
			for _, notWant := range tc.absent {
				assert.NotContains(t, out, notWant)
			}
		})
	}
}

// TestARunTheGeneratedAppWouldNotCoverIsNotOfferedOne is the gate on
// offersManifest, from both directions it closes. An app generated from the
// directory sync's declaration is the wrong app for either run, and the wrong
// app installs cleanly and then 403s on the one call its caller cared about —
// which is harder to diagnose than never being offered it.
func TestARunTheGeneratedAppWouldNotCoverIsNotOfferedOne(t *testing.T) {
	t.Run("a writing intent keeps the scope list and is offered no app", func(t *testing.T) {
		req := testRequest()
		req.UserIntent = "read channels and post messages as the agent"
		_, out, err := run(t, req, []string{goodToken}, nil)
		require.NoError(t, err)
		assert.NotContains(t, out, "display_information:",
			"the generated app does not request chat:write, so it is not the app this run needs")
		assert.Contains(t, out, "chat:write", "and the recommendation that does name it must survive")
	})

	t.Run("a caller's own scope lines are printed, not replaced by a generated app", func(t *testing.T) {
		req := testRequest()
		req.ScopeHint = []string{"team:read         the caller's own call surface"}
		_, out, err := run(t, req, []string{goodToken}, nil)
		require.NoError(t, err)
		assert.Contains(t, out, "team:read",
			"a caller that supplied lines must have them shown; silently dropping them is the failure ScopeHint exists to prevent")
		assert.NotContains(t, out, "display_information:",
			"and must not be handed an app built from a declaration its lines disagree with")
	})
}

// TestTheRouteKeyIsDeclaredSoAnAnswerCannotBeSilentlyIgnored: `oap directory
// configure` refuses any --answer naming a key the flow declares, and it
// learns the keys by asking the screens. A route screen that declared none
// would let `--answer app=existing` be accepted, ignored, and believed.
func TestTheRouteKeyIsDeclaredSoAnAnswerCannotBeSilentlyIgnored(t *testing.T) {
	screens, err := slack_bot_token.New(noBrowser, noManifestDir).
		Screens(context.Background(), testRequest())
	require.NoError(t, err)

	var declared []string
	for _, s := range screens {
		if a, ok := s.(interface{ AnswerKeys() []string }); ok {
			declared = append(declared, a.AnswerKeys()...)
		}
	}
	assert.Contains(t, declared, slack_bot_token.KeyAppRoute)
	assert.Contains(t, declared, slack_bot_token.KeyToken)
}

// TestGuidanceFitsTheNoteWidth guards the address and the scope names on the
// use-an-app-you-have route: both are meant to be copied or looked up, and huh
// wraps an over-long note line at the form's column budget with no sign the
// halves belong together.
//
// The browser's own failure reason is exempt. It comes from the operating
// system, so its length is not ours to bound — which is exactly why it is
// composed onto a line of its own, and why this test proves it drags nothing
// else over with it.
func TestGuidanceFitsTheNoteWidth(t *testing.T) {
	const reason = "an unusually long explanation of why no browser could be started here"
	req := testRequest()
	req.UserIntent = "read channels and post messages as the agent" // the wider scope list
	_, out, err := runWithBrowser(t,
		func(string) error { return errors.New(reason) },
		req, []string{goodToken}, seedRoute(slack_bot_token.RouteExisting))
	require.NoError(t, err)
	for _, line := range tui.RailedNoteBudget().Overflows(out) {
		assert.Contains(t, line, reason,
			"only the operating system's own failure text may exceed the note width; %q (%d columns) did too",
			line, len([]rune(line)))
	}
	assert.Contains(t, out, "Page: https://api.slack.com/apps",
		"the address must appear whole, on one line")
}

// TestManifestGuidanceFitsTheNoteWidth is the same rule on the generate route,
// where it is no longer merely tidiness: huh hard-wraps at the form's width, a
// wrapped YAML line continues at column 0 inside an indented block, and Slack
// REJECTS that manifest on import with nothing in the render to say why.
//
// The saved path is exempt for the reason the browser's failure text is: it is
// the operator's own filesystem, not our prose, and MessageLines already puts
// it on a line of its own so a long one drags nothing with it.
func TestManifestGuidanceFitsTheNoteWidth(t *testing.T) {
	dir := t.TempDir()
	_, out, err := runIn(t, dir, noBrowser, testRequest(),
		[]string{goodToken}, seedRoute(slack_bot_token.RouteManifest))
	require.NoError(t, err)
	for _, line := range tui.RailedNoteBudget().Overflows(out) {
		assert.Contains(t, line, dir,
			"only the operator's own path may exceed the note width; %q (%d columns) did too",
			line, len([]rune(line)))
	}
}

// TestVerifyIsUnsupported pins the reason Slack gets no probe: auth.test
// answers 200 for a token it rejects, so a status-based check would confirm a
// dead credential.
func TestVerifyIsUnsupported(t *testing.T) {
	res, err := slack_bot_token.New(noBrowser, noManifestDir).
		Verify(context.Background(), builtins.VerifyRequest{Value: builtins.StoreValue{Bearer: goodToken}})
	require.NoError(t, err)
	assert.Equal(t, builtins.VerifyUnsupported, res.Status,
		"Slack answers 200 for a rejected token; claiming a verification it did not run is worse than none")
	assert.NotEmpty(t, res.Detail, "an unsupported verdict must say why, per VerifyResult.Detail")
}
