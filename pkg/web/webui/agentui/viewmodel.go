// viewmodel.go serves the browser-facing MERGED agent-UI declaration —
// Tier 0 (the AgentUI CR) with the agent's stored Tier-1 fragments applied
// through uiview.Resolve and RE-VALIDATED on every read — the ONE document
// the session shell's view props (ViewFor, below), the bindings route
// (bindings.go), the actions route (actions.go), and the live route
// (live.go, once it pushes a declaration) all resolve against.
//
// resolveDeclaration (below) is a thin wrapper over resolveView, and it is
// what bindings.go and actions.go call. Because it returns the MERGED
// declaration, an agent-composed hook's binding is resolvable and an
// agent-named action findable on every route that serves this session,
// without either route knowing the merge happened.
package agentui

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"

	"github.com/authzed/openagentprimitives/pkg/agent/tool/synthesize"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/uiviewparams"
	"github.com/authzed/openagentprimitives/pkg/web/uicomponents"
	"github.com/authzed/openagentprimitives/pkg/web/uigrant"
	"github.com/authzed/openagentprimitives/pkg/web/uiview"
	"github.com/authzed/openagentprimitives/pkg/web/webui"
)

// viewOptions builds the ceiling-scoped validator options webd re-validates
// a session's merged declaration against.
//
// GrantedTools and ReadonlyTools are deliberately the SAME set: webd has no
// session origins — which MCPServer/SidecarToolbox origins resolved for THIS
// session is a live per-session fact only the runner's materialization
// computes — so it cannot evaluate the readonly auto-run predicate at all.
// Honestly LOOSE, never a widening of real authorization: a binding to a
// mutating tool may RENDER here after a grant change, but it can never be
// CALLED, because handleAppToolCallReq's readonly gate is unconditional and
// re-evaluates the live tool on every call. Same posture
// uicomponents.Options.ReadonlyTools describes for the AgentUI reconciler.
//
// KNOWN GAP. This ceiling is uigrant.Ceiling(aui.Spec.Tools,
// ac.Spec.AgentUI.GrantedTools) and nothing more — a strict SUBSET of what
// the AgentUI reconciler's grantedToolsForDeclaration uses for its own Tier-0
// Validate, which additionally unions in each action's own tool so a bundle
// author need not also list it in spec.Tools.
//
// Concretely: an AgentUI whose action names a granted tool absent from
// spec.Tools reconciles clean and renders clean (uiview.Resolve validates only
// once a fragment is applied). The gap opens on the FIRST agent write to ANY
// hook — Validate re-checks the whole action table on every candidate, so that
// action's tool failing this narrower ceiling rejects every subsequent write,
// not just ones bound to it. viewmodel_test.go's fixture routes around it by
// listing the tool in both places; a bundle author cannot. Closing it means
// duplicating that union here or exporting it from the controller.
func viewOptions(doors agentUIDoors) uicomponents.Options {
	o := uicomponents.DefaultOptions()
	o.NormalizeToolName = synthesize.NormalizeName

	var requested, granted []string
	if doors.UI != nil {
		requested = doors.UI.Spec.Tools
	}
	if doors.Grant != nil {
		granted = doors.Grant.GrantedTools
	}
	ceiling := uigrant.Ceiling(requested, granted)

	set := make(map[string]bool, len(ceiling))
	for _, t := range ceiling {
		set[t] = true
	}
	o.GrantedTools = set
	o.ReadonlyTools = set
	return o
}

