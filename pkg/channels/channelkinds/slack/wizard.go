// pkg/channels/channelkinds/slack/wizard.go
//
// The answer vocabulary, the manifests and the operator-facing text for
// `oap channel create --kind slack`. What the flow ASKS — Inputs, Resolve and
// Result — is in wizard_data.go (the agent flow) and wizard_monitoring.go
// (the monitoring flow); this file is what those call.
//
// agentOutput emits the Secret + Channel manifests, plus the AgentClass
// capability patch, for the CLI dispatcher to apply.
package slack

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	slackapi "github.com/slack-go/slack"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/slack/appprovision"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/wizardkeys"
)

// State keys this wizard answers. Stable strings: a caller seeding State from
// flags for a non-interactive run addresses screens by these.
const (
	// keyAgentClass is the shared binding key, aliased so this package's own
	// reads stay short. The STRING is wizardkeys' — see KeyChannelName's doc
	// for why a kind must never retype one of these.
	keyAgentClass = wizardkeys.KeyAgentClass

	keyHasSlackApp = "slackapp"
	keyBotToken    = "bot-token"
	keyAppToken    = "app-token"
	// keyDestinationChannelID is the Slack channel ID a Channel that cannot be
	// told where to post by an inbound message posts into. BOTH flows ask for
	// it under this one key — the monitoring flow always (nothing on a
	// monitoring channel listens), the agent flow only for a role=output
	// Channel (see outputDestinationNeeded) — and it lands on the same field,
	// spec.slack.outputDefaults.channelId. Two constants for one key is two
	// spellings for `--answer destination=` to drift into.
	keyDestinationChannelID = "destination"
	// The three below are not asked but derived — what auth.test reported for
	// the bot token. They live in State because Result needs them and because
	// the review screen renders them.
	keyBotUserID   = "botuserid"
	keyTeamName    = "slackteam"
	keyBotUserName = "botuser"
)

// appRoute is what the Slack-app question decided: the user has an app, wants
// one created, or wants the manifest to create one by hand.
//
// The values are the strings the answer is stored as, because
// `--answer slackapp=true` and `--answer slackapp=false` already work and
// changing their spelling would break callers that use them. "provision" is
// additive.
type appRoute string

const (
	routeHave      appRoute = "true"
	routeProvision appRoute = "provision"
	routeManual    appRoute = "false"
)

// routeOfAnswer is the ONLY place the answer's vocabulary is interpreted.
// Anything reading keyHasSlackApp directly would be a second interpretation,
// and "does this run have an app?" is exactly the sort of question two readers
// end up disagreeing about.
//
// An unanswered question resolves to the manual route: it is the floor that
// always works, so an operator who has not been asked yet, or a fat-fingered
// accept, costs a run one extra step rather than silently claiming an app
// exists that does not.
func routeOfAnswer(answer string) appRoute {
	if r, ok := routeFor(answer); ok {
		return r
	}
	return routeManual
}

// routeFor interprets the answer's vocabulary, reporting whether it named a
// route at all. Every reader goes through it so the string spellings are
// interpreted in exactly one place — see routeOfAnswer's doc for why a second
// interpretation is the hazard worth closing.
func routeFor(answer string) (appRoute, bool) {
	switch strings.TrimSpace(answer) {
	case string(routeProvision):
		return routeProvision, true
	case string(routeHave):
		return routeHave, true
	case string(routeManual):
		return routeManual, true
	}
	return routeManual, false
}

// routeLabel is the text the Slack-app question shows for a route. Centralized
// so two flows offering the same route (both list routeManual and routeHave;
// only the agent flow adds routeProvision) show it with identical wording.
func routeLabel(r appRoute) string {
	switch r {
	case routeManual:
		return "Show me the manifest — I'll create it myself"
	case routeHave:
		return "I already have a Slack app"
	case routeProvision:
		return "Create one for me"
	default:
		return string(r)
	}
}

