package kgingestion

import (
	"context"
	"reflect"
	"sync"

	"github.com/authzed/openagentprimitives/pkg/memory"
)

var (
	mu        sync.RWMutex
	kgProv    memory.KGProvider
	memRef    memory.Memory
	configRef IngestionConfig
)

// KindName is the registered name of this memory Kind.
const KindName = "kg_ingestion"

type Kind struct{}

func (Kind) Name() string     { return KindName }
func (Kind) IDPrefix() string { return "kgi-" }

// WriteAuthority: hooks-only: nothing Puts this kind at all. It exists to
// carry ScopeHooks, and the fail-closed default is the correct answer for a
// kind with no writer.
func (Kind) WriteAuthority() memory.WriteAuthority { return memory.ComponentWritten }
func (Kind) Retention() memory.Retention           { return memory.Retention{} }
func (Kind) ContentSchema() reflect.Type           { return nil }
func (Kind) IndexedFields() []string               { return nil }

func (Kind) NewScopeHooks(scope memory.Scope) memory.ScopeHooks {
	mu.RLock()
	defer mu.RUnlock()
	if kgProv == nil {
		return noopHooks{}
	}
	return &hooks{
		scope:    scope,
		provider: kgProv,
		mem:      memRef,
		config:   configRef,
	}
}

func Setup(m memory.Memory, provider memory.KGProvider, config IngestionConfig) {
	mu.Lock()
	defer mu.Unlock()
	memRef = m
	kgProv = provider
	configRef = config
}

func Teardown() {
	mu.Lock()
	defer mu.Unlock()
	memRef = nil
	kgProv = nil
}

type noopHooks struct{}

func (noopHooks) OnSignal(_ context.Context, _ memory.Signal) error { return nil }

func init() { memory.RegisterKind(Kind{}) }
