// bindings.go serves POST /agent-ui/{ns}/{name}/bindings: the browser's only
// way to fill a declared component prop's data binding.
//
// The browser sends ONLY a parameter map. The binding's source, ref and args
// TEMPLATE always come from the server-side declaration (resolveDeclaration,
// viewmodel.go), read fresh per request under the viewer's own subject, behind
// the shared agentsession#interact gate (gateAgentUIPost, below). Resolution
// is delegated to pkg/web/uibindings' registered resolvers — this file never
// branches on a source string.
package agentui

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"sync"

	"github.com/authzed/openagentprimitives/pkg/authz/toolguard"
	"github.com/authzed/openagentprimitives/pkg/web/uibindings"
	"github.com/authzed/openagentprimitives/pkg/web/uibindings/registry"
	"github.com/authzed/openagentprimitives/pkg/web/uicomponents"
	"github.com/authzed/openagentprimitives/pkg/web/uiselect"
	"github.com/authzed/openagentprimitives/pkg/web/webui"
)

// maxBindingsBody bounds POST /bindings request bodies — the body carries
// only a parameter map (bindingsRequestBody), never a bulk payload. Mirrors
// pkg/web/webui/interact's maxAppToolCallBody rationale.
const maxBindingsBody = 16 << 10

// maxBindingsPerRequest bounds the fan-out one POST resolves. A real UI
// declares a handful of bound props; a document with more than this is
// refused outright (400) rather than silently truncated — truncating would
// mean SOME slots never get a chance to render, with nothing in the
// response saying why.
const maxBindingsPerRequest = 64

// selectorInvalidMessage and selectorMissMessage are the two browser-facing
// outcomes of a selector that produced no value. FIXED copy: a
// *uiselect.MissError names the selector and the upstream object's own field
// names, which a viewer cannot act on and which the user-facing-text rule
// keeps out of the UI. The real diagnostic — failing segment, element index,
// keys that WERE present — exists only in the operator log
// (resolveOneBinding).
const (
	// selectorInvalidMessage: the declaration itself is wrong — the author's
	// fault, and reachable only for a declaration validated by a reconciler
	// predating this platform version's syntax gate.
	selectorInvalidMessage = "this view is not configured correctly"
	// selectorMissMessage: the declaration is fine and the response did not
	// have the shape it declares — a changed upstream, or a selector written
	// against a different endpoint.
	selectorMissMessage = "this view's data is not in the shape this section expects"
)

// selectorTooLargeMessage is the browser-facing copy for the ceiling check on
// the value AFTER extraction — the SECOND of two meters this path needs. The
// RAW upstream response is already metered before Apply runs (memoryref and
// artifactref meter their own fetched bytes; the "tool" source's raw result is
// metered by toolguard.GuardRecord in the runner's pipeline). That is not
// sufficient: encoding/json escapes '<', '>', '&' and U+2028/U+2029 on
// re-marshal with no stdlib opt-out, so Apply's OUTPUT can exceed the raw
// response it came from — a response just under the ceiling can extract to
// several times over it.
const selectorTooLargeMessage = "This view's data is too large to display. Narrow the time span, add a filter, or ask for fewer rows."

// bindingsRequestBody is the ONLY thing a browser supplies. There is
// deliberately no source, ref, args, scope or session field: every one comes
// from the server-side declaration or the CheckInteract-gated URL path. A
// viewer may change WHICH time span a granted readonly tool is asked about —
// never which tool, memory kind, artifact, or session. Any other field a
// client sends is simply not a member of this struct and is dropped by the
// decoder; no path in bindingsHandler reads a client-supplied source/ref.
type bindingsRequestBody struct {
	// Params are the view's live control values, filtered against the
	// declaration's own key set before any substitution.
	Params map[string]string `json:"params,omitempty"`
}

