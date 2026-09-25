// What `oap channel create --kind github` ASKS: Inputs / Handoff / Resolve /
// Result (channelkinds.Wizard). The manifests those produce, and the
// operator-facing text they hand over, are in wizard.go.
//
// This is the only kind with a browser handoff, and the shape of the whole
// flow follows from one fact about GitHub: creating an App from a manifest
// redirects back with a one-time code, and INSTALLING that App does not —
// GitHub only redirects an installation to a caller when the App declares a
// Setup URL, and this App's manifest deliberately declares none (see
// appprovision/manifest.go). So the round trip answers four of the five things
// a github Channel needs and the fifth, the installation ID, is asked for
// afterwards on every route. That is why it lives in the handoff's
// FallbackInputs rather than in the up-front batch: asked up front it would be
// put to an operator before the App it identifies exists.
package github

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/github/appprovision"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/wizardkeys"
	"github.com/authzed/openagentprimitives/pkg/platform/oap"
)

// The prompt text this kind's questions declare, as named constants rather
// than literals so a test asserting what an operator is asked compares
// against one string per question rather than two copies that can drift.
const (
	agentClassPrompt = "Bind this reviewbot Channel to AgentClass"

	// appSource is asked SECOND, right after the AgentClass and before every
	// question whose answer depends on whether the App exists yet.
	//
	// IT IS AN AUTHORITY QUESTION, and the labels say so because getting that
	// across is the whole point. An App this run registers is one the channel
	// controller may later repoint the webhook of; an App the operator brings
	// is not, and a run where "I already have one" quietly meant "and you may
	// now rewrite its settings" would be the worst outcome this route could
	// have. So the choice states which of the two it is, in the place the
	// choice is made, rather than leaving it to be inferred from a marker
	// nobody sees.
	appSourcePrompt      = "Does a GitHub App for this already exist"
	appSourceDescription = "An App you bring stays yours: this run records its details and " +
		"never changes its settings. An App created here is registered by this run, which is " +
		"what later lets the Channel correct its webhook URL if this cluster's address changes."

	// The LABELS are what an operator reads; the VALUES are what a scripted
	// `--answer app-source=…` writes.
	appSourceCreateLabel   = "No — create one now, and this run registers it under that owner"
	appSourceExistingLabel = "Yes — I already have one, and I will give you its details"

	// ownerType is asked BEFORE the owner it describes, and before everything
	// derived from the two, because GitHub puts an organization's App pages
	// and a personal account's on DIFFERENT paths and there is no address
	// that serves both. Getting it wrong is not a cosmetic error: the
	// manifest POST lands on a 404 and the operator is left staring at a
	// blank App form with none of the permissions, events or URLs this run
	// meant to fill in.
	//
	// It is ASKED rather than probed. An operator always knows which kind of
	// account they own; asking works with no network at all, so an
	// `oap agent install --apply` never acquires a dependency on reaching
	// api.github.com; and a Begin failure ends the run by design here
	// (handoffSetupError), so a probe would turn a transient GitHub outage
	// into a dead run.
	ownerTypePrompt      = "Who owns the GitHub App"
	ownerTypeDescription = "GitHub registers an App under a different settings page for each, " +
		"and this run opens the one you pick."

	// The LABELS are what an operator reads; the VALUES are what a scripted
	// `--answer owner-type=…` writes, so they are short, stable and
	// unmistakable in either role.
	ownerTypeOrgLabel  = "An organization — the App is registered under the organization's settings"
	ownerTypeUserLabel = "A personal account — the App is registered under your own account settings"

	ownerPrompt      = "GitHub organization or user login"
	ownerDescription = `The account this App is registered under, e.g. "demo-org" — ` +
		`whichever kind of owner the question above named.`

	externalBaseURLPrompt      = "External base URL"
	externalBaseURLDescription = `Where GitHub can reach this cluster, e.g. "https://ap.example.com" — ` +
		`becomes the App's homepage and the prefix of its webhook URL.`

	// The Channel is a Kubernetes object, not a GitHub repository; say so
	// where the question is asked. Carried as a Description, which
	// the renderer shows as a note title above the field — visible under
	// the line-oriented driver too, where a huh Description is not.
	channelNameDescription = "This names a Channel object in the cluster — not the GitHub App and not a repository."

	authzSubjectDescription = "Required: a pull request's author has no AP identity, so there is no per-user attribution to derive one from."

	appIDPrompt      = "App ID"
	appIDDescription = `Shown under "About" on the App's settings page.`

	slugPrompt      = "App slug"
	slugDescription = "The URL-safe name in the App's settings URL: github.com/settings/apps/<slug>."

	privateKeyPathPrompt      = "Path to the App's downloaded private key (.pem)"
	privateKeyPathDescription = `"Generate a private key" on the App's settings page downloads this file.`

	// privateKeyPrompt names the question a run that SEEDED the key already
	// answered. It is never rendered — a seeded question is dropped before a
	// screen is built for it — and exists so that `--answer private-key=<pem>`,
	// the spelling the unattended route has always used, is a key this kind
	// declares rather than one its client refuses.
	privateKeyPrompt = "App private key (PEM)"

	webhookSecretPrompt      = "Webhook secret"
	webhookSecretDescription = `The value you set in the App's "Webhook secret" field — choose one if you have not yet.`

	installationIDPrompt      = "Installation ID"
	installationIDDescription = "Shown in the URL after you click \"Configure\" on the installation, " +
		"which ends in /installations/<id>."
)

