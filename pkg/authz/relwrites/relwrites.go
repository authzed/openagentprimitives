// Package relwrites evaluates and writes the SpiceDB relationship
// tuples declared by MCPServer.spec.tools[*].writesRelationships
// after a successful tool call. See slice-4 design §2.3.
//
// CEL bindings: a Block's When/ForEach/Tuple expressions see a single
// `vars map[string]any` overlaid onto a nil-filled set of recognized
// keys — "args" (the tool-call argument map), "result" (the tool-call
// response payload), "session" (string "<ns>/<name>" of the calling
// AgentSession, optional), and "call" (reserved for a future
// parsed-argv object; always bound nil today). "item" is bound
// internally per forEach iteration and is not part of vars. Callers
// only need to populate the keys their blocks actually reference —
// every declared variable is nil-filled so CEL's "every declared var
// must be bound" requirement is satisfied regardless.
package relwrites

import (
	"context"
	"errors"
	"fmt"
	"reflect"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/common/types/ref"
	"github.com/google/cel-go/common/types/traits"

	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/x/celbudget"
	"github.com/authzed/openagentprimitives/pkg/x/celconv"
)

// Writer is the SpiceDB write surface. The concrete impl in production
// wraps an authzed client; tests use an in-memory fake.
type Writer interface {
	// WriteRelationships writes every resolved tuple, atomically where the
	// backend allows it. An error must reach the caller: these JIT writes are
	// what later permission Checks read, so a dropped write shows up much later
	// as an unexplained deny. ErrWriteOnceConflict is the one error that means
	// "intentionally rejected", not "failed".
	WriteRelationships(ctx context.Context, tuples []ResolvedTuple) error
}

// SlotBoundChecker reports whether the calling session holds a slot grant on
// resource. It is consulted once per resolved tuple emitted by a block whose
// RequireSlotBound is true — never for an unmarked block.
//
// An error return is a refusal, not a skip: a grant set that could not be
// read is unknown, not empty, so the caller must treat it exactly like a
// false result and refuse the tuple, never silently write it through.
type SlotBoundChecker func(ctx context.Context, resource string) (bool, error)

// ErrSlotBoundRefused marks every error the slot-bound gate produces —
// refused, unreadable, or unwired. A caller uses errors.Is to tell "the gate
// said no" from "the store hiccuped", because the two deserve different
// handling: a store failure is a mechanism problem the agent cannot act on,
// while a gate refusal is an AUTHORIZATION outcome that changes what the agent
// should believe landed. The sandbox dispatcher swallows the first and
// surfaces the second (pkg/agent/tool/sandbox.evaluateWritesRelationships).
//
// All three gate outcomes carry it, not just the false one. An unreadable
// grant set and an unwired checker both mean the same thing to the agent as a
// flat refusal — a marked block did not write — and the whole point of this
// feature is that such an outcome is never invisible.
//
// It is carried by a wrapper that renders nothing of its own (slotBoundRefusal
// below), so adding it changed no error string. The gate's messages are
// user-visible on the MCP path — they become the tool result the agent reads —
// and a sentinel stitched onto the end would be noise the agent has to read
// past to find the resource it was refused.
var ErrSlotBoundRefused = errors.New("slot-bound refusal")

// slotBoundRefusal tags an error as the gate's without altering how it reads.
// Error() forwards verbatim, Unwrap preserves whatever chain the wrapped error
// already had (a checker's own store error stays matchable), and Is answers
// only for the sentinel.
type slotBoundRefusal struct{ err error }

func (r slotBoundRefusal) Error() string        { return r.err.Error() }
func (r slotBoundRefusal) Unwrap() error        { return r.err }
func (r slotBoundRefusal) Is(target error) bool { return target == ErrSlotBoundRefused }

