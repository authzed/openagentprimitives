// Package credential is the credential revocation Invalidator. The revoke key
// is "<secretNamespace>/<secretName>"; Invalidate notifies every registered
// holder of that credential so the next use re-reads it (failing closed if the
// Secret was deleted). Covers UserIdentity and AgentIdentity credentials alike.
//
// There is more than one holder. The broker's resolution cache is the obvious
// one, but a credential resolved once at session start may also have been frozen
// into a live object — the runner's MCPTool copies (header, value) into struct
// fields and never re-reads the broker cache. Dropping the cache alone would
// leave that copy live, so Invalidate fans out to every target.
package credential

import (
	"errors"
	"fmt"
	"strings"

	"github.com/authzed/openagentprimitives/pkg/authz/revocation"
)

// SecretInvalidator is the slice of broker.Broker this needs. Anything holding a
// credential resolved from a Secret implements it — the broker's cache, and the
// runner's set of MCPTools whose auth header was frozen at session start.
type SecretInvalidator interface {
	// InvalidateSecret drops every in-process copy of the credential resolved
	// from the named Secret, so the next use re-resolves it (or fails). An error
	// means a stale copy may still be in use and the revocation is incomplete —
	// the Invalidator joins and returns it rather than reporting success.
	InvalidateSecret(namespace, name string) error
}

// Invalidator is the credential revocation Invalidator.
type Invalidator struct{ targets []SecretInvalidator }

// New constructs the credential Invalidator over every holder of the credential.
// Passing none is accepted here and rejected at Invalidate time, so a
// misconfigured caller fails loudly on the first revoke rather than silently
// dropping every revocation for the life of the process.
func New(targets ...SecretInvalidator) *Invalidator { return &Invalidator{targets: targets} }

// Kind implements revocation.Invalidator.
func (i *Invalidator) Kind() string { return "credential" }

// Noun implements revocation.Invalidator.
//
// "A connected account" rather than "a credential" or "a secret": what was
// revoked is, from the reader's side, the thing they signed in to and connected
// — the vocabulary the identity setup flows and the account-settings portal
// already use with them. It covers UserIdentity and AgentIdentity alike, which
// is right, because the reader does not know or care which one held the token.
func (i *Invalidator) Noun() string { return "a connected account" }

// Invalidate parses key "<namespace>/<name>" and invalidates that Secret in
// every target.
//
// Every target is attempted even if an earlier one fails: a revocation that
// reached only some holders of a credential is strictly worse than one that
// reports failure, because the caller would believe the credential is dead while
// a live copy of it keeps authenticating. Failures are joined and returned, and
// the subscriber treats a returned error as a refusal: it logs at Error AND
// skips its onRevoked hook, so nothing appends a signed Revoked record claiming
// a credential is dead while a live copy of it is still authenticating.
func (i *Invalidator) Invalidate(key string) error {
	ns, name, ok := strings.Cut(key, "/")
	if !ok || ns == "" || name == "" {
		return fmt.Errorf("credential revoke: malformed key %q (want <namespace>/<name>)", key)
	}
	if len(i.targets) == 0 {
		return fmt.Errorf("credential revoke %q: no invalidation targets registered", key)
	}
	var errs []error
	for _, t := range i.targets {
		if err := t.InvalidateSecret(ns, name); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

var _ revocation.Invalidator = (*Invalidator)(nil)
