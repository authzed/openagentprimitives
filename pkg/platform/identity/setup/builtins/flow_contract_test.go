package builtins_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/cli/tui"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/setup/builtins"
	_ "github.com/authzed/openagentprimitives/pkg/platform/identity/setup/builtins/loader"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/setup/builtins/slack_bot_token"
)

// registeredFlows is the real flow set, snapshotted at init.
//
// It is captured here rather than read inside each test because other tests in
// this binary call builtins.Reset to install fakes, and a contract asserted over
// whatever the registry happens to hold at the time would quietly assert
// nothing. The loader's blank import above is what guarantees the real flows
// have registered by the time this runs: an imported package's init completes
// before the importing package's does.
var registeredFlows = builtins.All()

// TestEveryFlowDescribesScreensRatherThanPrompting is the contract every
// credential-setup flow answers to, asserted over the whole registry rather
// than per flow: a flow that is added tomorrow is covered the day it registers.
//
// The interesting half is Result. huh's accessible renderer has no error
// channel — an input script that runs out yields a fully-defaulted form and a
// nil error — so "the run finished" proves nothing about what the user typed.
// Result is the one place that can tell an answered flow from a silent one,
// and these flows store TOKENS: a Result that stored an empty credential
// without complaint would leave an AgentIdentity that authenticates as nobody
// and a CLI that says it succeeded.
func TestEveryFlowDescribesScreensRatherThanPrompting(t *testing.T) {
	require.NotEmpty(t, registeredFlows, "the loader must register at least one flow for this contract to mean anything")

	for _, f := range registeredFlows {
		t.Run(f.Name(), func(t *testing.T) {
			require.NotEmpty(t, f.Name(), "a flow with no name cannot be resolved from a provider's builtin: field")

			// Store records rather than refusing outright, so that a flow which
			// stores something it should not is reported as the wrong VALUE it
			// stored — the detail that says which guard is missing — instead of
			// as a bare fatal inside a callback.
			var storedCalls []builtins.StoreValue
			req := builtins.Request{
				IdentityName: "demo-bot",
				Namespace:    "demo-ns",
				Store: func(_ context.Context, v builtins.StoreValue) error {
					storedCalls = append(storedCalls, v)
					return nil
				},
			}

			screens, err := f.Screens(context.Background(), req)
			if err != nil {
				// A flow with nothing to offer says so. What it must not do is
				// return neither screens nor a reason, which would run a wizard
				// that asks nothing and then store whatever an unanswered State
				// yielded.
				assert.Empty(t, screens, "a flow that refuses must not also hand back screens")
			} else {
				require.NotEmpty(t, screens, "Screens returned no screens and no reason")
				seen := map[string]bool{}
				for i, s := range screens {
					assert.NotEmpty(t, s.ID(), "screen %d has no ID; the step rail and every wrapped error key off it", i)
					assert.NotEmpty(t, s.Label(), "screen %q has no Label; the rail would render a blank step", s.ID())
					assert.False(t, seen[s.ID()], "screen ID %q is used twice; the rail cannot highlight either", s.ID())
					seen[s.ID()] = true
				}
			}

			assert.Empty(t, storedCalls, "describing a flow must not store anything")

			// The fail-closed half: an unanswered State must never reach Store.
			err = f.Result(context.Background(), req, tui.NewState())
			assert.Error(t, err, "Result must refuse an unanswered State rather than storing an empty credential")
			assert.Empty(t, storedCalls,
				"Result stored %+v for a State that holds no answers at all", storedCalls)
		})
	}
}

// TestResultToleratesANilState guards the shape a caller reaches Result with
// after a run that failed partway: tui.RunWith always hands back a usable
// State, but a caller holding a zero value must get an error rather than a
// panic on a flow that stores secrets.
func TestResultToleratesANilState(t *testing.T) {
	for _, f := range registeredFlows {
		t.Run(f.Name(), func(t *testing.T) {
			var storedCalls []builtins.StoreValue
			req := builtins.Request{
				IdentityName: "demo-bot",
				Store: func(_ context.Context, v builtins.StoreValue) error {
					storedCalls = append(storedCalls, v)
					return nil
				},
			}
			assert.NotPanics(t, func() {
				err := f.Result(context.Background(), req, nil)
				assert.Error(t, err, "a nil State carries no answers, so Result must refuse")
			})
			assert.Empty(t, storedCalls, "Result stored %+v for a nil State", storedCalls)
		})
	}
}