const (
	// defaultChannelNamePrefix is prepended to the bound AgentClass name to
	// suggest a default Channel CR name (AgentClass "helpdesk" →
	// "slack-helpdesk"). Defined here so the naming scheme can change in one
	// place rather than at every call site.
	defaultChannelNamePrefix = "slack-"
	// botDisplayNameSuffix is appended to the bound AgentClass name to derive
	// the Slack app + bot display name printed in the generated manifest
	// (AgentClass "helpdesk" → "helpdesk-bot").
	botDisplayNameSuffix = "-bot"
	// credsSecretNameSuffix names the Secret after its Channel.
	credsSecretNameSuffix = "-creds"

	botTokenPrefix = "xoxb-"
	appTokenPrefix = "xapp-"
)

// The prompt text the agent flow's questions declare, as named constants
// rather than literals so a test asserting what an operator is asked compares
// against one string per question rather than two copies that can drift.
const (
	agentClassPrompt = "Bind this Slack app to AgentClass"

	agentSlackAppPrompt      = "Have you already created a Slack app for this agent?"
	agentSlackAppDescription = "It needs Socket Mode enabled and the Agent feature turned on."

	botTokenPrompt = "Bot User OAuth Token (xoxb-)"
	appTokenPrompt = "App-Level Token (xapp-)"

	agentDestinationPrompt = "Slack channel ID to post into"

	// agentChannelNameHint is shown above the Channel-name question. The
	// question itself is the shared wizardkeys.ChannelNamePrompt.
	agentChannelNameHint = "This names a Channel object in the cluster, not a Slack channel — the bot replies in every Slack channel it is added to."
)

// agentDestinationGuidance says what is being asked for, why this run is being
// asked at all, and where in Slack's UI the value is found.
//
// The last part is the one that earns its length. A channel NAME is what an
// operator reaches for, it sits right beside the ID in every Slack surface,
// and nothing here resolves one into the other — so a question that only said
// "Slack channel" would be answered with "#demo-room" by most of the people
// who see it.
const agentDestinationGuidance = `Where should this agent's work be posted?

This Channel is somebody else's destination: nothing arrives on it, so no
inbound message can name a thread to reply into. Without an ID the agent's
first completed piece of work has nowhere to go.

  Find the ID: open the channel → right-click its name → 'Copy link' → the
               last segment of the URL (e.g. C0123ABCDEF). The channel NAME
               is not the same value and is not accepted.
  Invite the bot to it first: /invite @<bot-name>`

// outputDestinationNeeded reports whether a Channel this run creates has to
// carry its own destination, from the role the caller declared for it.
//
// role=output IS the whole condition, and it is the role rather than the bound
// AgentClass deliberately. A role=output Channel is somebody else's
// destination by construction: nothing is ever delivered TO it, so no inbound
// message can supply a Slack channel and a thread to reply into. Both of the
// Channel controller's destination rules land on exactly this set — the one
// keyed off the class's status.userlessInput, and the one a role=input Channel
// raises when it resolves this Channel and asks it for an anchor — so a
// role=output Slack Channel without a destination is refused under every
// configuration that routes anything to it.
//
// IT IS NOT KEYED OFF status.userlessInput, and that is a decision rather than
// an omission. Narrowing to a userless-input class would leave the
// split-channel case unasked — a Slack role=input Channel paired with a Slack
// role=output one, where the input is perfectly user-attributable and the
// output Channel still has to be told where to post (the controller's
// role=input binding check calls outputbind.Anchor on it). It would also read
// a status the wizard frequently cannot trust: the field is derived from the
// class's INPUT Channels, and `oap agent install` wires a bundle's channels in
// declaration order, so an output channel declared first is asked before the
// input Channel that makes the class userless exists at all.
//
// role=both is left out for the opposite reason: it is its own origin and its
// own destination, its inbound carries the thread to reply into, and
// outputbind never resolves one as a target. Asking there would put a Slack
// channel ID in front of every operator who runs `oap channel create --kind
// slack`, to fill in a field nothing reads.
func outputDestinationNeeded(role string) bool {
	return strings.TrimSpace(role) == spiceboxv1alpha1.ChannelRoleOutput
}

// slackChannelID is the shape of a Slack conversation ID: a C (public
// channel), G (private channel) or D (direct message) prefix followed by
// Slack's own uppercase alphanumeric body.
//
// The lower bound is 8 body characters, which is the shortest ID Slack has
// ever issued; there is no upper bound, because Enterprise Grid IDs are longer
// and a check that refused a real ID would be worse than no check at all.
//
// What it is FOR is not typo-catching but the one substitution an operator
// actually makes: pasting a channel NAME ("#demo-room"), or the whole
// Copy-link URL, into a field that only accepts an ID. Nothing downstream
// resolves a name, so the wrong value here is a delivery failure discovered
// when the agent finishes its first piece of work.
var slackChannelID = regexp.MustCompile(`^[CGD][A-Z0-9]{8,}$`)

