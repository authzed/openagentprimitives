// pkg/channels/channelkinds/slack/wizard_monitoring.go
//
// Setup flow for a monitoring-role Slack Channel — the pure output sink that
// framework events (credential failures, reconciler alerts) are posted to. It
// asks four things:
//
//	slackapp     is there a Slack app with a bot token yet?
//	bot-token    the token, verified with Slack by resolve; on re-setup a
//	             blank answer keeps the one already stored
//	destination  the Slack channel ID events are posted to
//	name         name the Channel resource; not asked on re-setup, where the
//	             Channel being reconfigured already has a name
//
// It differs from the agent flow in three ways, all consequences of one fact:
// nothing on a monitoring channel LISTENS. There is no AgentClass to bind to,
// no app-level token because Socket Mode is not involved, and a destination
// channel ID because the events have nowhere else to go.
//
// Re-setup is a first-class path here, unlike the agent flow: `oap init` offers
// monitoring on every run, so the second run must reconfigure the channel the
// first one made rather than refuse its name as a duplicate.
//
// This is the monitoring half of the slack kind's channelkinds.Wizard;
// slackWizard dispatches to it on in.Monitoring. Its two steps that ask
// NOTHING — handing over the create-an-app instructions, and asking Slack who
// the token belongs to — are resolve, which is the first step of this contract
// that may do I/O and the first that can refuse on an answer's content.
package slack

import (
	"context"
	"errors"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/wizardkeys"
	"github.com/authzed/openagentprimitives/pkg/platform/oap"
	"github.com/authzed/openagentprimitives/pkg/x/credmask"
)

// The prompt text this flow's questions declare, as named constants rather
// than literals so a test asserting what an operator is asked compares against
// one string per question rather than two copies that can drift.
const (
	monitoringSlackAppPrompt      = "Have you already created a Slack app with a bot token?"
	monitoringSlackAppDescription = "A monitoring channel only posts, so the app needs the chat:write scope — no Socket Mode, no Agent feature."

	monitoringDestinationPrompt = "Slack channel ID"

	// monitoringKeepBlankLine is appended to the token guidance on a re-setup
	// that has credentials stored, which is the only configuration in which a
	// blank answer means anything other than "you forgot the token".
	monitoringKeepBlankLine = "\n\n  Leave it blank to keep the token already stored."

	// monitoringChannelNameHint is shown above the Channel-name question. The
	// question itself is the shared wizardkeys.ChannelNamePrompt.
	monitoringChannelNameHint = "This names a Channel object in the cluster. The Slack channel events post to is the ID you just gave."
)

// monitoringFlow is the monitoring-role branch of the Slack wizard: the
// questions it declares and the manifests it reads back out of their answers.
//
// It is REBUILT from the WizardInput on every call rather than held on the
// wizard, which is what monitoringFlowFor is for. existing and
// credentialsExist in particular decide whether a blank token means "keep the
// stored one" or "you forgot the token", and inputs, resolve and output all
// have to answer that the same way — from the same argument, not from
// whatever an earlier call happened to leave behind.
type monitoringFlow struct {
	authFactory      authTesterFactory
	namespace        string
	existing         *spiceboxv1alpha1.Channel
	credentialsExist bool
}

// monitoringFlowFor builds the branch from the INERT half of a WizardInput
// alone — the half channelkinds.Wizard.Result is allowed to read.
//
// It exists so Result can construct the branch from its own argument: under
// this contract the calls of one run may land in different processes, so a
// Result that read a receiver field would build its manifests from a zero
// value.
//
// authFactory is deliberately left nil: it is a live client, and the result
// path must not need one. A caller that IS doing I/O — Resolve — sets the
// field before using the flow, rather than passing the factory alongside it,
// so there is never a configuration in which the field is nil while the
// branch is calling Slack.
func monitoringFlowFor(in channelkinds.WizardInput) *monitoringFlow {
	return &monitoringFlow{
		namespace:        in.Namespace,
		existing:         in.Existing,
		credentialsExist: in.CredentialsExist,
	}
}