// Inputs states what this kind needs before the App exists, as data.
//
// It is one batch, asked before any of it is answered, which is what the two
// branches below are decided from instead — both reading in.Seeded, i.e. what
// the flags settled before the run started:
//
//   - Whether the App's credentials are already in hand. When they are there
//     is no handoff (see Handoff), so the questions the handoff would have
//     carried are declared HERE. Every one of them is seeded by definition, so
//     none is rendered; declaring them is what makes their flags accepted
//     (a client builds its allowed --answer set from the declared questions)
//     and what carries their values into the map Result reads.
//   - Whether a step that needs a human can succeed at all. See
//     unattendedRefusal.
func (w *wizard) Inputs(ctx context.Context, in channelkinds.WizardInput) ([]oap.Question, error) {
	if in.Namespace == "" {
		return nil, errors.New("namespace is required")
	}
	if in.NonInteractive {
		if err := unattendedRefusal(in); err != nil {
			return nil, err
		}
	}

	classQ, boundClass, unambiguous, err := wizardkeys.AgentClassQuestion(ctx, in, agentClassPrompt)
	if err != nil {
		return nil, err
	}

	// P5-R16: a WRONG default is worse than NO default, because a wrong one
	// can be silently accepted (a blank answer falls back to it) while an
	// omitted one cannot (a required question with no Default fails closed).
	// Inputs runs before any answer exists, and questionscreen bakes a Default
	// into a closure over this one call's value — it never re-consults an
	// answer a later question records — so a default derived from a GUESS at
	// the AgentClass would stand even after the operator picked another.
	//
	// Naming the wrong AgentClass here is not cosmetic: the Channel's
	// credentials Secret is named after the Channel, and the authzSubject is
	// what every session this Channel starts is attributed to.
	var nameDefault, subjectDefault any
	if unambiguous {
		nameDefault = defaultChannelName(boundClass)
		subjectDefault = defaultSubjectFor(boundClass)
	}

	qs := []oap.Question{classQ}

	// ASKED ONLY WHEN IT IS STILL AN OPEN QUESTION. A run whose flags already
	// carry the App's credentials has demonstrated the answer — it has the
	// App — so putting the choice to it would be asking a question whose
	// answer changes nothing, and REQUIRING it would refuse every scripted run
	// that worked before this question existed. Not declaring it here is also
	// what makes `--answer app-source=…` beside those flags a refused key
	// rather than a second, contradictable statement of the same fact.
	if !appCredsSeeded(in) {
		qs = append(qs, oap.Question{
			Name:        keyAppSource,
			Type:        oap.QEnum,
			Prompt:      appSourcePrompt,
			Description: appSourceDescription,
			Enum:        []string{appSourceCreate, appSourceExisting},
			EnumLabels:  []string{appSourceCreateLabel, appSourceExistingLabel},
			// NO DEFAULT, for the same P5-R16 reason the owner type below
			// carries, and here the cost of a wrong one is externally visible:
			// a bare Enter accepting "create" would send an operator who
			// already has an App to register a SECOND one on github.com, under
			// a name close enough to the first to be confusing, which nothing
			// on this side can take back. A required question with no Default
			// fails closed instead.
		})
	}

	qs = append(qs, []oap.Question{
		// NO DEFAULT, deliberately, and for P5-R16's reason one paragraph up:
		// a default here would be a GUESS at which kind of GitHub account the
		// operator owns, silently accepted with a bare Enter, and the whole
		// cost of guessing it wrong is a browser landing on a page that
		// cannot create this App.
		{
			Name:        keyOwnerType,
			Type:        oap.QEnum,
			Prompt:      ownerTypePrompt,
			Description: ownerTypeDescription,
			Enum:        []string{ownerTypeOrg, ownerTypeUser},
			EnumLabels:  []string{ownerTypeOrgLabel, ownerTypeUserLabel},
		},
		{Name: keyOrg, Type: oap.QString, Prompt: ownerPrompt, Description: ownerDescription},
		{Name: keyExternalBaseURL, Type: oap.QString, Prompt: externalBaseURLPrompt, Description: externalBaseURLDescription},
		{
			Name:        wizardkeys.KeyChannelName,
			Type:        oap.QString,
			Prompt:      wizardkeys.ChannelNamePrompt,
			Description: channelNameDescription,
			Default:     nameDefault,
		},
		{
			Name:        wizardkeys.KeyAuthzSubject,
			Type:        oap.QString,
			Prompt:      wizardkeys.AuthzSubjectPrompt,
			Description: authzSubjectDescription,
			Default:     subjectDefault,
		},
	}...)
	if appCredsSeeded(in) {
		qs = append(append(qs, appCredentialQuestions(in)...), installationIDQuestion())
	}
	return qs, nil
}

