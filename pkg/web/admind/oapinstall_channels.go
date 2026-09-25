// The channel half of admind's install endpoint: what a bundle's
// requires.channels means for the form the admin UI renders.
//
// `oap agent install` and this endpoint answer the same two questions about a
// declared channel — is one already there, and which answers can this install
// supply without asking — and both get their answer from
// pkg/platform/oap/channelplan, once. What is HERE is the admind-specific
// part: turning one plan into a row a browser can render, and deciding which
// verdicts are a form at all.
//
// WHY A SEEDED ANSWER IS NOT A PRE-FILLED DEFAULT. cmd/oap's renderer
// (channelwizard.renderQuestions) DROPS a seeded question rather than
// rendering it with the value in place, and this does the same. The difference
// is not cosmetic: a Default is editable, and two of the three values a plan
// seeds are the bundle's, not the operator's. The declared Channel name is
// what every other bundled CR was written against — a bundled AgentIdentity
// reads "<name>-creds", and the wizard names the Secret it writes after the
// Channel — so an operator who edits it in a form gets a Channel whose
// credentials the agent cannot find, which is the defect
// channelplan.LintRequiredChannels exists to catch and which a form field
// bypasses entirely, because the lint reads the bundle and not the form. The
// AgentClass comes from the bundle's own agent, of which there is exactly one.
// "Pre-seeding is not silent" is satisfied by SHOWING what was decided and
// where it came from, which is what oapInstallChannel.Seeded is.
package admind

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/registry"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/wizardrun"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/platform/oap"
	"github.com/authzed/openagentprimitives/pkg/platform/oap/channelplan"
	"github.com/authzed/openagentprimitives/pkg/platform/oap/install"
)

// The Status values one oapInstallChannel can carry. A closed set, because the
// UI branches on it: only channelFormAsk means "render these fields", and
// every other value means "this channel will not be set up by this form", with
// Reason saying why and Remedy saying what does work.
const (
	// channelFormAsk: this form can collect what the kind needs. Questions
	// carries the fields; Seeded says what was decided without asking.
	channelFormAsk = "ask"
	// channelFormAlreadyWired: a Channel of this name is already here and is
	// already this agent's. Nothing to do, which is what makes re-installing
	// safe.
	channelFormAlreadyWired = "alreadyWired"
	// channelFormConflict: a Channel of this name exists and is NOT ours —
	// another kind, or bound to another agent (channelplan B-R11). Rendering a
	// form for it would offer to create something that cannot be created.
	channelFormConflict = "conflict"
	// channelFormUnknown: the planner never got to look, so "not wired" is the
	// absence of an answer rather than an answer. See ChannelPlan.WiringKnown.
	channelFormUnknown = "unknown"
	// channelFormUnavailable: there is no setup flow this build can offer for
	// this kind at all — it is not registered, its wizard refuses (see
	// channelkinds.UnavailableWizard), or what it declared is not renderable.
	channelFormUnavailable = "unavailable"
	// channelFormHandoff: this form can collect what the kind needs up front,
	// AND the setup then sends the operator to an external service in a
	// browser. Questions carries the up-front fields exactly as for
	// channelFormAsk; what differs is where they are submitted — the handoff
	// route rather than the setup route — and that the fallback questions the
	// round trip does not answer are asked afterwards, by the callback.
	channelFormHandoff = "handoff"
)

