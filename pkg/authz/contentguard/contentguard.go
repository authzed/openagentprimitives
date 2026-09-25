// Package contentguard is the pluggable content-inspection seam over the
// tool-call hook pipeline. An Inspector is a registry-keyed factory (referenced
// from settings by ID); its Configure parses+validates the per-instance config
// once and returns an Instance that inspects tool args (input) and/or results
// (output) and returns a Finding (Pass | Block | Approve). The framework adapter
// (hook.go) maps Findings onto pipeline Decisions; plugin authors implement only
// Configure + Inspect.
package contentguard

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"
)

// InspectionPoints is the closed set of pipeline Points an Instance may
// declare, in execution order.
//
// It exists so that a consumer dispatching per-Point enumerates the CONTRACT
// rather than the one point it happened to think about. Filtering for a single
// named point is what left every args-side inspector inert on the runner's
// ungated meta path while the identical rule denied a gated tool's args — the
// admin saw a configured guard and got nothing on the surface that egresses.
// Both consumers (the pipeline adapter here, the runner's meta path) cover
// every entry, and each has a test that walks this slice, so a Point added here
// fails CI rather than being silently skipped at runtime.
//
// Returns a fresh slice: callers range over it and must not be able to mutate
// the contract.
func InspectionPoints() []pipeline.Point {
	return []pipeline.Point{pipeline.PreToolCall, pipeline.PostToolCall}
}

// SubjectFor builds the Subject an Instance sees at point at for one tool call.
// It is the single mapping from a Point onto the content inspected there, so no
// caller can cover one point and quietly hand another an empty Subject (an
// inspector handed empty content Passes — a fail-OPEN).
//
// A Point outside InspectionPoints is an error, never an empty Subject; every
// caller fails closed on it.
func SubjectFor(at pipeline.Point, toolName string, args json.RawMessage, result string, isError bool) (Subject, error) {
	s := Subject{Point: at, ToolName: toolName, IsError: isError}
	switch at {
	case pipeline.PreToolCall:
		s.Args = args
	case pipeline.PostToolCall:
		s.Result = result
	default:
		return Subject{}, fmt.Errorf("contentguard: %q is not an inspection point", at)
	}
	return s, nil
}

// DetectorSpec describes a co-located detector sidecar that an inspector needs
// injected into the runner pod. The operator pins Image by digest and injects a
// no-egress sidecar listening on Port; the runner reaches it at 127.0.0.1:Port.
type DetectorSpec struct {
	Image      string
	Port       int32
	HealthPath string // default "/healthz"
}

// DetectorProvider is OPTIONALLY implemented by an Inspector that requires a
// co-located detector sidecar. The operator iterates inspectors generically and
// type-asserts this — no consumer branches on a specific inspector id.
type DetectorProvider interface {
	// Detector parses raw config and returns the sidecar spec the operator must
	// stand up before this inspector can run, or nil when this configuration
	// needs none. An error makes the settings object Invalid — the inspector
	// never runs rather than running without its detector (fail-closed).
	Detector(raw json.RawMessage) (*DetectorSpec, error)
}

// Action is what an inspector decides for one Inspect call.
type Action int

const (
	Pass    Action = iota // allow, no effect
	Block                 // deny the call/result wholesale
	Approve               // raise a human approval to release it
)

// Inspector is the registry-keyed factory. Self-registers via init() + blank
// import (kindregistry), like pinning kinds / channel kinds.
type Inspector interface {
	// ID is the settings reference key, e.g. "url-allowlist". Must be unique.
	ID() string
	// Configure parses+validates raw config ONCE (compile regex/CEL here) and
	// returns a ready Instance. A non-nil error makes the settings object
	// Invalid (admission-denied) — a misconfigured guard must never run.
	Configure(raw json.RawMessage) (Instance, error)
}

// Instance is a configured inspector ready to evaluate content.
type Instance interface {
	// Points declares where this instance runs: pipeline.PreToolCall (args)
	// and/or pipeline.PostToolCall (result).
	Points() []pipeline.Point
	// Inspect evaluates one Subject and returns its Finding. An error is
	// treated by the adapter as Block (fail-closed).
	Inspect(ctx context.Context, s Subject) (Finding, error)
}

