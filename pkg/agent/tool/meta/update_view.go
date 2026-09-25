package meta

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/web/uicomponents"
	"github.com/authzed/openagentprimitives/pkg/web/uiview"
)

// UpdateViewConfig carries what the capability resolved once, at session
// start, so the tool's DESCRIPTION and SCHEMA are fixed for the turn while
// its VALIDATION stays live.
type UpdateViewConfig struct {
	// View is the runner-side handle. A nil View is a wiring bug the
	// capability must have caught (it skips rather than offering an inert
	// tool); Execute still guards it, because an inert tool that reports
	// success is the worst of the available failures.
	View *uiview.Runtime

	// Hooks is the page's hooks, in document order, read from the AgentUI at
	// session start — each with its author's intent and allowlist. The names
	// populate the `hook` property's enum and the lines (HookLine) populate
	// its description, which is what stops the agent from spending a turn
	// discovering either. It is NOT the authorization: Runtime.Write/Clear
	// re-read the CR live and re-derive the page's hooks, because a bundle
	// redeploy can add, remove, or narrow one mid-session.
	Hooks []uicomponents.Hook
}

// NewUpdateView builds the update_view tool.
//
// The input schema is assembled here rather than returned as a constant
// because two of its parts are per-session or per-build facts: the hook
// enum, and the component vocabulary, which comes from
// uicomponents.VocabularySchema() — i.e. from the registry, so a component
// added to the vocabulary is published to the agent by its registration
// alone. A transcribed list here would go stale on the first new component
// and the agent would be told a legal component is illegal.
func NewUpdateView(cfg UpdateViewConfig) tool.Tool {
	return &updateViewTool{cfg: cfg, schema: buildUpdateViewSchema(cfg.Hooks)}
}

type updateViewTool struct {
	cfg    UpdateViewConfig
	schema json.RawMessage
}

func (*updateViewTool) Name() string    { return "update_view" }
func (*updateViewTool) Kind() tool.Kind { return tool.KindMeta }

// Permission: update_view mutates session-scoped presentation state that is
// already gated by AgentSession#interact, and it reaches no external
// resource — the same reasoning update_plan records for the plans registry.
// It is deliberately not Stateless: this call does change observable state.
func (*updateViewTool) Permission() authz.Permission {
	return authz.Permission{StateImpact: authz.Passthrough}
}
func (*updateViewTool) PermissionVariants() []authz.PermissionVariant { return nil }

func (*updateViewTool) Description() string {
	return "Fill or clear one generative hook of the page the person is looking at. Target a hook by name; its intent " +
		"(listed under the hook property) is the standing instruction for that region, and each hook admits only the " +
		"components its author allowed — a fill using anything else is rejected with a structured reason naming the " +
		"allowed set, and nothing is written until the whole page validates. Repaint a hook to replace its content: a " +
		"timeline advances by repainting it with the next step active; a question is asked by placing an ap:question in " +
		"a hook, then waiting for the answer. `clear: true` collapses a hook whose moment has passed — the author's " +
		"default does not return."
}

func (t *updateViewTool) InputSchema() json.RawMessage { return t.schema }

// updateViewSchema* mirror the wire shape update_view's input schema is
// published as. Separate small structs rather than one nested literal so the
// hook enum and the registry-derived component vocabulary can be slotted in
// without hand-building JSON.
type updateViewHookSchema struct {
	Type        string   `json:"type"`
	Enum        []string `json:"enum"`
	Description string   `json:"description"`
}

type updateViewNodeSchema struct {
	Type        string `json:"type"`
	Description string `json:"description"`
}

type updateViewClearSchema struct {
	Type        string `json:"type"`
	Description string `json:"description"`
}

type updateViewSchemaProps struct {
	Hook  updateViewHookSchema  `json:"hook"`
	Node  updateViewNodeSchema  `json:"node"`
	Clear updateViewClearSchema `json:"clear"`
}

