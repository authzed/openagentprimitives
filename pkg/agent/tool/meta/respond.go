package meta

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/authzed/openagentprimitives/pkg/agent/session/state/deliveries"
	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/sandbox"
	"github.com/authzed/openagentprimitives/pkg/agent/toolenvelope"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/authz/guardian/leakage"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	chregistry "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/registry"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/platform/artifacts"
)

// defaultMaxAttachments caps the number of artifact handles a single
// respond_to_user call may carry. Surfaced to the schema as `maxItems`
// so the model sees the limit; enforced server-side too.
const defaultMaxAttachments = 10

// RespondConfig wires respond_to_user to the runner's NATS publisher and
// memory client. Capabilities and ChannelKind shape the JSON schema and
// description so the model sees an honest description of what's supported.
type RespondConfig struct {
	Capabilities      []string // {"text","markdown","asset:text/html",...}
	ChannelKind       string   // "fake", "slack", etc.
	NATSPublish       func(ctx context.Context, subject string, payload []byte) error
	NATSSubjectPrefix string // e.g., "ap.session.default.foo"
	AppendSystemNote  func(ctx context.Context, content map[string]any) error

	// EnvelopeSigner signs every envelope respond_to_user publishes with the
	// session's identity key. Nil-safe: a nil signer leaves the envelope
	// unsigned (test fixtures without a signer still work).
	EnvelopeSigner *channelevents.EnvelopeSigner

	// Client is the controller-runtime client used to resolve
	// artifact handles passed via the `attached` field. Required when
	// Capabilities surfaces any `asset:*` entry; otherwise unused.
	Client client.Client

	// Artifacts resolves logical/tag attachment handles (artifact-…,
	// artifact-…#tag, artrev-…) to the ArtifactRender CR name before the
	// ownership + Ready checks. Nil falls back to treating every handle
	// as a literal CR name (pre-versioning behavior).
	Artifacts *artifacts.Service

	// MaxAttachments caps how many handles a single respond_to_user call
	// may carry. Zero means use defaultMaxAttachments (10).
	MaxAttachments int

	// LeakageGate, when non-nil, is called before the outbound channel
	// envelope is published. It receives the text and resolved attachment
	// list. Return nil to allow the publish; return a non-nil error to
	// block it — respond_to_user surfaces the error to the LLM as an
	// IsError result so the model can see the gate fired and adapt.
	//
	// The gate is responsible for its own approval flow: if it decides to
	// pause for human approval, it blocks here until a decision arrives,
	// then returns nil (approve) or an error (deny/timeout).
	LeakageGate func(ctx context.Context, sess *tool.SessionContext, text string, attachments []channelevents.AttachmentRef) error
}

func (c RespondConfig) maxAttachmentsOrDefault() int {
	if c.MaxAttachments > 0 {
		return c.MaxAttachments
	}
	return defaultMaxAttachments
}

// New respond-to-user tool. Construct via runner setup when the session is
// channel-attached.
func New(cfg RespondConfig) tool.Tool {
	return newRespondTool(cfg)
}

type respondTool struct {
	cfg           RespondConfig
	schema        json.RawMessage
	desc          string
	hasAsset      bool
	hasComponents bool
	// components validates and projects agent-authored `blocks`. Resolved from
	// the outbound kind at construction; nil when the kind advertises no
	// "components" capability, or (a wiring bug) advertises it without a
	// validator — Execute fails closed on the latter.
	components channelkinds.ComponentValidator
}