// WholeContentInspector is OPTIONALLY implemented by an Instance that CANNOT be
// outrun by a large payload, and which therefore must not be handed content
// capped at MaxInspectBytes. Capped type-asserts it generically — no consumer
// branches on a specific inspector id, same shape as DetectorProvider.
//
// The cap is chosen against the prompt-injection DETECTOR (cap.go): a ~512-token
// local classifier reached over HTTP under a short timeout whose default
// onError=warn turns a stall into a Pass, so for THAT inspector an uncapped
// payload chooses whether it is scanned at all. An inspector with none of those
// properties gains nothing from the cap and loses exactly the coverage the
// attacker picks: capping url-allowlist — a deterministic RE2 scan — turns its
// deny-default into a Pass for any payload past the cap, on the meta surfaces
// that egress.
//
// The DEFAULT — not implementing this interface — keeps the cap, and that
// direction is deliberate. Over-capping a deterministic inspector costs coverage
// past 32 KiB; under-capping a fail-open one converts a Block into a Pass on an
// attacker-chosen input. An inspector nobody has classified is also far likelier
// to be network- or model-backed than a pure local scan, so "unknown ⇒ needs the
// cap" matches the population as well as the risk.
type WholeContentInspector interface {
	// InspectsWholeContent reports whether this instance's verdict is
	// independent of how large the content is. Return true ONLY when Inspect
	// runs to completion on any input — no network hop, no timeout, no
	// fail-open error mode, and runtime linear in the input — because a true
	// here removes the only bound on what this inspector is handed.
	InspectsWholeContent() bool
}

// inspectsWholeContent reports whether inst has declared itself unable to be
// outrun. Anything that does not answer the question takes the capped default.
func inspectsWholeContent(inst Instance) bool {
	w, ok := inst.(WholeContentInspector)
	return ok && w.InspectsWholeContent()
}

// Subject is the read-only content an Instance sees at one point.
type Subject struct {
	Point    pipeline.Point
	ToolName string
	// Args is the model-authored tool arguments, whole (set at PreToolCall).
	// It is NOT bounded: it is a json.RawMessage, and slicing it would hand a
	// structured inspector invalid JSON. Scan Text() instead — an inspector
	// reading string(Args) opts itself out of the cap that keeps a padded
	// payload from outrunning a detector (see cap.go).
	Args    json.RawMessage
	Result  string // set at PostToolCall
	IsError bool

	// wholeContent lifts the MaxInspectBytes cap off Text for this one Inspect
	// call. Only Capped sets it, and only for an Instance that declares
	// WholeContentInspector — it is unexported so no caller outside this package
	// can grant itself the whole payload, and its ZERO VALUE keeps the cap, so a
	// Subject built anywhere else (SubjectFor, a test, a future consumer) is
	// bounded exactly as before.
	wholeContent bool
}

// Text is the content an inspector scans at this Subject's Point — the tool's
// result after the call, its serialized arguments before it — capped at
// MaxInspectBytes unless the inspector has declared it cannot be outrun
// (WholeContentInspector).
//
// It is a method, not a field the Capped decorator fills in, because a CONTENT
// field would fail OPEN: a caller that built a Subject without wrapping the
// Instance would hand the inspector an empty string, and an inspector handed no
// content Passes. Deriving it here bounds every construction site, wrapped or
// not, while Capped stays responsible for consulting the declaration and for
// recording any truncation on the audit event (content_bytes / inspected_bytes).
func (s Subject) Text() string {
	if s.wholeContent {
		return s.rawText()
	}
	return capForInspection(s.rawText())
}

// rawText is the whole, uncapped content this Subject carries at its Point.
// Capped measures against it to report how much of the content was scanned.
func (s Subject) rawText() string {
	if s.Point == pipeline.PreToolCall {
		return string(s.Args)
	}
	return s.Result
}

// Finding is an Instance's verdict for one Inspect call.
type Finding struct {
	Action  Action         // Pass | Block | Approve
	Reason  string         // → model (Block) / approver (Approve); structured, injection-safe
	Details map[string]any // → audit (e.g. {"urls": [...], "rule": "..."})
}
