// Package slack_bot_token is the builtin Go flow for the slack-bot-token
// provider. Offers to generate the Slack app's manifest — with exactly the
// scopes the code that will USE this token calls with — walks the user through
// creating the app from it and installing it, captures the pasted Bot User
// OAuth Token, validates the shape, and stores it.
//
// As a screen sequence:
//
//	app       generate the app, or use one that already exists
//	browser   open the Slack app list, where an app is created or opened
//	manifest  render the app manifest and leave a copy beside the run
//	token     say what to do with it, take the pasted token
//
// The app screen is offered only when this run's credential is one the
// manifest actually covers; see offersManifest. When it is not, the sequence
// is the two screens it always was.
package slack_bot_token

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/charmbracelet/huh"

	"github.com/authzed/openagentprimitives/pkg/channels/channelfeatures"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/slack"
	"github.com/authzed/openagentprimitives/pkg/cli/tui"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/provider"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/setup/builtins"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/setup/builtins/flowscreens"
	"github.com/authzed/openagentprimitives/pkg/x/credmask"
)

// KeyToken is the State key the pasted token lands under. Stable: a caller
// answering this flow ahead of time addresses the screen by it.
const KeyToken = "token"

// KeyAppRoute is the State key the app route lands under. Stable for the same
// reason KeyToken is, and it is NOT a credential — which is why it is safe to
// name here while the token is not: `oap directory configure` runs this flow
// on a State of its own and refuses any --answer naming a key the flow
// declares, so neither is settable from a flag either way.
const KeyAppRoute = "app"

// The two app routes. RouteManifest generates the app's manifest and walks the
// operator through creating the app from it; RouteExisting takes a token from
// an app they set up themselves.
const (
	RouteManifest = "manifest"
	RouteExisting = "existing"
)

// appsPage is Slack's app list — where an app is created, and where the one
// that already exists is opened. There is no deep link to a specific app's
// OAuth page, because the app id is not known until the app exists, so the
// guidance below names the sub-page to click through to instead.
const appsPage = "https://api.slack.com/apps"

// manifestFeatures is the channel-feature set the generated app is scoped to.
//
// One entry, and it is not a placeholder: channelfeatures.DirectorySync is the
// Slack directory sync's own declaration of what it calls
// (pkg/channels/channelkinds/slack/features.go), and the sync is what reads
// the token this flow mints. Deriving the manifest from it rather than from a
// list written here is what makes "the app requests what the code calls" a
// property rather than a coincidence — a scope added to that declaration is in
// the next generated app with nothing else edited, and one wrongly missing
// from it fails that package's own guard test.
//
// offersManifest is the other half: a run this feature set does NOT describe
// is never offered a manifest built from it.
var manifestFeatures = []channelfeatures.Feature{channelfeatures.DirectorySync}

// Flow is the slack-bot-token builtin.
type Flow struct {
	// open opens a page in the operator's browser.
	//
	// INJECTED, not read from a package-level hook. The difference is which
	// path you get by default: with a global hook plus a setter, the safe path
	// is opt-in and a test that merely forgets to opt out launches a browser on
	// the machine running the suite. Here a caller must hand over an opener to
	// have one at all, so a test cannot reach a real browser without asking for
	// it. init() passes browser.Open; tests pass a recorder.
	//
	// Nil is tolerated: flowscreens.Browser reports that this build cannot open
	// a browser, and the screen after it repeats the address in its guidance.
	open func(string) error

	// manifestDir is where the generated manifest's copy is written. Injected
	// for the same reason open is, and read the same way: EMPTY means this run
	// has nowhere to write, so no file is written and the guidance simply says
	// nothing about one — the manifest itself is on the screen either way.
	//
	// That spelling is what keeps a test off the filesystem by default. A
	// package-level "current directory" default would put a stray YAML file in
	// whichever package's directory the suite happened to run from, and it
	// would take remembering to opt out to stop it. init() passes the working
	// directory; a test passes t.TempDir(), or "" when the file is not what it
	// is testing.
	manifestDir string
}