func newRespondTool(cfg RespondConfig) *respondTool {
	hasMarkdown := false
	hasComponents := false
	hasAsset := false
	for _, c := range cfg.Capabilities {
		switch {
		case c == "markdown":
			hasMarkdown = true
		case c == "components":
			hasComponents = true
		case strings.HasPrefix(c, "asset:"):
			hasAsset = true
		}
	}

	// WHAT a reply may carry and HOW it is authored are the KIND's facts, not
	// this tool's: the rules land in model-facing prompt text the runner
	// presents as channel-specific and authoritative, so asking the registry
	// keeps that promise for every registered kind. An unregistered name yields
	// a nil Kind — TextFormattingInstructionsFor answers with the generic
	// CommonMark line, and ComponentValidatorFor answers with nil.
	kind, _ := chregistry.Get(cfg.ChannelKind)

	props := map[string]any{}
	required := []string{"text"}
	textDesc := "The message text."
	if hasMarkdown {
		textDesc += " " + channelkinds.TextFormattingInstructionsFor(kind)
	} else {
		textDesc += " Plain text only — markdown will be sent literally."
	}
	props["text"] = map[string]any{"type": "string", "description": textDesc}

	var components channelkinds.ComponentValidator
	if hasComponents {
		components = channelkinds.ComponentValidatorFor(kind)
		desc := "Optional structured UI components in the channel-kind's native format, rendered as the message body with `text` as the notification preview and fallback. " +
			channelkinds.ComponentFormattingInstructionsFor(kind)
		props["blocks"] = map[string]any{
			"type":        "array",
			"description": desc,
			"items":       map[string]any{"type": "object"},
		}
	}

	if hasAsset {
		props["attached"] = map[string]any{
			"type":        "array",
			"description": "Handles to deliver alongside the text. Each may be: a handle from artifact_prepare/artifact_await, an artifact_id (delivers the newest revision), artifact_id#tag (delivers that tagged revision), or a `delegation/handle` pair exactly as a delegate or reply_to_subagent result listed it (delivers an artifact an agent you delegated to returned to you). Each MUST resolve to a status=ready render — owned by this session, or returned to you by that delegation. If any handle is unknown, expired, still pending, or was never handed to you, this tool fails and no message is delivered.",
			"items":       map[string]any{"type": "string"},
			"maxItems":    cfg.maxAttachmentsOrDefault(),
		}
	}

	schema := map[string]any{
		"type":                 "object",
		"properties":           props,
		"required":             required,
		"additionalProperties": false,
	}
	schemaBytes, _ := json.Marshal(schema)

	var descParts string
	if hasMarkdown {
		descParts = " Markdown is supported."
	} else {
		descParts = " Plain text only on this channel."
	}
	if hasComponents {
		descParts += " Structured `blocks` components are supported."
	}
	desc := fmt.Sprintf(
		"Send a reply to the user via the %s channel.%s Call this whenever you have a message to surface. "+
			"After your reply is complete, call await_user_message to yield until the user responds, "+
			"or call agent_work_complete if the current work item is done and the session should go idle.",
		cfg.ChannelKind, descParts,
	)

	return &respondTool{cfg: cfg, schema: schemaBytes, desc: desc, hasAsset: hasAsset, hasComponents: hasComponents, components: components}
}

func (t *respondTool) Name() string                 { return "respond_to_user" }
func (t *respondTool) Kind() tool.Kind              { return tool.KindMeta }
func (t *respondTool) Description() string          { return t.desc }
func (t *respondTool) InputSchema() json.RawMessage { return t.schema }
func (t *respondTool) Permission() authz.Permission {
	// respond_to_user posts to the agent's bound channel. The
	// channel-level "can this user receive a message here?" question
	// is governed by the existing AgentSession#interact policy; the
	// tool itself is treated as Passthrough — when provenance lands
	// (slice 2+), the data flowing into the channel will be
	// classified by its origin.
	return authz.Permission{StateImpact: authz.Passthrough}
}

// PermissionVariants returns nil — meta tools have no conditional
// variants today (only MCP-tooled AgentClasses use them).
func (t *respondTool) PermissionVariants() []authz.PermissionVariant { return nil }

