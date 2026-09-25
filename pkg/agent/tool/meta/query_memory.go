package meta

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/memory"
)

func NewQueryMemory() tool.Tool {
	return &queryMemoryTool{}
}

type queryMemoryTool struct{}

func (*queryMemoryTool) Name() string    { return "query_memory" }
func (*queryMemoryTool) Kind() tool.Kind { return tool.KindMeta }
func (*queryMemoryTool) Permission() authz.Permission {
	return authz.Permission{StateImpact: authz.Stateless}
}
func (*queryMemoryTool) PermissionVariants() []authz.PermissionVariant { return nil }

func (*queryMemoryTool) Description() string {
	return "Search memory entries. By default queries this session's entries. " +
		"Pass `session` (e.g. \"default/other-session\") to query a different session " +
		"you have been granted access to. " +
		"It reads ONE session's scope and never a resource pool — for entries stored on " +
		"a resource this session has access to, use `search_memory`, whose results span " +
		"those pools and report the scope each entry came from. " +
		"Use `kinds` to filter by entry type (e.g. \"turn\", \"lifecycle\", \"label\"). " +
		"Use `tags` to require specific tags (AND semantics). " +
		"Use `field_filters` for content field queries (requires postgres backend). " +
		"Results are ordered newest-first by default. " +
		"The response's `truncated` field reports whether `limit` cut the read short: " +
		"when it is true, `count` is a page size and not a total — re-ask with a larger " +
		"`limit` before drawing a conclusion from the set."
}