// oapInstallChannel is one channel the bundle declares, as the install form
// needs it.
//
// It carries a field's SHAPE and never an answer, exactly like the manifest
// questions beside it, so the whole body stays safe to log — see Seeded for
// the one place a VALUE appears and the rule that keeps it harmless.
type oapInstallChannel struct {
	// AgentPath addresses the dependency node that owns this channel. It is
	// omitted for root channels to preserve the original wire shape.
	AgentPath string `json:"agentPath,omitempty"`
	// Name is the Channel CR's name, as the bundle declared it. It is not a
	// question: other bundled CRs are already written against it.
	Name string `json:"name"`
	// Kind is the registered channel kind ("slack", "github", …).
	Kind string `json:"kind"`
	// Role is the declared ChannelSpec.Role. No kind's wizard asks for one; it
	// reaches the Channel from the declaration.
	Role string `json:"role"`
	// Purpose is the bundle author's prose saying what this channel is for,
	// shown beside the fields so the operator knows which of the agent's
	// channels they are answering for.
	Purpose string `json:"purpose,omitempty"`

	// Status is one of the channelForm* values above.
	Status string `json:"status"`

	// SetupToken is the opaque handle the UI submits this channel's answers
	// against (channelSetupPath). Present only for a row this admind can act
	// on, so a UI that has no token has no button either.
	//
	// It is what stands in for the declaration itself: the Channel name and the
	// AgentClass reach the setup route from the server's own copy of this row,
	// never from the form, which is what makes the seeded answers above
	// unforgeable rather than merely unrendered (B-R13).
	SetupToken string `json:"setupToken,omitempty"`

	// Questions is what the operator still has to answer, in the kind's own
	// declaration order — the full typed schema, and never an answer value.
	// Present only for channelFormAsk. A question this install already
	// answered is NOT here in any form: see Seeded.
	Questions []oap.Question `json:"questions,omitempty"`

	// Seeded is what this install decided on the operator's behalf, with where
	// each value came from. Reported so the decision is visible, and
	// deliberately NOT offered as an editable Question default.
	Seeded []oapInstallChannelSeed `json:"seeded,omitempty"`

	// NotSeeded is a value this install TRIED to pre-fill and could not, and
	// why — the matching question is in Questions, and this is the whole of
	// what the operator gets about the failure.
	//
	// A separate list from Seeded rather than more entries in it, for the
	// reason ChannelPlan splits the same two maps: rendering "could not read
	// ConfigMap … forbidden" under a "came from" heading labels a failure as a
	// provenance note.
	NotSeeded []oapInstallChannelNote `json:"notSeeded,omitempty"`

	// Reason is why Status is not channelFormAsk, in the words of whatever
	// refused — a kind's own sentence, the conflict the planner found, or this
	// endpoint's.
	Reason string `json:"reason,omitempty"`
	// Remedy is what the operator can do about it, which is not always the
	// same command: a name held by somebody else's Channel has to be resolved
	// before any command could succeed.
	Remedy string `json:"remedy,omitempty"`
}

// oapInstallChannelSeed is one answer this install supplied, and its source.
type oapInstallChannelSeed struct {
	// Key is the answer key, spelled the way the kind's Question names it.
	Key string `json:"key"`
	// Value is what was decided — EMPTY when the question it answers is
	// secret-typed, in which case Redacted is set. Nothing the planner seeds
	// today is a credential (it seeds a declared name, the bundle's own
	// AgentClass, and a URL webd publishes), but this body is logged, and a
	// property that holds only because of what a different package currently
	// happens to do is one edit away from not holding.
	Value string `json:"value,omitempty"`
	// Source is where the value came from, in prose. Pre-seeding must not be
	// silent: a value taken from a ConfigMap the operator has never seen,
	// applied without a word, is impossible to debug when it is wrong.
	Source string `json:"source,omitempty"`
	// Redacted says Value was withheld because the answer is secret-typed,
	// distinguishing it from a seed that genuinely had nothing to show.
	Redacted bool `json:"redacted,omitempty"`
}

// oapInstallChannelNote is one answer this install could not supply, and why.
type oapInstallChannelNote struct {
	Key    string `json:"key"`
	Reason string `json:"reason"`
}

// channelWizardLookup resolves a declared kind to its wizard. registryWizard
// is the only production implementation; it is a parameter so a row can be
// built for a wizard that misbehaves — an unrenderable question, a refusal, a
// handoff — without registering a kind into the process-wide registry.
type channelWizardLookup func(kind string) (channelkinds.Wizard, bool)

// registryWizard is the production lookup: the channel-kind registry this
// binary linked. internal/cmd/operator, which hosts admind, blank-imports
// every kind.
func registryWizard(kind string) (channelkinds.Wizard, bool) {
	k, ok := registry.Get(kind)
	if !ok {
		return nil, false
	}
	return k.Wizard(), true
}