// New returns a Flow that opens pages with open and writes the generated
// manifest's copy into manifestDir. Registration passes browser.Open and the
// working directory (init.go); a test passes its own func and its own dir.
func New(open func(string) error, manifestDir string) *Flow {
	return &Flow{open: open, manifestDir: manifestDir}
}

// Name returns the registry name for this flow.
func (*Flow) Name() string { return "slack-bot-token" }

// Screens describes the flow: decide where the app comes from, open the page
// it is made on, generate its manifest, then take the token it issues once it
// is installed. The browser step is a screen of its own because it is work
// rather than a question — which lets a run that cannot prompt refuse it up
// front instead of opening a tab and failing at the screen after it. The
// manifest step is a screen for the same reason, and so that generating the
// file happens once, in Apply, rather than every time a note is re-rendered.
//
// req.Provider.Prompt is LLM-system-prompt content for the fallback agent. Do
// NOT show it to the user; the guidance below is composed from structured
// fields.
func (f *Flow) Screens(_ context.Context, req builtins.Request) ([]tui.Screen, error) {
	page := flowscreens.NewBrowser(flowscreens.BrowserOpts{
		ID:        "browser",
		Label:     "Browser",
		URL:       appsPage,
		Guard:     KeyToken,
		Open:      f.open,
		NoteLabel: "Browser",
	})

	if !offersManifest(req) {
		return []tui.Screen{page, f.tokenScreen(req, page, nil)}, nil
	}

	man := newManifestScreen(req.IdentityName, f.manifestDir)
	return []tui.Screen{f.routeScreen(), page, man, f.tokenScreen(req, page, man)}, nil
}

// routeScreen asks where the app comes from.
//
// RouteManifest is first and is the default, which means it is what SILENCE
// answers with (tui.ChoiceOpts.Default). That is deliberate and it is the same
// call the Slack channel wizard makes for its own routes: generating the
// manifest is the floor that always works, while the other answer CLAIMS an
// app already exists — and an operator who has none, handed a bare "paste the
// token", has nowhere to get one. The cost of being wrong in this direction is
// one non-secret YAML file in the working directory, reported on the next
// screen and in the run summary.
func (f *Flow) routeScreen() tui.Screen {
	return tui.NewChoice(tui.ChoiceOpts{
		QuestionOpts: tui.QuestionOpts{
			ID:        "app",
			Label:     "Slack app",
			Key:       KeyAppRoute,
			Title:     "Where does this bot token come from?",
			Guidance:  func(*tui.State) string { return routeGuidance() },
			NoteLabel: "Slack app",
			NoteValue: routeNote,
		},
		Options: []tui.Choice{
			{Label: "Create one — oap writes the app manifest", Value: RouteManifest},
			{Label: "A Slack app I already have", Value: RouteExisting},
		},
		Default: func() string { return RouteManifest },
	})
}

// tokenScreen takes the pasted token. man is nil on a run that was never
// offered a manifest, which is also the shape guidance branches on.
func (f *Flow) tokenScreen(req builtins.Request, page *flowscreens.Browser, man *manifestScreen) tui.Screen {
	return tui.NewText(tui.TextOpts{
		QuestionOpts: tui.QuestionOpts{
			ID:    "token",
			Label: "Token",
			Key:   KeyToken,
			Title: "Paste the bot token",
			Guidance: func(st *tui.State) string {
				if man != nil && st.Get(KeyAppRoute) == RouteManifest {
					return manifestGuidance(req, page.Err(), man)
				}
				return guidance(req, page.Err())
			},
			Addresses: []tui.Address{flowscreens.DocsAddress(req.Provider)},
			NoteLabel: "Slack bot token",
			NoteValue: credmask.Mask,
		},
		Check: flowscreens.TokenShapeCheck(req.Provider),
	})
}

