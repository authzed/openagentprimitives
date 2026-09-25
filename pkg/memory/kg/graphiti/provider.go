package graphiti

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"

	"github.com/google/uuid"

	"github.com/authzed/openagentprimitives/pkg/memory"
	searchgraphiti "github.com/authzed/openagentprimitives/pkg/memory/search/graphiti"
)

type GraphitiKGProvider struct {
	search   *searchgraphiti.GraphitiSearchProvider
	endpoint string
	client   *http.Client
}

var _ memory.KGProvider = (*GraphitiKGProvider)(nil)

func New(search *searchgraphiti.GraphitiSearchProvider) *GraphitiKGProvider {
	return &GraphitiKGProvider{
		search:   search,
		endpoint: search.Endpoint(),
		client:   search.Client(),
	}
}

func (p *GraphitiKGProvider) Ingest(ctx context.Context, input memory.KGInput) error {
	_, err := p.search.Ingest(ctx, searchgraphiti.EpisodeInput{
		GroupID: input.GroupID,
		Content: input.Content,
		Role:    input.Role,
	})
	return err
}

func (p *GraphitiKGProvider) SearchFacts(ctx context.Context, query string, limit int) ([]memory.KGFact, error) {
	reqBody := map[string]interface{}{"query": query, "max_facts": limit}
	if s, ok := memory.KGScopeFrom(ctx); ok {
		reqBody["group_ids"] = []string{s.ID}
	}
	body, _ := json.Marshal(reqBody)
	resp, err := p.doPost(ctx, "/search", body)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var result struct {
		Facts []struct {
			UUID string `json:"uuid"`
			Name string `json:"name"`
			Fact string `json:"fact"`
		} `json:"facts"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("graphiti kg: decode search: %w", err)
	}
	facts := make([]memory.KGFact, len(result.Facts))
	for i, f := range result.Facts {
		facts[i] = memory.KGFact{UUID: f.UUID, Name: f.Name, Fact: f.Fact}
	}
	return facts, nil
}

// GetEntity is a UUID GET against Graphiti's /entity-edge/{uuid}, which takes
// no group parameter — unlike every sibling here, the read cannot be
// group-filtered at the API. The upstream approval gate does NOT cover the
// gap: it authorizes the scope in the request URL, not the UUID in the path,
// so a caller holding one session's token could name any UUID.
//
// The group used to be checked on the way out instead, but this deployment's
// real response (graph_service's FactResult, confirmed against a live pod —
// get_fact_result_from_edge never sets one) carries NO group id at all, ever.
// Comparing an always-empty field against the caller's scope did not fail
// open OR correctly fail closed: it failed closed for EVERY scoped caller,
// including a legitimate one reading their own graph, which is a functional
// break of a feature the check was only meant to make safe. A field that is
// never on the wire cannot prove membership OR foreignness, so a scoped read
// is refused outright — before the request is even made — rather than
// guessed at from content that was never coming. Unscoped reads (the
// platform-admin browser, gated on platform#view_audit) are unaffected and
// stay cross-session by design.
//
// entityUUID is the ONLY caller-controlled URL component in this package, and
// is both validated and escaped before it becomes one. Unvalidated it is not a
// path segment at all: net/url resolves dot segments before the request goes
// out, so "../../nodes/all" leaves /entity-edge/ entirely and re-targets the
// read within the Graphiti host, under an approval that authorized a session
// scope rather than a path. The shape check is the real guard; PathEscape stays
// so the guarantee does not rest on the validator remaining strict.
func (p *GraphitiKGProvider) GetEntity(ctx context.Context, entityUUID string) (*memory.KGEntity, error) {
	if _, err := uuid.Parse(entityUUID); err != nil {
		// A caller bug, not a fault: 400 rather than eight retries of a request
		// that can never succeed. The id is echoed back because it is the
		// caller's own input; the parse error is not, since it adds nothing.
		return nil, fmt.Errorf("%w: graphiti kg: get entity: %q is not a UUID",
			memory.ErrInvalidQuery, entityUUID)
	}
	if _, ok := memory.KGScopeFrom(ctx); ok {
		return nil, fmt.Errorf("%w: graphiti kg: get entity: this deployment cannot scope-check %s",
			memory.ErrKGUnsupported, entityUUID)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		p.endpoint+"/entity-edge/"+url.PathEscape(entityUUID), nil)
	if err != nil {
		return nil, fmt.Errorf("graphiti kg: get entity: %w", err)
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("graphiti kg: get entity: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("graphiti kg: get entity: status %d: %s", resp.StatusCode, msg)
	}
	var ent memory.KGEntity
	if err := json.NewDecoder(resp.Body).Decode(&ent); err != nil {
		return nil, fmt.Errorf("graphiti kg: decode entity: %w", err)
	}
	return &ent, nil
}

// EntityFacts asks Graphiti's /get-memory for facts near entityUUID.
// GetMemoryRequest requires group_id, center_node_uuid, AND messages
// (confirmed against a live pod's OpenAPI schema) — omitting any of the three
// is refused with a 422 before the search ever runs. messages is sent empty:
// this deployment's handler (graph_service/routers/retrieve.py) builds its
// search query by concatenating messages' content and does not otherwise read
// center_node_uuid at all, so an empty messages list yields the group's
// general fact search rather than a query centered on the entity — the
// closest this endpoint can get to "facts about this entity" without also
// composing a synthetic message from the entity's own name/summary, which
// EntityFacts has no cheap way to fetch here.
func (p *GraphitiKGProvider) EntityFacts(ctx context.Context, entityUUID string) ([]memory.KGFact, error) {
	groupID := ""
	if s, ok := memory.KGScopeFrom(ctx); ok {
		groupID = s.ID
	}
	body, _ := json.Marshal(map[string]interface{}{
		"group_id":         groupID,
		"center_node_uuid": entityUUID,
		"max_facts":        50,
		"messages":         []interface{}{},
	})
	resp, err := p.doPost(ctx, "/get-memory", body)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var result struct {
		Facts []struct {
			UUID    string `json:"uuid"`
			Name    string `json:"name"`
			Fact    string `json:"fact"`
			GroupID string `json:"group_id"`
		} `json:"facts"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("graphiti kg: decode entity facts: %w", err)
	}
	// group_id here is a REQUEST-side filter Graphiti is trusted to honor, but
	// center_node_uuid is model-supplied — so belt-and-suspenders, drop any fact
	// whose returned group_id is present and does NOT match this scope. Only a
	// PRESENT mismatch is dropped, so this deployment's FactResult — which never
	// sets group_id at all (confirmed against a live pod; see GetEntity's
	// comment) — is unaffected (no false drop) and the primary isolation stays
	// the request filter.
	scope, haveScope := memory.KGScopeFrom(ctx)
	facts := make([]memory.KGFact, 0, len(result.Facts))
	for _, f := range result.Facts {
		if haveScope && f.GroupID != "" && f.GroupID != scope.ID {
			continue
		}
		facts = append(facts, memory.KGFact{UUID: f.UUID, Name: f.Name, Fact: f.Fact})
	}
	return facts, nil
}

