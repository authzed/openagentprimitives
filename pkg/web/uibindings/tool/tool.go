// Package tool is the "tool" uibindings.Resolver: it turns one agent-UI data
// binding into the same synchronous, readonly-gated app-tool call the
// runner already answers for MCP-UI widgets. All authorization (interact
// re-auth, the AppTools grant lookup, the readonly gate, the per-origin
// rate limit) happens inside runner.Loop.HandleUIDataBinding, reached over
// NATS via channelevents.KindUIDataBinding — this package is a thin wire
// adapter around that privileged core, not a second implementation of it.
package tool

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/authzed/openagentprimitives/pkg/agent/tool/synthesize"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/web/uibindings"
	"github.com/authzed/openagentprimitives/pkg/web/uibindings/registry"
)

// source is this resolver's uibindings.Request.Source / Resolver.Source()
// value, and the string a component prop's Binding declares to route here.
const source = "tool"

// resolveTimeout bounds the synchronous runner round-trip for one binding.
// Matches pkg/web/webui/interact's appToolCallTimeout (the ordinary app-tool-call
// path): a data binding's resolution shares the exact same "the runner may be
// wedged" failure mode, so it gets the same budget rather than a bespoke one.
const resolveTimeout = 30 * time.Second

// Resolver is the "tool" uibindings.Resolver.
type Resolver struct{}

// New returns the "tool" resolver.
func New() uibindings.Resolver { return Resolver{} }

func init() { registry.Register(New()) }

// Source implements uibindings.Resolver.
func (Resolver) Source() string { return source }

// Resolve turns req into a channelevents.AppToolCallRequest and answers it
// through the runner's KindUIDataBinding responder. See the package doc for
// why the privileged checks are NOT duplicated here.
func (Resolver) Resolve(ctx context.Context, d uibindings.Deps, req uibindings.Request) (uibindings.Result, error) {
	logger := d.Logger()

	// NATSRequest is nil in an unconfigured webd. Fail closed with a returned
	// error before building anything — never a silent empty result, and never
	// a nil func reaching a call site as a panic. RequestIn carries the same
	// check as defense in depth, but a resolver fails closed on its own rather
	// than depending on a callee remembering to.
	reqFn := d.NATSRequest()
	if reqFn == nil {
		logger.Info("ui data binding tool resolve failed: NATSRequest is not configured",
			"session", req.Namespace+"/"+req.Session, "ui", req.Path, "ref", req.Ref)
		return uibindings.Result{}, errors.New("uibindings/tool: NATSRequest is not configured")
	}

	requestID, err := newRequestID()
	if err != nil {
		return uibindings.Result{}, fmt.Errorf("uibindings/tool: %w", err)
	}

	// The ref carries no CRD pattern of its own (it lives in an opaque
	// slots[].default JSON blob elsewhere), so normalizing it here is what
	// makes it land on the same AppTools registry key synthesize.Build already
	// produced for the tool — not belt-and-braces, the call fails NotFound
	// without it.
	callReq := channelevents.AppToolCallRequest{
		ToolName:  synthesize.NormalizeName(req.Ref),
		Args:      req.Args,
		Requester: req.Subject,
		RequestID: requestID,
	}

	raw, err := channelevents.RequestIn(reqFn, req.Namespace, req.Session, channelevents.KindUIDataBinding, callReq, resolveTimeout)
	if err != nil {
		logger.Info("ui data binding tool resolve failed: runner request failed",
			"session", req.Namespace+"/"+req.Session, "ui", req.Path, "ref", callReq.ToolName, "err", err.Error())
		// The session's RUNNER answers this request, so every failure means
		// the same thing to a viewer — nothing on the agent side replied —
		// and the common cause is the ordinary one: an idle session's pods
		// were reaped, leaving the subject with no subscriber.
		//
		// A TYPED sentinel, not just copy: the caller ACTS on this one by
		// waking the session, and matching on message text to decide that
		// would let a copy edit silently disable the recovery. The copy is the
		// fallback for a caller that cannot wake, and hedges deliberately —
		// a timeout cannot tell "no runner" from "runner wedged".
		return uibindings.Result{}, fmt.Errorf("%w: the agent for this session isn't responding", uibindings.ErrRunnerUnreachable)
	}

	var resp channelevents.AppToolCallResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		logger.Info("ui data binding tool resolve failed: malformed runner reply",
			"session", req.Namespace+"/"+req.Session, "ui", req.Path, "ref", callReq.ToolName, "err", err.Error())
		return uibindings.Result{}, errors.New("this view's data could not be loaded")
	}

	if resp.Status != channelevents.AppToolCallStatusOK {
		err := errorForStatus(resp)
		// resp.Message is logged and NOT returned: it is the only place the
		// real cause (the containment pipeline's deny reason) survives, since
		// errorForStatus deliberately drops it rather than render it to a
		// viewer. Without this field an operator would have a denied binding
		// with no recorded reason on the webd side at all.
		logger.Info("ui data binding tool resolve rejected by runner",
			"session", req.Namespace+"/"+req.Session, "ui", req.Path, "ref", callReq.ToolName,
			"status", resp.Status, "reason", resp.Message)
		return uibindings.Result{}, err
	}

	if resp.IsError {
		msg := resultText(resp.Result)
		if msg == "" {
			msg = "this action failed"
		}
		logger.Info("ui data binding tool resolve: bound tool returned an error result",
			"session", req.Namespace+"/"+req.Session, "ui", req.Path, "ref", callReq.ToolName)
		return uibindings.Result{}, errors.New(msg)
	}

	return uibindings.Result{Value: unwrapResult(resp.Result)}, nil
}