// checkSlackChannelID refuses a destination that is not a Slack channel ID.
//
// It is the ONLY validation between the answer and the Channel: a channel
// wizard's questions carry no Validation any client evaluates
// (channelkinds.ValidateInputs refuses one outright), so a wrong value here is
// otherwise written to spec.slack.outputDefaults.channelId unexamined.
//
// The value is echoed back because it is not a credential — it is a channel ID
// the operator can read off their own Slack — and naming it is what makes the
// difference between "that is a channel name" and "that is an ID" visible.
func checkSlackChannelID(field, id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return fmt.Errorf("%s is required: a Slack channel ID (e.g. C0123ABCDEF) — open the channel → 'Copy link' → the last segment of the URL", field)
	}
	if !slackChannelID.MatchString(id) {
		return fmt.Errorf("%s must be a Slack channel ID like C0123ABCDEF, not %q; the channel NAME is a different value and nothing resolves one into the other — open the channel → 'Copy link' → the last segment of the URL", field, id)
	}
	return nil
}

// authTester is the slack-go subset used here and by the user_profile
// fetch path (pkg/channels/channelkinds/slack/userprofile.go); injectable for tests.
type authTester interface {
	AuthTestContext(ctx context.Context) (*slackapi.AuthTestResponse, error)
}

// authTesterFactory builds an authTester from a bot-token.
type authTesterFactory func(botToken string) authTester

// slackWizard is this kind's channelkinds.Wizard. Kind.Wizard returns a
// fresh one per call.
//
// Both fields are injected collaborators, wired once and never written by a
// run: Resolve uses them, and holding a live client is what "does I/O" means.
// Nothing else lives here, and nothing may — Result must be callable on a
// value no Inputs call ever touched, so per-run state on the receiver is
// exactly what channelkinds.Wizard.Result rules out. Which flow a run is
// (agent or monitoring) therefore comes from WizardInput.Monitoring on every
// call, never from a field an earlier call set.
type slackWizard struct {
	authFactory authTesterFactory

	// provisionClient performs the app-configuration calls. Injected the same
	// way authFactory is, so a test drives both halves without a network.
	provisionClient appprovision.Client
}

