// The other half of the install form's channel block: the route that takes the
// answers back and actually creates the Channel.
//
// oapinstall_channels.go decides what a declared channel's form LOOKS like.
// This is where a filled-in one lands. They are two files because they are two
// requests — the form is rendered from a .oap the browser uploaded, and it is
// submitted later against a token — but they are one contract, and the token
// is what carries the half of the answer the browser is not allowed to supply.
//
// EVERYTHING FROM THE COLLISION CHECK ONWARDS IS SHARED WITH THE CLI, through
// pkg/channels/channelkinds/wizardrun. The order of those steps is not
// derivable from the channelkinds.Wizard contract and each position cost a bug;
// what is HERE is only the part a browser client owns — turning a JSON body
// into an answer map, and refusing the keys that are not the operator's to
// send.
package admind

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/registry"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/wizardkeys"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/wizardrun"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/platform/oap"
	"github.com/authzed/openagentprimitives/pkg/platform/oap/channelplan"
	// questionscreen is the terminal's screen builder, and this is a server —
	// but DefaultString is the rule for what an UNANSWERED question is
	// preseeded with, and that rule has to be the same one on both clients or
	// the same declaration produces a different Channel depending on who set it
	// up. admind already depends on this package transitively (install's enum
	// validation reads EnumAnswerValues from it); nothing new is linked in.
	"github.com/authzed/openagentprimitives/pkg/platform/oap/questionscreen"
	"github.com/authzed/openagentprimitives/pkg/web/admind/agentcred"
)

// channelSetupPath is the route that completes one declared channel. Named as
// a constant because the install response points the UI at it and admind.go
// mounts it; two spellings of one path is one edit from a 404 nobody notices.
const channelSetupPath = "/admin/v1/agents/channel-setup"

// maxChannelSetupBody caps the answer body. A channel's answers are a handful
// of tokens and IDs; the largest single value any kind asks for is a PEM
// private key. 256 KiB is orders of magnitude above that and still nothing.
const maxChannelSetupBody = 256 << 10

// installedByAdmind is the provenance value stamped on everything this route
// applies, so `oap clean` treats a Channel the admin UI created exactly like
// one `oap channel create` created.
const installedByAdmind = "ap"

// channelSetupRequest is one filled-in channel form.
type channelSetupRequest struct {
	// SetupToken is the handle the install response minted for this declared
	// channel. It stands in for the namespace, the AgentClass and the
	// declaration itself — none of which the browser may restate; see
	// oapinstall_channel_pending.go.
	SetupToken string `json:"setupToken"`
	// Answers is what the operator filled in, keyed by Question.Name. A key
	// this install already seeded, or one the kind never declared, is REFUSED
	// rather than ignored.
	Answers map[string]string `json:"answers"`
}

// channelSetupResponse is the 200 for a direct channel that now exists or a
// graph channel whose sealed output is staged for the final install request.
//
// It carries NAMES and the kind's own next-step prose, and no answer value of
// any kind. The Secret this route just wrote is named, never read back: the
// same rule the install response follows, for the same reason — this body is
// logged and rendered, and a credential in it outlives both.
type channelSetupResponse struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	Kind      string `json:"kind"`
	// AgentClass is the class the new Channel binds to.
	AgentClass string `json:"agentClass"`
	// SecretName names the credentials Secret this setup wrote, or is empty
	// when the kind deliberately left an existing one untouched.
	SecretName string `json:"secretName,omitempty"`
	// AlreadyWired reports that the Channel was already there and already this
	// agent's, so this request created nothing. Success, not a failure — it is
	// what makes re-submitting the form safe.
	AlreadyWired bool `json:"alreadyWired,omitempty"`
	// Staged reports that the wizard output is sealed for the graph install
	// request that minted the token, but has not been applied to the cluster.
	Staged bool `json:"staged,omitempty"`
	// NextSteps is the kind's own post-setup guidance
	// (channelkinds.WizardOutput.Notes): what the operator must still do
	// OUTSIDE the cluster, such as inviting the bot to a conversation.
	NextSteps []string `json:"nextSteps,omitempty"`
	// Warnings are things that went wrong, or are worth saying, AFTER the
	// Channel was already on the cluster — today, a Channel that did not take
	// the declared name. They do not make a created Channel uncreated, so they
	// ride on the 200 rather than turning it into an error.
	Warnings []string `json:"warnings,omitempty"`
}

