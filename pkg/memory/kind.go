package memory

import (
	"context"
	"fmt"
	"reflect"
	"time"
)

// Kind declares one variety of entry that the framework can store. Kinds
// register at init() time via RegisterKind. Implementations typically
// live under pkg/memory/kinds/<name>/.
type Kind interface {
	// Name is the entry's Kind field and the registry's primary key
	// (e.g., "authz_decision"). Must be non-empty and unique across
	// the registry.
	Name() string
	// IDPrefix is the literal byte prefix every Entry of this Kind's
	// ID must start with (e.g., "authzd-"). Required, non-empty, and
	// must not be a prefix-of nor be prefixed-by any other registered
	// Kind's prefix.
	IDPrefix() string
	// Retention advertises this Kind's preferences to the backend.
	Retention() Retention
	// ContentSchema optionally declares the Kind's Content struct so backends
	// advertising Capabilities.ContentSchemas can index declared fields.
	// nil means opaque content.
	ContentSchema() reflect.Type
	// IndexedFields names the fields the Kind wants indexed, as the keys they
	// are STORED under — the Content struct's `json` tags, NOT the Go field
	// names. FieldFilter.Path draws from the same vocabulary and accessor
	// authors read this list before writing a query, so a Go name here is
	// misinformation that reappears as a predicate matching nothing. Only
	// meaningful when ContentSchema is non-nil.
	IndexedFields() []string
	// WriteAuthority declares WHICH CREDENTIAL CLASS may author entries of this
	// Kind. An interface method rather than a table in a consumer, so a new Kind
	// cannot be registered without answering the question.
	WriteAuthority() WriteAuthority
	// NewScopeHooks materializes the per-(Scope, Kind) hook signals are
	// dispatched to, lazily on the first SendSignal touching the scope. Keep it
	// cheap — long-running work belongs in OnSignal.
	NewScopeHooks(scope Scope) ScopeHooks
}

// WriteAuthority answers "may an agent session author entries of this Kind?".
//
// The memory HTTP plane authenticates three credential classes: the per-session
// bearer a runner pod holds, the system component tokens (channelsd/authzd/webd),
// and — not a credential at all — the operator's own in-process calls. Without
// this classification a valid per-session token could Put ANY registered Kind
// into its scope, so a compromised runner could author component-owned state:
// `cold_start_task` (which authzd reads as "this session's scope review is
// decided" and the runner BLOCKS on), every append-only audit kind, channelsd's
// `parked_prompt`, authzd's extraction records.
//
// It lives on the Kind because that is the only place a new variant is
// guaranteed to be looked at. A denylist in the HTTP handler, or an `if kind ==
// "cold_start_task"` in a consumer, protects exactly the kinds somebody
// remembered that day and silently admits every kind added after.
type WriteAuthority uint8

const (
	// ComponentWritten: only a platform component may author this Kind — the
	// operator in-process, or a system component bearer (channelsd, authzd). A
	// per-session credential is refused.
	//
	// The ZERO VALUE, deliberately: the failure directions are asymmetric. A
	// Kind wrongly left ComponentWritten refuses a legitimate write LOUDLY on
	// its first attempt, naming the Kind and the rule; a Kind wrongly left
	// session-writable is silent, indefinite, and re-opens exactly the hole this
	// type closes. Fail toward the noisy one.
	ComponentWritten WriteAuthority = iota

	// SessionWritten: the agent session itself authors this Kind, through the
	// per-session memory bearer its runner pod holds (and, on the same
	// credential, `oap memory put`) — transcript turns, artifacts, labels, the
	// per-turn audit the runner produces about its own tool calls.
	//
	// Choosing it is a claim about the THREAT model, not convenience: it says
	// the session's agent loop — driven by model output and by whatever content
	// its tools pulled in — is an acceptable author of this record. If a
	// component reads the Kind to make an authorization decision, it is not.
	SessionWritten
)

// String renders a WriteAuthority for logs and test failures. An out-of-range
// value renders as such rather than panicking: enforcement already treats
// anything that is not exactly SessionWritten as component-only, so an unknown
// value is a fail-closed outcome that should be readable, not fatal.
func (w WriteAuthority) String() string {
	switch w {
	case ComponentWritten:
		return "component-written"
	case SessionWritten:
		return "session-written"
	default:
		return fmt.Sprintf("WriteAuthority(%d)", uint8(w))
	}
}