// agentOutput builds the Secret, the Channel, the AgentClass capability patch
// and the next-steps notes for the agent flow, from the answers alone.
//
// Summary is left to Result, which is where the run's decisions are stated —
// see channelkinds.WizardOutput.Summary.
func agentOutput(namespace, role string, a wizardAnswers) (channelkinds.WizardOutput, error) {
	// Fail closed on a missing answer rather than emitting a Secret with an
	// empty token or a Channel bound to no agent: huh's accessible renderer
	// cannot report a read error, so a truncated input script reaches here as
	// silence rather than as a failure.
	answers := map[string]string{}
	for _, key := range []string{keyAgentClass, keyBotToken, keyAppToken, wizardkeys.KeyChannelName, keyBotUserID} {
		v := strings.TrimSpace(a.get(key))
		if v == "" {
			return channelkinds.WizardOutput{}, fmt.Errorf("slack wizard: %q was not answered", key)
		}
		answers[key] = v
	}
	// Checked separately because it is a multi-value answer, and because an
	// ABSENT one must never be read as "nothing was checked": that reading
	// would disable every default-on capability on the bound agent.
	//
	// Conditioned on this kind offering any capability at all, which is the
	// same guard Inputs applies: it never declares a question with no
	// options, so requiring an answer to one that was never posed would
	// refuse a legitimate run.
	options := slackCapabilityOptions()
	if len(options) > 0 && !a.has(keyCapabilities) {
		return channelkinds.WizardOutput{}, fmt.Errorf("slack wizard: %q was not answered", keyCapabilities)
	}
	// The fail-closed check on a token pasted into the wrong field. It is
	// repeated here, and not left to Resolve, because a channel wizard's
	// questions carry no validation any client evaluates — Question.Validation
	// is refused by channelkinds.ValidateInputs, since nothing on this side
	// evaluates it — so this is the last place a swapped pair is caught
	// before it is written into a Secret.
	if err := checkTokenPrefix("bot", botTokenPrefix, answers[keyBotToken]); err != nil {
		return channelkinds.WizardOutput{}, err
	}
	if err := checkTokenPrefix("app-level", appTokenPrefix, answers[keyAppToken]); err != nil {
		return channelkinds.WizardOutput{}, err
	}

	// The destination this Channel posts into, when it is a Channel that has
	// to carry one. Checked HERE and not only at the widget for the reason the
	// token prefixes are: a channel wizard's questions carry no validation any
	// client evaluates, so this is the last place a channel NAME pasted into
	// an ID field is caught before it is written into a Channel that the
	// controller then refuses (ReasonChannelOutputDestinationMissing) or that
	// silently posts nowhere.
	//
	// The role rather than the answer decides whether it is required: a
	// role=both run is never asked, and refusing it for not having answered
	// would refuse every ordinary Slack channel.
	var outputDefaults *spiceboxv1alpha1.SlackOutputDefaults
	destination := strings.TrimSpace(a.get(keyDestinationChannelID))
	switch {
	case outputDestinationNeeded(role):
		if err := checkSlackChannelID("slack wizard: "+keyDestinationChannelID, destination); err != nil {
			return channelkinds.WizardOutput{}, err
		}
		// threadStrategy is deliberately left unset: the CRD defaults it to
		// new-thread-per-session, which is what a Channel delivering completed
		// work wants, and a value written here would be a second place for
		// that default to live. staticThreadTs has no wizard question for the
		// same reason — the strategy that needs it is not reachable from one.
		outputDefaults = &spiceboxv1alpha1.SlackOutputDefaults{ChannelID: destination}
	case destination != "":
		// Answered on a run that was never asked. Refused rather than written,
		// because the two readings — the caller means a different role, or the
		// key is a leftover — differ by whether the Channel they get is the
		// one they asked for. checkAnswerKeys already refuses the flag on this
		// route; this catches a client that never declared its questions.
		return channelkinds.WizardOutput{}, fmt.Errorf(
			"slack wizard: %q is only asked of a role=%s Channel, which is the only one nothing inbound can tell where to post; this run declared role %q",
			keyDestinationChannelID, spiceboxv1alpha1.ChannelRoleOutput, role)
	}

	className := answers[keyAgentClass]
	channelName := answers[wizardkeys.KeyChannelName]
	secretName := credsSecretName(channelName)
	appID := strings.TrimSpace(a.get(keyAppID))

	secret := &corev1.Secret{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
		ObjectMeta: metav1.ObjectMeta{
			Name: secretName, Namespace: namespace,
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			SecretKeyBotToken: []byte(answers[keyBotToken]),
			SecretKeyAppToken: []byte(answers[keyAppToken]),
		},
	}
	channel := &spiceboxv1alpha1.Channel{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "agentprimitives.authzed.com/v1alpha1",
			Kind:       "Channel",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: channelName, Namespace: namespace,
		},
		Spec: spiceboxv1alpha1.ChannelSpec{
			Kind:           "slack",
			AgentClass:     className,
			SessionScope:   "auto",
			CredentialsRef: spiceboxv1alpha1.ChannelCredentialsRef{SecretName: secretName},
			Slack: &spiceboxv1alpha1.SlackChannelConfig{
				Mode:      "socket",
				BotUserID: answers[keyBotUserID],
				// Empty unless this run created the app.
				AppID: appID,
				// nil unless this run's Channel has to carry its own
				// destination; a Channel nobody asked one of must not be
				// given one.
				OutputDefaults: outputDefaults,
			},
		},
	}

	checked := a.all(keyCapabilities)
	return channelkinds.WizardOutput{
		SecretManifest:  secret,
		ChannelManifest: channel,
		CapabilityPatch: capabilityPatch(namespace, className, options, checked),
		Notes:           postSetupNotes(channelName, className, namespace, selectedCapabilitiesFrom(checked), appID, destination),
	}, nil
}

