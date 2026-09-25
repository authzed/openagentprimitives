// Package artifactdelivery registers the `artifact-delivered` completion
// requirement: a session may not declare its work complete while it owns a
// Ready ArtifactRender that no respond_to_user ever attached.
//
// This is the observed defect. A review agent rendered its report, told the
// user "full report attached below", passed no `attached` field, and finished
// clean. The bytes existed and were Ready; nothing had carried them anywhere.
// The system already held both halves of that contradiction at the moment
// agent_work_complete arrived — it simply never compared them.
//
// # Why a live-view offer does not count
//
// artifact_offer_view hands the user a button that opens the artifact in a
// browser, and a later session raised the obvious question: an offer the user
// can click did reach them, so should it satisfy this? It does not, for two
// independent reasons.
//
// The runner cannot know an offer RENDERED. The tool publishes an envelope and
// returns; the sender runs in another process — channelsd, or the user's own
// oap for a client-hosted kind — and no result travels back. Counting the offer
// would therefore be counting the CALL, which is the same mistake as counting a
// render going Ready: the system would once again mistake "we tried" for "they
// have it".
//
// And an offer carries no bytes. The button mints a fresh signed link at CLICK
// time against webd, refuses a viewer without interact permission, and points
// at a view of a session that is about to end. `attached` is the only path that
// puts a durable copy in front of a person, which is why it stays the only
// thing this requirement counts — and why an agent should send it AND offer the
// view, rather than choosing between them.
//
// # Delivery to a USER
//
// Because the requirement counts exactly one call, it holds only a session that
// HAS that call. A session with no user-facing delivery path — respond_to_user
// withheld, or no channel at all — was never offered a way to answer, and is
// reported satisfied rather than made to bypass on every finish. See
// respondToUserOffered for the predicate and why it is not "is this a delegated
// child".
package artifactdelivery

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/pkg/agent/completion"
	"github.com/authzed/openagentprimitives/pkg/agent/session/state/deliveries"
	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	chregistry "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/registry"
)

// Key is the value an AgentClass declares in spec.completionRequirements.
const Key = "artifact-delivered"

func init() { completion.Register(Requirement{}) }

// Requirement is the registered artifact-delivered kind.
type Requirement struct{}

func (Requirement) Key() string { return Key }

// Title is user copy — it appears on the bypass notice a person reads — so it
// names the obligation and not the objects behind it.
func (Requirement) Title() string { return "every file the agent produced reaches you" }

// Check lists the Ready ArtifactRenders this session owns and reports any the
// delivery record does not account for.
//
// Ownership is filtered by owner reference rather than by name prefix: renders
// are namespace-scoped and a namespace hosts many sessions, so the ownerRef is
// the only thing that says WHOSE a render is. Non-Ready renders are ignored —
// a render still working, or one that failed, is not an undelivered result, and
// failing on it would refuse completion for something the agent cannot fix.
func (Requirement) Check(ctx context.Context, in completion.Input) (completion.Finding, error) {
	sess := in.Session
	if sess == nil {
		return completion.Finding{}, errors.New("no session context")
	}
	c, ok := sess.K8sClient.(client.Client)
	if !ok || c == nil {
		// Fail-closed rather than "met": a session with no client cannot
		// establish that it delivered anything, and reporting satisfaction
		// would make the requirement inert exactly where it is unverifiable.
		return completion.Finding{}, errors.New("no Kubernetes client on this session, so rendered artifacts cannot be listed")
	}

	offered, err := respondToUserOffered(ctx, c, sess)
	if err != nil {
		return completion.Finding{}, err
	}
	if !offered {
		// Met, for the reason triggerconcluded returns Met when the binding's
		// kind reports no trigger surface: those sessions were never offered a
		// way to answer. This requirement is satisfied by ONE act —
		// respond_to_user naming the handle in `attached` — and a session that
		// was not given that tool could only ever finish by bypassing, every
		// single time, which is an off switch wearing a requirement's clothes.
		//
		// It does not follow that the artifact goes undelivered. A delegated
		// child hands its artifacts to its PARENT through return_result, and
		// the parent is the session that has a user: it delivers what the
		// child returned, asks again if nothing came back, or produces its
		// own. Whether it is OBLIGED to is its own class's
		// completionRequirements, which is where the obligation belongs.
		return completion.Finding{Met: true}, nil
	}

	log, ok := deliveries.TryFrom(sess)
	if !ok {
		return completion.Finding{}, errors.New("this session carries no artifact-delivery record")
	}

	var list spiceboxv1alpha1.ArtifactRenderList
	if err := c.List(ctx, &list, client.InNamespace(sess.Namespace)); err != nil {
		return completion.Finding{}, fmt.Errorf("list artifact renders: %w", err)
	}

	var undelivered []string
	for i := range list.Items {
		cr := &list.Items[i]
		if cr.Status.Phase != spiceboxv1alpha1.ArtifactRenderPhaseReady {
			continue
		}
		if !tool.OwnedBySession(cr.OwnerReferences, sess) {
			continue
		}
		if log.Delivered(cr.Name) {
			continue
		}
		undelivered = append(undelivered, describe(cr))
	}
	if len(undelivered) == 0 {
		return completion.Finding{Met: true}, nil
	}
	sort.Strings(undelivered) // stable message across List orderings

	// The artifact_offer_view clause is not padding: an agent refused here often
	// HAS offered a live view of the very artifact named, so without being told
	// why that did not count, its next move is to call it again and be refused
	// again.
	//
	// It names no terminal tool. Which one to call again is the REFUSAL's line
	// to write, not a requirement's: the caller knows whether this session
	// finishes through agent_work_complete or, for a delegated child, through
	// return_result, and a requirement naming one would tell half of them to
	// call a tool they were never offered.
	return completion.Finding{Missing: fmt.Sprintf(
		"%d artifact(s) you rendered were never delivered to the user: %s. "+
			"A render being ready only means the bytes exist — nothing reaches the user until a "+
			"respond_to_user call names the handle in its `attached` array, e.g. "+
			`{"text":"…","attached":[%q]}. Calling artifact_offer_view does not deliver it: `+
			"that offers a browser view, which carries no copy and cannot be confirmed to have reached anyone. "+
			"Send the reply with the attachment.",
		len(undelivered), strings.Join(undelivered, "; "), firstName(undelivered)),
	}, nil
}