// bindingResult is one entry of the response, keyed by uicomponents.BindingPath.
type bindingResult struct {
	// Status is "ok", "error", "needs_input", or "waking".
	//
	// needs_input and waking are NOT failures and must not render as one; the
	// browser picks its component from this status alone. needs_input means
	// the binding waits on a choice the viewer has not made — a control
	// declaring a parameter with no default, the shape an author is pushed
	// toward whenever a literal default would go stale. waking means the
	// session's runner was reaped and a wake is requested: the data is coming,
	// and the browser retries. The two ask opposite things — choose something
	// versus wait — and neither is a fault, so drawing a red "cannot load"
	// card over either reports a failure that has not happened.
	Status string `json:"status"`
	// Value is the resolved (and selector-extracted) data; absent unless
	// Status=="ok".
	Value json.RawMessage `json:"value,omitempty"`
	// Message is human copy for the viewer, never an internal identifier.
	Message string `json:"message,omitempty"`
}

// bindingStatusNeedsInput marks a binding waiting on a viewer choice rather
// than one that failed. Spelled once, here, because it is a wire value the
// browser switches on (web/packages/agentui/src/bindings.ts) and a typo would
// silently fall through to that switch's error arm — restoring exactly the red
// card this status exists to remove.
const bindingStatusNeedsInput = "needs_input"

type bindingsResponseBody struct {
	// Bindings maps each uicomponents.BindingPath to its own outcome; one
	// binding's failure never fails the others.
	Bindings map[string]bindingResult `json:"bindings"`
}

// resolverDeps adapts agentui.Deps to pkg/web/uibindings.Deps. Every method
// uibindings.Deps needs already lives on agentui.Deps (see deps.go), so
// embedding suffices; the type exists only so the plugin's own Deps interface
// stays the SINGLE thing internal/cmd/webd must satisfy.
type resolverDeps struct{ Deps }

var _ uibindings.Deps = resolverDeps{}

// --- POST /agent-ui/{ns}/{name}/bindings ----------------------------------

// bindingsHandler resolves every data binding in a session's agent-UI
// declaration under the viewer's own subject, through fail-closed gates
// checked IN ORDER; the numbered steps are marked in the body. Four points
// carry the weight:
//
//   - Steps 1-3 are gateAgentUIPost, shared VERBATIM with actionsHandler and
//     startHandler, so no state-changing POST route can open a different set
//     of doors for the same (ns, name). CheckInteract IS this route's
//     authorization — nothing upstream ran one — and an ERROR fails closed as
//     503, never 403.
//
//   - Step 5 resolves through the same session -> class -> UI ladder ViewFor
//     uses, INCLUDING the AgentUI Valid=True gate, so a document the
//     reconciler never validated is never resolved against even though this
//     route is reachable directly.
//
//   - Step 6 rejects the WHOLE request (400) for any param key outside
//     uicomponents.ParamKeys(decl), rather than dropping it. Only this
//     fan-out caller can see the full key set — a single binding's args
//     template sees only the name it references — so an undeclared key
//     dropped here would surface, if at all, as an unrelated missing-param
//     failure much later.
//
//   - Step 9 is ALWAYS 200 once the request is well-formed and authorized: a
//     failed binding is a per-binding "error" result and the rest of the page
//     keeps working.
//
// Bindings are resolved CONCURRENTLY into a pre-sized slice indexed by
// position — no shared map writes, no mutex. Every error path logs through
// d.Logger(); the browser receives only human copy.
func bindingsHandler(d Deps) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		ns := r.PathValue("ns")
		name := r.PathValue("name")
		session := ns + "/" + name

		// 1-3. Shared preamble — see gateAgentUIPost's doc comment.
		subject, ok := gateAgentUIPost(w, r, d)
		if !ok {
			return
		}

		// 4. Decode the minimal body — params ONLY.
		r.Body = http.MaxBytesReader(w, r.Body, maxBindingsBody)
		var body bindingsRequestBody
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON body")
			return
		}

		// 5. Resolve the declaration through the SAME doors actionsHandler and
		// the live route walk (resolveView, viewmodel.go). A door failure here
		// maps 1:1 onto the ladder's own PageError.
		decl, pe := resolveDeclaration(ctx, d, ns, name)
		if pe != nil {
			writeError(w, pe.Status, pe.Message)
			return
		}

		// 6. Filter + validate the supplied params against the full
		// declaration's OWN set of runtime parameter keys — see the doc
		// comment above for why this belongs here and not in SubstituteParams.
		declared := uicomponents.ParamKeys(decl)
		params, unknown := filterParams(body.Params, declared)
		if len(unknown) > 0 {
			d.Logger().Info("agent-ui bindings: request rejected: undeclared parameter(s)",
				"session", session, "params", unknown)
			writeError(w, http.StatusBadRequest, "this view's settings do not match the page; reload the page")
			return
		}

		// 7. Enumerate bindings; refuse rather than truncate over the bound.
		bound := uicomponents.WalkBindings(decl)
		if len(bound) > maxBindingsPerRequest {
			d.Logger().Info("agent-ui bindings: request rejected: too many bindings",
				"session", session, "count", len(bound), "max", maxBindingsPerRequest)
			writeError(w, http.StatusBadRequest, "this view has too many data sources to load at once")
			return
		}

		// 8. Resolve every binding concurrently into a PRE-SIZED slice indexed
		// by position: each goroutine only ever writes its own index, so there
		// is no shared map write and no mutex to hold.
		type outcome struct {
			path   string
			result bindingResult
		}
		results := make([]outcome, len(bound))
		var wg sync.WaitGroup
		for i, bp := range bound {
			wg.Add(1)
			go func(i int, bp uicomponents.BoundProp) {
				defer wg.Done()
				results[i] = outcome{path: bp.Path, result: resolveOneBinding(ctx, d, ns, name, subject, params, bp)}
			}(i, bp)
		}
		wg.Wait()

		// 9. Always 200 once the request itself is well-formed and authorized —
		// a failed binding is a per-binding result, never a failed request.
		out := bindingsResponseBody{Bindings: make(map[string]bindingResult, len(results))}
		for _, o := range results {
			out.Bindings[o.path] = o.result
		}
		writeJSON(w, http.StatusOK, out)
	})
}

