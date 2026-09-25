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
	"github.com/authzed/openagentprimitives/pkg/web/uiview"
)

// SetViewParamsConfig carries what the capability resolved once, at session
// start, so the tool's description and schema are fixed for the turn while its
// validation stays live.
type SetViewParamsConfig struct {
	// View is the runner-side handle. A nil View is a wiring bug the
	// capability must have caught (it skips rather than offering an inert
	// tool); Execute still guards it, because an inert tool that reports
	// success is the worst of the available failures.
	View *uiview.Runtime

	// ParamKeys is the declaration's runtime parameter keys, read at session
	// start. It populates the description so the agent does not spend a turn
	// discovering them.
	//
	// It is NOT the authorization: Runtime.WriteParams re-reads the AgentUI
	// live and re-derives the legal set, because a bundle redeploy can remove
	// a control mid-session.
	ParamKeys []string
}

// NewSetViewParams builds the set_view_params tool.
func NewSetViewParams(cfg SetViewParamsConfig) tool.Tool {
	return &setViewParamsTool{cfg: cfg}
}

type setViewParamsTool struct {
	cfg SetViewParamsConfig
}

func (*setViewParamsTool) Name() string    { return "set_view_params" }
func (*setViewParamsTool) Kind() tool.Kind { return tool.KindMeta }

// Permission: like update_view, this mutates session-scoped presentation state
// already gated by AgentSession#interact, and reaches no external resource. It
// is deliberately not Stateless — the call does change observable state.
//
// Setting a parameter does cause tool calls to be made with different
// arguments, but it does not MAKE them: each binding is still resolved through
// the same per-resolve authorization under the VIEWER's own subject. The agent
// gains no reach it did not already have by calling the tool itself.
func (*setViewParamsTool) Permission() authz.Permission {
	return authz.Permission{StateImpact: authz.Passthrough}
}
func (*setViewParamsTool) PermissionVariants() []authz.PermissionVariant { return nil }

func (t *setViewParamsTool) Description() string {
	base := "Set the values of the controls on the UI a person is looking at — a date range, a filter — so the page " +
		"opens on what they asked for instead of on its defaults. Use this when someone has told you what they want to " +
		"see and the page has a control for it; do NOT use it to change what someone is already looking at unless they " +
		"asked. Values the person has set themselves always win over these. Pass the complete set you intend to drive: " +
		"a key you leave out is a control you are no longer driving, and it returns to the viewer."
	if len(t.cfg.ParamKeys) > 0 {
		base += " This view's parameters are: " + strings.Join(t.cfg.ParamKeys, ", ") + "."
	}
	return base
}

// InputSchema publishes the parameter keys as an ENUM of property names via
// a fixed map shape, rather than one named property per key.
//
// A map keyed by string is the honest schema: the keys are per-session facts,
// and an agent that names an unknown one gets a correctable error listing the
// legal set (uiview.UnknownParamError) rather than a schema violation it
// cannot read.
func (t *setViewParamsTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "params": {
      "type": "object",
      "description": "Parameter key to value. Keys must be this view's own parameters; values are plain strings, and a date is an RFC3339 instant (2026-02-01T00:00:00Z).",
      "additionalProperties": {"type": "string"}
    }
  },
  "required": ["params"]
}`)
}

type setViewParamsArgs struct {
	Params map[string]string `json:"params"`
}

func (t *setViewParamsTool) Execute(ctx context.Context, raw json.RawMessage, sess *tool.SessionContext) (tool.Result, error) {
	var a setViewParamsArgs
	if res, ok := tool.ParseArgs(raw, &a, t.Name(), `{"params":{"window.from":"2026-02-01T00:00:00Z"}}`); !ok {
		return res, nil
	}

	ns, name := "", ""
	if sess != nil {
		ns, name = sess.Namespace, sess.Name
	}

	if t.cfg.View == nil {
		slog.Info("set_view_params: called with no Runtime attached; this is a wiring bug — the capability should have skipped offering this tool",
			"namespace", ns, "name", name)
		return tool.Result{
			Content: "set_view_params: the view is not available for this session.",
			IsError: true, Trusted: true,
		}, nil
	}

	// An explicit empty map is meaningful — "I am no longer driving anything"
	// — and must reach WriteParams rather than being read as a malformed call.
	// nil (the key absent) means the same thing here, since the schema
	// requires the field: normalizing is kinder than refusing.
	if a.Params == nil {
		a.Params = map[string]string{}
	}

	if err := t.cfg.View.WriteParams(ctx, a.Params); err != nil {
		var uerr *uiview.UnknownParamError
		if errors.As(err, &uerr) {
			// Correctable by the agent in one turn, and the one place a
			// structured internal-shaped error is right: this is a tool result
			// the agent reads, not a browser surface.
			slog.Info("set_view_params: rejected an unknown parameter",
				"namespace", ns, "name", name, "key", uerr.Key)
			body, merr := json.Marshal(map[string]any{
				"error": "unknown_param", "key": uerr.Key, "known": uerr.Known,
			})
			if merr != nil {
				return tool.Result{Content: "set_view_params: " + uerr.Error(), IsError: true, Trusted: true}, nil
			}
			return tool.Result{Content: string(body), IsError: true, Trusted: true}, nil
		}
		// Anything else is infrastructure (the AgentUI could not be fetched,
		// the memory backend errored) rather than a correctable mistake.
		// Logged in full for an operator; the agent gets a generic message so
		// no CRD/K8s-shaped detail reaches a surface that may be quoted to a
		// user.
		slog.Info("set_view_params: write failed", "namespace", ns, "name", name, "err", err.Error())
		return tool.Result{
			Content: "set_view_params: unable to set the view's controls right now; try again.",
			IsError: true, Trusted: true,
		}, nil
	}

	body, err := json.Marshal(map[string]any{"params": a.Params, "updated": true})
	if err != nil {
		return tool.Result{Trusted: true}, fmt.Errorf("set_view_params: marshal confirmation: %w", err)
	}
	return tool.Result{Content: string(body), Trusted: true}, nil
}