// Run evaluates each block and writes resolved tuples to SpiceDB.
// Errors per block are logged via logFn for observability AND
// returned (joined) so the caller can surface the failure — these
// JIT writes are load-bearing for downstream Checks, and silently
// dropping them would leave permission checks denying with no signal
// the agent or user could act on.
//
// checker gates any block whose RequireSlotBound is true: each resolved
// tuple's Resource is checked before it is written, and the tuple is refused
// (not written) unless checker reports true. A marked block with a nil
// checker refuses EVERY tuple of that block — loudly, naming the block — as
// unwired rather than silently writing as if unmarked; this feature's prior
// history is five silent gaps, and a wiring gap must surface as a refusal.
// An unmarked block (RequireSlotBound false) never consults checker at all,
// so passing nil is safe when no block declares the requirement.
//
// Returns the tuples successfully written — the caller records them in
// the relwrites_audit entry (the tamper-evident answer to "what tuples
// did this session generate?"; the audit previously recorded none) —
// and a nil error when every block evaluates and writes (or is skipped
// by its When clause) without error. Blocks are attempted
// independently — one failure does not abort the rest.
func Run(ctx context.Context, w Writer, blocks []Block, vars map[string]any, checker SlotBoundChecker, logFn func(msg string, kv ...any)) ([]ResolvedTuple, error) {
	var errs []error
	var written []ResolvedTuple
	for i, block := range blocks {
		tuples, err := Evaluate(block, vars)
		if err != nil {
			logFn("relwrites: evaluate failed", "block", i, "err", err.Error())
			errs = append(errs, fmt.Errorf("block %d: evaluate: %w", i, err))
			continue
		}
		if len(tuples) == 0 {
			continue
		}
		if block.RequireSlotBound {
			tuples, err = filterSlotBound(ctx, i, tuples, checker, logFn)
			if err != nil {
				errs = append(errs, err)
			}
			if len(tuples) == 0 {
				continue
			}
		}
		if err := w.WriteRelationships(ctx, tuples); err != nil {
			logFn("relwrites: SpiceDB write failed", "block", i, "tuples", len(tuples), "err", err.Error())
			errs = append(errs, fmt.Errorf("block %d: spicedb write: %w", i, err))
			continue
		}
		written = append(written, tuples...)
	}
	return written, errors.Join(errs...)
}

// filterSlotBound applies checker to every tuple of a RequireSlotBound
// block, returning only the tuples the checker approved, plus a joined error
// describing every refusal (nil when nothing was refused).
//
// A nil checker means the gate was never wired up: rather than treat that as
// "nothing to check" and write through, every tuple of the block is refused
// and the error says so by name — see the Run doc comment for why.
//
// Every error this returns — refused, unreadable, unwired — is tagged
// ErrSlotBoundRefused, so a dispatcher can tell the gate's answer from a store
// failure. The tag renders nothing, so the messages read exactly as before.
func filterSlotBound(ctx context.Context, blockIdx int, tuples []ResolvedTuple, checker SlotBoundChecker, logFn func(msg string, kv ...any)) ([]ResolvedTuple, error) {
	if checker == nil {
		logFn("relwrites: slot-bound check required but the checker is unwired", "block", blockIdx, "tuples", len(tuples))
		return nil, slotBoundRefusal{fmt.Errorf("block %d: requires a slot-bound check but the checker is unwired", blockIdx)}
	}
	var errs []error
	allowed := make([]ResolvedTuple, 0, len(tuples))
	for _, t := range tuples {
		ok, err := checker(ctx, t.Resource)
		if err != nil {
			logFn("relwrites: slot-bound check errored", "block", blockIdx, "resource", t.Resource, "err", err.Error())
			errs = append(errs, slotBoundRefusal{fmt.Errorf("block %d: slot-bound check for resource %q: %w", blockIdx, t.Resource, err)})
			continue
		}
		if !ok {
			logFn("relwrites: slot-bound check refused", "block", blockIdx, "resource", t.Resource)
			errs = append(errs, slotBoundRefusal{fmt.Errorf("block %d: resource %q has no slot-bound grant for this session", blockIdx, t.Resource)})
			continue
		}
		allowed = append(allowed, t)
	}
	return allowed, errors.Join(errs...)
}

