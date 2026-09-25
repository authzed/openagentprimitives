// pkg/web/webui/sessions/view.go is the ONE place the shell decides what a
// selected session's content region renders: the agent-defined UI, the chat
// transcript, or an inline "unavailable" card. The rule is "agent has an
// AgentUI -> the agent-defined UI; otherwise -> the chat transcript", turned
// here into data the browser dispatches on — never a capability question the
// browser asks itself.
package sessions

import (
	"context"
	"errors"
	"net/http"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/web/webui"
	"github.com/authzed/openagentprimitives/pkg/web/webui/agentui"
)

// viewKind discriminates what the content region renders. The value is
// chosen once, server-side, by viewFor; the browser dispatches on it and
// asks no capability question of its own.
type viewKind string

const (
	// viewAgentUI: the agent declares a usable AgentUI.
	viewAgentUI viewKind = "agent-ui"
	// viewChat: the transcript. The DEFAULT — most agents declare no UI, and
	// an ended session renders here read-only whatever its class declares.
	viewChat viewKind = "chat"
	// viewUnavailable: a view was asked for and cannot be shown. Carries the
	// same title/message a *webui.PageError would have rendered as a whole
	// page; the shell renders it inside the content region so chrome, the
	// session list and the escape hatch survive a broken agent UI.
	viewUnavailable viewKind = "unavailable"
)

// chatViewProps is the chat view's bootstrap shape: enough for the content
// region to address the transcript this session already has. Deliberately
// minimal — the transcript is read through the chat data plane
// (pkg/web/webui/chat), gated on the SAME interact standing shellPageBuild
// already confirmed; this struct only names which session to open.
type chatViewProps struct {
	Ns   string `json:"ns"`
	Name string `json:"name"`
}

// unavailableMessage is the copy a broken or unreachable agent-defined view
// renders inline, in place of the view itself. Title/Message come straight
// from the *webui.PageError agentui.ViewFor (or this package's own doors)
// produced — never re-authored here, so the careful Valid=Unknown-vs-False
// distinction resolveAgentUIDoors draws survives verbatim into the shell.
type unavailableMessage struct {
	Title   string `json:"title"`
	Message string `json:"message"`
}

// viewBody is the content region's payload. Kind names which view is SHOWN;
// sibling payloads may also be present so the browser can switch between them
// without a navigation, which would otherwise tear down and rebuild the shell,
// the session list and the approval surface — and take the viewer's unsaved
// view state with it. Nothing reads a sibling payload except the switch.
type viewBody struct {
	// Kind is the single answer to "what renders now".
	Kind viewKind `json:"kind"`
	// UI is the agent-defined view's props; present when it is resolvable.
	UI *agentui.ViewProps `json:"ui,omitempty"`
	// Chat is present whenever this session has a transcript to switch to,
	// which is whenever the viewer got this far — see decideView.
	Chat *chatViewProps `json:"chat,omitempty"`
	// Unavailable REPLACES the content region: there is nothing else to show.
	Unavailable *unavailableMessage `json:"unavailable,omitempty"`
	// Notice sits BESIDE content that is shown — an additive, non-blocking
	// "you asked for something this session cannot offer, here is what you got
	// instead", never a substitute for content. See viewFor's Ended branch,
	// the one case that sets it today.
	Notice *unavailableMessage `json:"notice,omitempty"`
}

