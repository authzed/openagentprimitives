// Package artifact registers the logical-artifact head Kind. One Entry
// per logical artifact in a session. The head is mutable (upserted on
// each new revision) and tracks the human name/description, the renderer
// kind, the monotonic RevisionCount, and the movable tag refs
// (tagName -> revisionID). The reserved "latest" tag always points at the
// newest revision.
package artifact

import (
	"context"
	"reflect"

	"github.com/authzed/openagentprimitives/pkg/memory"
)

const (
	KindName = "artifact"
	IDPrefix = "artifact-"
)

// Artifact is the Content of an artifact head Entry.
type Artifact struct {
	// Name is the human-facing title the agent gave this artifact.
	Name string `json:"name,omitempty"`
	// Description is the agent's one-line summary of what the artifact is.
	Description string `json:"description,omitempty"`
	// RendererKind is the channelassets Renderer that renders every revision
	// (html, css, svg, image, mcpui); fixed for the artifact's whole life.
	RendererKind string `json:"rendererKind"`
	// RevisionCount is the highest revision Seq this head has seen; the next
	// revision takes RevisionCount+1. It only ever rises, never decreases.
	RevisionCount int `json:"revisionCount"`
	// Tags maps a movable tag name to the artifact_revision entry ID it points
	// at. The reserved "latest" tag always points at the newest revision.
	Tags map[string]string `json:"tags,omitempty"`
	// Internal marks a system-managed artifact hidden from the agent (e.g. a
	// bundled-only kind's browser-preview render). Internal artifacts are
	// excluded from ListArtifacts / artifact_history and are not agent-facing.
	Internal bool `json:"internal,omitempty"`
}

type Kind struct{}

func (Kind) Name() string     { return KindName }
func (Kind) IDPrefix() string { return IDPrefix }

// WriteAuthority: the agent authors artifacts, through pkg/platform/artifacts running
// in the runner.
func (Kind) WriteAuthority() memory.WriteAuthority { return memory.SessionWritten }
func (Kind) Retention() memory.Retention           { return memory.Retention{EssentialWhileLive: true} }
func (Kind) ContentSchema() reflect.Type           { return reflect.TypeOf(Artifact{}) }
func (Kind) IndexedFields() []string               { return nil }

func (Kind) NewScopeHooks(_ memory.Scope) memory.ScopeHooks { return hooks{} }

type hooks struct{}

func (hooks) OnSignal(_ context.Context, _ memory.Signal) error { return nil }

func init() { memory.RegisterKind(Kind{}) }