// resolveOneBinding substitutes bp's args template from params, parses and
// applies bp's selector (pkg/web/uiselect — the MATCH gate), and resolves the
// source through the registry — every failure mode mapped to a client-safe
// bindingResult, with the real cause logged. Order matters and is fixed:
//
//  1. SubstituteParams — cheap, no I/O.
//  2. uiselect.Parse(bp.Binding.Select) — ALSO cheap, no I/O: a selector that
//     cannot parse can never produce a value, so calling the upstream for it
//     would be pure cost with no possible payoff. Parsed BEFORE resolving,
//     never after — see selectorInvalidMessage's doc.
//  3. registry.Get + resolver.Resolve — the one step that costs an upstream
//     round-trip.
//  4. sel.Apply(result.Value). A *uiselect.MissError is NEVER folded into
//     step 3's arm: step 3 forwards err.Error() verbatim, safe only because
//     every shipped resolver returns browser-safe copy. A uiselect error does
//     not, so it gets its own arm with fixed copy.
//  5. The ceiling check on the value AFTER extraction — see
//     selectorTooLargeMessage for why the raw-response meter upstream is not
//     sufficient by itself.
func resolveOneBinding(ctx context.Context, d Deps, ns, name, subject string, params map[string]string, bp uicomponents.BoundProp) bindingResult {
	logger := d.Logger()
	session := ns + "/" + name

	args, err := uibindings.SubstituteParams(bp.Binding.Args, params)
	if err != nil {
		if errors.Is(err, uibindings.ErrUnknownParam) {
			// uicomponents.Validate rejects an unsatisfiable $param reference,
			// so the only way here is a control declaring a parameter with no
			// default (a legal ap:select with a placeholder and no `value`).
			// The key exists; the viewer just has not chosen. The copy asks
			// them to pick and deliberately never says "reload" — a reload
			// re-sends the same empty map and re-renders the same card.
			logger.Info("agent-ui bindings: binding args reference a param the viewer has not chosen a value for",
				"session", session, "path", bp.Path, "source", bp.Binding.Source, "err", err.Error())
			return bindingResult{
				Status:  bindingStatusNeedsInput,
				Message: "Choose a value above to load this data.",
			}
		}
		logger.Info("agent-ui bindings: substitute params failed",
			"session", session, "path", bp.Path, "source", bp.Binding.Source, "err", err.Error())
		return bindingResult{Status: "error", Message: "this view's data could not be loaded"}
	}

	// Parse BEFORE resolving — see this func's own doc comment for why the
	// order is load-bearing, not incidental.
	sel, err := uiselect.Parse(bp.Binding.Select)
	if err != nil {
		// Reachable only for a declaration validated by a reconciler
		// predating this platform version's syntax gate. Not a panic and not
		// silently ignored: a selector that stops being applied would let a
		// whole envelope reach ap:table.rows with nothing in the logs.
		logger.Info("agent-ui bindings: selector failed to parse",
			"session", session, "path", bp.Path, "source", bp.Binding.Source, "ref", bp.Binding.Ref,
			"select", bp.Binding.Select, "err", err.Error())
		return bindingResult{Status: "error", Message: selectorInvalidMessage}
	}

	resolver, ok := registry.Get(bp.Binding.Source)
	if !ok {
		// A miss means the binary is missing the blank import for that source
		// package — the same silent-404 class this package's Deps cast guard
		// exists for. Logged loudly rather than left as a bare "this binding
		// is broken" with nothing to diagnose from.
		logger.Info("agent-ui bindings: no resolver registered for source",
			"session", session, "path", bp.Path, "source", bp.Binding.Source)
		return bindingResult{Status: "error", Message: "this view's data source is not available"}
	}

	req := uibindings.Request{
		Namespace: ns,
		Session:   name,
		Subject:   subject,
		Path:      bp.Path,
		Ref:       bp.Binding.Ref,
		Args:      args,
	}
	result, err := resolver.Resolve(ctx, resolverDeps{d}, req)
	if err != nil {
		// The runner not answering is the one resolve failure the platform can
		// act on: ask for the session back (wake.go). The cost is a model turn
		// — the woken runner re-enters its loop before settling — because what
		// a data binding actually needs is the runner's privileged core (viewer
		// re-authorization, readonly gate, containment pipeline, resolved
		// credentials) WITHOUT the agent loop, and no serve-only runner mode
		// exists yet. The sentinel stays typed rather than
		// prose because it is the seam that work plugs into.
		if errors.Is(err, uibindings.ErrRunnerUnreachable) {
			// Usually an idle session whose pods were reaped. "waking" is not
			// an error — the request is in progress and the browser retries —
			// so reporting one would put a destructive card over a section
			// that is about to work.
			if requestWake(ctx, d, ns, name) {
				logger.Info("agent-ui bindings: the session runner was gone; a wake is pending",
					"session", session, "path", bp.Path, "ref", bp.Binding.Ref)
				return bindingResult{Status: "waking", Message: "Waking this agent…"}
			}
			// No wake is coming — a wedged runner rather than a reaped one,
			// an unwritable namespace, or a failed patch. All three mean the
			// page cannot be recovered from here, so say so plainly rather
			// than animate a retry that can never succeed.
			logger.Info("agent-ui bindings: the session runner did not answer and no wake could be requested",
				"session", session, "path", bp.Path, "ref", bp.Binding.Ref, "err", err.Error())
			return bindingResult{Status: "error", Message: "the agent for this session isn't responding"}
		}
		// Every shipped resolver (tool, memory, artifact) already returns
		// browser-safe human copy from Resolve — see their own doc comments —
		// so err.Error() is safe to forward verbatim here.
		logger.Info("agent-ui bindings: binding resolve failed",
			"session", session, "path", bp.Path, "source", bp.Binding.Source, "err", err.Error())
		return bindingResult{Status: "error", Message: err.Error()}
	}

	value, err := sel.Apply(result.Value)
	if err != nil {
		// A *uiselect.MissError carries the operator-actionable diagnostic
		// (which segment, which element, what WAS there). It is logged here
		// and NEVER returned: Message stays the fixed, leak-free
		// selectorMissMessage whatever the underlying uiselect error, since
		// all of them read the same to a viewer.
		fields := []any{
			"session", session, "path", bp.Path, "source", bp.Binding.Source,
			"ref", bp.Binding.Ref, "select", sel.String(),
		}
		var missErr *uiselect.MissError
		if errors.As(err, &missErr) {
			fields = append(fields,
				"segment", missErr.Segment, "index", missErr.Index,
				"element", missErr.Element, "kind", missErr.Kind, "found", missErr.Found,
			)
		}
		logger.Info("agent-ui bindings: selector did not match the resolved value", fields...)
		return bindingResult{Status: "error", Message: selectorMissMessage}
	}

	// The SECOND ceiling meter — see selectorTooLargeMessage's doc. Withheld,
	// not truncated: a truncated JSON document is not parseable, which would
	// turn a size failure into a confusing parse failure on the browser side.
	if n := int64(len(value)); n > toolguard.DefaultUIIngressBytes {
		logger.Info("agent-ui bindings: selector result over the UI ingress ceiling",
			"session", session, "path", bp.Path, "source", bp.Binding.Source, "ref", bp.Binding.Ref,
			"select", sel.String(), "bytes", n, "limit", toolguard.DefaultUIIngressBytes)
		return bindingResult{Status: "error", Message: selectorTooLargeMessage}
	}

	return bindingResult{Status: "ok", Value: value}
}