// declaredChannelForm is the channel block for one bundle: a row per declared
// channel, plus any warning raised working them out.
//
// The warning is how a failure here reaches the operator. The channels are an
// ADDITION to a response that is already a 400 about something else, so a
// planning failure must not replace the answer they are waiting for — but it
// must not vanish either, so it is logged with context AND stated on the
// response.
//
// instanceName is the install's --name. It is passed through rather than
// zeroed: PlanChannels refuses it alongside a declared channel, and a caller
// that quietly told the planner there was no name would be hiding the very
// combination that refusal exists for. handleOapInstall refuses it earlier,
// before anything is applied, so in practice this arm is the backstop.
func (a *Admind) declaredChannelForm(ctx context.Context, b *oap.Bundle, namespace, instanceName string, owner identity.CanonicalUserID) (rows []oapInstallChannel, warnings []string) {
	if b == nil || b.Manifest == nil || len(b.Manifest.Requires.Channels) == 0 {
		return nil, nil
	}

	// surface reports a failure BOTH ways — a log line an operator can grep
	// and a sentence on the response they are actually waiting for — because
	// either alone is the silent half of it.
	surface := func(err error) []string {
		a.cfg.Logger.Info("admind: could not work out what this bundle's declared channels need",
			"namespace", namespace, "name", instanceName, "err", err.Error())
		return []string{"the channels this bundle declares could not be inspected, so this form does not show them: " + err.Error()}
	}

	agentClass, err := bundleAgentClassName(b)
	if err != nil {
		return nil, surface(err)
	}
	plans, err := channelplan.PlanChannels(ctx, a.cfg.K8s, b, namespace, agentClass, instanceName)
	if err != nil {
		return nil, surface(err)
	}
	return a.declaredChannelRows(ctx, plans, namespace, agentClass, owner, nil, nil, nil)
}

