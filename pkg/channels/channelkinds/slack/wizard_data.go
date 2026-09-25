// pkg/channels/channelkinds/slack/wizard_data.go
//
// What `oap channel create --kind slack` ASKS for the AGENT flow: Inputs /
// Handoff / Resolve / Result (channelkinds.Wizard). The monitoring flow's
// own half is in wizard_monitoring.go; the manifests, the answer vocabulary
// and the operator-facing text both call are in wizard.go.
//
// ONE COST OF A DATA-SHAPED CONTRACT IS RECORDED HERE rather than worked
// around: Inputs is a single batch, asked before any of it is answered, so a
// step whose CONTENT is derived from an earlier answer cannot be interleaved
// into it. Slack has exactly one — generating the app manifest from the
// AgentClass and capability answers, handing it to the operator, and then
// taking the tokens the app they create issues. There is no question shape
// for "read this, go do something, come back", and HandoffSpec is
// browser-callback-shaped rather than a hold. What that costs is a SECOND
// RUN; see manualRouteRefusal, which is where the first one ends and the
// manifest is handed over.
//
// WHICH QUESTIONS ARE ASKED IS A DIFFERENT PROBLEM, AND IT IS SOLVED. A batch
// stated up front can still BRANCH on an answer inside it: a question carries
// oap.Question.AskWhen and is put to the operator only when an earlier answer
// opens it. That is what makes the three Slack-app routes reachable by
// CHOOSING one rather than only by declaring one with a flag — see
// agentCredentialQuestions. It buys nothing for the paragraph above, whose
// problem is not which question to ask but text that has to be READ between
// two of them.
package slack

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/slack/appprovision"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/wizardkeys"
	"github.com/authzed/openagentprimitives/pkg/platform/oap"
	"github.com/authzed/openagentprimitives/pkg/x/credmask"
)

// wizardAnswers is the read side of an answered run: three ways of asking the
// answer map one question, shared by both this kind's flows.
//
// Three accessors and not one, because the three readings are genuinely
// different and the difference is load-bearing. has distinguishes "answered
// with nothing" from "never asked" — the whole difference between a capability
// list the operator deliberately emptied and a question that was never posed,
// which would silently disable every default-on capability. all is the
// multi-value reading only the capability answer needs.
type wizardAnswers struct {
	get func(key string) string
	all func(key string) []string
	has func(key string) bool
}

// mapAnswers reads the answer map channelkinds.Wizard.Result is handed.
//
// all splits on "," because that is exactly how the CLI joins a multi-select
// answer back into the map (channelwizard.answersFrom) and how `--answer
// key=a,b,c` supplies one in the first place (channelwizard.splitList). The two
// halves of that round trip live in different packages, so this is a real
// contract between them and not an internal detail: a comma is the only
// separator either side uses.
func mapAnswers(answers map[string]string) wizardAnswers {
	return wizardAnswers{
		get: func(key string) string { return answers[key] },
		all: func(key string) []string { return splitAnswerList(answers[key]) },
		has: func(key string) bool { _, ok := answers[key]; return ok },
	}
}

