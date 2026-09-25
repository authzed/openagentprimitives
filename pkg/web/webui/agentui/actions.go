// actions.go serves POST /agent-ui/{ns}/{name}/actions: the browser's only way
// to INVOKE a declared agent-UI action — the WRITE half of the binding
// vocabulary, where data bindings (bindings.go) are read-only by construction.
//
// The browser sends ONLY a declared action NAME plus the values that action
// declared it accepts. The tool and the args TEMPLATE always come from the
// server-side declaration (resolveDeclaration, viewmodel.go), read fresh per
// request under the viewer's own subject, behind the same
// agentsession#interact gate every POST here shares verbatim
// (gateAgentUIPost, bindings.go).
//
// This route mutates. The runner side re-authorizes and executes through the
// shared privileged core every browser tool surface uses
// (runner.handleAppToolCallReq, over NATS via channelevents.KindUIAction), and
// records the real outcome to a ui_action memory record pushed live later
// (KindUIActionUpdate). This file only sends the invocation and relays the
// SYNCHRONOUS first answer.
package agentui

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"time"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/web/uibindings"
	"github.com/authzed/openagentprimitives/pkg/web/uicomponents"
)

// maxActionBody bounds POST /actions request bodies. Larger than
// maxBindingsBody because a form's field values (Inputs) ride here too,
// still far below anything that could be called a bulk payload.
const maxActionBody = 64 << 10

// actionTimeout bounds the synchronous webd->runner round-trip for one
// action invocation. Matches pkg/web/uibindings/tool's resolveTimeout and
// pkg/web/webui/interact's appToolCallTimeout — the same "the runner may be
// wedged" failure mode as every other synchronous browser->runner call, so
// it gets the same budget rather than a bespoke one.
const actionTimeout = 30 * time.Second

// actionRequestBody is the ONLY thing a browser supplies to invoke an action.
// There is deliberately no tool, no args, no scope and no session field: the
// tool and the args TEMPLATE come from the server-side declaration, and the
// session comes from the CheckInteract-gated URL path. A viewer chooses WHICH
// declared action fires and supplies the values it declared it accepts —
// never what it does.
//
// Stricter than the sibling POST .../app-tool-call, which does accept a
// browser-chosen toolName: that route serves an mcp-ui widget with no
// server-side declaration to read from. This one has one.
type actionRequestBody struct {
	// Action names a declared action; an unknown name rejects the request.
	Action string `json:"action"`
	// Params are the view's own live control values (a selected date range, a
	// selected row) — the same vocabulary a data binding's args template draws
	// on. Disjoint from Inputs by construction (validateActions).
	Params map[string]string `json:"params,omitempty"`
	// Inputs are values supplied by THIS invocation's form only, with no life
	// in the view outside this one call.
	Inputs map[string]string `json:"inputs,omitempty"`
}

// actionResponseBody is the synchronous answer to one invocation.
type actionResponseBody struct {
	// RequestID is an opaque server-minted correlation handle the browser
	// matches later live updates against; never rendered as prose.
	RequestID string `json:"requestId"`
	// State is the action's lifecycle outcome, including a runner-side denial.
	State string `json:"state"`
	// Message is browser-safe human copy; empty when the state says it all.
	Message string `json:"message,omitempty"`
}

// --- POST /agent-ui/{ns}/{name}/actions -----------------------------------

