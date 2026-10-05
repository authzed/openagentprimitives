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
	PlanApproval       *plangateaudit.ApprovalAuthority         `json:"planApproval,omitempty"`
	Approval           *channelevents.InteractionRequestPayload `json:"approval,omitempty"`
	Resource           string                                   `json:"resource"`
	Run                *Occurrence                              `json:"run,omitempty"`
	Runs               *RunPage                                 `json:"runs,omitempty"`
	Goal               *Goal                                    `json:"goal,omitempty"`
	Page               *Page                                    `json:"page,omitempty"`
	ExecutionAvailable bool                                     `json:"executionAvailable"`
}
