// Package toolsession registers the tool_session Kind — one Entry per
// parsed event from an interactive tool's stream (e.g. Claude Code's
// stream-json). Events are append-only and ordered by CreatedAt.
package toolsession

import (
	"context"
	"reflect"

	"github.com/authzed/openagentprimitives/pkg/memory"
)

const (
	KindName = "tool_session"
	IDPrefix = "toolsess-"

	// softCap bounds a scope's tool_session entries (FIFO eviction past
	// it). A long `full`-mode session can emit many text-delta events;
	// the cap protects the in-memory backend. --follow-live tails
	// forward, so eviction of the oldest history is acceptable.
	softCap = 2000
)

type Kind struct{}

func (Kind) Name() string     { return KindName }
func (Kind) IDPrefix() string { return IDPrefix }

// WriteAuthority: the runner records its own tool-session bookkeeping
// (internal/cmd/runner/main.go).
func (Kind) WriteAuthority() memory.WriteAuthority { return memory.SessionWritten }
func (Kind) Retention() memory.Retention {
	return memory.Retention{
		SoftCapPerScope: softCap,
		AppendOnly:      true, // tool session records are per-invocation audit context — write-once.
	}
}
func (Kind) ContentSchema() reflect.Type                    { return nil }
func (Kind) IndexedFields() []string                        { return nil }
func (Kind) NewScopeHooks(_ memory.Scope) memory.ScopeHooks { return hooks{} }

type hooks struct{}

func (hooks) OnSignal(_ context.Context, _ memory.Signal) error { return nil }

func init() { memory.RegisterKind(Kind{}) }
