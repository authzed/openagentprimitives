package meta

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/web/uicomponents"
	"github.com/authzed/openagentprimitives/pkg/web/uiview"
)

// ReadViewConfig is deliberately one field. See NewReadView.
type ReadViewConfig struct {
	View *uiview.Runtime
}

// NewReadView builds the read_view tool: what the human in front of this UI
// is currently being shown, in declaration form.
//
// It returns the MERGED declaration — Tier 0 with the agent's own accepted
// fragments applied — because the agent's next update_view has to be
// coherent with what is actually on screen, and because a Tier-0 hook the
// agent never wrote is exactly as much a part of "what the viewer sees" as
// one it did.
//
// It returns NO binding VALUES, and only ONE of the four registered binding
// sources is held out by the TYPE SYSTEM — worth stating precisely, since a
// comment claiming more than holds is worse than none:
//
//   - tool — STRUCTURALLY unreachable: its transport is a
//     channelevents.RequestFunc that ReadViewConfig does not carry and Runtime
//     cannot supply.
//   - memory and action — reachable from Runtime.Mem alone.
//   - artifact — its METADATA is reachable from Runtime.Mem; only the raw render
//     BYTES need a func nothing here has.
//
// For three of the four the guarantee therefore rests on DISCIPLINE: this file
// must never query Runtime.Mem. Do NOT add a values field from any source — the
// agent gets numbers by calling the tool itself under the ordinary per-turn
// budget, and a cached readback would be a second, unmetered data-into-context
// path. Widening ReadViewConfig to make one convenient is the exact mistake this
// type's narrow shape prevents.
//
// TestReadViewResultCarriesNoOtherFieldByType pins the payload's field set over
// the TYPE, so a new field fails whether or not anything populates it;
// TestReadViewResolvesNoBindingFromAnySource plants resolvable content behind
// every reachable source and asserts none of it reaches the payload.
func NewReadView(cfg ReadViewConfig) tool.Tool {
	return &readViewTool{cfg: cfg}
}

type readViewTool struct{ cfg ReadViewConfig }

func (*readViewTool) Name() string    { return "read_view" }
func (*readViewTool) Kind() tool.Kind { return tool.KindMeta }

// Permission: read_view observes session-scoped state it already holds a
// handle to (via Runtime) and mutates nothing.
func (*readViewTool) Permission() authz.Permission {
	return authz.Permission{StateImpact: authz.Stateless}
}
func (*readViewTool) PermissionVariants() []authz.PermissionVariant { return nil }

func (*readViewTool) Description() string {
	return "Call this FIRST — before your first update_view in a session, and again after anything you did not write " +
		"may have changed the page. It returns the page the person is looking at as JSX (`jsx`): the author's tree with " +
		"every oap:generative hook in place, each hook's current content inside it, your own fills marked, and empty " +
		"hooks marked empty. Alongside it: the same page in the node grammar (`declaration`), which hooks carry your " +
		"writes, the current value of each on-screen control's binding parameter, and any of your writes no longer " +
		"being served (with the reason). It carries NO bound data — no tool results, no memory entries, no artifact " +
		"bytes. If you need the current numbers, call the binding's own tool yourself."
}

func (*readViewTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{}}`)
}

// readViewResult is the tool's entire result payload — one bounded JSON
// object, bounded by the same Options.MaxNodes/MaxDepth every declaration is
// bounded by. Exactly these five fields, never a sixth carrying resolved
// data — see NewReadView's doc comment for what that guarantee does and does
// not rest on. Pinned twice, deliberately: over this TYPE
// (TestReadViewResultCarriesNoOtherFieldByType, which an `omitempty` field
// cannot hide from) and over the marshalled bytes
// (TestReadViewPayloadCarriesNoOtherFields, which a non-struct-field key
// cannot hide from).
type readViewResult struct {
	Declaration   uicomponents.Declaration  `json:"declaration"`
	AgentComposed []string                  `json:"agentComposed"`
	Parameters    []uicomponents.ParamState `json:"parameters"`
	Rejected      []uicomponents.Rejection  `json:"rejected"`
	// JSX is Declaration printed as JSX (uicomponents.JSX) with AgentComposed
	// marked — the form the agent reads; the declaration above is the same
	// page for anything that wants nodes. Derived from the declaration only,
	// so it carries exactly as much data as the declaration does: none.
	JSX string `json:"jsx"`
}

func (t *readViewTool) Execute(ctx context.Context, _ json.RawMessage, sess *tool.SessionContext) (tool.Result, error) {
	ns, name := "", ""
	if sess != nil {
		ns, name = sess.Namespace, sess.Name
	}

	if t.cfg.View == nil {
		slog.Info("read_view: called with no Runtime attached; this is a wiring bug — the capability should have skipped offering this tool",
			"namespace", ns, "name", name)
		return tool.Result{
			Content: "read_view: the view-model is not available for this session.",
			IsError: true, Trusted: true,
		}, nil
	}

	view, err := t.cfg.View.View(ctx)
	if err != nil {
		slog.Info("read_view: failed", "namespace", ns, "name", name, "err", err.Error())
		return tool.Result{
			Content: "read_view: unable to read the view right now; try again.",
			IsError: true, Trusted: true,
		}, nil
	}

	out := readViewResult{
		Declaration:   view.Declaration,
		AgentComposed: view.AgentComposed,
		Parameters:    uicomponents.ParamStates(view.Declaration),
		Rejected:      view.Rejected,
		JSX:           uicomponents.JSX(view.Declaration, view.AgentComposed),
	}

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	// The JSX is full of `<` and `>`; Go's default would hand the agent
	// `\u003c`. This payload is a tool result, never a browser surface.
	enc.SetEscapeHTML(false)
	if err := enc.Encode(out); err != nil {
		return tool.Result{Trusted: true}, fmt.Errorf("read_view: marshal result: %w", err)
	}
	return tool.Result{Content: strings.TrimSuffix(buf.String(), "\n"), Trusted: true}, nil
}