// Handoff is the App-manifest browser round trip: POST a manifest to GitHub's
// App-creation endpoint for whichever kind of owner the run named, let the
// operator confirm it on GitHub's own page, and exchange the code the redirect
// carries for the App's identity and secrets.
//
// nil when every credential is already seeded: a run that has the App must
// not be sent to create a second one. Inputs declares those questions in
// that case, so nothing about the run's seedable set changes with the
// branch.
func (w *wizard) Handoff(_ context.Context, in channelkinds.WizardInput) (*channelkinds.HandoffSpec, error) {
	if in.Namespace == "" {
		return nil, errors.New("namespace is required")
	}
	if appCredsSeeded(in) {
		return nil, nil
	}
	// Captured rather than read off the receiver later: Begin, Complete and
	// FallbackGuidance are called by the client, at a point where the only
	// WizardInput in play is the one this call was given.
	//
	// The default client is built into the LOCAL, never assigned back onto w.
	// Harmless here today — Kind.Wizard() returns a fresh value per call, so
	// nothing else would ever read it — but this contract's whole discipline is
	// that a method writes nothing to the receiver, and an exception that is
	// safe only because of how the one caller happens to behave is the kind
	// that stops being safe silently.
	namespace, workingDir, convert := in.Namespace, in.WorkingDir, w.convert
	if convert == nil {
		convert = appprovision.NewHTTPClient()
	}

	return &channelkinds.HandoffSpec{
		Begin: func(answers map[string]string, callbackURL string) (channelkinds.HandoffStart, error) {
			params, err := manifestParams(namespace, answers)
			if err != nil {
				return channelkinds.HandoffStart{}, err
			}
			// GitHub reads the redirect target out of the manifest body, not
			// off the query string — so the client's callback goes in here,
			// and a handoff that left it empty would create the App and never
			// come back.
			params.RedirectURL = callbackURL
			manifest, err := appprovision.BuildManifest(params)
			if err != nil {
				return channelkinds.HandoffStart{}, fmt.Errorf("build the github app manifest: %w", err)
			}
			owner := strings.TrimSpace(answers[keyOrg])
			return channelkinds.HandoffStart{
				// THE SIGN-IN LINE IS A PREREQUISITE, NOT A PLEASANTRY, and it
				// is stated before the browser opens because afterwards is too
				// late. GitHub answers a signed-out request to the
				// App-creation endpoint with a redirect to its login page, and
				// no browser replays a POST body across a login redirect — so
				// the manifest is dropped and GitHub renders an empty form
				// from the `return_to` GET that follows. That is the single
				// most likely way this flow degrades, and it is invisible from
				// here: the client's loopback listener never sees GitHub's
				// response, so this can only be forewarned, never detected.
				//
				// The blank-form sentence is what makes a DEGRADED flow
				// self-diagnosing. Everything this run knows travels in the
				// POSTed manifest, so a page that arrives blank is the one
				// symptom of the manifest not reaching GitHub — and without
				// being told what to expect, an operator reads a blank form as
				// the flow working and starts typing. It now names the usual
				// cause and the way out, which is re-opening the local address
				// the client prints directly below this text: that page
				// re-POSTs the manifest, whereas GitHub's own address is a
				// POST endpoint that a hand-typed GET always reaches empty.
				Explain: fmt.Sprintf("Opening GitHub to create the App %q under %s.\n\n"+
					"SIGN IN TO GITHUB FIRST, in the browser this is about to open. A signed-out "+
					"request is redirected to GitHub's login page, and a browser does not carry "+
					"this run's details across that redirect — you would arrive at an empty form.\n\n"+
					"The form arrives already filled in from this run — review it and confirm, and "+
					"this run picks up where it left off. A BLANK form means the manifest did not "+
					"reach GitHub, most often because you were signed out: sign in, then open the "+
					"local address below again to re-send it. Failing that, this run falls back to "+
					"asking for the App's details instead.", params.Name, owner),
				URL:        appCreateURL(answers[keyOwnerType], owner),
				FormFields: map[string]string{"manifest": manifest},
			}, nil
		},
		Complete: func(ctx context.Context, callback url.Values) (map[string]string, error) {
			code := strings.TrimSpace(callback.Get("code"))
			if code == "" {
				return nil, errors.New("github app manifest flow: the callback carried no exchange code")
			}
			conv, err := convert.Convert(ctx, code)
			if err != nil {
				return nil, fmt.Errorf("exchange the manifest-flow code: %w", err)
			}
			return map[string]string{
				keyAppID:         conv.AppID,
				keySlug:          conv.Slug,
				keyPrivateKeyPEM: string(conv.PEM.UnderlyingValue()),
				keyWebhookSecret: string(conv.WebhookSecret.UnderlyingValue()),
				// The fifth answer is not a credential: it is the one thing
				// that distinguishes this route from the manual one. Both
				// arrive at Result with the same four credentials, so without
				// a key written HERE — at the only point in the flow that
				// knows this run created the App — Result has nothing to tell
				// them apart by. See keyProvisionedByOAP.
				keyProvisionedByOAP: "true",
			}, nil
		},
		// THE ROUTE QUESTION'S TEETH. Handoff is described before a single
		// question is answered, so the branch above can only read what the
		// FLAGS settled; an operator who says at the prompt that they already
		// have an App is settling it one question later, and this is where
		// that answer stops the round trip.
		//
		// It is also the whole of why an App the operator brought cannot be
		// marked as ours: Complete is the only writer of keyProvisionedByOAP,
		// and a stood-down handoff never reaches Complete. The property is
		// structural, not a rule anybody has to remember at the stamping site.
		SkipWhen: func(answers map[string]string) string {
			if !bringsExistingApp(answers) {
				return ""
			}
			return "You already have a GitHub App, so this run has none to register — " +
				"it will record the App's details and never change its settings"
		},
		// Fire-and-forget, and only once the App exists: see installURLFor and
		// this package's doc on why GitHub never sends an installation back
		// here, which is what makes this an address to OPEN rather than a
		// second round trip to wait on.
		FallbackOpenURL: func(answers map[string]string) string {
			if !appCredsAnswered(answers) {
				return ""
			}
			return installURLFor(answers[keySlug])
		},
		FallbackInputs: append(appCredentialQuestions(in), installationIDQuestion()),
		// The manual route asks for a PATH and a completed exchange returns
		// the KEY, so without this the happy path asks the operator for a .pem
		// file that does not exist — and, in a scripted run, eats the answer
		// meant for the next question.
		SatisfiedBy: map[string]string{keyPrivateKeyPath: keyPrivateKeyPEM},
		FallbackGuidance: func(answers map[string]string) (string, error) {
			return fallbackSteps(namespace, workingDir, answers)
		},
	}, nil
}

