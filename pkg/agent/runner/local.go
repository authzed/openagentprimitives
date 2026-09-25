// pkg/agent/runner/local.go
//
// Local-mode helpers for the runner Loop. The kubectl-driven and channel-
// attached runtimes both run inside a pod with a real AgentSession CR and
// an HTTP-backed memory client. A gen-agent driver (e.g.
// pkg/platform/identity/setup/llmagent) runs in-process inside the `oap`
// binary with no CR, no cluster, and an in-memory transient memory store.
// These helpers wire that mode without touching any of the
// controller-facing code paths.
package runner

import (
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/turn"
)

// LocalStatusPatcher returns a *StatusPatcher whose mutate / Get methods
// short-circuit to nil. Used by in-process callers (the gen-agent loop)
// that don't have an AgentSession CR to patch. WriteSucceeded captures
// the AgentResult into LocalResult() so the caller can read the agent's
// final summary back.
func LocalStatusPatcher() *StatusPatcher {
	return &StatusPatcher{local: true}
}

// LocalMemoryStore returns a MemoryAppender backed by a fresh in-process
// memory.Local over the inmem Kind backend, scoped to `key`. Suitable for
// a single gen-agent session; state vanishes when the *oap* invocation
// exits.
func LocalMemoryStore(key memory.NamespacedName) MemoryAppender {
	return LocalMemoryAdapter(memory.NewLocal(inmem.NewBackend()), key)
}

// LocalMemoryAdapter binds an in-process memory.Memory to a session scope
// derived from key, returning a turn.Appender — the same transcript
// client production wires over HTTP. Used by LocalMemoryStore and by
// runner tests that want a transcript without an HTTP hop.
func LocalMemoryAdapter(mem memory.Memory, key memory.NamespacedName) *turn.Appender {
	return turn.NewAppender(mem, memory.Scope{
		Kind: "session",
		ID:   key.Namespace + "/" + key.Name,
	})
}