// offersManifest reports whether this run can generate the app it is about to
// take a token from.
//
// The manifest is generated from ONE declaration — manifestFeatures — so it is
// only ever the right app for a run that wants exactly what that declaration
// covers. Two runs do not:
//
//   - a caller that supplied its own ScopeHint lines knows something about its
//     call surface this flow does not (builtins.Request.ScopeHint says so in
//     as many words), and an app built from a declaration that disagrees with
//     those lines is one that installs cleanly and then 403s on the very call
//     the caller named;
//   - an intent that mentions writing wants chat:write, which the directory
//     sync does not declare and this manifest therefore does not request.
//
// Neither is refused, and neither loses anything it had: both get the flow
// exactly as it was, printing the caller's own scope lines and asking for a
// token from an app the operator set up themselves. What they do not get is an
// offer to generate the wrong app — which would be worse than the manual route
// it replaced, because an app that installs and then cannot do its job is
// harder to diagnose than one that was never created.
func offersManifest(req builtins.Request) bool {
	return len(req.ScopeHint) == 0 && !wantsWrite(req.UserIntent)
}

// manifestScreen renders the Slack app manifest for this credential and leaves
// a copy beside the run.
//
// It asks nothing: Prepare returns no group and Apply does the work, the shape
// tui.Screen gives a step that is neither a question nor a summary — the same
// shape flowscreens.Browser has. A failed write is not fatal and is not
// swallowed either: SavedManifest carries the error and MessageLines says so
// in the guidance, on the screen that holds the only other copy of the
// manifest.
type manifestScreen struct {
	// identity names the app, and names the file. Both are the AgentIdentity
	// the credential is for, which is the only name this flow knows.
	identity string
	dir      string

	// yaml is computed in the constructor rather than in Apply. It is a pure
	// function of identity and manifestFeatures, and computing it up front
	// means the guidance below cannot render an empty block if the screens are
	// ever reordered around it.
	yaml string

	saved     slack.SavedManifest
	attempted bool
}

func newManifestScreen(identity, dir string) *manifestScreen {
	return &manifestScreen{
		identity: identity,
		dir:      dir,
		yaml:     slack.BotTokenAppManifestFor(identity, manifestFeatures),
	}
}

func (s *manifestScreen) ID() string { return "manifest" }

func (s *manifestScreen) Label() string { return "Manifest" }

// Prepare skips the screen entirely on a run that is using an app it already
// has: there is nothing to generate, and nothing to write.
func (s *manifestScreen) Prepare(_ context.Context, st *tui.State) (*huh.Group, error) {
	if st.Get(KeyAppRoute) != RouteManifest {
		return nil, tui.ErrSkip
	}
	return nil, nil
}

// Apply leaves the copy on disk. THE FILE IS THE POINT, not a nicety: the
// manifest is shown on a note in scrollback that the next command scrolls
// away, and the copy is what the operator still has when they come back to
// paste it into Slack.
func (s *manifestScreen) Apply(_ context.Context, st *tui.State) error {
	if s.dir == "" {
		// Nowhere to write — a surface that offered no directory. The manifest
		// is still on the screen, so there is nothing to report and nothing
		// lost.
		return nil
	}
	s.attempted = true
	s.saved = slack.SaveManifest(s.dir, s.identity, s.yaml)
	if err := s.saved.Err(); err != nil {
		// Not returned: ending the run here would take the manifest with it.
		// Said out loud twice instead — once in the run summary, and once in
		// the guidance MessageLines composes, which is beside the only other
		// copy there is.
		st.Note("App manifest", "not saved ("+err.Error()+") — copy it from the screen")
		return nil
	}
	st.Note("App manifest", filepath.Base(s.saved.Path()))
	return nil
}

// savedLines is what the guidance says about the copy on disk, or "" when this
// run never had anywhere to write one.
func (s *manifestScreen) savedLines() string {
	if !s.attempted {
		return ""
	}
	return s.saved.MessageLines()
}

