package graphiti

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// EpisodeInput is the caller-facing input for ingesting content into the
// knowledge graph, mapped internally to Graphiti's /messages request format.
type EpisodeInput struct {
	// GroupID is the session graph to ingest into; it is the isolation boundary
	// every scoped read is later checked against.
	GroupID string `json:"group_id"`
	// Content is the raw text Graphiti extracts entities and facts from.
	Content string `json:"content"`
	// Role attributes the content ("user", "assistant", …).
	Role string `json:"role"`
}

// graphitiMessage is one message in Graphiti's /messages request.
type graphitiMessage struct {
	// Content is the message text.
	Content string `json:"content"`
	// RoleType is Graphiti's own role vocabulary, distinct from Role.
	RoleType string `json:"role_type"`
	// Role is the caller's role label, passed through.
	Role string `json:"role"`
	// Name optionally labels the speaker; omitted when unknown.
	Name string `json:"name,omitempty"`
}

// graphitiMessagesRequest is the Graphiti server's /messages request body.
type graphitiMessagesRequest struct {
	// GroupID is the session graph the messages are ingested into.
	GroupID string `json:"group_id"`
	// Messages are the episodes to ingest, in order.
	Messages []graphitiMessage `json:"messages"`
}

// IngestResult holds what Graphiti extracted and stored. Extraction is
// ASYNCHRONOUS, so both slices are routinely empty on an accepted ingest.
type IngestResult struct {
	// Entities extracted from this episode.
	Entities []GraphitiResult `json:"entities"`
	// Facts extracted from this episode.
	Facts []GraphitiResult `json:"facts"`
}

// Ingest sends content to Graphiti for knowledge graph extraction via
// POST /messages. Graphiti handles entity extraction, deduplication,
// and contradiction detection, storing results in Neo4j.
func (p *GraphitiSearchProvider) Ingest(ctx context.Context, input EpisodeInput) (IngestResult, error) {
	role := input.Role
	if role == "" {
		role = "user"
	}
	apiReq := graphitiMessagesRequest{
		GroupID: input.GroupID,
		Messages: []graphitiMessage{
			{Content: input.Content, RoleType: role, Role: role},
		},
	}
	body, err := json.Marshal(apiReq)
	if err != nil {
		return IngestResult{}, fmt.Errorf("graphiti ingest: marshal: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		p.endpoint+"/messages", bytes.NewReader(body))
	if err != nil {
		return IngestResult{}, fmt.Errorf("graphiti ingest: request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := p.client.Do(req)
	if err != nil {
		return IngestResult{}, fmt.Errorf("graphiti ingest: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return IngestResult{}, fmt.Errorf("graphiti ingest: status %d: %s", resp.StatusCode, msg)
	}

	var result IngestResult
	// 202 Accepted means async processing; response may not contain entities/facts yet.
	_ = json.NewDecoder(resp.Body).Decode(&result)
	return result, nil
}
