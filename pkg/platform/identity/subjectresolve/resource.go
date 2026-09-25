package subjectresolve

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// resourceTypePattern matches a lowercase SpiceDB object-type identifier.
var resourceTypePattern = regexp.MustCompile(`^[a-z][a-z0-9_]{1,62}$`)

// resourceIDPattern matches the SpiceDB object-id charset. The length ceiling
// (1024) is checked separately in splitTypeID: Go's regexp engine caps a
// literal repeat count at 1000, below the id's own limit.
var resourceIDPattern = regexp.MustCompile(`^[a-zA-Z0-9/_|\-=+]+$`)

// maxResourceIDLen is the SpiceDB object-id length ceiling.
const maxResourceIDLen = 1024

// ErrNoRelationReader is returned — deliberately as an ERROR, never a silent
// unresolved Resolution — when a well-formed "<type>:<id>" reference needs a
// RelationReader to resolve and env.Relations is nil. A caller with no
// RelationReader configured at all (rather than one that legitimately found
// no linked user) must not be told the SAME thing a resource with no
// sole_user is told; those are different facts; a caller that wants to
// PRESENT them the same way (e.g. an HTTP handler with no RelationReader
// wired, where an email-form reference should still work) must translate
// this sentinel itself via errors.Is, not rely on Resolve to blur it.
var ErrNoRelationReader = errors.New("subjectresolve: no RelationReader configured")

// resourceResolver is the generic "<type>:<id>" fallback: any resource the
// platform already links to a user via its sole_user relation. It must
// stay the LAST registered resolver (see registry.go's init) — it is the
// catch-all every other scheme is tried ahead of.
type resourceResolver struct{}

func (resourceResolver) Usage() (string, string) {
	return "<type>:<id>", "the platform user linked to that resource via its sole_user relation (e.g. github_user:12345)"
}

func (resourceResolver) TryResolve(ctx context.Context, ref string, env Env) (Resolution, bool, error) {
	return resolveTypeID(ctx, ref, env)
}

// resolveTypeID is the shared "<type>:<id>" resolution shared by
// resourceResolver directly and by triggerAuthorResolver's recursion into a
// resource reference stripped of its "#<relation>" suffix.
//
// matched=false means ref is not even shaped like "<type>:<id>" — either
// syntactically (no colon, more than one colon) or by charset (type/id fail
// their SpiceDB-identifier patterns) — and the caller must fall through
// rather than ever handing an unvalidated string to SpiceDB.
func resolveTypeID(ctx context.Context, ref string, env Env) (Resolution, bool, error) {
	typ, id, ok := splitTypeID(ref)
	if !ok {
		return Resolution{}, false, nil
	}
	if env.Relations == nil {
		return Resolution{}, true, ErrNoRelationReader
	}

	// Durable authority traverses sole_user ONLY, never user: the schema's
	// #user relation is session membership — legitimately multi-claimant,
	// with sole_user derived level-triggered from it — so reading #user here
	// could resolve a possibly-stale claimant during the derivation window
	// (see pkg/controllers/useridentity/attested_edge.go). Zero sole_user
	// subjects therefore means unresolved, even when #user has exactly one.
	sole, err := env.Relations.UserSubjects(ctx, typ, id, "sole_user")
	if err != nil {
		return Resolution{}, true, err
	}
	switch len(sole) {
	case 1:
		return Resolution{Subject: sole[0]}, true, nil
	case 0:
		return Resolution{Reason: fmt.Sprintf("%s:%s has no linked platform user", typ, id)}, true, nil
	default:
		// More than one sole_user is an invariant violation; fail closed.
		return Resolution{Reason: fmt.Sprintf("%s:%s has no unambiguous linked platform user", typ, id)}, true, nil
	}
}

// splitTypeID parses and shape-validates "<type>:<id>": exactly one colon,
// a lowercase SpiceDB-identifier type, and an id in the SpiceDB object-id
// charset. Validation happens BEFORE any SpiceDB call, so a malformed
// reference never reaches the RelationReader.
func splitTypeID(ref string) (typ, id string, ok bool) {
	idx := strings.Index(ref, ":")
	if idx <= 0 || idx == len(ref)-1 {
		return "", "", false
	}
	typ, id = ref[:idx], ref[idx+1:]
	if strings.Contains(id, ":") {
		return "", "", false // more than one colon: ambiguous, not this shape
	}
	if len(id) > maxResourceIDLen {
		return "", "", false
	}
	if !resourceTypePattern.MatchString(typ) || !resourceIDPattern.MatchString(id) {
		return "", "", false
	}
	return typ, id, true
}