func (t *respondTool) Execute(ctx context.Context, args json.RawMessage, sess *tool.SessionContext) (tool.Result, error) {
	if t.cfg.NATSPublish == nil {
		return tool.Result{Trusted: true}, errors.New("respond_to_user: NATSPublish is unset")
	}
	var in struct {
		// Text is the message body delivered to the channel.
		Text string `json:"text"`
		// Attached are artifact handles to attach; each is gated on the channel's
		// asset:* capability.
		Attached []string `json:"attached,omitempty"`
		// Blocks are agent-authored structured components in the channel-kind's
		// native format, gated on the channel's "components" capability and
		// validated by the kind before publish.
		Blocks json.RawMessage `json:"blocks,omitempty"`
	}
	if res, ok := tool.ParseArgs(args, &in, t.Name(), `{"text": "the message body"}`); !ok {
		return res, nil
	}
	if strings.TrimSpace(in.Text) == "" {
		return tool.Result{Content: "respond_to_user: `text` is required and must be non-empty. Pass the full message body you want the user to see, e.g. {\"text\": \"Here are the results: …\"}. If your previous reply was truncated, you may need to shorten it before retrying.", IsError: true, Trusted: true}, nil
	}

	// Validate agent-authored components BEFORE anything is published: a
	// rejection returns IsError with a detailed, correctable message and leaves
	// no envelope on the wire, exactly like the attachment-resolution guard
	// below. Only the kind knows its component format, so the kind's validator
	// (resolved from the outbound binding at construction) is the authority.
	if len(in.Blocks) > 0 {
		if !t.hasComponents {
			return tool.Result{Content: "respond_to_user: `blocks` (structured components) are not supported on this channel. Put your reply in `text` instead.", IsError: true, Trusted: true}, nil
		}
		if t.components == nil {
			// Advertised "components" but no validator: a wiring bug, not the
			// model's fault. Fail closed rather than publish unvalidated blocks.
			return tool.Result{Trusted: true}, fmt.Errorf("respond_to_user: channel kind %q advertises components but supplies no validator", t.cfg.ChannelKind)
		}
		if err := t.components.ValidateComponents(in.Blocks); err != nil {
			return tool.Result{Content: err.Error(), IsError: true, Trusted: true}, nil
		}
	}

	// Resolve attached artifact handles BEFORE publishing — any failure
	// returns IsError without putting the envelope on the wire so the
	// model can correct and retry.
	var attachments []channelevents.AttachmentRef
	if len(in.Attached) > 0 {
		if !t.hasAsset {
			return tool.Result{Content: "respond_to_user: `attached` is not supported on this channel (no asset:* capability). Drop the field and send a text-only reply.", IsError: true, Trusted: true}, nil
		}
		if t.cfg.Client == nil {
			return tool.Result{Trusted: true}, errors.New("respond_to_user: Client is unset; cannot resolve attached handles")
		}
		max := t.cfg.maxAttachmentsOrDefault()
		if len(in.Attached) > max {
			return tool.Result{Content: fmt.Sprintf("respond_to_user: too many attachments (%d > %d). Send fewer artifacts per reply.", len(in.Attached), max), IsError: true, Trusted: true}, nil
		}
		var err error
		attachments, err = t.resolveAttachments(ctx, sess, in.Attached)
		if err != nil {
			return tool.Result{Content: err.Error(), IsError: true, Trusted: true}, nil
		}
	}

	// Info-leakage write-side gate, before the outbound envelope is published.
	// Returns IsError so the model sees the gate and can adapt.
	//
	// When it blocks, a user is waiting on the channel for a reply, so publish a
	// generic fallback notice rather than leaving them in silence while the LLM
	// ends the session. Generic deliberately: the raw gate error can name
	// internal config (approver subject, SpiceDB endpoint) that must not reach
	// channel members.
	if t.cfg.LeakageGate != nil {
		// The gate measures TEXT. Components carry their own text, so feed the
		// gate the block content too (projected to plain text by the kind) —
		// otherwise an agent could route blockable content through `blocks` and
		// bypass the gate, a fail-open egress path. Appended to the TAGGED
		// in.Text the gate already runs on; the block text is untagged and so
		// falls to the gate's coarse check, same as an attachment does.
		gateText := in.Text
		if len(in.Blocks) > 0 && t.components != nil {
			if projected := t.components.ComponentsPlainText(in.Blocks); projected != "" {
				gateText = in.Text + "\n\n" + projected
			}
		}
		if err := t.cfg.LeakageGate(ctx, sess, gateText, attachments); err != nil {
			logger := log.FromContext(ctx)
			pub := func(subject string, data []byte) error {
				return publishWithRetry(ctx, t.cfg.NATSPublish, subject, data)
			}

			// Denial is terminal, not retryable. Re-issuing respond_to_user
			// after a denied share only re-publishes the blocked notice on every
			// attempt (the user saw it 3×). Post a single notice asking how to
			// proceed and yield the turn to the user (Idle) instead of returning
			// a retryable IsError that the LLM loops on.
			if errors.Is(err, leakage.ErrShareDenied) {
				logger.Info("respond_to_user: info-leakage share denied; yielding to user instead of retrying",
					"session", sess.Namespace+"/"+sess.Name, "err", err.Error())
				notice := channelevents.OutboundUserMessagePayload{
					Text: "🔒 I wasn't able to share that information — the request to share it was denied. How would you like me to proceed?",
				}
				if perr := t.cfg.EnvelopeSigner.PublishOut(pub, sess.Namespace, sess.Name,
					channelevents.KindUserMessage, notice); perr != nil {
					logger.Info("respond_to_user: denial notice publish failed; channel will see silence",
						"session", sess.Namespace+"/"+sess.Name, "err", perr.Error())
				}
				// Terminal+IdleExit yields the channel-attached session to the
				// user (WriteIdle/AwaitingUserMsg). The Content lands in the
				// transcript so the next turn knows the share was denied and not
				// to retry it.
				return tool.Result{
					Content:     fmt.Sprintf("information-leakage gate: sharing was denied (%v). Do not retry sending this content. The user has been asked how to proceed — wait for their response.", err),
					IsError:     true,
					Terminal:    true,
					IdleExit:    true,
					ShareDenied: true,
					Trusted:     true,
				}, nil
			}

			// Non-denial gate errors (approval timeout, misconfiguration,
			// audience-resolution failure) remain retryable: post the generic
			// blocked notice and return IsError so a transient problem can
			// recover. The fallback text is intentionally generic — the raw gate
			// error may contain internal config details (ApproverSubject, SpiceDB
			// endpoint, etc.) that should not leak to channel members.
			logger.Info("respond_to_user: info-leakage gate blocked publish; emitting fallback notice to channel",
				"session", sess.Namespace+"/"+sess.Name, "err", err.Error())
			fallback := channelevents.OutboundUserMessagePayload{
				Text: "🔒 I prepared a response but couldn't deliver it: a security check blocked the message. An operator has been notified.",
			}
			if perr := t.cfg.EnvelopeSigner.PublishOut(pub, sess.Namespace, sess.Name,
				channelevents.KindUserMessage, fallback); perr != nil {
				// Already failing this call; log + continue so the LLM still
				// sees the gate error and can stop trying.
				logger.Info("respond_to_user: fallback notice publish failed; channel will see silence",
					"session", sess.Namespace+"/"+sess.Name, "err", perr.Error())
			}
			return tool.Result{
				Content: fmt.Sprintf("information-leakage gate: %v", err),
				IsError: true,
				Trusted: true,
			}, nil
		}
	}

	// pt markup is model-facing only: strip any pt-untrusted envelope before the
	// human sees the reply. The LeakageGate above ran on the TAGGED in.Text (it
	// needs the tags to decide), so the strip is on the outbound copy only.
	outText := in.Text
	if stripped, had := toolenvelope.StripPt(in.Text); had {
		outText = stripped
	}

	logger := log.FromContext(ctx)
	toolUseID := ""
	if ids, ok := sandbox.IDsFromCtx(ctx); ok {
		toolUseID = ids.ToolUseID
	}
	logger.Info("respond_to_user: publishing user_message envelope",
		"session", sess.Namespace+"/"+sess.Name, "toolUseID", toolUseID, "textLen", len(outText), "attachments", len(attachments), "components", len(in.Blocks) > 0)
	publish := func(subject string, data []byte) error {
		return publishWithRetry(ctx, t.cfg.NATSPublish, subject, data)
	}
	if err := t.cfg.EnvelopeSigner.PublishOut(publish, sess.Namespace, sess.Name,
		channelevents.KindUserMessage,
		channelevents.OutboundUserMessagePayload{Text: outText, Attachments: attachments, Components: in.Blocks},
	); err != nil {
		return tool.Result{Content: fmt.Sprintf("publish failed: %v", err), IsError: true, Trusted: true}, nil
	}

	// Record WHICH renders this reply carried, now that the envelope is on the
	// wire. This is the only place the system learns that an artifact reached
	// anyone: a render going Ready says the bytes exist, and the completion
	// gate's artifact-delivered requirement reads exactly this set to tell the
	// two apart. Recorded after the publish, never before — a delivery marked
	// for an envelope that failed to publish would report a user saw something
	// they did not.
	if len(attachments) > 0 {
		// Both halves of each attachment: the render name the completion gate
		// matches against the API server, and the logical artifact id a durable
		// view link names. resolveAttachments read them from the same CR, so
		// recording only one would make the other unanswerable later without
		// re-reading it.
		items := make([]deliveries.Item, 0, len(attachments))
		names := make([]string, 0, len(attachments))
		for _, a := range attachments {
			items = append(items, deliveries.Item{RenderName: a.RenderName, ArtifactID: a.ArtifactID})
			names = append(names, a.RenderName)
		}

		// Both failure paths are non-fatal and neither is silent. The message
		// IS delivered by now, so re-failing the call would be worse than an
		// over-strict completion gate — but the consequence surfaces much later
		// and somewhere else, as agent_work_complete refusing over an artifact
		// the user is already looking at, which reads as a bug in the gate. The
		// log line is the only thing that leads back here.
		switch delivered, ok := deliveries.TryFrom(sess); {
		case !ok:
			logger.Info("respond_to_user: this session carries no artifact-delivery record, so the completion gate cannot see that these renders reached the user",
				"session", sess.Namespace+"/"+sess.Name, "renders", names)
		default:
			if err := delivered.Record(ctx, items...); err != nil {
				logger.Info("respond_to_user: failed to record artifact delivery; the completion gate may report these as undelivered",
					"session", sess.Namespace+"/"+sess.Name, "renders", names, "err", err.Error())
			}
		}
	}

	// Best-effort: record the delivered tool_use ID so resume can skip
	// re-dispatch. Failure here is non-fatal — duplicate publishes on
	// resume are recoverable; failing the tool call would be worse.
	// But it MUST log: a silently-failed delivered-marker means
	// resume duplicates the user_message envelope, which DOES post
	// twice to Slack — exactly the class of "user sees a problem but
	// no error in logs to trace it" bug the no-silent-errors rule
	// exists to prevent.
	if t.cfg.AppendSystemNote != nil {
		if ids, ok := sandbox.IDsFromCtx(ctx); ok && ids.ToolUseID != "" {
			if err := t.cfg.AppendSystemNote(ctx, map[string]any{
				"delivered": []string{ids.ToolUseID},
			}); err != nil {
				log.FromContext(ctx).Info(
					"respond_to_user: failed to record delivered system_note; resume may re-publish",
					"session", sess.Namespace+"/"+sess.Name,
					"toolUseID", ids.ToolUseID,
					"err", err.Error(),
				)
			}
		}
	}

	return tool.Result{Content: "delivered", IsError: false, Terminal: false, Trusted: true}, nil
}

