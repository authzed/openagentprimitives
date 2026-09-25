package channelkinds

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/oap"
)

// Wizard is how a channel kind states what it needs from an operator. It
// answers in DATA, not in screens: the CLI renders the questions as terminal
// widgets, the admin UI renders them as a form, and neither is the kind's
// concern.
//
// NOTHING ON THIS INTERFACE NAMES A TERMINAL TYPE, and that is the property
// it exists for rather than a detail of it: a kind that named one could only
// ever be driven by a terminal client. Inputs answers in oap.Question values,
// Result takes a plain answer map, and the one step no form can express —
// sending the operator to an external service and receiving a callback — is
// DESCRIBED here (HandoffSpec) and DRIVEN by the client, which owns the
// callback endpoint. Keep it that way: a tui, huh, or bubbletea type
// appearing in any signature below re-closes the door to a server-rendered
// wizard.
type Wizard interface {
	// Inputs is what this kind needs, as data. May do I/O — listing
	// AgentClasses to populate an enum, probing an API to default a field.
	//
	// Returning no questions, with a nil Handoff and a nil error, is
	// legitimate. Result derives its output from in AND answers, both
	// explicit, so a kind whose output is fully determined by in has nothing
	// to ask: the fake test kind is exactly that — every value in its
	// manifests is fixed — and inventing a question for it would add a prompt
	// no operator's answer changes. A kind that cannot be configured here at
	// all, because its Channels are built by another process, returns an
	// error instead; that is what UnavailableWizard does.
	//
	// Every returned Question must satisfy ValidateInputs: Name non-empty and
	// unique across the slice, Type one of the six QuestionTypes, a QEnum
	// question carrying at least one Enum value, Prompt non-empty, and Binding
	// empty. Binding is a bundle-install concept — where an answer lands in a
	// CR — that does not apply here: a channel input's answer is applied by
	// Result itself, so a Binding on one would be silently ignored by every
	// renderer, which is worse than not having it at all.
	Inputs(ctx context.Context, in WizardInput) ([]oap.Question, error)

	// Handoff describes an interactive step no form can express: sending the
	// operator to an external service and receiving a callback. nil when the
	// kind needs none, which is the common case.
	//
	// Called alongside Inputs, BEFORE anything has been answered, because the
	// questions the handoff may fall back on are part of the set a client
	// accepts a flag for. What the handoff needs from the answers therefore
	// arrives later, through the closures on HandoffSpec — see its doc.
	//
	// It reads in the way Inputs does, Seeded included: a run whose flags
	// already carry everything the round trip would have produced has nothing
	// to hand off, and returning a spec for it would send the operator to
	// create a second copy of what they already have.
	Handoff(ctx context.Context, in WizardInput) (*HandoffSpec, error)

	// Resolve performs I/O that DERIVES further answers from the answers
	// already given — verifying a credential and recording the identity it
	// belongs to. It returns only the ADDITIONAL answers, to be merged into
	// the map before Result runs; nil or empty means the kind needs none,
	// which is the common case.
	//
	// It exists so Result can stay a pure function: a kind that must talk to
	// an external service to complete its manifests does it here, where the
	// client can run it as its own step with its own error surface. A rejected
	// credential is a user-facing failure, and burying it inside Result would
	// make it indistinguishable from a manifest-construction bug.
	//
	// It is NOT a second Inputs. A kind with something further to ASK has no
	// hook here — the answer map it is handed is already complete, and the
	// client has stopped rendering by the time it runs. Slack is the shape
	// this is for: `auth.test` on a bot token the operator has just pasted,
	// whose UserID the Channel needs in order to recognise its own messages.
	//
	// It may be called with answers a client collected in any way — typed,
	// seeded from flags, or returned by HandoffSpec.Complete — so it must not
	// assume a terminal, and it reads the same INERT subset of in that Result
	// does plus nothing on the receiver that only Inputs would have set.
	// Unlike Result it may hold live clients injected at construction (a Slack
	// API factory), which is what doing I/O means.
	Resolve(ctx context.Context, in WizardInput, answers map[string]string) (map[string]string, error)

	// Result reads the answers and produces the manifests. Keyed by
	// Question.Name, plus whatever HandoffSpec.Complete contributed.
	//
	// MUST be a pure function of its TWO arguments: no clock, no randomness,
	// no I/O — and, just as load-bearing, nothing carried on the receiver
	// from an earlier Inputs call. The admin UI calls this server-side, where
	// the two calls may land on different instances or in different
	// processes: a Result that read state Inputs had stashed would either
	// have to be preceded by an Inputs call made purely for its side effect,
	// or return manifests built from a zero value. An impure Result would
	// separately make an install unreproducible from the same answers.
	//
	// in is the same WizardInput Inputs received, passed again rather than
	// remembered. Most of what a kind needs in order to build its manifests
	// is not an answer to any question — the namespace above all — so it
	// cannot arrive in the map, and the alternative to passing it is exactly
	// the receiver state the paragraph above rules out.
	//
	// Only the INERT fields of in are readable here: Namespace, Monitoring,
	// Existing and CredentialsExist. K8s is a live API client, and Seeded says
	// what a client's own flags answered BEFORE the run — reading the first
	// would break the no-I/O rule above, and reading the second would make
	// the manifests depend on HOW an answer arrived rather than on what it
	// was, when the map already carries every seeded value. Both belong to
	// Inputs, which runs where a cluster connection exists; Result must stay
	// callable by a client that has none, from an in carrying nothing but the
	// four fields above.
	//
	// NonInteractive, WorkingDir and OperatorShell are inert but are NOT among
	// them: a manifest that differed by whether a human was watching, by where
	// the client was run from, or by whether that client was the operator's own
	// shell would make an install unreproducible from its own answers. All
	// three are for Inputs and Resolve — see their own docs.
	Result(in WizardInput, answers map[string]string) (WizardOutput, error)
}