// mergedViewFor is resolveView's and ViewFor's shared tail: given resolved
// doors, read the Tier-0 declaration with the agent's stored fragments merged
// on and RE-VALIDATED via uiview.Resolve. Shared so the two callers cannot
// open the memory backend differently or skip the re-validation.
//
// Every rejection is LOGGED with session, ui, hook and reason. A dropped
// fragment presents as "the agent's change didn't show up", which reads as a
// slow agent rather than a bug, so the log is the only place the truth lives.
func mergedViewFor(ctx context.Context, d Deps, doors agentUIDoors, ns, name string) (uicomponents.View, *webui.PageError) {
	mem := d.Memory()
	if mem == nil {
		// Fail closed LOUDLY (as live.go's nil-Memory branch does): an empty
		// fragment list would read as "the agent composed nothing", a claim
		// nothing checked, hiding a broken deployment behind a page that looks
		// like it rendered correctly.
		d.Logger().Info("agentui: Memory not configured; failing closed",
			"ns", ns, "name", name, "ui", doors.UI.Name)
		return uicomponents.View{}, &webui.PageError{Status: http.StatusServiceUnavailable, Kind: "error",
			Title: "UI unavailable", Message: "This agent's UI isn't available right now."}
	}

	// Scope is built server-side from the URL path exactly as bindings.go's
	// resolveOneBinding already builds it — never from anything a browser
	// supplies.
	scope := memory.Scope{Kind: "session", ID: ns + "/" + name}

	v, err := uiview.Resolve(ctx, mem, scope, doors.UI, viewOptions(doors))
	if err != nil {
		d.Logger().Error(err, "agentui: resolve merged view failed", "ns", ns, "name", name, "ui", doors.UI.Name)
		return uicomponents.View{}, &webui.PageError{Status: http.StatusInternalServerError, Kind: "error",
			Title: "Rendering error", Message: "Could not render this agent's UI."}
	}

	for _, r := range v.Rejected {
		d.Logger().Info("agentui: stored fragment dropped on read; hook serves its Tier-0 default",
			"ns", ns, "name", name, "ui", doors.UI.Name, "hook", r.Hook, "reason", r.Reason)
	}

	return v, nil
}

// resolveView is the one browser-facing read of a session's UI for the three
// /agent-ui/{ns}/{name}/... API routes: the doors ladder
// (resolveAgentUIDoors, page.go), then mergedViewFor's merge + re-validation.
//
// PRECONDITION: the caller has already confirmed the viewer may interact with
// (ns, name). CheckInteract is NOT re-checked here — gateAgentUIPost gates the
// POST routes and liveHandler gates itself. ViewFor carries the same
// precondition, gated by shellPageBuild in pkg/web/webui/sessions.
func resolveView(ctx context.Context, d Deps, ns, name string) (agentUIDoors, uicomponents.View, *webui.PageError) {
	doors, pe := resolveAgentUIDoors(ctx, d, ns, name)
	if pe != nil {
		return agentUIDoors{}, uicomponents.View{}, pe
	}
	v, pe := mergedViewFor(ctx, d, doors, ns, name)
	if pe != nil {
		return agentUIDoors{}, uicomponents.View{}, pe
	}
	return doors, v, nil
}

// Availability is ViewFor's second return: whether this session HAS an
// agent-defined UI at all. It is deliberately not folded into the error
// return — "this agent declares no UI" is the ordinary case for most agents
// and selects a different view (chat), while a *webui.PageError means
// something is wrong with a UI that IS declared.
type Availability int

const (
	// UIAvailable: props are populated and the agent-defined view can render.
	// Also the value ViewFor returns alongside a non-nil *webui.PageError —
	// the agent DOES declare a UI in that case, it just cannot currently be
	// rendered, so it is a doors failure, not a "no UI" one. Callers branch
	// on the error first.
	UIAvailable Availability = iota
	// UINotDeclared: the AgentClass has no spec.agentUI. Not an error. The
	// three /agent-ui/{ns}/{name}/… API routes still answer 404 for this case
	// (resolveAgentUIDoors) — they have nothing to serve — but a shell selects
	// the chat view.
	UINotDeclared
)

// UIChromeRequest is the AgentUI's REQUESTED chrome visibility
// (AgentUISpec.Chrome.InitialState), returned alongside the view because the
// shell — not the view — now owns chrome. A request only: the viewer's own
// toggle outranks it and any trust event auto-reveals over it.
type UIChromeRequest struct {
	// InitialState is the chrome state the AgentUI asks for on first paint;
	// empty means the shell's own default.
	InitialState string `json:"initialState,omitempty"`
}