// keepOffered reports whether a blank token answer means "keep the one already
// stored". Both halves are required: with no existing Channel there is nothing
// to reconfigure, and with no stored Secret there is nothing to keep — a blank
// answer in either case is a missing token, not a choice.
func (f *monitoringFlow) keepOffered() bool {
	return f.existing != nil && f.credentialsExist
}

// keepingToken reports whether THIS run is keeping the stored token. One
// definition, consulted by resolve (skip auth.test), summary (say so) and
// output (emit no Secret), because a disagreement between them would either
// overwrite a good Secret with an empty one or test a token that is not there.
func (f *monitoringFlow) keepingToken(token string) bool {
	return f.keepOffered() && strings.TrimSpace(token) == ""
}

// existingBotUserID is what the Channel being reconfigured already recorded, or
// "" when there is none.
func (f *monitoringFlow) existingBotUserID() string {
	if f.existing == nil || f.existing.Spec.Slack == nil {
		return ""
	}
	return f.existing.Spec.Slack.BotUserID
}

// existingChannelID is the destination the Channel being reconfigured already
// posts to, offered as the default so re-setup does not make the user look it
// up again.
func (f *monitoringFlow) existingChannelID() string {
	if f.existing == nil || f.existing.Spec.Slack == nil || f.existing.Spec.Slack.OutputDefaults == nil {
		return ""
	}
	return f.existing.Spec.Slack.OutputDefaults.ChannelID
}

// channelNameFrom is the name of the Channel this run produces: the one being
// reconfigured, or the one the name question asked for.
//
// On re-setup the existing Channel's own name wins outright rather than being
// read back out of the answers — inputs declares no name question at all in
// that configuration, so there is no answer to read.
func (f *monitoringFlow) channelNameFrom(a wizardAnswers) string {
	if f.existing != nil {
		return f.existing.Name
	}
	return strings.TrimSpace(a.get(wizardkeys.KeyChannelName))
}

// credsSecretName is where the bot token lives.
//
// It follows the "<channelName>-creds" convention only for a Channel that does
// not already declare one. On re-setup the existing CredentialsRef wins even
// when it is nothing like the convention — a hand-authored or pre-declared ref
// pointed at a Secret of the wizard's own choosing would leave the re-applied
// Channel reading credentials from an object that holds nothing.
func (f *monitoringFlow) credsSecretName(channelName string) string {
	if f.existing != nil && f.existing.Spec.CredentialsRef.SecretName != "" {
		return f.existing.Spec.CredentialsRef.SecretName
	}
	return credsSecretName(channelName)
}

