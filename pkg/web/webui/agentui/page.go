package agentui

import (
	"context"
	"fmt"
	"net/http"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/web/webui"
)

// agentUIDoors is resolveAgentUIDoors' success result: the session -> class
// -> UI resolution ladder's own findings, gathered from a SINGLE walk so
// ViewFor's props and bindingsHandler's declaration parsing are guaranteed to
// agree on which AgentSession, AgentClass, and AgentUI a request (ns, name)
// resolves to.
type agentUIDoors struct {
	AgentClass string
	UI         *spiceboxv1alpha1.AgentUI
	Resolution Resolution

	// Grant is the AgentClass's own agentUI grant (spec.agentUI), carried out
	// of the same Get that found ac.Spec.AgentUI.Ref so viewOptions
	// (viewmodel.go) can compute the same ceiling the AgentUI reconciler
	// observes onto status.eligibleTools.
	Grant *spiceboxv1alpha1.AgentClassUIGrant
}

// doorsOutcome is walkAgentUIDoors' raw result: agentUIDoors plus the one
// fork resolveAgentUIDoors and ViewFor disagree on. NoUIDeclared is true
// exactly when the AgentClass exists but ac.Spec.AgentUI is nil — the
// resolved AgentClass name is still set (in agentUIDoors.AgentClass) so a
// caller that wants to name it (resolveAgentUIDoors' 404 copy) can, without
// re-fetching.
type doorsOutcome struct {
	agentUIDoors
	NoUIDeclared bool
}