// Block mirrors MCPServerRelationshipWrite. Decoupled from the CRD
// type so test fixtures don't need the full v1alpha1 import.
type Block struct {
	When    string
	ForEach string
	Tuple   Tuple
	// Exclusive makes every tuple this block emits atomically write-once
	// per subject: the write FAILS (nothing written) if the subject
	// already holds `relation` on ANY resource of the tuple's resource
	// type. Implemented by the writer as a SpiceDB MUST_NOT_MATCH
	// precondition, so it is atomic under concurrent writers. Default
	// false preserves the plain TOUCH-upsert behavior. See
	// SpiceDBWriter.WriteRelationships.
	Exclusive bool
	// RequireSlotBound gates every tuple this block emits on a
	// SlotBoundChecker call against the tuple's RESOURCE: the session must
	// hold a slot grant on that resource or the tuple is refused rather than
	// written. Default false is byte-identical to today's behaviour — the
	// checker passed to Run is never consulted for such a block. See Run's
	// doc comment for the nil-checker and false/error refusal semantics.
	RequireSlotBound bool
}

// Tuple is the unresolved CEL-expression form of a SpiceDB tuple.
type Tuple struct {
	Resource string
	Relation string
	Subject  string
}

// ResolvedTuple is one tuple ready to write.
type ResolvedTuple struct {
	Resource string
	Relation string
	Subject  string
	// Exclusive, when true, makes the writer attach a MUST_NOT_MATCH
	// precondition so this tuple is written only if the subject does not
	// already hold Relation on any resource of Resource's type. Carried
	// from the emitting Block; see Block.Exclusive.
	Exclusive bool
}

// celNative converts a CEL result into plain, JSON-shaped Go values: []any for
// a list, map[string]any for a map, Go nil for null, and cel-go's underlying Go
// value for every scalar — all the way down.
//
// "JSON-shaped" is a claim about the CONTAINERS, and about the scalars CEL
// produces from the JSON-decoded args and results these expressions actually
// bind: bool, int64, uint64, float64, string. It is not a claim that every CEL
// scalar survives a JSON round trip as its own type. Bytes, timestamp and
// duration pass through as []byte, time.Time and time.Duration, which encode as
// base64, RFC 3339 and integer nanoseconds and decode back as a string, a
// string and a number. Nothing reachable produces them — a fact expression sees
// only JSON-decoded vars — so this is recorded rather than converted: a
// conversion for types nothing generates would be untested by construction, and
// what the right encoding for each would be is a decision to make when
// something actually needs one.
//
// It recurses because ConvertToNative does NOT. ConvertToNative converts the
// value it is handed and leaves that value's CHILDREN in cel-go's internal
// form: a map literal converts to a map[string]any whose values are still
// []ref.Val / map[ref.Val]ref.Val, and a list macro converts to a []any whose
// elements are still map[ref.Val]ref.Val. Those children marshal to JSON as
// {"Adapter":{}} — every key and value silently gone, with no error to notice.
// A one-level conversion therefore looks right and still destroys a list of
// maps, which is exactly the fidelity bug this exists to close.
//
// The recursion goes back through the type adapter rather than switching on
// []ref.Val / map[ref.Val]ref.Val directly, for celconv.List's reason:
// asserting on one of cel-go's internal representations is what silently broke
// forEach for everything but a bare field reference. NativeToValue re-wraps
// whatever ConvertToNative handed back — internal or already-native — so each
// level dispatches on cel-go's own traits, never on a concrete internal type,
// and a future representation is handled the same way.
//
// This is the value that becomes an observed_fact / envelope_fact
// (pkg/authz/observe.Evaluate -> factcontent.Record), which a later gate reads
// to make a binding decision. A fact that stored empty would make that gate
// read something the tool never reported, so every conversion failure here is
// returned rather than degraded into a value.
func celNative(v ref.Val) (any, error) {
	if v == nil {
		return nil, fmt.Errorf("nil CEL value")
	}
	if types.IsError(v) || types.IsUnknown(v) {
		// Reachable through the recursion below, not from a successful
		// top-level Eval: a child the type adapter cannot represent comes back
		// as an error value whose Value() is a Go error, and json.Marshal
		// renders that as {} — the same silent emptiness as the composites
		// above, so it fails closed here instead.
		return nil, fmt.Errorf("CEL produced a %s rather than a value: %v", v.Type().TypeName(), v.Value())
	}
	if v.Type() == types.NullType {
		// CEL null's Value() is a structpb.NullValue — an int32-backed protobuf
		// enum that JSON-marshals to 0, so a null-valued fact would be stored
		// and later read as the NUMBER ZERO rather than as "no value".
		return nil, nil
	}

	switch t := v.(type) {
	case traits.Lister:
		items, err := celconv.List(v)
		if err != nil {
			return nil, err
		}
		out := make([]any, len(items))
		for i, raw := range items {
			elem, err := celNative(types.DefaultTypeAdapter.NativeToValue(raw))
			if err != nil {
				return nil, fmt.Errorf("list element %d: %w", i, err)
			}
			out[i] = elem
		}
		return out, nil

	case traits.Mapper:
		if err := requireStringKeys(t); err != nil {
			return nil, err
		}
		native, err := t.ConvertToNative(reflect.TypeOf(map[string]any{}))
		if err != nil {
			return nil, fmt.Errorf("converting map: %w", err)
		}
		m, ok := native.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("converting map: got %T", native)
		}
		out := make(map[string]any, len(m))
		for k, raw := range m {
			val, err := celNative(types.DefaultTypeAdapter.NativeToValue(raw))
			if err != nil {
				return nil, fmt.Errorf("map key %q: %w", k, err)
			}
			out[k] = val
		}
		return out, nil

	default:
		return v.Value(), nil
	}
}

