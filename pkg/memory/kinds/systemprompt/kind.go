// Package systemprompt is the memory Kind for the append-only record of the
// system prompt an agent actually ran under.
//
// The audit log recorded which MODEL was asked (turn.Model) but never what it
// was ASKED. That asymmetry is the gap: the chain can prove an agent cloned a
// repository and cannot say under what instructions, so an investigator cannot
// tell a prompt regression from a model regression — and repo instructions,
// skills and artifact-kind guidance all compose INTO this text, which makes it
// attack surface whose alteration otherwise leaves no trace in a record built
// to be tamper-evident.
//
// One entry per DISTINCT prompt per session, not per turn: the text is constant
// across a session's turns, so per-turn writes would be pure bloat, but repo
// instructions and skills are session state and can change under the agent —
// and that change is exactly what this exists to make visible.
package systemprompt

import (
	"context"
	"reflect"
	"time"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/lifecycle"
)

// Content is one recorded system prompt.
//
// Deliberately no timestamp field. The entry is keyed by its digest so a re-put
// of an unchanged prompt is byte-identical and idempotent, and a wall-clock
// value inside the content would make every re-put differ and conflict. The
// entry's own CreatedAt carries when it was first seen.
type Content struct {
	// Digest is the hex sha256 of Prompt — plain, so "is what I shipped what is
	// running" is checkable offline against a locally-composed prompt.
	Digest string `json:"digest"`
	// Prompt is the full composed text. The digest answers whether it changed;
	// only the text answers what it said.
	Prompt string `json:"prompt"`
}

type Kind struct{}

func (Kind) Name() string     { return "system_prompt" }
func (Kind) IDPrefix() string { return "sysprompt-" }

// WriteAuthority: the runner records the system prompt it actually composed
// and ran under.
func (Kind) WriteAuthority() memory.WriteAuthority { return memory.SessionWritten }
func (Kind) Retention() memory.Retention {
	return memory.Retention{
		ArchiveOn:       []memory.SignalKind{lifecycle.SigSessionCompleted, lifecycle.SigSessionFailed},
		TTLAfterArchive: 90 * 24 * time.Hour,
		// The instructions an agent ran under are evidence. A rewritten prompt
		// would erase why it acted, which is the one thing this record exists
		// to preserve.
		AppendOnly: true,
	}
}
func (Kind) ContentSchema() reflect.Type                    { return reflect.TypeOf(Content{}) }
func (Kind) IndexedFields() []string                        { return []string{"digest"} }
func (Kind) NewScopeHooks(_ memory.Scope) memory.ScopeHooks { return hooks{} }

type hooks struct{}

func (hooks) OnSignal(_ context.Context, _ memory.Signal) error { return nil }

func init() { memory.RegisterKind(Kind{}) }