// postSetupNotes is the guidance the CLI prints after applying: the facts about
// an app that now exists, which belong after the run rather than as a wall of
// prose in front of the first question.
//
// Every line earns its place against one test: is this still work the user has
// to do? A note describing something ALREADY DONE — by the manifest oap
// generated, or by the CLI itself — reads as though the run did not take. The
// two arguments are what that is judged against.
//
// `chosen` is the capabilities this run asked for, so a caveat exists only for a
// capability that was picked.
//
// appID is empty unless this run provisioned the app itself (only a
// successful install derives keyAppID), which makes it the reliable
// "did oap build this app?" signal. It splits the list in two:
//
//   - A PASTED-TOKEN run's app is the user's: oap requested no scope, set no
//     manifest field, enabled no feature, so every caveat still applies and the
//     user holds the only copy of the tokens.
//   - A PROVISIONED run's app is oap's: appManifestFor declared agent_view,
//     turned the Agent feature on, and requested the scopes `chosen` implies.
//     What remains is the one thing no Slack API can set or read — the
//     workspace-level "Agents & AI Apps" toggle — plus the app ID, which
//     nothing in Slack's API can list afterwards.
//
// Nothing here tells the user to watch the Channel's conditions: `oap channel
// create` watches the Channel it applied, reports the outcome, and names the
// follow-up command on anything but a clean connect.
// destination is the Slack channel this Channel posts into, or "" for a
// Channel whose replies follow whatever came in. It splits the opening line —
// "replies wherever it is added" is exactly wrong for a Channel that only ever
// posts into one place — and adds the one step no manifest can do for the
// operator: a bot that is not a member of the destination cannot post to it,
// and Slack reports that only at the first send.
func postSetupNotes(channelName, className, namespace string, chosen []capabilityOption, appID, destination string) []string {
	opening := fmt.Sprintf("Created Channel resource %q (a Kubernetes object) bound to AgentClass %q in namespace %q. The bot replies in any Slack channel it's added to.", channelName, className, namespace)
	if destination != "" {
		opening = fmt.Sprintf("Created Channel resource %q (a Kubernetes object) bound to AgentClass %q in namespace %q. This agent's work is posted into Slack channel %s.", channelName, className, namespace, destination)
	}
	notes := []string{opening}
	if destination != "" {
		notes = append(notes, fmt.Sprintf("Invite the bot to %s (/invite @<bot-name>) — it cannot post into a channel it is not a member of, and Slack only says so at the first send.", destination))
	}
	if appID != "" {
		// Nothing in Slack's API can list the apps a user created, so the one
		// place this app is written down is here and on the Channel.
		return append(notes,
			fmt.Sprintf("oap created and installed Slack app %s for this Channel. Slack has no API that lists your apps, so this ID and the Channel's spec.slack.appId are the only record of it.", appID),
			"If the bot never shows an 'is thinking…' status, a workspace admin has to enable 'Agents & AI Apps' — the one part of the app setup no Slack API can do, so oap could not do it for you.",
		)
	}

	// Only a user who pasted the tokens holds the only copy.
	notes = append(notes,
		"Save the bot-token and app-token in your password manager — they cannot be retrieved later from the Secret.",
		"If the bot never shows an 'is thinking…' status: the workspace needs 'Agents & AI Apps' enabled, the app needs its 'Agent or Assistant' feature ON, and the bot needs the assistant:write scope.",
	)
	for _, o := range chosen {
		// Only the capabilities whose cost is NOT a Slack scope.
		//
		// A capability that costs scopes needs no line here any more: channelsd
		// computes the gap against this same FeatureSupport table and writes it
		// as ScopesValid=False naming the scopes ACTUALLY missing — not every
		// scope the choice implies — and the watch surfaces that with the
		// command to read it in full. Listing them here would be a worse copy
		// of a report the run now makes.
		//
		// A capability whose cost is a manifest field or an event subscription
		// has no such reporter: no condition can observe it, so if this line
		// does not say it, nothing does. credential_update is that case —
		// features.app_home.home_tab_enabled plus the app_home_opened event,
		// neither of which oap touched on an app it did not build.
		if len(o.scopes) == 0 {
			notes = append(notes, o.note())
		}
	}
	return append(notes,
		"An app created before the inline Messages-tab surface still declares assistant_view: change it to agent_view (and assistant_description to agent_description, dropping the assistant_thread_started and assistant_thread_context_changed events) in the app manifest. Scopes are unchanged, so no reinstall — but the flip is irreversible, and Slack clients show the old surface until they are restarted.",
	)
}

