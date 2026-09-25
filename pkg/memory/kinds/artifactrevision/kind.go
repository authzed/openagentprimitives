// Package artifactrevision registers the immutable per-revision Kind of
// the artifact-versioning system. Each Entry is one rendered revision of
// a logical artifact; revisions never change after creation. The head
// (pkg/memory/kinds/artifact) tracks identity, the revision count, and
// the movable tag refs.
package artifactrevision

import (
	"context"
	"reflect"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/memory"
)

const (
	KindName = "artifact_revision"
	IDPrefix = "artrev-"
)

// Revision is the Content of an artifact_revision Entry. Self-sufficient:
// it carries the artifactstore OutputRef and render metadata so the
// history survives the ArtifactRender CR being garbage-collected with the
// session. Links carry lineage (revision_of head, parent revision,
// of_render CR).
type Revision struct {
	// Seq is this revision's 1-based position in the head's history; unique and
	// monotonic per artifact.
	Seq int `json:"seq"`
	// ChangeDescription is the agent's note on what changed since the parent
	// revision; empty on the first revision and whenever the agent gave none.
	ChangeDescription string `json:"changeDescription,omitempty"`
	// RenderName is the ArtifactRender CR that produced this revision. Recorded
	// for correlation only — the CR is garbage-collected with the session.
	RenderName string `json:"renderName"`
	// OutputRef is the artifactstore key holding the rendered bytes. This is the
	// only durable handle on the content once the CR is gone.
	OutputRef string `json:"outputRef"`
	// MIME is the rendered output's content type, used to serve it back.
	MIME string `json:"mime"`
	// Size is the rendered output's length in bytes.
	Size int64 `json:"size"`
	// Filename is the suggested download name; empty when the renderer offered
	// none, in which case callers derive one.
	Filename string `json:"filename,omitempty"`
	// Warnings are the sanitizer's findings for this render — content that was
	// stripped or rewritten. Empty means the render passed clean.
	Warnings []spiceboxv1alpha1.SanitizerWarning `json:"warnings,omitempty"`
	// CSP is the widget's declared `_meta.ui.csp`, copied from
	// ArtifactRender.Spec.CSP at FinalizeRevision time so it survives the CR
	// being garbage-collected with the session. nil for non-widget
	// (non-mcpui) revisions, or when the widget declared no CSP.
	CSP *spiceboxv1alpha1.WidgetCSP `json:"csp,omitempty"`
}

type Kind struct{}

func (Kind) Name() string     { return KindName }
func (Kind) IDPrefix() string { return IDPrefix }

// WriteAuthority: written alongside its artifact head by pkg/platform/artifacts, in the
// runner.
func (Kind) WriteAuthority() memory.WriteAuthority { return memory.SessionWritten }
func (Kind) Retention() memory.Retention           { return memory.Retention{EssentialWhileLive: true} }
func (Kind) ContentSchema() reflect.Type           { return reflect.TypeOf(Revision{}) }
func (Kind) IndexedFields() []string               { return nil }

func (Kind) NewScopeHooks(_ memory.Scope) memory.ScopeHooks { return hooks{} }

type hooks struct{}

func (hooks) OnSignal(_ context.Context, _ memory.Signal) error { return nil }

func init() { memory.RegisterKind(Kind{}) }