// TestResultRefusesAnAnswerThatIsBlank is the emptiness guard on its own,
// tested against a provider that declares no token shape.
//
// Without the shapeless provider this proves nothing: every real provider here
// declares a format, and a format check refuses "" as a side effect — so a flow
// that had dropped its emptiness guard entirely would still look correct. But
// provider.ValidateToken is deliberately permissive, accepting anything from a
// provider whose format we do not know, and a flow leaning on it for this would
// happily store a blank credential for one of those.
func TestResultRefusesAnAnswerThatIsBlank(t *testing.T) {
	for _, f := range registeredFlows {
		t.Run(f.Name(), func(t *testing.T) {
			screens, err := f.Screens(context.Background(), builtins.Request{IdentityName: "demo-bot"})
			if err != nil {
				t.Skipf("%s describes no screens in this build, so it has no answers to blank out", f.Name())
			}

			// Explicitly blank, not absent: "answered with nothing" and "never
			// asked" are different States, and only the first can reach Result
			// from a driver that ran.
			st := tui.NewState()
			for _, s := range screens {
				keyer, ok := s.(interface{ AnswerKeys() []string })
				if !ok {
					continue
				}
				for _, k := range keyer.AnswerKeys() {
					st.Set(k, "   ")
				}
			}

			var storedCalls []builtins.StoreValue
			req := builtins.Request{
				// No Provider: nothing declares a format, so the emptiness
				// refusal is the only thing that can refuse this.
				IdentityName: "demo-bot",
				Store: func(_ context.Context, v builtins.StoreValue) error {
					storedCalls = append(storedCalls, v)
					return nil
				},
			}
			assert.Error(t, f.Result(context.Background(), req, st),
				"a blank answer must be refused even when no provider format would have caught it")
			assert.Empty(t, storedCalls, "Result stored %+v for a blank answer", storedCalls)
		})
	}
}

// answerFor supplies one flow-appropriate answer per registered flow, keyed by
// flow name, for TestNoCredentialReachesTheSummary.
//
// A table rather than one generic string because the answers are not
// interchangeable: three of these flows want a credential, and one wants the
// path of a file that has to exist and parse. What keeps the table from rotting
// into a list nobody maintains is that the test REQUIRES an entry for every
// flow that describes screens, and fails naming the flow when one is missing —
// so a flow added without an entry is reported rather than silently skipped,
// which is exactly the flow whose masking nobody has checked.
func answerFor(t *testing.T) map[string]string {
	t.Helper()

	kubeconfig := filepath.Join(t.TempDir(), "config")
	require.NoError(t, os.WriteFile(kubeconfig, []byte(
		"apiVersion: v1\nkind: Config\nclusters: []\ncontexts: []\nusers: []\n"+
			"current-context: \"\"\n# sentinel-contents-must-not-be-summarised\n"), 0o600))

	return map[string]string{
		"anthropic-oauth":    "sk-ant-oat01-CONTRACTSENTINELVALUE",
		"github-pat":         "ghp_CONTRACTSENTINELVALUE0123",
		"slack-bot-token":    "xoxb-CONTRACTSENTINELVALUE0123",
		"onepassword-scim":   "op-scim-CONTRACTSENTINELVALUE",
		"tailscale-authkey":  "tskey-auth-kCONTRACT-SENTINELVALUE",
		"kubectl-kubeconfig": kubeconfig,
	}
}

// nonCredentialAnswers seeds, per flow, the questions that are NOT the
// credential.
//
// TestNoCredentialReachesTheSummary answers every key a flow declares with ONE
// sentinel string. That is exactly right for a token field and impossible for
// a question with a fixed answer set: slack-bot-token opens by asking where
// the Slack app comes from, and a choice question REFUSES a value no option
// offered rather than quietly defaulting past it (tui.NewChoice, deliberately
// — see its own test). A flow that grows such a question and is not named here
// fails loudly, naming the choices, which is the right failure and not one to
// engineer around.
//
// The answer chosen must leave the CREDENTIAL question in the run. This
// contract is about what the stored credential does to the summary, so an
// answer that routed past the screen collecting it would prove nothing — which
// is why slack-bot-token is answered "an app I already have" rather than the
// route that generates one and writes a file into whatever directory the suite
// happens to be running from.
var nonCredentialAnswers = map[string]map[string]string{
	"slack-bot-token": {slack_bot_token.KeyAppRoute: slack_bot_token.RouteExisting},
}

// credentialsIn returns the strings in v that ARE the credential — the ones
// whose disclosure is the harm. Client ids, token endpoints and scopes are
// deliberately absent: they are not secret, and treating them as such would
// make this test refuse legitimate summary lines.
func credentialsIn(v builtins.StoreValue) []string {
	out := []string{}
	add := func(s string) {
		if strings.TrimSpace(s) != "" {
			out = append(out, s)
		}
	}
	add(v.Bearer)
	add(v.KubeconfigYAML)
	if v.OAuth != nil {
		add(v.OAuth.AccessToken)
		add(v.OAuth.RefreshToken)
		add(v.OAuth.ClientSecret)
	}
	return out
}

// flowsWithTheirOwnMaskingTest names the flows whose masking is asserted in
// their own package instead of by TestNoCredentialReachesTheSummary below, and
// is what stops that exemption from being silent.
//
// The test cannot describe every flow: it drives one with a bare
// builtins.Request and seeds every answer key from a single string, so a flow
// that needs a resolved Target to have any screens at all returns an error from
// Screens and is skipped — and the skip fires BEFORE the answerFor lookup that
// is supposed to report an unmaintained flow. Left alone, "a flow added without
// an entry is reported rather than skipped" has a hole shaped like "needs a
// Target", and the first flow through it mints OAuth tokens.
//
// So a skipping flow must be named here, and being named is a claim that the
// flow's own package asserts the same property against a real run. The value is
// where that test lives, so a reader can go and check it — and deleting it
// breaks this test, which is the tie that makes the exemption honest.
var flowsWithTheirOwnMaskingTest = map[string]string{
	"oauth-mcp": "oauth_mcp.TestNoCredentialReachesTheSummaryOnTheOAuthFlow " +
		"(needs an MCPServer target, and mints its credential through a live exchange " +
		"rather than taking it from a seedable answer key)",
}

