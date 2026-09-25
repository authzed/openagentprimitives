package github

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/github/appprovision"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/kindtest"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/wizardkeys"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/sensitive"
	"github.com/authzed/openagentprimitives/pkg/platform/oap"
)

// ---------------------------------------------------------------------------
// fixtures
// ---------------------------------------------------------------------------

// dataInput is the WizardInput a run is described against: one
// AgentClass in the namespace, so the enum has a value and the derived
// defaults are unambiguous (P5-R16).
func dataInput(t *testing.T) channelkinds.WizardInput {
	t.Helper()
	return channelkinds.WizardInput{
		K8s:       newFakeK8s(newAgentClass("demo-reviewbot", "default")).Build(),
		Namespace: "default",
	}
}

// seededInput is dataInput with the given answers already supplied by flags.
//
// The map is COPIED: WizardInput is passed by value but the map inside it is
// not, so a test that went on to mutate its own literal would change what a
// wizard it had already called was given.
func seededInput(t *testing.T, seeded map[string]string) channelkinds.WizardInput {
	t.Helper()
	in := dataInput(t)
	in.Seeded = make(map[string]string, len(seeded))
	maps.Copy(in.Seeded, seeded)
	return in
}

// writePEM writes a plausible downloaded private key and returns its path.
func writePEM(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "demo-app.private-key.pem")
	require.NoError(t, os.WriteFile(path,
		[]byte("-----BEGIN RSA PRIVATE KEY-----\nZmFrZWZha2U=\n-----END RSA PRIVATE KEY-----\n"), 0o600))
	return path
}

// answerFixtures is one plausible answer per key this kind can ask for.
//
// Keyed by question NAME and consumed only through answersFor, which requires
// an entry for every question a run declares: a question added to either set
// with no fixture here fails loudly rather than being silently unanswered by
// the completeness test below.
func answerFixtures(t *testing.T) map[string]string {
	t.Helper()
	return map[string]string{
		keyAgentClass:              "demo-reviewbot",
		keyAppSource:               appSourceCreate,
		keyOwnerType:               ownerTypeOrg,
		keyOrg:                     "demo-org",
		keyExternalBaseURL:         "https://ap.demo.test",
		wizardkeys.KeyChannelName:  "demo-reviewbot-gh",
		wizardkeys.KeyAuthzSubject: "service:demo-reviewbot-github",
		keyAppID:                   "12345",
		keySlug:                    "demo-manual",
		keyPrivateKeyPath:          writePEM(t),
		keyPrivateKeyPEM:           "-----BEGIN RSA PRIVATE KEY-----\nc2VlZGVk\n-----END RSA PRIVATE KEY-----\n",
		keyWebhookSecret:           "whsec_faketestwebhooksecretvalue",
		keyInstallationID:          "67890",
	}
}

// answersFor builds the answer map an operator who answered exactly these
// question sets would produce.
func answersFor(t *testing.T, fixtures map[string]string, sets ...[]oap.Question) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, set := range sets {
		for _, q := range set {
			v, ok := fixtures[q.Name]
			require.Truef(t, ok, "no fixture answer for question %q; add one to answerFixtures", q.Name)
			out[q.Name] = v
		}
	}
	return out
}

// callbackValues is the query GitHub's redirect arrives with. An empty code
// yields a callback carrying none, which is the shape the missing-code test
// needs.
func callbackValues(code string) url.Values {
	v := url.Values{}
	if code != "" {
		v.Set("code", code)
	}
	return v
}

// sensitiveValue wraps a fixture the way appprovision.Conversion carries the
// App's real secrets, so Complete's unwrapping is exercised rather than
// bypassed.
func sensitiveValue(s string) sensitive.SensitiveValue {
	return sensitive.NewSensitiveValue([]byte(s))
}

// fakeConverter stands in for appprovision.HTTPClient so no test reaches
// api.github.com.
type fakeConverter struct {
	gotCode string
	conv    *appprovision.Conversion
	err     error
}

func (f *fakeConverter) Convert(_ context.Context, code string) (*appprovision.Conversion, error) {
	f.gotCode = code
	if f.err != nil {
		return nil, f.err
	}
	return f.conv, nil
}

// ---------------------------------------------------------------------------
// Inputs — the prompt-text gate (P5-R3)
// ---------------------------------------------------------------------------

// pinnedInputPrompts is what Inputs must declare on an ordinary run: the
// AgentClass, where the App COMES FROM, the owner TYPE, the owner, the base
// URL, the Channel name and the subject, in that order.
//
// The App's source comes second because it settles what the rest of the run
// IS: whether this run registers an App — and may therefore later repoint its
// webhook — or records the details of one the operator brought, which it never
// writes to. The owner type comes BEFORE the owner because every GitHub
// address this kind produces branches on it, and an operator has to have
// settled it before they are asked for the login it qualifies.
//
// Every prompt is a LITERAL, never a reference to the constant the production
// code uses: comparing a constant against itself proves nothing, and a
// reworded prompt is invisible to every other gate in the tree.
func pinnedInputPrompts() []kindtest.PromptShape {
	return []kindtest.PromptShape{
		{Name: "agentclass", Type: oap.QEnum, Prompt: "Bind this reviewbot Channel to AgentClass", Required: true},
		{Name: "app-source", Type: oap.QEnum, Prompt: "Does a GitHub App for this already exist", Required: true},
		{Name: "owner-type", Type: oap.QEnum, Prompt: "Who owns the GitHub App", Required: true},
		{Name: "org", Type: oap.QString, Prompt: "GitHub organization or user login", Required: true},
		{Name: "external-base-url", Type: oap.QString, Prompt: "External base URL", Required: true},
		{Name: "name", Type: oap.QString, Prompt: "Channel resource name", Required: true},
		{Name: "authzsubject", Type: oap.QString, Prompt: "SpiceDB authzSubject", Required: true},
	}
}

// pinnedInputPromptsWithout is the up-front list minus one question, for the
// routes that do not ask it. DERIVED from the one list rather than written out
// a second time: two copies would let a question added to the ordinary run go
// missing from the seeded one without anything reddening.
func pinnedInputPromptsWithout(name string) []kindtest.PromptShape {
	all := pinnedInputPrompts()
	out := make([]kindtest.PromptShape, 0, len(all))
	for _, p := range all {
		if p.Name != name {
			out = append(out, p)
		}
	}
	return out
}

// TestGitHubWizard_TheOwnerTypeIsAChoiceAnOperatorCanRead pins the half of the
// owner-type question PromptShape does not carry: its two values, which a
// scripted `--answer owner-type=…` pins, and the labels beside them, which are
// the only thing an operator actually reads.
//
// Both halves are asserted as LITERALS. The values are a compatibility surface
// for a script; the labels are the whole of the operator's information, and a
// label that merely echoed its value ("organization", "user") would put them
// back to guessing which one a personal GitHub account is.
func TestGitHubWizard_TheOwnerTypeIsAChoiceAnOperatorCanRead(t *testing.T) {
	qs, err := (&wizard{}).Inputs(context.Background(), dataInput(t))
	require.NoError(t, err)
	q := questionNamed(t, qs, keyOwnerType)

	assert.Equal(t, []string{"organization", "user"}, q.Enum,
		"the VALUES are what `--answer owner-type=…` writes, so they are pinned")
	assert.Equal(t, []string{
		"An organization — the App is registered under the organization's settings",
		"A personal account — the App is registered under your own account settings",
	}, q.EnumLabels, "and the LABELS are the only thing that tells an operator which is which")
	assert.Nil(t, q.Default,
		"a default here would be a guess at which kind of account the operator owns, accepted with a bare Enter")
}

