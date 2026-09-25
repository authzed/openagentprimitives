//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"strings"

	v1 "github.com/authzed/authzed-go/proto/authzed/api/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/spicedb"
	fakekind "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/fake"
	pkgmemory "github.com/authzed/openagentprimitives/pkg/memory"
	turnkind "github.com/authzed/openagentprimitives/pkg/memory/kinds/turn"
)

// dumpState renders a structured snapshot of the harness's state for a
// timeout failure: channel outbound events, approval prompts captured
// by the fake sub-channel sender, the LLM rule script's served-request
// log, the cluster's AgentSession status, and (when at least one
// AgentSession exists) the SpiceDB grant tuples scoped to it.
//
// Pure read; safe to call on a failure path. All errors are swallowed
// on purpose — this is a diagnostic appended to a t.Fatalf message, and
// a partial dump is always more useful than no dump. AGENTS.md's "never
// silently drop errors" rule explicitly carves out failure-path
// diagnostics (the caller has already decided the test is doomed).
func (h *Harness) dumpState() string {
	var b strings.Builder
	ctx := context.Background()

	// 1. Channel outbound events + approval prompts (best-effort:
	// requires exactly one Channel CR, which the conversation API
	// already assumes; multi-channel dumps would need a richer helper).
	if ch := h.maybeSingleChannel(); ch != nil {
		// The Channel's own conditions, first. A session that never appears is
		// usually a binding the operator refused rather than anything the test
		// did, and the pipeline's servability gate reads exactly these — so
		// without them a refused inbound looks identical to a lost one.
		fmt.Fprintf(&b, "Channel %s/%s conditions:\n", ch.Namespace, ch.Name)
		for _, c := range ch.Status.Conditions {
			fmt.Fprintf(&b, "  %s=%s reason=%s msg=%q\n", c.Type, c.Status, c.Reason, c.Message)
		}
		if ch.Spec.AgentClass != "" {
			var ac spiceboxv1alpha1.AgentClass
			if err := h.K8s.Get(ctx, client.ObjectKey{
				Namespace: ch.Namespace, Name: ch.Spec.AgentClass,
			}, &ac); err == nil {
				fmt.Fprintf(&b, "AgentClass %s/%s conditions:\n", ac.Namespace, ac.Name)
				for _, c := range ac.Status.Conditions {
					fmt.Fprintf(&b, "  %s=%s reason=%s msg=%q\n", c.Type, c.Status, c.Reason, c.Message)
				}
			} else {
				fmt.Fprintf(&b, "AgentClass %s/%s: get failed: %v\n", ch.Namespace, ch.Spec.AgentClass, err)
			}
		}
		if drv := fakekind.DriverFor(ch.Namespace, ch.Name); drv != nil {
			sent := drv.Sent()
			fmt.Fprintf(&b, "Channel outbound events (%d):\n", len(sent))
			for i, m := range sent {
				fmt.Fprintf(&b, "  [%d] %q\n", i, m.Text)
			}
			joins := drv.SessionJoinPrompts()
			fmt.Fprintf(&b, "Session-join prompts (%d):\n", len(joins))
			for i, p := range joins {
				fmt.Fprintf(&b, "  [%d] requester=%s/%s/%s startedBy=%s/%s/%s session=%s/%s preview=%q\n",
					i,
					p.Payload.Requester.Kind, p.Payload.Requester.ExternalID, p.Payload.Requester.Email,
					p.Payload.StartedBy.Kind, p.Payload.StartedBy.ExternalID, p.Payload.StartedBy.Email,
					p.Payload.AgentSessionRef.Namespace, p.Payload.AgentSessionRef.Name,
					p.Payload.Preview)
			}
			// Generic Interaction model prompts — credential_link, identity_choice,
			// portal_access, and (post Tasks 9/10) permission_request all funnel
			// through this single queue now (see pkg/channels/channelevents/interaction.go).
			// The Session-join dump above stays structurally empty for
			// permission_request since nothing publishes the legacy
			// KindPermissionRequest anymore; this is where that category's
			// requests actually show up.
			interactions := drv.InteractionPrompts()
			fmt.Fprintf(&b, "Interaction prompts (%d):\n", len(interactions))
			for i, p := range interactions {
				fmt.Fprintf(&b, "  [%d] category=%s requestRef=%s session=%s/%s lead=%q\n",
					i,
					p.Payload.Category, p.Payload.RequestRef,
					p.Payload.AgentSessionRef.Namespace, p.Payload.AgentSessionRef.Name,
					p.Payload.Lead)
			}
		} else {
			fmt.Fprintf(&b, "Channel %s/%s: no fake driver registered yet\n",
				ch.Namespace, ch.Name)
		}
	} else {
		fmt.Fprintln(&b, "Channel: (no single Channel CR found; skipping outbound/prompt dump)")
	}

	// 2b. Inbound-message drain state. Placed before the LLM dump because it
	// answers the question the LLM dump provokes: when a turn served no
	// requests, this says whether the message even arrived, and whether any
	// runner consumed it.
	if h.memStore != nil {
		if ns, name := h.sessionRefOrEmpty(); name != "" {
			ctxSys := pkgmemory.WithSystemApproval(ctx, "e2e-diagnostics")
			if turns, err := turnkind.ReadAll(ctxSys, h.memStore,
				pkgmemory.Scope{Kind: "session", ID: ns + "/" + name}); err == nil {
				fmt.Fprintf(&b, "\nInbound (mid-session) messages: %s\n", describeInboxTurns(turns))
			} else {
				fmt.Fprintf(&b, "\nInbound (mid-session) messages: unreadable: %v\n", err)
			}
		}
	}

	// 2. LLM scripted-rule state.
	if h.LLM != nil {
		reqs := h.LLM.Requests()
		fmt.Fprintf(&b, "\nLLM requests served (%d):\n", len(reqs))
		for i, req := range reqs {
			fmt.Fprintf(&b, "  [%d]\n%s\n", i, describeRequest(req))
		}
	}

	// 3. AgentSession status (one session per test in the typical
	// conversation API; loop just in case).
	var sessions spiceboxv1alpha1.AgentSessionList
	if err := h.K8s.List(ctx, &sessions); err == nil {
		fmt.Fprintf(&b, "\nAgentSessions (%d):\n", len(sessions.Items))
		for i := range sessions.Items {
			sess := &sessions.Items[i]
			fmt.Fprintf(&b, "  %s/%s phase=%q\n",
				sess.Namespace, sess.Name, sess.Status.Phase)
			// The wake pair, together, because only their ORDER matters and
			// neither is interpretable alone. The operator respawns a parked
			// runner iff wake-requested-at is strictly after lastWakeAt — but
			// the two come from different clocks at different moments:
			// channelsd stamps the annotation when the message ARRIVES, the
			// operator stamps lastWakeAt when it RECONCILES. A follow-up that
			// lands inside that window is dropped with no error, so an inbound
			// sitting undrained next to `requested <= lastWake` is the whole
			// diagnosis in one line.
			fmt.Fprintf(&b, "    wake: requested=%q lastWake=%v\n",
				sess.Annotations[spiceboxv1alpha1.AnnotationWakeRequestedAt],
				sess.Status.LastWakeAt)
			for _, cond := range sess.Status.Conditions {
				fmt.Fprintf(&b, "    %s=%s reason=%s message=%q\n",
					cond.Type, cond.Status, cond.Reason, cond.Message)
			}
		}
	} else {
		fmt.Fprintf(&b, "\nAgentSessions: list failed: %v\n", err)
	}

	// 4. SpiceDB grant tuples on the active session (best-effort: scope
	// to agentsession:<ns>/<name> so we don't dump unrelated bootstrap
	// tuples).
	if h.SpiceDB != nil && len(sessions.Items) > 0 {
		sess := &sessions.Items[0]
		key := sess.Namespace + "/" + sess.Name
		dumpRelationships(&b, h.SpiceDB, ctx, &v1.RelationshipFilter{
			ResourceType:       "agentsession",
			OptionalResourceId: key,
		}, fmt.Sprintf("agentsession:%s", key))
	}

	// 5. SpiceDB tuples on the fixture's permission-model resources.
	// Dump these unconditionally — a missing wildcard tuple here is the
	// most common cause of "approval fired when it shouldn't have"
	// timeouts, and dumping per-resource-type keeps the output legible.
	if h.SpiceDB != nil {
		for _, rt := range []string{"crm_company", "crm_owner"} {
			dumpRelationships(&b, h.SpiceDB, ctx, &v1.RelationshipFilter{
				ResourceType: rt,
			}, rt)
		}
	}

	return b.String()
}