// HandoffSpec describes sending the operator to an external service and
// receiving a callback. The CLIENT owns the callback endpoint — a loopback
// listener for the CLI, a route for the UI — so one spec drives both.
//
// It is described by Handoff, BEFORE any question has been answered, but most
// of what it needs is derived from those answers: github's App-manifest flow
// puts the organization in the address it sends the operator to and the
// Channel's name, namespace and external base URL in the manifest it POSTs.
// So the parts that depend on answers are FUNCTIONS of them, called by the
// client once it has them, and only the parts that do not — the fallback
// questions, whose names a client must know up front to accept a flag for
// each — are plain data.
type HandoffSpec struct {
	// Begin says where to send the operator, given the answers collected so
	// far and the callback URL the client is listening on.
	//
	// Required on a non-nil HandoffSpec — a kind with nothing to send the
	// operator to returns a nil *HandoffSpec from Handoff instead.
	//
	// ITS ERROR ENDS THE RUN. This is the mirror of FallbackGuidance's rule
	// below, and the opposite answer: a client treats a Begin failure as the
	// run's failure and does NOT fall back to the manual questions. The reason
	// is that the fallback is built by this same kind from these same answers
	// — FallbackGuidance reads them too — so a Begin that could not describe
	// the round trip is a Begin whose guidance cannot describe the manual
	// route either, and entering the detour buys the operator bare prompts
	// plus a note saying the browser step did not finish. github's live case
	// is a mistyped organization login: every question accepted it and only
	// Begin looks at its shape.
	//
	// SO STATE THE ANSWERS' PROBLEM HERE, in words the operator can act on,
	// and do not use an error to mean "skip the round trip". The contract does
	// not forbid I/O in Begin, and a kind that opened a device flow from it
	// would have transient failures that genuinely ARE the fallback's case —
	// a service that is down, a network that is not there. Such a kind must
	// not report those from Begin: it returns a HandoffStart the client then
	// fails to reach, which is the failure the detour is for.
	//
	// callbackURL is the client's own endpoint, passed rather than named by a
	// parameter the client fills in: where it belongs is the SERVICE's
	// business, not a shape every service shares. GitHub reads it out of the
	// POSTed manifest body, an OAuth authorization endpoint reads it from
	// `redirect_uri` on the query string, and a kind that hardcoded one for
	// the other would produce a flow that opens correctly and never comes
	// back.
	//
	// The client owns the CSRF nonce, because the client is what verifies the
	// callback: it sets `state` on the returned URL's query and refuses a
	// callback that does not echo it. A kind neither generates nor checks one.
	Begin func(answers map[string]string, callbackURL string) (HandoffStart, error)

	// Complete exchanges the callback's values for answers, merged into the
	// map Result reads. It runs in-process — server-side in the UI — so it may
	// handle secrets the operator's browser must never see.
	//
	// Required on a non-nil HandoffSpec. A kind whose exchange would do
	// nothing has nothing to hand off: return a nil *HandoffSpec from Handoff
	// instead of a non-nil one with no Complete.
	Complete func(ctx context.Context, callback url.Values) (map[string]string, error)

	// FallbackInputs is what to ask for that the handoff did not supply: every
	// one of them when it could not run at all — a headless host, a blocked
	// port, a service that refused — and whichever ones Complete did not
	// answer when it did. Empty means the handoff is mandatory and the kind
	// fails without it.
	//
	// The second half of that rule is not a hedge, it is github's shape: the
	// App-manifest flow mints the App's identity and secrets, but GitHub only
	// redirects an INSTALLATION back to a caller when the App declares a Setup
	// URL and this one does not, so the installation ID is asked for on every
	// route. A set that were only ever asked as a unit would have to be asked
	// up front instead, before the App it identifies exists.
	//
	// Rendered as a form exactly like Wizard.Inputs, so it must satisfy
	// ValidateInputs too — a Binding here would be silently ignored by every
	// renderer, the same hazard ValidateInputs closes for Inputs. Its Names
	// are also what a client accepts a flag for, which is why they are DATA
	// and not derived from the answers the way everything else here is: the
	// flags are checked before the first question is asked.
	FallbackInputs []oap.Question

	// SatisfiedBy names, for a fallback question whose answer the handoff
	// produces UNDER A DIFFERENT KEY, the derived answer that stands in for
	// it: question name -> derived answer name. Absent for a question the
	// exchange answers by its own name, which is the common case.
	//
	// It exists because the two are the same thing in different FORMS, and
	// only the kind knows which pairs. github's manual route asks for a PATH
	// to the App's downloaded key file, because a multi-line PEM cannot be
	// typed at a line-oriented prompt — but a completed exchange returns the
	// key ITSELF, under `private-key`. Filtering on the question's own name
	// alone leaves `private-key-path` unanswered after a perfectly successful
	// handoff, so the operator is asked for a file that does not exist, on the
	// primary happy path, and every scripted answer after it shifts by one.
	//
	// Declared rather than inferred, and rather than papered over by having
	// Complete return an empty `private-key-path`: a blank standing in for an
	// answer works only while the filter is a presence check, which is a
	// property no reader of the kind can see. Unanswered, below, is the one
	// reading of this field.
	//
	// Every key must name a question in FallbackInputs; the VALUE need not
	// name one at all, since the whole point is a derived answer no question
	// declares.
	SatisfiedBy map[string]string

	// FallbackGuidance is the block shown above the fallback questions that
	// remain to be asked, given every answer known by then — INCLUDING
	// whatever the handoff itself produced. Empty or nil shows nothing.
	//
	// It exists because those questions are routinely unanswerable on their
	// own. An operator creating a GitHub App by hand has to be told the
	// webhook URL, the permissions and the events to enter, none of which they
	// can guess and all of which are derived from answers — so the text cannot
	// travel on the static questions above. Being handed the post-handoff
	// answers is what lets it name the App that was just created rather than a
	// pattern the operator has to substitute into.
	//
	// It is called ONLY when at least one fallback question is actually going
	// to be asked, so a kind may do work here that a completed handoff should
	// not incur — github writes its reference manifest to
	// WizardInput.WorkingDir from inside it.
	//
	// Its error does not end the run: the questions are the floor that always
	// works, and an operator who knows the values must not lose them because
	// the guidance could not be rendered. The client reports it instead.
	FallbackGuidance func(answers map[string]string) (string, error)

	// SkipWhen reports, from the answers the up-front questions collected,
	// that this run has no round trip to MAKE — the reason, in the operator's
	// terms, or "" to make it. Nil never skips.
	//
	// IT IS THE ONLY WAY A KIND CAN DECLINE A ROUND TRIP FROM AN ANSWER, and
	// that is why it exists rather than being expressible through Handoff.
	// Handoff describes the spec BEFORE any question is answered, so a kind
	// whose route is settled at the PROMPT — "do you already have a GitHub
	// App?" — has nothing to decide with at the moment it would have to return
	// nil. Reading the route off the flags instead is what the seeded-only
	// branch does, and it reaches exactly the callers who did not need to be
	// asked.
	//
	// A STAND-DOWN IS NOT A FAILURE and a client must not report it as one.
	// The round trip is what REGISTERS the upstream application; declining it
	// is the operator saying they already have one, so the fallback questions
	// are put to them with this reason above them rather than with "the
	// browser step did not finish". Nothing about the handoff's own failure
	// path changes — that still exists, for a round trip that started and did
	// not come back.
	//
	// The AUTHORITY consequence is the reason to get this right, and it is
	// structural rather than a matter of wording: Complete is the only writer
	// of the provenance an outward-facing write is later authorized by (see
	// AnnotationAppProvisionedBy), and a stood-down round trip never reaches
	// Complete. An application the operator brought therefore cannot be
	// marked as this tool's, by construction rather than by remembering to.
	SkipWhen func(answers map[string]string) string

	// FallbackOpenURL is an address the operator has to VISIT before the
	// fallback questions can be answered, given every answer known by then.
	// Empty or nil is nothing to open, which is the common case.
	//
	// FIRE AND FORGET, and every part of that is load-bearing. It is not a
	// second handoff: there is no callback, no listener and nothing to wait
	// for, and the questions that follow are asked exactly as they would have
	// been. github's is the live case — GitHub redirects an App's INSTALLATION
	// back to a caller only when the App declares a Setup URL, and its
	// manifest deliberately declares none, so the installation ID is asked for
	// afterwards on every route. A client that treated this as a round trip
	// would hang the run waiting for a redirect that is never sent.
	//
	// A REMOTE ADDRESS, unlike the one Begin names, which a client may have to
	// front with a local page because the service wants a POST. This one is
	// navigated to, so a client with a browser opens it directly.
	//
	// HONORED ONLY BY A CLIENT THAT CAN PUT A BROWSER IN FRONT OF THE
	// OPERATOR. A server rendering this wizard has none — opening one there
	// would launch it on the server — so it does nothing, and that is
	// correct rather than a gap: the address also belongs in
	// FallbackGuidance, which every client shows, so this only ever saves the
	// operator a copy and paste. For the same reason a client that tried and
	// failed prints the address and carries on; a browser that will not open
	// is never a reason to lose a run.
	FallbackOpenURL func(answers map[string]string) string
}

