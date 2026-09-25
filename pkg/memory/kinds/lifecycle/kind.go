package lifecycle

import (
	"reflect"
	"sync"

	"github.com/authzed/openagentprimitives/pkg/memory"
)

var (
	// memRef is set by Setup() and is the memory.Memory the hooks Put
	// lifecycle Entries into. Unusual among Kinds: these hooks write
	// entries, where most only react to a signal to update internal state.
	memMu  sync.RWMutex
	memRef memory.Memory
)

// KindName is the registered name of this memory Kind.
const KindName = "lifecycle"

type Kind struct{}

func (Kind) Name() string     { return KindName }
func (Kind) IDPrefix() string { return "lifecycle-" }

// WriteAuthority: the runner appends its own phase transitions
// (pkg/agent/runner/sequencer.go). The operator and authzd append too, on
// their own doors.
func (Kind) WriteAuthority() memory.WriteAuthority { return memory.SessionWritten }
func (Kind) Retention() memory.Retention           { return memory.Retention{AppendOnly: true} } // lifecycle events are a tamper-evident session timeline — write-once.
func (Kind) ContentSchema() reflect.Type           { return nil }
func (Kind) IndexedFields() []string               { return nil }
func (Kind) NewScopeHooks(scope memory.Scope) memory.ScopeHooks {
	return &hooks{scope: scope}
}

// Setup wires the lifecycle hooks to m. Call this once per Memory
// instance after construction. The operator's main.go calls this after
// memory.NewLocal — the lifecycle hook runs operator-side.
func Setup(m memory.Memory) {
	memMu.Lock()
	memRef = m
	memMu.Unlock()
}

// Teardown clears the wiring. Tests use this; production never does.
func Teardown() {
	memMu.Lock()
	memRef = nil
	memMu.Unlock()
}

func init() { memory.RegisterKind(Kind{}) }
