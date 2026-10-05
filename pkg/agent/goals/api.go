package goals

import (
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
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
}
type Response struct {
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
}

// ReadDependencies carries source authority through a goal read into the
// caller's session. The private domain is an additional audience boundary,
// rather than a replacement for the sources behind the returned content.
func (r Response) ReadDependencies() []Source {
	var sources []Source
	addGoal := func(g Goal) { sources = mergeSources(sources, g.Sources) }
	addRun := func(o Occurrence) { sources = mergeSources(sources, o.ReadDependencies()) }
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