// Skipped is why this run has no round trip to make, or "" to make it — the
// one reading of SkipWhen, on the type that declares it, for Unanswered's
// reason: a CLI and a UI that decided this differently would send one
// operator to an external service and not the other, off the same answers.
//
// Nil-safe in both directions, so a client may ask it of any spec.
func (s *HandoffSpec) Skipped(answers map[string]string) string {
	if s == nil || s.SkipWhen == nil {
		return ""
	}
	return strings.TrimSpace(s.SkipWhen(answers))
}

// FallbackOpen is the address to put in front of the operator before the
// fallback questions, or "" for nothing — the one reading of FallbackOpenURL,
// here for the reason Skipped is.
func (s *HandoffSpec) FallbackOpen(answers map[string]string) string {
	if s == nil || s.FallbackOpenURL == nil {
		return ""
	}
	return strings.TrimSpace(s.FallbackOpenURL(answers))
}

// Unanswered is the fallback questions still to be put to the operator, given
// what the handoff produced — in declaration order, and every one of them when
// derived is empty because the round trip never ran.
//
// The rule lives HERE, on the type that declares both halves, rather than in
// each client's own loop: which questions survive an exchange is a semantic of
// this contract, and a CLI and a UI that answered it differently would ask
// different things of the same operator. It is also what lets a kind's own
// test assert what its exchange leaves outstanding without reimplementing the
// reading.
//
// A derived answer counts only when it is NON-BLANK. An exchange that returned
// a key with nothing behind it produced nothing, and treating that as answered
// would skip the question and fail later, in Result, against a key the
// operator was never given the chance to supply.
func (s *HandoffSpec) Unanswered(derived map[string]string) []oap.Question {
	out := make([]oap.Question, 0, len(s.FallbackInputs))
	for _, q := range s.FallbackInputs {
		if answered(derived, q.Name) || answered(derived, s.SatisfiedBy[q.Name]) {
			continue
		}
		out = append(out, q)
	}
	return out
}