// Resolve reads the private key file the manual route answered with.
//
// The manual route asks for a PATH, not the key: huh's line-oriented renderer
// reads exactly one line per field, so a pasted multi-line PEM would arrive as
// its first line and nothing else, and that renderer has no error channel with
// which to say so (see readPrivateKeyPEM). Reading the file is I/O, which is
// what this step is for (P5-R17) — Result stays a pure function of its
// arguments.
//
// Nothing to do on the automated route, where the exchange returned the key
// itself, or on a run that seeded it.
func (w *wizard) Resolve(_ context.Context, _ channelkinds.WizardInput, answers map[string]string) (map[string]string, error) {
	if strings.TrimSpace(answers[keyPrivateKeyPEM]) != "" {
		return nil, nil
	}
	path := strings.TrimSpace(answers[keyPrivateKeyPath])
	if path == "" {
		// Not this step's refusal to make: Result names every answer it is
		// missing, in one place, rather than each producer guessing which of
		// them the operator forgot.
		return nil, nil
	}
	pem, err := readPrivateKeyPEM(path)
	if err != nil {
		return nil, err
	}
	return map[string]string{keyPrivateKeyPEM: pem}, nil
}

// Result reads the answers and produces the manifests, as a pure function of
// (in, answers) — see channelkinds.Wizard.Result for why it may not read
// anything a prior Inputs call left on the receiver (P5-R12).
//
// It carries the run's decisions in Summary because this kind runs no code of
// its own while the questions are being answered
// (channelkinds.WizardOutput.Summary).
func (w *wizard) Result(in channelkinds.WizardInput, answers map[string]string) (channelkinds.WizardOutput, error) {
	if in.Namespace == "" {
		return channelkinds.WizardOutput{}, errors.New("namespace is required")
	}
	resolved, err := requiredGithubAnswers(func(key string) string { return answers[key] })
	if err != nil {
		return channelkinds.WizardOutput{}, err
	}
	// Checked for shape but not for presence, which is the difference between
	// this and the ten above: the manifests do not use the external base URL
	// — only App creation does — so a run that reached here without one is
	// legitimate and gets a summary one line shorter. A run that DID answer it
	// with something unusable is not, and this is the only place that says so
	// on the route with no handoff, where manifestParams never runs. A
	// scheme-less answer is the shape that matters: it becomes the App's
	// homepage and the prefix of its webhook URL, and GitHub silently never
	// delivers to one it cannot resolve.
	externalBaseURL := strings.TrimSpace(answers[keyExternalBaseURL])
	if externalBaseURL != "" {
		if err := validateExternalBaseURL(externalBaseURL); err != nil {
			return channelkinds.WizardOutput{}, fmt.Errorf("github wizard: %s: %w", keyExternalBaseURL, err)
		}
	}
	// FAIL CLOSED ON THE CONTRADICTION, before anything is built from either
	// half of it. "The operator brought this App" and "this run registered it"
	// cannot both be true, and the second is what authorizes an outward-facing
	// PATCH of a third party's App settings.
	//
	// Today they cannot both arrive: keyProvisionedByOAP is written only by
	// the handoff's Complete, and the handoff stands down on this route
	// (HandoffSpec.SkipWhen). But that is two functions agreeing, held by
	// nothing the compiler checks — so the one place the marker is stamped
	// refuses the pair outright rather than resolving it in the direction that
	// hands out authority.
	provisionedHere := strings.TrimSpace(answers[keyProvisionedByOAP]) != ""
	if provisionedHere && bringsExistingApp(answers) {
		return channelkinds.WizardOutput{}, fmt.Errorf(
			"github wizard: this run was told the App already exists (%s=%s), so it cannot also record having registered it",
			keyAppSource, appSourceExisting)
	}

	out, err := githubOutput(in.Namespace, resolved, provisionedHere)
	if err != nil {
		return channelkinds.WizardOutput{}, err
	}
	// Read off the RAW answers rather than the required set, because the
	// marker is not one of them: its absence is every route-but-one's correct
	// state, not an unanswered question, and requiredGithubAnswers would fail
	// a legitimate run closed over it.
	//
	// Nothing volatile joins it. The annotation is applied by the client, so
	// a re-run with the same answers has to produce a byte-identical object
	// or the apply stops being a no-op — a timestamp or a run id here would
	// churn field ownership on every install. See
	// channelkinds.AnnotationAppProvisionedBy.
	if provisionedHere {
		metav1.SetMetaDataAnnotation(&out.ChannelManifest.ObjectMeta,
			channelkinds.AnnotationAppProvisionedBy, channelkinds.AppProvisionedByOAP)
	}
	out.Summary = githubSummary(resolved, externalBaseURL)
	return out, nil
}

