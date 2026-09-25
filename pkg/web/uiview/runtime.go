package uiview

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/uiviewmodel"
	"github.com/authzed/openagentprimitives/pkg/web/uicomponents"
)

// Runtime is the runner-side handle the update_view and read_view tools draw
// on. It is a CONCRETE POINTER on capability.RunnerEnv, deliberately: a nil
// check on it means what it says, with no typed-nil interface to hide behind.
type Runtime struct {
	Namespace string
	Session   string // AgentSession name; the memory scope is "<Namespace>/<Session>"
	UIName    string // AgentUI CR name

	Client client.Client
	Mem    memory.Memory // declared as the interface at every construction site

	// ToolOptions supplies the grant maps uicomponents.Validate is run with.
	// A FUNC, not a value, because the runner materializes Loop.AppTools
	// AFTER capability.Assemble has built this Runtime; runner.AttachUIView
	// is the only thing that sets it.
	//
	// nil is FAIL-CLOSED and LOUD: Options() returns DefaultOptions() — zero
	// granted tools, so no fragment carrying a tool binding can be written —
	// and logs, so a wiring regression is found rather than presenting as
	// "the agent's tables stopped working".
	ToolOptions func() uicomponents.Options

	// Publish delivers one agent-UI push envelope (ui_view_update, on a
	// successful Write) to webd's live route. A FUNC, so the nil check is
	// honest, and NIL-TOLERATED: a kubectl-driven session has no NATS and
	// must still compose its UI — uiviewmodel.Record's write is durable and
	// the next page load resolves it with no push. A nil or failing Publish
	// LOGS at INFO and returns nil rather than failing the write, since the
	// memory write already succeeded and reporting failure to the agent
	// would invite it to write again for no reason.
	Publish func(ctx context.Context, env channelevents.Envelope) error
}

// scope is the memory.Scope every Runtime method reads/writes under —
// derived, never stored, so it can never drift from Namespace/Session.
func (r *Runtime) scope() memory.Scope {
	return memory.Scope{Kind: "session", ID: r.Namespace + "/" + r.Session}
}

// Base re-reads the AgentUI CR LIVE and converts it. Deliberately not a
// snapshot taken at session start: a bundle redeploy changes the hook set and
// the action table under a running session, and validating a write against a
// stale document would accept a fragment for a hook that no longer exists.
// The tool's DESCRIPTION is built from a start-of-session snapshot (it has to
// be — a description is fixed for the turn); its VALIDATION is not.
func (r *Runtime) Base(ctx context.Context) (*spiceboxv1alpha1.AgentUI, error) {
	var ui spiceboxv1alpha1.AgentUI
	if err := r.Client.Get(ctx, client.ObjectKey{Namespace: r.Namespace, Name: r.UIName}, &ui); err != nil {
		return nil, fmt.Errorf("uiview: get AgentUI %s/%s: %w", r.Namespace, r.UIName, err)
	}
	return &ui, nil
}

// View is Base + Resolve: what the viewer is currently looking at.
func (r *Runtime) View(ctx context.Context) (uicomponents.View, error) {
	ui, err := r.Base(ctx)
	if err != nil {
		return uicomponents.View{}, err
	}
	return Resolve(ctx, r.Mem, r.scope(), ui, r.Options())
}

// Write validates a proposed fill IN CONTEXT and, only if the merged
// declaration is legal, records it. See apply.
func (r *Runtime) Write(ctx context.Context, hook string, node uicomponents.Node) (uicomponents.View, error) {
	return r.apply(ctx, uicomponents.Fragment{Hook: hook, Node: &node})
}

// Clear collapses a hook to an intentional empty. It is a write in every
// respect Write is — validated in context, recorded durably, pushed — and
// the author default does not return until the session's records do: List
// sees the same one record a fill would have left, Cleared instead of a
// parseable Node, and AgentComposed names the hook exactly as a fill would.
// The only difference from Write is WHAT replaces the hook's children:
// nothing, rather than a fill.
func (r *Runtime) Clear(ctx context.Context, hook string) (uicomponents.View, error) {
	return r.apply(ctx, uicomponents.Fragment{Hook: hook, Node: nil})
}