// resolveAttachments fetches each ArtifactRender CR by name in
// sess.Namespace and verifies it (a) is owned by the AgentSession entitled to
// deliver it, (b) is in status.phase=Ready. Non-CR handles (artifact-… /
// artrev-…) are first resolved to a render CR name via
// Artifacts.ResolveToRender before the ownership + Ready checks. On any
// failure returns an error naming the bad handle; the caller surfaces this to
// the model as IsError and skips the publish.
//
// Two entitlements, not one. The ordinary handle must name a render this
// session OWNS — the check that stops a session delivering a stranger's
// artifact to its own channel. A `delegation/handle` pair instead names an
// artifact a child RETURNED to this session, and is authorized against that
// delegation's controller-owned status (see resolveDelegatedHandle): the
// parent owns user-delivery, so it must be able to deliver what it asked for
// and was given, and only that.
func (t *respondTool) resolveAttachments(ctx context.Context, sess *tool.SessionContext, handles []string) ([]channelevents.AttachmentRef, error) {
	out := make([]channelevents.AttachmentRef, 0, len(handles))
	for _, h := range handles {
		if h == "" {
			return nil, fmt.Errorf("respond_to_user: empty handle in `attached`; remove it and retry")
		}
		renderName := h
		// ownerName is the AgentSession the render must be owned by. Empty
		// means this session itself, matched by UID as well as name; a
		// delegated handle names the child the delegation ran instead.
		ownerName := ""
		if delegation, returned, isDelegated := cutDelegatedArtifactHandle(h); isDelegated {
			child, err := t.resolveDelegatedHandle(ctx, sess, h, delegation, returned)
			if err != nil {
				return nil, err
			}
			renderName, ownerName = returned, child
		} else if t.cfg.Artifacts != nil && !strings.HasPrefix(h, "ar-") {
			scope := memory.Scope{Kind: "session", ID: sess.Namespace + "/" + sess.Name}
			resolved, err := t.cfg.Artifacts.ResolveToRender(ctx, scope, h)
			if err != nil {
				return nil, fmt.Errorf("respond_to_user: %v", err)
			}
			renderName = resolved
		}
		var cr spiceboxv1alpha1.ArtifactRender
		if err := t.cfg.Client.Get(ctx, client.ObjectKey{Namespace: sess.Namespace, Name: renderName}, &cr); err != nil {
			if k8serrors.IsNotFound(err) {
				return nil, fmt.Errorf("respond_to_user: attached handle %q not found in this session — call artifact_prepare first or drop it", h)
			}
			return nil, fmt.Errorf("respond_to_user: attached handle %q lookup failed: %v", h, err)
		}
		switch {
		case ownerName == "" && !tool.OwnedBySession(cr.OwnerReferences, sess):
			return nil, fmt.Errorf("respond_to_user: attached handle %q is not owned by this session — drop it and use one returned by your own artifact_prepare", h)
		case ownerName != "" && !ownedBySessionNamed(cr.OwnerReferences, ownerName):
			// The delegation vouched for the handle and the render does not
			// bear it out: the child returned a handle it did not produce, or
			// the name now belongs to something else entirely. Refused rather
			// than delivered — a render nobody in this delegation owns is
			// exactly what the ownership check exists to keep off a channel.
			return nil, fmt.Errorf("respond_to_user: attached handle %q names a render that %s does not own — ask that agent for the handle again with reply_to_subagent", h, ownerName)
		}
		if cr.Status.Phase != spiceboxv1alpha1.ArtifactRenderPhaseReady {
			return nil, fmt.Errorf("respond_to_user: attached handle %q is not ready (phase=%q) — call artifact_await until status=ready, or drop it", h, cr.Status.Phase)
		}
		out = append(out, channelevents.AttachmentRef{
			RenderName: cr.Name,
			ArtifactID: cr.Labels[artifacts.LabelArtifactID],
			MIME:       cr.Status.OutputMIME,
			Filename:   cr.Status.OutputFilename,
		})
	}
	return out, nil
}