// githubSummary is what the run DECIDED, in the order the operator answered
// it: the AgentClass, the org, the external base URL, the Channel name, the
// subject, the App, the installation.
//
// NO CREDENTIAL APPEARS HERE, and that is the point of the function rather
// than a detail of it (P5-R15). A SummaryNote.Value reaches plain scrollback
// verbatim, the client filters nothing, and this block is deliberately
// left behind after the alt-screen is released — so it outlives the run in the
// operator's terminal history. This kind's App private key mints installation
// tokens indefinitely and its webhook secret is the HMAC key every delivery is
// verified against; both are in the answer map this function reads, and
// neither is recorded.
//
// externalBaseURL is passed separately because Result does not REQUIRE it —
// the manifests do not use it, only App creation does — so a run that reached
// Result without one still gets a summary, one line shorter.
//
// The App line has ONE spelling whichever route produced it: a pure function
// of the answers cannot tell an App the handoff created from one the operator
// created by hand, and inventing a distinction would make the summary claim
// something it does not know. Where the reference manifest was saved is not
// here either — that file is written from the handoff's guidance, which runs
// long before Result and only on the route that needs it.
func githubSummary(answers map[string]string, externalBaseURL string) []channelkinds.SummaryNote {
	notes := []channelkinds.SummaryNote{
		{Label: "AgentClass", Value: answers[keyAgentClass]},
		// The owner TYPE rides on the same line as the owner because it is a
		// decision this run made and every GitHub address it produced branches
		// on — an operator re-reading this in scrollback after landing on the
		// wrong page needs to see which of the two was chosen.
		{Label: "GitHub owner", Value: answers[keyOrg] + " (" + answers[keyOwnerType] + ")"},
	}
	if externalBaseURL != "" {
		notes = append(notes, channelkinds.SummaryNote{Label: "External base URL", Value: externalBaseURL})
	}
	return append(notes,
		channelkinds.SummaryNote{Label: "Channel", Value: answers[wizardkeys.KeyChannelName]},
		channelkinds.SummaryNote{Label: "Subject", Value: answers[wizardkeys.KeyAuthzSubject]},
		channelkinds.SummaryNote{Label: "GitHub App", Value: answers[keySlug] + " (id " + answers[keyAppID] + ")"},
		channelkinds.SummaryNote{Label: "Installation", Value: answers[keyInstallationID]},
	)
}

// --- the questions the handoff and a seeded run share ---