// actionsHandler invokes ONE declared agent-UI action under the viewer's own
// subject, through fail-closed gates checked IN ORDER; the numbered steps are
// marked in the body. Three points carry the weight:
//
//   - Steps 1-3 are gateAgentUIPost (bindings.go), shared VERBATIM with
//     bindingsHandler and startHandler, so no POST route can open a different
//     set of doors for the same (ns, name). Origin pinning is the CSRF
//     boundary here in a way it is not on a GET, and a CheckInteract ERROR is
//     503, never 403 — an outage is not a denial.
//
//   - Step 7 filters params against uicomponents.ParamKeys(decl), NOT
//     ParamNames: the browser's params map is always keyed by the EXPANDED
//     runtime key (an ap:daterange named "window" appears only as
//     "window.from"/"window.to"), so ParamNames would reject every request
//     from a page carrying such a control. Any unknown key in either map
//     rejects the WHOLE request — an undeclared value passing unremarked is
//     the hole the read half closes, and the write half must not reopen it.
//
//   - Step 11 returns 200 even for a runner-side DENIAL, carrying a "denied"
//     state. The lifecycle must render on the control that was clicked; an
//     HTTP error status would be swallowed by the browser's generic error
//     path instead. Failing to REACH the runner (step 10) is different — the
//     caller does not know whether the action ran — and is 503.
//
// Every error path logs through d.Logger(). resp.Message is browser-safe human
// copy by the time it arrives (runner.HandleUIAction reads
// ViewerMessage/uiaction.DisplayCopy, never a raw diagnostic).
func actionsHandler(d Deps) http.Handler {
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

		// 4. Decode the minimal body — action name + values ONLY.
		r.Body = http.MaxBytesReader(w, r.Body, maxActionBody)
		var body actionRequestBody
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON body")
			return
		}

		// 5. Resolve the declaration through the SAME doors bindingsHandler and
		// the live route walk (resolveView, viewmodel.go). A door failure here
		// maps 1:1 onto the ladder's own PageError.
		decl, pe := resolveDeclaration(ctx, d, ns, name)
		if pe != nil {
			writeError(w, pe.Status, pe.Message)
			return
		}

		// 6. The named action must exist in the SERVER's own declaration.
		action, found := decl.Action(body.Action)
		if !found {
			d.Logger().Info("agent-ui actions: request rejected: undeclared action",
				"session", session, "action", body.Action)
			writeError(w, http.StatusBadRequest, "this view's controls do not match the page; reload the page")
			return
		}

		// 7. Filter + validate the supplied params/inputs against the
		// declaration's own vocabulary. Never silently dropped — see the
		// handler doc comment above.
		params, unknownParams := filterParams(body.Params, uicomponents.ParamKeys(decl))
		inputs, unknownInputs := filterActionInputs(body.Inputs, action.Inputs)
		if len(unknownParams) > 0 || len(unknownInputs) > 0 {
			d.Logger().Info("agent-ui actions: request rejected: undeclared parameter(s)/input(s)",
				"session", session, "action", body.Action, "params", unknownParams, "inputs", unknownInputs)
			writeError(w, http.StatusBadRequest, "this view's controls do not match the page; reload the page")
			return
		}

		// 8. Merge into one values map for substitution. Disjoint by
		// construction (validateActions), so a collision means the validator
		// and this route disagree — worth logging, though nothing is silently
		// shadowed: the loop order is deterministic and inputs always win.
		values := make(map[string]string, len(params)+len(inputs))
		for k, v := range params {
			values[k] = v
		}
		for k, v := range inputs {
			if _, collide := values[k]; collide {
				d.Logger().Info("agent-ui actions: an input name collides with a param name; the declaration's own validation should have rejected this",
					"session", session, "action", body.Action, "key", k)
			}
			values[k] = v
		}

		// 9. Fill the action's args template — the SAME substitution the read
		// half (bindings.go) uses.
		args, err := uibindings.SubstituteParams(action.Args, values)
		if err != nil {
			if errors.Is(err, uibindings.ErrUnknownParam) {
				d.Logger().Info("agent-ui actions: action args reference a value the viewer did not supply",
					"session", session, "action", body.Action, "err", err.Error())
				writeError(w, http.StatusBadRequest, "this view is missing a setting it needs; reload the page")
				return
			}
			// Not a client mistake: the args template itself is server-declared
			// and only ever fails to substitute for a reason a viewer cannot fix
			// by supplying different values (e.g. nesting too deep).
			d.Logger().Error(err, "agent-ui actions: substitute params failed on a server-declared action",
				"session", session, "action", body.Action)
			writeError(w, http.StatusInternalServerError, "this action could not be processed")
			return
		}

		// 10. Dispatch to the runner. Any failure to reach it — including a
		// nil/unconfigured NATSRequest, which channelevents.RequestIn itself
		// turns into an error — is fail-closed 503: the caller genuinely does
		// not know whether the action ran, which must never be reported as a
		// (known) denial.
		requestID, err := newActionRequestID()
		if err != nil {
			d.Logger().Error(err, "agent-ui actions: generate request id failed", "session", session, "action", body.Action)
			writeError(w, http.StatusInternalServerError, "this action could not be processed")
			return
		}

		actionReq := channelevents.UIActionRequest{
			RequestID: requestID,
			Action:    body.Action,
			ToolName:  action.Tool,
			Args:      args,
			Requester: subject,
		}

		reply, err := channelevents.RequestIn(d.NATSRequest(), ns, name, channelevents.KindUIAction, actionReq, actionTimeout)
		if err != nil {
			d.Logger().Info("agent-ui actions: RequestIn failed", "session", session, "action", body.Action, "err", err.Error())
			writeError(w, http.StatusServiceUnavailable, "the agent runner did not respond")
			return
		}

		var resp channelevents.UIActionResponse
		if err := json.Unmarshal(reply, &resp); err != nil {
			d.Logger().Info("agent-ui actions: malformed runner reply", "session", session, "action", body.Action, "err", err.Error())
			writeError(w, http.StatusServiceUnavailable, "the agent runner returned an invalid response")
			return
		}

		// 11. Always 200 once the request itself is well-formed and
		// authorized and the runner actually answered — a runner-side denial
		// is a per-action outcome, never a failed request. See the handler
		// doc comment for why.
		writeJSON(w, http.StatusOK, actionResponseBody{RequestID: resp.RequestID, State: resp.State, Message: resp.Message})
	})
}

// filterActionInputs splits browser-supplied inputs into the subset the
// ACTION declared (exact match against action.Inputs) and the rest. The
// caller rejects the whole request when unknown is non-empty; this function
// makes no policy decision.
//
// Distinct from filterParams (bindings.go), which matches against the EXPANDED
// key set because a control's declared name can drive several runtime keys. An
// action's Inputs have no such expansion — each name IS the literal key — so
// this matches action.Inputs directly.
func filterActionInputs(supplied map[string]string, declared []string) (kept map[string]string, unknown []string) {
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

// newActionRequestID returns a 32-character hex string from 16 random bytes,
// used as UIActionRequest.RequestID and transitively the ui_action record's
// key. Never browser-supplied: a client-chosen id could collide with another
// viewer's in-flight action.
//
// Same construction as pkg/web/webui/interact's newAppToolCallRequestID and
// pkg/web/uibindings/tool's newRequestID; duplicated because both are
// unexported in packages this one must not depend on. Unlike interact's, this
// one returns the crypto/rand failure — the caller has an HTTP error path.
func newActionRequestID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate request id: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}