type updateViewInputSchema struct {
	Type                 string                     `json:"type"`
	AdditionalProperties bool                       `json:"additionalProperties"`
	Properties           updateViewSchemaProps      `json:"properties"`
	Required             []string                   `json:"required"`
	Defs                 map[string]json.RawMessage `json:"$defs,omitempty"`
}

// updateViewNodeDescription is what the AGENT reads to learn a node's shape.
// It is prompt text on every turn this tool is offered, so it is terse and
// exact: a vague binding description costs a correction round-trip per
// malformed selector validateBindings rejects.
//
// The selector sentence is here rather than in $defs.components because a
// binding is not a component — VocabularySchema publishes component PROPS,
// and this is the only channel that describes the binding wrapper itself.
const updateViewNodeDescription = "The fill for the hook. Its top-level component, and every component beneath it, must be in that hook's allowed set (see hook). " +
	"One component node: {component, props, bindings, children}. " +
	"Legal component types and their props are under $defs.components. " +
	"A binding is {source, ref, args, select}. " +
	"select is an optional path into the source's response: dot-separated keys, with [] after a key to iterate that array, e.g. \"results[].properties\". " +
	"Omit select to bind the whole response; use it when the response wraps the data in an envelope."

// describeHooks is the `hook` property's description: which region, by name,
// and — one line per hook, in page order — what each is for and what it
// admits. The same lines the prompt section prints (HookLine), so the schema
// and the section never disagree.
func describeHooks(hooks []uicomponents.Hook) string {
	var b strings.Builder
	b.WriteString("Which region of the page to change, by name. The page's hooks, in order:")
	for _, h := range hooks {
		b.WriteString("\n- " + HookLine(h))
	}
	return b.String()
}