// answered reports whether derived carries a usable value for key. An empty
// key — the zero value of a SatisfiedBy lookup that found nothing — never is.
func answered(derived map[string]string, key string) bool {
	if key == "" {
		return false
	}
	return strings.TrimSpace(derived[key]) != ""
}

// HandoffStart is where one handoff sends the operator, computed from the
// answers.
type HandoffStart struct {
	// Explain is shown before the browser opens, so the operator knows what is
	// about to happen and why.
	Explain string

	// URL is the address the operator is sent to. Empty is invalid — a kind
	// with nowhere to send them returns a nil *HandoffSpec from Handoff.
	URL string

	// FormFields, when non-empty, are POSTed to URL as an HTML form rather
	// than navigated to. GitHub's App-manifest flow needs a POST body carrying
	// the manifest JSON; a GET cannot express it.
	FormFields map[string]string
}

// unavailableWizard is the Wizard for a kind whose Channels are built by
// something other than `oap channel create`. Every method refuses with the
// same reason.
type unavailableWizard struct{ reason string }

// UnavailableWizard returns a value that refuses every method of Wizard,
// with reason.
//
// Some kinds build their Channel CR in their own host process (the local TUI,
// the browser web chat) and have no questions to ask. They still need a
// non-nil wizard so that an accidental `oap channel create --kind local` says
// why it cannot run rather than dereferencing nil, and so that it fails
// loudly instead of running a flow that asks nothing.
//
// It returns a VALUE, not a typed-nil pointer: the result is assigned
// straight into Kind.Wizard()'s interface return, where a typed nil would
// compare non-nil and then panic on the first method call.
func UnavailableWizard(reason string) Wizard {
	return unavailableWizard{reason: reason}
}