// output builds the monitoring Channel, its Secret (or nil, on the keep path)
// and the next-steps notes from the answers alone. Summary is left to the
// caller, for the same reason agentOutput leaves it.
func (f *monitoringFlow) output(a wizardAnswers) (channelkinds.WizardOutput, error) {
	// Fail closed on a missing answer rather than emitting a Channel that posts
	// nowhere or a Secret holding an empty token: huh's accessible renderer
	// cannot report a read error, so a truncated input script reaches here as
	// silence rather than as a failure.
	if !a.has(keyBotToken) {
		return channelkinds.WizardOutput{}, unansweredErr(keyBotToken)
	}
	channelID := strings.TrimSpace(a.get(keyDestinationChannelID))
	if channelID == "" {
		// Named key plus how to find the value. An interactive operator reads
		// monitoringDestinationGuidance as the question's Description, but a
		// non-interactive one never renders a question at all — this refusal is
		// the only place they can be told what the answer looks like or where
		// Slack keeps it.
		return channelkinds.WizardOutput{}, fmt.Errorf("%w: a Slack channel ID (e.g. C0123ABCDEF) — open the channel → 'Copy link' → the last segment of the URL",
			unansweredErr(keyDestinationChannelID))
	}
	channelName := f.channelNameFrom(a)
	if channelName == "" {
		return channelkinds.WizardOutput{}, unansweredErr(wizardkeys.KeyChannelName)
	}

	botToken := strings.TrimSpace(a.get(keyBotToken))
	keep := f.keepingToken(botToken)
	if !keep {
		if botToken == "" {
			// Blank with nothing stored to keep is a missing answer, not a
			// choice. Named as the key rather than as prose, because the caller
			// that reaches here without one is seeding State from flags.
			return channelkinds.WizardOutput{}, unansweredErr(keyBotToken)
		}
		if err := checkTokenPrefix("bot", botTokenPrefix, botToken); err != nil {
			return channelkinds.WizardOutput{}, err
		}
	}
	// Required only when a token was supplied, because that is the only case in
	// which auth.test ran and must have reported one. On the keep path the ID
	// is whatever the existing Channel recorded, which may predate the field:
	// refusing there would wedge re-setup of a Channel that has been posting
	// happily for months.
	botUserID := strings.TrimSpace(a.get(keyBotUserID))
	if keep && botUserID == "" {
		// Resolve returns this as a derived answer on every run that fetched
		// it. A client that skipped Resolve still gets the Channel's own
		// recorded ID rather than a blank one.
		botUserID = f.existingBotUserID()
	}
	if !keep && botUserID == "" {
		return channelkinds.WizardOutput{}, unansweredErr(keyBotUserID)
	}

	secretName := f.credsSecretName(channelName)
	// A nil Secret is how the CLI is told to leave the stored credentials alone.
	var secret *corev1.Secret
	if !keep {
		secret = &corev1.Secret{
			TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
			ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: f.namespace},
			Type:       corev1.SecretTypeOpaque,
			// No app-level token: nothing listens, so Socket Mode never connects.
			Data: map[string][]byte{SecretKeyBotToken: []byte(botToken)},
		}
	}
	channel := &spiceboxv1alpha1.Channel{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "agentprimitives.authzed.com/v1alpha1",
			Kind:       "Channel",
		},
		ObjectMeta: metav1.ObjectMeta{Name: channelName, Namespace: f.namespace},
		Spec: spiceboxv1alpha1.ChannelSpec{
			Kind:           "slack",
			Role:           spiceboxv1alpha1.ChannelRoleMonitoring,
			CredentialsRef: spiceboxv1alpha1.ChannelCredentialsRef{SecretName: secretName},
			Slack: &spiceboxv1alpha1.SlackChannelConfig{
				BotUserID:      botUserID,
				OutputDefaults: &spiceboxv1alpha1.SlackOutputDefaults{ChannelID: channelID},
			},
		},
	}

	replaceExisting := f.existing != nil
	return channelkinds.WizardOutput{
		SecretManifest:  secret,
		ChannelManifest: channel,
		Notes:           monitoringNotes(channelName, channelID, f.namespace, replaceExisting, keep),
		ReplaceExisting: replaceExisting,
	}, nil
}