// buildUpdateViewSchema assembles update_view's input schema from the hook
// enum and the live component vocabulary. If uicomponents.VocabularySchema
// fails (a malformed component registration — see its own doc comment), the
// schema is still emitted, just without `$defs.components`: a tool with an
// unparseable input schema is a tool the provider may reject for the whole
// turn, which is worse than one whose schema is merely less helpful.
func buildUpdateViewSchema(hooks []uicomponents.Hook) json.RawMessage {
	names := make([]string, len(hooks))
	for i, h := range hooks {
		names[i] = h.Name
	}

	s := updateViewInputSchema{
		Type:                 "object",
		AdditionalProperties: false,
		Required:             []string{"hook"},
		Properties: updateViewSchemaProps{
			Hook: updateViewHookSchema{
				Type:        "string",
				Enum:        names,
				Description: describeHooks(hooks),
			},
			Node: updateViewNodeSchema{
				Type:        "object",
				Description: updateViewNodeDescription,
			},
			Clear: updateViewClearSchema{
				Type:        "boolean",
				Description: "Collapse the hook to nothing (an intentional empty). The author's default does not return. Exactly one of node and clear.",
			},
		},
	}

	vocab, err := uicomponents.VocabularySchema()
	if err != nil {
		slog.Info("update_view: VocabularySchema failed; publishing schema without $defs.components", "err", err.Error())
	} else {
		s.Defs = map[string]json.RawMessage{"components": vocab}
	}

	raw, err := json.Marshal(s)
	if err != nil {
		// json.Marshal on this struct can only fail if a field's MarshalJSON
		// panics or errors — none of ours do — so this is a defensive log, not
		// an expected path. Fall back to the schema with no vocabulary at all
		// rather than a nil InputSchema, which would be worse for the same
		// reason a VocabularySchema failure is handled above.
		slog.Info("update_view: marshal input schema failed", "err", err.Error())
		return json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{"hook":{"type":"string"},"node":{"type":"object"},"clear":{"type":"boolean"}},"required":["hook"]}`)
	}
	return raw
}

type updateViewArgs struct {
	// Hook names a generative hook in the AgentUI's page tree.
	Hook string `json:"hook"`
	// Node is the component subtree to place in Hook, validated against the
	// declaration before it is written. Exactly one of Node and Clear is set.
	Node json.RawMessage `json:"node"`
	// Clear collapses Hook to an intentional empty instead of filling it.
	// Exactly one of Node and Clear is set.
	Clear bool `json:"clear"`
}

func (t *updateViewTool) Execute(ctx context.Context, raw json.RawMessage, sess *tool.SessionContext) (tool.Result, error) {
	var a updateViewArgs
	if res, ok := tool.ParseArgs(raw, &a, t.Name(), `{"hook":"brief","node":{"component":"ap:markdown","props":{"body":"…"}}}`); !ok {
		return res, nil
	}

	// Exactly one of node and clear: a call carrying both says two different
	// things about one hook, and a call carrying neither says nothing at all.
	hasNode := len(a.Node) > 0
	if hasNode == a.Clear {
		return tool.Result{Content: "update_view: pass exactly one of node and clear", IsError: true, Trusted: true}, nil
	}

	ns, name := "", ""
	if sess != nil {
		ns, name = sess.Namespace, sess.Name
	}

	if t.cfg.View == nil {
		slog.Info("update_view: called with no Runtime attached; this is a wiring bug — the capability should have skipped offering this tool",
			"namespace", ns, "name", name)
		return tool.Result{
			Content: "update_view: the view-model is not available for this session.",
			IsError: true, Trusted: true,
		}, nil
	}

	// The returned View is unused here: read_view is the one channel that
	// reports what a hook now holds, so update_view's own confirmation stays
	// the small {hook, updated|cleared} body regardless of what apply
	// returns.
	var applyErr error
	if a.Clear {
		_, applyErr = t.cfg.View.Clear(ctx, a.Hook)
	} else {
		node, perr := uicomponents.ParseNode(a.Node)
		if perr != nil {
			return tool.Result{
				Content: fmt.Sprintf("update_view: %s", perr.Error()),
				IsError: true, Trusted: true,
			}, nil
		}
		_, applyErr = t.cfg.View.Write(ctx, a.Hook, node)
	}

	if applyErr != nil {
		var verr *uicomponents.ValidationError
		if errors.As(applyErr, &verr) {
			// The spec's "rejected with a structured error the agent can correct
			// from; nothing written; logged" — nothing was written (Write/Clear
			// validate before they record), and this is the one place a
			// structured internal-shaped error is correct: it is a tool result
			// read by the agent, not a browser surface.
			slog.Info("update_view: rejected", "namespace", ns, "name", name, "hook", a.Hook, "path", verr.Path, "reason", verr.Reason)
			body, merr := json.Marshal(map[string]string{
				"error": "invalid_view", "hook": a.Hook, "path": verr.Path, "reason": verr.Reason,
			})
			if merr != nil {
				slog.Info("update_view: marshal rejection failed", "namespace", ns, "name", name, "hook", a.Hook, "err", merr.Error())
				return tool.Result{Content: "update_view: " + verr.Reason, IsError: true, Trusted: true}, nil
			}
			return tool.Result{Content: string(body), IsError: true, Trusted: true}, nil
		}
		// Anything else is an unexpected infrastructure failure (the AgentUI
		// couldn't be fetched, the memory backend errored) rather than a
		// correctable fragment mistake. Logged in full for an operator; the
		// agent gets a generic message so no CRD/K8s-shaped detail reaches a
		// surface that may end up quoted back to a user.
		slog.Info("update_view: write failed", "namespace", ns, "name", name, "hook", a.Hook, "err", applyErr.Error())
		return tool.Result{
			Content: "update_view: unable to update the view right now; try again.",
			IsError: true, Trusted: true,
		}, nil
	}

	confirmation := map[string]any{"hook": a.Hook, "updated": true}
	if a.Clear {
		confirmation = map[string]any{"hook": a.Hook, "cleared": true}
	}
	body, err := json.Marshal(confirmation)
	if err != nil {
		return tool.Result{Trusted: true}, fmt.Errorf("update_view: marshal confirmation: %w", err)
	}
	return tool.Result{Content: string(body), Trusted: true}, nil
}
