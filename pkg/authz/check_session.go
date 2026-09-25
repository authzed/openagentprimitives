package authz

import (
	"context"
	"errors"
	"fmt"

	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// SessionChecker is the SpiceDB-side dependency CheckSessionInteract
// wraps. Implemented by *spicedb.Client; abstracted so tests can fake it.
type SessionChecker interface {
	// CheckInteract reports whether canonicalID holds agentsession#interact on
	// ns/name — may this user message this session. fullyConsistent=true reads
	// at head. Returns (false, err) on failure; the Interact hook denies on it
	// (fail-CLOSED), marking the deny transient so it is not shown to the user
	// as "not authorized".
	CheckInteract(ctx context.Context, ns, name string, canonicalID identity.CanonicalUserID, fullyConsistent bool) (bool, error)
}

// CheckSessionInteract — channelsd-side authz: can <subject> message in
// this session? Returns (allowed, err). Wraps spicedb.Client.CheckInteract.
func CheckSessionInteract(ctx context.Context, sdb SessionChecker,
	scope SessionRef, canonicalID identity.CanonicalUserID, fullyConsistent bool) (bool, error) {
	if sdb == nil {
		return false, nil
	}
	return sdb.CheckInteract(ctx, scope.Namespace, scope.Name, canonicalID, fullyConsistent)
}

// SessionConverseChecker is the SpiceDB-side dependency for the agent-to-agent
// arm of CheckSessionInbound. Implemented by *spicedb.Client.
type SessionConverseChecker interface {
	// CheckConverse reports whether the session (senderNS, senderName) holds
	// agentsession#converse on (ns, name) — may that session put a turn into
	// this one. Callers MUST bind fullyConsistent=true: the lineage tuples are
	// written by the same controller pass that creates the child, so its first
	// message races a MinimizeLatency read. (false, err) on failure; callers
	// deny.
	CheckConverse(ctx context.Context, ns, name, senderNS, senderName string, fullyConsistent bool) (bool, error)
}

// SessionInboundChecker is both arms of the inbound-turn gate: the human one
// (agentsession#interact, user-typed) and the agent-to-agent one
// (agentsession#converse, agentsession-typed). CheckSessionInbound needs both
// because it dispatches on the acting subject's type.
type SessionInboundChecker interface {
	SessionChecker
	SessionConverseChecker
}

// ErrActorTypeUnsupported marks a DEFINITIVE refusal: the acting subject is a
// SpiceDB object type for which no inbound permission exists, so no tuple
// anybody could write would change the answer. It is deliberately distinct
// from a backend failure, which is transient and must be retried — the
// Interact hook consults errors.Is on this to keep an unretryable refusal out
// of the drop-and-retry path.
var ErrActorTypeUnsupported = errors.New("authz: acting subject type cannot send a turn into a session")

// CheckSessionInbound is the single inbound-turn gate: may `actor` put a turn
// into this session?
//
// It dispatches on the acting subject's SpiceDB object TYPE, because the two
// kinds of sender are two different questions against two different
// permissions, and collapsing them is how the agent-to-agent path shipped
// permanently denied:
//
//   - user:<canonical>          → agentsession#interact  (a person, or a group
//     they belong to, with standing on the session)
//   - agentsession:<ns>/<name>  → agentsession#converse  (the session's parent
//     or one of its delegated children)
//
// Nothing here widens interact. An agentsession subject never reaches
// CheckInteract, whose only relations are user-typed; a user subject never
// reaches CheckConverse, whose only relations are agentsession-typed. Which
// subject types a Channel may assert in the first place is settled earlier, at
// the pipeline's ValidateSubject gate.
//
// Fail-closed on every unhandled shape: an empty actor, a type with no inbound
// permission (service:, group:, anything unregistered), and a malformed
// agentsession reference all return ErrActorTypeUnsupported rather than
// falling through to a check that would silently ask the wrong question. A nil
// checker is (false, nil), matching CheckSessionInteract.
func CheckSessionInbound(ctx context.Context, sdb SessionInboundChecker,
	scope SessionRef, actor identity.Subject, fullyConsistent bool) (bool, error) {
	if sdb == nil {
		return false, nil
	}
	switch SubjectType(actor.ObjectType()) {
	case SubjectUser:
		canonical, err := actor.CanonicalUserID()
		if err != nil {
			// Unreachable while ObjectType() reports "user" — CanonicalUserID
			// rejects only a non-user type — but propagated rather than
			// discarded so a future change to either cannot fail open.
			return false, fmt.Errorf("%w: %q: %w", ErrActorTypeUnsupported, actor, err)
		}
		return sdb.CheckInteract(ctx, scope.Namespace, scope.Name, canonical, fullyConsistent)
	case SubjectAgentSession:
		// ParseAgentSessionSubject is the repo's one parser for this shape;
		// see its doc for why all three call sites share it. A malformed id is
		// a DEFINITIVE refusal, not a backend fault: no tuple will ever make
		// an unparseable reference resolvable, so it keeps the sentinel.
		sender, err := ParseAgentSessionSubject(actor.String())
		if err != nil {
			return false, fmt.Errorf("%w: %w", ErrActorTypeUnsupported, err)
		}
		return sdb.CheckConverse(ctx, scope.Namespace, scope.Name, sender.Namespace, sender.Name, fullyConsistent)
	default:
		return false, fmt.Errorf("%w: %q", ErrActorTypeUnsupported, actor)
	}
}

// ManageScopeChecker is the SpiceDB-side dependency CheckSessionManageScope
// wraps. Implemented by *spicedb.Client; abstracted so tests can fake it. The
// metaagent MetaagentReceived stage uses this to gate a scope change on the
// session owner (agentsession#manage_scope = owner).
type ManageScopeChecker interface {
	// CheckManageScope reports whether canonicalID holds
	// agentsession#manage_scope on ns/name — may this user change what the
	// session is allowed to do. Callers MUST pass fullyConsistent=true.
	// Returns (false, err) on failure and the scope change is refused
	// (fail-CLOSED).
	CheckManageScope(ctx context.Context, ns, name string, canonicalID identity.CanonicalUserID, fullyConsistent bool) (bool, error)
}

// CheckSessionManageScope — authzd-side control-plane authz: may <subject>
// change this session's scope? Returns (allowed, err). Wraps
// spicedb.Client.CheckManageScope. Callers MUST bind fullyConsistent=true: a
// freshly-started session's started_by tuple may be a recent write, so a
// MinimizeLatency read could miss it. A nil checker is a fail-closed no-op
// (false, nil) — same contract as CheckSessionInteract.
func CheckSessionManageScope(ctx context.Context, sdb ManageScopeChecker,
	scope SessionRef, canonicalID identity.CanonicalUserID, fullyConsistent bool) (bool, error) {
	if sdb == nil {
		return false, nil
	}
	return sdb.CheckManageScope(ctx, scope.Namespace, scope.Name, canonicalID, fullyConsistent)
}

// ForkChecker is the SpiceDB-side dependency CheckSessionFork wraps.
// Implemented by *spicedb.Client; abstracted so tests can fake it. The operator's
// SessionFork hook uses this to gate a fork on the session owner
// (agentsession#fork = owner — started_by confers interact only, never fork).
type ForkChecker interface {
	// CheckFork reports whether canonicalID holds agentsession#fork on ns/name
	// — may this user branch a child session off it. Ownership confers fork;
	// started_by alone does not. Callers MUST pass fullyConsistent=true.
	// Returns (false, err) on failure and the fork is refused (fail-CLOSED).
	CheckFork(ctx context.Context, ns, name string, canonicalID identity.CanonicalUserID, fullyConsistent bool) (bool, error)
}

// CheckSessionFork — operator-side control-plane authz: may <subject> fork this
// session? Returns (allowed, err). Wraps spicedb.Client.CheckFork. Callers MUST
// bind fullyConsistent=true (the parent's started_by tuple may be a recent
// write). A nil checker is a fail-closed no-op (false, nil) — same contract as
// CheckSessionManageScope.
func CheckSessionFork(ctx context.Context, sdb ForkChecker,
	scope SessionRef, canonicalID identity.CanonicalUserID, fullyConsistent bool) (bool, error) {
	if sdb == nil {
		return false, nil
	}
	return sdb.CheckFork(ctx, scope.Namespace, scope.Name, canonicalID, fullyConsistent)
}

// SessionGrantChecker is the dependency CheckSessionGrant wraps.
// Returned Result.Outcome is OutcomeAllowed when the post-approval
// grant relation exists.
type SessionGrantChecker interface {
	// CheckSessionGrant reports whether the per-session grant written at
	// approval time (agentsession#check_<permName>_<resType>) now admits
	// subject. Returns the verdict as a Result plus a transport error;
	// CheckSessionGrant (the free function) denies on either (fail-CLOSED),
	// carrying the error text as the Result Message.
	CheckSessionGrant(ctx context.Context, ns, name, subject, permName, resType string) (Result, error)
}

// CheckSessionGrant — JIT post-approval grant check. Replaces the
// existing JIT branch when consumers flip to the Engine.
func CheckSessionGrant(ctx context.Context, sdb SessionGrantChecker,
	scope SessionRef, subject, permName, resType string) Result {
	if sdb == nil {
		return Result{Outcome: OutcomeDenied, Message: "no SessionGrantChecker wired"}
	}
	res, err := sdb.CheckSessionGrant(ctx, scope.Namespace, scope.Name, subject, permName, resType)
	if err != nil {
		return Result{Outcome: OutcomeDenied, Message: err.Error()}
	}
	return res
}