// inputs states this flow's questions, in the order they are asked.
//
// The name question is omitted on re-setup: the Channel being reconfigured
// already has a name, and asking again could only produce that name — which
// the CLI's collision check refuses — or a SECOND monitoring Channel beside
// the one the operator meant to change. refuseRenamingOnResetup is what makes
// a seeded name on that route a refusal rather than a value nothing reads.
func (f *monitoringFlow) inputs(in channelkinds.WizardInput) ([]oap.Question, error) {
	if err := f.refuseRenamingOnResetup(in); err != nil {
		return nil, err
	}

	// Refused before anything is asked, when the flags already chose a route
	// this flow cannot serve. Both messages are static — neither is derived
	// from an answer — so unlike the agent flow's manual route there is
	// nothing to collect first, and asking for a bot token, a destination and
	// a Channel name before saying "you have no Slack app yet" is an ordering
	// worth going out of the way to avoid.
	route, decided := seededRoute(in)
	if decided {
		switch route {
		case routeProvision:
			return nil, errMonitoringProvisionUnsupported
		case routeManual:
			return nil, errors.New(monitoringAppSetupSteps)
		}
	}

	guidance := monitoringTokenGuidance
	tokenRequired := true
	if f.keepOffered() {
		guidance += monitoringKeepBlankLine
		// Blank is a real answer here — "keep the token already stored" — so
		// the question must accept one. output reads the same condition
		// (keepingToken) to decide whether to emit a Secret at all.
		tokenRequired = false
	}
	// Nor can anything below be required while the route is still open: an
	// operator who answers the Slack-app question with "no app yet" has no bot
	// token, no destination and no Channel to name, and a required question
	// with no default refuses a blank outright — leaving them unable to reach
	// the resolve step that would have told them what to do. The fail-closed
	// checks do not move: output refuses a missing token, destination or name
	// by name, on every path.
	if !decided {
		tokenRequired = false
	}

	// Offered so a bare accept keeps the destination rather than making the
	// operator look the ID up a second time. Omitted (untyped nil) on first
	// setup, where there is nothing to derive it from.
	var destinationDefault any
	if id := f.existingChannelID(); id != "" {
		destinationDefault = id
	}

	qs := []oap.Question{
		{
			Name:        keyHasSlackApp,
			Type:        oap.QEnum,
			Prompt:      monitoringSlackAppPrompt,
			Description: monitoringSlackAppDescription,
			// No routeProvision: nothing in this flow can act on "create one
			// for me" here — see resolve, which refuses it. routeManual is
			// first and is the default: it is the floor that always works.
			Enum:       routeValues(routeManual, routeHave),
			EnumLabels: routeLabels(routeManual, routeHave),
			Default:    string(routeManual),
		},
		{
			Name:        keyBotToken,
			Type:        oap.QSecret,
			Prompt:      botTokenPrompt,
			Description: guidance,
			Required:    requiredAnswer(tokenRequired),
		},
		{
			Name:        keyDestinationChannelID,
			Type:        oap.QString,
			Prompt:      monitoringDestinationPrompt,
			Description: monitoringDestinationGuidance,
			Default:     destinationDefault,
			Required:    requiredAnswer(decided),
		},
	}
	if f.existing != nil {
		return qs, nil
	}
	return append(qs, oap.Question{
		Name:        wizardkeys.KeyChannelName,
		Type:        oap.QString,
		Prompt:      wizardkeys.ChannelNamePrompt,
		Description: monitoringChannelNameHint,
		Required:    requiredAnswer(decided),
		// P5-R16: NO default, and there is no way to offer a right one. The
		// only fact worth naming a monitoring Channel after is the Slack
		// workspace — it binds to no AgentClass — and auth.test does not
		// report that until resolve, long after this batch is declared. A
		// default derived from what is known HERE would be
		// "slack-monitoring-" and nothing else, which a bare accept would
		// take silently.
	}), nil
}

// requiredAnswer is Question.Required for a known answer to "must this be
// filled in?". nil means required, so only the false case needs a pointer.
func requiredAnswer(required bool) *bool {
	if required {
		return nil
	}
	return optionalAnswer()
}

// refuseRenamingOnResetup refuses a run that names a Channel other than the
// one it is reconfiguring, at the only moment this contract can refuse it.
//
// The name question is not declared on re-setup, so a seeded name would
// otherwise be read by nothing at all and silently ignored — `--name` in
// particular, which reaches WizardInput.Seeded but is not an `--answer` and so
// is never checked against the declared key set.
//
// The two readings of that input are "rename the Channel" (which this flow
// cannot do: a rename is a create plus a delete) and "configure a different
// Channel" (which contradicts ReplaceExisting, the flag that relaxes the CLI's
// refuse-to-overwrite guard). Neither is safe to guess at.
func (f *monitoringFlow) refuseRenamingOnResetup(in channelkinds.WizardInput) error {
	if f.existing == nil {
		return nil
	}
	seeded := wizardkeys.SeededAnswer(in, wizardkeys.KeyChannelName)
	if seeded == "" || seeded == f.existing.Name {
		return nil
	}
	return fmt.Errorf("this run reconfigures the existing monitoring Channel %q, so it cannot also be named %q; drop the name to update %[1]q, or delete it first to create a new one",
		f.existing.Name, seeded)
}