// errorForStatus maps a non-OK AppToolCallResponse to browser-safe human
// copy — no tool registry vocabulary, no NATS subjects, no CRD names, no
// SpiceDB permissions, no resource IDs.
//
// It reads ViewerMessage, NEVER Message. Message is the operator/diagnostic
// channel — on the denial arm it carries the containment pipeline's own hook
// reason, naming permissions, resource IDs, raw subjects, CRD fields — and
// rendering that in a browser is the no-internal-ops-in-user-messages
// violation this repo forbids. Resolve logs the real reason with session/ui/ref.
//
// ViewerMessage is set only where a reply site authored its own viewer copy;
// everywhere else it is empty and the fixed copy below is used. not_found and
// requires_approval take fixed copy unconditionally — their runner-side text
// is absent or written for a different caller.
func errorForStatus(resp channelevents.AppToolCallResponse) error {
	switch resp.Status {
	case channelevents.AppToolCallStatusDenied:
		if resp.ViewerMessage != "" {
			return errors.New(resp.ViewerMessage)
		}
		return errors.New("you do not have access to this data")
	case channelevents.AppToolCallStatusMisconfigured:
		// Deliberately fixed copy, never ViewerMessage: the runner-side text is
		// the CEL failure, and a viewer cannot act on a rule they did not write
		// and cannot see. What they CAN act on is knowing this is not about
		// them — the section is broken for everyone, retrying and requesting
		// access are both wasted effort, and someone else has already been
		// told. The detail went to the monitoring channel.
		return errors.New("this section can't load: the agent's configuration has an error that's been reported to its operators")
	case channelevents.AppToolCallStatusNotFound:
		return errors.New("this view's data source is not available")
	case channelevents.AppToolCallStatusRateLimited:
		if resp.ViewerMessage != "" {
			return errors.New(resp.ViewerMessage)
		}
		return errors.New("rate limit exceeded; try again shortly")
	case channelevents.AppToolCallStatusRequiresApproval:
		// handleAppToolCallReq denies any non-readonly data binding BEFORE it
		// ever reaches the approval branch (dataBinding=true short-circuits
		// there), so this status is unreachable on the binding path in
		// practice. Mapped to an error rather than left unhandled anyway, per
		// the no-bypass-return rule: an unreachable-in-practice branch still
		// gets a real outcome, not a silent fallthrough.
		return errors.New("this data cannot be read without approval")
	default:
		return errors.New("this view's data could not be loaded")
	}
}

// unwrapResult unwraps an AppToolCallResponse.Result for binding into a
// component prop. Result is always json.Marshal of a Go string (the tool
// result's Content, JSON-Marshal'd once by appToolResponseFor) — so it is
// unmarshaled into a string here, and if THAT string is itself valid JSON
// (a tool that serialized a table as text) its bytes are returned directly
// so ap:table can bind rows; otherwise the original quoted string is kept so
// prose stays bindable to ap:markdown.body as a JSON string.
func unwrapResult(raw json.RawMessage) json.RawMessage {
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		// Not a JSON string at all (a malformed or non-conforming reply) —
		// hand back whatever arrived rather than manufacture a value.
		return raw
	}
	if json.Valid([]byte(s)) {
		return json.RawMessage(s)
	}
	return raw
}

// resultText returns the plain-text form of an AppToolCallResponse.Result
// for use in an error message: the unmarshaled string when Result is a JSON
// string, else the raw bytes verbatim.
func resultText(raw json.RawMessage) string {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	return string(raw)
}

// newRequestID returns a 32-character hex string from 16 random bytes for
// AppToolCallRequest.RequestID — server-minted, because a client-supplied id
// could collide with another viewer's in-flight request. Same construction as
// pkg/web/webui/interact's newAppToolCallRequestID, duplicated because that
// helper is unexported in a package this one must not depend on
// (uibindings/tool → webui is the wrong direction). Unlike that helper, this
// returns the crypto/rand failure — Resolve has an error channel for it.
func newRequestID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate request id: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}