// Result stores the pasted token as a bearer credential.
//
// The emptiness check is not belt-and-braces: huh's accessible renderer has no
// error channel, so an input that runs out arrives here as an unanswered State
// and a nil error. Storing that leaves a credential authenticating as nobody
// behind a CLI that reported success.
func (*Flow) Result(ctx context.Context, req builtins.Request, st *tui.State) error {
	if st == nil {
		return errors.New("slack-bot-token: no token was supplied")
	}
	token := strings.TrimSpace(st.Get(KeyToken))
	if token == "" {
		return errors.New("slack-bot-token: no token was supplied")
	}
	if req.Provider != nil {
		if err := provider.ValidateToken(*req.Provider, token); err != nil {
			return fmt.Errorf("slack-bot-token: %w", err)
		}
	}
	if req.Store == nil {
		return errors.New("slack-bot-token: nowhere to store the token")
	}
	return req.Store(ctx, builtins.StoreValue{Bearer: token})
}

// Verify reports VerifyUnsupported: Slack has no probe this flow can run
// honestly.
//
// auth.test is the obvious candidate and is a trap. It answers HTTP 200 for a
// token it REJECTS, putting the verdict in the body as
// {"ok": false, "error": "invalid_auth"} — so the declarative status-based
// probe every other bearer provider uses (builtins.VerifyHTTPBearer) would
// report a dead token as live and stamp a subject id on it. Claiming a
// verification that did not happen is worse than reporting none, so the
// provider declares no verify: block and this says so plainly.
func (*Flow) Verify(_ context.Context, _ builtins.VerifyRequest) (builtins.VerifyResult, error) {
	return builtins.VerifyResult{
		Status: builtins.VerifyUnsupported,
		Detail: "Slack answers 200 for a rejected token, so a status-based probe would confirm nothing",
	}, nil
}

// routeGuidance is what the user reads above the route question. Short on
// purpose: the manifest itself is two screens away, and a wall of text above a
// two-option list is a wall of text nobody finishes.
func routeGuidance() string {
	var b strings.Builder
	b.WriteString("The credential is a Slack app's bot token.\n\n")
	b.WriteString("oap can write the app's manifest for you, with\n")
	b.WriteString("exactly the scopes this sync calls with — you\n")
	b.WriteString("create the app from it in two clicks and paste\n")
	b.WriteString("the token it issues. Creating it by hand means\n")
	b.WriteString("adding those scopes yourself; the app that\n")
	b.WriteString("misses one installs cleanly and syncs nothing.")
	return b.String()
}

// routeNote is the route's line in the run summary. Says what the run DID, not
// which option was highlighted.
func routeNote(v string) string {
	if v == RouteManifest {
		return "created from a generated manifest"
	}
	return "an app the operator already had"
}

// manifestGuidance is what the user reads above the token field on the
// generate-the-app route: what became of the copy on disk, the manifest, and
// the clicks either side of it.
//
// The two step blocks come from the Slack channel wizard rather than from here
// (slack.CreateAppFromManifestStep, slack.InstallAndCopyBotTokenStep). They
// name Slack's own UI labels, which an operator matches against the screen in
// front of them, and a second copy is a second place for those labels to drift
// from what Slack calls them.
//
// Every line is kept inside the note's column budget, which this package's
// tests assert. The manifest is the strict half of that: huh hard-wraps a long
// line, a wrapped YAML line continues at column 0 inside an indented block,
// and Slack's manifest import REJECTS that — with nothing in the render to say
// why.
func manifestGuidance(req builtins.Request, browserErr error, man *manifestScreen) string {
	var b strings.Builder
	if browserErr != nil {
		// The reason comes from the operating system and can be any length; on a
		// line of its own, a long one cannot drag the next instruction into a wrap.
		fmt.Fprintf(&b, "Your browser didn't open:\n  %s\nVisit the page below yourself.\n\n", browserErr)
	}
	fmt.Fprintf(&b, "Slack app for %q:\n\n", req.IdentityName)
	if saved := man.savedLines(); saved != "" {
		b.WriteString(saved + "\n\n")
	}
	// No separator after the manifest: CreateAppFromManifestStep already ends
	// one blank line past it, the same way the channel wizard's own step does.
	b.WriteString(slack.CreateAppFromManifestStep(1, man.yaml))
	b.WriteString(slack.InstallAndCopyBotTokenStep(2))
	b.WriteString("\n")
	b.WriteString("  3. Paste that token below — the xoxb- one,\n")
	b.WriteString("     not xapp- or xoxp-.\n")
	b.WriteString("\nPage: " + appsPage + "\n")
	return strings.TrimRight(b.String(), "\n")
}