// requireStringKeys fails closed on a CEL map whose keys are not all strings.
//
// CEL allows int, uint and bool keys; JSON objects have string keys only. The
// check is made HERE rather than left to ConvertToNative — which does reject
// every non-string key type today — because that rejection is an
// implementation detail, not a contract: were a future cel-go to coerce the
// key 1 into "1", the recorded fact would silently gain a key its author never
// wrote, and nothing would report it. Failing closed on our own terms is what
// keeps that impossible.
//
// Erroring is the right answer rather than stringifying, because the value is
// about to become an append-only fact: a fact nobody can decode must not be
// recorded as though it were fine.
func requireStringKeys(m traits.Mapper) error {
	for it := m.Iterator(); it.HasNext() == types.True; {
		k := it.Next()
		if _, ok := k.Value().(string); !ok {
			return fmt.Errorf("map key %v has CEL type %s, but a value must be JSON-encodable and JSON object keys are strings",
				k.Value(), k.Type().TypeName())
		}
	}
	return nil
}

// ValidateBlock compile-checks every CEL expression in a Block WITHOUT
// evaluating it, so a malformed expression is caught when the MCPServer is
// applied rather than during a live tool call.
//
// It deliberately compiles through this package's own celEnv — the same one
// Evaluate uses. Validating against any other environment would let the two
// drift, which is exactly the failure this closes: `spicedb_user_id` was
// registered in the shared pkg/authz env but missing from this package's
// forked one, so a spec that looked valid could not compile at execution time.
// Sharing the env makes "validated" and "will run" the same claim.
//
// Errors name the offending field (when / forEach / tuple.resource /
// tuple.relation / tuple.subject) so an author can fix the spec without
// reading source.
func ValidateBlock(block Block) error {
	if block.When != "" {
		if _, err := compileBool(block.When); err != nil {
			return fmt.Errorf("when: %w", err)
		}
	}
	if block.ForEach != "" {
		if _, err := compileDyn(block.ForEach); err != nil {
			return fmt.Errorf("forEach: %w", err)
		}
	}
	for _, f := range []struct {
		name string
		expr string
	}{
		{"tuple.resource", block.Tuple.Resource},
		{"tuple.relation", block.Tuple.Relation},
		{"tuple.subject", block.Tuple.Subject},
	} {
		if _, err := compileString(f.expr); err != nil {
			return fmt.Errorf("%s: %w", f.name, err)
		}
	}
	return nil
}

