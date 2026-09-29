// Package preferenceaccess is the memory Kind for the audit log of
// subject-named preference reads: every GET /memory/_preferences?user-ref=
// request, resolved or not. It exists because that route is a disclosure
// surface an agent controls the input to (the reference it asks the
// operator to resolve) and the platform decides the output of (which keys,
// if any, a class marked visibility: class) — the tamper-evident record is
// what lets a human answer "who did this agent look up, and what did it
// see" after the fact, the same evidentiary bar the rest of
// pkg/memory/kinds' append-only Kinds hold audit and security-relevant
// events to.
package preferenceaccess

import (
	"context"
	"reflect"
	"time"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/lifecycle"
)

// The canonical Outcome strings. They live here rather than being whatever a
// caller happens to spell — the same reasoning authzdecision's outcome
// constants record: a forensic query filtering on an outcome token must
// match every writer, and a writer inventing "resolver_error" where the
// reader greps "resolver-error" produces an audit trail whose faults are
// unfindable.
const (
	// OutcomeOK: the reference resolved and the class-visible snapshot was
	// returned.
	OutcomeOK = "ok"
	// OutcomeUnresolved: the reference did not resolve (unknown form, no
	// linked platform user, resolution unavailable on this cluster); the
	// request answered 422 and disclosed nothing beyond the schema. Reason
	// says why, in the resolver's own bounded words.
	OutcomeUnresolved = "unresolved"
	// OutcomeUnknownSubject: the reference resolved in FORM only (an email
	// canonicalizes to a subject unconditionally) and the platform has no
	// record of that subject, so the request answered 422 exactly like an
	// unresolved reference. ResolvedSubject IS populated — whom the attempt
	// was about is known and belongs in the record — which is what
	// distinguishes this token from OutcomeUnresolved in a forensic query.
	OutcomeUnknownSubject = "unknown-subject"
	// OutcomeResolverError: subjectresolve.Resolve itself faulted (a SpiceDB
	// read error, a session-annotation read error); the request answered
	// 502 and disclosed nothing. Reason carries the bounded fault text.
	OutcomeResolverError = "resolver-error"
	// OutcomeGlobalsError: the AgentSettings read faulted before resolution
	// was attempted; the request answered 500 and disclosed nothing.
	OutcomeGlobalsError = "globals-error"
	// OutcomeUserScopeError: the resolved subject's user scope could not be
	// derived; the request answered 500 and disclosed nothing.
	OutcomeUserScopeError = "user-scope-error"
	// OutcomeQueryError: the resolved subject's user_preference query
	// faulted; the request answered 5xx and disclosed nothing.
	OutcomeQueryError = "query-error"
)

// Content is one subject-named preference read ATTEMPT — recorded on every
// exit of the ?user-ref= route once the session/class are validated, fault
// paths included, so a failed (and possibly client-retried) attempt is as
// visible in the trail as a successful disclosure.
type Content struct {
	// Ref is the raw reference the caller supplied to ?user-ref= (e.g.
	// "email:alice@example.com", "github_user:12345", "trigger-author").
	Ref string `json:"ref"`
	// ResolvedSubject is the canonical user id the reference resolved to;
	// empty when resolution did not (or could not yet) produce one. It IS
	// populated on the post-resolution fault outcomes (user-scope-error,
	// query-error) AND on unknown-subject: the subject was known by then,
	// and "whom the attempt was about" is the fact the trail exists to
	// carry.
	ResolvedSubject string `json:"resolvedSubject,omitempty"`
	// Outcome is one of the canonical Outcome* tokens above.
	Outcome string `json:"outcome"`
	// Reason carries the human-readable detail: the resolver's Reason on
	// OutcomeUnresolved, the bounded fault text on the error outcomes,
	// empty on OutcomeOK.
	Reason string `json:"reason,omitempty"`
	// Keys lists the class-visible preference keys the read considered —
	// the schema, filtered to visibility: class, BEFORE any user value was
	// read. Self-only keys never appear here, resolved or not. Empty on the
	// error outcomes: a faulted attempt disclosed nothing.
	Keys []string `json:"keys,omitempty"`
	// RequestedAt is deliberately NOT a field here — the request time is
	// carried on Entry.CreatedAt (see Record), so there is exactly one
	// timestamp for this fact rather than two that could disagree.
}

// KindName is the registered name of this memory Kind.
const KindName = "preference_access"

// IDPrefix is the literal byte prefix every Entry of this Kind's ID starts
// with.
const IDPrefix = "prefacc-"

type Kind struct{}

func (Kind) Name() string     { return KindName }
func (Kind) IDPrefix() string { return IDPrefix }

// WriteAuthority: ComponentWritten — only the operator's own preferences GET
// handler authors this Kind, signed as "system:operator" through its own
// signing facade (see httpsrv.WithPreferenceAudit and the operator's
// newMemHandlerOpts). A session credential can never author its own audit
// record — the same reasoning user_preference's WriteAuthority documents.
func (Kind) WriteAuthority() memory.WriteAuthority { return memory.ComponentWritten }

func (Kind) Retention() memory.Retention {
	return memory.Retention{
		ArchiveOn:       []memory.SignalKind{lifecycle.SigSessionCompleted, lifecycle.SigSessionFailed},
		TTLAfterArchive: 90 * 24 * time.Hour,
		AppendOnly:      true, // subject-named preference reads are security audit evidence — write-once.
	}
}
func (Kind) ContentSchema() reflect.Type { return reflect.TypeOf(Content{}) }

// IndexedFields: "outcome" — the field a forensic sweep filters on ("show me
// every failed subject-named read attempt"). JSON tag, not the Go name, per
// the Kind interface's own contract.
func (Kind) IndexedFields() []string                        { return []string{"outcome"} }
func (Kind) NewScopeHooks(_ memory.Scope) memory.ScopeHooks { return hooks{} }

type hooks struct{}

func (hooks) OnSignal(_ context.Context, _ memory.Signal) error { return nil }

func init() { memory.RegisterKind(Kind{}) }
