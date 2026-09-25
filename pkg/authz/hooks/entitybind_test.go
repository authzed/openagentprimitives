package hooks_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/authz/extract"
	"github.com/authzed/openagentprimitives/pkg/authz/hooks"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"
)

// recordingEntityBind captures the entity/state writes the EntityBind hook
// makes, so tests assert on what would land in extracted_entity /
// extraction_state without a memory store.
type recordingEntityBind struct {
	mu       sync.Mutex
	entities []extract.ExtractedEntity
	states   []hooks.EntityBindState
}

func (r *recordingEntityBind) recordEntity(_ context.Context, turnIdx int, e extract.ExtractedEntity) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.entities = append(r.entities, e)
	return nil
}

func (r *recordingEntityBind) recordState(_ context.Context, s hooks.EntityBindState) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.states = append(r.states, s)
	return nil
}

func (r *recordingEntityBind) lastState() hooks.EntityBindState {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.states) == 0 {
		return hooks.EntityBindState{}
	}
	return r.states[len(r.states)-1]
}

func inboundTurnInput(text string, idx int) pipeline.Input {
	return pipeline.Input{
		Point:     pipeline.InboundTurn,
		Session:   pipeline.SessionRef{Namespace: "ns", Name: "a"},
		Requester: identity.CanonicalFromTrusted("user:alice", "test fixture"),
		Turn:      &pipeline.TurnInfo{Text: text, InboxIdx: idx},
	}
}

func TestEntityBind_InboundTurn_PrefilterSkips_RecordsCompleteZero(t *testing.T) {
	rec := &recordingEntityBind{}
	extractCalled := false
	h := hooks.NewEntityBind(hooks.EntityBindDeps{
		BoundEntities: []authz.BoundEntitySpec{{ResourceType: "github_repo"}},
		Prefilter:     func(string, []authz.BoundEntitySpec) bool { return false },
		Extract: func(context.Context, string, []authz.BoundEntitySpec) ([]extract.ExtractedEntity, error) {
			extractCalled = true
			return nil, nil
		},
		RecordEntity: rec.recordEntity,
		RecordState:  rec.recordState,
	})

	dec := h.Eval(context.Background(), inboundTurnInput("hello there", 1))
	assert.Equal(t, pipeline.Allow, dec.Verdict, "EntityBind is advisory; always Allow")
	assert.False(t, extractCalled, "prefilter skip must NOT call the extractor")
	assert.Empty(t, rec.entities)
	last := rec.lastState()
	assert.Equal(t, hooks.ExtractionStatusComplete, last.Status)
	assert.Equal(t, 0, last.CandidateCount, "prefilter skip records complete with 0 candidates")
	assert.Equal(t, 1, last.TurnIndex)
}

func TestEntityBind_InboundTurn_Extract_RecordsEntitiesAndComplete(t *testing.T) {
	rec := &recordingEntityBind{}
	h := hooks.NewEntityBind(hooks.EntityBindDeps{
		BoundEntities: []authz.BoundEntitySpec{{ResourceType: "github_repo"}},
		Prefilter:     func(string, []authz.BoundEntitySpec) bool { return true },
		Extract: func(_ context.Context, text string, _ []authz.BoundEntitySpec) ([]extract.ExtractedEntity, error) {
			return []extract.ExtractedEntity{
				{ResourceType: "github_repo", ResourceID: "authzed/spicedb", SourceText: text},
				{ResourceType: "github_repo", ResourceID: "example/repo"},
			}, nil
		},
		RecordEntity: rec.recordEntity,
		RecordState:  rec.recordState,
	})

	dec := h.Eval(context.Background(), inboundTurnInput("open authzed/spicedb and example/repo", 2))
	assert.Equal(t, pipeline.Allow, dec.Verdict)
	require.Len(t, rec.entities, 2, "one extracted_entity per result")
	assert.Equal(t, "authzed/spicedb", rec.entities[0].ResourceID)
	last := rec.lastState()
	assert.Equal(t, hooks.ExtractionStatusComplete, last.Status)
	assert.Equal(t, 2, last.CandidateCount)
	assert.Equal(t, 2, last.TurnIndex)
}

func TestEntityBind_InboundTurn_ExtractError_RecordsFailed_StillAllow(t *testing.T) {
	rec := &recordingEntityBind{}
	h := hooks.NewEntityBind(hooks.EntityBindDeps{
		BoundEntities: []authz.BoundEntitySpec{{ResourceType: "github_repo"}},
		Prefilter:     func(string, []authz.BoundEntitySpec) bool { return true },
		Extract: func(context.Context, string, []authz.BoundEntitySpec) ([]extract.ExtractedEntity, error) {
			return nil, errors.New("llm down")
		},
		RecordEntity: rec.recordEntity,
		RecordState:  rec.recordState,
	})

	dec := h.Eval(context.Background(), inboundTurnInput("open authzed/spicedb", 3))
	assert.Equal(t, pipeline.Allow, dec.Verdict, "extractor error is advisory: still Allow (never gates the turn)")
	assert.Empty(t, rec.entities)
	last := rec.lastState()
	assert.Equal(t, hooks.ExtractionStatusFailed, last.Status)
	assert.Contains(t, last.Error, "llm down")
	assert.Equal(t, 3, last.TurnIndex)
}

func TestEntityBind_RecordsPendingBeforeWork(t *testing.T) {
	rec := &recordingEntityBind{}
	h := hooks.NewEntityBind(hooks.EntityBindDeps{
		BoundEntities: []authz.BoundEntitySpec{{ResourceType: "github_repo"}},
		Prefilter:     func(string, []authz.BoundEntitySpec) bool { return false },
		RecordEntity:  rec.recordEntity,
		RecordState:   rec.recordState,
	})
	_ = h.Eval(context.Background(), inboundTurnInput("hello", 5))
	require.GreaterOrEqual(t, len(rec.states), 2, "pending then complete")
	assert.Equal(t, hooks.ExtractionStatusPending, rec.states[0].Status, "first state write is pending")
}

func TestEntityBind_SessionStart_NoOpAllow(t *testing.T) {
	rec := &recordingEntityBind{}
	extractCalled := false
	h := hooks.NewEntityBind(hooks.EntityBindDeps{
		BoundEntities: []authz.BoundEntitySpec{{ResourceType: "github_repo"}},
		Prefilter:     func(string, []authz.BoundEntitySpec) bool { return true },
		Extract: func(context.Context, string, []authz.BoundEntitySpec) ([]extract.ExtractedEntity, error) {
			extractCalled = true
			return nil, nil
		},
		RecordEntity: rec.recordEntity,
		RecordState:  rec.recordState,
	})
	in := inboundTurnInput("open authzed/spicedb", 0)
	in.Point = pipeline.SessionStart
	dec := h.Eval(context.Background(), in)
	assert.Equal(t, pipeline.Allow, dec.Verdict, "SessionStart defaults binding is deferred — thin pass-through Allow")
	assert.False(t, extractCalled, "SessionStart must NOT run the per-turn extractor (defaults deferred)")
	assert.Empty(t, rec.states, "SessionStart pass-through writes no extraction_state")
}

func TestEntityBind_Points(t *testing.T) {
	h := hooks.NewEntityBind(hooks.EntityBindDeps{})
	assert.ElementsMatch(t, []pipeline.Point{pipeline.SessionStart, pipeline.InboundTurn}, h.Points())
	assert.Equal(t, "entity_bind", h.Name())
}

var _ pipeline.Hook = hooks.NewEntityBind(hooks.EntityBindDeps{})