// ResolveItems evaluates the When gate and the ForEach fan-out shared by
// every block-shaped CEL evaluation over these bindings: relwrites.Evaluate
// (tuples) and observe.Evaluate (pkg/authz/observe, facts) both drive their
// per-item loop from this one resolution so the two cannot diverge on what
// "the gate" or "the fan-out" means. RULING R2 assigns this half of the
// relwrites/observe shared CEL surface to the observe evaluator's task; the
// compile half (CompileBoolExpr etc., below) already belongs to both.
//
// The (items, error) contract callers rely on:
//   - When evaluates false: (nil, nil). This is the ONLY path that returns a
//     nil slice with a nil error — a caller uses that, and only that, to
//     mean "the block does not fire, skip it entirely, don't even compile
//     the rest." A ForEach that matches zero items is a different thing (see
//     below) and must not be confused with this.
//   - ForEach is empty: ([]any{nil}, nil) — run once, with a nil item. Both
//     callers' no-ForEach case (a single tuple / a single observation) relies
//     on this exact shape.
//   - ForEach is set: the resolved item list, always non-nil (even when it
//     matches zero items) — celconv.List's underlying conversion never yields a
//     literal Go nil for a valid empty list, and the explicit guard below
//     makes that a guarantee rather than an implementation detail a future
//     cel-go upgrade could silently invalidate out from under the When-false
//     sentinel above.
func ResolveItems(when, forEach string, vars map[string]any) ([]any, error) {
	baseBindings := nilFilledBindings(vars)

	if when != "" {
		whenPrg, err := compileBool(when)
		if err != nil {
			return nil, fmt.Errorf("when compile: %w", err)
		}
		ok, err := evalBool(whenPrg, baseBindings)
		if err != nil {
			return nil, fmt.Errorf("when eval: %w", err)
		}
		if !ok {
			return nil, nil
		}
	}

	if forEach == "" {
		return []any{nil}, nil
	}

	fePrg, err := compileDyn(forEach)
	if err != nil {
		return nil, fmt.Errorf("forEach compile: %w", err)
	}
	v, _, err := fePrg.Eval(baseBindings)
	if err != nil {
		return nil, fmt.Errorf("forEach eval: %w", err)
	}
	list, err := celconv.List(v)
	if err != nil {
		return nil, fmt.Errorf("forEach: %w", err)
	}
	if list == nil {
		// See the doc comment above: nil is reserved for "When was false."
		list = []any{}
	}
	return list, nil
}

// EvalStringWithItem evaluates a compiled string-typed CEL program (from
// compileString / CompileStringExpr) against vars overlaid with a
// per-iteration `item` binding, enforcing the non-empty-string result every
// string-valued expression in this shared surface relies on — a resource,
// relation or subject that resolved to "" is exactly the malformed-tuple
// case relwrites.Evaluate used to catch via evalStringWith directly, and a
// subject that resolved to "" is the exact "fact filed under an object
// nobody can name" case pkg/authz/observe must fail closed on. One check,
// enforced once, for both.
func EvalStringWithItem(prg cel.Program, vars map[string]any, item any) (string, error) {
	bindings := nilFilledBindings(vars)
	bindings["item"] = item
	return evalStringWith(prg, bindings)
}

// EvalAnyWithItem evaluates a compiled dyn-typed CEL program (from
// compileDyn / CompileAnyExpr) against vars overlaid with a per-iteration
// `item` binding and returns a plain, JSON-shaped Go value with no type or
// emptiness constraint. Unlike EvalStringWithItem's tuple components, an
// observe fact value may legitimately be false, 0, "" or null — only the
// subject identifying WHERE a fact is filed must be non-empty, never the
// fact's own value.
//
// The result goes through celNative rather than out as a bare v.Value(),
// because v.Value() leaks cel-go's internal representation for anything that
// is not a scalar: see celNative's comment for what that costs.
func EvalAnyWithItem(prg cel.Program, vars map[string]any, item any) (any, error) {
	bindings := nilFilledBindings(vars)
	bindings["item"] = item
	v, _, err := prg.Eval(bindings)
	if err != nil {
		return nil, err
	}
	return celNative(v)
}