// pinnedFallbackPrompts is what the handoff's fallback must ask — the
// "appmanual" and "installation" screens' questions, in their screen order.
func pinnedFallbackPrompts() []kindtest.PromptShape {
	return []kindtest.PromptShape{
		{Name: "app-id", Type: oap.QString, Prompt: "App ID", Required: true},
		{Name: "slug", Type: oap.QString, Prompt: "App slug", Required: true},
		{Name: "private-key-path", Type: oap.QString, Prompt: "Path to the App's downloaded private key (.pem)", Required: true},
		{Name: "webhook-secret", Type: oap.QSecret, Prompt: "Webhook secret", Required: true},
		{Name: "installation-id", Type: oap.QString, Prompt: "Installation ID", Required: true},
	}
}

func TestGitHubWizard_InputsDeclareThePinnedPrompts(t *testing.T) {
	qs, err := (&wizard{}).Inputs(context.Background(), dataInput(t))
	require.NoError(t, err)
	kindtest.AssertPromptShapes(t, qs, pinnedInputPrompts())
}

// TestGitHubWizard_InputsDeriveDefaultsOnlyFromAnUnambiguousAgentClass is
// P5-R16: with two AgentClasses in the namespace the Channel name and the
// authzSubject derive from a GUESS, so no default is offered at all and the
// questions fail closed instead of being silently accepted with a bare Enter.
func TestGitHubWizard_InputsDeriveDefaultsOnlyFromAnUnambiguousAgentClass(t *testing.T) {
	t.Run("one AgentClass: the derived defaults are offered", func(t *testing.T) {
		qs, err := (&wizard{}).Inputs(context.Background(), dataInput(t))
		require.NoError(t, err)
		assert.Equal(t, "github-demo-reviewbot", defaultOf(t, qs, wizardkeys.KeyChannelName))
		assert.Equal(t, "service:demo-reviewbot-github", defaultOf(t, qs, wizardkeys.KeyAuthzSubject))
	})

	t.Run("two AgentClasses: neither derived default is offered", func(t *testing.T) {
		in := channelkinds.WizardInput{
			K8s: newFakeK8s(
				newAgentClass("demo-reviewbot", "default"),
				newAgentClass("demo-other", "default"),
			).Build(),
			Namespace: "default",
		}
		qs, err := (&wizard{}).Inputs(context.Background(), in)
		require.NoError(t, err)
		assert.Nil(t, questionNamed(t, qs, wizardkeys.KeyChannelName).Default,
			"a name derived from a guessed AgentClass can be silently accepted with a bare Enter")
		assert.Nil(t, questionNamed(t, qs, wizardkeys.KeyAuthzSubject).Default,
			"and so can a subject")
	})

	// The seeded class is "demo-other", NOT the first one the listing returns:
	// a run that derived from classes[0] would offer "github-demo-reviewbot"
	// here, which is the exact silent mis-binding P5-R16 is about.
	t.Run("seeded AgentClass: the operator named it, so the defaults derive from it", func(t *testing.T) {
		in := seededInput(t, map[string]string{keyAgentClass: "demo-other"})
		in.K8s = newFakeK8s(
			newAgentClass("demo-reviewbot", "default"),
			newAgentClass("demo-other", "default"),
		).Build()
		qs, err := (&wizard{}).Inputs(context.Background(), in)
		require.NoError(t, err)
		assert.Equal(t, "github-demo-other", defaultOf(t, qs, wizardkeys.KeyChannelName))
		assert.Equal(t, "service:demo-other-github", defaultOf(t, qs, wizardkeys.KeyAuthzSubject))
	})

	// The other half of reading the listing even for a seeded answer: a flag
	// naming an AgentClass that does not exist would otherwise bind the
	// Channel to nothing.
	t.Run("seeded AgentClass that does not exist: refused", func(t *testing.T) {
		_, err := (&wizard{}).Inputs(context.Background(),
			seededInput(t, map[string]string{keyAgentClass: "demo-absent"}))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "demo-absent")
	})
}

// TestGitHubWizard_InputsDeclareTheSeededCredentialsSoTheirFlagsAreAccepted:
// when every App credential arrived from a flag there is no browser round trip
// to run, so the questions the handoff would have asked are declared HERE
// instead. Declaring them is what makes `--answer app-id=…` accepted
// (checkAnswerKeys builds the allowed set from the declared questions) and
// what carries the seeded values into the answer map Result reads —
// renderQuestions drops each one before it is ever rendered.
//
// AND THE ROUTE QUESTION IS NOT ASKED, which is the same rule read the other
// way. A run holding the App's credentials has demonstrated that the App
// exists; asking would change nothing, and asking it as a REQUIRED question
// would break every scripted run that predates it — the exact regression this
// list is here to catch.
func TestGitHubWizard_InputsDeclareTheSeededCredentialsSoTheirFlagsAreAccepted(t *testing.T) {
	f := answerFixtures(t)
	in := seededInput(t, map[string]string{
		keyAppID:          f[keyAppID],
		keySlug:           f[keySlug],
		keyPrivateKeyPEM:  f[keyPrivateKeyPEM],
		keyWebhookSecret:  f[keyWebhookSecret],
		keyInstallationID: f[keyInstallationID],
	})

	qs, err := (&wizard{}).Inputs(context.Background(), in)
	require.NoError(t, err)

	want := append(pinnedInputPromptsWithout("app-source"),
		kindtest.PromptShape{Name: "app-id", Type: oap.QString, Prompt: "App ID", Required: true},
		kindtest.PromptShape{Name: "slug", Type: oap.QString, Prompt: "App slug", Required: true},
		kindtest.PromptShape{Name: "private-key", Type: oap.QSecret, Prompt: "App private key (PEM)", Required: true},
		kindtest.PromptShape{Name: "webhook-secret", Type: oap.QSecret, Prompt: "Webhook secret", Required: true},
		kindtest.PromptShape{Name: "installation-id", Type: oap.QString, Prompt: "Installation ID", Required: true},
	)
	kindtest.AssertPromptShapes(t, qs, want)
}

// TestGitHubWizard_NonInteractiveWithoutTheAppCredentials_RefusesInItsOwnWords
// is P5-R19: nobody is watching, and creating a GitHub App needs a human to
// confirm it on GitHub's own page. The refusal names the flags that WOULD
// answer it, which the dispatcher's generic "this screen has no answer"
// cannot.
func TestGitHubWizard_NonInteractiveWithoutTheAppCredentials_RefusesInItsOwnWords(t *testing.T) {
	in := dataInput(t)
	in.NonInteractive = true

	_, err := (&wizard{}).Inputs(context.Background(), in)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "browser")
	for _, flag := range []string{"--answer app-id=", "--answer slug=", "--answer private-key=", "--answer webhook-secret="} {
		assert.Containsf(t, err.Error(), flag, "the refusal must name %s", flag)
	}
}

// TestGitHubWizard_NonInteractiveWithoutTheInstallationID_RefusesInItsOwnWords:
// installing the App is the second step no flag can stand in for. Its refusal
// is separate from the App-creation one because a run that seeded every
// credential has already done the first.
func TestGitHubWizard_NonInteractiveWithoutTheInstallationID_RefusesInItsOwnWords(t *testing.T) {
	f := answerFixtures(t)
	in := seededInput(t, map[string]string{
		keyAppID:         f[keyAppID],
		keySlug:          f[keySlug],
		keyPrivateKeyPEM: f[keyPrivateKeyPEM],
		keyWebhookSecret: f[keyWebhookSecret],
	})
	in.NonInteractive = true

	_, err := (&wizard{}).Inputs(context.Background(), in)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--answer installation-id=")
}

