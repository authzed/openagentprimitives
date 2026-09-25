package memory

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// Permission names a gated capability on the memory data plane. An "internal:"
// prefix marks the dangerous tier (see approval_internal.go) — those can only be
// authorized by pkg/memory's own trusted path, never by a bearer or system
// approval.
type Permission string

const (
	ReadMemory   Permission = "memory:read"
	WriteMemory  Permission = "memory:write"
	DeleteMemory Permission = "memory:delete"
	ReadKG       Permission = "kg:read"

	// AppendAudit gates append-only (tamper-evident) writes. Internal tier:
	// mintable only inside pkg/memory, by the provenance verify-on-write path.
	AppendAudit Permission = "internal:append-audit"
)

// internal reports whether p is in the dangerous "internal:" tier — permissions
// that may be authorized only by pkg/memory's own trusted path, never by a
// bearer or system approval.
func (p Permission) internal() bool {
	return strings.HasPrefix(string(p), "internal:")
}

// ErrMissingApproval is returned by EnsureApproval when the context carries no
// approval authorizing the requested (permission, resource). It is a sentinel so
// callers can map it to the right surface (HTTP 403, a denied CLI message).
var ErrMissingApproval = errors.New("memory: missing capability approval")

// Approval is an opaque witness that authorization for (perm, resource) was
// established. Its fields are unexported, so a constructor in this package is
// the ONLY way to obtain one: an outside package can neither forge a value nor
// write it into a context, since the context key is unexported too.
type Approval struct {
	perm     Permission
	resource string // canonical resource id, e.g. a scope "<ns>/<name>"
	source   string // "bearer-token" | "spicedb" | "system" | "internal"
	evidence string // tokenID / spicedb subject / component — for audit logging only
}

// approvalsKey is unexported so no other package can write the approval set via
// context.WithValue — the other half of the forge-prevention guarantee.
type approvalsKey struct{}

// WithApproval returns a context carrying the given approvals appended to any
// already present. A nil/empty call is a no-op.
func WithApproval(ctx context.Context, a ...Approval) context.Context {
	if len(a) == 0 {
		return ctx
	}
	existing, _ := ctx.Value(approvalsKey{}).([]Approval)
	merged := make([]Approval, 0, len(existing)+len(a))
	merged = append(merged, existing...)
	merged = append(merged, a...)
	return context.WithValue(ctx, approvalsKey{}, merged)
}

func approvalsFrom(ctx context.Context) []Approval {
	a, _ := ctx.Value(approvalsKey{}).([]Approval)
	return a
}

// EnsureApproval returns nil iff ctx carries an approval authorizing
// (perm, resource); otherwise an error wrapping ErrMissingApproval that names
// the missing perm and resource. Rules differ by tier:
//
//   - INTERNAL-tier perms (internal: prefix, e.g. AppendAudit) are satisfied
//     ONLY by an internal-source approval matching (perm, resource) exactly.
//     The unexported mintInternal is the sole producer of one, so a bearer or
//     system approval — even one naming an internal: perm via the exported
//     ForBearerToken/ForSpiceDBCheck — can never satisfy it. That is what makes
//     "only pkg/memory can authorize an append-only write" unforgeable rather
//     than conventional.
//   - NON-internal perms are satisfied by a system approval (any resource), or
//     by an exact (perm, resource) match from any source.
func EnsureApproval(ctx context.Context, perm Permission, resource string) error {
	for _, a := range approvalsFrom(ctx) {
		if perm.internal() {
			if a.source == "internal" && a.perm == perm && a.resource == resource {
				return nil
			}
			continue
		}
		if a.source == "system" {
			return nil
		}
		if a.perm == perm && a.resource == resource {
			return nil
		}
	}
	return fmt.Errorf("%w: perm=%s resource=%s", ErrMissingApproval, perm, resource)
}

// ForBearerToken mints an approval for a validated bearer token already
// confirmed to authorize `resource`. CONTRACT: call only after the token-scope
// check has passed.
func ForBearerToken(perm Permission, resource, tokenID string) Approval {
	return Approval{perm: perm, resource: resource, source: "bearer-token", evidence: tokenID}
}

// ForSpiceDBCheck mints an approval established by a successful SpiceDB check.
// CONTRACT: call ONLY inside a Check == HAS_PERMISSION branch.
func ForSpiceDBCheck(perm Permission, resource, subject string) Approval {
	return Approval{perm: perm, resource: resource, source: "spicedb", evidence: subject}
}

// ForSystemCaller mints a wildcard approval for a trusted in-process/system
// caller (operator controllers, ingestion hooks, the system bearer tokens). It
// satisfies any NON-internal permission for any resource, and can NEVER satisfy
// an internal-tier one — EnsureApproval enforces that.
func ForSystemCaller(component string) Approval {
	return Approval{source: "system", evidence: component}
}

// WithSystemApproval is sugar for WithApproval(ctx, ForSystemCaller(component)).
func WithSystemApproval(ctx context.Context, component string) context.Context {
	return WithApproval(ctx, ForSystemCaller(component))
}