// resolveDelegatedHandle authorizes one `delegation/handle` attachment and
// returns the name of the child session whose render it must be.
//
// Everything it reads is CONTROLLER-owned. spec.parent is set by the
// delegating runner and verified at admission against its own identity;
// status.childRef and status.artifacts are written only by the SubagentRequest
// controller, and the per-session runner Role grants no write on
// subagentrequests/status at all. So a parent cannot mint itself an
// entitlement by claiming one: the two facts that grant it — "this delegation
// is mine" and "this handle came back from it" — are both recorded by parties
// the parent does not control.
//
// The handle's presence in status.artifacts is the CHILD's claim and is not
// treated as proof the bytes exist: it only says the child offered this handle
// to this parent. The render Get, the ownership match and the Ready check that
// follow in resolveAttachments are what establish the rest, at the moment the
// parent tries to deliver it rather than a reconcile earlier.
//
// Reads by Get, never List, because the runner Role grants only `get` on
// subagentrequests — which is why the delegation is named in the handle.
func (t *respondTool) resolveDelegatedHandle(
	ctx context.Context, sess *tool.SessionContext, handle, delegation, returned string,
) (childName string, err error) {
	var sr spiceboxv1alpha1.SubagentRequest
	if err := t.cfg.Client.Get(ctx, client.ObjectKey{Namespace: sess.Namespace, Name: delegation}, &sr); err != nil {
		if k8serrors.IsNotFound(err) {
			return "", fmt.Errorf("respond_to_user: attached handle %q names delegation %q, which does not exist here — pass a handle exactly as the delegate result listed it", handle, delegation)
		}
		return "", fmt.Errorf("respond_to_user: attached handle %q: reading delegation %q failed: %v", handle, delegation, err)
	}
	if sr.Spec.Parent.Namespace != sess.Namespace || sr.Spec.Parent.Name != sess.Name {
		return "", fmt.Errorf("respond_to_user: attached handle %q names delegation %q, which is not yours — you may only deliver artifacts returned to you", handle, delegation)
	}
	if sr.Status.ChildRef == nil {
		return "", fmt.Errorf("respond_to_user: attached handle %q names delegation %q, which never ran an agent, so nothing was returned from it", handle, delegation)
	}
	for _, a := range sr.Status.Artifacts {
		if a.ID == returned {
			return sr.Status.ChildRef.Name, nil
		}
	}
	return "", fmt.Errorf("respond_to_user: attached handle %q: delegation %q returned no artifact %q — deliver only what it handed back, or ask it again with reply_to_subagent", handle, delegation, returned)
}