// apply is Write and Clear's shared body: validate f IN CONTEXT and, only if
// the merged declaration is legal, record it.
//
// "In context" is the whole contract: the proposal is appended to the fragments
// already stored, and the WHOLE merged declaration is validated. A proposal
// checked in isolation would miss every cross-hook fact — Action.Inputs
// disjointness against ParamKeys above all.
//
// Returns the merged View on success. On rejection it returns a
// *uicomponents.ValidationError the agent can correct from, and writes NOTHING.
func (r *Runtime) apply(ctx context.Context, f uicomponents.Fragment) (uicomponents.View, error) {
	if r.Mem == nil {
		return uicomponents.View{}, fmt.Errorf("uiview: apply: no memory backend for %s/%s", r.Namespace, r.Session)
	}

	ui, err := r.Base(ctx)
	if err != nil {
		return uicomponents.View{}, err
	}
	base, err := DeclarationFromSpec(ui)
	if err != nil {
		return uicomponents.View{}, fmt.Errorf("uiview: apply: convert %s: %w", ui.Name, err)
	}

	scope := r.scope()
	stored, err := uiviewmodel.List(ctx, r.Mem, scope, r.UIName)
	if err != nil {
		return uicomponents.View{}, fmt.Errorf("uiview: apply: list fragments for %s: %w", r.UIName, err)
	}

	fragments, parseRejections := loadFragments(stored)
	// The proposal REPLACES any stored fragment for the same hook: a second
	// Write (or a Clear) to one hook must leave exactly one fragment naming
	// it, or ResolveView sees two candidates with no rule for which wins. The
	// replacement also retires a parse rejection for THAT hook — this write
	// overwrites the record that would not parse, so reporting it would call
	// a hook broken at the moment it stops being.
	fragments = slices.DeleteFunc(fragments, func(fr uicomponents.Fragment) bool { return fr.Hook == f.Hook })
	parseRejections = slices.DeleteFunc(parseRejections, func(rej uicomponents.Rejection) bool { return rej.Hook == f.Hook })
	fragments = append(fragments, f)

	// Logged HERE, before the acceptance check below, because that check can
	// return early with a ValidationError and drop the View entirely — and a
	// stored fragment that no longer parses is a hook silently serving Tier 0
	// while the agent believes it composed one, on either path.
	for _, rej := range parseRejections {
		slog.Default().Info("uiview: a stored fragment no longer parses; that hook is serving its Tier-0 default",
			"namespace", r.Namespace, "session", r.Session, "ui", r.UIName, "hook", rej.Hook, "reason", rej.Reason)
	}

	view := uicomponents.ResolveView(base, fragments, r.Options())
	// Carried back on the View, not only logged: read_view hands View.Rejected
	// to the agent, which is the only channel that can prompt it to rewrite a
	// hook it believes is still live. Sorted because a list whose order varied
	// per call is a log an operator cannot diff.
	view.Rejected = append(view.Rejected, parseRejections...)
	slices.SortFunc(view.Rejected, func(a, b uicomponents.Rejection) int { return strings.Compare(a.Hook, b.Hook) })

	if !slices.Contains(view.AgentComposed, f.Hook) {
		reason := fmt.Sprintf("hook %q was not accepted", f.Hook)
		for _, rej := range view.Rejected {
			if rej.Hook == f.Hook {
				reason = rej.Reason
				break
			}
		}
		return uicomponents.View{}, &uicomponents.ValidationError{Path: fmt.Sprintf("hooks[%s]", f.Hook), Reason: reason}
	}

	c := uiviewmodel.Content{UI: r.UIName, Slot: f.Hook, WrittenAt: time.Now().UTC()}
	if f.Node == nil {
		c.Cleared = true
	} else {
		raw, err := json.Marshal(*f.Node)
		if err != nil {
			return uicomponents.View{}, fmt.Errorf("uiview: apply: marshal accepted node for hook %q: %w", f.Hook, err)
		}
		c.Node = raw
	}
	if err := uiviewmodel.Record(ctx, r.Mem, scope, c); err != nil {
		return uicomponents.View{}, err
	}

	// Publish AFTER the durable write, never before: a push that preceded it
	// would let a browser observe a state no reconnect could reproduce if the
	// write then failed.
	r.publish(ctx, f.Hook)

	return view, nil
}

// publish builds and sends one ui_view_update envelope for hook, logging
// (never failing the write) on every non-happy path — see Publish's own doc
// comment for why a nil or failing Publish must not turn a successful Write
// into an error.
func (r *Runtime) publish(ctx context.Context, hook string) {
	if r.Publish == nil {
		slog.Default().Info("uiview: Publish not attached; ui_view_update push skipped (the write itself already succeeded)",
			"namespace", r.Namespace, "session", r.Session, "ui", r.UIName, "hook", hook)
		return
	}
	env, err := channelevents.BuildEnvelope(r.Namespace, r.Session, channelevents.KindUIViewUpdate,
		channelevents.UIViewUpdatePayload{Hook: hook, UpdatedAt: time.Now().UTC()})
	if err != nil {
		slog.Default().Info("uiview: build ui_view_update envelope failed",
			"namespace", r.Namespace, "session", r.Session, "ui", r.UIName, "hook", hook, "err", err.Error())
		return
	}
	if err := r.Publish(ctx, env); err != nil {
		slog.Default().Info("uiview: ui_view_update push failed",
			"namespace", r.Namespace, "session", r.Session, "ui", r.UIName, "hook", hook, "err", err.Error())
	}
}

// Options returns ToolOptions()' result, or the fail-closed default when no
// collaborator has been attached (see ToolOptions' doc comment).
func (r *Runtime) Options() uicomponents.Options {
	if r.ToolOptions == nil {
		slog.Default().Info("uiview: Runtime.Options called with no ToolOptions attached; granting zero tools",
			"namespace", r.Namespace, "session", r.Session, "ui", r.UIName)
		return uicomponents.DefaultOptions()
	}
	return r.ToolOptions()
}