func (w unavailableWizard) Inputs(context.Context, WizardInput) ([]oap.Question, error) {
	return nil, errors.New(w.reason)
}

func (w unavailableWizard) Handoff(context.Context, WizardInput) (*HandoffSpec, error) {
	return nil, errors.New(w.reason)
}

func (w unavailableWizard) Resolve(context.Context, WizardInput, map[string]string) (map[string]string, error) {
	return nil, errors.New(w.reason)
}

func (w unavailableWizard) Result(WizardInput, map[string]string) (WizardOutput, error) {
	return WizardOutput{}, errors.New(w.reason)
}

type WizardInput struct {
	K8s       client.Client
	Namespace string
	// Role is the ChannelSpec.Role the CALLER decided this Channel must have —
	// a bundle's declared one for `oap agent install` and admind, `--role` for
	// `oap channel create` — or empty to leave whatever the kind's own Result
	// set. wizardrun.Finish stamps it onto the produced Channel.
	//
	// IT IS NOT AN ANSWER AND NEVER WILL BE. No kind's wizard asks an operator
	// for a role, so it reaches the Channel from here or not at all — and the
	// alternative to "not at all" is not "unset": ChannelSpec.Role carries
	// +kubebuilder:default=both, and role=both is deliberately not an
	// output-binding candidate (channelkinds/outputbind). A bundle declaring
	// `role: output` whose kind sets no role of its own would therefore get a
	// Channel that is BOTH, the agent's output binding would resolve to
	// nothing, and the Channel that was supposed to deliver its work would be
	// healthy and never used. slack is exactly that kind.
	//
	// IT IS ON THE INPUT, AND NOT ONLY ON wizardrun.Params, BECAUSE A KIND HAS
	// TO SHAPE ITS QUESTIONS AROUND IT. Stamped after Result and carried
	// nowhere else, the role is invisible to Inputs — so a kind whose Channel
	// needs a field that only a particular role needs cannot ask for it, and
	// the Channel is created without it. slack is that case too: a role=output
	// Slack Channel is somebody else's destination, nothing inbound ever names
	// one for it, and its spec.slack.outputDefaults.channelId was never asked
	// for because the kind could not see that the run was building one.
	//
	// Readable by Inputs, Handoff, Resolve AND Result — unlike NonInteractive,
	// OperatorShell and WorkingDir. Those describe the CLIENT, so manifests
	// that varied with them would be unreproducible from their own answers;
	// the role describes the CHANNEL, and manifests are supposed to depend on
	// what the Channel is.
	Role string
	// Monitoring, when true, causes the wizard to produce a monitoring-role
	// Channel (Role=monitoring, no AgentClass, requires the kind's monitoring
	// destination) instead of an agent channel.
	Monitoring bool
	// Existing is the monitoring Channel being reconfigured (matched by
	// Role==monitoring && Kind), or nil on first setup. Wizards pre-fill
	// non-secret fields from it (e.g. the monitoring channel ID, BotUserID) and
	// reuse its name instead of refusing as a duplicate.
	Existing *spiceboxv1alpha1.Channel
	// CredentialsExist reports whether the existing creds Secret is present, so
	// the wizard can offer keep-on-blank for the bot token.
	CredentialsExist bool
	// NonInteractive reports that nobody is watching this run: it was started
	// with --non-interactive, or by a client that cannot prompt at all.
	//
	// A kind reads it to refuse a step that CANNOT succeed unattended, with its
	// own words. Slack has two: creating an app by hand happens in a browser,
	// and one of its app-configuration token sources waits on a challenge code
	// no flag can supply. Carrying the fact the dispatcher already knows is
	// fewer moving parts than a fifth contract method, and it lets the kind
	// refuse from Inputs (before anything is asked) or from Resolve (before
	// anything is created), whichever is the honest moment.
	//
	// Inputs and Resolve may read it. Result MUST NOT: a manifest that differed
	// by whether a human was watching would make an install unreproducible from
	// its own answers, which is the property Result exists to provide.
	NonInteractive bool
	// OperatorShell reports that this wizard is running in the OPERATOR'S OWN
	// shell — same machine, same filesystem, same terminal, same PATH as the
	// person answering it — rather than on a server rendering the wizard for a
	// browser somewhere else.
	//
	// FALSE IS THE DEFAULT AND THE SAFE ANSWER, for the same reason WorkingDir's
	// empty string is: a client that IS the operator's shell says so
	// (`oap channel create` sets it), and nothing else has to remember to
	// suppress a capability it never had. NonInteractive does not imply it and
	// is not implied by it — a scripted CI run is unattended AND on the
	// operator's own machine; an admin UI form is attended and is not.
	//
	// A kind reads it to gate a step whose cost is paid by whoever is sitting at
	// the terminal. Slack's is the live case: one of its app-configuration token
	// sources shells out to the `slack` CLI, which prints a /slackauthticket
	// command to run inside Slack and then WAITS for the challenge code that
	// comes back. Offering that source is a function of whether a `slack` binary
	// is on the PATH — and under a server that is the serving container's PATH,
	// not the operator's, so a question computed from it offers a route that
	// execs a binary the operator cannot see and blocks on a person who is not
	// there. Probing the host is not the same question as "can this run reach
	// the operator's own machine", and only the client knows the second.
	//
	// Inputs, Handoff and Resolve may read it. Result MUST NOT, for exactly the
	// reason it may not read NonInteractive or WorkingDir: manifests that
	// differed by WHERE the client ran would make an install unreproducible from
	// its own answers.
	OperatorShell bool
	// WorkingDir is a directory this run may leave a file in, or "" when the
	// client has none to offer.
	//
	// EMPTY MEANS DO NOT WRITE, and that is the default deliberately: a client
	// with a filesystem asks for one (`oap channel create` passes "."), rather
	// than a client without one having to remember to suppress a write it
	// never wanted. A server rendering the same wizard has no working
	// directory in any meaningful sense, and the safe behaviour belongs on the
	// side that does nothing.
	//
	// It exists because one step genuinely produces a FILE and not an answer:
	// slack's app manifest, which the operator pastes into api.slack.com and
	// which is far too long to retype out of a terminal. A kind that has
	// nowhere to put it says so in its message instead — see
	// slack.manualRouteRefusal.
	//
	// Readable by Inputs, Handoff and Resolve, NOT by Result, for the same
	// reason NonInteractive is not: manifests must not vary with where the
	// client happened to be run from.
	//
	// Handoff reads it for the same "one step produces a FILE" case, one
	// remove further out: github captures it when it describes its handoff and
	// writes its App reference manifest from inside HandoffSpec.
	// FallbackGuidance, which is the only moment that file is worth anything —
	// the operator is about to fill in GitHub's own form by hand.
	WorkingDir string
	// Seeded is what the client already knows the answer to, keyed the way a
	// Question names it, so a kind can shape the set it declares around what
	// it will not have to ask. Nil is an empty map and reads the same way.
	//
	// A PLAIN MAP, not a client's own answer store. It carries the same
	// key/value pairs whatever the client is — `oap channel create`'s
	// --answer/--name flags, a form the admin UI has already collected — and a
	// kind reading it therefore learns a fact ("this is already answered")
	// rather than acquiring a dependency on the thing that answered it. A
	// terminal type here would put this whole contract back out of a server's
	// reach, which is the one property it exists to have.
	//
	// PRESENCE IS THE ANSWER, including an empty value: `--answer bot-token=`
	// says "I have no token", which is not the same as never having been
	// asked. Read it with the two-value form.
	//
	// Inputs, Handoff and Resolve may read it. Result MUST NOT: every seeded
	// value is in the answer map Result is handed, so consulting this as well
	// would make the manifests depend on HOW an answer arrived rather than on
	// what it was.
	Seeded map[string]string
}

