package goals

import "github.com/authzed/openagentprimitives/pkg/channels/channelevents"

type Request struct {
	Reply     *channelevents.OutboundUserMessagePayload `json:"reply,omitempty"`
	Proposal  RunProposal                               `json:"proposal,omitempty"`
	Operation string                                    `json:"operation"`
	Resource  string                                    `json:"resource,omitempty"`
	Create    CreateRequest                             `json:"create,omitempty"`
	Change    Change                                    `json:"change,omitempty"`
	Execution ExecutionRequest                          `json:"execution,omitempty"`
	List      ListRequest                               `json:"list,omitempty"`
	ID        string                                    `json:"id,omitempty"`
}
type Response struct {
	Resource           string      `json:"resource"`
	Run                *Occurrence `json:"run,omitempty"`
	Runs               *RunPage    `json:"runs,omitempty"`
	Goal               *Goal       `json:"goal,omitempty"`
	Page               *Page       `json:"page,omitempty"`
	ExecutionAvailable bool        `json:"executionAvailable"`
}