// handleChannelSetup completes one declared channel from the answers the
// install form collected.
//
// WHAT IT REFUSES, and why each refusal is here rather than left to a later
// layer:
//
//   - An answer for a key this install SEEDED. That is B-R13 made structural:
//     the declared Channel name and the bundle's AgentClass are the bundle's,
//     other bundled CRs are already written against "<name>-creds", and a form
//     that could send a different name recreates the dangling-credential defect
//     that no lint can see. The form never renders those fields; a request that
//     supplies one anyway is refused by name rather than silently ignored,
//     because ignoring it would tell the caller their value was taken.
//
//   - An answer for a key the kind never declared. The same refusal
//     `oap channel create` makes for an unknown --answer, and for the same
//     reason: a typo'd key that is quietly dropped looks exactly like an answer
//     that was accepted.
//
//   - A Channel of this name that is not ours, and a namespace this install
//     could not read. Both are verdicts no form can change, and both are
//     re-derived HERE rather than trusted from the response that rendered the
//     form: a Channel can appear, or be bound to another agent, between the two
//     requests.
func (a *Admind) handleChannelSetup(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()

	owner, ok := a.channelSetupOwner(w, r)
	if !ok {
		return
	}

	var req channelSetupRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxChannelSetupBody)).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "decode request: "+err.Error())
		return
	}

	rec, err := a.channelSetups.get(req.SetupToken, owner)
	if err != nil {
		a.cfg.Logger.Info("admind: channel setup for an unknown, expired, or unowned token",
			"subject", owner.String(), "err", err.Error())
		writeJSONError(w, http.StatusNotFound, err.Error())
		return
	}
	if refuseCompletedGraphSetup(w, &rec) {
		return
	}
	rec.redactor.register(req.Answers)
	graphResolveClaimed := false
	if rec.graphBinding != nil {
		rec, err = a.channelSetups.beginGraphResolve(req.SetupToken, owner)
		if err != nil {
			writeJSONError(w, http.StatusConflict, err.Error())
			return
		}
		graphResolveClaimed = true
		defer func() {
			if !graphResolveClaimed {
				return
			}
			if cleanupErr := a.channelSetups.failGraphResolve(context.WithoutCancel(r.Context()), rec.token, rec.owner); cleanupErr != nil {
				a.cfg.Logger.Info("admind: failed graph channel resolver cleanup", "namespace", rec.namespace, "channel", rec.required.Name, "err", cleanupErr.Error())
			}
		}()
	}

	prep, ok := a.prepareChannelSetup(w, r, &rec)
	if !ok {
		return
	}
	if prep.alreadyWired {
		if rec.graphBinding != nil {
			if err := a.channelSetups.abandon(r.Context(), rec.token, rec.owner); err != nil {
				a.cfg.Logger.Info("admind: obsolete staged channel setup could not be cleaned",
					"namespace", rec.namespace, "channel", rec.required.Name, "kind", rec.required.Kind, "err", err.Error())
				writeJSONError(w, http.StatusInternalServerError, "clean obsolete channel setup: "+err.Error())
				return
			}
		} else {
			a.channelSetups.forget(rec.token)
		}
		writeJSON(w, http.StatusOK, channelSetupResponse{
			Name: rec.required.Name, Namespace: rec.namespace, Kind: rec.required.Kind,
			AgentClass: rec.agentClass, AlreadyWired: true,
		})
		return
	}

	// A completed browser round trip is finished HERE: the callback exchanged
	// the code and recorded what it produced, and this request carries the
	// fallback answers the exchange did not supply.
	if rec.handoffDerived != nil {
		if a.finishExchangedChannelHandoff(w, r, &rec, prep, req.Answers) {
			graphResolveClaimed = false
		}
		return
	}
	// Merged over the FULL declared set — the up-front questions plus the
	// handoff's fallback ones — because the branch below needs the answers to
	// ask the kind whether a round trip is owed at all, and because a kind
	// that declines one asks its fallback questions here. A nil handoff
	// leaves the set exactly as it was (wizardrun.AllInputs).
	declared := wizardrun.AllInputs(prep.inputs, prep.handoff)
	answers, err := mergeChannelAnswers(declared, rec.seeded, nil, req.Answers)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, rec.redactor.string(err.Error()))
		return
	}
	rec.redactor.register(answers)

	// A kind whose setup needs a browser round trip cannot be completed from
	// this route's body alone. Building manifests from answers the round trip
	// was going to supply is the failure this refusal replaces.
	//
	// UNLESS THE KIND ITSELF DECLINED IT for these answers. github's operator
	// saying they already have a GitHub App is exactly that: there is nothing
	// to register, so there is no round trip to insist on, and the App's
	// details are what this request carries. Asking the SPEC keeps this and
	// `oap channel create` from disagreeing about which runs go to a browser.
	owed := prep.inputs
	if prep.handoff != nil {
		if prep.handoff.Skipped(answers) == "" {
			writeJSONError(w, http.StatusBadRequest, fmt.Sprintf(
				"setting up a %s channel sends you to an external service in a browser first; "+
					"POST this same setupToken to %s to start that, then submit the answers it asks for",
				rec.required.Kind, channelHandoffPath))
			return
		}
		// The fallback questions are owed too: on this route they are not what
		// a round trip would have supplied, they are how the operator
		// describes the application they already have.
		owed = declared
	}

	if missing := unansweredChannelQuestions(owed, rec.seeded, answers); len(missing) > 0 {
		if rec.graphBinding != nil {
			if err := a.channelSetups.releaseGraphResolve(rec.token, rec.owner); err != nil {
				writeJSONError(w, http.StatusConflict, err.Error())
				return
			}
			graphResolveClaimed = false
		}
		writeJSON(w, http.StatusBadRequest, oapInstallMissingQuestionsResponse{
			Error:     "missing required answer(s) for channel " + rec.required.Name,
			Questions: missing,
		})
		return
	}

	// NIL handoff, deliberately, even where the kind declared one: a stood-down
	// round trip is one that must not run, and wizardrun.Finish drives every
	// handoff it is handed. Resolve and Result still run in their own
	// positions, which is where the answers this route collected are read.
	a.finishChannelSetup(w, r, &rec, prep.wizard, prep.in, answers, nil, nil)
}

