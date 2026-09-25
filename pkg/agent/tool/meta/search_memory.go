package meta

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/memory"
)

func NewSearchMemory() tool.Tool {
	return &searchMemoryTool{}
}

type searchMemoryTool struct{}

// SearchMemoryToolName is the tool's name, exported because the runner's
// read-declaration wiring names this tool specifically: it is the one tool
// whose result can span several memory pools, and the declaration is built-in
// rather than CRD-settable.
const SearchMemoryToolName = "search_memory"

func (*searchMemoryTool) Name() string    { return SearchMemoryToolName }
func (*searchMemoryTool) Kind() tool.Kind { return tool.KindMeta }
func (*searchMemoryTool) Permission() authz.Permission {
	return authz.Permission{StateImpact: authz.Stateless}
}
func (*searchMemoryTool) PermissionVariants() []authz.PermissionVariant { return nil }

func (*searchMemoryTool) Description() string {
	return "Search memory using natural language. Combines full-text search, " +
		"vector similarity, and structured filters across search providers. " +
		"Results are ranked by relevance. " +
		"Results may include entries from resource pools this session has access to, " +
		"alongside this session's own entries; every entry reports the `scope` it came from, " +
		"so you can say which resource a fact belongs to. " +
		"Which scopes are searched is decided by what this session has access to. " +
		"Every entry reports `created_at` (UTC, fixed width, so two are ordered by " +
		"comparing them as strings) — ranking is by relevance, not recency, so when " +
		"two entries contradict each other that field, not their order or score, " +
		"says which one is current. " +
		"The response's `truncated` field reports whether `limit` cut the ranking short: " +
		"when it is true there are further matches below the ones returned."
}