// dumpRelationships writes all relationships matching the filter to b
// under the given label. Best-effort: swallows errors per dumpState's
// "always emit something" contract.
func dumpRelationships(b *strings.Builder, cli *spicedbClientFacade, ctx context.Context, filter *v1.RelationshipFilter, label string) {
	stream, err := cli.ReadRelationships(ctx, &v1.ReadRelationshipsRequest{
		RelationshipFilter: filter,
	})
	if err != nil {
		fmt.Fprintf(b, "\nSpiceDB ReadRelationships on %s failed: %v\n", label, err)
		return
	}
	fmt.Fprintf(b, "\nSpiceDB relationships on %s:\n", label)
	count := 0
	for {
		resp, recvErr := stream.Recv()
		if recvErr != nil {
			break
		}
		rel := resp.Relationship
		fmt.Fprintf(b, "  %s:%s#%s @ %s:%s",
			rel.Resource.ObjectType, rel.Resource.ObjectId,
			rel.Relation,
			rel.Subject.Object.ObjectType, rel.Subject.Object.ObjectId)
		if rel.Subject.OptionalRelation != "" {
			fmt.Fprintf(b, "#%s", rel.Subject.OptionalRelation)
		}
		fmt.Fprintln(b)
		count++
	}
	if count == 0 {
		fmt.Fprintln(b, "  (none)")
	}
}