// TestEveryExemptFlowIsStillRegistered keeps flowsWithTheirOwnMaskingTest from
// outliving the flows it excuses. A renamed or deleted flow leaves an entry
// behind that would silently excuse nothing — or, worse, excuse a DIFFERENT
// flow that later takes the same name.
func TestEveryExemptFlowIsStillRegistered(t *testing.T) {
	registered := map[string]bool{}
	for _, f := range registeredFlows {
		registered[f.Name()] = true
	}
	for name := range flowsWithTheirOwnMaskingTest {
		assert.True(t, registered[name],
			"flow %q is exempted from the summary-masking contract but is no longer registered; drop the entry", name)
	}
}

// TestNoCredentialReachesTheSummary is the masking contract, asserted over the
// whole registry rather than per flow.
//
// The summary is rendered to the user's scrollback after the run, where it
// outlives the terminal the credential was typed into — and it is a plain
// io.Writer, so it is equally the thing a `> setup.log` redirect captures.
// tui.QuestionOpts.NoteValue is what masks a value there, and it is a func that
// defaults to NIL — and nil means record the answer verbatim. So a flow whose
// author forgets it writes the raw credential into a note and nothing else
// notices; the hazard survived the move off flowscreens' Secret bool, under a
// different name and the same default. All five flows today are correct; this
// is what keeps the sixth honest.
//
// The oracle is derived rather than declared: whatever reached Store is the
// credential, by definition, so nothing that reached Store may appear verbatim
// in a note. That deliberately does NOT flag kubectl-kubeconfig's summary of
// the PATH it read — a path is not what was stored — while it does flag a raw
// token, which is.
func TestNoCredentialReachesTheSummary(t *testing.T) {
	answers := answerFor(t)

	for _, f := range registeredFlows {
		t.Run(f.Name(), func(t *testing.T) {
			req := builtins.Request{IdentityName: "demo-bot", Namespace: "demo-ns"}

			screens, err := f.Screens(context.Background(), req)
			if err != nil {
				// Checked BEFORE skipping, not after. A flow this test cannot
				// describe is still a flow that writes a summary, so it has to
				// say where its masking IS asserted — otherwise the skip below
				// is indistinguishable from no coverage at all.
				where, exempt := flowsWithTheirOwnMaskingTest[f.Name()]
				require.True(t, exempt,
					"flow %q cannot be described here (%v), so this contract cannot cover it. Assert its masking "+
						"in its own package and name it in flowsWithTheirOwnMaskingTest.", f.Name(), err)
				t.Skipf("%s asserts its own masking: %s", f.Name(), where)
			}

			answer, ok := answers[f.Name()]
			require.True(t, ok,
				"flow %q has no entry in answerFor, so its masking is unchecked; add one", f.Name())

			// Seeded, so no screen prompts — which also proves the seeding
			// worked, since anything that still asked would read EOF and the
			// driver would have written to out.
			st := tui.NewState()
			overrides := nonCredentialAnswers[f.Name()]
			for _, s := range screens {
				keyer, isKeyer := s.(interface{ AnswerKeys() []string })
				if !isKeyer {
					continue
				}
				for _, k := range keyer.AnswerKeys() {
					if v, ok := overrides[k]; ok {
						st.Set(k, v)
						continue
					}
					st.Set(k, answer)
				}
			}

			var stored []builtins.StoreValue
			req.Store = func(_ context.Context, v builtins.StoreValue) error {
				stored = append(stored, v)
				return nil
			}

			var out bytes.Buffer
			st, err = tui.RunWith(context.Background(), screens, tui.Options{
				Theme: tui.NewTheme(tui.Caps{}),
				In:    strings.NewReader(""),
				Out:   &out,
			}, st)
			require.NoError(t, err, "a fully seeded run must complete without prompting")
			require.NoError(t, f.Result(context.Background(), req, st))
			require.Len(t, stored, 1, "a seeded run must store exactly once")

			creds := credentialsIn(stored[0])
			require.NotEmpty(t, creds, "the flow stored nothing recognisable as a credential, so this test proved nothing")

			notes := st.Notes()
			require.NotEmpty(t, notes, "the flow recorded no summary at all, so this test proved nothing")

			for _, n := range notes {
				for _, cred := range creds {
					assert.NotContains(t, n.Value, cred,
						"summary line %q carries the stored credential verbatim; the field that collected it needs Secret: true", n.Label)
					assert.NotContains(t, n.Label, cred,
						"summary label %q carries the stored credential verbatim", n.Label)
				}
			}
		})
	}
}
