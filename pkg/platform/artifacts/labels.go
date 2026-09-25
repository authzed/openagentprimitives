// Package artifacts holds the cross-cutting versioning logic shared by
// the artifact_* meta tools, respond_to_user attachment resolution, and
// the oap artifact CLI. Memory is the source of truth (the artifact +
// artifact_revision Kinds); this package writes and reads that chain and
// stamps versioning intent onto ArtifactRender CRs so the prepare->await
// handoff can finalize a revision regardless of which tool sees Ready.
package artifacts

// Label + annotation keys stamped on ArtifactRender CRs at create time.
// The render controller never reads these; they are runner-to-runner
// state that lets FinalizeRevision run from either artifact_prepare or
// artifact_await. Keys follow the feature-prefixed convention used
// elsewhere (e.g. channel.agentprimitives.authzed.com/...).
const (
	// LabelArtifactID groups every render of one logical artifact. Used by
	// FinalizeRevision and the live-view to List/watch.
	LabelArtifactID = "artifact.agentprimitives.authzed.com/id"

	AnnoParentRevision      = "artifact.agentprimitives.authzed.com/parent-revision"
	AnnoChangeDescription   = "artifact.agentprimitives.authzed.com/change-description"
	AnnoAppliedTags         = "artifact.agentprimitives.authzed.com/applied-tags" // comma-separated
	AnnoArtifactName        = "artifact.agentprimitives.authzed.com/name"
	AnnoArtifactDescription = "artifact.agentprimitives.authzed.com/description"
	// AnnoInternal ("true") marks the artifact head Internal at finalize time.
	AnnoInternal = "artifact.agentprimitives.authzed.com/internal"
	// AnnoPreviewOf / AnnoPreviewRevisionOf link a preview child to the source
	// bundled-only artifact head + the exact source revision it previews.
	AnnoPreviewOf         = "artifact.agentprimitives.authzed.com/preview-of"
	AnnoPreviewRevisionOf = "artifact.agentprimitives.authzed.com/preview-revision-of"
)

// TagLatest is the reserved, system-maintained tag that always points at
// the newest revision. Authors may not set it explicitly.
const TagLatest = "latest"