func refuseCompletedGraphSetup(w http.ResponseWriter, rec *pendingChannelSetup) bool {
	if rec == nil || rec.graphBinding == nil || rec.graphState == graphSetupPending || rec.graphState == graphSetupHandoffResolved {
		return false
	}
	writeJSONError(w, http.StatusConflict, "channel setup is already resolving or complete; re-open the install form if it was not staged")
	return true
}

// finishExchangedChannelHandoff completes a channel whose browser round trip
// already ran.
//
// It uses the spec and the WizardInput the BEGIN request captured, not the ones
// prepareChannelSetup just rebuilt: the stored spec's closures hold the
// exchange this run belongs to, and asking the kind for a fresh one would get a
// second spec that knows nothing about the App that now exists.
//
// The answers the operator gave BEFORE the browser left (github's organization,
// the Channel's authz subject) are carried forward as `prior` rather than
// demanded again — the browser only ever supplied them once, and Result needs
// them.
//
// The exchange's own answers are NOT merged in here. They arrive through
// wizardrun.Finish, from a driver that replays them, so they land in the
// handoff's position in the sequence: before Resolve, which is what reads them.
func (a *Admind) finishExchangedChannelHandoff(
	w http.ResponseWriter,
	r *http.Request,
	rec *pendingChannelSetup,
	prep channelSetupPrep,
	supplied map[string]string,
) bool {
	declared := wizardrun.AllInputs(prep.inputs, rec.handoff)
	answers, err := mergeChannelAnswers(declared, rec.seeded, rec.handoffAnswers, supplied)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return false
	}
	rec.redactor.register(answers)
	// Only what the exchange did NOT answer is owed. Asked of the spec rather
	// than filtered here, because a question the handoff answers under another
	// name is still answered — github's private-key-path against its
	// private-key — and a client that decided that for itself would ask the
	// operator for a file that does not exist, on the happy path.
	remaining := rec.handoff.Unanswered(rec.handoffDerived)
	if missing := unansweredChannelQuestions(remaining, rec.seeded, answers); len(missing) > 0 {
		if rec.graphBinding != nil {
			if err := a.channelSetups.releaseGraphResolve(rec.token, rec.owner); err != nil {
				writeJSONError(w, http.StatusConflict, err.Error())
				return false
			}
		}
		writeJSON(w, http.StatusBadRequest, oapInstallMissingQuestionsResponse{
			Error:     "missing required answer(s) for channel " + rec.required.Name,
			Questions: missing,
		})
		return rec.graphBinding != nil
	}
	a.finishChannelSetup(w, r, rec, prep.wizard, rec.handoffIn, answers,
		rec.handoff, replayHandoff(rec.handoffDerived))
	return false
}

