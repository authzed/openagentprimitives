package hooks

import (
	"context"
	"log/slog"
	"time"

	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/authz/extract"
	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"
)

// Extraction status values mirror pkg/memory/kinds/extraction_state. They are
// re-declared here (not imported) so the hook stays free of the memory-kind
// dependency; the authzd host's RecordState closure maps these onto the
// extraction_state kind's own constants (which are byte-identical strings).
const (
	ExtractionStatusPending  = "pending"
	ExtractionStatusComplete = "complete"
	ExtractionStatusFailed   = "failed"
)

// EntityBindState is the extraction_state transition the hook asks the host to
// persist. The host's RecordState closure maps it onto the extraction_state
// memory kind. StartedAt/CompletedAt are carried so the host writes the same
// timing fields internal/cmd/authzd's processOne did.
type EntityBindState struct {
	TurnIndex      int
	Status         string
	StartedAt      time.Time
	CompletedAt    time.Time
	CandidateCount int
	Error          string
}

// EntityBindDeps is the dependency struct for the EntityBind hook. The authzd
// host supplies every closure so the hook imports neither internal/cmd/authzd nor
// pkg/apis/v1alpha1: the v1alpha1 BoundEntityType → authz.BoundEntitySpec
// adaptation and the EntityTypes/PerToolPrompts binding live in the host's
// Extract closure.
//
// The Extract closure runs the untrusted-text extractor LLM. Because Eval is
// invoked by the authzd host inside authzd's process, this LLM call stays
// inside the prompt-injection quarantine (plan §5 / Risk 1).
type EntityBindDeps struct {
	// BoundEntities are the pre-adapted specs (from authz_session_config). Passed
	// to Prefilter + Extract so the host need not re-read config per closure call.
	BoundEntities []authz.BoundEntitySpec

	// Prefilter reports whether the text is worth an extractor call (authz.Prefilter).
	Prefilter func(text string, specs []authz.BoundEntitySpec) bool
	// Extract runs the entity extractor LLM. The host binds EntityTypes +
	// PerToolPrompts from the session config; the hook supplies the text + specs.
	Extract func(ctx context.Context, text string, specs []authz.BoundEntitySpec) ([]extract.ExtractedEntity, error)

	// RecordEntity persists one extracted_entity for the given turn.
	RecordEntity func(ctx context.Context, turnIdx int, e extract.ExtractedEntity) error
	// RecordState persists an extraction_state transition.
	RecordState func(ctx context.Context, s EntityBindState) error

	Logger *slog.Logger

	// Now is overridable for deterministic timestamps in tests.
	Now func() time.Time
}

// EntityBind is the InboundTurn hook that runs the entity extractor over the
// user's message and records extracted_entity / extraction_state, ported from
// internal/cmd/authzd/worker.go's processOne. It is ADVISORY: it always returns Allow and
// never gates the turn (a failed extraction records StatusFailed but still
// Allows — autofill degrades, the turn proceeds).
//
// Points() includes SessionStart so defaults-binding can slot in later, but its
// SessionStart Eval is a pass-through Allow: BindClassDefaults runs in the
// runner.
type EntityBind struct{ d EntityBindDeps }

// NewEntityBind constructs an EntityBind hook with defaults applied.
func NewEntityBind(d EntityBindDeps) *EntityBind {
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	if d.Now == nil {
		d.Now = func() time.Time { return time.Now().UTC() }
	}
	return &EntityBind{d: d}
}

func (h *EntityBind) Name() string { return "entity_bind" }
func (h *EntityBind) Points() []pipeline.Point {
	return []pipeline.Point{pipeline.SessionStart, pipeline.InboundTurn}
}

func (h *EntityBind) Eval(ctx context.Context, in pipeline.Input) pipeline.Decision {
	// BindClassDefaults stays in the runner: it is a pure SpiceDB Check with no
	// untrusted-text LLM, so the quarantine does not require moving it here.
	if in.Point != pipeline.InboundTurn {
		return pipeline.Decision{}
	}
	if in.Turn == nil {
		return pipeline.Decision{}
	}

	turnIdx := in.Turn.InboxIdx
	text := in.Turn.Text
	start := h.d.Now()

	h.recordState(ctx, EntityBindState{
		TurnIndex: turnIdx, Status: ExtractionStatusPending, StartedAt: start,
	})

	if h.d.Prefilter == nil || !h.d.Prefilter(text, h.d.BoundEntities) {
		// Nothing to extract: complete with 0 candidates (no extractor call).
		h.recordState(ctx, EntityBindState{
			TurnIndex: turnIdx, Status: ExtractionStatusComplete,
			StartedAt: start, CompletedAt: h.d.Now(), CandidateCount: 0,
		})
		return pipeline.Decision{}
	}

	if h.d.Extract == nil {
		h.d.Logger.Info("entity_bind: Extract not wired; recording failed (advisory)",
			"session", in.Session.String(), "turn", turnIdx)
		h.recordState(ctx, EntityBindState{
			TurnIndex: turnIdx, Status: ExtractionStatusFailed,
			StartedAt: start, CompletedAt: h.d.Now(), Error: "extractor not wired",
		})
		return pipeline.Decision{}
	}

	out, err := h.d.Extract(ctx, text, h.d.BoundEntities)
	if err != nil {
		h.d.Logger.Info("entity_bind: extract failed (advisory; turn proceeds)",
			"session", in.Session.String(), "turn", turnIdx, "err", err.Error())
		h.recordState(ctx, EntityBindState{
			TurnIndex: turnIdx, Status: ExtractionStatusFailed,
			StartedAt: start, CompletedAt: h.d.Now(), Error: err.Error(),
		})
		return pipeline.Decision{}
	}

	for _, e := range out {
		if h.d.RecordEntity == nil {
			continue
		}
		if rerr := h.d.RecordEntity(ctx, turnIdx, e); rerr != nil {
			h.d.Logger.Info("entity_bind: record entity failed (advisory)",
				"session", in.Session.String(), "turn", turnIdx, "err", rerr.Error())
		}
	}
	h.recordState(ctx, EntityBindState{
		TurnIndex: turnIdx, Status: ExtractionStatusComplete,
		StartedAt: start, CompletedAt: h.d.Now(), CandidateCount: len(out),
	})
	h.d.Logger.Info("entity_bind: extract done",
		"session", in.Session.String(), "turn", turnIdx, "candidates", len(out))
	return pipeline.Decision{}
}

// recordState invokes RecordState, logging (not failing) on error — advisory.
func (h *EntityBind) recordState(ctx context.Context, s EntityBindState) {
	if h.d.RecordState == nil {
		return
	}
	if err := h.d.RecordState(ctx, s); err != nil {
		h.d.Logger.Info("entity_bind: record state failed (advisory)",
			"turn", s.TurnIndex, "status", s.Status, "err", err.Error())
	}
}

var _ pipeline.Hook = (*EntityBind)(nil)