// RelatedEntities always reports memory.ErrKGUnsupported without contacting
// Graphiti: this deployment's only entity-adjacent endpoint (/get-memory)
// returns a GetMemoryResponse that is facts-only (confirmed against a live
// pod's DTOs, graph_service/dto/retrieve.py upstream) — no request shape
// makes it answer with entities. Round-tripping a request that can only ever
// come back empty would be worse than refusing: it would also 422 (the same
// missing-messages defect EntityFacts had), and a caller cannot tell "no
// related entities" from "this deployment cannot tell you."
func (p *GraphitiKGProvider) RelatedEntities(_ context.Context, entityUUID string, _ int) ([]memory.KGEntity, error) {
	return nil, fmt.Errorf("%w: graphiti kg: related entities for %s", memory.ErrKGUnsupported, entityUUID)
}

// Communities always reports memory.ErrKGUnsupported without contacting
// Graphiti, for the same reason as RelatedEntities: GetMemoryResponse never
// carries a communities key, on this or any version of graph_service's
// /get-memory (its handler only ever runs a facts search). Graphiti does
// support community detection, but only through its separate MCP server
// (build_communities), a different protocol and deployment this provider does
// not speak.
func (p *GraphitiKGProvider) Communities(_ context.Context, groupID string) ([]memory.KGCommunity, error) {
	return nil, fmt.Errorf("%w: graphiti kg: communities for %s", memory.ErrKGUnsupported, groupID)
}

func (p *GraphitiKGProvider) doPost(ctx context.Context, path string, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		p.endpoint+path, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("graphiti kg: request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("graphiti kg: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		resp.Body.Close()
		return nil, fmt.Errorf("graphiti kg: status %d: %s", resp.StatusCode, msg)
	}
	return resp, nil
}