// selectedView is the shell's per-session props — scoped to this selection,
// which is why it lives here and not on shellProps: navigating to another
// session drops all of it.
type selectedView struct {
	Ns   string `json:"ns"`
	Name string `json:"name"`
	// SessionOrigin is the wake-ladder branch (agentui.Branch) chrome discloses.
	SessionOrigin string `json:"sessionOrigin"`
	// Chrome is this session's own AgentUI chrome request.
	Chrome agentui.UIChromeRequest `json:"chrome"`
	// OffersAgentUI reports that the sibling-view switch's agent-view half
	// should render, so the chat control is not a one-way trip out of the
	// agent's view recoverable only by editing the URL.
	//
	// False for a class that declares no UI and for an ended session, whose
	// bindings/actions/live routes all answer 410. True whenever a non-ended
	// session's class DECLARES a UI — including a broken one (Valid=False or
	// Unknown, or the AgentUI object missing): the switch still renders and
	// selecting it shows the same unavailable card, rather than hiding that an
	// agent-defined view was supposed to exist here.
	OffersAgentUI bool `json:"offersAgentUI"`
	// EndedReason is why an ended session ended, when it ended in failure:
	// the Failed condition's message, verbatim. Empty for every other
	// selection, including a session that simply finished — an ending with
	// nothing wrong with it has no reason to disclose.
	//
	// It exists because a session that never BOOTED has no transcript to
	// explain itself: the shell's ended line is the only surface the viewer
	// reaches (an ended session's agent-UI page answers 410), and "This
	// session has ended." alone reads as an expiry rather than as an answer
	// to what just happened.
	//
	// The condition's message, not the reason code: the reason is
	// operational vocabulary (WorkshopLimitExceeded), and the message is the
	// sentence whoever wrote the refusal wrote for a person to read.
	EndedReason string `json:"endedReason,omitempty"`

	// View is the content region's payload for this selection.
	View viewBody `json:"view"`

	// There is deliberately NO "canStartReplacement" field: it would be exactly
	// `SessionOrigin == "ended"`, and a bootstrap-frozen copy is unusable
	// anyway — a session that ends while the viewer watches must surface the
	// start-a-replacement control without a reload, so the control gates on the
	// shell's own live answer (SessionShell's `selectedEnded`). A stored copy
	// would be a second source of truth for one fact, and the stale one.
}

// errViewDepsCastFailed reports that Deps does not additionally implement
// agentui.Deps. Unreachable in production: internal/cmd/webd's *artifactViewDeps
// satisfies both (see its compile-time guards in main.go). Handled explicitly
// rather than with a bare type-assertion panic, so a future Deps split that
// drops one of agentui.Deps' methods fails closed with a diagnosable log.
var errViewDepsCastFailed = errors.New("sessions: deps does not implement agentui.Deps")

