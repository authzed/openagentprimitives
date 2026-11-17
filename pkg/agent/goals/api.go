package goals

import (
	"time"

	"github.com/authzed/openagentprimitives/pkg/authz/plangate"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/plangateaudit"
)

// PlanApprovalRequest names an already frozen plan in the authenticated root's
// durable log. The caller cannot supply a grant, owner, or approval record.
type PlanApprovalRequest struct {
	Digest string `json:"digest"`
	Phase  int    `json:"phase"`
}

type Request struct {
	Discovery    DiscoveryRequest                          `json:"discovery,omitempty"`
	PlanApproval *PlanApprovalRequest                      `json:"planApproval,omitempty"`
	Reply        *channelevents.OutboundUserMessagePayload `json:"reply,omitempty"`
	Proposal     RunProposal                               `json:"proposal,omitempty"`
	Operation    string                                    `json:"operation"`
	Resource     string                                    `json:"resource,omitempty"`
	Create       CreateRequest                             `json:"create,omitempty"`
	Change       Change                                    `json:"change,omitempty"`
	Execution    ExecutionRequest                          `json:"execution,omitempty"`
	List         ListRequest                               `json:"list,omitempty"`
	ID           string                                    `json:"id,omitempty"`
	// Scope selects whose goals a request addresses: "" or "private" (the
	// verified human's own) or "group" (the members of this conversation).
	// The owning group itself is always resolved server-side from the
	// session's binding, never from this field.
	Scope string `json:"scope,omitempty"`
	// TurnRef names the inbound that started the caller's current turn. It
	// only selects which signed attestation to consult; the server never
	// takes it as evidence of who spoke.
	TurnRef string `json:"turnRef,omitempty"`
}

type Response struct {
	ExecutionTerms     *ExecutionTerms                          `json:"executionTerms,omitempty"`
	EventFeeds         []EventFeed                              `json:"eventFeeds,omitempty"`
	DiscoveryPolicy    *DiscoveryPolicy                         `json:"discoveryPolicy,omitempty"`
	DiscoveryProposal  *DiscoveryProposal                       `json:"discoveryProposal,omitempty"`
	PlanApproval       *plangateaudit.ApprovalAuthority         `json:"planApproval,omitempty"`
	Approval           *channelevents.InteractionRequestPayload `json:"approval,omitempty"`
	Resource           string                                   `json:"resource"`
	Run                *Occurrence                              `json:"run,omitempty"`
	Runs               *RunPage                                 `json:"runs,omitempty"`
	Goal               *Goal                                    `json:"goal,omitempty"`
	Page               *Page                                    `json:"page,omitempty"`
	ExecutionAvailable bool                                     `json:"executionAvailable"`
	ExecutionPolicy    *ExecutionPolicy                         `json:"executionPolicy,omitempty"`
}

// ExecutionPolicy is guidance for proposing limits, not execution authority.
// Every request still requires validation and approval of its resolved terms.
type ExecutionPolicy struct {
	DefaultBounds ExecutionBounds `json:"defaultBounds"`
	MaxBounds     ExecutionBounds `json:"maxBounds"`
	ServerTime    time.Time       `json:"serverTime"`
}

// ReadDependencies carries source authority through a goal read into the
// caller's session. The private domain is an additional audience boundary,
// rather than a replacement for the sources behind the returned content.
func (r Response) ReadDependencies() []Source {
	var sources []Source
	addGoal := func(g Goal) { sources = mergeSources(sources, g.Sources) }
	addRun := func(o Occurrence) { sources = mergeSources(sources, o.ReadDependencies()) }
	for _, feed := range r.EventFeeds {
		sources = mergeSources(sources, feed.Sources)
	}
	if r.Goal != nil {
		addGoal(*r.Goal)
	}
	if r.Page != nil {
		for _, g := range r.Page.Goals {
			addGoal(g)
		}
	}
	if r.Run != nil {
		addRun(*r.Run)
	}
	if r.Runs != nil {
		for _, o := range r.Runs.Runs {
			addRun(o)
		}
	}
	if r.DiscoveryPolicy != nil {
		addGoal(r.DiscoveryPolicy.Template)
	}
	if r.DiscoveryProposal != nil {
		addGoal(r.DiscoveryProposal.Goal)
		for _, dep := range r.DiscoveryProposal.Observation.Dependencies {
			sources = mergeSources(sources, []Source{{ResourceType: dep.ResourceType, ResourceID: dep.ResourceID, Permission: dep.Permission}})
		}
	}
	return sources
}

// RunDecisionIngressCheck asks the goal server, as channelsd, whether a
// decision on Session may arrive on the session's ordinary inbound subject.
type RunDecisionIngressCheck struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

// RunDecisionIngress answers RunDecisionIngressCheck. Group is true only for
// the live run of a group-owned goal, read from its occurrence.
type RunDecisionIngress struct {
	Group bool `json:"group"`
}

// ManageRequest is channelsd's call to the goal server's management API on
// behalf of a person a channel kind has verified (Viewer, canonical). It is
// accepted only with channelsd's own token: Viewer is channelsd's word.
type ManageRequest struct {
	Operation string `json:"operation"`
	Viewer    string `json:"viewer"`
	Namespace string `json:"namespace"`
	// Class narrows a list to one AgentClass's goals.
	Class string `json:"class,omitempty"`
	// Goal names one goal, "agent_goal:<id>", for plan and act.
	Goal   string `json:"goal,omitempty"`
	Action string `json:"action,omitempty"`
	Limit  int    `json:"limit,omitempty"`
}

// ManageResponse answers a ManageRequest.
type ManageResponse struct {
	Goals []GoalSummary  `json:"goals,omitempty"`
	Total int            `json:"total,omitempty"`
	Plan  *plangate.Plan `json:"plan,omitempty"`
	// Approval is the operator-signed consent request an act (resume, renew)
	// leaves for the viewer to decide: channelsd records the viewer's
	// decision on it exactly as for a consent card, and commits it.
	Approval *memory.Entry `json:"approval,omitempty"`
}
