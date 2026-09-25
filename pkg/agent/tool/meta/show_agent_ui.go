package meta

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
)

// ShowAgentUIConfig wires show_agent_ui to the outbound publisher and to the
// viewer authorization check. Both are required; the capability that offers
// this tool declines to offer it when either is missing, rather than
// constructing a tool that cannot work.
type ShowAgentUIConfig struct {
	NATSPublish func(ctx context.Context, subject string, payload []byte) error

	// EnvelopeSigner signs every envelope show_agent_ui publishes with the
	// session's identity key. Nil-safe: a nil signer leaves the envelope
	// unsigned (test fixtures without a signer still work).
	EnvelopeSigner *channelevents.EnvelopeSigner

	// ViewerCanInteract reports whether the participant the agent is
	// currently answering may interact with this session. A func rather than
	// a checker interface + a subject, because resolving "who is speaking
	// now" reads live Loop state that only the runner can see, and because a
	// nil func is an honest nil (an interface field holding a typed-nil
	// pointer would not be). Any error is a refusal, never an allow.
	ViewerCanInteract func(ctx context.Context) (bool, error)
}

// NewShowAgentUI builds show_agent_ui: the tool that hands the user a button
// opening this session's own dashboard page in their browser.
//
// The name carries no "modal" and must not gain one: a Slack modal hosts Block
// Kit blocks and nothing else, so an HTTP-served page cannot live inside one. A
// name implying otherwise would point the next reader — and the model choosing
// between tools — at an implementation that exists on no channel. This tool
// offers a link OUT to a browser, which is why it behaves the same on every
// non-browser transport.
//
// It publishes an offer and stops: it never learns whether a channel rendered
// it, and re-checks nothing about the channel. Whether the transport can render
// an offer, and whether the session has a dashboard at all, are settled when the
// tool is OFFERED, so the model never sees a tool its channel cannot render. The
// one thing decided at call time is whether the person being replied to may open
// the page, which Description() warns the model about.
func NewShowAgentUI(cfg ShowAgentUIConfig) tool.Tool {
	return &showAgentUITool{cfg: cfg}
}

type showAgentUITool struct{ cfg ShowAgentUIConfig }

func (*showAgentUITool) Name() string    { return "show_agent_ui" }
func (*showAgentUITool) Kind() tool.Kind { return tool.KindMeta }

// Permission: the call changes observable state — a message appears in the
// channel — but maps to no SpiceDB-keyed resource, exactly artifact_offer_view's
// classification. The authorization that matters here is the viewer interact
// check inside Execute, not a per-tool resource check.
func (*showAgentUITool) Permission() authz.Permission {
	return authz.Permission{StateImpact: authz.Passthrough}
}
func (*showAgentUITool) PermissionVariants() []authz.PermissionVariant { return nil }

func (*showAgentUITool) Description() string {
	return "Offer the user this conversation's dashboard — a button that opens the session's own page in their browser, " +
		"which keeps reflecting the session as it goes on. The user must click it to open it: you cannot open it for them, " +
		"and calling this tool sends the offer and nothing more, so never tell them you have opened or shown them anything. " +
		"Takes no arguments — the dashboard is this conversation's, and there is no other one to name. " +
		"It will not work for someone who is not allowed to open this session's pages; in that case it refuses rather than " +
		"sending a link that would fail for them."
}

// InputSchema takes no arguments at all: the session is the tool's own
// context, and accepting a session id from the model would only be a
// parameter the tool then has to refuse to honour.
func (*showAgentUITool) InputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{}}`)
}

// Execute publishes the offer, gating on the viewer's interact permission
// first. The raw argument is ignored, as read_view's is — the schema accepts
// nothing.
func (t *showAgentUITool) Execute(ctx context.Context, _ json.RawMessage, sess *tool.SessionContext) (tool.Result, error) {
	if t.cfg.NATSPublish == nil {
		return tool.Result{Trusted: true}, fmt.Errorf("show_agent_ui: NATSPublish is unset")
	}

	ns, name := "", ""
	if sess != nil {
		ns, name = sess.Namespace, sess.Name
	}
	if ns == "" || name == "" {
		// An offer built from a half-identified session would address nobody:
		// there would be no page for the link to open and nowhere for the
		// offer to be routed.
		slog.Info("show_agent_ui: refused — the session context carries no namespace/name",
			"namespace", ns, "name", name)
		return tool.Result{
			Content: "show_agent_ui: this session could not be identified, so no dashboard offer was sent.",
			IsError: true, Trusted: true,
		}, nil
	}

	if t.cfg.ViewerCanInteract == nil {
		slog.Info("show_agent_ui: called with no viewer permission check attached; this is a wiring bug — the capability should not have offered this tool",
			"namespace", ns, "name", name)
		return tool.Result{
			Content: "show_agent_ui: the dashboard offer is not available for this session.",
			IsError: true, Trusted: true,
		}, nil
	}

	allowed, err := t.cfg.ViewerCanInteract(ctx)
	if err != nil {
		// Indeterminate, not denied — and the two are very different to
		// diagnose, so the cause goes to the log while the model gets a
		// refusal it can act on.
		slog.Info("show_agent_ui: refused — could not determine whether the current speaker may open this session",
			"namespace", ns, "name", name, "err", err.Error())
		return tool.Result{
			Content: "show_agent_ui: could not confirm that the person you are replying to is allowed to open this session's dashboard, so no offer was sent. Do not tell them a link is on its way.",
			IsError: true, Trusted: true,
		}, nil
	}
	if !allowed {
		// A plain answer rather than a failure: nothing here is an operator's
		// problem, so there is nothing to log.
		return tool.Result{
			Content: "show_agent_ui: the person you are replying to is not allowed to open this session's dashboard, so the link would not work for them and no offer was sent.",
			IsError: true, Trusted: true,
		}, nil
	}

	publish := func(subject string, data []byte) error {
		return publishWithRetry(ctx, t.cfg.NATSPublish, subject, data)
	}
	if err := t.cfg.EnvelopeSigner.PublishOut(publish, ns, name,
		channelevents.KindAgentUIOffer,
		// The ref is built here, from the session context, and never from the
		// model's arguments — which is also why the schema accepts none.
		channelevents.AgentUIOfferPayload{SessionRef: ns + "/" + name},
	); err != nil {
		// The cause goes to the log and the model gets fixed copy, the same
		// split the four gates above use. A transport error is not a string
		// this tool controls: NATS names the subject it failed on, so
		// interpolating it here would put the session's routing into the
		// model's context — and from there into whatever the model says next.
		// Nothing in it is actionable to the model anyway; the only useful
		// answer is "it did not go out".
		slog.Info("show_agent_ui: refused — publishing the dashboard offer failed",
			"namespace", ns, "name", name, "err", err.Error())
		return tool.Result{
			Content: "show_agent_ui: the dashboard offer could not be sent. Do not tell the user a link is on its way; you can try again.",
			IsError: true, Trusted: true,
		}, nil
	}

	// "sent", never "delivered" or "shown": a channel kind with no
	// agent_ui_offer sender drops the envelope with a log, so anything
	// stronger would be a promise this tool cannot keep — and the model
	// repeats what it is told here to the user.
	return tool.Result{Content: "dashboard offer sent — the user can click to open it", Trusted: true}, nil
}