// respondToUserOffered reports whether this session has the user-facing
// delivery path the requirement names.
//
// The predicate is "respond_to_user is offered to this session", NOT "this
// session is a delegated child". The two coincide in tree today and are not
// the same statement: what the requirement depends on is the presence of the
// call that answers it, and keying on the delegation would leave a future
// session-to-session shape — or a session with no channel at all — demanding a
// tool it does not have while a delegated child that regained a human surface
// went unchecked.
//
// It is derived from the two facts the runner's channel_interaction capability
// derives it from, and one of them through the very same registry lookup:
//
//   - No channel binding at all (a kubectl-driven session) means the
//     capability contributes no channel tools whatsoever.
//   - A binding whose kind reaches another AgentSession means respond_to_user
//     is WITHHELD (chregistry.FirstSessionCounterparty, the shared derivation
//     the capability's own respondToUserSkip calls), because a message
//     arriving that way lands in the other session's transcript uninspected.
//
// An unregistered kind is an ERROR, not an exemption, and that is deliberate:
// treating an unanswerable question as "never offered a way to answer" would
// widen the exemption from a requirement to a failure mode. The session then
// refuses completion and can still finish by bypassing — with a stated reason
// a human reads — which is the loud outcome a wiring bug deserves.
func respondToUserOffered(ctx context.Context, c client.Client, sess *tool.SessionContext) (bool, error) {
	var cr spiceboxv1alpha1.AgentSession
	if err := c.Get(ctx, types.NamespacedName{Namespace: sess.Namespace, Name: sess.Name}, &cr); err != nil {
		return false, fmt.Errorf("read AgentSession %s/%s: %w", sess.Namespace, sess.Name, err)
	}
	if cr.Spec.InputChannel == nil {
		return false, nil
	}
	// Never nil once InputChannel is: OutboundBinding falls back to it.
	outbound := spiceboxv1alpha1.OutboundBinding(&cr)
	b, reaches, err := chregistry.FirstSessionCounterparty(cr.Spec.InputChannel.Kind, outbound.Kind)
	if err != nil {
		return false, fmt.Errorf(
			"this session's %s channel kind %q is not registered in this binary, so whether it can deliver to a user could not be established: %w",
			b.Role, b.Kind, err)
	}
	return !reaches, nil
}

// describe renders one undelivered artifact as the handle the model must pass
// back, plus enough detail to tell two apart.
func describe(cr *spiceboxv1alpha1.ArtifactRender) string {
	parts := []string{cr.Name}
	if cr.Status.OutputFilename != "" {
		parts = append(parts, cr.Status.OutputFilename)
	}
	if cr.Status.OutputMIME != "" {
		parts = append(parts, cr.Status.OutputMIME)
	}
	if len(parts) == 1 {
		return parts[0]
	}
	return parts[0] + " (" + strings.Join(parts[1:], ", ") + ")"
}

// firstName is the bare handle of the first entry, for the worked example in
// the message. describe may have appended a parenthesised detail; the example
// has to be something the model can paste.
func firstName(described []string) string {
	if len(described) == 0 {
		return ""
	}
	return strings.TrimSuffix(strings.SplitN(described[0], " (", 2)[0], " ")
}