// TestGitHubWizard_NonInteractiveWithEverySeededAnswer_AsksItsQuestions: the
// two refusals above must not fire on a run that CAN succeed unattended.
func TestGitHubWizard_NonInteractiveWithEverySeededAnswer_AsksItsQuestions(t *testing.T) {
	f := answerFixtures(t)
	in := seededInput(t, map[string]string{
		keyAgentClass:     f[keyAgentClass],
		keyAppID:          f[keyAppID],
		keySlug:           f[keySlug],
		keyPrivateKeyPEM:  f[keyPrivateKeyPEM],
		keyWebhookSecret:  f[keyWebhookSecret],
		keyInstallationID: f[keyInstallationID],
	})
	in.NonInteractive = true

	qs, err := (&wizard{}).Inputs(context.Background(), in)
	require.NoError(t, err, "every step a human is needed for was answered by a flag")
	require.NotEmpty(t, qs)
}

// ---------------------------------------------------------------------------
// Handoff
// ---------------------------------------------------------------------------

func TestGitHubWizard_HandoffCarriesTheManifestAsAFormPost(t *testing.T) {
	spec, err := (&wizard{}).Handoff(context.Background(), dataInput(t))
	require.NoError(t, err)
	require.NotNil(t, spec, "github's App-manifest flow cannot be a form field")
	require.NotNil(t, spec.Begin, "the operator has to be sent somewhere")
	require.NotNil(t, spec.Complete, "and the code that comes back has to be exchanged")

	start, err := spec.Begin(map[string]string{
		keyOwnerType:              ownerTypeOrg,
		keyOrg:                    "demo-org",
		keyExternalBaseURL:        "https://ap.demo.test",
		wizardkeys.KeyChannelName: "demo-reviewbot-gh",
	}, "http://127.0.0.1:54321/callback")
	require.NoError(t, err)

	assert.Equal(t, "https://github.com/organizations/demo-org/settings/apps/new", start.URL,
		"the org is part of the address, which is why Begin cannot be a fixed string")
	assert.Contains(t, start.Explain, "already filled in",
		"an operator told to expect a prefilled form can recognise a blank one as the flow having failed")
	require.NotEmpty(t, start.FormFields["manifest"],
		"GitHub requires the manifest as a POST body; a GET cannot express it")

	var doc struct {
		Name        string `json:"name"`
		URL         string `json:"url"`
		RedirectURL string `json:"redirect_url"`
		HookAttrs   struct {
			URL string `json:"url"`
		} `json:"hook_attributes"`
	}
	require.NoError(t, json.Unmarshal([]byte(start.FormFields["manifest"]), &doc))
	assert.Equal(t, "demo-org-demo-reviewbot-gh", doc.Name)
	assert.Equal(t, "https://ap.demo.test", doc.URL)
	assert.Equal(t, "http://127.0.0.1:54321/callback", doc.RedirectURL,
		"GitHub reads the redirect out of the manifest, not off the query string, so the client's callback has to go in here")
	assert.Contains(t, doc.HookAttrs.URL, "default/demo-reviewbot-gh",
		"the webhook route resolves against the Channel this run is creating")

	require.NotEmpty(t, spec.FallbackInputs,
		"a blocked port or headless host must not make this kind unusable")
}

// TestGitHubWizard_TheAppCreationAddressFollowsTheOwnerType is the regression
// this whole owner-type question exists for.
//
// A personal GitHub account has no /organizations/<login>/ tree at all. Posting
// the App manifest there does not fail loudly: GitHub answers 404, the POST
// body goes nowhere, and the operator's browser lands on a bare App form with
// none of the name, URLs, permissions or events this run just built — which
// reads as the handoff working, so they start typing rather than falling back.
// The address for a personal account is the signed-in account's own settings
// and carries no login, because GitHub already knows whose it is.
//
// The two addresses are asserted as WHOLE literal strings, not as a "contains
// /settings/apps/new": the defect was entirely in the path PREFIX, so an
// assertion that both ends agree on the suffix would have stayed green
// throughout.
func TestGitHubWizard_TheAppCreationAddressFollowsTheOwnerType(t *testing.T) {
	cases := []struct {
		name      string
		ownerType string
		want      string
	}{
		{
			name:      "an organization: the address is scoped by its login",
			ownerType: ownerTypeOrg,
			want:      "https://github.com/organizations/demo-org/settings/apps/new",
		},
		{
			name:      "a personal account: the address is the signed-in account's own settings",
			ownerType: ownerTypeUser,
			want:      "https://github.com/settings/apps/new",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			answers := map[string]string{
				keyOwnerType:              tc.ownerType,
				keyOrg:                    "demo-owner",
				keyExternalBaseURL:        "https://ap.demo.test",
				wizardkeys.KeyChannelName: "demo-reviewbot-gh",
			}
			if tc.ownerType == ownerTypeOrg {
				answers[keyOrg] = "demo-org"
			}

			// The BEGIN route: what the browser is actually POSTed to.
			spec, err := (&wizard{}).Handoff(context.Background(), dataInput(t))
			require.NoError(t, err)
			require.NotNil(t, spec)
			start, err := spec.Begin(answers, "http://127.0.0.1:54321/callback")
			require.NoError(t, err)
			assert.Equal(t, tc.want, start.URL)

			// The MANUAL route: the same address, printed for an operator who
			// is about to fill GitHub's own form in by hand. A fallback that
			// named the org address for a personal account would send them to
			// the same 404 the handoff just avoided.
			guidance, err := spec.FallbackGuidance(answers)
			require.NoError(t, err)
			assert.Contains(t, guidance, tc.want)
		})
	}
}

// TestGitHubWizard_TheInstallationsPageFollowsTheOwnerType is the same rule for
// the page the run points at AFTER applying: "confirm this App's installation
// reaches every repository". An organization's installations live under the
// org; a personal account's under the account's own settings.
func TestGitHubWizard_TheInstallationsPageFollowsTheOwnerType(t *testing.T) {
	cases := []struct {
		ownerType string
		owner     string
		want      string
	}{
		{ownerType: ownerTypeOrg, owner: "demo-org", want: "https://github.com/organizations/demo-org/settings/installations"},
		{ownerType: ownerTypeUser, owner: "demo-owner", want: "https://github.com/settings/installations"},
	}
	for _, tc := range cases {
		t.Run(tc.ownerType, func(t *testing.T) {
			answers := demoAnswers()
			answers[keyOwnerType] = tc.ownerType
			answers[keyOrg] = tc.owner

			out, err := (&wizard{}).Result(channelkinds.WizardInput{Namespace: "default"}, answers)
			require.NoError(t, err)
			assert.Contains(t, strings.Join(out.Notes, "\n"), tc.want)
		})
	}
}