// resolve is what only Slack (or the Channel being reconfigured) can answer:
// who the bot token belongs to.
//
// It also carries the two refusals for a route this flow cannot serve, for the
// run whose route was NOT settled by a flag: inputs refuses a SEEDED one
// before asking anything, but a route the operator picks at the Slack-app
// question is not knowable until here.
//
// KNOWN GAP, and the reason the rest of this flow's questions are optional
// while the route is still open: the whole batch is asked before this runs, so
// an operator who then says they have no app has already been shown three
// questions they cannot answer. Optionality keeps that from being a dead end —
// three bare accepts reach this refusal, which hands over the create-an-app
// steps — but the three prompts are a real cost of a single up-front batch.
func (f *monitoringFlow) resolve(ctx context.Context, a wizardAnswers) (map[string]string, error) {
	switch routeOfAnswer(a.get(keyHasSlackApp)) {
	case routeProvision:
		// Refused rather than silently treated as "have one" — that would
		// carry the run into building a Channel around a token from an app
		// nobody created.
		return nil, errMonitoringProvisionUnsupported
	case routeManual:
		return nil, errors.New(monitoringAppSetupSteps)
	}

	token := strings.TrimSpace(a.get(keyBotToken))
	if f.keepingToken(token) {
		// Never asked on the keep path: there is no plaintext to test, and
		// what the existing Channel recorded was validated when it was
		// written. An ID it never recorded is derived not at all, rather than
		// as an empty string that would read as an answer.
		if id := strings.TrimSpace(f.existingBotUserID()); id != "" {
			return map[string]string{keyBotUserID: id}, nil
		}
		return nil, nil
	}

	if err := checkTokenPrefix("bot", botTokenPrefix, token); err != nil {
		return nil, err
	}
	auth, err := authTest(ctx, f.authFactory, token)
	if err != nil {
		return nil, err
	}
	return map[string]string{
		keyBotUserID:   auth.UserID,
		keyTeamName:    auth.Team,
		keyBotUserName: auth.User,
	}, nil
}

// summary is what the run DECIDED, in the order the operator decided it: the
// token, who Slack says it belongs to, the destination, the Channel.
//
// This kind runs no code of its own while the questions are being answered, so
// every line here has to be stated by Result or it is never written at all —
// see channelkinds.WizardOutput.Summary.
//
// THE TOKEN IS MASKED. A SummaryNote.Value reaches plain scrollback verbatim
// and outlives the run in the operator's terminal history; see agentSummary
// for the same obligation on the agent flow's two tokens.
func (f *monitoringFlow) summary(a wizardAnswers) []channelkinds.SummaryNote {
	token := strings.TrimSpace(a.get(keyBotToken))
	var notes []channelkinds.SummaryNote
	if f.keepingToken(token) {
		notes = append(notes, channelkinds.SummaryNote{
			Label: "Bot token", Value: "kept — the stored credentials are unchanged",
		})
		// Nothing asked Slack on this path, so there is no workspace name and
		// no bot NAME to report, and rendering "@ ()" for the halves that
		// were never fetched is worse than reporting only the half that
		// exists. An identity the existing Channel never recorded is reported
		// not at all.
		id := strings.TrimSpace(a.get(keyBotUserID))
		if id == "" {
			id = strings.TrimSpace(f.existingBotUserID())
		}
		if id != "" {
			notes = append(notes, channelkinds.SummaryNote{
				Label: "Bot", Value: id + " — kept from the existing Channel",
			})
		}
	} else {
		notes = append(notes,
			channelkinds.SummaryNote{Label: "Bot token", Value: credmask.Mask(token)},
			channelkinds.SummaryNote{Label: "Slack team", Value: a.get(keyTeamName)},
			channelkinds.SummaryNote{Label: "Bot", Value: fmt.Sprintf("@%s (%s)", a.get(keyBotUserName), a.get(keyBotUserID))},
		)
	}

	notes = append(notes, channelkinds.SummaryNote{
		Label: "Slack channel", Value: strings.TrimSpace(a.get(keyDestinationChannelID)),
	})

	name := f.channelNameFrom(a)
	if f.existing != nil {
		name += " — updated in place"
	}
	return append(notes, channelkinds.SummaryNote{Label: "Channel", Value: name})
}