// appCredentialQuestions is the four values that identify the App and let this
// cluster act as it.
//
// The private key is spelled as a PATH unless the run already seeded the key
// itself, and both spellings exist because neither covers both cases: a path
// is the only form a line-oriented prompt can read (see readPrivateKeyPEM),
// while `--answer private-key=<pem>` is the spelling unattendedRefusal names,
// and a key this kind does not declare is refused at the flag.
func appCredentialQuestions(in channelkinds.WizardInput) []oap.Question {
	privateKey := oap.Question{
		Name:        keyPrivateKeyPath,
		Type:        oap.QString,
		Prompt:      privateKeyPathPrompt,
		Description: privateKeyPathDescription,
	}
	if wizardkeys.SeededHas(in, keyPrivateKeyPEM) {
		privateKey = oap.Question{Name: keyPrivateKeyPEM, Type: oap.QSecret, Prompt: privateKeyPrompt}
	}
	return []oap.Question{
		{Name: keyAppID, Type: oap.QString, Prompt: appIDPrompt, Description: appIDDescription},
		{Name: keySlug, Type: oap.QString, Prompt: slugPrompt, Description: slugDescription},
		privateKey,
		{Name: keyWebhookSecret, Type: oap.QSecret, Prompt: webhookSecretPrompt, Description: webhookSecretDescription},
	}
}

func installationIDQuestion() oap.Question {
	return oap.Question{
		Name:        keyInstallationID,
		Type:        oap.QString,
		Prompt:      installationIDPrompt,
		Description: installationIDDescription,
	}
}

// --- the handoff's answer-derived halves ---

// manifestParams is the App manifest's shape, read out of the answers the
// up-front batch collected.
//
// It fails closed on a missing one rather than building a manifest around an
// empty string: an empty owner produces the address
// https://github.com/organizations//settings/apps/new, and an empty Channel
// name produces a webhook URL resolving against no Channel — both of which
// look like a working handoff right up until the browser lands on a page that
// cannot do anything.
//
// It is also where the org and the base URL are checked for SHAPE before the
// handoff opens a browser, and every refusal here NAMES ITS FIELD. These two
// used to be validated in place by the terminal screen that asked for them,
// which re-prompted; a question set stated as data carries no validator any
// client evaluates (see channelkinds.ValidateInputs), so this is the only
// check there is on the handoff route, and the message is the whole of what
// the operator gets. Reaching Begin is what makes it early enough to matter:
// channelwizard reads a Begin failure as a setup failure and ends the run with
// this sentence, rather than detouring to a manual route whose own guidance is
// built from the same bad answer.
func manifestParams(namespace string, answers map[string]string) (appprovision.Params, error) {
	owner := strings.TrimSpace(answers[keyOrg])
	ownerType := strings.TrimSpace(answers[keyOwnerType])
	baseURL := strings.TrimSpace(answers[keyExternalBaseURL])
	channelName := strings.TrimSpace(answers[wizardkeys.KeyChannelName])
	for key, v := range map[string]string{
		keyOrg:                    owner,
		keyOwnerType:              ownerType,
		keyExternalBaseURL:        baseURL,
		wizardkeys.KeyChannelName: channelName,
	} {
		if v == "" {
			return appprovision.Params{}, fmt.Errorf("github app manifest: %q was not answered", key)
		}
	}
	// Checked HERE, in front of Begin's own appCreateURL call, because
	// ownerIsUser reads anything that is not "user" as an organization: a
	// typo'd `--answer owner-type=usr` would otherwise send a personal account
	// to the org address, which is the exact 404-into-a-blank-form this
	// question exists to prevent.
	if err := validateOwnerType(ownerType); err != nil {
		return appprovision.Params{}, fmt.Errorf("github app manifest: %s: %w", keyOwnerType, err)
	}
	if err := validateOwnerLogin(owner); err != nil {
		return appprovision.Params{}, fmt.Errorf("github app manifest: %s: %w", keyOrg, err)
	}
	if err := validateExternalBaseURL(baseURL); err != nil {
		return appprovision.Params{}, fmt.Errorf("github app manifest: %s: %w", keyExternalBaseURL, err)
	}
	return appprovision.Params{
		Name:            appDisplayName(owner, channelName),
		ExternalBaseURL: baseURL,
		Namespace:       namespace,
		ChannelName:     channelName,
	}, nil
}