// AnnotationAppProvisionedBy records that a run of this tool created the
// upstream application a Channel talks to — not merely that it was given that
// application's credentials. A wizard stamps it on the Channel it produces
// only on the route where it registered the application itself.
//
// It is what later authorizes an OUTWARD-FACING write against that
// application: a controller may repoint the webhook of an app this tool
// created, and must never touch one an operator registered by hand, which is
// someone else's resource and only ever gets a drift finding.
//
// Because of that it is a stable fact about how the Channel came to be, and
// so an APPLIED field — a pure function of the wizard's inputs, byte-identical
// on a re-run, carrying no timestamp and no run id. An observation would
// belong in controller-owned status instead, and would break SSA's
// re-apply-is-a-no-op property here.
const AnnotationAppProvisionedBy = "agentprimitives.authzed.com/app-provisioned-by"

// AppProvisionedByOAP is AnnotationAppProvisionedBy's value: this tool. A
// constant rather than a literal at each site so the writer (a kind's wizard)
// and the reader (a controller deciding whether it may write upstream) cannot
// drift into disagreeing about what counts as provisioned.
const AppProvisionedByOAP = "oap"

type WizardOutput struct {
	// SecretManifest is nil when the wizard intentionally keeps the existing
	// creds Secret untouched (re-setup where the user left the token blank).
	// The CLI dispatcher must skip applying/replacing the Secret in that case.
	SecretManifest  *corev1.Secret
	ChannelManifest *spiceboxv1alpha1.Channel
	// CapabilityPatch is the server-side-apply payload for the AgentClass whose
	// spec.capabilities the wizard's feature answers imply, or nil when the flow
	// has no AgentClass to patch (monitoring). The CLI applies it under its own
	// field manager, separately from the Channel — the AgentClass is not the
	// wizard's object to create; only the capability keys it asked about are
	// its to own.
	//
	// Unstructured rather than a typed AgentClass, deliberately: an apply sends
	// whatever the object holds, and a typed AgentClass serializes every spec
	// field without omitempty (SystemPrompt among them), which a
	// force-conflicts apply would then take ownership of. Nothing is present
	// unless the wizard put it there.
	//
	// A re-run with the same answers must produce a byte-identical apply: the
	// payload is a pure function of the answers, with no observation,
	// timestamp, or counter in it.
	CapabilityPatch *unstructured.Unstructured
	Notes           []string
	// Summary is what this run DECIDED, as label/value pairs, for the client's
	// post-run summary — distinct from Notes, which is what the operator
	// should do NEXT.
	//
	// It exists because a kind that answers in data runs no code of its own
	// during the run: there is no per-question hook in which it could record
	// what it decided, so anything the operator should be able to read back —
	// which AgentClass was bound, which workspace the tokens belong to — has
	// to be stated here or it is never written at all. Stating it as data
	// keeps it renderable by a non-terminal client too, which the terminal's
	// own note type could not be.
	//
	// The client renders these AFTER whatever the run's own questions
	// recorded, so a line stated here as well as noted by the client would
	// render twice.
	//
	// A Value reaches plain scrollback verbatim, so a kind summarizing a
	// credential masks it first — see SummaryNote.
	Summary []SummaryNote
	// ReplaceExisting is set when the wizard intentionally updates an existing
	// Channel in place (re-setup of the same channel). The CLI dispatcher relaxes
	// its refuse-to-overwrite guard for this case.
	ReplaceExisting bool
}