// TestGitHubWizard_AnUnrecognizedOwnerTypeIsRefusedRatherThanReadAsAnOrg:
// ownerIsUser reads everything that is not "user" as an organization, so a
// typo in `--answer owner-type=` would reintroduce the exact bug the question
// removes — silently, on a personal account. Both disjoint routes refuse it.
func TestGitHubWizard_AnUnrecognizedOwnerTypeIsRefusedRatherThanReadAsAnOrg(t *testing.T) {
	for _, bad := range []string{"usr", "personal", "ORGANIZATION", "org"} {
		t.Run(bad, func(t *testing.T) {
			t.Run("the handoff route", func(t *testing.T) {
				spec, err := (&wizard{}).Handoff(context.Background(), dataInput(t))
				require.NoError(t, err)
				require.NotNil(t, spec)
				_, err = spec.Begin(map[string]string{
					keyOwnerType:              bad,
					keyOrg:                    "demo-owner",
					keyExternalBaseURL:        "https://ap.demo.test",
					wizardkeys.KeyChannelName: "demo-reviewbot-gh",
				}, "http://127.0.0.1:1/callback")
				require.Error(t, err, "Begin must refuse before a browser is opened")
				assert.Contains(t, err.Error(), keyOwnerType+": ", "the refusal must name the field")
			})

			t.Run("the seeded route, where there is no handoff at all", func(t *testing.T) {
				answers := demoAnswers()
				answers[keyOwnerType] = bad
				_, err := (&wizard{}).Result(channelkinds.WizardInput{Namespace: "default"}, answers)
				require.Error(t, err)
				assert.Contains(t, err.Error(), keyOwnerType+": ", "the refusal must name the field")
			})
		})
	}
}

// TestGitHubWizard_HandoffBeginFailsClosedOnAMissingAnswer: an empty org would
// build the address https://github.com/organizations//settings/apps/new and an
// empty Channel name a manifest GitHub rejects — both of which look like a
// working handoff right up until the browser lands on a 404.
func TestGitHubWizard_HandoffBeginFailsClosedOnAMissingAnswer(t *testing.T) {
	spec, err := (&wizard{}).Handoff(context.Background(), dataInput(t))
	require.NoError(t, err)
	require.NotNil(t, spec)

	full := map[string]string{
		keyOwnerType:              ownerTypeOrg,
		keyOrg:                    "demo-org",
		keyExternalBaseURL:        "https://ap.demo.test",
		wizardkeys.KeyChannelName: "demo-reviewbot-gh",
	}
	for _, missing := range []string{keyOwnerType, keyOrg, keyExternalBaseURL, wizardkeys.KeyChannelName} {
		t.Run("missing "+missing, func(t *testing.T) {
			answers := map[string]string{}
			for k, v := range full {
				if k != missing {
					answers[k] = v
				}
			}
			_, err := spec.Begin(answers, "http://127.0.0.1:1/callback")
			require.Error(t, err)
			assert.Contains(t, err.Error(), missing)
		})
	}
}