func credsSecretName(channelName string) string { return channelName + credsSecretNameSuffix }

// appSetupSteps renders the create-an-app instructions around an already
// rendered manifest, and says what became of the copy on disk.
//
// orphanAppID, when set, is an app oap already created for this agent: step 2
// then opens that app instead of creating one, and the manifest is left out
// entirely rather than sitting under an instruction not to use it. Everything
// from step 3 down is the same either way — those steps are what an existing,
// uninstalled app still needs.
//
// Every line is kept inside the note's column budget (61 on a standard
// 80-column terminal) so that huh's wrap has nothing to do: prose that wraps is
// merely untidy, but the manifest sits in the middle of this text and a wrapped
// YAML line is one Slack refuses to import.
//
// comeBack says how the operator gets the tokens back into oap once the app
// exists, and savedLines says what became of the copy on disk — both supplied
// by the caller rather than composed here, because the two callers answer them
// differently: the agent flow's manual route spells out a whole second command
// (dataComeBack) and writes the file, while a route with nowhere to write says
// nothing about one.
func appSetupSteps(manifest, comeBack, savedLines, orphanAppID string) string {
	var b strings.Builder
	if orphanAppID != "" {
		b.WriteString("Finish the Slack app oap created, then come back:\n")
	} else {
		b.WriteString("Create the Slack app for this agent, then come back:\n")
	}
	b.WriteString(comeBack + "\n\n")
	if savedLines != "" {
		b.WriteString(savedLines)
		b.WriteString("\n\n")
	}
	b.WriteString("  1. Workspace prerequisite (one-time, admin-level):\n")
	b.WriteString("       Slack admin → Settings & administration →\n")
	b.WriteString("       Workspace settings → 'Permissions' tab →\n")
	b.WriteString("       'Agents & AI Apps' → enable. Without it, the\n")
	b.WriteString("       AI-native 'is thinking…' status indicator\n")
	b.WriteString("       silently no-ops.\n\n")
	if orphanAppID != "" {
		// No manifest here: this app was created FROM that manifest and
		// already requests these scopes. Showing it again next to an
		// api.slack.com link is what invites a second app.
		b.WriteString("  2. Open the app oap already created for you:\n")
		b.WriteString("       https://api.slack.com/apps/" + orphanAppID + "\n")
		b.WriteString("     oap built it from the same manifest, so it already\n")
		b.WriteString("     requests the scopes you chose. Carry on at step 3.\n\n")
		b.WriteString(remainingAppSetupSteps())
		return b.String()
	}
	b.WriteString(CreateAppFromManifestStep(2, manifest))
	b.WriteString(remainingAppSetupSteps())
	return b.String()
}

// CreateAppFromManifestStep is the "make the app out of this document" step,
// numbered n, with the manifest itself below it.
//
// Numbered by the caller because the two callers count differently: a Channel's
// app is created at step 2, after the workspace prerequisite, while an app that
// exists only to issue a bot token has no prerequisite and creates at step 1.
// EXPORTED for that second caller — the credential-setup flow in
// pkg/platform/identity/setup/builtins/slack_bot_token, which shows this
// alongside BotTokenAppManifestFor's narrower manifest. It is the same three
// clicks through the same Slack UI, and the quoted labels are what an operator
// matches against the screen in front of them, so there is one copy of them.
//
// The manifest is written verbatim, with no indent of its own: it is YAML on a
// note that hard-wraps, and an extra two columns is two columns closer to the
// wrap that makes it unimportable.
func CreateAppFromManifestStep(n int, manifest string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "  %d. Create the app from this manifest:\n", n)
	b.WriteString("       https://api.slack.com/apps → 'Create New App'\n")
	b.WriteString("       → 'From an app manifest'. Pick your workspace,\n")
	b.WriteString("       then paste:\n\n")
	b.WriteString(manifest)
	b.WriteString("\n")
	return b.String()
}

