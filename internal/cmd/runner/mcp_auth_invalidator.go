package main

import "sync"

// authInvalidatable is the slice of *mcpdispatch.MCPTool the invalidator needs:
// anything holding a credential frozen in memory that can be marked revoked.
type authInvalidatable interface{ InvalidateAuth() }

// mcpAuthInvalidator maps a backing Secret to the live MCPTools whose auth
// header was resolved from it, so a credential revocation reaches the frozen
// in-memory copies of the token — not just the broker's resolution cache.
//
// It satisfies credential.SecretInvalidator and is registered alongside the
// broker, so one revoke drops the cache AND marks every affected tool stale.
// Without it, InvalidateSecret would drop a cache entry that MCPTool never
// re-reads, and the runner would keep presenting the revoked token upstream.
type mcpAuthInvalidator struct {
	mu       sync.Mutex
	bySecret map[secretKey][]authInvalidatable
}

// secretKey identifies a backing Secret. Namespace is part of the identity: the
// same Secret name in two namespaces is two distinct credentials.
type secretKey struct{ namespace, name string }

func newMCPAuthInvalidator() *mcpAuthInvalidator {
	return &mcpAuthInvalidator{bySecret: map[secretKey][]authInvalidatable{}}
}

// register binds a tool to the Secret its credential was resolved from. A tool
// with several credential descriptors registers once per Secret. Called at
// session start, before the tool loop begins.
func (m *mcpAuthInvalidator) register(namespace, name string, t authInvalidatable) {
	if t == nil {
		return
	}
	k := secretKey{namespace: namespace, name: name}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.bySecret[k] = append(m.bySecret[k], t)
}

// InvalidateSecret implements credential.SecretInvalidator: it marks every tool
// bound to this Secret as holding a revoked credential.
//
// An unregistered Secret is a safe no-op — revocation delivery is at-most-once
// and idempotent, and a runner legitimately holds no tool for most credentials
// in the cluster. Returns nil because InvalidateAuth cannot fail; the signature
// exists to satisfy credential.SecretInvalidator.
func (m *mcpAuthInvalidator) InvalidateSecret(namespace, name string) error {
	m.mu.Lock()
	tools := m.bySecret[secretKey{namespace: namespace, name: name}]
	// Copy under lock, invalidate outside it: InvalidateAuth takes the tool's own
	// mutex, and holding two locks in an order this type does not control is how
	// deadlocks are born.
	targets := append([]authInvalidatable(nil), tools...)
	m.mu.Unlock()

	for _, t := range targets {
		t.InvalidateAuth()
	}
	return nil
}