// channelSetupOwner reads the subject `require` already proved.
//
// It is read again rather than threaded through the middleware because
// `require`'s job is the platform permission and every other route needs
// nothing more. What THIS route needs it for is different and narrower: two
// operators can both hold install_agent, so the permission cannot distinguish
// them, and a pending setup is one operator's capability.
func (a *Admind) channelSetupOwner(w http.ResponseWriter, r *http.Request) (identity.CanonicalUserID, bool) {
	canonical, err := identity.Subject(r.Header.Get(agentcred.SubjectHeader)).CanonicalUserID()
	if err != nil || canonical.IsZero() {
		// Unreachable behind `require`, which refuses the same header shape
		// before this handler runs. Kept because "the middleware already
		// checked" is a property of a route table one edit can change, and the
		// failure it would cause here is a pending setup owned by "".
		writeJSONError(w, http.StatusUnauthorized, "missing "+agentcred.SubjectHeader+" (want \"user:<canonical>\")")
		return identity.CanonicalUserID{}, false
	}
	return canonical, true
}

// channelSetupPrep is everything both the submit route and the handoff route
// need to have established before they diverge.
type channelSetupPrep struct {
	wizard  channelkinds.Wizard
	in      channelkinds.WizardInput
	inputs  []oap.Question
	handoff *channelkinds.HandoffSpec
	// alreadyWired says the Channel is already there and already ours, so
	// there is nothing for either route to do.
	alreadyWired bool
}