// SummaryNote is one line of a run's post-run summary: what was decided, in
// the operator's terms.
//
// Deliberately its own type, and not the terminal client's note type — which
// is the same two fields under different names. A kind that named that one
// could only ever be summarized by a terminal, which is the coupling this
// contract exists to remove. The client converts.
//
// VALUE MUST NEVER BE A RAW CREDENTIAL. It reaches plain scrollback verbatim
// — the client writes it to the run's stream with no filtering of any
// kind, and the CLI's summary is the block deliberately left behind after the
// alt-screen is released, so it also outlives the run in the user's terminal
// history. A kind summarizing a token records credmask.Mask(tok) — slack's
// bot and app tokens, on both the agent and the monitoring flow.
//
// The obligation is stated here because it is not structural: nothing between
// this field and the operator's scrollback inspects the value, so a kind that
// forgets the wrapper leaks a live token with nothing to catch it. The same
// hazard is why
// questionscreen.NewScreen keeps NO summary line for a manifest question at
// all — "one of them is a secret, which plain scrollback must never carry."
// A channel wizard needs the line, so it carries the duty instead.
type SummaryNote struct {
	Label string
	// Value is rendered verbatim to plain scrollback. Mask anything secret
	// before it gets here; nothing downstream will.
	Value string
}

// ValidateInputs checks the renderer-relevant subset of oap.Question's rules
// against a Wizard.Inputs (or HandoffSpec.FallbackInputs) result: Name is
// non-empty and unique across the slice, Type is one of the six
// QuestionTypes, a QEnum question carries at least one Enum value, any
// EnumLabels line up with Enum positionally, and Prompt is non-empty. Binding
// MUST be empty — see Wizard.Inputs' doc for why.
//
// Deliberately narrower than oap.Manifest.ValidateQuestions, which additionally
// requires a Binding on every non-secret question and a Secret block
// (CreateSecret and/or OrExisting) on every QSecret question. Both of those
// are bundle-install concepts — where an answer lands in a CR, which Secret
// the installer creates — that do not apply to a channel wizard's answers,
// which Result applies itself, so neither carries over here. A QSecret
// Wizard question means only "render this with echo off"; Secret may be
// nil.
//
// One oap.Manifest.ValidateQuestions rule DOES carry over: a Secret block is
// rejected on any non-QSecret question. That rule is not bundle-install
// specific — it is the same "silently ignored is worse than absent" hazard
// Binding closes, just on the other question-shaping field: Result builds
// whatever Secret a QSecret answer needs itself, so a Secret block on a
// QString/QInt/etc. question would never be read by anything.
//
// Validation MUST be empty, for the same "silently ignored is worse than
// absent" reason as Binding and Secret above. Validation is a CEL rule that
// pkg/platform/oap/install evaluates once, after every question in its batch
// has an answer (Resolve's validateAll, run over the full answer map) — but
// that evaluator, and the answer-typing (typedAnswer) it depends on, are
// unexported there, and no channel-side caller runs an equivalent step. A
// Question carrying Validation would therefore build without error and never
// be checked, which is worse than refusing it outright: the bundle author
// would believe their rule is enforced.
//
// AskWhen, by contrast, IS honored — questionscreen.NewScreen turns it into a
// skipped screen — so it is checked rather than refused: the question it reads
// must be declared before it, and it must name at least one answer. See
// validateAskWhen for what each rule prevents.
//
// This is a fail-closed placeholder, not a permanent design decision. If a
// kind genuinely needs per-question validation, the answer is to build the
// honoring mechanism — an exported evaluator, and a call to it from whatever
// drives these questions to completion — with a test proving it runs, and
// only then lift this rejection. Do not relax it to unblock a kind before
// that mechanism exists; that recreates exactly the silent gap this rule
// closes.
func ValidateInputs(qs []oap.Question) error {
	seen := make(map[string]bool, len(qs))
	for i, q := range qs {
		if q.Name == "" {
			return fmt.Errorf("inputs[%d]: name is required", i)
		}
		if seen[q.Name] {
			return fmt.Errorf("inputs[%d]: duplicate input name %q", i, q.Name)
		}
		seen[q.Name] = true
		switch q.Type {
		case oap.QString, oap.QInt, oap.QBool, oap.QEnum, oap.QSecret, oap.QResourceList:
		default:
			return fmt.Errorf("input %q: unknown type %q", q.Name, q.Type)
		}
		if q.Type == oap.QEnum && len(q.Enum) == 0 {
			return fmt.Errorf("input %q: type=enum requires enum values", q.Name)
		}
		// Held to the same positional-pairing rule a bundle question is, through
		// the same check: a label list that does not line up with Enum labels the
		// wrong rows rather than failing, which is a worse outcome here than in a
		// manifest because a channel input is answered once and applied.
		if err := q.ValidateEnumLabels(); err != nil {
			return err
		}
		if q.Prompt == "" {
			return fmt.Errorf("input %q: prompt is required", q.Name)
		}
		if len(q.Binding) != 0 {
			return fmt.Errorf("input %q: binding is a bundle-install concept; a channel wizard's Result applies the answer directly", q.Name)
		}
		if q.Type != oap.QSecret && q.Secret != nil {
			return fmt.Errorf("input %q: secret block is only valid for type=secret", q.Name)
		}
		if q.Validation != "" {
			return fmt.Errorf("input %q: validation is not evaluated for a channel wizard's inputs (no channel-side caller runs it yet); drop it, or check the answer yourself in Result", q.Name)
		}
		if err := validateAskWhen(q, seen); err != nil {
			return err
		}
	}
	return nil
}

