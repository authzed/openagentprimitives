package meta

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/memory"
)

func NewQueryKnowledge() tool.Tool {
	return &queryKnowledgeTool{}
}

type queryKnowledgeTool struct{}

func (*queryKnowledgeTool) Name() string    { return "query_knowledge" }
func (*queryKnowledgeTool) Kind() tool.Kind { return tool.KindMeta }
func (*queryKnowledgeTool) Permission() authz.Permission {
	return authz.Permission{StateImpact: authz.Stateless}
}
func (*queryKnowledgeTool) PermissionVariants() []authz.PermissionVariant { return nil }

func (*queryKnowledgeTool) Description() string {
	return "Query the knowledge graph for entities, facts, and relationships. " +
		"Use mode=search for natural language fact search. " +
		"Use mode=entity_facts with entity_uuid to get all facts about an entity. " +
		"Use mode=related with entity_uuid to find connected entities. " +
		"Use mode=communities to discover entity clusters. " +
		"The search and related modes report a `truncated` field: when it is true, " +
		"`limit` cut the answer short and `count` is a page size, not a total."
}

func (*queryKnowledgeTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type":"object",
		"additionalProperties":false,
		"properties":{
			"text":{
				"type":"string",
				"description":"Natural language query (for search mode)"
			},
			"entity_uuid":{
				"type":"string",
				"description":"Entity UUID (for entity_facts and related modes)"
			},
			"mode":{
				"type":"string",
				"enum":["search","entity_facts","related","communities"],
				"default":"search",
				"description":"Query mode"
			},
			"limit":{
				"type":"integer",
				"minimum":1,
				"maximum":50,
				"default":10,
				"description":"Max results"
			}
		}
	}`)
}

type queryKnowledgeArgs struct {
	// Text is the free-text query, used by the search-shaped modes.
	Text string `json:"text"`
	// EntityUUID anchors the entity-shaped modes; empty for a text search.
	EntityUUID string `json:"entity_uuid"`
	// Mode selects which graph query to run.
	Mode string `json:"mode"`
	// Limit caps returned rows; zero takes the schema default.
	Limit int `json:"limit"`
}

func (t *queryKnowledgeTool) Execute(ctx context.Context, raw json.RawMessage, sess *tool.SessionContext) (tool.Result, error) {
	var a queryKnowledgeArgs
	if res, ok := tool.ParseArgs(raw, &a, t.Name(), `{"text":"what do we know about Bob?","mode":"search"}`); !ok {
		return res, nil
	}
	if sess == nil || sess.KG == nil {
		return tool.Result{Content: "query_knowledge: knowledge graph not available", IsError: true, Trusted: true}, nil
	}
	if a.Limit <= 0 {
		a.Limit = 10
	}
	if a.Limit > 50 {
		a.Limit = 50
	}
	if a.Mode == "" {
		a.Mode = "search"
	}

	// Every success payload below is graph content an external service
	// (Graphiti) extracted from stored turns — third-party, not
	// framework-controlled — so Trusted is deliberately left false on them. Meta
	// tools bypass the PostToolCall pipeline, so false is what makes the runner
	// inspect them (Loop.inspectUntrustedResult). The error arms delegate the
	// same judgement to recallError, which keeps Trusted for a memory sentinel
	// (a non-UUID entity's ErrInvalidQuery, ErrKGScopeMismatch) and drops it for
	// a provider error, which embeds up to 1KiB of the upstream HTTP response
	// body. Only the fixed platform strings below are unconditionally Trusted.
	// Same reasoning as query_memory / read_thread_history.
	switch a.Mode {
	case "search":
		// The graph answers with a bare slice, so the probe that separates a
		// full page from a complete answer is made here: ask for one row past
		// the limit and trim it back off before the model ever sees a count.
		facts, err := sess.KG.SearchFacts(ctx, a.Text, memory.ProbeLimit(a.Limit))
		if err != nil {
			return recallError(t.Name(), err), nil
		}
		facts, truncated := memory.TrimProbe(facts, a.Limit)
		b, _ := json.Marshal(map[string]interface{}{"facts": facts, "count": len(facts), "truncated": truncated})
		return tool.Result{Content: string(b)}, nil

	case "entity_facts":
		if a.EntityUUID == "" {
			return tool.Result{Content: "query_knowledge: entity_uuid required for entity_facts mode", IsError: true, Trusted: true}, nil
		}
		facts, err := sess.KG.EntityFacts(ctx, a.EntityUUID)
		if err != nil {
			return recallError(t.Name(), err), nil
		}
		b, _ := json.Marshal(map[string]interface{}{"facts": facts, "count": len(facts)})
		return tool.Result{Content: string(b)}, nil

	case "related":
		if a.EntityUUID == "" {
			return tool.Result{Content: "query_knowledge: entity_uuid required for related mode", IsError: true, Trusted: true}, nil
		}
		entities, err := sess.KG.RelatedEntities(ctx, a.EntityUUID, memory.ProbeLimit(a.Limit))
		if err != nil {
			return recallError(t.Name(), err), nil
		}
		entities, truncated := memory.TrimProbe(entities, a.Limit)
		b, _ := json.Marshal(map[string]interface{}{"entities": entities, "count": len(entities), "truncated": truncated})
		return tool.Result{Content: string(b)}, nil

	case "communities":
		comms, err := sess.KG.Communities(ctx, sess.Namespace+"/"+sess.Name)
		if err != nil {
			return recallError(t.Name(), err), nil
		}
		b, _ := json.Marshal(map[string]interface{}{"communities": comms, "count": len(comms)})
		return tool.Result{Content: string(b)}, nil

	default:
		return tool.Result{Content: fmt.Sprintf("query_knowledge: unknown mode %q", a.Mode), IsError: true, Trusted: true}, nil
	}
}