func (*queryMemoryTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type":"object",
		"additionalProperties":false,
		"properties":{
			"session":{
				"type":"string",
				"description":"Session to query (namespace/name). Omit to query this session. Only sessions explicitly granted to this agent are accessible."
			},
			"kinds":{
				"type":"array",
				"items":{"type":"string"},
				"description":"Filter by entry kind (e.g. turn, lifecycle, label, authz_decision)"
			},
			"tags":{
				"type":"array",
				"items":{"type":"string"},
				"description":"Require all listed tags (AND semantics)"
			},
			"field_filters":{
				"type":"array",
				"items":{
					"type":"object",
					"properties":{
						"path":{"type":"string","description":"Content field path (e.g. outcome, reason)"},
						"op":{"type":"string","enum":["eq","lt","lte","gt","gte","is_nil","not_nil"],"default":"eq"},
						"value":{"type":"string","description":"Comparison value (not needed for is_nil/not_nil)"}
					},
					"required":["path"]
				},
				"description":"Filter by content fields (honored by postgres backend; dropped by inmem)"
			},
			"limit":{
				"type":"integer",
				"minimum":1,
				"maximum":100,
				"default":20,
				"description":"Max entries to return"
			}
		}
	}`)
}

type queryMemoryArgs struct {
	// Session scopes the query; empty means this session's own scope.
	Session string `json:"session"`
	// Kinds and Tags narrow the result set; empty means no narrowing on that axis.
	Kinds []string `json:"kinds"`
	Tags  []string `json:"tags"`
	// FieldFilters are ANDed structured predicates over entry content.
	FieldFilters []queryFieldFilter `json:"field_filters"`
	// Limit caps returned entries; zero takes the tool's default.
	Limit int `json:"limit"`
}

// queryFieldFilter is one structured predicate over an entry's content.
type queryFieldFilter struct {
	// Path is a dotted field path into the entry's content object.
	Path string `json:"path"`
	// Op is the comparison operator to apply at Path.
	Op string `json:"op"`
	// Value is the operand, always carried as a string and coerced per Op.
	Value string `json:"value"`
}

// queryMemoryResult is the JSON the tool renders back to the model.
type queryMemoryResult struct {
	Entries []queryMemoryEntry `json:"entries"`
	// Count is len(Entries), not the total matching the query before Limit.
	// Truncated says which of the two this is.
	Count int `json:"count"`
	// Truncated reports that `limit` ended the read before the matches did, so
	// the model can tell a page from a total and re-ask with a larger limit
	// instead of reasoning over a partial set as if it were complete. Always
	// emitted: a field that appears only when true reads as absent-means-false
	// on one hand and unsupported on the other.
	Truncated bool `json:"truncated"`
	// DroppedPredicates names field filters the backend could not evaluate, so
	// the model can tell an unfiltered result from a genuinely empty one.
	DroppedPredicates []string `json:"dropped_predicates,omitempty"`
}

// queryMemoryEntry is one memory entry as the model sees it.
type queryMemoryEntry struct {
	// Kind is the memory kind name (turn, transcript, …).
	Kind      string   `json:"kind"`
	ID        string   `json:"id"`
	CreatedAt string   `json:"created_at"`
	Tags      []string `json:"tags,omitempty"`
	// Content is the entry body verbatim; omitted when the kind stores none.
	Content json.RawMessage `json:"content,omitempty"`
}

func (t *queryMemoryTool) Execute(ctx context.Context, raw json.RawMessage, sess *tool.SessionContext) (tool.Result, error) {
	var a queryMemoryArgs
	if res, ok := tool.ParseArgs(raw, &a, t.Name(), `{"kinds":["turn"],"limit":10}`); !ok {
		return res, nil
	}
	if sess == nil || sess.Mem == nil {
		return tool.Result{Content: "query_memory: memory not available", IsError: true, Trusted: true}, nil
	}
	if a.Limit <= 0 {
		a.Limit = 20
	}
	if a.Limit > 100 {
		a.Limit = 100
	}

	sessionID := sess.Namespace + "/" + sess.Name
	if a.Session != "" {
		sessionID = a.Session
	}
	scope := memory.Scope{Kind: "session", ID: sessionID}
	q := memory.Query{
		Scope:   scope,
		Kinds:   a.Kinds,
		Tags:    a.Tags,
		OrderBy: memory.OrderBy{Field: "createdAt", Desc: true},
		Limit:   a.Limit,
	}
	for _, ff := range a.FieldFilters {
		q.FieldEquals = append(q.FieldEquals, memory.FieldFilter{
			Path:  ff.Path,
			Op:    memory.FieldOp(ff.Op),
			Value: ff.Value,
		})
	}

	res, err := sess.Mem.Query(ctx, q)
	if err != nil {
		// Trust is decided by the error's provenance, not by the fact that a
		// framework tool produced it: a memory sentinel (ErrInvalidQuery's
		// "did you mean", a capability refusal) is platform-authored and must
		// reach the model; a backend error is not, because the graphiti
		// providers interpolate up to 1KiB of the upstream HTTP response body
		// into theirs. See recallError.
		return recallError(t.Name(), err), nil
	}

	out := queryMemoryResult{
		Count:             len(res.Entries),
		Truncated:         res.Truncated,
		DroppedPredicates: res.DroppedPredicates,
	}
	for _, e := range res.Entries {
		out.Entries = append(out.Entries, queryMemoryEntry{
			Kind:      e.Kind,
			ID:        e.ID,
			CreatedAt: e.CreatedAt.Format("2006-01-02T15:04:05Z"),
			Tags:      e.Tags,
			Content:   e.Content,
		})
	}

	b, err := json.Marshal(out)
	if err != nil {
		return tool.Result{Trusted: true}, fmt.Errorf("query_memory: marshal: %w", err)
	}
	// Trusted is deliberately left false: this payload is stored Entry.Content
	// relayed verbatim — `turn` entries hold raw channel text and tool_result
	// blocks, and `session` lets the agent read a scope authored entirely
	// outside this session. Result.Trusted means "framework-CONTROLLED
	// content", not "a framework tool returned it"; meta tools bypass the
	// PostToolCall pipeline, so leaving this false is what makes the runner
	// run the content inspectors over it (Loop.inspectUntrustedResult).
	// Same reasoning as read_thread_history — see renderHistoryResponse.
	return tool.Result{Content: string(b)}, nil
}