// splitSessionRef parses the `?session=` query value as "<ns>/<name>" —
// exactly one '/', both halves non-empty. Neither half may itself contain a
// '/', which this format cannot represent and so correctly rejects.
func splitSessionRef(raw string) (ns, name string, ok bool) {
	parts := strings.Split(raw, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}

// normalizeRequestedView maps the `?view=` query value to one of the two
// values viewFor honors, or "" (the capability default) for anything else —
// the same posture Chrome.tsx takes toward an unrecognized string: treat it as
// the ordinary case rather than erroring or guessing.
func normalizeRequestedView(raw string) string {
	switch raw {
	case "chat", "ui":
		return raw
	default:
		return ""
	}
}

// decideView is viewFor's pure decision table, isolated from I/O so the
// branching itself is directly testable. requested is already normalized
// (normalizeRequestedView) to "", "chat", or "ui".
//
//   - requested == "chat" always wins when it can: a session's transcript
//     exists independently of whether its agent's UI is declared, valid, or
//     broken, so an explicit chat request is honored regardless of doorErr.
//   - doorErr != nil means the agent DOES declare a UI but it cannot
//     currently be resolved (missing object, unreconciled, or Valid=False).
//     That is shown inline — never silently downgraded to chat — because a
//     broken agent UI is a fact worth disclosing, not hiding.
//   - avail == UINotDeclared with requested == "ui" is an explicit request
//     for something that does not exist: answered, not silently substituted.
//   - avail == UINotDeclared otherwise is the ordinary case: chat.
//   - avail == UIAvailable with doorErr == nil is the agent-UI view.
func decideView(requested string, avail agentui.Availability, doorErr *webui.PageError, props agentui.ViewProps, ns, name string) viewBody {
	// chat is populated on EVERY body that has a transcript to offer, not only
	// when the transcript is the shown view — that is what lets the browser
	// switch between siblings without a navigation. It costs nothing to always
	// send ({ns, name}, values the body already carries) and discloses nothing:
	// the transcript is read afterwards through the chat data plane under the
	// viewer's own standing, and a viewer who may not interact with this
	// session never reaches this function.
	chat := &chatViewProps{Ns: ns, Name: name}
	switch {
	case requested == "chat":
		b := viewBody{Kind: viewChat, Chat: chat}
		// The agent's view travels alongside so switching back is local too,
		// but only when it is actually resolvable: a broken UI is disclosed by
		// SELECTING it (the doorErr branch below), never pre-loaded as if it
		// worked.
		if avail == agentui.UIAvailable && doorErr == nil {
			ui := props
			b.UI = &ui
		}
		return b
	case doorErr != nil:
		return viewBody{Kind: viewUnavailable, Chat: chat, Unavailable: &unavailableMessage{
			Title: doorErr.Title, Message: doorErr.Message}}
	case avail == agentui.UINotDeclared:
		if requested == "ui" {
			return viewBody{Kind: viewUnavailable, Chat: chat, Unavailable: &unavailableMessage{
				Title:   "No custom view",
				Message: "This agent has no custom view for this session."}}
		}
		return viewBody{Kind: viewChat, Chat: chat}
	default: // agentui.UIAvailable, doorErr == nil
		return viewBody{Kind: viewAgentUI, UI: &props, Chat: chat}
	}
}

// offersAgentUI reports whether the sibling-view switch should render — see
// selectedView.OffersAgentUI's own doc comment for the exact rule.
func offersAgentUI(avail agentui.Availability, doorErr *webui.PageError) bool {
	return doorErr != nil || avail == agentui.UIAvailable
}

// viewFor is the ONE place the capability question is asked. (ns, name) name
// an AgentSession the caller has ALREADY confirmed the viewer may interact
// with — a fully-consistent CheckInteract, gated in shellPageBuild; viewFor
// does not re-check it, mirroring resolveAgentUIDoors' contract.
//
// Order: Get the AgentSession (a NotFound is a 404 *webui.PageError — a
// page-level failure, since there is no session to put chrome around at all);
// resolve its ladder branch (agentui.ResolveSession); if Ended, answer chat
// WITHOUT calling agentui.ViewFor, since an ended session's
// bindings/actions/live routes all answer 410 and the read-only transcript
// does not depend on the declaration. Otherwise agentui.ViewFor walks the
// class -> UI ladder and decideView picks the payload.
func viewFor(ctx context.Context, d Deps, ns, name, requested string) (selectedView, *webui.PageError) {
	var sess spiceboxv1alpha1.AgentSession
	if err := d.K8s().Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &sess); err != nil {
		if apierrors.IsNotFound(err) {
			return selectedView{}, &webui.PageError{Status: http.StatusNotFound, Kind: "notFound",
				Title: "Session not found", Message: "This session could not be found."}
		}
		d.Logger().Error(err, "sessions: get AgentSession failed; returning 500", "ns", ns, "name", name)
		return selectedView{}, &webui.PageError{Status: http.StatusInternalServerError, Kind: "error",
			Title: "Session error", Message: "Could not load this session."}
	}

	res := agentui.ResolveSession(&sess)
	requested = normalizeRequestedView(requested)

	if res.Branch == agentui.Ended {
		body := viewBody{Kind: viewChat, Chat: &chatViewProps{Ns: ns, Name: name}}
		if requested == "ui" {
			// The request is answered, not ignored — but not by replacing the
			// content region: OffersAgentUI is false below, so there is no
			// switch to send the viewer back through, and a viewUnavailable
			// card would be a dead end with no way back to the transcript it
			// names. A Notice beside the still-rendered transcript answers the
			// request without costing the viewer the one thing this session can
			// still show them.
			body.Notice = &unavailableMessage{
				Title:   "Session ended",
				Message: "This session has ended, so its agent-defined view is no longer available. Its transcript is shown below."}
		}
		return selectedView{
			Ns: ns, Name: name,
			SessionOrigin: string(res.Branch),
			OffersAgentUI: false,
			EndedReason:   endedReasonFor(&sess),
			View:          body,
		}, nil
	}

	// Widened here, not on Deps itself: agentui.ViewFor needs the fuller
	// agentui.Deps surface (Memory/Artifacts/…, the bindings-route
	// collaborators that ViewFor's own read path does not use but that
	// interface's single cast contract includes), and internal/cmd/webd's umbrella
	// already implements it alongside sessions.Deps. A cast failure means an
	// implementation satisfies this package's interface but not agentui.Deps —
	// a configuration bug, not a viewer's fault.
	au, ok := d.(agentui.Deps)
	if !ok {
		d.Logger().Error(errViewDepsCastFailed,
			"sessions: cannot resolve the agent-defined view; deps does not implement agentui.Deps", "ns", ns, "name", name)
		return selectedView{}, &webui.PageError{Status: http.StatusInternalServerError, Kind: "error",
			Title: "Session error", Message: "Could not load this session."}
	}

	props, chrome, avail, doorErr := agentui.ViewFor(ctx, au, ns, name)
	if doorErr != nil {
		// The underlying cause is already logged inside agentui.ViewFor with
		// ns/class/ui/reason; this line only records that the shell rendered it
		// inline rather than dropping the rest of the page.
		d.Logger().Info("sessions: agent-defined view unavailable; rendering inline in the content region",
			"ns", ns, "name", name, "status", doorErr.Status, "title", doorErr.Title)
	}

	return selectedView{
		Ns: ns, Name: name,
		SessionOrigin: string(res.Branch),
		Chrome:        chrome,
		OffersAgentUI: offersAgentUI(avail, doorErr),
		View:          decideView(requested, avail, doorErr, props, ns, name),
	}, nil
}