// fallbackSteps is what the operator reads above whichever fallback questions
// are left to answer, which is one of three situations.
//
// The App DOES exist, because the handoff just created it: all that is left is
// installing it, so they get installationSteps, naming the exact page from
// the slug the exchange returned.
//
// The operator BROUGHT an App: nothing is to be created, and what they need is
// where to read its details off and what this Channel requires the App to be
// configured as — plus, above all, what this cluster will and will not do to
// it. See existingAppSteps.
//
// The App does not exist: the handoff could not run, or GitHub refused it, and
// the operator is about to create the App by hand through GitHub's own form.
// They get appManualSteps and the reference manifest — the webhook URL, the
// permissions, the events — which nobody can guess, plus a copy on disk,
// because this text is the whole product of that detour and the terminal it
// is printed into scrolls away.
func fallbackSteps(namespace, workingDir string, answers map[string]string) (string, error) {
	if appCredsAnswered(answers) {
		return installationSteps(installURLFor(answers[keySlug])), nil
	}

	// The reference manifest is what BOTH remaining routes are built around,
	// and for the same reason: it states the webhook URL, the permissions and
	// the events, none of which an operator can guess. The route only decides
	// whether they are creating an App to match it or checking one they
	// already have against it.
	// The reference manifest is built without a RedirectURL: neither remaining
	// route has a code to redirect back with — one creates the App through
	// GitHub's own form, the other is not creating one at all. It is shown
	// only as a reference for what the App has to be.
	params, err := manifestParams(namespace, answers)
	if err != nil {
		return "", err
	}
	manifestJSON, err := appprovision.BuildManifest(params)
	if err != nil {
		return "", fmt.Errorf("render the App reference manifest: %w", err)
	}
	saved := savedManifest{}
	if workingDir != "" {
		saved = saveManifest(workingDir, params.ChannelName, manifestJSON)
	}

	// Both addresses below are built only AFTER manifestParams returned, which
	// is what validated the owner type they branch on — see ownerIsUser on why
	// nothing may read an unvalidated one.
	if bringsExistingApp(answers) {
		return existingAppSteps(
			appSettingsListURL(answers[keyOwnerType], strings.TrimSpace(answers[keyOrg])),
			appprovision.WebhookURL(params),
			manifestJSON, saved), nil
	}

	// Built only AFTER manifestParams returned, which is what validated the
	// owner type this address branches on — see ownerIsUser on why nothing may
	// read an unvalidated one.
	createURL := appCreateURL(answers[keyOwnerType], strings.TrimSpace(answers[keyOrg]))
	return appManualSteps(createURL, manifestJSON, saved) + "\n\n" +
		installationSteps(installURLFor("<slug>")), nil
}

// appCreateURL is GitHub's "new App" page — the manifest flow POSTs to it, and
// the manual route opens the same address by hand.
//
// THERE ARE TWO ADDRESSES AND NEITHER SERVES BOTH OWNERS. An organization's is
// scoped by its login; a personal account's is the account's own settings and
// takes no login at all, because GitHub already knows who is signed in. Posting
// the manifest to the org address for a personal account 404s, and a 404 does
// not fail the way a rejected manifest does: the operator's browser ends up at
// a bare, unprefilled App form, which looks like the flow working and is not.
func appCreateURL(ownerType, owner string) string {
	if ownerIsUser(ownerType) {
		return "https://github.com/settings/apps/new"
	}
	return "https://github.com/organizations/" + url.PathEscape(owner) + "/settings/apps/new"
}

// appSettingsListURL is where an owner's own GitHub Apps are listed — the page
// an operator who BROUGHT an App reads its ID, slug and webhook settings off,
// and generates a private key on.
//
// Same two-address rule as appCreateURL, which it is the parent of: an
// organization's Apps live under the organization, a personal account's under
// the account's own settings with no login in the path at all. Sending one to
// the other's address is the same 404 the owner-type question exists to
// prevent.
func appSettingsListURL(ownerType, owner string) string {
	if ownerIsUser(ownerType) {
		return "https://github.com/settings/apps"
	}
	return "https://github.com/organizations/" + url.PathEscape(owner) + "/settings/apps"
}

// installationsURL is where an owner's existing App installations are listed —
// the page the post-setup note points at, so the operator can confirm the
// installation reaches every repository this Channel is meant to review. Same
// two-address rule as appCreateURL.
func installationsURL(ownerType, owner string) string {
	if ownerIsUser(ownerType) {
		return "https://github.com/settings/installations"
	}
	return "https://github.com/organizations/" + url.PathEscape(owner) + "/settings/installations"
}

// ownerIsUser reads the answered owner type.
//
// It is deliberately NOT the place a bad answer is caught: an unrecognized
// value must be refused by name, which validateOwnerType does at each of the
// two disjoint routes' entry points (manifestParams, requiredGithubAnswers),
// the same way the owner login and the base URL are. Treating "anything that
// is not `user`" as an organization HERE is safe only because nothing reaches
// here unvalidated — and reads as the org-shaped assumption this whole change
// removes if it ever stops being true. Hence the ordering rule stated on both
// validators.
func ownerIsUser(ownerType string) bool { return strings.TrimSpace(ownerType) == ownerTypeUser }

