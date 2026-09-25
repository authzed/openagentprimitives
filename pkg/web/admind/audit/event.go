// Package audit normalizes memory entries from the audit-producing Kinds
// into one cross-session Event shape, via a per-memory-kind Mapper
// registry (repo convention: new variants register, consumers don't
// branch). The admind engine queries memory cross-scope and maps through
// this package.
package audit

import (
	"encoding/json"
	"strings"
	"time"
)

// Event is the normalized audit record the admin UI consumes.
type Event struct {
	// Time is when the underlying memory entry was recorded, and the sort key.
	Time time.Time `json:"time"`
	// SessionNamespace and SessionName identify the owning session; both are
	// empty for an entry whose scope id was not "ns/name" shaped.
	SessionNamespace string `json:"sessionNamespace"`
	SessionName      string `json:"sessionName"`
	// AgentClass is enriched by the engine; empty when the session is gone —
	// audit entries outlive sessions by design.
	AgentClass string `json:"agentClass,omitempty"`
	// Kind is the mapper that produced this event, and what the UI facets on.
	Kind string `json:"kind"` // approval | tool_call | authz_decision | scope_change | leakage | rel_writes | lifecycle | tool_session
	// Actor is the canonical user subject responsible; empty for an
	// agent-driven event with no human behind it.
	Actor string `json:"actor,omitempty"`
	// Tool is the tool involved; empty for a non-tool event.
	Tool string `json:"tool,omitempty"`
	// Outcome is the mapper's verdict word (allowed/denied/…), for filtering.
	Outcome string `json:"outcome"`
	// Summary is one line of human copy for the row.
	Summary string `json:"summary"`
	// EntryID is the underlying memory entry's id, for the detail drawer.
	EntryID string `json:"entryId"`
	// Raw is the original entry content, shown in the drawer; omitted from
	// list responses to keep them small.
	Raw json.RawMessage `json:"raw,omitempty"`
}

// splitScopeID splits a session scope id "ns/name" into its parts.
func splitScopeID(id string) (ns, name string) {
	if ns, name, ok := strings.Cut(id, "/"); ok {
		return ns, name
	}
	return "", id
}
