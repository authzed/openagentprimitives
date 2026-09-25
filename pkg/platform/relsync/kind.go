// Package relsync defines the seam between the RelationshipSource
// reconciler and the directories it syncs. A Kind knows how to enumerate
// and read one upstream; everything about ordering, diffing, pruning and
// resumption lives in the reconciler and is identical for every kind.
package relsync

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/authzed/openagentprimitives/pkg/authz/spicedb"
	"github.com/authzed/openagentprimitives/pkg/authz/spicedb/relsource"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/sensitive"
	"github.com/authzed/openagentprimitives/pkg/x/kindregistry"
)

// ScopeID identifies a scope within one source. It is OURS: stable,
// sortable, and non-expiring, which is why it is the only progress marker
// that reaches the CR. Upstream pagination tokens are not.
type ScopeID string

// Scope is one unit of sync — a Slack channel, a 1Password group. Its
// tuples are fetched, diffed and written atomically.
type Scope struct {
	ID ScopeID
	// ResourceType is the SpiceDB definition this scope's tuples live on
	// (e.g. "slack_channel"). Used to bound read-back and to address the
	// hash sentinel.
	ResourceType string
}

// Cursor is an upstream pagination token, opaque to everything but the kind
// that minted it. It is tagged so a token can never cross kinds, and it is
// never persisted: upstream cursors expire, and a stored token replayed
// fifteen minutes later is a defect waiting for a large workspace.
type Cursor struct {
	Kind  string
	Token string
}

// ForKind returns the raw token if this cursor belongs to kind, and an
// error naming both kinds otherwise. The zero Cursor belongs to everyone —
// it means "start at the beginning".
func (c Cursor) ForKind(kind string) (string, error) {
	if c.Kind == "" && c.Token == "" {
		return "", nil
	}
	if c.Kind != kind {
		return "", fmt.Errorf("relsync: cursor minted by %q cannot be consumed by %q", c.Kind, kind)
	}
	return c.Token, nil
}

// ScopePage is one page of an enumeration.
type ScopePage struct {
	Scopes []Scope
	// Next is the cursor for the following page. Zero when exhausted.
	Next Cursor
	// Complete reports whether the enumeration finished. It is FALSE when
	// paging stopped early — a partial list that is non-empty and
	// well-formed and short. Reaping is gated on this, because an
	// emptiness check cannot tell a truncated list from a whole one.
	// Returning true for a page you stopped fetching early arms the reaper
	// against every scope beyond it: each one is pruned as though it no
	// longer exists upstream.
	Complete bool
}

// SourceParams is everything one RelationshipSource CR contributes to a
// kind's calls: the resolved credential, the per-install endpoint, and the
// kind's own configuration, all verbatim.
//
// Named for what it carries rather than for the credential alone: a kind is
// a process-wide registered singleton, so nothing per-CR may live on the
// kind itself, and this type is the only channel for it.
type SourceParams struct {
	Token sensitive.SensitiveValue
	// Endpoint is spec.baseURL, verbatim. Empty for kinds that talk to a
	// fixed vendor endpoint; a kind that needs one refuses when it is empty.
	Endpoint string
	// Config is spec.config, verbatim and unparsed — the kind owns its own
	// shape and validates it. Nil when the CR sets none.
	Config json.RawMessage
}

// ScopeContent is what one scope's fetch produced.
type ScopeContent struct {
	Tuples []spicedb.Tuple
	// JoinMisses counts upstream members dropped because their identity
	// resolved to no platform user. They are deliberately not written —
	// they must resolve to nobody — but the count is reported, because a
	// silent partial membership looks exactly like a complete one. See
	// sync.go's Pass, which sums this into PassResult.JoinMisses.
	JoinMisses int
}

// Kind is one directory OAP can sync.
type Kind interface {
	// Name is the spec.kind value that selects this kind.
	Name() string
	// ListScopes returns one page of visible scopes. Resumable.
	ListScopes(ctx context.Context, params SourceParams, after Cursor) (ScopePage, error)
	// FetchScope returns everything one scope's fetch produced, paginated
	// internally to completion. ATOMIC: it returns a complete ScopeContent
	// or an error, never a partial one, because the per-scope prune deletes
	// whatever is absent.
	FetchScope(ctx context.Context, params SourceParams, s Scope) (ScopeContent, error)
	// Source is the relsource.Source this kind writes as. Its Claims must
	// cover every relation the kind writes, including its #relhash relations.
	Source() relsource.Source
}

// ErrScopeGone is returned by FetchScope when the scope no longer exists
// upstream. It means "prune this scope", and is deliberately distinct from
// an empty result, which means "this scope exists and has no members".
var ErrScopeGone = errors.New("relsync: scope is gone upstream")

// reg is the process-global registry of sync kinds. It is never reset
// between tests: production kinds register into this registry from init()
// in each kind's own package (a later task's Slack kind, for one), and a
// test-only Reset would erase those, breaking any test that ran afterward
// in the same binary. Deliberately no Reset() is exported here — see
// pkg/authz/spicedb/relsource for the same choice and rationale.
var reg = kindregistry.New[Kind]("relsync", Kind.Name)

// Register adds k under k.Name(), panicking on an empty or duplicate name —
// a programmer error that must surface at init(), matching every other kind
// registry in the repo.
func Register(k Kind) { reg.Register(k) }

// Get looks up a registered Kind by name.
func Get(name string) (Kind, bool) { return reg.Get(name) }

// All returns every registered Kind, sorted by name.
func All() []Kind { return reg.All() }
