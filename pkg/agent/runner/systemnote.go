package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/authzed/openagentprimitives/pkg/memory"
)

// AppendSystemNoteFunc returns a closure that appends a system_note turn to the
// session memory. The content map is JSON-encoded as the turn text so
// downstream processors can unmarshal structured data (e.g., delivered IDs, or
// wrapped state-store payloads like plans).
//
// It is the single source for the state registry's AppendSystemNote: both
// internal/cmd/runner and the in-process e2e harness wire Deps.AppendSystemNote through
// this, so plan/state notes persist identically in production and tests.
//
// Memory turns are keyed by (Index, Role); two system_notes with the same index
// collide and the second returns memory.ErrIndexConflict. This closure assigns
// a unique index per call, lazily primed from the memory tail and refreshed on
// conflict (which can happen if the loop appends an assistant/user turn between
// our reads). Retries up to 5 times before giving up.
func AppendSystemNoteFunc(mc MemoryAppender) func(ctx context.Context, content map[string]any) error {
	var (
		mu        sync.Mutex
		nextIndex = -1 // -1 sentinel: needs (re-)prime from memory tail.
	)
	return func(ctx context.Context, content map[string]any) error {
		text, err := json.Marshal(content)
		if err != nil {
			return fmt.Errorf("appendSystemNote: marshal: %w", err)
		}
		mu.Lock()
		defer mu.Unlock()

		const maxRetries = 5
		for attempt := 0; attempt < maxRetries; attempt++ {
			if nextIndex < 0 {
				turns, rerr := mc.ReadAll(ctx)
				if rerr != nil {
					return fmt.Errorf("appendSystemNote: refresh tail: %w", rerr)
				}
				nextIndex = 0
				for _, t := range turns {
					if t.Index >= nextIndex {
						nextIndex = t.Index + 1
					}
				}
			}
			err := mc.Append(ctx, memory.Turn{
				Index:     nextIndex,
				Role:      "system_note",
				Content:   []memory.ContentBlock{{Type: "text", Text: string(text)}},
				CreatedAt: time.Now().UTC(),
			})
			if err == nil {
				nextIndex++
				return nil
			}
			// Index conflict → another writer claimed our index; refresh and
			// retry. turn.Appender surfaces this as memory.ErrIndexConflict (the
			// operator's Put rejects a differing payload at the same (index, role)).
			if errors.Is(err, memory.ErrIndexConflict) {
				nextIndex = -1
				continue
			}
			return err
		}
		return fmt.Errorf("appendSystemNote: index conflict after %d retries", maxRetries)
	}
}