// installURLFor is where an App is installed onto its owner — one address for
// both owner kinds, because it is scoped by the App's slug rather than by who
// owns it. A slug of "<slug>" is the placeholder the manual route uses: on that
// route the App does not exist yet, so the operator substitutes the name they
// are about to choose.
func installURLFor(slug string) string {
	slug = strings.TrimSpace(slug)
	if slug == "" || slug == "<slug>" {
		return "https://github.com/apps/<slug>/installations/new"
	}
	return "https://github.com/apps/" + url.PathEscape(slug) + "/installations/new"
}

// --- reading what the flags already answered ---

// appCredsSeeded reports that this run already has the App, so there is
// nothing to create — read from the flags, before anything is asked, because
// that is when Handoff has to decide whether to send the operator anywhere.
//
// Either spelling of the private key counts. A path is resolved to the key
// itself by Resolve, so a run that seeded one is no less complete than a run
// that seeded the other — and treating it as incomplete would send an operator
// who already has the App back to GitHub to create a second one.
func appCredsSeeded(in channelkinds.WizardInput) bool {
	return wizardkeys.SeededHas(in, keyAppID) && wizardkeys.SeededHas(in, keySlug) && wizardkeys.SeededHas(in, keyWebhookSecret) &&
		(wizardkeys.SeededHas(in, keyPrivateKeyPEM) || wizardkeys.SeededHas(in, keyPrivateKeyPath))
}

// bringsExistingApp reports that the operator said they already have the App,
// read from the ANSWERS rather than the flags: this is the route question
// settled at the prompt, which is the whole case appCredsSeeded cannot cover.
//
// IT IS NEVER A CLAIM OF PROVENANCE, in either direction. It says only which
// questions to ask and whether to make the round trip; what authorizes an
// outward-facing write is keyProvisionedByOAP, which the exchange alone
// writes. Reading this as "so the App is not ours" would be correct today and
// would put the security property in the hands of a route answer a caller can
// supply — see Result, which refuses the contradiction instead.
func bringsExistingApp(answers map[string]string) bool {
	return strings.TrimSpace(answers[keyAppSource]) == appSourceExisting
}

// appCredsAnswered is the same question asked of a run's ANSWERS, which is
// what the handoff's guidance reads: by then the exchange may have supplied
// what no flag did.
func appCredsAnswered(answers map[string]string) bool {
	for _, key := range []string{keyAppID, keySlug, keyWebhookSecret} {
		if strings.TrimSpace(answers[key]) == "" {
			return false
		}
	}
	return strings.TrimSpace(answers[keyPrivateKeyPEM]) != "" || strings.TrimSpace(answers[keyPrivateKeyPath]) != ""
}

// unattendedRefusal is why this kind cannot finish a run nobody is watching,
// in its own words, raised from Inputs — before anything is asked (P5-R19).
//
// It names the flags that WOULD answer the step, which the client's generic
// "this screen has no answer, supply it via flags" cannot: there is no flag
// for "confirm this App in a browser", and a message implying there is sends
// the operator hunting for one.
//
// Both steps are checked, in the order they happen, because they are
// different steps: a run
// that seeded every credential has already created the App and still has to
// install it.
func unattendedRefusal(in channelkinds.WizardInput) error {
	if !appCredsSeeded(in) {
		// THE ROUTE DECIDES WHICH INSTRUCTION IS THE RIGHT ONE. Both refusals
		// name the same four flags, because those are what the run is missing
		// either way — but a caller that has already told us the App exists
		// must not be sent off to create one. Being handed the wrong remedy is
		// worse than a generic message: it is a confident instruction to do
		// the thing they said they had already done.
		credFlags := "--answer " + keyAppID + "=<id> --answer " + keySlug + "=<slug> " +
			"--answer " + keyPrivateKeyPEM + "=<pem contents> --answer " + keyWebhookSecret + "=<secret>"
		if wizardkeys.SeededAnswer(in, keyAppSource) == appSourceExisting {
			return errors.New("this run says the GitHub App already exists, so supply its details: " +
				credFlags + ", or drop --non-interactive to be asked for them")
		}
		return errors.New("creating a GitHub App needs a human to confirm it in a browser on GitHub's own page; " +
			"either create the App yourself and supply " + credFlags + ", or drop --non-interactive")
	}
	if !wizardkeys.SeededHas(in, keyInstallationID) {
		return errors.New("installing the GitHub App onto its owner needs a human to click through GitHub's install flow; " +
			"either install it yourself and supply --answer " + keyInstallationID + "=<id>, or drop --non-interactive")
	}
	return nil
}

// defaultChannelName and defaultSubjectFor are the suggestions both contracts
// offer, derived from the bound AgentClass.
//
// The subject derives from the AgentClass rather than from the Channel name
// because it names WHO is acting (the reviewbot), not WHERE the events
// arrived: one per AgentClass, so two reviewbots in a namespace do not share a
// started_by.
func defaultChannelName(agentClass string) string { return "github-" + agentClass }

func defaultSubjectFor(agentClass string) string { return "service:" + agentClass + "-github" }