// spicedbClientFacade is the subset of *spicedb.Client dumpRelationships
// uses. Declared as an interface so dumpState can pass h.SpiceDB without
// any concrete-vs-interface friction in this package's import graph.
type spicedbClientFacade = spicedb.Client

// maybeSingleChannel returns the single Channel CR in the namespace, or
// nil if zero or many exist. Unlike singleChannel(), this never fatals —
// it's the diagnostic-path variant for dumpState's "best effort" reads.
func (h *Harness) maybeSingleChannel() *spiceboxv1alpha1.Channel {
	var channels spiceboxv1alpha1.ChannelList
	if err := h.K8s.List(context.Background(), &channels); err != nil {
		return nil
	}
	if len(channels.Items) != 1 {
		return nil
	}
	return &channels.Items[0]
}

// sessionRefOrEmpty is the non-fatal sibling of SessionRef, for use from
// dumpState.
//
// SessionRef fatals when there is not exactly one session, which is right for a
// test asserting on a conversation and wrong here: dumpState runs on a path
// that is ALREADY reporting a failure, and a t.Fatal from inside the diagnostic
// would replace the real failure message with a complaint about the
// diagnostic. Same reasoning as maybeSingleChannel above.
func (h *Harness) sessionRefOrEmpty() (ns, name string) {
	var sessions spiceboxv1alpha1.AgentSessionList
	if err := h.K8s.List(context.Background(), &sessions); err != nil {
		return "", ""
	}
	if len(sessions.Items) != 1 {
		return "", ""
	}
	return sessions.Items[0].Namespace, sessions.Items[0].Name
}