// walkAgentUIDoors is the session -> class -> UI resolution ladder's ONE
// implementation. resolveAgentUIDoors and ViewFor both wrap it, so a door
// added or a bug fixed here cannot land in one caller and not the other.
//
// It stops SHORT of the "no spec.agentUI" verdict, the one door with two
// legitimate readings: a 404 for the API routes, which have nothing to serve,
// versus the ordinary "show chat instead" for the session shell. Every other
// door has one correct answer regardless of caller, so it lives here.
//
// PRECONDITION: the caller has already confirmed the viewer may interact with
// (ns, name). CheckInteract is not re-checked here.
func walkAgentUIDoors(ctx context.Context, d Deps, ns, name string) (doorsOutcome, *webui.PageError) {
	// The NAMED session decides which AgentClass — and so which AgentUI —
	// this ladder resolves. Never a search for "the most recent session for
	// this class"; see ResolveSession for why that breaks navigation.
	var sess spiceboxv1alpha1.AgentSession
	if err := d.K8s().Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &sess); err != nil {
		if apierrors.IsNotFound(err) {
			return doorsOutcome{}, &webui.PageError{Status: http.StatusNotFound, Kind: "notFound",
				Title: "Session not found", Message: "This session could not be found."}
		}
		d.Logger().Error(err, "agentui: get AgentSession failed; returning 500", "ns", ns, "name", name)
		return doorsOutcome{}, &webui.PageError{Status: http.StatusInternalServerError, Kind: "error",
			Title: "Session error", Message: "Could not load this session."}
	}
	agentClass := sess.Spec.Class

	// Walking this ladder never creates a session. A UI is always
	// session-scoped, so Ended is a 410, never success with an empty session;
	// starting a replacement is a separate, explicit POST. The session shell's
	// viewFor makes its own earlier Ended check and never reaches here for
	// that branch — this 410 is what the three API routes answer.
	res := ResolveSession(&sess)
	if res.Branch == Ended {
		return doorsOutcome{}, &webui.PageError{Status: http.StatusGone, Kind: "expired",
			Title: "Session ended", Message: "This session has ended. Start a new one to continue."}
	}

	var ac spiceboxv1alpha1.AgentClass
	if err := d.K8s().Get(ctx, client.ObjectKey{Namespace: ns, Name: agentClass}, &ac); err != nil {
		d.Logger().Error(err, "agentui: get AgentClass failed; returning 500", "ns", ns, "class", agentClass)
		return doorsOutcome{}, &webui.PageError{Status: http.StatusInternalServerError, Kind: "error",
			Title: "Agent error", Message: "Could not load this agent's configuration."}
	}

	if ac.Spec.AgentUI == nil {
		return doorsOutcome{agentUIDoors: agentUIDoors{AgentClass: agentClass, Resolution: res}, NoUIDeclared: true}, nil
	}

	var aui spiceboxv1alpha1.AgentUI
	if err := d.K8s().Get(ctx, client.ObjectKey{Namespace: ns, Name: ac.Spec.AgentUI.Ref}, &aui); err != nil {
		if apierrors.IsNotFound(err) {
			return doorsOutcome{}, &webui.PageError{Status: http.StatusNotFound, Kind: "notFound",
				Title: "UI unavailable", Message: "This agent's UI isn't available right now."}
		}
		d.Logger().Error(err, "agentui: get AgentUI failed; returning 500",
			"ns", ns, "class", agentClass, "ref", ac.Spec.AgentUI.Ref)
		return doorsOutcome{}, &webui.PageError{Status: http.StatusInternalServerError, Kind: "error",
			Title: "Agent error", Message: "This agent's UI isn't available right now."}
	}

	// Valid=Unknown and a missing condition are NOT the same failure as
	// Valid=False. False is a VERDICT: the reconciler parsed the declaration
	// and rejected it, in the author's own vocabulary ("slots[0]: unknown
	// component ap:bogus"), so showing it helps the person who can fix it.
	// Unknown and nil mean "not known yet" — unreconciled since creation
	// (every fresh install passes through this) or a failed AgentClass read.
	// Both clear on their own, and the Unknown message embeds a raw client-go
	// error (API-server URL, resource path, dial failure) that no user-facing
	// surface may carry, so the cause goes to the log instead.
	cond := meta.FindStatusCondition(aui.Status.Conditions, spiceboxv1alpha1.AgentUIConditionValid)
	switch {
	case cond == nil || cond.Status == metav1.ConditionUnknown:
		reason, detail := "NotReconciled", "the Valid condition is not set yet"
		if cond != nil {
			reason, detail = cond.Reason, cond.Message
		}
		d.Logger().Info("agentui: AgentUI validity is undetermined; returning 503",
			"ns", ns, "class", agentClass, "ui", aui.Name, "reason", reason, "detail", detail)
		return doorsOutcome{}, &webui.PageError{Status: http.StatusServiceUnavailable, Kind: "info",
			Title: "UI not ready yet", Message: "This agent's UI isn't ready yet. Try again in a moment."}
	case cond.Status != metav1.ConditionTrue:
		msg := "This UI is not valid."
		if cond.Message != "" {
			msg = cond.Message
		}
		return doorsOutcome{}, &webui.PageError{Status: http.StatusUnprocessableEntity, Kind: "error",
			Title: "UI not ready", Message: msg}
	}

	return doorsOutcome{agentUIDoors: agentUIDoors{AgentClass: agentClass, UI: &aui, Resolution: res, Grant: ac.Spec.AgentUI}}, nil
}

// resolveAgentUIDoors walks the ladder via walkAgentUIDoors and answers "no
// spec.agentUI" with a 404 naming the class — right for its only caller,
// resolveView, which serves the three API routes and has nothing to show for a
// UI-less agent. ViewFor walks the SAME ladder and answers that one door
// differently, since the shell's ordinary case is to show chat instead.
//
// PRECONDITION: the caller has already confirmed the viewer may interact with
// (ns, name). CheckInteract is not re-checked here.
func resolveAgentUIDoors(ctx context.Context, d Deps, ns, name string) (agentUIDoors, *webui.PageError) {
	out, pe := walkAgentUIDoors(ctx, d, ns, name)
	if pe != nil {
		return agentUIDoors{}, pe
	}
	if out.NoUIDeclared {
		return agentUIDoors{}, &webui.PageError{Status: http.StatusNotFound, Kind: "notFound",
			Title:   "No UI for this agent",
			Message: fmt.Sprintf("Agent %q does not declare a UI.", out.AgentClass)}
	}
	return out.agentUIDoors, nil
}