// TestGitHubWizard_MalformedOrgOrBaseURLIsRefusedAsAMalformedAnswer covers the
// two answers whose shape used to be checked in place, by the terminal field
// that asked for them, and re-prompted on the spot.
//
// A question set stated as data carries no validator any client evaluates (see
// channelkinds.ValidateInputs), so every question now ACCEPTS whatever is
// typed and the check has to happen somewhere the answer is read. Both routes
// are covered because they are disjoint: a run with the App's credentials
// already in hand has no handoff at all, so manifestParams never runs and
// Result is the only reader there is.
//
// The refusal must NAME THE FIELD. "demo org" and "ap.example.com" are both
// plausible enough that an operator who mistyped one has no way to tell which
// of the five answers a bare shape complaint refers to — and on the handoff
// route the same bad answer is what the fallback's own guidance would be built
// from, so a message that does not say what to fix is the whole of what they
// get.
//
// THE NAMING ASSERTION MATCHES `<key>: `, WITH THE WRAPPER'S DELIMITER, and
// that is not pedantry. `assert.Contains(err, "org")` is satisfied by the
// substring inside "GitHub organization or user login" — the validator's OWN
// text —
// so it stays green with both `fmt.Errorf("…: %s: %w", keyOrg, err)` wrappers
// stripped, pinning the validation while proving nothing about the naming.
// Only the wrapper can produce "org: ".
func TestGitHubWizard_MalformedOrgOrBaseURLIsRefusedAsAMalformedAnswer(t *testing.T) {
	cases := []struct {
		name string
		key  string
		bad  string
		// wantSay is every substring the refusal must carry. A slice rather
		// than one string because the reachability refusal has to say two
		// separate things — which value, and why it cannot work — and a test
		// that checked only the first would pass against "invalid URL".
		wantSay []string
	}{
		{
			name:    "an org login with a space in it",
			key:     keyOrg,
			bad:     "demo org",
			wantSay: []string{"does not look like a GitHub organization or user login"},
		},
		{
			name:    "a base URL with no scheme",
			key:     keyExternalBaseURL,
			bad:     "ap.example.com",
			wantSay: []string{"must be an absolute URL with a scheme and host"},
		},
		{
			// The value is a perfectly well-formed URL, which is why a shape
			// check let it through and GitHub refused the entire App —
			// "Hook url is not supported because it isn't reachable over the
			// public Internet (127.0.0.1)" — after the operator had been to
			// the browser and submitted the form.
			name:    "a base URL GitHub could never deliver a webhook to",
			key:     keyExternalBaseURL,
			bad:     "http://127.0.0.1:8080",
			wantSay: []string{"http://127.0.0.1:8080", "loopback address", "public Internet", "tunnel"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name+": refused on the handoff route", func(t *testing.T) {
			spec, err := (&wizard{}).Handoff(context.Background(), dataInput(t))
			require.NoError(t, err)
			require.NotNil(t, spec, "an unseeded run has a handoff to refuse from")

			answers := map[string]string{
				keyOwnerType:              ownerTypeOrg,
				keyOrg:                    "demo-org",
				keyExternalBaseURL:        "https://ap.demo.test",
				wizardkeys.KeyChannelName: "demo-reviewbot-gh",
			}
			answers[tc.key] = tc.bad

			_, err = spec.Begin(answers, "http://127.0.0.1:1/callback")
			require.Error(t, err, "Begin must refuse before a browser is opened")
			assert.Contains(t, err.Error(), tc.key+": ", "the refusal must name the field")
			for _, say := range tc.wantSay {
				assert.Contains(t, err.Error(), say, "and say what is wrong with it")
			}
		})

		t.Run(tc.name+": refused on the manual route", func(t *testing.T) {
			// The SAME answers reach fallbackSteps, which builds the reference
			// manifest the operator would paste into GitHub's own form by
			// hand. A value refused on the handoff route and accepted here
			// would hand them a manifest GitHub is going to reject, with the
			// bad hook URL already written into it.
			spec, err := (&wizard{}).Handoff(context.Background(), dataInput(t))
			require.NoError(t, err)
			require.NotNil(t, spec)
			require.NotNil(t, spec.FallbackGuidance)

			answers := map[string]string{
				keyOwnerType:              ownerTypeOrg,
				keyOrg:                    "demo-org",
				keyExternalBaseURL:        "https://ap.demo.test",
				wizardkeys.KeyChannelName: "demo-reviewbot-gh",
			}
			answers[tc.key] = tc.bad

			_, err = spec.FallbackGuidance(answers)
			require.Error(t, err, "the manual route must refuse what the handoff route refuses")
			assert.Contains(t, err.Error(), tc.key+": ", "the refusal must name the field")
			for _, say := range tc.wantSay {
				assert.Contains(t, err.Error(), say, "and say what is wrong with it")
			}
		})

		t.Run(tc.name+": refused on the seeded route", func(t *testing.T) {
			f := answerFixtures(t)
			in := seededInput(t, map[string]string{
				keyAppID:         f[keyAppID],
				keySlug:          f[keySlug],
				keyPrivateKeyPEM: f[keyPrivateKeyPEM],
				keyWebhookSecret: f[keyWebhookSecret],
			})
			// Proven to BE the seeded route, not merely asserted to fail: a run
			// with every credential in hand is sent nowhere, so Result is the
			// only reader of these two answers there is.
			spec, err := (&wizard{}).Handoff(context.Background(), in)
			require.NoError(t, err)
			require.Nil(t, spec, "a run that already has the App must not be sent to create a second one")

			qs, err := (&wizard{}).Inputs(context.Background(), in)
			require.NoError(t, err)
			answers := answersFor(t, f, qs)
			answers[tc.key] = tc.bad

			_, err = (&wizard{}).Result(in, answers)
			require.Error(t, err, "Result must refuse the same shapes the handoff route refuses")
			assert.Contains(t, err.Error(), tc.key+": ", "the refusal must name the field")
			for _, say := range tc.wantSay {
				assert.Contains(t, err.Error(), say, "and say what is wrong with it")
			}
		})
	}
}

func TestGitHubWizard_HandoffFallbackAsksTheManualScreensQuestions(t *testing.T) {
	spec, err := (&wizard{}).Handoff(context.Background(), dataInput(t))
	require.NoError(t, err)
	require.NotNil(t, spec)
	kindtest.AssertPromptShapes(t, spec.FallbackInputs, pinnedFallbackPrompts())
}

// TestGitHubWizard_HandoffFallbackTakesASeededPEMInPlaceOfAPath: the manual
// screen asks for a PATH because a multi-line PEM cannot be typed at a
// line-oriented prompt — but `--answer private-key=<pem>` is the documented
// unattended spelling, and a key the fallback does not declare is refused at
// the flag. So a seeded PEM replaces the path question rather than being
// rejected alongside it.
func TestGitHubWizard_HandoffFallbackTakesASeededPEMInPlaceOfAPath(t *testing.T) {
	f := answerFixtures(t)
	spec, err := (&wizard{}).Handoff(context.Background(),
		seededInput(t, map[string]string{keyPrivateKeyPEM: f[keyPrivateKeyPEM]}))
	require.NoError(t, err)
	require.NotNil(t, spec)

	want := pinnedFallbackPrompts()
	want[2] = kindtest.PromptShape{Name: "private-key", Type: oap.QSecret, Prompt: "App private key (PEM)", Required: true}
	kindtest.AssertPromptShapes(t, spec.FallbackInputs, want)
}

// TestGitHubWizard_HandoffIsSkippedWhenTheAppAlreadyExists mirrors
// appCreateScreen.Prepare's ErrSkip: a run that already has every credential
// must not be sent to GitHub to create a SECOND App.
func TestGitHubWizard_HandoffIsSkippedWhenTheAppAlreadyExists(t *testing.T) {
	f := answerFixtures(t)
	spec, err := (&wizard{}).Handoff(context.Background(), seededInput(t, map[string]string{
		keyAppID:         f[keyAppID],
		keySlug:          f[keySlug],
		keyPrivateKeyPEM: f[keyPrivateKeyPEM],
		keyWebhookSecret: f[keyWebhookSecret],
	}))
	require.NoError(t, err)
	assert.Nil(t, spec, "an App that already exists must not be created again")
}

// TestGitHubWizard_HandoffCompleteExchangesTheCodeForTheCredentials proves the
// exchange leg reaches Convert with the callback's code and returns the four
// credentials as answers — against a fake converter, so no test touches
// api.github.com.
func TestGitHubWizard_HandoffCompleteExchangesTheCodeForTheCredentials(t *testing.T) {
	conv := &fakeConverter{conv: &appprovision.Conversion{
		AppID:         "55555",
		Slug:          "demo-org-demo-reviewbot-gh",
		PEM:           sensitiveValue("-----BEGIN RSA PRIVATE KEY-----\nZTJlZmFrZQ==\n-----END RSA PRIVATE KEY-----\n"),
		WebhookSecret: sensitiveValue("whsec_e2efaketestsecret"),
	}}
	spec, err := (&wizard{convert: conv}).Handoff(context.Background(), dataInput(t))
	require.NoError(t, err)
	require.NotNil(t, spec)

	got, err := spec.Complete(context.Background(), callbackValues("fake-manifest-code-abc"))
	require.NoError(t, err)
	assert.Equal(t, "fake-manifest-code-abc", conv.gotCode)
	// The four credentials, plus the marker recording that THIS run created
	// the App — the one answer the manual route cannot produce, and the only
	// thing Result can tell the two routes apart by.
	assert.Equal(t, map[string]string{
		keyAppID:            "55555",
		keySlug:             "demo-org-demo-reviewbot-gh",
		keyPrivateKeyPEM:    "-----BEGIN RSA PRIVATE KEY-----\nZTJlZmFrZQ==\n-----END RSA PRIVATE KEY-----\n",
		keyWebhookSecret:    "whsec_e2efaketestsecret",
		keyProvisionedByOAP: "true",
	}, got)
}

func TestGitHubWizard_HandoffCompleteRefusesACallbackWithNoCode(t *testing.T) {
	conv := &fakeConverter{}
	spec, err := (&wizard{convert: conv}).Handoff(context.Background(), dataInput(t))
	require.NoError(t, err)
	require.NotNil(t, spec)

	_, err = spec.Complete(context.Background(), callbackValues(""))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "exchange code")
	assert.Empty(t, conv.gotCode, "a callback with no code must not be exchanged")
}