func (a *Admind) declaredChannelFormForNode(ctx context.Context, node install.NodeContext, owner identity.CanonicalUserID, bindingBase *channelSetupBinding) (planned install.PlannedChannels, rows []oapInstallChannel, warnings []string, err error) {
	if node.Bundle == nil || node.Bundle.Manifest == nil || len(node.Bundle.Manifest.Requires.Channels) == 0 {
		return install.PlannedChannels{}, nil, nil, nil
	}
	mapped := *node.Bundle
	manifest := *node.Bundle.Manifest
	requires := manifest.Requires
	requires.Channels = slices.Clone(requires.Channels)
	credentialNames := make(map[string]string, len(requires.Channels))
	for i := range requires.Channels {
		logical := strings.TrimSpace(requires.Channels[i].Name)
		physical := node.ResourceNames["Channel/"+logical]
		if physical == "" {
			return install.PlannedChannels{}, nil, nil, fmt.Errorf("declared channel %q has no physical resource name", logical)
		}
		credential := node.ResourceNames["Secret/"+logical+"-creds"]
		if credential == "" {
			return install.PlannedChannels{}, nil, nil, fmt.Errorf("declared channel %q credential Secret has no physical resource name", logical)
		}
		requires.Channels[i].Name = physical
		credentialNames[physical] = credential
	}
	manifest.Requires = requires
	mapped.Manifest = &manifest
	plans, err := channelplan.PlanChannels(ctx, a.cfg.K8s, &mapped, node.Namespace, node.PhysicalName, "")
	if err != nil {
		return install.PlannedChannels{}, nil, nil, err
	}
	rows, warnings = a.declaredChannelRows(ctx, plans, node.Namespace, node.PhysicalName, owner, node.Path, bindingBase, credentialNames)
	if bindingBase == nil {
		return install.PlannedChannels{Plans: plans}, rows, warnings, nil
	}
	type stagedToken struct {
		token               string
		binding             channelSetupBinding
		credentialPreflight install.ChannelCredentialPreflight
	}
	var staged []stagedToken
	var credentialPreflights []install.ChannelCredentialPreflight
	pending := false
	for i, row := range rows {
		if row.Status == channelFormAlreadyWired {
			continue
		}
		if !actionableChannelStatus(row.Status) || row.SetupToken == "" {
			// A required channel with no usable setup action is still an
			// unresolved graph decision. Keep its diagnostic row on the form and
			// refuse an executable plan until the cluster/build changes.
			pending = true
			continue
		}
		binding := *bindingBase
		binding.AgentPath = node.Path.String()
		binding.AgentClass = node.PhysicalName
		binding.CredentialSecret = credentialNames[plans[i].Required.Name]
		binding.Required = plans[i].Required
		ready, readyErr := a.channelSetups.graphReady(row.SetupToken, owner, binding)
		if readyErr != nil {
			return install.PlannedChannels{}, rows, warnings, readyErr
		}
		if !ready {
			pending = true
			continue
		}
		credentialPreflight, preflightErr := install.PreflightGraphChannelCredential(
			ctx, a.cfg.K8s, node.RootInstallName, node.Namespace, binding.CredentialSecret,
		)
		if preflightErr != nil {
			return install.PlannedChannels{}, rows, warnings, preflightErr
		}
		credentialPreflights = append(credentialPreflights, credentialPreflight)
		staged = append(staged, stagedToken{token: row.SetupToken, binding: binding, credentialPreflight: credentialPreflight})
	}
	if pending {
		return install.NewPendingPlannedChannels(plans), rows, warnings, nil
	}

	var claimed []*claimedChannelSetup
	planned = install.NewPlannedChannelsWithRollback(plans,
		func() []string {
			var values []string
			for _, setup := range claimed {
				values = append(values, setup.staged.sensitiveValues...)
			}
			return values
		},
		func(context.Context) error {
			for _, setup := range staged {
				claim, claimErr := a.channelSetups.claimGraph(setup.token, owner, setup.binding)
				if claimErr != nil {
					var cleanupErrs []error
					for _, prior := range claimed {
						cleanupErrs = append(cleanupErrs, prior.invalidate(context.Background()))
					}
					return errors.Join(claimErr, errors.Join(cleanupErrs...))
				}
				claimed = append(claimed, claim)
			}
			return nil
		},
		func(rollbackCtx context.Context) error {
			var errs []error
			for i := len(claimed) - 1; i >= 0; i-- {
				errs = append(errs, claimed[i].invalidate(rollbackCtx))
			}
			return errors.Join(errs...)
		},
		func(applyCtx context.Context) error {
			for i, setup := range claimed {
				if setup.staged.output.SecretManifest != nil {
					install.StampGraphOwnership(setup.staged.output.SecretManifest, node.RootInstallName, node.Namespace, node.Path)
				}
				if setup.staged.output.ChannelManifest != nil {
					install.StampGraphOwnership(setup.staged.output.ChannelManifest, node.RootInstallName, node.Namespace, node.Path)
				}
				var guards []wizardrun.ObjectGuard
				if setup.staged.output.SecretManifest != nil {
					guards = append(guards, staged[i].credentialPreflight.ApplyGuard())
				}
				receipt, applyErr := wizardrun.ApplyTrackedGuarded(applyCtx, a.cfg.K8s, installedByAdmind, setup.staged.output, guards)
				setup.setApplyRollback(func(rollbackCtx context.Context) error {
					return receipt.Rollback(rollbackCtx, a.cfg.K8s)
				})
				if applyErr != nil {
					return applyErr
				}
				if consumeErr := setup.consume(); consumeErr != nil {
					return consumeErr
				}
			}
			return nil
		},
		func(rollbackCtx context.Context) error {
			var errs []error
			for i := len(claimed) - 1; i >= 0; i-- {
				errs = append(errs, claimed[i].rollbackApply(rollbackCtx))
			}
			return errors.Join(errs...)
		},
		func() {
			for _, setup := range claimed {
				setup.finalize()
			}
		},
	)
	planned = planned.WithChannelCredentialPreflights(credentialPreflights...)
	return planned, rows, warnings, nil
}