// prepareChannelSetup resolves the kind, re-checks the namespace, and asks the
// kind what it needs — the steps every route that acts on a pending setup
// makes, in the order oapinstall_channels.go's row builder makes them and for
// the same reason: a verdict no form can change is reached before the kind is
// consulted at all.
//
// It writes the response and returns ok=false on every refusal, so a caller
// that forgets to check cannot proceed on a zero value.
func (a *Admind) prepareChannelSetup(w http.ResponseWriter, r *http.Request, rec *pendingChannelSetup) (channelSetupPrep, bool) {
	ctx := r.Context()

	// Re-planned rather than trusted from the record: what the earlier response
	// reported was true when the form was rendered, and the collision it looks
	// for can have appeared since.
	plan, err := channelplan.PlanOne(ctx, a.cfg.K8s, rec.required, rec.namespace, rec.agentClass)
	if err != nil {
		a.cfg.Logger.Info("admind: could not re-check a declared channel before setting it up",
			"namespace", rec.namespace, "channel", rec.required.Name, "err", err.Error())
		writeJSONError(w, http.StatusInternalServerError, "re-check this channel: "+err.Error())
		return channelSetupPrep{}, false
	}
	switch {
	case plan.Conflict != nil:
		writeJSONError(w, http.StatusConflict, plan.Conflict.Reason)
		return channelSetupPrep{}, false
	case plan.AlreadyWired:
		return channelSetupPrep{alreadyWired: true}, true
	case !plan.WiringKnown:
		writeJSONError(w, http.StatusInternalServerError, fmt.Sprintf(
			"this install could not determine whether a Channel named %q already exists in namespace %q, so it did not create one",
			rec.required.Name, rec.namespace))
		return channelSetupPrep{}, false
	}

	wiz, registered := a.wizardFor(rec.required.Kind)
	if !registered || wiz == nil {
		writeJSONError(w, http.StatusBadRequest, fmt.Sprintf(
			"%q is not a channel kind this build can set up; it has %s",
			rec.required.Kind, strings.Join(registry.Names(), ", ")))
		return channelSetupPrep{}, false
	}

	in := a.channelWizardInput(rec, plan.Seeded)
	if rec.graphBinding != nil {
		// Graph channel setup happens before the graph's AgentClass exists. The
		// declaration already binds the physical class name; provider resolution
		// in Finish gets the live client after all form decisions are complete.
		in.K8s = nil
	}
	inputs, err := wiz.Inputs(ctx, in)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, rec.redactor.string(err.Error()))
		return channelSetupPrep{}, false
	}
	if err := channelkinds.ValidateInputs(inputs); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return channelSetupPrep{}, false
	}
	// NOT re-asked once an exchange has already run against a spec this setup
	// is holding. Wizard.Handoff may do I/O and nothing in the contract says it
	// is idempotent, so a second call is a second chance to answer differently
	// — and the answer that matters is already in hand: rec.handoff is the very
	// spec whose Complete produced rec.handoffDerived, closures and captured
	// state included. A fresh one would know nothing about the App that now
	// exists.
	//
	// A begin that never came back is deliberately NOT covered by this: nothing
	// has been exchanged, so re-asking the kind is the honest thing and lets it
	// answer against the current seeds.
	handoff := rec.handoff
	if rec.handoffDerived == nil {
		handoff, err = wiz.Handoff(ctx, in)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, rec.redactor.string(err.Error()))
			return channelSetupPrep{}, false
		}
	}
	// The record's seeds are refreshed from the plan just made: the external
	// base URL is a cluster fact webd can publish between the two requests, and
	// a stale absent one would make a handoff unstartable for no reason. Its
	// absence-reasons come with it, so a refusal quotes the planner's current
	// words rather than the ones a request minutes ago recorded.
	//
	// Written back through the store as well as onto this request's own copy:
	// the CALLBACK reads the seeds and never re-plans, so a refresh that stayed
	// local would be lost exactly where it is needed.
	rec.seeded, rec.notSeeded = plan.Seeded, plan.NotSeeded
	rec.redactor.register(rec.seeded)
	if _, err := a.channelSetups.update(rec.token, rec.owner, func(p *pendingChannelSetup) {
		p.seeded, p.notSeeded = plan.Seeded, plan.NotSeeded
	}); err != nil {
		writeJSONError(w, http.StatusNotFound, err.Error())
		return channelSetupPrep{}, false
	}
	return channelSetupPrep{wizard: wiz, in: in, inputs: inputs, handoff: handoff}, true
}

// channelWizardInput is what a kind is asked with once the operator is
// submitting rather than previewing.
//
// K8s IS LIVE here, unlike previewWizardInput's deliberate nil. By the time a
// channel is being created the agent has been installed, so the AgentClass the
// seed names really is in the namespace and wizardkeys.AgentClassQuestion's
// verification of it is exactly right — an operator who wires a channel before
// installing the agent should be told so, by the kind, in its own words.
//
// NonInteractive is false: a human IS at this form. OperatorShell is false and
// WorkingDir is empty: the form is rendered by a server, so a kind must not
// offer a step whose cost is paid by whoever is sitting at a terminal, and this
// process has no directory to leave a file in that the operator could read.
//
// Role is the role the BUNDLE declared, which is not an answer any kind asks
// for and so reaches the Channel from here or not at all. Omitted, a declared
// `role: output` would take ChannelSpec.Role's `both` default for every kind
// whose Result sets no role (slack is one) — and role=both is deliberately not
// an output-binding candidate, so the agent's OTHER Channel would go
// Valid=False with nothing to deliver to. It is also what lets the kind shape
// its question set around the role. See channelkinds.WizardInput.Role.
func (a *Admind) channelWizardInput(rec *pendingChannelSetup, seeded map[string]string) channelkinds.WizardInput {
	return channelkinds.WizardInput{
		K8s:       a.cfg.K8s,
		Namespace: rec.namespace,
		Seeded:    seeded,
		Role:      rec.required.Role,
	}
}

