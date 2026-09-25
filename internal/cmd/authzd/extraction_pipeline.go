package main

import (
	"context"
	"fmt"
	"time"

	"github.com/authzed/openagentprimitives/pkg/authz"
	authzdhost "github.com/authzed/openagentprimitives/pkg/authz/authzd/pipelinehost"
	"github.com/authzed/openagentprimitives/pkg/authz/extract"
	"github.com/authzed/openagentprimitives/pkg/authz/hooks"
	"github.com/authzed/openagentprimitives/pkg/memory"
	asc "github.com/authzed/openagentprimitives/pkg/memory/kinds/authz_session_config"
	eex "github.com/authzed/openagentprimitives/pkg/memory/kinds/extracted_entity"
	exs "github.com/authzed/openagentprimitives/pkg/memory/kinds/extraction_state"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"
)

// runExtractionViaExecutor drives one inbox turn's entity extraction through the
// generic pipeline executor: the EntityBind hook (prefilter → extractor LLM →
// extracted_entity / extraction_state). Replaces processOne's inline body. The
// worker keeps the goroutine/idle/backlog/cursor machinery and the
// authz_session_config read (cfg); only the per-turn work delegates here.
//
// The extractor LLM runs inside the hook's Eval — i.e. inside authzd's process
// — preserving the prompt-injection quarantine. EntityBind is advisory (always
// Allow, never an ApprovalAsk), so the authzd Host's approval methods are
// unreachable on this path.
func (w *Worker) runExtractionViaExecutor(ctx context.Context, scope memory.Scope, cfg asc.Content, t memory.Turn) error {
	// Only slots the class lets the extractor fill reach this path. Both the
	// prefilter and the extractor prompt are narrowed, so a slot declared
	// fillFrom: [ask] is neither used to justify an extractor call nor named in
	// the prompt as something to look for in the user's text.
	extractable := slotsFillableByQuery(cfg.BoundEntities)
	specs, err := toBoundEntitySpecs(extractable)
	if err != nil {
		// A slot precondition that will not compile is a class the AgentClass
		// reconciler should already have refused. Returned rather than degraded
		// into "extract nothing": this path's caller records extraction_state,
		// so the reason reaches the session's own record instead of only a log.
		return fmt.Errorf("runExtractionViaExecutor: adapt bound entities: %w", err)
	}

	host := authzdhost.New(authzdhost.Deps{
		Session: authzdhost.SessionRef{Namespace: nsOf(scope), Name: nameOf(scope)},
	})

	hook := hooks.NewEntityBind(hooks.EntityBindDeps{
		BoundEntities: specs,
		Prefilter:     authz.Prefilter,
		Extract: func(ctx context.Context, text string, _ []authz.BoundEntitySpec) ([]extract.ExtractedEntity, error) {
			extCtx, cancel := context.WithTimeout(ctx, w.deps.ExtractTimeout)
			defer cancel()
			return w.deps.Extractor.Extract(extCtx, extract.ExtractInput{
				UserMessage:    text,
				EntityTypes:    extractable,
				PerToolPrompts: cfg.PerToolPrompts,
			})
		},
		RecordEntity: func(ctx context.Context, turnIdx int, e extract.ExtractedEntity) error {
			return eex.Record(ctx, w.deps.Memory, scope, eex.Content{
				ResourceType: e.ResourceType,
				ResourceID:   e.ResourceID,
				SourceText:   e.SourceText,
				TurnIndex:    turnIdx,
				ExtractedAt:  time.Now().UTC(),
			})
		},
		RecordState: func(ctx context.Context, s hooks.EntityBindState) error {
			return exs.Record(ctx, w.deps.Memory, scope, exs.Content{
				TurnIndex:      s.TurnIndex,
				Status:         s.Status,
				StartedAt:      s.StartedAt,
				CompletedAt:    s.CompletedAt,
				CandidateCount: s.CandidateCount,
				Error:          s.Error,
			})
		},
	})

	reg := pipeline.NewRegistry()
	reg.Register(hook, hooks.OrderEntityBind)
	exec := pipeline.NewExecutor(reg)
	_, err = exec.Run(ctx, pipeline.InboundTurn, pipeline.Input{
		Session: pipeline.SessionRef{Namespace: nsOf(scope), Name: nameOf(scope)},
		// The session subject the operator resolved; see the audit note that
		// extraction drops the TURN author in favour of it.
		Requester: identity.CanonicalFromTrusted(cfg.Subject,
			"session subject resolved by the operator at session start"),
		Turn: &pipeline.TurnInfo{Text: firstTextBlock(t), InboxIdx: t.Index},
	}, host)
	return err
}