func (a *Admind) declaredChannelRows(ctx context.Context, plans []channelplan.ChannelPlan, namespace, agentClass string, owner identity.CanonicalUserID, path oap.DependencyPath, bindingBase *channelSetupBinding, credentialNames map[string]string) (rows []oapInstallChannel, warnings []string) {
	rows = make([]oapInstallChannel, 0, len(plans))
	for _, p := range plans {
		row := declaredChannelRow(ctx, p, namespace, a.wizardFor)
		row.AgentPath = path.String()
		// A token only for a row this admind can actually act on. Minting one
		// for a conflict or an unavailable kind would hand the UI a submit
		// button for a channel no submission can create, and would grow the
		// pending store with entries nothing will ever complete.
		if actionableChannelStatus(row.Status) {
			pending := &pendingChannelSetup{
				owner:      owner,
				namespace:  namespace,
				agentClass: agentClass,
				required:   p.Required,
				seeded:     p.Seeded,
				notSeeded:  p.NotSeeded,
			}
			if bindingBase != nil {
				binding := *bindingBase
				binding.AgentPath = path.String()
				binding.AgentClass = agentClass
				binding.CredentialSecret = credentialNames[p.Required.Name]
				binding.Required = p.Required
				pending.graphBinding = &binding
			}
			tok, err := a.channelSetups.put(pending)
			if err != nil {
				// Not fatal to the response the operator is waiting for, and
				// not silent either: the row still renders, with no token, and
				// a warning says why there is nothing to submit it with.
				a.cfg.Logger.Info("admind: could not offer a setup token for a declared channel",
					"namespace", namespace, "channel", row.Name, "subject", owner.String(), "err", err.Error())
				warnings = append(warnings, fmt.Sprintf(
					"the form for channel %q cannot be submitted right now: %s", row.Name, err.Error()))
			}
			row.SetupToken = tok
		}
		rows = append(rows, row)
	}

	// One line per channel this form will not set up, so the decision is
	// reconstructible from admind's logs alone rather than only from a body
	// the operator may never quote back.
	for _, r := range rows {
		if r.Status != channelFormAsk {
			a.cfg.Logger.Info("admind: a declared channel will not be set up from the install form",
				"namespace", namespace, "channel", r.Name, "kind", r.Kind, "status", r.Status, "reason", r.Reason)
		}
	}
	return rows, warnings
}

// actionableChannelStatus reports whether a row is one this admind can carry
// through to a Channel — and so whether it is worth a setup token.
//
// A conflict, an unavailable kind and an unknown wiring state get none: a token
// for one would hand the UI a submit button for a channel no submission can
// create, and would grow the pending store with entries nothing completes.
func actionableChannelStatus(status string) bool {
	return status == channelFormAsk || status == channelFormHandoff
}

