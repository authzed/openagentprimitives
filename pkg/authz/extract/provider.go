// Package extract defines the Provider interface and shared types
// the slice-3 binding orchestrator uses to extract entities from user
// messages. Per-provider implementations live in subpackages (anthropic
// is the default; future Gemini/Bedrock would slot in alongside).
package extract

import (
	"context"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// ExtractedEntity is one LLM-identified candidate from the user message.
// The ResourceID is the RAW text the LLM extracted; transforms (lowercase,
// spicedb_object_id, ...) are applied later by the binding orchestrator.
type ExtractedEntity struct {
	// ResourceType matches one of the AgentClass BoundEntities entries.
	ResourceType string

	// ResourceID is the raw text the LLM extracted (e.g., "foo/bar").
	// Slice-1 transforms (lowercase, spicedb_object_id) are applied by
	// the binding orchestrator before any SpiceDB Check.
	ResourceID string

	// SourceText is the <=200-char substring of the user message that
	// produced this entity. Used for audit + spec-2-approval body.
	SourceText string

	// Confidence is 0..1 (provider-specific; 0 if not supported).
	Confidence float64
}

// Provider is the seam for entity extraction. Production impl is the
// Anthropic-backed default in pkg/authz/extract/anthropic. Tests
// inject stubs.
type Provider interface {
	// Extract runs the LLM call and returns the list of candidate
	// entities. The provider MAY do its own pre-filtering. Returns
	// nil + nil if the message has no candidates.
	Extract(ctx context.Context, in ExtractInput) ([]ExtractedEntity, error)
}

// ExtractInput is the per-call input to Provider.Extract.
type ExtractInput struct {
	// UserMessage is the text content of the user's latest message.
	UserMessage string

	// EntityTypes is the AgentClass.Spec.Authz.Slots list (typed
	// via the CRD package to avoid duplicating shape).
	EntityTypes []spiceboxv1alpha1.BoundEntityType

	// AlreadyBound is the session's current binding set. The provider
	// includes these in the prompt and tells the LLM to skip
	// re-extracting them.
	AlreadyBound []SessionBinding

	// PerToolPrompts is the aggregated per-tool ExtractionPrompts the
	// runner has collected from the AgentClass's resolved tools,
	// keyed by resourceType. Provider concatenates into its system
	// prompt; AgentClass-level ExtractionPrompt on BoundEntityType
	// overrides per-type.
	PerToolPrompts map[string][]string // resourceType -> deduped prompts
}

// SessionBinding is the runtime shape of the already-bound list passed
// to the provider. Mirrors AgentSession.Status.BoundEntities but
// trimmed to the fields the extractor cares about.
type SessionBinding struct {
	ResourceType string
	ResourceID   string
}