// guidance is what the user reads above the field on the route that uses an
// app they already have: which scopes it needs, which of its several tokens to
// paste, and where — repeated in text because a browser that refused to open
// leaves the address as the only way through.
//
// Every line is kept inside the note's column budget, which this package's
// tests assert: the address and the scope names have to survive being copied,
// and huh wraps an over-long line with no sign the halves belong together.
func guidance(req builtins.Request, browserErr error) string {
	var b strings.Builder
	if browserErr != nil {
		fmt.Fprintf(&b, "Your browser didn't open:\n  %s\nVisit the page below yourself.\n\n", browserErr)
	}
	fmt.Fprintf(&b, "Slack app for %q:\n\n", req.IdentityName)
	b.WriteString("  1. Add the scopes below under\n")
	b.WriteString("     'OAuth & Permissions'.\n")
	b.WriteString("  2. Install the app to your workspace.\n")
	b.WriteString("  3. Paste its 'Bot User OAuth Token' below —\n")
	b.WriteString("     the xoxb- one, not xapp- or xoxp-.\n")
	b.WriteString("\nSuggested bot token scopes:\n")
	for _, line := range recommendedScopes(req) {
		b.WriteString("  " + line + "\n")
	}
	b.WriteString("\nPage: " + appsPage + "\n")
	return strings.TrimRight(b.String(), "\n")
}

// recommendedScopes is what the guidance actually prints: the caller's own
// lines when it supplied any, and otherwise this flow's guess from intent. A
// caller that knows its own call surface beats a heuristic over prose; see
// builtins.Request.ScopeHint. No shipped caller overrides this flow's guess
// today — the directory sync's kind returns nil rather than restate a list
// that is already derived below and already tested — but honouring the field
// is what stops a caller that DID supply lines from having them silently
// dropped. It is also what stops such a caller being offered a generated app
// built from a declaration its lines disagree with; see offersManifest.
func recommendedScopes(req builtins.Request) []string {
	if len(req.ScopeHint) > 0 {
		return req.ScopeHint
	}
	return scopeHint(req.UserIntent)
}

// scopeHint maps a coarse user-intent string to a concise scope
// recommendation, one short line per entry.
//
// The read set is derived from the calls a Slack directory sync actually
// makes, not from Slack's catalogue, and matches the kind's own
// relsyncMethodBotScopes table: conversations.list, .info and .members are
// each gated on channels:read for a public channel and groups:read for a
// private one; users.info per member needs users:read; and the member's
// email — the field the identity join is keyed on, which users.info withholds
// without a scope of its own — needs users:read.email. auth.test needs none.
//
// Writing is additive rather than a different set, because an agent that posts
// still has to read what it is posting into. Split across lines because the
// note rendering it has roughly sixty columns, and a scope list that wraps
// mid-identifier reads as a scope nobody can look up.
func scopeHint(intent string) []string {
	read := []string{
		"channels:read      public channels + members",
		"groups:read        private channels it is in",
		"users:read         member profiles",
		"users:read.email   the email members join on",
	}
	if wantsWrite(intent) {
		return append(read, "chat:write         post messages as the app")
	}
	return read
}

// wantsWrite reads a coarse intent string for "this credential will post, not
// just read".
//
// Its own function because two decisions turn on it and they must not drift:
// which scopes are RECOMMENDED (scopeHint), and whether a generated app is
// offered at all (offersManifest). A run told to recommend chat:write and then
// handed a manifest that does not request it is the exact mismatch the second
// check exists to avoid.
func wantsWrite(intent string) bool {
	low := strings.ToLower(intent)
	return strings.Contains(low, "write") || strings.Contains(low, "post")
}