// SessionReadabilityDecider is the READ half of the per-kind door, and it is
// optional: a Kind that does not implement it is session-readable, which is
// what every Kind was before this existed. Only a Kind holding data the agent
// must never see implements it.
//
// Optional rather than a required Kind method because the two directions are
// not symmetric in cost or in risk. Every Kind must answer WriteAuthority —
// getting it wrong grants a session the pen for records a component trusts.
// Almost no Kind needs to be unreadable, and making all thirty-odd declare
// "yes, readable" would be thirty lines of noise around one real answer.
//
// The risk that comes with optional — a future sensitive Kind forgetting to
// implement it — is covered by a reviewed pin over the registry, the same
// device that guards the session-writable set.
type SessionReadabilityDecider interface {
	// SessionReadable reports whether a SESSION credential may read entries of
	// this Kind. False means platform components only.
	SessionReadable() bool
}

// SessionMayRead reports whether a session credential may read this Kind.
//
// Fail-closed on the two cases that are not a plain "yes": an unregistered
// Kind is unreadable, because "declared nothing" must not read as "anyone may
// read it" — the same reasoning authorizeKindWrite applies to writes.
func SessionMayRead(kind string) bool {
	k, registered := LookupKind(kind)
	if !registered {
		return false
	}
	if d, ok := k.(SessionReadabilityDecider); ok {
		return d.SessionReadable()
	}
	return true
}

// ScopeHooks reacts to signals dispatched into the (Scope, Kind) pair.
// Implementations switch on Signal.Kind for the signals they care about
// and ignore the rest. Errors are collected (not aborted-on) by the
// framework's dispatch loop.
type ScopeHooks interface {
	OnSignal(ctx context.Context, sig Signal) error
}

// Framework-level signal kinds: fired by the facade itself on every
// registered Kind's ScopeHooks, rather than by one Kind's own package.
const (
	// SignalEntryAppended fires after a successful append-only Put. It carries
	// the entry's Scope so a hook can fold that scope's log; it deliberately
	// carries no entry payload, so a hook reads the durable record rather than
	// trusting what the writer said it wrote.
	SignalEntryAppended SignalKind = "memory/entry.appended"
)

// Retention is a Kind's advice to the backend about lifecycle and
// immutability of entries. Fields are hints — backends honor what their
// policy supports.
type Retention struct {
	// EssentialWhileLive: never evict while ScopeStatus == Live.
	EssentialWhileLive bool
	// ArchiveOn names signal kinds that flip this Kind's entries
	// from "kept" to "eligible for eviction".
	ArchiveOn []SignalKind
	// TTLAfterArchive: how long after an ArchiveOn signal the
	// backend may drop the entry. Zero = backend default; negative
	// = never auto-drop after archive.
	TTLAfterArchive time.Duration
	// SoftCapPerScope is a per-scope FIFO cap for cache-shaped
	// Kinds (label). Zero = no cap.
	SoftCapPerScope int
	// AppendOnly: entries of this Kind are immutable once written. Put
	// may create (or idempotently re-put byte-identical content) but
	// never change an existing entry, and per-entry Delete is refused.
	// Scope-level deletion (retention, session GC) is unaffected — that
	// is the backend's retention policy, not entry mutation.
	AppendOnly bool

	// NeverForkCopy: a fork must NOT copy this Kind's entries into the child
	// scope. The forking code derives whatever the child needs instead.
	//
	// This exists for Kinds whose entries are a CHAIN rather than a set —
	// where meaning comes from order and from the (scope, publisher) provenance
	// sequence, not from each entry alone. Copying those into a new scope
	// produces a chain whose seq and prevHash refer to a history the child does
	// not have, so it verifies as gapped at best and replays in the wrong order
	// at worst.
	//
	// The plan gate is the motivating case: its rules ("a denial survives a
	// later supersede", "the last write wins for this ceiling") are all
	// order-dependent, so a mis-ordered replay in a child could resurrect
	// authority a human revoked.
	NeverForkCopy bool
}
