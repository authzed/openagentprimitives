package memory

import (
	"context"
	"time"
)

// KGInput is one episode handed to the graph for entity extraction.
type KGInput struct {
	// GroupID partitions the graph per session; it is the isolation boundary a
	// scoped read is checked against.
	GroupID string `json:"groupId"`
	// Content is the raw text to extract entities and facts from.
	Content string `json:"content"`
	// Role attributes the content ("user", "assistant", …).
	Role string `json:"role"`
}

// KGEntity is a node the graph extracted and deduplicated.
type KGEntity struct {
	// UUID is the graph's identifier, stable across re-ingestion of the same entity.
	UUID string `json:"uuid"`
	// Name is the entity as the extractor named it.
	Name string `json:"name"`
	// Summary is the graph's rolling description, rewritten as facts accumulate.
	Summary string `json:"summary"`
	// Attributes are extractor-supplied properties; absent when none were found.
	Attributes map[string]string `json:"attributes,omitempty"`
}

// KGFact is an edge: one relationship the graph asserts between two entities.
type KGFact struct {
	// UUID is the graph's identifier for this edge.
	UUID string `json:"uuid"`
	// Name is the relationship type.
	Name string `json:"name"`
	// Fact is the natural-language assertion.
	Fact string `json:"fact"`
	// FromEntity is the source entity's UUID.
	FromEntity string `json:"fromEntity"`
	// ToEntity is the target entity's UUID.
	ToEntity string `json:"toEntity"`
	// ValidAt is when the fact began holding; nil when the graph could not date it.
	ValidAt *time.Time `json:"validAt,omitempty"`
	// InvalidAt is when a later fact contradicted this one; nil means still current.
	InvalidAt *time.Time `json:"invalidAt,omitempty"`
}

// KGCommunity is a cluster of related entities the graph derived.
type KGCommunity struct {
	// UUID is the graph's identifier for the cluster.
	UUID string `json:"uuid"`
	// Name is the cluster's generated label.
	Name string `json:"name"`
	// Summary describes what its members have in common.
	Summary string `json:"summary"`
	// Members are the entity UUIDs in the cluster.
	Members []string `json:"members"`
}

// KGProvider is the graph-native read/write surface. Extraction, dedup and
// contradiction detection happen inside the provider, asynchronously, so an
// Ingest that returns nil has been accepted, not yet reflected in reads.
type KGProvider interface {
	Ingest(ctx context.Context, input KGInput) error
	SearchFacts(ctx context.Context, query string, limit int) ([]KGFact, error)
	GetEntity(ctx context.Context, uuid string) (*KGEntity, error)
	EntityFacts(ctx context.Context, entityUUID string) ([]KGFact, error)
	RelatedEntities(ctx context.Context, entityUUID string, limit int) ([]KGEntity, error)
	Communities(ctx context.Context, groupID string) ([]KGCommunity, error)
}