// Evaluate produces the list of tuples a single Block emits. Returns
// an empty slice when When evaluates false. Errors surface as a
// single error per malformed tuple in the forEach iteration; the
// caller (Writer.Write) decides how to log them.
//
// vars supplies the CEL bindings for this evaluation — see the package
// doc comment for the recognized keys. Absent keys are nil-filled
// before eval, so a caller need only set the keys its blocks use.
func Evaluate(block Block, vars map[string]any) ([]ResolvedTuple, error) {
	items, err := ResolveItems(block.When, block.ForEach, vars)
	if err != nil {
		return nil, err
	}
	if items == nil {
		return nil, nil
	}

	resPrg, err := compileString(block.Tuple.Resource)
	if err != nil {
		return nil, fmt.Errorf("tuple.resource compile: %w", err)
	}
	relPrg, err := compileString(block.Tuple.Relation)
	if err != nil {
		return nil, fmt.Errorf("tuple.relation compile: %w", err)
	}
	subPrg, err := compileString(block.Tuple.Subject)
	if err != nil {
		return nil, fmt.Errorf("tuple.subject compile: %w", err)
	}

	emit := func(item any) (ResolvedTuple, error) {
		res, err := EvalStringWithItem(resPrg, vars, item)
		if err != nil {
			return ResolvedTuple{}, fmt.Errorf("resource: %w", err)
		}
		rel, err := EvalStringWithItem(relPrg, vars, item)
		if err != nil {
			return ResolvedTuple{}, fmt.Errorf("relation: %w", err)
		}
		sub, err := EvalStringWithItem(subPrg, vars, item)
		if err != nil {
			return ResolvedTuple{}, fmt.Errorf("subject: %w", err)
		}
		t := ResolvedTuple{Resource: res, Relation: rel, Subject: sub, Exclusive: block.Exclusive}
		// Shape AND type. Every part of this tuple was produced by CEL over
		// model-authored args and the upstream server's response, so refusing
		// the platform's own scaffold types here is what stops a tool writing
		// itself platform#admin or agentsession#owner.
		if err := ValidateResolvedTuple(t); err != nil {
			return ResolvedTuple{}, err
		}
		// A sibling check, not folded into ValidateResolvedTuple: this one
		// needs block.RequireSlotBound, which ValidateResolvedTuple's
		// tuple-only signature does not see. See ValidateSlotBoundSubject's
		// doc comment for the gap this closes.
		if err := ValidateSlotBoundSubject(t, block.RequireSlotBound); err != nil {
			return ResolvedTuple{}, err
		}
		return t, nil
	}

	out := make([]ResolvedTuple, 0, len(items))
	for _, item := range items {
		t, err := emit(item)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, nil
}

// nilFilledBindings returns the full CEL activation map for one Evaluate
// call: every variable celEnv declares (except "item", which is bound
// per-emit) defaults to nil, then vars is overlaid on top. CEL requires
// every declared variable to be bound at eval time, so callers that
// don't care about "session" or "call" don't need to know they exist.
func nilFilledBindings(vars map[string]any) map[string]any {
	bindings := map[string]any{"args": nil, "result": nil, "session": nil, "call": nil, "item": nil}
	for k, v := range vars {
		bindings[k] = v
	}
	return bindings
}

func evalBool(prg cel.Program, bindings map[string]any) (bool, error) {
	v, _, err := prg.Eval(bindings)
	if err != nil {
		return false, err
	}
	b, ok := v.Value().(bool)
	if !ok {
		return false, fmt.Errorf("expected bool, got %T", v.Value())
	}
	return b, nil
}

func evalStringWith(prg cel.Program, bindings map[string]any) (string, error) {
	v, _, err := prg.Eval(bindings)
	if err != nil {
		return "", err
	}
	s, ok := v.Value().(string)
	if !ok {
		return "", fmt.Errorf("expected string, got %T", v.Value())
	}
	if s == "" {
		return "", fmt.Errorf("empty string")
	}
	return s, nil
}

// celEnv builds the CEL env for this package's expressions: args,
// result, item, session, and call, all cel.DynType. Package-local
// (rather than the shared authz.CompileBool/CompileString helpers)
// because those declare only args/result/item — widening them would
// leak relwrites-specific bindings into every other CEL slot in the
// package.
func celEnv() (*cel.Env, error) {
	return cel.NewEnv(
		cel.Variable("args", cel.DynType),
		cel.Variable("result", cel.DynType),
		cel.Variable("item", cel.DynType),
		cel.Variable("session", cel.DynType),
		cel.Variable("call", cel.DynType),
		// Forking the env to add session/call must NOT drop the shared
		// function library. It did: every relationship-write expression
		// deriving a subject from an email — the documented way to write one —
		// failed at runtime with "undeclared reference to 'spicedb_user_id'",
		// aborting the tool call that depended on the relationship.
		authz.SpiceDBUserIDFunction(),
	)
}

// compileBool compiles a CEL expression expected to yield a bool, using
// this package's celEnv (args/result/item/session/call).
func compileBool(expr string) (cel.Program, error) {
	env, err := celEnv()
	if err != nil {
		return nil, fmt.Errorf("CEL env: %w", err)
	}
	ast, iss := env.Compile(expr)
	if iss != nil && iss.Err() != nil {
		return nil, fmt.Errorf("CEL compile %q: %w", expr, iss.Err())
	}
	out := ast.OutputType()
	if out != cel.BoolType && out.TypeName() != "dyn" {
		return nil, fmt.Errorf("CEL: expression %q must return bool, got %s", expr, out)
	}
	prg, err := env.Program(ast, celbudget.ProgramOptions()...)
	if err != nil {
		return nil, fmt.Errorf("CEL program: %w", err)
	}
	return prg, nil
}

// compileString compiles a CEL expression expected to yield a string,
// using this package's celEnv (args/result/item/session/call).
func compileString(expr string) (cel.Program, error) {
	env, err := celEnv()
	if err != nil {
		return nil, fmt.Errorf("CEL env: %w", err)
	}
	ast, iss := env.Compile(expr)
	if iss != nil && iss.Err() != nil {
		return nil, fmt.Errorf("CEL compile %q: %w", expr, iss.Err())
	}
	prg, err := env.Program(ast, celbudget.ProgramOptions()...)
	if err != nil {
		return nil, fmt.Errorf("CEL program: %w", err)
	}
	return prg, nil
}

func compileDyn(expr string) (cel.Program, error) {
	env, err := celEnv()
	if err != nil {
		return nil, err
	}
	ast, iss := env.Compile(expr)
	if iss != nil && iss.Err() != nil {
		return nil, iss.Err()
	}
	return env.Program(ast, celbudget.ProgramOptions()...)
}

// CompileBoolExpr, CompileStringExpr and CompileAnyExpr are the exported
// forms of compileBool / compileString / compileDyn, for sibling packages
// (pkg/authz/observe today) that share this package's CEL surface —
// args/result/item/session/call plus spicedb_user_id — rather than forking
// their own cel.Env.
//
// A forked env is exactly how this package once lost spicedb_user_id: it
// declared its own args/result/item/session/call variables without also
// carrying the shared function library, so every relationship-write
// expression deriving a subject from an email failed to compile at
// dispatch time despite having validated at apply time. See
// authz.SpiceDBUserIDFunction's doc comment. Exporting these three keeps a
// second package's "validated" and "will run" the same claim, the same way
// ValidateBlock already does for this package's own callers — no fork, no
// second env to drift.
//
// The eval half of this shared surface — ResolveItems (the When-gate +
// ForEach fan-out) and EvalStringWithItem / EvalAnyWithItem (per-item
// evaluation of a precompiled program) — is exported further down in this
// file, for observe.Evaluate (pkg/authz/observe/evaluate.go). relwrites.Evaluate
// calls those same three functions rather than a private duplicate, so the
// two packages' resolution of When/ForEach/item cannot drift. What stays
// unshared is the per-block model each package folds the resolved items
// into: relwrites builds a ResolvedTuple (Tuple, Resource/Relation/Subject),
// observe builds a factcontent.Observation (co-derived Subjects + Facts) —
// that assembly is each package's own, deliberately.

// CompileBoolExpr compiles a CEL expression expected to yield a bool.
func CompileBoolExpr(expr string) (cel.Program, error) {
	return compileBool(expr)
}

// CompileStringExpr compiles a CEL expression expected to yield a string.
func CompileStringExpr(expr string) (cel.Program, error) {
	return compileString(expr)
}

// CompileAnyExpr compiles a CEL expression of unconstrained output type
// (e.g. a forEach list expression, or a fact value expression whose type
// depends on the fact). Errors are wrapped with the same "CEL compile %q"
// framing CompileBoolExpr / CompileStringExpr use, unlike the unexported
// compileDyn this wraps, which relwrites' own ValidateBlock/Evaluate wrap
// themselves with field-specific context.
func CompileAnyExpr(expr string) (cel.Program, error) {
	prg, err := compileDyn(expr)
	if err != nil {
		return nil, fmt.Errorf("CEL compile %q: %w", expr, err)
	}
	return prg, nil
}