// InstallAndCopyBotTokenStep is the "install it and copy the xoxb- token"
// step, numbered n. Exported, and numbered by the caller, for the reasons
// CreateAppFromManifestStep's doc gives: every route that ends with a bot
// token ends with exactly these clicks.
func InstallAndCopyBotTokenStep(n int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "  %d. Install to the workspace:\n", n)
	b.WriteString("       'Install App' → 'Install to <Workspace>' →\n")
	b.WriteString("       Allow, then copy the bot token (xoxb-) shown\n")
	b.WriteString("       after install.\n")
	return b.String()
}

// remainingAppSetupSteps is everything after the app exists: the toggles,
// the install and the two tokens. One copy, because both ways of arriving at
// step 3 — creating the app by hand, or opening the one oap created and could
// not finish — need exactly these steps, and two copies is two places for the
// numbering and Slack's own UI labels to drift.
func remainingAppSetupSteps() string {
	var b strings.Builder
	// Each quoted label is kept whole on its line. They are what the user hunts
	// for in Slack's own UI, and a label split across two lines is one they
	// cannot match against what is in front of them.
	b.WriteString("  3. Enable the AI Agent feature (a UI toggle, not\n")
	b.WriteString("     expressible in the manifest):\n")
	b.WriteString("       your app → 'Agents & AI Apps' → toggle\n")
	b.WriteString("       'Agent or Assistant' ON → Save. This is what\n")
	b.WriteString("       unlocks the status indicator and the inline\n")
	b.WriteString("       Messages-tab experience, and it adds the\n")
	b.WriteString("       assistant:write bot scope for you.\n\n")
	b.WriteString("  4. (optional) 'Basic Information' →\n")
	b.WriteString("     'Display Information' → 'App icon': upload the\n")
	b.WriteString("     agent's logo. Slack has no manifest field for it.\n")
	b.WriteString("     If the agent came from a .oap bundle carrying a\n")
	b.WriteString("     logo, that asset is the image.\n\n")
	b.WriteString(InstallAndCopyBotTokenStep(5))
	b.WriteString("\n")
	b.WriteString("  6. Generate the app-level token for Socket Mode:\n")
	b.WriteString("       'Basic Information' → 'App-Level Tokens' →\n")
	b.WriteString("       'Generate Token and Scopes' → add the\n")
	b.WriteString("       connections:write scope → Generate, then copy\n")
	b.WriteString("       it (xapp-).\n")
	return b.String()
}

// authTest asks Slack who a bot token belongs to, rejecting a response that
// names no bot user: a Channel needs one to recognise its own messages, and a
// blank one would only be discovered once the bot started answering itself.
//
// Called once per run, from each flow's Resolve — the one step that may do
// I/O the answers imply (channelkinds.Wizard.Resolve).
func authTest(ctx context.Context, factory authTesterFactory, botToken string) (*slackapi.AuthTestResponse, error) {
	auth, err := factory(botToken).AuthTestContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("Slack rejected this bot token (auth.test): %w", err)
	}
	if auth == nil || auth.UserID == "" {
		return nil, errors.New("Slack accepted this bot token but reported no bot user; the Channel needs one to recognise its own messages")
	}
	return auth, nil
}

// checkTokenPrefix rejects a token pasted into the wrong field — the common
// mistake, since both are copied from the same page — without echoing more
// than the prefix that identifies it.
func checkTokenPrefix(which, prefix, token string) error {
	if token == "" {
		return fmt.Errorf("the %s token is required (it starts with %q)", which, prefix)
	}
	if !strings.HasPrefix(token, prefix) {
		return fmt.Errorf("the %s token must start with %q; this one starts with %q", which, prefix, safePrefix(token))
	}
	return nil
}

// tokenGuidance says where in Slack's UI each token is found. Both come from
// the same app but different pages, which is why they are asked for together.
const tokenGuidance = `Paste both tokens from your Slack app.

  Bot User OAuth Token (xoxb-)
    your app → 'OAuth & Permissions' → 'Bot User OAuth Token', at the top of
    the page once the app is installed to the workspace.

  App-Level Token (xapp-)
    your app → 'Basic Information' → 'App-Level Tokens'. Generate one with the
    connections:write scope if there is none. It is what Socket Mode connects
    with, so no public ingress is needed.`

// safePrefix returns the first ≤6 chars of s for error messages without
// leaking a full token.
func safePrefix(s string) string {
	if len(s) > 6 {
		return s[:6]
	}
	return s
}