// ownedBySessionNamed reports whether refs carry an AgentSession owner of the
// given name.
//
// By NAME alone, where tool.OwnedBySession also matches the UID, because the
// caller checking a delegated handle holds no UID to match: status.childRef is
// a namespace+name reference, and the per-session runner Role pins `get` on
// agentsessions to the runner's OWN session, so the child's UID cannot be read
// to compare. Name is sufficient here: an AgentSession name is unique in a
// namespace while it exists, and a render outlives its session by nothing —
// the ownerRef cascade reaps it — so there is no window in which a recreated
// same-named session inherits a stranger's render.
//
// That last clause is an invariant this function cannot check, so it is pinned
// where it is created instead: TestArtifactPrepare_StampsCascadingOwnerRef
// asserts the render carries a controller ownerRef to its session. If that
// stamp is ever dropped, renders begin outliving their sessions and the
// name-only match here stops being sufficient — silently, since nothing on
// this path would change.
func ownedBySessionNamed(refs []metav1.OwnerReference, name string) bool {
	for _, r := range refs {
		if r.Kind == "AgentSession" && r.Name == name {
			return true
		}
	}
	return false
}

func publishWithRetry(ctx context.Context, publish func(ctx context.Context, subject string, payload []byte) error, subject string, payload []byte) error {
	var lastErr error
	delay := 100 * time.Millisecond
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if err := publish(ctx, subject, payload); err == nil {
			return nil
		} else {
			lastErr = err
		}
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return ctx.Err()
		}
		if delay < 2*time.Second {
			delay *= 2
		}
	}
	return fmt.Errorf("publish failed after retries: %w", lastErr)
}