// filterParams splits browser-supplied params into the subset that matches a
// declared runtime parameter key EXACTLY, and the keys that match nothing.
// The caller rejects the whole request when unknown is non-empty (see
// bindingsHandler step 6); this function itself makes no policy decision.
//
// Exact membership, never a "<name>." prefix rule: compound keys arrive from
// uicomponents.ParamKeys already expanded by each control's own registered
// ParamValues. A prefix test would duplicate a control's knowledge in a
// consumer AND be strictly looser — "window.anything" would pass.
func filterParams(supplied map[string]string, declared []string) (kept map[string]string, unknown []string) {
	kept = make(map[string]string, len(supplied))
	for k, v := range supplied {
		if slices.Contains(declared, k) {
			kept[k] = v
			continue
		}
		unknown = append(unknown, k)
	}
	return kept, unknown
}

// --- shared POST preamble ---------------------------------------------------

// gateAgentUIPost is this package's REFERENCE gate — where a reader asking
// "what authorizes an agent-UI request" should land. All three state-changing
// POST routes (bindings, actions, start) call it, so none can independently
// open a different set of doors for the same (ns, name):
//
//  1. webui cookie auth -> subject; empty => 401.
//  2. Origin pinned to the trusted origin (CSRF) => mismatch => 403 + log.
//  3. CheckInteract(ns, name, subject) -> agentsession#interact, fully
//     consistently. An error is NEVER a denial: 503, never 403.
//
// Nothing upstream authorizes anything — GET /agent-ui/{ns}/{name} is a
// redirect that reads nothing. The live route is the one place this preamble
// is not reused: it runs the same CheckInteract inline before its upgrade,
// because a websocket answers a failure differently.
//
// On ok=false the error response is ALREADY written; the caller only returns.
func gateAgentUIPost(w http.ResponseWriter, r *http.Request, d Deps) (subject string, ok bool) {
	ctx := r.Context()
	ns := r.PathValue("ns")
	name := r.PathValue("name")
	session := ns + "/" + name

	// 1. Authentication.
	subject = webui.SubjectFromContext(ctx)
	if subject == "" {
		writeError(w, http.StatusUnauthorized, "not authenticated")
		return "", false
	}

	// 2. Origin pin (CSRF) — the shared webui.TrustedOriginMatch, so every
	// browser-only mutating route fails closed on a blank trusted origin
	// rather than each re-deriving a compare that silently passes when unset.
	if !webui.TrustedOriginMatch(r, d.TrustedOrigin()) {
		d.Logger().Info("agent-ui: origin mismatch", "session", session, "origin", r.Header.Get("Origin"))
		writeError(w, http.StatusForbidden, "origin not trusted")
		return "", false
	}

	// 3. Send/view authorization: agentsession#interact. An error is
	// fail-closed 503, never 403 — an authz outage must never be read as a
	// denial decision.
	okInteract, err := d.CheckInteract(ctx, ns, name, subject)
	if err != nil {
		d.Logger().Info("agent-ui: CheckInteract errored", "session", session, "subject", subject, "err", err.Error())
		writeError(w, http.StatusServiceUnavailable, "authorization check failed")
		return "", false
	}
	if !okInteract {
		writeError(w, http.StatusForbidden, "not authorized to interact with this session")
		return "", false
	}
	return subject, true
}

// --- shared HTTP helpers ---------------------------------------------------

type bindingsErrorResponse struct {
	// Error is the client-safe reason; internal causes stay in the logs.
	Error string `json:"error"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, bindingsErrorResponse{Error: msg})
}