// endedReasonFor returns the sentence a failed session ended with, when that
// sentence was written for the person to read — otherwise "".
//
// Three things must hold, and each rules out a different wrong disclosure:
//
//   - Phase is Failed. A condition left behind by a recovered failure must not
//     be presented as the reason a later, clean ending happened.
//   - The condition is TRUE. A Failed condition set False records that the
//     session is NOT failing; its message describes a state that is over.
//   - The reason is one whose message is copy
//     (v1alpha1.FailureReasonWrittenForThePerson). Most Failed messages are
//     written for an operator and name pods, CR kinds and retry budgets —
//     forwarding those into the chrome would put that vocabulary in front of
//     a person who can neither read nor act on it. A reason outside the
//     allowlist leaves the shell's plain "This session has ended." sentence,
//     which is true of every one of them.
//
// This is deliberately STRICTER than pkg/web/webui/chat's failureMessageOf,
// which reads the same condition for the live transcript's terminal notice:
// that surface reaches a session that RAN, beside the transcript that gives
// the message its context. This one is the only thing a session that never
// booted ever shows.
func endedReasonFor(sess *spiceboxv1alpha1.AgentSession) string {
	if sess.Status.Phase != spiceboxv1alpha1.AgentSessionPhaseFailed {
		return ""
	}
	c := apimeta.FindStatusCondition(sess.Status.Conditions, spiceboxv1alpha1.AgentSessionConditionFailed)
	if c == nil || c.Status != metav1.ConditionTrue {
		return ""
	}
	if !spiceboxv1alpha1.FailureReasonWrittenForThePerson(c.Reason) {
		return ""
	}
	return c.Message
}