// finishChannelSetup runs the shared tail and lands the result.
//
// handoff/drive are nil for a kind that needs no browser round trip, and are
// the stored spec plus a driver that replays what the callback already
// exchanged for one that does — so both routes go through the SAME
// wizardrun.Finish, with the collision check in front of the irreversible
// steps and Resolve reading what the handoff produced.
func (a *Admind) finishChannelSetup(
	w http.ResponseWriter,
	r *http.Request,
	rec *pendingChannelSetup,
	wiz channelkinds.Wizard,
	in channelkinds.WizardInput,
	answers map[string]string,
	handoff *channelkinds.HandoffSpec,
	drive wizardrun.HandoffDriver,
) {
	ctx := r.Context()
	if rec.graphBinding != nil {
		in.K8s = a.cfg.K8s
	}
	rec.redactor.register(answers)
	out, err := wizardrun.Finish(ctx, wizardrun.Params{
		Wizard: wiz,
		// in carries the declared role (channelWizardInput), which Finish
		// stamps onto the produced Channel — the same value the kind's own
		// Inputs and Result were handed.
		In:           in,
		Answers:      answers,
		Handoff:      handoff,
		DriveHandoff: drive,
	})
	if err != nil {
		safeErr := rec.redactor.string(err.Error())
		// The kind's own sentence, verbatim: "Slack rejected this bot token
		// (auth.test): …" is the one line that says what to fix. Logged as well
		// as returned, because the operator may never quote the body back.
		a.cfg.Logger.Info("admind: channel setup could not produce its manifests",
			"namespace", rec.namespace, "channel", rec.required.Name, "kind", rec.required.Kind, "err", safeErr)
		writeJSONError(w, http.StatusBadRequest, safeErr)
		return
	}
	sensitiveValues := channelOutputSensitiveValues(answers, out)
	rec.redactor.registerValues(sensitiveValues)
	if rec.graphBinding != nil {
		wizardrun.NormalizeOutputIdentity(&out, rec.namespace, rec.required.Name, rec.graphBinding.CredentialSecret, rec.agentClass)
		if err := a.channelSetups.completeGraph(rec.token, rec.owner, *rec.graphBinding, stagedChannelSetup{
			output:          out,
			sensitiveValues: sensitiveValues,
		}); err != nil {
			a.cfg.Logger.Info("admind: channel setup could not be staged for its graph install",
				"namespace", rec.namespace, "channel", rec.required.Name, "kind", rec.required.Kind, "err", rec.redactor.string(err.Error()))
			writeJSONError(w, http.StatusConflict, rec.redactor.string(err.Error()))
			return
		}
		writeJSON(w, http.StatusOK, channelSetupResponse{
			Name: rec.required.Name, Namespace: rec.namespace, Kind: rec.required.Kind,
			AgentClass: rec.agentClass, Staged: true, NextSteps: redactStrings(rec.redactor, out.Notes),
		})
		return
	}

	if err := wizardrun.Apply(ctx, a.cfg.K8s, wizardrun.ClientApplier(a.cfg.K8s, installedByAdmind), out); err != nil {
		a.cfg.Logger.Info("admind: channel setup could not be applied",
			"namespace", rec.namespace, "channel", rec.required.Name, "kind", rec.required.Kind, "err", rec.redactor.string(err.Error()))
		writeJSONError(w, http.StatusInternalServerError, "apply this channel: "+rec.redactor.string(err.Error()))
		return
	}

	// What the run DECIDED (WizardOutput.Summary) is logged and not returned.
	// A kind is contractually obliged to mask a credential before putting one
	// there, and every kind does — but nothing between that field and a
	// consumer inspects it, so echoing it into an HTTP body would make this
	// route's safety a property of every kind's future edits rather than of
	// this route.
	for _, s := range out.Summary {
		a.cfg.Logger.Info("admind: channel setup decided",
			"namespace", rec.namespace, "channel", rec.required.Name, "label", rec.redactor.string(s.Label), "value", rec.redactor.string(s.Value))
	}
	a.cfg.Logger.Info("admind: channel created from the install form",
		"namespace", rec.namespace, "channel", rec.required.Name, "kind", rec.required.Kind,
		"agentClass", rec.agentClass, "subject", rec.owner.String())

	a.channelSetups.forget(rec.token)
	resp := channelSetupResponse{
		// The name the Channel ACTUALLY took, read off the manifest that was
		// just applied rather than off the declaration that asked for it. The
		// two are the same for every kind that asks for a name and honours the
		// seeded one; a kind whose manifests fix their own name is exactly the
		// case where reporting the declared one would tell the operator a
		// Channel exists under a name that does not exist. Falls back to the
		// declared name only when this run produced no Channel manifest at all
		// (a re-setup that rewrote only the Secret), where there is nothing
		// truer to say.
		Name:       rec.required.Name,
		Namespace:  rec.namespace,
		Kind:       rec.required.Kind,
		AgentClass: rec.agentClass,
		NextSteps:  redactStrings(rec.redactor, out.Notes),
	}
	if out.ChannelManifest != nil {
		resp.Name = out.ChannelManifest.Name
	}
	// The same sentence `oap agent install` carries, from the same function:
	// a Channel created under another name leaves every bundled CR reading
	// "<declared>-creds" with no Secret to find. A warning and not a failure —
	// the Channel is already applied.
	if warn := channelplan.DeclaredNameWarning(rec.required, out.ChannelManifest); warn != "" {
		a.cfg.Logger.Info("admind: a channel did not take the declared name",
			"namespace", rec.namespace, "declared", rec.required.Name,
			"created", resp.Name, "kind", rec.required.Kind)
		resp.Warnings = append(resp.Warnings, warn)
	}
	if out.SecretManifest != nil {
		resp.SecretName = out.SecretManifest.Name
	}
	writeJSON(w, http.StatusOK, resp)
}

