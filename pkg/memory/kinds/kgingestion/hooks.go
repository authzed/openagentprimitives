package kgingestion

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"sync"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/lifecycle"
)

type hooks struct {
	scope    memory.Scope
	provider memory.KGProvider
	mem      memory.Memory
	config   IngestionConfig

	batchMu sync.Mutex
	batch   []string
}

func (h *hooks) OnSignal(ctx context.Context, sig memory.Signal) error {
	// Operator in-process caller: inject a wildcard system approval so this
	// hook's h.mem.Query (via turnContent) passes the facade's capability door.
	// LOAD-BEARING, not defensive — a signal arrives on whatever ctx the Put
	// that raised it carried, which holds no approval of this hook's own, and
	// memory.Local.Query calls ensureApproval(ReadMemory) and fails closed. Drop
	// this and every turn read is denied: KG ingestion stops for the whole
	// session, and (by design here) stays quiet about it.
	ctx = memory.WithSystemApproval(ctx, "operator:kgingestion")
	switch sig.Kind {
	case lifecycle.SigTurnCompleted:
		return h.handleTurn(ctx, sig)
	case lifecycle.SigSessionCompleted:
		if h.config.Strategy == StrategyBatch {
			return h.flushBatch(ctx)
		}
	}
	return nil
}

func (h *hooks) handleTurn(ctx context.Context, sig memory.Signal) error {
	var payload struct {
		TurnIndex string `json:"turnIndex"`
		Role      string `json:"role"`
	}
	if sig.Payload != nil {
		if err := json.Unmarshal(sig.Payload, &payload); err != nil {
			// Falling through with the zero payload would read turn 0 of role
			// "" — a turn that does not exist — so the real turn is never
			// ingested. Say so rather than ingesting nothing in silence.
			slog.Info("kg ingestion: undecodable turn.completed payload",
				"scope", h.scope.ID, "err", err.Error())
			return nil
		}
	}

	content := h.turnContent(ctx, payload.TurnIndex, payload.Role)
	if content == "" {
		return nil
	}

	switch h.config.Strategy {
	case StrategyContentGated:
		if len(content) < h.config.MinContentLength {
			return nil
		}
		return h.ingest(ctx, content, payload.Role)
	case StrategyBatch:
		h.batchMu.Lock()
		h.batch = append(h.batch, content)
		h.batchMu.Unlock()
		return nil
	default:
		return h.ingest(ctx, content, payload.Role)
	}
}

func (h *hooks) ingest(ctx context.Context, content, role string) error {
	if err := h.provider.Ingest(ctx, memory.KGInput{
		GroupID: h.scope.ID,
		Content: content,
		Role:    role,
	}); err != nil {
		slog.Info("kg ingestion failed", "scope", h.scope.ID, "err", err.Error())
		return nil
	}
	return nil
}

func (h *hooks) flushBatch(ctx context.Context) error {
	h.batchMu.Lock()
	combined := ""
	for _, c := range h.batch {
		combined += c + "\n"
	}
	h.batch = nil
	h.batchMu.Unlock()

	if combined == "" {
		return nil
	}
	return h.ingest(ctx, combined, "batch")
}

func (h *hooks) turnContent(ctx context.Context, turnIndex, role string) string {
	idx := 0
	if n, err := strconv.Atoi(turnIndex); err == nil {
		idx = n
	}
	turnID := fmt.Sprintf("turn-%06d-%s", idx, role)
	res, err := h.mem.Query(ctx, memory.Query{
		Scope: h.scope,
		Kinds: []string{"turn"},
		IDs:   []string{turnID},
		Limit: 1,
	})
	if err != nil {
		// A failed read and a turn that is genuinely not there both yield "",
		// so without this line a memory outage is indistinguishable from an
		// empty turn: ingestion stops for the whole session with nothing in
		// the log. Only the failure is worth a line — a missing turn is
		// normal and stays quiet.
		slog.Info("kg ingestion: turn read failed",
			"scope", h.scope.ID, "turn", turnID, "err", err.Error())
		return ""
	}
	if len(res.Entries) == 0 {
		return ""
	}
	var turn struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(res.Entries[0].Content, &turn); err != nil {
		// A turn that will not decode is a corrupt record, not an absent one.
		slog.Info("kg ingestion: undecodable turn content",
			"scope", h.scope.ID, "turn", turnID, "err", err.Error())
		return ""
	}
	var text string
	for _, b := range turn.Content {
		if b.Text != "" {
			text += b.Text + "\n"
		}
	}
	return text
}
