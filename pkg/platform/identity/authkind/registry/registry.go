// Package registry holds the global authkind registry. Each kind under
// pkg/platform/identity/authkind/<x>/ calls Register from an init() so importing
// the package wires it in. Storage/mutex/sort/panic-on-dup live in
// pkg/x/kindregistry; the exported funcs here are thin forwarders plus the
// authkind-specific ParseBindingMatch.
package registry

import (
	"errors"
	"fmt"
	"strings"

	"github.com/authzed/openagentprimitives/pkg/platform/identity/authkind"
	"github.com/authzed/openagentprimitives/pkg/x/kindregistry"
)

// reg is the process-wide authkind registry, keyed by Kind.Prefix().
var reg = kindregistry.New[authkind.Kind]("authkind", authkind.Kind.Prefix)

// ErrUnknownPrefix is returned by ParseBindingMatch when the prefix
// portion of a binding-match string is not registered.
var ErrUnknownPrefix = errors.New("authkind: unknown prefix")

// Register adds k to the registry. Panics on duplicate prefix.
func Register(k authkind.Kind) { reg.Register(k) }

// ByPrefix returns the kind registered under prefix.
func ByPrefix(prefix string) (authkind.Kind, bool) { return reg.Get(prefix) }

// All returns a snapshot of every registered kind, sorted by prefix.
func All() []authkind.Kind { return reg.All() }

// ParseBindingMatch splits a binding-match string of the form "<prefix>:<suffix>"
// and returns the registered kind plus the suffix. The error wraps
// ErrUnknownPrefix when the prefix is syntactically valid but not registered, so
// callers can tell that apart with errors.Is; a malformed-input error wraps no
// sentinel.
func ParseBindingMatch(s string) (authkind.Kind, string, error) {
	idx := strings.Index(s, ":")
	if idx <= 0 || idx == len(s)-1 {
		return nil, "", fmt.Errorf("authkind: malformed binding-match %q (want <prefix>:<suffix>)", s)
	}
	prefix := s[:idx]
	suffix := s[idx+1:]
	k, ok := ByPrefix(prefix)
	if !ok {
		return nil, "", fmt.Errorf("%w: %q", ErrUnknownPrefix, prefix)
	}
	return k, suffix, nil
}

// Reset clears the registry. Test-only; do not call from production code.
func Reset() { reg.Reset() }