// validateAskWhen checks a branching question's gate against the questions
// declared BEFORE it — seen, which ValidateInputs has already filled with every
// earlier Name (and this question's own).
//
// Every refusal here closes one hazard, and it is the hazard the Binding and
// Validation rules above close, on the field that decides whether a question is
// asked AT ALL: a malformed gate does not fail, it silently removes the
// question from every run — including the one route that needed it, which is
// the only route that would ever have noticed.
//
//   - A gate naming a question that is not earlier in this batch can never be
//     read: answers arrive in declaration order, so the value is not there yet
//     when the gate is evaluated.
//   - A gate naming the question's OWN answer is the same thing at zero
//     distance — the answer does not exist until the question has been asked,
//     and it is not asked until the gate opens. Checked separately from the
//     rule above only because seen already carries this question's own Name by
//     the time this runs, so the "declared before it" test would let it pass.
//   - A gate with no values, and values with no gate, are both a gate that can
//     never open. A kind that means "never ask this" does not declare the
//     question.
func validateAskWhen(q oap.Question, seen map[string]bool) error {
	if q.AskWhen.Question == "" {
		if len(q.AskWhen.In) != 0 {
			return fmt.Errorf("input %q: askWhen names no question to read, so its values gate nothing", q.Name)
		}
		return nil
	}
	if q.AskWhen.Question == q.Name {
		return fmt.Errorf("input %q: askWhen reads its own answer, which does not exist until the question has been asked", q.Name)
	}
	if !seen[q.AskWhen.Question] {
		return fmt.Errorf("input %q: askWhen reads %q, which is not declared before it; answers arrive in declaration order, so this gate would never open",
			q.Name, q.AskWhen.Question)
	}
	if len(q.AskWhen.In) == 0 {
		return fmt.Errorf("input %q: askWhen names %q but no answer to it, so this question could never be asked", q.Name, q.AskWhen.Question)
	}
	return nil
}