// unansweredErr is the refusal for a State that reached Result without an
// answer the manifests need. It names the key so a caller seeding State from
// flags is told which flag was missing.
func unansweredErr(key string) error {
	return fmt.Errorf("slack monitoring wizard: %q was not answered", key)
}

// monitoringNotes is the guidance the CLI prints after applying.
func monitoringNotes(channelName, channelID, namespace string, replaced, keptToken bool) []string {
	verb := "Created"
	if replaced {
		verb = "Updated"
	}
	notes := []string{
		fmt.Sprintf("%s monitoring Channel %q in namespace %q. Framework events post to Slack channel %q.", verb, channelName, namespace, channelID),
		"After apply, invite the bot to the Slack channel (/invite @<bot>), then watch `oap channel show " + channelName + "` for Valid=True.",
	}
	if !keptToken {
		notes = append(notes, "Save the bot-token in your password manager — it cannot be retrieved later from the Secret.")
	}
	return notes
}

// errMonitoringProvisionUnsupported is what a monitoring run gets if it
// answered "provision" — reachable only by seeding --answer slackapp=provision
// directly, since the enum this flow declares never offers that option.
var errMonitoringProvisionUnsupported = errors.New(
	"slack monitoring wizard: automatic Slack app creation is not offered for a monitoring channel; " +
		"pass --answer slackapp=true (you already have an app) or slackapp=false (show the manifest) instead")

// monitoringAppSetupSteps is delivered as an error message, so it opens by
// saying what happened.
const monitoringAppSetupSteps = `no Slack app with a bot token yet.

Create one, then run this again and paste the token.

  1. https://api.slack.com/apps → 'Create New App' → 'From scratch'. Name it
     something an operator will recognise in an alert, and pick your workspace.

  2. 'OAuth & Permissions' → 'Scopes' → 'Bot Token Scopes' → add chat:write.
     That is all a monitoring channel needs: it posts and never listens, so
     there is no Socket Mode and no app-level token.

  3. 'Install App' → 'Install to <Workspace>' → Allow, then copy the
     'Bot User OAuth Token' (xoxb-) shown after install.

  4. Invite the bot to the channel the events should land in:
     /invite @<bot-name>`

// monitoringTokenGuidance says where in Slack's UI the token is found.
const monitoringTokenGuidance = `Paste the bot token of the app that will post monitoring events.

  Bot User OAuth Token (xoxb-)
    your app → 'OAuth & Permissions' → 'Bot User OAuth Token', at the top of
    the page once the app is installed to the workspace.`

// monitoringDestinationGuidance says what is being asked for and where it is
// found in Slack's UI.
const monitoringDestinationGuidance = `Where should framework events be posted?

This is a fixed destination — a channel an operator watches — not an agent
channel: nobody talks to a monitoring channel, it only reports.

  Find the ID: open the channel → right-click its name → 'Copy link' → the
               last segment of the URL (e.g. C0123ABCDEF).
  Invite the bot to it first: /invite @<bot-name>`
