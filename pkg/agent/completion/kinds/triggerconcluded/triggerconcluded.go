// Package triggerconcluded registers the `trigger-status-concluded` completion
// requirement: a session whose trigger has a status surface may not declare its
// work complete while that surface still carries no answer.
//
// This is the observed defect. A review agent read the pull request, delivered
// its findings to the thread, and ended its session one step early — so the
// pull request sat showing "Started 4 minutes ago — This check has started…"
// permanently, for a review that had in fact finished. Both halves of the
// contradiction were already in the system when agent_work_complete arrived:
// the session knew its trigger had a status surface, and it knew nothing had
// been written back to it. Nothing compared them.
//
// The recorded bypass stays, and is the point: an agent that genuinely cannot
// answer its trigger — a provider outage, a refusal — says why, and the reason
// reaches a human. What this makes impossible is forgetting SILENTLY.
package triggerconcluded

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/pkg/agent/completion"
	"github.com/authzed/openagentprimitives/pkg/agent/session/state/triggerstatus"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	chregistry "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/registry"
)

// Key is the value an AgentClass declares in spec.completionRequirements.
const Key = "trigger-status-concluded"

func init() { completion.Register(Requirement{}) }

// Requirement is the registered trigger-status-concluded kind.
type Requirement struct{}

func (Requirement) Key() string { return Key }

// Title is user copy — it appears on the bypass notice a person reads — so it
// names the obligation in the reader's terms and not the objects behind it.
func (Requirement) Title() string { return "the event that started this agent gets an answer" }

// Check reports whether this session's trigger carries an answer.
//
// Two facts, from two places that each own theirs. WHETHER there is a surface
// to answer comes from the session's INPUT binding resolved through the one
// shared channel-kind lookup — the same gate that decided whether the
// trigger-status tools were offered at all, so the requirement can never demand
// a call the agent was never given. WHETHER it was answered comes from the
// session's own trigger-status record, which those tools write.
//
// It deliberately does not ask the provider. channelkinds.TriggerSurface has no
// read-only query: Claim reports an existing answer but also OPENS a claim when
// there is none, so a check that asked would put a pull request into "in
// progress" as a side effect of asking whether it was finished.
func (Requirement) Check(ctx context.Context, in completion.Input) (completion.Finding, error) {
	sess := in.Session
	if sess == nil {
		return completion.Finding{}, errors.New("no session context")
	}
	c, ok := sess.K8sClient.(client.Client)
	if !ok || c == nil {
		// Fail-closed rather than "met": a session that cannot read its own
		// input binding cannot establish that it had nothing to answer, and
		// reporting satisfaction would make the requirement inert exactly where
		// it is unverifiable.
		return completion.Finding{}, errors.New(
			"no Kubernetes client on this session, so its input channel binding cannot be read")
	}

	var cr spiceboxv1alpha1.AgentSession
	if err := c.Get(ctx, types.NamespacedName{Namespace: sess.Namespace, Name: sess.Name}, &cr); err != nil {
		return completion.Finding{}, fmt.Errorf("read AgentSession %s/%s: %w", sess.Namespace, sess.Name, err)
	}

	b := cr.Spec.InputChannel
	if b == nil {
		// A kubectl-driven session: no channel event started it, so there is
		// no trigger and nothing is owed. Met, not an error.
		return completion.Finding{Met: true}, nil
	}
	// THE shared lookup. A kind whose trigger is just a message — every
	// conversational kind — reports nothing here, and a class that declares this
	// requirement anyway must not wedge on it: those sessions were never offered
	// a way to answer.
	reporter, ok := chregistry.TriggerStatusReporterFor(b.Kind)
	if !ok {
		return completion.Finding{Met: true}, nil
	}

	store, ok := triggerstatus.TryFrom(sess)
	if !ok {
		// Fail-closed for the same reason as the missing client: the class
		// declared this requirement, and a session carrying no trigger-status
		// record cannot answer whether the trigger was concluded.
		return completion.Finding{}, errors.New("this session carries no trigger-status record")
	}
	if _, concluded := store.Concluded(); concluded {
		return completion.Finding{Met: true}, nil
	}
	// Names no terminal tool, for the reason artifactdelivery's Missing records:
	// which one to call again belongs to the refusal, which knows whether this
	// session finishes through agent_work_complete or return_result.
	return completion.Finding{Missing: fmt.Sprintf(
		"This session was started by %s, and nothing has been written back to it — whoever is watching "+
			"it still sees this work as in progress, and will go on seeing that after you finish. Call "+
			"conclude_trigger_status with an `outcome` (one of %s) and a short `summary` a person can "+
			"read there.",
		reporter.TriggerSurfaceKind(), legalOutcomes()),
	}, nil
}

// legalOutcomes renders the seam's own closed set for the refusal message.
// Derived from TriggerOutcomes, never retyped: a value this names and the tool
// then refuses would be a gate proposing an answer that cannot be given.
func legalOutcomes() string {
	all := channelkinds.TriggerOutcomes()
	out := make([]string, 0, len(all))
	for _, o := range all {
		out = append(out, string(o))
	}
	return strings.Join(out, ", ")
}