// declaredChannelRow turns one planned channel into the row the form renders.
//
// THE GUARD ORDER IS cmd/oap's wireOne, and for its reason. Is this name held
// by something that is not ours, is it already ours, do we actually KNOW
// either way — every one of those is a verdict no form could change, so each
// is reached before the kind is consulted at all. Asking the kind first and
// rendering its fields would offer the operator a form for a Channel that
// cannot be created, which is the silent cross-binding channelplan B-R11
// refuses.
//
// Seeded and NotSeeded are populated for channelFormAsk AND channelFormHandoff
// — the two statuses that carry Questions — and for no other, which is the
// same narrower test cmd/oap's writeSeeded makes: nothing was going to be
// asked under any other status, so "answered for you, so you were not asked"
// would describe a run that never happened.
func declaredChannelRow(ctx context.Context, p channelplan.ChannelPlan, namespace string, wizardFor channelWizardLookup) oapInstallChannel {
	name := strings.TrimSpace(p.Required.Name)
	row := oapInstallChannel{
		Name:    name,
		Kind:    p.Required.Kind,
		Role:    p.Required.Role,
		Purpose: strings.TrimSpace(p.Required.Purpose),
	}
	// The command that wires this channel by hand, with the namespace spelled
	// out: a copy-pasted command that silently used a different default would
	// create the Channel where nothing is looking for it.
	//
	// AND WITH THE DECLARED ROLE, for the same reason and a sharper one. No
	// kind's wizard asks for a role, so a paste that cannot express it leaves
	// the Channel on ChannelSpec.Role's `both` default — which is deliberately
	// not an output-binding candidate — and the agent's other Channel then
	// goes Valid=False with nothing to deliver to. The row RENDERS the declared
	// role beside this command; a remedy that could not reproduce it would be
	// telling the operator to create something other than what is on screen.
	// Omitted when the declaration names none, matching what this form would
	// itself have created.
	finish := func() string {
		cmd := fmt.Sprintf("oap channel create --kind %s --name %s --namespace %s", p.Required.Kind, name, namespace)
		if role := strings.TrimSpace(p.Required.Role); role != "" {
			cmd += " --role " + role
		}
		return cmd
	}

	switch {
	case p.Conflict != nil:
		row.Status = channelFormConflict
		row.Reason = p.Conflict.Reason
		row.Remedy = "resolve the collision first — rename or delete that Channel, or install this agent into a namespace of its own — then: " + finish()
		return row
	case p.AlreadyWired:
		row.Status = channelFormAlreadyWired
		return row
	case !p.WiringKnown:
		row.Status = channelFormUnknown
		row.Reason = fmt.Sprintf(
			"this install could not determine whether a Channel named %q already exists in namespace %q, so it did not offer to create one",
			name, namespace)
		row.Remedy = "check for it, then: " + finish()
		return row
	}

	wiz, registered := wizardFor(p.Required.Kind)
	if !registered {
		row.Status = channelFormUnavailable
		row.Reason = fmt.Sprintf("%q is not a channel kind this build has; it has %s",
			p.Required.Kind, strings.Join(registry.Names(), ", "))
		row.Remedy = "install a build that registers this kind, then wire the channel with `oap channel create`"
		return row
	}
	// Kind.Wizard() returns an INTERFACE, and a kind that answered it with
	// nothing would be dereferenced on the very next line. Reported instead.
	// (The typed-nil variant of the same hazard compares non-nil here and
	// cannot be caught from this side; it is closed at the producer, by
	// channelkinds.UnavailableWizard returning a value rather than a pointer.)
	if wiz == nil {
		row.Status = channelFormUnavailable
		row.Reason = fmt.Sprintf("the %q kind offers no setup flow", p.Required.Kind)
		row.Remedy = "wire the channel with `oap channel create`"
		return row
	}

	// Asked alongside Inputs and before anything is rendered, the same place
	// channelwizard.Run asks it: a kind whose setup includes a browser round
	// trip describes it as Go closures (channelkinds.HandoffSpec — Begin,
	// Complete, FallbackGuidance), which no JSON body can carry, so the SPEC
	// stays server-side and only the fact that there is one reaches the row.
	//
	// A handoff row still carries the kind's up-front Questions, because those
	// are what the round trip is described FROM: github puts the organization
	// in the address the browser is sent to and in the manifest POSTed there.
	// What it does NOT carry is the fallback questions — those are asked after
	// the callback, and only the ones the exchange did not answer.
	handoff, err := wiz.Handoff(ctx, previewWizardInput(namespace, p))
	if err != nil {
		row.Status = channelFormUnavailable
		row.Reason = err.Error()
		row.Remedy = "once the cause is fixed: " + finish()
		return row
	}

	qs, err := wiz.Inputs(ctx, previewWizardInput(namespace, p))
	if err != nil {
		row.Status = channelFormUnavailable
		row.Reason = err.Error()
		row.Remedy = "once the cause is fixed: " + finish()
		return row
	}
	// The same contract check cmd/oap's renderer runs before it builds a
	// screen. A question this form cannot honor — an unknown type, an enum
	// with no values, a Binding or a Validation every renderer silently
	// ignores — is refused naming the question rather than shipped as a
	// silently wrong widget.
	if err := channelkinds.ValidateInputs(qs); err != nil {
		row.Status = channelFormUnavailable
		row.Reason = err.Error()
		row.Remedy = "this is a defect in the " + p.Required.Kind + " channel kind; wire the channel with `oap channel create` meanwhile"
		return row
	}

	row.Status = channelFormAsk
	if handoff != nil {
		row.Status = channelFormHandoff
		row.Reason = fmt.Sprintf(
			"setting up a %s channel sends you to an external service in a browser and waits for it to send you back",
			p.Required.Kind)
		row.Remedy = fmt.Sprintf(
			"answer these, then POST this channel's setupToken and answers to %s; the browser goes where that answers, and comes back here",
			channelHandoffPath)
	}
	row.Questions = unseededQuestions(qs, p.Seeded)
	row.Seeded = seededAnswers(qs, p)
	row.NotSeeded = unseededReasons(qs, p)
	return row
}