func (*searchMemoryTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type":"object",
		"additionalProperties":false,
		"properties":{
			"text":{
				"type":"string",
				"description":"Natural language search query"
			},
			"kinds":{
				"type":"array",
				"items":{"type":"string"},
				"description":"Filter by entry kind (e.g. entity, fact, turn)"
			},
			"tags":{
				"type":"array",
				"items":{"type":"string"},
				"description":"Require all listed tags (AND semantics)"
			},
			"scopes":{
				"type":"array",
				"items":{"type":"string"},
				"description":"Leave unset. The scopes searched are this session's own plus the resource pools it has access to, decided by the server."
			},
			"limit":{
				"type":"integer",
				"minimum":1,
				"maximum":100,
				"default":20,
				"description":"Max results to return"
			}
		}
	}`)
}

type searchMemoryArgs struct {
	Text   string   `json:"text"`
	Kinds  []string `json:"kinds"`
	Tags   []string `json:"tags"`
	Scopes []string `json:"scopes"`
	Limit  int      `json:"limit"`
}

type searchMemoryResult struct {
	Entries []searchMemoryEntry `json:"entries"`
	Count   int                 `json:"count"`
	// Truncated reports that `limit` ended this page before the matches did.
	// See queryMemoryResult.Truncated for why it is always emitted.
	Truncated      bool     `json:"truncated"`
	DroppedFilters []string `json:"dropped_filters,omitempty"`
}

type searchMemoryEntry struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
	// Scope names where this entry lives — the session's own scope, or a
	// resource pool the session reached through a slot grant.
	//
	// Emitted for EVERY entry, session-scoped ones included, and never
	// omitempty. An envelope where the field's absence means "this session" is
	// one a later reader gets wrong exactly once, and the readers here are the
	// per-datum tagging path — which would then mint a pool's entry under the
	// session's audience — and the model, which can only say which resource a
	// fact came from if every fact says.
	//
	// The memory.Scope value itself rather than a "<kind>/<id>" string: the
	// type already has a wire form, and a string would need a split that
	// memory.ResourceRef exists precisely to keep callers out of.
	Scope memory.Scope `json:"scope"`
	// CreatedAt is authorship time, in the same fixed UTC layout query_memory
	// emits, so a reader meeting both envelopes parses one format and may
	// order any two entries by comparing the strings.
	//
	// It is what lets a reader decide which of two CONTRADICTING entries is
	// current — a note and the later note meant to cancel it. Score cannot
	// answer that: this tool ranks by relevance, and the more relevant of two
	// opposed notes is as often the stale one. Emitted for every entry and
	// never omitempty, for the reason the Scope field above gives: a reader
	// that has to treat an absent field as "unknown" gets the one case that
	// matters wrong.
	//
	// Second precision, matching query_memory exactly. Two entries written
	// inside one second sort ambiguously; that is a real limit, and it is the
	// sibling tool's limit too — the alternative is two tools whose timestamps
	// disagree in width, which is worse for a reader comparing them.
	CreatedAt string          `json:"created_at"`
	Score     float64         `json:"score"`
	Source    string          `json:"source"`
	Content   json.RawMessage `json:"content,omitempty"`
}

// SearchResultScope is one scope's slice of a search result.
type SearchResultScope struct {
	// Scope the entries came from.
	Scope memory.Scope
	// Result is a search-result envelope holding ONLY this scope's entries,
	// in the same shape the tool returns, so a consumer parses one format.
	Result string
}

// SplitSearchResultByScope parses a search_memory result and returns one group
// per scope, in order of first appearance in the ranking.
//
// It lives here because this package owns the envelope. The per-datum tagging
// path needs each pool's entries SEPARATELY — a tag for one pool whose stored
// content carried another pool's entries would put those entries behind the
// wrong audience — and re-declaring the envelope at the consumer would give the
// repo two definitions of one wire format, free to drift apart.
//
// An unreadable envelope is an error, never an empty list: "no pools" and "I
// could not tell" must not be the same answer, since the caller treats the
// first as nothing-to-tag.
func SplitSearchResultByScope(result string) ([]SearchResultScope, error) {
	var parsed searchMemoryResult
	if err := json.Unmarshal([]byte(result), &parsed); err != nil {
		return nil, fmt.Errorf("search_memory: parsing result: %w", err)
	}
	var out []SearchResultScope
	// Grouped by the scope VALUE, which is comparable and is what the consumer
	// keys on; index into out so first-appearance order survives.
	at := map[memory.Scope]int{}
	grouped := map[memory.Scope]*searchMemoryResult{}
	for _, e := range parsed.Entries {
		if _, ok := at[e.Scope]; !ok {
			at[e.Scope] = len(out)
			out = append(out, SearchResultScope{Scope: e.Scope})
			// Truncated is carried verbatim: it is a fact about the ranking
			// this slice came out of, and a slice of a truncated search is
			// just as incomplete as the search was.
			grouped[e.Scope] = &searchMemoryResult{Truncated: parsed.Truncated}
		}
		g := grouped[e.Scope]
		g.Entries = append(g.Entries, e)
		g.Count = len(g.Entries)
	}
	for sc, i := range at {
		b, err := json.Marshal(grouped[sc])
		if err != nil {
			return nil, fmt.Errorf("search_memory: re-encoding the %s/%s slice: %w", sc.Kind, sc.ID, err)
		}
		out[i].Result = string(b)
	}
	return out, nil
}

func (t *searchMemoryTool) Execute(ctx context.Context, raw json.RawMessage, sess *tool.SessionContext) (tool.Result, error) {
	var a searchMemoryArgs
	if res, ok := tool.ParseArgs(raw, &a, t.Name(), `{"text":"example query","limit":10}`); !ok {
		return res, nil
	}
	if sess == nil || sess.Mem == nil {
		return tool.Result{Content: "search_memory: memory not available", IsError: true, Trusted: true}, nil
	}
	if a.Limit <= 0 {
		a.Limit = 20
	}
	if a.Limit > 100 {
		a.Limit = 100
	}

	sessionScope := memory.Scope{Kind: "session", ID: sess.Namespace + "/" + sess.Name}
	scopes := []memory.Scope{sessionScope}
	for _, s := range a.Scopes {
		parts := strings.SplitN(s, "/", 2)
		if len(parts) == 2 {
			scopes = append(scopes, memory.Scope{Kind: parts[0], ID: parts[1]})
		}
	}

	req := memory.SearchRequest{
		Scopes: scopes,
		Text:   a.Text,
		Kinds:  a.Kinds,
		Tags:   a.Tags,
		Limit:  a.Limit,
	}

	res, err := sess.Mem.Search(ctx, req)
	if err != nil {
		if errors.Is(err, memory.ErrNoSearchProviders) {
			// Same trust verdict recallError would reach, with copy that points
			// the model at the tool that still works.
			return tool.Result{
				Content: "search_memory: no search providers configured; use query_memory for structured queries",
				IsError: true,
				Trusted: true,
			}, nil
		}
		// Trust is decided by the error's provenance, not by the fact that a
		// framework tool produced it: a memory sentinel (ErrInvalidQuery's
		// "did you mean", a capability refusal) is platform-authored and must
		// reach the model; a provider error is not, because the graphiti
		// provider interpolates up to 1KiB of the upstream HTTP response body
		// into it. See recallError.
		return recallError(t.Name(), err), nil
	}

	out := searchMemoryResult{Count: len(res.Entries), Truncated: res.Truncated}
	// Flattened in ascending PROVIDER NAME order, not in map-iteration order.
	//
	// res.PerProvider is a Go map, so ranging it directly put the dropped
	// filters of a multi-provider search into the model's result in a different
	// sequence on every call — two identical searches, two different results.
	// Sorting the provider names rather than the flattened list keeps each
	// provider's own ordering of its filters, which that provider chose, and
	// makes only the concatenation deterministic.
	//
	// This is the third unordered collection found in a model-facing meta-tool
	// result; see TestMetaToolResults_AreDeterministic, which is what found it.
	var allDropped []string
	for _, name := range slices.Sorted(maps.Keys(res.PerProvider)) {
		allDropped = append(allDropped, res.PerProvider[name].DroppedFilters...)
	}
	out.DroppedFilters = allDropped

	for _, se := range res.Entries {
		out.Entries = append(out.Entries, searchMemoryEntry{
			Kind: se.Entry.Kind,
			ID:   se.Entry.ID,
			// queryMemoryEntry's layout, spelled the same way rather than
			// shared through a helper: the two are one decision, and a
			// reader comparing the envelopes should find them identical.
			CreatedAt: se.Entry.CreatedAt.Format("2006-01-02T15:04:05Z"),
			Scope:     se.Entry.Scope,
			Score:     se.Score,
			Source:    se.Source,
			Content:   se.Entry.Content,
		})
	}

	b, err := json.Marshal(out)
	if err != nil {
		return tool.Result{Trusted: true}, fmt.Errorf("search_memory: marshal: %w", err)
	}
	// Trusted is deliberately left false: these are stored entries relayed
	// verbatim, from this session's scope AND any extra `scopes` the agent
	// asked for. See query_memory's success return for the full rationale.
	return tool.Result{Content: string(b)}, nil
}
