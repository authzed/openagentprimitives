// Package modality lets provider- and capability-aware "surfaces" (tools +
// prompt instructions) plug into a turn without the runner branching on
// which one is active.
//
// A modality answers "given what this model can do and what the operator opted
// into this turn, what tools and prompt text should I contribute?" — e.g. the
// "files" modality, which offers byte-moving tools only when the resolved model
// advertises llm.CapNativeFileOut/In and the session opted in. Modalities
// self-register (see the sibling registry package) rather than being switched on
// by name in the runner: new ones are added, not branched on.
package modality

import (
	"context"

	"github.com/authzed/openagentprimitives/pkg/agent/llm"
	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/platform/artifactstore"
)

// ArtifactReader reads stored artifact bytes by ref, optionally a range.
// Satisfied by the Tier-1 reader in pkg/agent/modality/files; declared here to
// avoid an import cycle.
type ArtifactReader interface {
	// ReadRange returns bytes [start, start+length) of ref. length<=0 means
	// "to EOF". Returns artifactstore.ErrNotFound if ref is unknown. The second
	// return is the number of bytes returned.
	ReadRange(ctx context.Context, ref artifactstore.Ref, start, length int64) ([]byte, int64, error)
}

// Bridge moves bytes between artifactstore and a provider code-execution
// container. Non-nil only when native file handling is active. Declared here to
// avoid an import cycle.
type Bridge interface {
	IntoStore(ctx context.Context, providerFileID string) (artifactstore.Ref, error)
	IntoContainer(ctx context.Context, ref artifactstore.Ref) (containerUploadID string, err error)
}

// Env is the per-turn resolution a modality reads to decide its surface.
type Env struct {
	ModelCaps   llm.CapabilitySet // from Provider.Capabilities(resolvedModel)
	NativeOptIn bool              // resolved settings (default false)
	Reader      ArtifactReader    // Tier-1 read-by-ref; nil ⇒ no Tier-1 tool
	Bridge      Bridge            // Tier-2; nil unless native active
}

// NativeActive reports whether Tier-2 (provider-native) is usable for cap:
// the agent opted in AND the provider advertises cap.
func (e Env) NativeActive(cap llm.Capability) bool {
	return e.NativeOptIn && e.ModelCaps.Has(cap)
}

// Modality contributes a capability-aware surface (tools + prompt) to a turn.
type Modality interface {
	Name() string
	MetaTools(env Env) []tool.Tool
	Instructions(env Env) string
}
