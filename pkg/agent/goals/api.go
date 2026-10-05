package goals

type Request struct {
	Operation string        `json:"operation"`
	Resource  string        `json:"resource,omitempty"`
	Create    CreateRequest `json:"create,omitempty"`
	Change    Change        `json:"change,omitempty"`
	List      ListRequest   `json:"list,omitempty"`
	ID        string        `json:"id,omitempty"`
}
type Response struct {
	Resource           string `json:"resource"`
	Goal               *Goal  `json:"goal,omitempty"`
	Page               *Page  `json:"page,omitempty"`
	ExecutionAvailable bool   `json:"executionAvailable"`
}