// ViewProps is the agent-defined view's bootstrap shape: what the content
// region needs, and nothing chrome needs. The viewer's identity, the session
// disclosure and the requested chrome state are the shell's
// (pkg/web/webui/sessions), because chrome is the shell's — see its own
// shellProps.
type ViewProps struct {
	// Ns is the session's namespace.
	Ns string `json:"ns"`
	// Name is the resolved AgentSession name, which may differ from the one in
	// the URL when the doors ladder resolved an alias.
	Name string `json:"name"`
	// Declaration is the merged, re-validated wire document (declarationWire)
	// — the page tree plus the hooks the agent composed.
	Declaration json.RawMessage `json:"declaration"`
	// AgentParams is what the AGENT set this view's controls to
	// (set_view_params); empty on the ordinary page, where the viewer drives
	// every control themselves.
	//
	// Carried SEPARATELY from the declaration so the browser can tell an
	// author's default from an agent's choice: both are "a value the viewer
	// did not pick", but they lose to a viewer's own selection at different
	// moments. AgentUIView owns that precedence, because only the browser
	// knows what the viewer has touched.
	//
	// Filtered to the declaration's own parameter keys on the way out, so a
	// value stored against a control a redeploy removed is dropped here rather
	// than shipped to a page that can neither use nor clear it.
	AgentParams map[string]string `json:"agentParams,omitempty"`
}

// ViewFor resolves the agent-defined view for a session the caller has
// ALREADY confirmed the viewer may interact with. It walks the same session
// -> class -> UI ladder resolveAgentUIDoors walks (via walkAgentUIDoors) and
// returns the same merged, re-validated declaration declarationWireFor
// produces, so the shell and the bindings/actions routes cannot serve
// different documents for one session.
//
// It differs from resolveAgentUIDoors in exactly one place: a class with no
// spec.agentUI returns (zero, UINotDeclared, nil) instead of a 404. Every
// other door returns the same *webui.PageError, which the shell renders INTO
// the content region rather than in place of the page.
//
// PRECONDITIONS the caller must have met: the viewer may interact with (ns,
// name), and the session is not Ended. An ended session's doors answer 410
// here — right for the API routes, wrong for a shell that wants a read-only
// transcript rather than an "unavailable" card. pkg/web/webui/sessions' viewFor
// is the one caller and checks both itself.
func ViewFor(ctx context.Context, d Deps, ns, name string) (ViewProps, UIChromeRequest, Availability, *webui.PageError) {
	out, pe := walkAgentUIDoors(ctx, d, ns, name)
	if pe != nil {
		return ViewProps{}, UIChromeRequest{}, UIAvailable, pe
	}
	if out.NoUIDeclared {
		return ViewProps{}, UIChromeRequest{}, UINotDeclared, nil
	}
	doors := out.agentUIDoors

	v, pe := mergedViewFor(ctx, d, doors, ns, name)
	if pe != nil {
		return ViewProps{}, UIChromeRequest{}, UIAvailable, pe
	}

	// declarationWireFor is the ONLY producer of the browser-facing wire
	// document (see its own doc comment), so a hook the agent composed renders
	// identically on first paint and on every subsequent bindings, actions or
	// live read.
	//
	// answeredHooksFor is called with the SAME (ns, name) just passed to
	// mergedViewFor above — so its scope construction matches exactly the
	// scope mergedViewFor used to resolve v (and so fill v.ComposedAt), never
	// doors.Resolution.SessionName. The UI name rides along for its log line
	// only.
	answered := answeredHooksFor(ctx, d, ns, name, doors.UI.Name, v)
	declaration, err := json.Marshal(declarationWireFor(v, answered))
	if err != nil {
		d.Logger().Error(err, "agentui: marshal declaration failed", "ns", ns, "name", name, "ui", doors.UI.Name)
		return ViewProps{}, UIChromeRequest{}, UIAvailable, &webui.PageError{Status: http.StatusInternalServerError, Kind: "error",
			Title: "Rendering error", Message: "Could not render this agent's UI."}
	}

	var chrome UIChromeRequest
	if doors.UI.Spec.Chrome != nil {
		chrome.InitialState = doors.UI.Spec.Chrome.InitialState
	}

	return ViewProps{
		Ns: ns, Name: doors.Resolution.SessionName, Declaration: declaration,
		AgentParams: agentParamsFor(ctx, d, ns, doors.Resolution.SessionName, doors.UI.Name, v.Declaration),
	}, chrome, UIAvailable, nil
}