func TestGitHubWizard_HandoffCompleteSurfacesAnExchangeFailure(t *testing.T) {
	spec, err := (&wizard{convert: &fakeConverter{err: errors.New("github /app-manifests/{code}/conversions: status 422")}}).
		Handoff(context.Background(), dataInput(t))
	require.NoError(t, err)
	require.NotNil(t, spec)

	_, err = spec.Complete(context.Background(), callbackValues("fake-code"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "status 422")
	assert.NotContains(t, err.Error(), "fake-code",
		"the one-time code must never reach an error string")
}

// ---------------------------------------------------------------------------
// FallbackGuidance
// ---------------------------------------------------------------------------

// TestGitHubWizard_FallbackGuidanceCarriesTheReferenceManifest: the manual
// route is unusable without it — an operator filling in GitHub's own form by
// hand has to be told the webhook URL, the permissions and the events, none of
// which they can guess.
func TestGitHubWizard_FallbackGuidanceCarriesTheReferenceManifest(t *testing.T) {
	dir := t.TempDir()
	in := dataInput(t)
	in.WorkingDir = dir

	spec, err := (&wizard{}).Handoff(context.Background(), in)
	require.NoError(t, err)
	require.NotNil(t, spec)
	require.NotNil(t, spec.FallbackGuidance)

	got, err := spec.FallbackGuidance(map[string]string{
		keyOwnerType:              ownerTypeOrg,
		keyOrg:                    "demo-org",
		keyExternalBaseURL:        "https://ap.demo.test",
		wizardkeys.KeyChannelName: "demo-reviewbot-gh",
	})
	require.NoError(t, err)
	assert.Contains(t, got, "https://github.com/organizations/demo-org/settings/apps/new")
	assert.Contains(t, got, `"pull_requests":"read"`, "the reference manifest itself, not a description of it")
	assert.Contains(t, got, "/webhooks/github/default/demo-reviewbot-gh")

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Len(t, entries, 1, "the reference is saved so it survives the run's scrollback")
	assert.Equal(t, "github-app-manifest-demo-reviewbot-gh.json", entries[0].Name())
	assert.Contains(t, got, entries[0].Name(), "and the guidance says where it went")
}

// TestGitHubWizard_FallbackGuidanceWritesNothingWithoutAWorkingDir: an empty
// WizardInput.WorkingDir means the client has nowhere to offer, which is a
// server rendering this wizard. The manifest is still in the guidance.
func TestGitHubWizard_FallbackGuidanceWritesNothingWithoutAWorkingDir(t *testing.T) {
	spec, err := (&wizard{}).Handoff(context.Background(), dataInput(t))
	require.NoError(t, err)
	require.NotNil(t, spec)

	got, err := spec.FallbackGuidance(map[string]string{
		keyOwnerType:              ownerTypeOrg,
		keyOrg:                    "demo-org",
		keyExternalBaseURL:        "https://ap.demo.test",
		wizardkeys.KeyChannelName: "demo-reviewbot-gh",
	})
	require.NoError(t, err)
	assert.Contains(t, got, `"pull_requests":"read"`)
	assert.NotContains(t, got, "A copy is saved at")
}

// TestGitHubWizard_FallbackGuidanceAfterASuccessfulHandoffNamesTheInstallURL:
// once the App exists its slug is an answer, so the one question left —
// installing it — can name the exact page instead of a pattern.
func TestGitHubWizard_FallbackGuidanceAfterASuccessfulHandoffNamesTheInstallURL(t *testing.T) {
	dir := t.TempDir()
	in := dataInput(t)
	in.WorkingDir = dir

	spec, err := (&wizard{}).Handoff(context.Background(), in)
	require.NoError(t, err)
	require.NotNil(t, spec)

	f := answerFixtures(t)
	got, err := spec.FallbackGuidance(map[string]string{
		keyOrg:                    "demo-org",
		keyExternalBaseURL:        "https://ap.demo.test",
		wizardkeys.KeyChannelName: "demo-reviewbot-gh",
		keyAppID:                  f[keyAppID],
		keySlug:                   "demo-org-demo-reviewbot-gh",
		keyPrivateKeyPEM:          f[keyPrivateKeyPEM],
		keyWebhookSecret:          f[keyWebhookSecret],
	})
	require.NoError(t, err)
	assert.Contains(t, got, "https://github.com/apps/demo-org-demo-reviewbot-gh/installations/new")
	assert.NotContains(t, got, `"pull_requests":"read"`,
		"the App already exists; a reference manifest for creating one by hand would be noise")

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Empty(t, entries, "nothing is created by hand on this path, so nothing is written")
}

// TestGitHubWizard_FallbackGuidanceNeverCarriesACredential: the guidance is a
// note rendered above the questions and it is composed from the answer map,
// which by then holds the App's private key and webhook secret.
func TestGitHubWizard_FallbackGuidanceNeverCarriesACredential(t *testing.T) {
	spec, err := (&wizard{}).Handoff(context.Background(), dataInput(t))
	require.NoError(t, err)
	require.NotNil(t, spec)

	f := answerFixtures(t)
	got, err := spec.FallbackGuidance(map[string]string{
		keyOrg:                    "demo-org",
		keyExternalBaseURL:        "https://ap.demo.test",
		wizardkeys.KeyChannelName: "demo-reviewbot-gh",
		keyAppID:                  f[keyAppID],
		keySlug:                   "demo-org-demo-reviewbot-gh",
		keyPrivateKeyPEM:          f[keyPrivateKeyPEM],
		keyWebhookSecret:          f[keyWebhookSecret],
	})
	require.NoError(t, err)
	assert.NotContains(t, got, f[keyPrivateKeyPEM])
	assert.NotContains(t, got, f[keyWebhookSecret])
}

// ---------------------------------------------------------------------------
// Resolve
// ---------------------------------------------------------------------------

// TestGitHubWizard_ResolveReadsThePrivateKeyFile is P5-R17: the manual route
// answers with a PATH (a pasted multi-line PEM arrives at a line-oriented
// prompt as its first line and nothing else), and reading that file is I/O, so
// it happens here rather than in Result.
func TestGitHubWizard_ResolveReadsThePrivateKeyFile(t *testing.T) {
	path := writePEM(t)
	got, err := (&wizard{}).Resolve(context.Background(), dataInput(t), map[string]string{
		keyPrivateKeyPath: path,
	})
	require.NoError(t, err)
	assert.Equal(t, "-----BEGIN RSA PRIVATE KEY-----\nZmFrZWZha2U=\n-----END RSA PRIVATE KEY-----\n",
		got[keyPrivateKeyPEM], "the file's contents are what Result needs")
}

func TestGitHubWizard_ResolveSurfacesAnUnreadablePrivateKey(t *testing.T) {
	cases := []struct {
		name    string
		write   func(t *testing.T) string
		wantErr string
	}{
		{
			name:    "no such file: named, not swallowed",
			write:   func(t *testing.T) string { return filepath.Join(t.TempDir(), "absent.pem") },
			wantErr: "could not read the private key",
		},
		{
			name: "not a PEM: refused before it reaches the Secret",
			write: func(t *testing.T) string {
				p := filepath.Join(t.TempDir(), "notes.txt")
				require.NoError(t, os.WriteFile(p, []byte("my app id is 12345"), 0o600))
				return p
			},
			wantErr: "does not look like a PEM private key",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := (&wizard{}).Resolve(context.Background(), dataInput(t), map[string]string{
				keyPrivateKeyPath: tc.write(t),
			})
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

// TestGitHubWizard_ResolveLeavesAnAlreadyAnsweredPEMAlone: the automated route
// returns the key itself, and re-reading a path that was never answered would
// fail a run that has everything it needs.
func TestGitHubWizard_ResolveLeavesAnAlreadyAnsweredPEMAlone(t *testing.T) {
	f := answerFixtures(t)
	got, err := (&wizard{}).Resolve(context.Background(), dataInput(t), map[string]string{
		keyPrivateKeyPEM: f[keyPrivateKeyPEM],
	})
	require.NoError(t, err)
	assert.Empty(t, got[keyPrivateKeyPEM], "nothing to derive when the key is already in hand")
}

// ---------------------------------------------------------------------------
// Result
// ---------------------------------------------------------------------------

// TestGitHubWizard_TheDeclaredQuestionsAnswerEverythingResultRequires is the
// completeness gate: an operator who answered exactly the
// questions this kind declares — the up-front batch plus the handoff's
// fallback, which is the route a headless host or a blocked port takes — must
// end with manifests, not with a fail-closed "was not answered".
//
// The answer map is built FROM the declared questions rather than from a
// hand-written list, so dropping a question (or the whole FallbackInputs set)
// reddens this rather than leaving a list that still names it.
func TestGitHubWizard_TheDeclaredQuestionsAnswerEverythingResultRequires(t *testing.T) {
	in := dataInput(t)
	w := &wizard{}

	inputs, err := w.Inputs(context.Background(), in)
	require.NoError(t, err)
	spec, err := w.Handoff(context.Background(), in)
	require.NoError(t, err)
	require.NotNil(t, spec, "the fallback questions live on the handoff")

	answers := answersFor(t, answerFixtures(t), inputs, spec.FallbackInputs)

	derived, err := w.Resolve(context.Background(), in, answers)
	require.NoError(t, err, "the path answer has to become the key Result reads")
	for k, v := range derived {
		answers[k] = v
	}

	out, err := w.Result(in, answers)
	require.NoError(t, err, "answering every declared question must be sufficient")
	require.NotNil(t, out.SecretManifest)
	require.NotNil(t, out.ChannelManifest)
	assert.Equal(t, "demo-manual", out.ChannelManifest.Spec.GitHub.AppSlug)
	assert.Contains(t, string(out.SecretManifest.Data["private-key"]), "BEGIN RSA PRIVATE KEY")
}

// TestGitHubWizard_ResultReadsNothingOffTheReceiver is P5-R12: the admin UI
// calls Result server-side, where the Inputs call may have landed on another
// instance entirely.
//
// primed has had Inputs and Handoff run against a DIFFERENT namespace before
// Result is called with "default". A Result that read anything either call
// left on the receiver would build the manifests in "stale", or would differ
// from the fresh wizard's — so the two are compared rather than each checked
// alone.
func TestGitHubWizard_ResultReadsNothingOffTheReceiver(t *testing.T) {
	answers := demoAnswers()

	primed := &wizard{}
	stale := seededInput(t, nil)
	stale.Namespace = "stale"
	stale.K8s = newFakeK8s(newAgentClass("demo-reviewbot", "stale")).Build()
	_, err := primed.Inputs(context.Background(), stale)
	require.NoError(t, err, "Inputs")
	_, err = primed.Handoff(context.Background(), stale)
	require.NoError(t, err, "Handoff")

	fromPrimed, err := primed.Result(channelkinds.WizardInput{Namespace: "default"}, answers)
	require.NoError(t, err)
	fromFresh, err := (&wizard{}).Result(channelkinds.WizardInput{Namespace: "default"}, answers)
	require.NoError(t, err)

	assert.Equal(t, fromFresh, fromPrimed,
		"Result must be a pure function of (in, answers) — not of state an earlier call left on the receiver")
	assert.Equal(t, "default", fromPrimed.ChannelManifest.Namespace)
	assert.Equal(t, "default", fromPrimed.SecretManifest.Namespace)

	// The other direction of the same rule, checked at the source rather than
	// through its effect: NOTHING was written to the receiver at all. Handoff
	// needs an HTTP client and used to default one by assigning it onto w,
	// which is harmless only because Kind.Wizard() hands out a fresh value per
	// call — a property of the caller, not of this type. The default now lives
	// in a local the closures capture.
	assert.Nil(t, primed.convert,
		"Handoff must build its default client into a local, not assign one onto the receiver")
}

func TestGitHubWizard_ResultRefusesAnEmptyNamespace(t *testing.T) {
	_, err := (&wizard{}).Result(channelkinds.WizardInput{}, demoAnswers())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "namespace")
}

func TestGitHubWizard_ResultFailsClosedOnAMissingAnswer(t *testing.T) {
	for key := range demoAnswers() {
		t.Run("missing "+key, func(t *testing.T) {
			answers := demoAnswers()
			delete(answers, key)
			_, err := (&wizard{}).Result(channelkinds.WizardInput{Namespace: "default"}, answers)
			require.Error(t, err)
			assert.Contains(t, err.Error(), key)
		})
	}
}

// TestGitHubWizard_ResultRefusesAMalformedAuthzSubject is github's half of the
// same gate bento carries (TestWizardResult_RefusesAMalformedAuthzSubject):
// authzsubject is the one answer whose SHAPE the kind must check itself.
//
// A channel wizard's questions carry no validation a client evaluates
// (channelkinds.ValidateInputs refuses a Question.Validation outright), so
// Result is the only gate — and it gates every route at once, since typed,
// seeded and handoff-derived answers all converge on requiredGithubAnswers. A
// subject that is not "service:<name>" takes the bound AgentClass to
// Valid=False, which also takes down its paired output Channel.
func TestGitHubWizard_ResultRefusesAMalformedAuthzSubject(t *testing.T) {
	for _, subject := range []string{"user:someone", "demo-reviewbot-github", "service:"} {
		t.Run(subject, func(t *testing.T) {
			answers := demoAnswers()
			answers[wizardkeys.KeyAuthzSubject] = subject

			_, err := (&wizard{}).Result(channelkinds.WizardInput{Namespace: "default"}, answers)
			require.Error(t, err)
			assert.Contains(t, err.Error(), wizardkeys.KeyAuthzSubject)
		})
	}
}

// TestGitHubWizard_SummaryRecordsWhatTheRunDecided pins what the operator can
// read back afterwards. A kind runs no code of its own while the questions are
// being answered, so every line has to be stated by Result or it is never
// written at all.
func TestGitHubWizard_SummaryRecordsWhatTheRunDecided(t *testing.T) {
	answers := demoAnswers()
	answers[keyExternalBaseURL] = "https://ap.demo.test"

	out, err := (&wizard{}).Result(channelkinds.WizardInput{Namespace: "default"}, answers)
	require.NoError(t, err)

	assert.Equal(t, []channelkinds.SummaryNote{
		{Label: "AgentClass", Value: "demo-reviewbot"},
		{Label: "GitHub owner", Value: "demo-org (organization)"},
		{Label: "External base URL", Value: "https://ap.demo.test"},
		{Label: "Channel", Value: "demo-reviewbot-gh"},
		{Label: "Subject", Value: "service:demo-reviewbot-github"},
		{Label: "GitHub App", Value: "demo-reviewbot (id 12345)"},
		{Label: "Installation", Value: "67890"},
	}, out.Summary)
}

// TestGitHubWizard_SummaryNeverPutsACredentialInScrollback is the security
// gate on the summary (P5-R15). A SummaryNote.Value reaches plain
// scrollback verbatim — the client filters nothing, and the summary is
// the block deliberately left behind after the alt-screen is released, so it
// outlives the run in the operator's terminal history.
//
// This kind's App private key mints installation tokens indefinitely, which
// makes it the highest-value secret in the whole feature; the webhook secret
// is the HMAC key every delivery is verified against. Both are in the answer
// map Result reads, and nothing downstream would catch a line carrying one.
func TestGitHubWizard_SummaryNeverPutsACredentialInScrollback(t *testing.T) {
	answers := demoAnswers()
	answers[keyExternalBaseURL] = "https://ap.demo.test"

	out, err := (&wizard{}).Result(channelkinds.WizardInput{Namespace: "default"}, answers)
	require.NoError(t, err)

	rendered := renderedSummaryBlock(t, out.Summary)
	require.NotEmpty(t, rendered)
	for _, secret := range []string{answers[keyPrivateKeyPEM], answers[keyWebhookSecret]} {
		assert.NotContains(t, rendered, secret,
			"a credential reached plain scrollback, where it outlives the run")
	}
	// The Notes block is the other thing the CLI prints verbatim.
	for _, n := range out.Notes {
		assert.NotContains(t, n, answers[keyPrivateKeyPEM])
		assert.NotContains(t, n, answers[keyWebhookSecret])
	}
}

// TestGitHubWizard_ResultIsAPureFunctionOfItsArguments: an SSA re-apply of the
// same answers must be byte-identical.
func TestGitHubWizard_ResultIsAPureFunctionOfItsArguments(t *testing.T) {
	in := channelkinds.WizardInput{Namespace: "default"}
	first, err := (&wizard{}).Result(in, demoAnswers())
	require.NoError(t, err)
	second, err := (&wizard{}).Result(in, demoAnswers())
	require.NoError(t, err)
	assert.Equal(t, first, second)
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func questionNamed(t *testing.T, qs []oap.Question, name string) oap.Question {
	t.Helper()
	for _, q := range qs {
		if q.Name == name {
			return q
		}
	}
	t.Fatalf("no question named %q in %v", name, questionNames(qs))
	return oap.Question{}
}

func questionNames(qs []oap.Question) []string {
	out := make([]string, 0, len(qs))
	for _, q := range qs {
		out = append(out, q.Name)
	}
	return out
}

func defaultOf(t *testing.T, qs []oap.Question, name string) string {
	t.Helper()
	v, ok := questionNamed(t, qs, name).Default.(string)
	require.Truef(t, ok, "question %q has no string default", name)
	return v
}

// renderedSummaryBlock is every label and value this run's summary carries,
// joined — a stand-in for the block the client leaves behind in plain
// scrollback, and what a leaked credential would outlive the run in.
//
// Joined here rather than run through the client's real renderer, because that
// renderer lives in pkg/cli/tui and NOTHING under channelkinds may reach for a
// terminal package, tests included (see channelkinds.Wizard). The
// substitution is safe for the claim being made: the client is documented to
// filter nothing (channelkinds.SummaryNote), so what reaches the terminal is
// exactly these values — and "the client must not filter" is pkg/cli/tui's
// property to hold, not this kind's.
func renderedSummaryBlock(t *testing.T, notes []channelkinds.SummaryNote) string {
	t.Helper()
	var b strings.Builder
	for _, n := range notes {
		b.WriteString(n.Label)
		b.WriteString("  ")
		b.WriteString(n.Value)
		b.WriteString("\n")
	}
	return b.String()
}

// ---------------------------------------------------------------------------
// Result — the provenance marker
// ---------------------------------------------------------------------------

// provenanceBaseAnswers is everything a github run answers that is NOT one of
// the four App credentials. Both routes below start from it, so the only
// thing that differs between them is HOW the App came to be — which is
// exactly what the marker records.
func provenanceBaseAnswers(t *testing.T) map[string]string {
	t.Helper()
	f := answerFixtures(t)
	return map[string]string{
		keyAgentClass:              f[keyAgentClass],
		keyOwnerType:               f[keyOwnerType],
		keyOrg:                     f[keyOrg],
		keyExternalBaseURL:         f[keyExternalBaseURL],
		wizardkeys.KeyChannelName:  f[wizardkeys.KeyChannelName],
		wizardkeys.KeyAuthzSubject: f[wizardkeys.KeyAuthzSubject],
		keyInstallationID:          f[keyInstallationID],
	}
}

// resultFromHandoffComplete is the automated route: this run POSTed the
// manifest, GitHub created the App, and the exchange returned its
// credentials.
//
// The answers are merged from what Complete ACTUALLY returned rather than
// hand-written here. Hand-writing the marker would leave Complete free never
// to set it, with the assertions below still green.
func resultFromHandoffComplete(t *testing.T) channelkinds.WizardOutput {
	t.Helper()
	in := dataInput(t)
	w := &wizard{convert: &fakeConverter{conv: &appprovision.Conversion{
		AppID:         "55555",
		Slug:          "demo-handoff",
		PEM:           sensitiveValue("-----BEGIN RSA PRIVATE KEY-----\nZTJlZmFrZQ==\n-----END RSA PRIVATE KEY-----\n"),
		WebhookSecret: sensitiveValue("whsec_e2efaketestsecret"),
	}}}

	spec, err := w.Handoff(context.Background(), in)
	require.NoError(t, err)
	require.NotNil(t, spec)

	answers := provenanceBaseAnswers(t)
	fromExchange, err := spec.Complete(context.Background(), callbackValues("fake-manifest-code-abc"))
	require.NoError(t, err, "the exchange leg is what produces the marker")
	maps.Copy(answers, fromExchange)

	out, err := w.Result(in, answers)
	require.NoError(t, err)
	require.NotNil(t, out.ChannelManifest)
	return out
}

// resultFromPastedCredentials is the manual route: the operator registered the
// App on GitHub themselves and pasted its details in. The same four
// credential answers arrive, by hand.
func resultFromPastedCredentials(t *testing.T) channelkinds.WizardOutput {
	t.Helper()
	f := answerFixtures(t)
	answers := provenanceBaseAnswers(t)
	for _, k := range []string{keyAppID, keySlug, keyPrivateKeyPEM, keyWebhookSecret} {
		answers[k] = f[k]
	}

	out, err := (&wizard{}).Result(dataInput(t), answers)
	require.NoError(t, err)
	require.NotNil(t, out.ChannelManifest)
	return out
}

// allDeclaredQuestions is every question a caller could address with
// --answer, DERIVED from what the wizard declares rather than transcribed: a
// literal list would silently stop covering a question added later.
//
// All three declaring sites are walked — the up-front batch, the handoff's
// FallbackInputs, and the seeded route, where Inputs folds the credential
// questions in instead of handing off — so a question added to any one of
// them is covered.
func allDeclaredQuestions(t *testing.T) []oap.Question {
	t.Helper()
	w := &wizard{}
	in := dataInput(t)

	qs, err := w.Inputs(context.Background(), in)
	require.NoError(t, err)

	spec, err := w.Handoff(context.Background(), in)
	require.NoError(t, err)
	require.NotNil(t, spec)
	qs = append(qs, spec.FallbackInputs...)

	seeded, err := w.Inputs(context.Background(), seededInput(t, answerFixtures(t)))
	require.NoError(t, err)
	qs = append(qs, seeded...)

	require.NotEmpty(t, qs, "a gate over an empty question set proves nothing")
	return qs
}

func TestResult_StampsProvenanceOnlyWhenThisRunCreatedTheApp(t *testing.T) {
	// The handoff route: this run registered the App, so Complete set the marker.
	out := resultFromHandoffComplete(t)
	assert.Equal(t, "oap",
		out.ChannelManifest.Annotations[channelkinds.AnnotationAppProvisionedBy],
		"an App this run created may later be repointed; the marker is how the controller knows")

	// The manual route: the operator created the App by hand and pasted its
	// details. Same four credential answers, no marker.
	manual := resultFromPastedCredentials(t)
	assert.NotContains(t, manual.ChannelManifest.Annotations, channelkinds.AnnotationAppProvisionedBy,
		"an App we did not create is someone else's resource; it only ever gets a drift finding")
}

// The marker is what authorizes the channel controller to PATCH a third
// party's App. If it were a declared question, `--answer` would let a caller
// claim this tool provisioned an App it did not, and the controller would
// repoint a webhook this cluster does not own.
func TestResult_TheProvenanceMarkerIsNotAnAnswerACallerCanSupply(t *testing.T) {
	for _, q := range allDeclaredQuestions(t) {
		assert.NotEqual(t, keyProvisionedByOAP, q.Name,
			"the marker must not be reachable through --answer")
	}
}

func TestResult_ProvenanceIsAPureFunctionOfItsInputs(t *testing.T) {
	// Pinned as an exact set, not as two runs compared to each other: a
	// second-granularity timestamp would compare equal within one test and
	// the control would pass while the annotation was volatile.
	assert.Equal(t, map[string]string{channelkinds.AnnotationAppProvisionedBy: "oap"},
		resultFromHandoffComplete(t).ChannelManifest.Annotations,
		"an applied field must be byte-identical across runs or SSA stops being a no-op")
}