// splitAnswerList reads a comma-joined multi-value answer back as its
// elements, trimming each and dropping the blanks — the same reading
// channelwizard.splitList gives an `--answer key=a, b,` on the way in, so a
// value that made a round trip through the map is not silently a different
// answer than the one that was checked.
func splitAnswerList(v string) []string {
	var out []string
	for _, part := range strings.Split(v, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// seededRoute reports the Slack-app route the flags already chose, and whether
// they chose one at all.
//
// The second return is what routeOfAnswer cannot give: an unanswered question
// resolves to routeManual there, because manual is the floor that always
// works, so "not yet asked" and "chose the manual route" come back the same.
// Inputs has to tell them apart: it shapes the question set around the manual
// route, and doing that to a run nobody has been asked yet would shape EVERY
// unseeded run.
func seededRoute(in channelkinds.WizardInput) (appRoute, bool) {
	if !wizardkeys.SeededHas(in, keyHasSlackApp) {
		return routeManual, false
	}
	return routeOfAnswer(in.Seeded[keyHasSlackApp]), true
}

// seededTokens reports whether both credentials arrived from flags.
//
// It is what makes a caller who provisioned once, saved both tokens, and
// re-runs with slackapp=provision still set not ask to provision again. It
// does NOT override the manual route — see agentInputs for why that asymmetry
// is deliberate.
func seededTokens(in channelkinds.WizardInput) bool {
	return wizardkeys.SeededHas(in, keyBotToken) && wizardkeys.SeededHas(in, keyAppToken)
}

// optionalAnswer is Question.Required's explicit false. Required is a *bool
// whose nil means REQUIRED, so an optional question needs a pointer to a real
// false rather than the zero value.
func optionalAnswer() *bool { b := false; return &b }

// Inputs states what this kind needs as data.
//
// It is one batch, asked before any of it is answered, which is what the three
// branches below are decided from instead: in.Monitoring picks the flow,
// seededRoute picks the agent flow's route when a flag already chose one, and
// in.NonInteractive says whether anyone is here to be asked at all.
//
// The single-batch cost is the package doc's subject. What it does NOT cost is
// a question the operator cannot answer: the manual route stops declaring the
// credential questions once a flag chooses it, and leaves them optional while
// it is still on the table, so nobody is ever asked to invent a token in order
// to be shown a manifest.
func (w *slackWizard) Inputs(ctx context.Context, in channelkinds.WizardInput) ([]oap.Question, error) {
	if in.Namespace == "" {
		return nil, errors.New("namespace is required")
	}
	if in.Monitoring {
		return monitoringFlowFor(in).inputs(in)
	}
	return agentInputs(ctx, in)
}

// Handoff sends the operator nowhere. Slack's one browser step — creating the
// app by hand — is not a callback: nothing comes back on a redirect, the
// operator returns with two tokens they type in. See HandoffSpec's own doc for
// the shape this would have to be.
func (w *slackWizard) Handoff(context.Context, channelkinds.WizardInput) (*channelkinds.HandoffSpec, error) {
	return nil, nil
}

// Resolve derives what only Slack can answer: who the bot token belongs to,
// and — on the provisioning route — the two tokens themselves.
//
// It reads w.authFactory and w.provisionClient, which are NOT the receiver
// state channelkinds.Wizard.Result forbids: they are live clients injected
// once by Kind.Wizard and never written by a run, and doing I/O is what this
// method is for. WHICH FLOW a run is comes from in.Monitoring, on this call,
// and never from anything an earlier one left behind.
func (w *slackWizard) Resolve(ctx context.Context, in channelkinds.WizardInput, answers map[string]string) (map[string]string, error) {
	if w.authFactory == nil {
		return nil, errors.New("slack wizard: no Slack client factory wired")
	}
	if in.Monitoring {
		// The factory is set on the flow rather than passed alongside it, so
		// there is no configuration in which the field is nil while the branch
		// is doing I/O — monitoringFlowFor leaves it nil deliberately for the
		// result path, and a future reader of f.authFactory would otherwise
		// find it nil on the data path only.
		f := monitoringFlowFor(in)
		f.authFactory = w.authFactory
		return f.resolve(ctx, mapAnswers(answers))
	}
	return w.agentResolve(ctx, in, mapAnswers(answers))
}

// Result reads the answers and produces the manifests, as a pure function of
// (in, answers).
//
// The monitoring branch is decided by in.Monitoring, and the branch's own
// state is rebuilt from in: under this contract Result may be called on a
// value no Inputs call ever touched, or in a different process entirely, so a
// Result that consulted a receiver field would build a monitoring run's
// manifests as an agent run's. See monitoringFlowFor.
func (w *slackWizard) Result(in channelkinds.WizardInput, answers map[string]string) (channelkinds.WizardOutput, error) {
	if in.Namespace == "" {
		return channelkinds.WizardOutput{}, errors.New("namespace is required")
	}
	a := mapAnswers(answers)
	if in.Monitoring {
		f := monitoringFlowFor(in)
		out, err := f.output(a)
		if err != nil {
			return channelkinds.WizardOutput{}, err
		}
		out.Summary = f.summary(a)
		return out, nil
	}
	// in.Role is read here, unlike NonInteractive/OperatorShell/WorkingDir:
	// those describe the CLIENT, and manifests that varied with them would be
	// unreproducible from their own answers, while the role describes the
	// CHANNEL and is exactly the sort of thing the manifests should depend on.
	out, err := agentOutput(in.Namespace, in.Role, a)
	if err != nil {
		return channelkinds.WizardOutput{}, err
	}
	summary, err := agentSummary(a)
	if err != nil {
		return channelkinds.WizardOutput{}, err
	}
	out.Summary = summary
	return out, nil
}

// --- the agent flow ---

// agentInputs states the agent flow's questions, in the order they are asked.
//
// The route decides which PAIR of credential questions the operator is asked
// for: a provisioning run is asked how to authenticate with Slack's
// app-configuration API; a run that already has an app is asked for the two
// tokens themselves; the manual route is asked for neither, because it ends
// with the manifest rather than with a Channel.
//
// WHERE THE ROUTE COMES FROM DECIDES HOW THE PAIR IS EXPRESSED, and that is
// the whole shape of this function:
//
//   - A FLAG settled it, so this call already knows which pair the run can use
//     and declares that one alone. `--answer` then accepts exactly the keys
//     this run can use, and the manual route declares no credential key at all
//     — which is what lets checkAnswerKeys refuse a stray `bot-token` beside
//     `slackapp=false`.
//   - NOBODY has settled it, which is every interactive run: the answer is the
//     operator's next one, and this batch is stated before it exists. Both
//     pairs are declared, each GATED on the route that uses it (AskWhen), so
//     every key stays seedable while only the pair the operator's own choice
//     selects is ever put to them.
//
// The gate is what this route lacked. Reading the route from the flags alone
// meant an interactive operator who chose "create the app for me" was asked
// for the bot and app tokens that choice exists to mint, and never asked how
// to authenticate with the API that mints them — the provisioning questions
// were unreachable except through `--answer slackapp=provision`.
func agentInputs(ctx context.Context, in channelkinds.WizardInput) ([]oap.Question, error) {
	route, decided := seededRoute(in)
	tokensSeeded := seededTokens(in)

	// Refused before anything is asked, because nothing that could be asked
	// would help: the run has no way to authenticate with Slack's
	// app-configuration API and nobody is here to be prompted for one.
	if in.NonInteractive && decided && route == routeProvision && !tokensSeeded {
		if err := provisionUnattendedRefusal(in); err != nil {
			return nil, err
		}
	}

	classQ, boundClass, unambiguous, err := wizardkeys.AgentClassQuestion(ctx, in, agentClassPrompt)
	if err != nil {
		return nil, err
	}

	qs := []oap.Question{
		classQ,
		{
			Name:        keyHasSlackApp,
			Type:        oap.QEnum,
			Prompt:      agentSlackAppPrompt,
			Description: agentSlackAppDescription,
			Enum:        routeValues(routeManual, routeHave, routeProvision),
			EnumLabels:  routeLabels(routeManual, routeHave, routeProvision),
			// routeManual first and as the default: it is the floor that
			// always works, so an unattended or fat-fingered accept must never
			// silently claim an app exists that does not.
			Default: string(routeManual),
		},
	}

	// P5-R16, and the reason precheckClass is not simply boundClass: with more
	// than one AgentClass in the namespace, boundClass is classes[0] — a
	// GUESS, the enum's pre-selection and nothing more. Pre-checking the
	// capability boxes from that class's grants and then applying them to
	// whichever class the operator actually picks would revoke a capability
	// the picked class never disabled, silently, on a bare accept. An empty
	// class reads the pre-check off the capability defaults instead; see
	// capabilityQuestion for why that is the floor rather than "check
	// nothing".
	precheckClass := ""
	if unambiguous {
		precheckClass = boundClass
	}
	capQ, err := capabilityQuestion(ctx, in, precheckClass)
	if err != nil {
		return nil, err
	}
	if capQ != nil {
		qs = append(qs, *capQ)
	}

	// A FLAG-CHOSEN manual route declares nothing further. Everything below
	// this line — the credentials, the Channel name — belongs to a run that
	// ENDS with a Channel, and a run that chose to create the Slack app by hand
	// does not: it ends in Resolve with the manifest, generated from exactly
	// the answers above.
	//
	// An operator who picks the manual route at the PROMPT is not this branch —
	// nothing is decided when this batch is stated — and is spared the same
	// questions by the gates instead: the credentials below select on the other
	// two routes, so the manual choice reaches none of them. The Channel name
	// is still asked and still used, by the come-back command the refusal
	// prints.
	//
	// tokensSeeded does NOT override this, unlike on the provisioning route,
	// and the asymmetry is deliberate. "Create one for me" plus two tokens is
	// a run that has already been provisioned and is being repeated; "show me
	// the manifest" plus two tokens is a contradiction, and the two readings
	// of it — print the manifest, or ignore the answer and build a Channel —
	// differ by whether the operator gets what they asked for. Refusing the
	// stray key (checkAnswerKeys does, since it is not declared here) is the
	// only reading that cannot silently do the other thing.
	if decided && route == routeManual {
		return qs, nil
	}

	credQs, err := agentCredentialQuestions(in, route, decided, tokensSeeded)
	if err != nil {
		return nil, err
	}
	qs = append(qs, credQs...)

	// WHERE THE AGENT'S WORK GOES, asked only of a run whose Channel has to
	// carry its own destination — see outputDestinationNeeded for which role
	// that is and why the bound AgentClass is not consulted.
	//
	// DECLARED UNCONDITIONALLY WITHIN THAT ROLE, and not gated behind the
	// Slack-app question the credentials above are gated behind. It is not a
	// credential: an operator taking the manual route has already reached the
	// end of their run by the time Resolve raises the manifest, and a run that
	// gets that far declared this key without asking anyone anything they
	// could not answer. Gating it on the two routes that finish would only
	// mean two AskWhen values to keep in step with a question that has the
	// same answer either way.
	//
	// REQUIRED, because a run that is asked at all cannot finish without it:
	// the Channel it produces is refused by its own controller
	// (ReasonChannelOutputDestinationMissing), so a blank is a mistake the
	// widget should refuse in place rather than something the cluster reports
	// minutes later. agentOutput repeats the check for a client that skipped
	// the questions entirely.
	if outputDestinationNeeded(in.Role) {
		qs = append(qs, oap.Question{
			Name:        keyDestinationChannelID,
			Type:        oap.QString,
			Prompt:      agentDestinationPrompt,
			Description: agentDestinationGuidance,
		})
	}

	// P5-R16: the derived Channel name is offered only when the AgentClass it
	// derives from is not a guess. Inputs runs before any answer exists, and
	// questionscreen bakes a Default into a closure over this one call's
	// value — it never re-consults an answer a later question records. A wrong
	// name can be silently accepted with a bare Enter; an omitted one cannot,
	// because a required question with no Default fails closed.
	var nameDefault any
	if unambiguous {
		nameDefault = defaultChannelNamePrefix + boundClass
	}
	return append(qs, oap.Question{
		Name:        wizardkeys.KeyChannelName,
		Type:        oap.QString,
		Prompt:      wizardkeys.ChannelNamePrompt,
		Description: agentChannelNameHint,
		Default:     nameDefault,
	}), nil
}

// provisionUnattendedRefusal is why a run nobody is watching cannot have oap
// create the app for it, in this kind's own words, raised from Inputs before
// anything is asked.
//
// The distinction it draws is the useful half, and the reason this is not left
// to the client's generic "supply it via flags": a source that CANNOT serve an
// unattended run at all is a different problem from a source that can but was
// given no token. Telling a caller who picked the Slack CLI to supply a
// configuration token would send them to generate one this run then discards.
func provisionUnattendedRefusal(in channelkinds.WizardInput) error {
	if why := unattendedReason(wizardkeys.SeededAnswer(in, keyTokenSource)); why != "" {
		return errors.New(why + ". For an unattended run choose the paste source instead: " +
			"--answer " + keyTokenSource + "=" + appprovision.KeyPaste +
			" --answer " + keyConfigToken + "=<token>")
	}
	if wizardkeys.SeededHas(in, keyConfigToken) {
		return nil
	}
	return errors.New("provisioning a Slack app needs an app-configuration token: generate one at " +
		"https://api.slack.com/apps under \"Your App Configuration Tokens\" and supply it with " +
		"--answer " + keyConfigToken + "=<token>. Note that it lands in your shell history.")
}

// routeValues is the Enum for the Slack-app question: the strings the answer
// is STORED as, in the order the routes are offered. They are frozen —
// `--answer slackapp=true` and `slackapp=false` already work — which is why
// what the operator READS is carried separately, by routeLabels.
func routeValues(routes ...appRoute) []string {
	out := make([]string, 0, len(routes))
	for _, r := range routes {
		out = append(out, string(r))
	}
	return out
}

// routeLabels is the EnumLabels for the same question, positionally paired
// with routeValues, so the operator picks between "Show me the manifest — I'll
// create it myself" and the other two rather than between `false`, `true` and
// `provision`. Both flows build their question from the same two helpers, so
// neither can label a route differently from the other — the reason
// routeLabel is centralized in the first place.
func routeLabels(routes ...appRoute) []string {
	out := make([]string, 0, len(routes))
	for _, r := range routes {
		out = append(out, routeLabel(r))
	}
	return out
}

// capabilityQuestion states which capabilities the bound agent should have,
// pre-checked from what that AgentClass already grants.
//
// nil when this kind offers none — the same guard that makes
// agentOutput's requirement on the answer
// conditional too. A QResourceList with no Enum renders as free text, so
// declaring one here would ask for a typed list of options that do not exist.
//
// boundClassName MUST be empty unless the AgentClass is known — see the call site,
// which passes "" whenever the class is still the enum's pre-selection rather
// than an answer. An empty class reads the pre-check off the capability
// DEFAULTS alone (activeCapabilities' nil-class branch, the same reading an
// offline dry-run gets).
//
// That is P5-R16 applied to a widget the ruling's "omit the default" does not
// fit: an omitted pre-check is not "no suggestion" for a multi-select, it is
// every default-on capability shown UNCHECKED, and confirming that would
// revoke them. The defaults are the least-wrong floor. Pre-checking from a
// guessed class is the genuinely wrong one, and worse than the ruling's own
// example: the answer is applied to whichever class the operator picks
// (capabilityPatch writes an explicit enabled:false for every offered
// capability), so a guess taken from classes[0] would revoke, on the class
// they DID pick, a capability that class never disabled.
//
// Residual gap, reported rather than papered over: with the class unknown, a
// bound AgentClass that has explicitly turned a default-on capability off
// shows it checked.
func capabilityQuestion(ctx context.Context, in channelkinds.WizardInput, boundClassName string) (*oap.Question, error) {
	options := slackCapabilityOptions()
	if len(options) == 0 {
		return nil, nil
	}
	class, err := boundClass(ctx, in.K8s, in.Namespace, boundClassName)
	if err != nil {
		return nil, err
	}
	checked, err := activeCapabilities(options, class)
	if err != nil {
		return nil, err
	}
	// The labels carry each capability's SCOPE COST, which is the information
	// the operator is consenting to and which the bare capability name — the
	// stored answer, and the `--answer capabilities=` spelling — cannot show.
	// Same pairing the checkbox list has always had (capabilityOption.label).
	labels := make([]string, 0, len(options))
	for _, o := range options {
		labels = append(labels, o.label())
	}
	return &oap.Question{
		Name:        keyCapabilities,
		Type:        oap.QResourceList,
		Prompt:      capabilityPrompt,
		Description: capabilityGuidance(options),
		Enum:        optionCapabilityNames(options),
		EnumLabels:  labels,
		Default:     checked,
	}, nil
}

// agentCredentialQuestions is the credential half of the batch: which
// questions are declared, and which of them the route the operator ends up on
// will actually put to them.
//
// A DECIDED run declares the one pair its route uses, ungated. An UNDECIDED
// one declares both, each gated on the route that selects it — see agentInputs
// for why the two readings differ and what the gate buys.
//
// The tokensSeeded half of the first condition is what keeps a repeat run
// honest: a caller who provisioned once, saved both tokens and re-runs with
// slackapp=provision still set is not asking to provision again, so they are
// asked for neither. It is only consulted on the decided branch, because
// tokens seeded without a route settle nothing about which one the operator
// will pick.
func agentCredentialQuestions(in channelkinds.WizardInput, route appRoute, decided, tokensSeeded bool) ([]oap.Question, error) {
	if decided {
		if route == routeProvision && !tokensSeeded {
			return provisionCredentialQuestions(in, oap.AskWhen{})
		}
		return tokenQuestions(oap.AskWhen{}), nil
	}

	// Both pairs, each behind the route that uses it. The manual route selects
	// neither, so an operator who asks to be shown the manifest is asked for no
	// credential at all — the same thing a flag-settled manual run gets, now
	// reached by choosing it rather than only by declaring it.
	provisionQs, err := provisionCredentialQuestions(in, askWhenRoute(routeProvision))
	if err != nil {
		return nil, err
	}
	return append(tokenQuestions(askWhenRoute(routeHave)), provisionQs...), nil
}

// askWhenRoute gates a question on one answer to the Slack-app question.
//
// One helper rather than the literal at each site because the gate's Question
// must name keyHasSlackApp exactly and its value must be the STORED spelling of
// the route (`false`/`true`/`provision`, the ones `--answer` already depends
// on) rather than the label the operator reads. Two hand-written literals are
// two chances to gate on a label and silently hide a question forever.
func askWhenRoute(r appRoute) oap.AskWhen {
	return oap.AskWhen{Question: keyHasSlackApp, In: []string{string(r)}}
}

// tokenQuestions is the pair a run that already has a Slack app supplies.
//
// Both are REQUIRED unconditionally, and that is the gate's doing: they are
// only ever put to an operator whose route needs them, so a blank one is a
// mistake the widget should refuse in place rather than something Resolve
// discovers later. The fail-closed checks do not move either way — agentResolve
// rejects a blank or wrong-prefixed token on every route that needs one, and
// agentOutput rejects it again.
func tokenQuestions(gate oap.AskWhen) []oap.Question {
	return []oap.Question{
		{
			Name:   keyBotToken,
			Type:   oap.QSecret,
			Prompt: botTokenPrompt,
			// tokenGuidance covers BOTH tokens, so it sits on the first of
			// them rather than being split or repeated.
			Description: tokenGuidance,
			AskWhen:     gate,
		},
		{
			Name:    keyAppToken,
			Type:    oap.QSecret,
			Prompt:  appTokenPrompt,
			AskWhen: gate,
		},
	}
}

// provisionCredentialQuestions is how a run that asked oap to create the app
// authenticates with Slack's app-configuration API.
//
// The enum it offers is a function of the CLIENT as well as the machine.
// in.OperatorShell is what drops a source that only works at the operator's
// own terminal — see availableSources, and channelkinds.WizardInput's own doc
// for why a probe of the serving host is not the same question.
//
// gate is the zero AskWhen when a flag already chose this route (these are then
// the only credential questions declared) and the provisioning route's own gate
// when the operator has yet to choose, so the pair is declared but put only to
// someone who picks it.
func provisionCredentialQuestions(in channelkinds.WizardInput, gate oap.AskWhen) ([]oap.Question, error) {
	available := availableSources(in.OperatorShell)
	if len(available) == 0 {
		return nil, errors.New("no way to authenticate with Slack's app-configuration API is available on this machine")
	}
	sources := make([]string, 0, len(available))
	labels := make([]string, 0, len(available))
	for _, src := range available {
		sources = append(sources, src.Key())
		labels = append(labels, src.Label())
	}
	return []oap.Question{
		{
			Name:   keyTokenSource,
			Type:   oap.QEnum,
			Prompt: tokenSourcePrompt,
			// provisionGuidance is the one note above this screen's fields, so
			// it sits on the first of them — same rule as tokenGuidance above.
			Description: provisionGuidance,
			// The stored answer is the source KEY, which appprovision.SourceFor
			// resolves and `--answer app-token-source=paste` names; what the
			// operator reads is the source's own Label().
			Enum:       sources,
			EnumLabels: labels,
			Default:    sources[0],
			AskWhen:    gate,
		},
		{
			Name:    keyConfigToken,
			Type:    oap.QSecret,
			Prompt:  configTokenPrompt,
			AskWhen: gate,
			// Optional because a source that mints its own token ignores the
			// value, and which source that will be is not known until this
			// batch is answered.
			//
			// It buys the INTERACTIVE slack-cli case, where the operator picks
			// that source and presses Enter here. It buys nothing unattended,
			// where the fail-closed driver refuses any question it is shown
			// whatever its Required says — which is precisely why an
			// unattended provisioning run is refused earlier, by
			// provisionUnattendedRefusal, with a message that tells the
			// operator which flag actually helps.
			Required: optionalAnswer(),
		},
	}, nil
}

// agentResolve mints the tokens when this run asked for an app to be created,
// then asks Slack who the bot token belongs to.
//
// The auth.test call is not skippable: a Channel with an empty BotUserID
// cannot recognise its own messages, and Resolve is the one step of this
// contract that may do the I/O it takes to find out (P5-R17).
//
// KNOWN GAP, reported rather than papered over: a provisioning FAILURE ENDS
// THE RUN. There is no way, once the question set has been asked and answered,
// to detour into "create the app by hand instead" and carry the same run on
// with the tokens that produces — the manifest is derived from answers the
// operator has already given, but the questions that would take the resulting
// tokens are behind us. The error names the app if one was created (see
// provisionSlackApp's orphan return), and the operator re-runs with
// `--answer slackapp=false` to get the manifest.
func (w *slackWizard) agentResolve(ctx context.Context, in channelkinds.WizardInput, a wizardAnswers) (map[string]string, error) {
	derived := map[string]string{}

	route := routeOfAnswer(a.get(keyHasSlackApp))
	bot := strings.TrimSpace(a.get(keyBotToken))
	app := strings.TrimSpace(a.get(keyAppToken))

	// The manual route ends the run, unconditionally — an operator who asked
	// for the manifest gets the manifest. See Inputs for why two stray tokens
	// do not override it the way they override the provisioning route.
	if route == routeManual {
		return nil, manualRouteRefusal(in.WorkingDir, in.Namespace, in.Role, a)
	}

	// P5-R21: refused in FRONT of the provisioning step below, which creates
	// and installs a real Slack app that no Slack API will list afterwards.
	// agentOutput refuses the same value, but by then the app exists — so a
	// mistyped channel ID would cost the operator an app nothing can find
	// again, for an answer that was wrong before the run touched Slack at all.
	//
	// Not repeated for a role that was never asked: agentOutput owns that
	// case, and there is nothing irreversible in front of it to protect.
	if outputDestinationNeeded(in.Role) {
		if err := checkSlackChannelID("slack wizard: "+keyDestinationChannelID, a.get(keyDestinationChannelID)); err != nil {
			return nil, err
		}
	}

	if route == routeProvision && (bot == "" || app == "") {
		// in.NonInteractive is the fail-closed half of the rule
		// provisionUnattendedRefusal reads from Inputs. That check is the
		// friendly one and runs before anything is asked; this one catches a
		// client that never called Inputs, and keeps a source that waits on a
		// person from being handed a terminal nobody is at.
		token, err := provisionConfigToken(ctx, provisionSourceKey(a), a.get(keyConfigToken), in.NonInteractive, in.OperatorShell)
		if err != nil {
			return nil, err
		}
		// The orphan app ID is deliberately discarded HERE and only here: on
		// this path it is already inside err's own sentence ("the Slack app %q
		// (%s) was created but could not be installed"), and there is no
		// manual-fallback screen left to send the operator to, which is the
		// one thing the separate return value exists to feed.
		installed, _, err := provisionSlackApp(ctx, w.provisionClient,
			strings.TrimSpace(a.get(keyAgentClass)), a.all(keyCapabilities), token)
		if err != nil {
			return nil, err
		}
		bot, app = installed.BotToken, installed.AppLevelToken
		derived[keyBotToken] = bot
		derived[keyAppToken] = app
		derived[keyAppID] = installed.AppID
	}

	// Checked before the round trip so a token pasted into the wrong field is
	// named as such rather than coming back as Slack's "invalid_auth", which
	// says nothing about the commonest mistake there is. agentOutput repeats
	// the check for a client that skipped this step entirely.
	if err := checkTokenPrefix("bot", botTokenPrefix, bot); err != nil {
		return nil, err
	}
	if err := checkTokenPrefix("app-level", appTokenPrefix, app); err != nil {
		return nil, err
	}

	auth, err := authTest(ctx, w.authFactory, bot)
	if err != nil {
		return nil, err
	}
	derived[keyBotUserID] = auth.UserID
	derived[keyTeamName] = auth.Team
	derived[keyBotUserName] = auth.User
	return derived, nil
}

// dataComeBack is how the manual route tells the operator the tokens
// get back into oap: there is no next screen, because the run has ended — so
// it spells out the command that finishes the job rather than naming a flag
// and leaving them to reconstruct the rest.
//
// The answers this run already gave are written INTO that command. They are
// not carried anywhere else: the run ended, nothing was applied, and a second
// run starts from nothing, so an operator told only "re-run with
// slackapp=true" would be asked for the AgentClass and the capabilities again
// and could silently pick differently — which would mean an app whose scopes
// no longer match the Channel that uses it.
//
// Kept inside the note's column budget for the same reason everything else in
// appSetupSteps is; the two token placeholders are what keep the lines short.
//
// capabilitiesAnswered, not a non-empty capabilities string, is what decides
// whether that line is emitted — see selectedCapabilityAnswer for why an
// operator who unchecked everything must get `--answer capabilities=` rather
// than no line at all.
// namespace is written in for the same reason the answers are: the second run
// is a whole new invocation, and an operator who reached this one with
// `-n <ns>` and pastes a command without it creates the Channel somewhere the
// agent is not. It is the same rule `oap agent install`'s own finishing
// command follows, and the two now spell it the same way.
// role and destination are written in for the same reason, and are the two
// halves of one Channel the second run would otherwise get wrong. A command
// without `--role` produces a Channel on ChannelSpec.Role's `both` default,
// which is deliberately not an output-binding candidate; one without the
// destination produces a role=output Channel its own controller refuses
// (ReasonChannelOutputDestinationMissing). Between them that is the whole
// defect this question exists to close, reconstructed by an operator following
// the instructions.
//
// destination is a PLACEHOLDER when the run never collected one — a
// flag-settled manual route declares no questions past the capabilities, so
// there is no answer to carry — matching the two token placeholders beside it.
func dataComeBack(agentClass, capabilities string, capabilitiesAnswered bool, channelName, namespace, role, destination string) string {
	var b strings.Builder
	b.WriteString("run this command again to finish, with the two tokens\n")
	b.WriteString("the app gives you:\n\n")
	b.WriteString("  oap channel create --kind slack \\\n")
	b.WriteString("    --answer slackapp=true \\\n")
	if agentClass != "" {
		b.WriteString("    --answer agentclass=" + agentClass + " \\\n")
	}
	if capabilitiesAnswered {
		b.WriteString("    --answer capabilities=" + capabilities + " \\\n")
	}
	if outputDestinationNeeded(role) {
		if destination == "" {
			destination = "<C0…>"
		}
		b.WriteString("    --answer " + keyDestinationChannelID + "=" + destination + " \\\n")
	}
	b.WriteString("    --answer bot-token=<xoxb-…> \\\n")
	b.WriteString("    --answer app-token=<xapp-…>")
	if role != "" {
		b.WriteString(" \\\n    --role " + role)
	}
	if channelName != "" {
		b.WriteString(" \\\n    --name " + channelName)
	}
	if namespace != "" {
		b.WriteString(" \\\n    --namespace " + namespace)
	}
	return b.String()
}

// manualRouteRefusal ends a run that chose to create the Slack app by hand,
// handing over the generated manifest and the steps around it as the error.
//
// ENDING THE RUN IS THE DESIGN, not a shortfall of it. A single up-front batch
// of oap.Questions cannot hold anyone mid-flow, because this text is derived
// from answers (the AgentClass names the app, the capability answer decides
// its scopes) that do not exist when Inputs is called. The three ways out
// were: show nothing and ask for tokens the operator does not have (silent,
// and the run then dead-ends at a required question), invent a contract
// affordance for a mid-run hold, or end the run with the manifest in hand.
// The monitoring flow already answers the same question the same way (see
// monitoringAppSetupSteps), so this is the established shape here rather than
// a new one.
//
// The cost, stated plainly: creating a Slack app by hand takes two runs. The
// manifest is the same manifest, generated from the same answers, requesting
// the same scopes, and it is still written to disk.
//
// THE FILE IS THE POINT, not a nicety. This text is the whole product of the
// run and it is delivered as an error, so it lands in scrollback that the next
// command scrolls away and that a `2>/dev/null` never sees at all; the copy on
// disk is what the operator still has when they come back.
//
// dir is WizardInput.WorkingDir, and an empty one means the client offered
// nowhere to write — a server rendering this wizard, for one. Nothing is
// written then and the "A copy is saved at" block is simply absent; the
// manifest itself is in this message either way. A write that is ATTEMPTED and
// fails is a different case and says so, because SaveManifest fails soft and
// MessageLines reports it.
func manualRouteRefusal(dir, namespace, role string, a wizardAnswers) error {
	agentClass := strings.TrimSpace(a.get(keyAgentClass))
	capabilities, capabilitiesAnswered := selectedCapabilityAnswer(a)
	manifest := appManifestFor(agentClass+botDisplayNameSuffix, selectedFeaturesFrom(a.all(keyCapabilities)))

	var savedLines string
	if dir != "" {
		savedLines = SaveManifest(dir, agentClass, manifest).MessageLines()
	}

	comeBack := dataComeBack(agentClass, capabilities, capabilitiesAnswered,
		strings.TrimSpace(a.get(wizardkeys.KeyChannelName)), strings.TrimSpace(namespace),
		strings.TrimSpace(role), strings.TrimSpace(a.get(keyDestinationChannelID)))
	return errors.New(appSetupSteps(manifest, comeBack, savedLines, ""))
}

// selectedCapabilityAnswer re-joins the capability answer the way `--answer
// capabilities=` spells it, so the command the refusal prints can be pasted
// back verbatim, and reports whether the question was ANSWERED at all.
//
// The second return is the whole point. "Answered with nothing" and "never
// asked" are different runs and must produce different commands: an operator
// who deliberately unchecked every capability, told only to re-run with the
// AgentClass, gets a second run that re-defaults to every default-on
// capability — and so a Channel declaring more than the app's manifest
// requested. That drift is exactly what dataComeBack exists to prevent, and it
// drifts in the permissive direction, which is the worse one. `--answer
// capabilities=` round-trips correctly: channelwizard.splitList reads it as the
// empty list, and State.Has stays true.
func selectedCapabilityAnswer(a wizardAnswers) (string, bool) {
	if !a.has(keyCapabilities) {
		return "", false
	}
	return strings.Join(a.all(keyCapabilities), ","), true
}

// provisionSourceKey is the token source this run chose, defaulting to the
// paste source, for a run that reached it
// without one.
func provisionSourceKey(a wizardAnswers) string {
	if key := strings.TrimSpace(a.get(keyTokenSource)); key != "" {
		return key
	}
	return appprovision.KeyPaste
}

// agentSummary is what the run DECIDED, in the order the operator decided it:
// the AgentClass, the capabilities, the app if this run created one, the two
// tokens, who Slack says they belong to, the Channel.
//
// This kind runs no code of its own while the questions are being answered, so
// every line here has to be stated by Result or it is never written at all —
// see channelkinds.WizardOutput.Summary.
//
// BOTH TOKENS ARE MASKED, and that is the point of this function rather than
// a detail of it: a SummaryNote.Value reaches plain scrollback verbatim, the
// client filters nothing, and the summary is the block deliberately left
// behind after the alt-screen is released. A dropped credmask.Mask here puts a
// live bot token in the operator's terminal history with nothing else in the
// system to catch it.
func agentSummary(a wizardAnswers) ([]channelkinds.SummaryNote, error) {
	notes := []channelkinds.SummaryNote{
		{Label: "AgentClass", Value: strings.TrimSpace(a.get(keyAgentClass))},
	}

	if options := slackCapabilityOptions(); len(options) > 0 {
		chosen, err := canonicalCapabilities(options, a.all(keyCapabilities))
		if err != nil {
			return nil, err
		}
		value := "none"
		if len(chosen) > 0 {
			value = strings.Join(chosen, ", ")
		}
		notes = append(notes, channelkinds.SummaryNote{Label: "Capabilities", Value: value})
	}

	// Present only for a run that created the app, which is the same signal
	// postSetupNotes reads: keyAppID is derived only by a successful
	// provisioning install, and nothing else sets it.
	if appID := strings.TrimSpace(a.get(keyAppID)); appID != "" {
		notes = append(notes, channelkinds.SummaryNote{
			Label: "Slack app", Value: "created and installed (" + appID + ")",
		})
	}

	notes = append(notes,
		channelkinds.SummaryNote{Label: "Bot token", Value: credmask.Mask(strings.TrimSpace(a.get(keyBotToken)))},
		channelkinds.SummaryNote{Label: "App token", Value: credmask.Mask(strings.TrimSpace(a.get(keyAppToken)))},
		channelkinds.SummaryNote{Label: "Slack team", Value: a.get(keyTeamName)},
		channelkinds.SummaryNote{Label: "Bot", Value: fmt.Sprintf("@%s (%s)", a.get(keyBotUserName), a.get(keyBotUserID))},
	)

	// Present only for a run that was asked where to post — where the agent's
	// work will appear is one of the decisions an operator most needs to read
	// back, and the summary is the block deliberately left in scrollback.
	// Absent, rather than blank, on a run whose replies follow the inbound:
	// "Slack channel:" with nothing after it describes no decision.
	if destination := strings.TrimSpace(a.get(keyDestinationChannelID)); destination != "" {
		notes = append(notes, channelkinds.SummaryNote{Label: "Slack channel", Value: destination})
	}

	return append(notes,
		channelkinds.SummaryNote{Label: "Channel", Value: strings.TrimSpace(a.get(wizardkeys.KeyChannelName))},
	), nil
}