// agentParamsFor reads what the agent set this view's controls to, filtered to
// the parameters the served declaration actually declares.
//
// Best-effort by design: a page rendering at its author's defaults is a
// working page, and failing the whole view over an unreadable stored
// preference turns a cosmetic loss into a blank screen. The failure is LOGGED
// — what is skipped is the propagation, never the report.
//
// The browser applies the same filter again, and neither is redundant: this
// one stops a stale key from reaching the page at all, the browser's stops one
// arriving on a live push from being applied to a declaration that has since
// changed underneath it.
func agentParamsFor(ctx context.Context, d Deps, ns, name, ui string, decl uicomponents.Declaration) map[string]string {
	mem := d.Memory()
	if mem == nil {
		return nil
	}
	stored, err := uiviewparams.Get(ctx, mem, memory.Scope{Kind: "session", ID: ns + "/" + name}, ui)
	if err != nil {
		d.Logger().Info("agentui: could not read the agent's view parameters; serving the view at its declared defaults",
			"ns", ns, "name", name, "ui", ui, "err", err.Error())
		return nil
	}
	if len(stored) == 0 {
		return nil
	}
	known := uicomponents.ParamKeys(decl)
	out := make(map[string]string, len(stored))
	for k, val := range stored {
		if slices.Contains(known, k) {
			out[k] = val
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// declarationWire is the envelope @ap/agentui's TS `Declaration` mirror
// expects. Built from uicomponents types, not the CRD's own
// AgentUISlot/AgentUIAction, so the browser bootstrap and every server-side
// lookup read the SAME merged uicomponents.View — one document, not two
// independent marshals of it.
//
// View carries the WHOLE page tree, hooks included: the browser walks it
// itself (@ap/agentui's own Hooks-equivalent) to find the oap:generative
// nodes it renders as writable regions, rather than webd flattening them
// into a side list. AgentComposed is the one thing the tree cannot say for
// itself — a hook's CURRENT content looks identical whether it is the
// author's Tier-0 default or a fragment the agent wrote, and the persistent
// marker a viewer relies on (@ap/agentui's GenerativeHook) needs a name to
// key by.
type declarationWire struct {
	// Actions is the declaration's action table; absent when it declares none.
	Actions []uicomponents.Action `json:"actions,omitempty"`
	// View is the merged page tree, always set: uicomponents.Normalize
	// guarantees every declaration has one by the time it reaches here, so
	// there is no "no view" case to distinguish from a malformed document.
	View *uicomponents.Node `json:"view"`
	// AgentComposed lists, sorted, every hook a stored fragment successfully
	// changed on THIS read — a fill or a clear. Computed here from what
	// uiview.Resolve accepted, never stored anywhere an agent's own fragment
	// could set or clear it; absent when the agent has composed nothing.
	AgentComposed []string `json:"agentComposed,omitempty"`

	// Answered lists, sorted, every hook in AgentComposed whose fill predates
	// the viewer's latest visible message — DERIVED on THIS read from the
	// fill's WrittenAt (uiview.Resolve's View.ComposedAt) and the transcript
	// (answeredHooks), never stored and never something an agent's own
	// fragment could set. It is what makes a QuestionCard the viewer already
	// replied to render as sent again after a reload, instead of asking the
	// same question twice.
	Answered []string `json:"answered,omitempty"`
}

// declarationWireFor renders one resolved View, plus its separately-computed
// answered-hook list, as the wire document. It is the ONLY producer of that
// document — ViewFor's props and live.go's push both go through it, so a
// hook can never render one way on first paint and another after an update.
//
// answered is a parameter rather than computed inside this function because
// it needs mem/scope/ctx that a pure View does not carry; both callers reach
// it through the shared answeredHooksFor.
func declarationWireFor(v uicomponents.View, answered []string) declarationWire {
	return declarationWire{
		Actions:       v.Declaration.Actions,
		View:          v.Declaration.View,
		AgentComposed: v.AgentComposed,
		Answered:      answered,
	}
}

// resolveDeclaration is resolveView plus the "just the Declaration" shape
// bindings.go and actions.go both already depend on. Its signature is
// deliberately narrow: neither caller needs the resolved doors, only the
// document; see this file's package doc comment.
func resolveDeclaration(ctx context.Context, d Deps, ns, name string) (uicomponents.Declaration, *webui.PageError) {
	_, v, pe := resolveView(ctx, d, ns, name)
	if pe != nil {
		return uicomponents.Declaration{}, pe
	}
	return v.Declaration, nil
}