// previewWizardInput is what a kind is asked with when nobody has answered
// anything yet and nothing has been installed.
//
// K8s IS DELIBERATELY NIL, and that is the whole of why this is a named
// function rather than a struct literal at each call site. This runs BEFORE
// install applies a single CR, so the AgentClass the bundle is about to create
// is not in the namespace yet — and wizardkeys.AgentClassQuestion REFUSES a
// seeded class it cannot find in the live listing, which is exactly right for
// `oap channel create` and exactly wrong here: every first install of every
// channel-declaring bundle would fail on it, while a re-install of the same
// bundle succeeded. A nil client is the contract's offline reading — the
// seeded class is taken on trust and stands in as the sole option — which is
// the honest description of a preview whose cluster state does not exist yet.
//
// It is a genuine nil INTERFACE, never a typed-nil pointer: the field is left
// unset rather than assigned from a *someClient the caller forgot to fill in.
//
// NonInteractive is false and OperatorShell is false, which are not the same
// fact. A human IS at this form, so a kind must not refuse a step for want of
// anyone to answer it; but the form is rendered by a server, so a kind must
// not offer a step whose cost is paid by whoever is sitting at a terminal.
// WorkingDir is empty for the same reason: this process has no directory to
// leave a file in that the operator could ever read.
// Role is the role the BUNDLE declared, carried here so the preview asks the
// kind the same question the submit will: a kind that shapes its question set
// around the role (slack asks a role=output Channel where to post) would
// otherwise render a form missing the field the submit then needs.
func previewWizardInput(namespace string, p channelplan.ChannelPlan) channelkinds.WizardInput {
	return channelkinds.WizardInput{
		Namespace: namespace,
		Seeded:    p.Seeded,
		Role:      p.Required.Role,
	}
}

// unseededQuestions is the kind's declared inputs minus the ones this install
// already answered — dropped, not pre-filled. Mirrors
// channelwizard.renderQuestions, including reading seeded by PRESENCE rather
// than by a non-empty value, so both clients ask the same set.
func unseededQuestions(qs []oap.Question, seeded map[string]string) []oap.Question {
	out := make([]oap.Question, 0, len(qs))
	for _, q := range qs {
		if _, ok := seeded[q.Name]; ok {
			continue
		}
		out = append(out, q)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// seededAnswers reports what this install answered, for the questions this
// kind actually declared, in the kind's own declaration order.
//
// INTERSECTED WITH THE DECLARED QUESTIONS, deliberately. A plan seeds the
// external base URL for every declared channel and most kinds never ask for
// one; reporting it as "answered for you, so you were not asked" about a
// question that was never going to be asked describes a run that did not
// happen. It also makes the redaction below total: every key reported here has
// a known question type.
func seededAnswers(qs []oap.Question, p channelplan.ChannelPlan) []oapInstallChannelSeed {
	out := make([]oapInstallChannelSeed, 0, len(qs))
	for _, q := range qs {
		v, ok := p.Seeded[q.Name]
		if !ok {
			continue
		}
		s := oapInstallChannelSeed{Key: q.Name, Source: p.SeededFrom[q.Name]}
		if q.Type == oap.QSecret {
			s.Redacted = true
		} else {
			s.Value = v
		}
		out = append(out, s)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// unseededReasons reports why a value this install TRIED to pre-fill is
// missing, for the questions this kind actually declared.
//
// Same intersection as seededAnswers, and same reason: a reason attached to a
// key no kind asks about is noise on every response. NotSeeded and Seeded are
// disjoint by construction in the plan, so a key here is never one reported
// above.
func unseededReasons(qs []oap.Question, p channelplan.ChannelPlan) []oapInstallChannelNote {
	out := make([]oapInstallChannelNote, 0, len(qs))
	for _, q := range qs {
		why, ok := p.NotSeeded[q.Name]
		if !ok {
			continue
		}
		out = append(out, oapInstallChannelNote{Key: q.Name, Reason: why})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// bundleAgentClassName is the name the bundle's AgentClass CR will have in the
// cluster, which is what a declared Channel binds to.
//
// It reads the CR's OWN metadata.name and not the manifest's agent.name: the
// two are different fields and routinely differ. No rename is applied because
// none can be in play — a named install of a channel-declaring bundle is
// refused (channelplan.RefuseNamedInstall), so the bundled name is the
// installed name.
//
// A bundle with no AgentClass is an ERROR rather than an empty string. An
// empty class seeds nothing, and the kinds then refuse with "no AgentClasses
// found in namespace …" — a sentence about the namespace, which is not the
// problem, on a preview that never looked at one. install.Install refuses the
// same bundle a moment later anyway; saying which of the two things is wrong
// is the only difference.
func bundleAgentClassName(b *oap.Bundle) (string, error) {
	crs, err := b.CRs()
	if err != nil {
		return "", fmt.Errorf("decode bundle CRs to find the AgentClass its channels bind to: %w", err)
	}
	for _, cr := range crs {
		if cr.GetKind() == "AgentClass" {
			return cr.GetName(), nil
		}
	}
	return "", fmt.Errorf("this bundle declares channels but carries no AgentClass CR for them to bind to")
}