func channelOutputSensitiveValues(answers map[string]string, out channelkinds.WizardOutput) []string {
	values := make([]string, 0, len(answers))
	for _, value := range answers {
		if value != "" {
			values = append(values, value)
		}
	}
	if out.SecretManifest != nil {
		for _, value := range out.SecretManifest.StringData {
			if value != "" {
				values = append(values, value)
			}
		}
		for _, value := range out.SecretManifest.Data {
			if len(value) > 0 {
				values = append(values, string(value))
			}
		}
	}
	return values
}

// mergeChannelAnswers builds the answer map the kind is handed, in four
// precedence layers, highest first:
//
//  1. seeded — what this install decided; the browser may not override it.
//  2. supplied — what this request's operator filled in.
//  3. prior — what an EARLIER request of the same setup collected. Non-nil
//     only for a channel whose browser round trip already ran: those answers
//     were sent to the begin route and are not re-sent, and losing them would
//     drop values Result needs (github's organization) that the browser only
//     ever supplied once.
//  4. the question's own Default, for anything still unanswered.
//
// It mirrors what the terminal client ends up with — channelwizard's
// renderQuestions writes a seeded value straight into State and DROPS the
// question, questionscreen preseeds an unanswered one from its Default, and
// answersFrom reads back every declared question plus the Channel name whether
// declared or not — so one declaration produces one Channel regardless of which
// client set it up.
//
// A supplied key that is seeded, or that no question declares, is an ERROR
// rather than a silent drop. Both look identical to a caller otherwise: the
// request succeeds and the value it sent was never used.
func mergeChannelAnswers(inputs []oap.Question, seeded, prior, supplied map[string]string) (map[string]string, error) {
	declared := make(map[string]bool, len(inputs))
	for _, q := range inputs {
		declared[q.Name] = true
	}

	var unknown, notYours []string
	for k := range supplied {
		switch {
		case !declared[k]:
			unknown = append(unknown, k)
		case seeded != nil && func() bool { _, ok := seeded[k]; return ok }():
			notYours = append(notYours, k)
		}
	}
	sort.Strings(unknown)
	sort.Strings(notYours)
	if len(notYours) > 0 {
		return nil, fmt.Errorf(
			"this install already decided %s for you, so the form does not ask for it and will not accept it; "+
				"the declared Channel name and the bundle's AgentClass are what every other bundled resource is already written against",
			strings.Join(quoteAll(notYours), ", "))
	}
	if len(unknown) > 0 {
		return nil, fmt.Errorf("this channel kind declares no question named %s; it asks for %s",
			strings.Join(quoteAll(unknown), ", "), strings.Join(quoteAll(declaredQuestionNames(inputs)), ", "))
	}

	answers := make(map[string]string, len(inputs)+1)
	// The Channel name is carried whether the kind declares a question for it
	// or not, exactly as the terminal client carries it: it is a property of
	// the declaration rather than an answer to any kind's question, and a kind
	// that does not ask for one can still legitimately have been told one.
	if v, ok := seeded[wizardkeys.KeyChannelName]; ok {
		answers[wizardkeys.KeyChannelName] = v
	}
	for _, q := range inputs {
		if v, ok := seeded[q.Name]; ok {
			answers[q.Name] = v
			continue
		}
		if v, ok := supplied[q.Name]; ok {
			answers[q.Name] = strings.TrimSpace(v)
			continue
		}
		if v, ok := prior[q.Name]; ok {
			answers[q.Name] = v
			continue
		}
		if d := questionDefaultAnswer(q); d != "" {
			answers[q.Name] = d
		}
	}
	return answers, nil
}

// questionDefaultAnswer is the value an unanswered question carries, in the
// same spelling the terminal's own answer map would have held it in: a
// multi-select's selection is comma-joined (channelwizard.answersFrom), and
// everything else is the scalar default.
func questionDefaultAnswer(q oap.Question) string {
	if q.Type == oap.QResourceList && len(q.Enum) > 0 {
		return strings.Join(questionscreen.EnumAnswerValues(q.Default), ",")
	}
	return questionscreen.DefaultString(q)
}

// unansweredChannelQuestions is every question the operator still owes an
// answer to: declared, ASKED ON THIS ROUTE, not seeded, required, and answered
// with nothing.
//
// An EMPTY STRING counts as unanswered, not just an absent key — the UI writes
// "" for a field the operator cleared, and a required question answered with
// nothing would otherwise reach Result as a blank the manifests record.
//
// THE ROUTE CLAUSE IS WHERE THIS SERVER HONORS oap.Question.AskWhen, and it is
// the only place it can. A kind whose question set branches declares every
// route's questions in one batch — that is what keeps each of them seedable —
// and a one-shot form draws the superset, because it has no answers to gate on
// when it renders. What it DOES have answers for is this request, so the gate
// applies where the route decides what is still owed: without it, an operator
// who picked slack's "create the app for me" would be blocked here on the bot
// token that route exists to mint, which is the defect the gate closes on the
// terminal side.
//
// The gate is read against the MERGED answers, so a route settled by a seed,
// by this request, or by a question's own default all read the same way.
func unansweredChannelQuestions(inputs []oap.Question, seeded, answers map[string]string) []oap.Question {
	answer := func(key string) string { return answers[key] }
	var missing []oap.Question
	for _, q := range inputs {
		if _, ok := seeded[q.Name]; ok {
			continue
		}
		if !q.Applies(answer) {
			continue
		}
		if !q.IsRequired() {
			continue
		}
		if strings.TrimSpace(answers[q.Name]) != "" {
			continue
		}
		missing = append(missing, q)
	}
	sort.Slice(missing, func(i, j int) bool { return missing[i].Name < missing[j].Name })
	return missing
}

func declaredQuestionNames(inputs []oap.Question) []string {
	out := make([]string, 0, len(inputs))
	for _, q := range inputs {
		out = append(out, q.Name)
	}
	return out
}

func quoteAll(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		out = append(out, fmt.Sprintf("%q", s))
	}
	return out
}
