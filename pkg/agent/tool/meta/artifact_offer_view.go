package meta

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/channels/channelassets"
	assetregistry "github.com/authzed/openagentprimitives/pkg/channels/channelassets/registry"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	chregistry "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/registry"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/platform/artifacts"
)

// ArtifactOfferViewConfig wires artifact_offer_view to the version chain,
// the channel's renderer kinds, and the NATS publisher.
type ArtifactOfferViewConfig struct {
	Artifacts         *artifacts.Service
	AvailableKinds    []string
	NATSPublish       func(ctx context.Context, subject string, payload []byte) error
	NATSSubjectPrefix string

	// EnvelopeSigner signs every envelope artifact_offer_view publishes with
	// the session's identity key. Nil-safe: a nil signer leaves the envelope
	// unsigned (test fixtures without a signer still work).
	EnvelopeSigner *channelevents.EnvelopeSigner

	// ChannelKind names the transport this session is bound to ("slack",
	// "local", …). It is what the tool asks the channel-kind registry whether a
	// live-view offer can be RENDERED on at all, before publishing one.
	//
	// Empty or unregistered means the tool cannot tell, and it offers anyway: a
	// wiring gap must not quietly remove the live view from a channel that has
	// one. Every such call is logged, because the answer the tool can then give
	// the model is weaker than it should be.
	ChannelKind string

	// Client creates the internal preview-render CR. RenderFetch fetches a
	// source revision's rendered bytes from the operator. MarkupGen is the
	// secondary-LLM markup generator passed to css PreviewHTML (nil-safe).
	// These are only used for BundledOnly kinds; when any is nil, preview
	// generation is skipped (the kind still delivers as a file).
	Client      client.Client
	RenderFetch func(ctx context.Context, ns, sess, renderName string) ([]byte, string, error)
	MarkupGen   channelassets.MarkupGenerator

	// PollInterval is the gap between preview-render readiness polls.
	// Defaults to 250ms when unset.
	PollInterval time.Duration
}

// NewArtifactOfferView constructs the artifact_offer_view meta tool.
func NewArtifactOfferView(cfg ArtifactOfferViewConfig) tool.Tool {
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 250 * time.Millisecond
	}
	return &artifactOfferViewTool{cfg: cfg}
}

type artifactOfferViewTool struct{ cfg ArtifactOfferViewConfig }

func (*artifactOfferViewTool) Name() string    { return "artifact_offer_view" }
func (*artifactOfferViewTool) Kind() tool.Kind { return tool.KindMeta }
func (*artifactOfferViewTool) Permission() authz.Permission {
	return authz.Permission{StateImpact: authz.Passthrough}
}
func (*artifactOfferViewTool) PermissionVariants() []authz.PermissionVariant { return nil }

func (*artifactOfferViewTool) Description() string {
	return "Offer the user a live-view of an artifact — a button/link that opens a browser viewer which updates in real time as you revise the artifact. " +
		"The user must click to open it (you cannot open it for them). Input: `artifact_id` (from artifact_prepare). " +
		"Only works for renderer kinds that support live-view (html does), and only on channels that have somewhere to render an offer. " +
		"This ADDS to attaching the artifact to your respond_to_user reply; it does not replace it. The attachment is the copy the user " +
		"keeps and the one that works on every channel, so send it too — the offer alone may reach nobody."
}

func (*artifactOfferViewTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"additionalProperties": false,
		"properties": {"artifact_id": {"type": "string"}},
		"required": ["artifact_id"]
	}`)
}

func (t *artifactOfferViewTool) Execute(ctx context.Context, raw json.RawMessage, sess *tool.SessionContext) (tool.Result, error) {
	if t.cfg.NATSPublish == nil {
		return tool.Result{Trusted: true}, fmt.Errorf("artifact_offer_view: NATSPublish is unset")
	}
	var in struct {
		ArtifactID string `json:"artifact_id"`
	}
	if res, ok := tool.ParseArgs(raw, &in, t.Name(), `{"artifact_id": "artifact-…"}`); !ok {
		return res, nil
	}

	// Ask the transport, before anything else, whether it has anywhere to show
	// an offer. The outbound relay drops an envelope whose sub-channel sender is
	// nil with nothing but a log line, so publishing to a kind that declares no
	// live-view surface produces a reply the user never sees and a result the
	// model reads as success.
	//
	// Not an error: the attachment is the delivery on every channel, and
	// refusing here would make live-view-less channels worse off than they were
	// while the tool was lying.
	kind, known := chregistry.Get(t.cfg.ChannelKind)
	switch {
	case known && !kind.SupportsLiveViewOffer():
		return tool.Result{Content: fmt.Sprintf(
			"artifact_offer_view: the %s channel has no live-view surface, so no offer was rendered and nothing was sent. "+
				"That is expected on this channel, not a failure — attach the artifact to your respond_to_user reply and the user has it. "+
				"Do not retry this call, and do not tell the user a live view is available.", t.cfg.ChannelKind),
			Trusted: true}, nil
	case !known:
		// Offer anyway: a wiring gap must not quietly remove the live view from
		// a channel that has one. Logged because the tool's answer below is then
		// weaker than it should be, and because an unresolvable kind here is a
		// runner wiring bug worth finding.
		slog.Info("artifact_offer_view: channel kind not resolvable; offering without knowing whether it can be rendered",
			"session", sess.Namespace+"/"+sess.Name, "channelKind", t.cfg.ChannelKind, "artifact", in.ArtifactID)
	}

	scope := memory.Scope{Kind: "session", ID: sess.Namespace + "/" + sess.Name}
	head, ok, err := t.cfg.Artifacts.GetHead(ctx, scope, in.ArtifactID)
	if err != nil {
		return tool.Result{Content: "artifact_offer_view: " + err.Error(), IsError: true, Trusted: true}, nil
	}
	if !ok {
		return tool.Result{Content: fmt.Sprintf("artifact_offer_view: artifact %q not found in this session — pass an artifact_id from artifact_prepare.", in.ArtifactID), IsError: true, Trusted: true}, nil
	}
	if !containsString(t.cfg.AvailableKinds, head.RendererKind) {
		return tool.Result{Content: fmt.Sprintf("artifact_offer_view: renderer kind %q is not available on this channel.", head.RendererKind), IsError: true, Trusted: true}, nil
	}
	r, ok := assetregistry.ByKind(head.RendererKind)
	if !ok || !r.SupportsLiveView() {
		return tool.Result{Content: fmt.Sprintf("artifact_offer_view: renderer kind %q does not support live-view.", head.RendererKind), IsError: true, Trusted: true}, nil
	}

	publish := func(subject string, data []byte) error {
		return publishWithRetry(ctx, t.cfg.NATSPublish, subject, data)
	}
	if err := t.cfg.EnvelopeSigner.PublishOut(publish, sess.Namespace, sess.Name,
		channelevents.KindLiveViewOffer,
		channelevents.LiveViewOfferPayload{ArtifactID: in.ArtifactID, RendererKind: head.RendererKind},
	); err != nil {
		// The cause goes to the log and the model gets fixed copy: a transport
		// error names the NATS subject it failed on, which puts this session's
		// routing into the model's context and from there into whatever it says
		// next. None of it is actionable to the model anyway — the only useful
		// answer is that the offer did not go out.
		slog.Info("artifact_offer_view: publishing the live-view offer failed",
			"session", sess.Namespace+"/"+sess.Name, "artifact", in.ArtifactID, "err", err.Error())
		return tool.Result{Content: "artifact_offer_view: the live-view offer could not be sent, so nothing was rendered and the user has no live view. " +
			"Do not tell them one is available. The artifact you attach to your respond_to_user reply is what actually reaches them; " +
			"send that, and you may retry this call afterwards.",
			IsError: true, Trusted: true}, nil
	}

	// BundledOnly kinds (css, svg) are never the top-level browser document;
	// their live-view is a composed html preview that we generate async (one per
	// revision) so the tool call returns immediately. Standalone kinds (html)
	// serve their bytes directly and need no preview child.
	if r.Delivery() == channelassets.DeliveryBundledOnly && t.cfg.Client != nil && t.cfg.RenderFetch != nil {
		// Detach from the tool-call ctx so generation survives the turn; bound it.
		genCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Minute)
		go func() {
			defer cancel()
			if err := t.generatePreview(genCtx, scope, sess, in.ArtifactID, head.RendererKind, r); err != nil {
				slog.Info("artifact_offer_view: preview generation failed",
					"session", sess.Namespace+"/"+sess.Name, "artifact", in.ArtifactID, "err", err.Error())
			}
		}()
	}

	// "published", never "delivered" or "shown". The render happens in another
	// process — channelsd, or the user's own oap for a client-hosted kind — and
	// no result comes back, so this tool never learns whether an offer appeared.
	// The model repeats what it is told here to the user, which is why the one
	// thing it is told to rely on is the attachment.
	return tool.Result{Content: fmt.Sprintf(
		"live-view offer published to %s. If it renders, the user can click it to open the artifact in their browser — "+
			"you cannot open it for them, and this tool does not observe the render, so do not report the live view as delivered. "+
			"The copy the user is guaranteed to have is the one you attach to respond_to_user.",
		channelPhrase(t.cfg.ChannelKind)), Trusted: true}, nil
}

// channelPhrase names the transport in a sentence, degrading to the bare noun
// when the tool was given no kind to name.
func channelPhrase(kind string) string {
	if kind == "" {
		return "the channel"
	}
	return "the " + kind + " channel"
}

// generatePreview composes and finalizes an internal html preview child for
// EVERY revision of a BundledOnly source artifact, so pinning any revision in
// the live-viewer serves THAT revision's preview. Idempotent per revision and
// bounded by ctx.
//
// Revisions run sequentially inside the caller's async goroutine so this never
// fans out N concurrent LLM calls. A per-revision failure is logged and does NOT
// abort the loop — one bad revision must not block the others.
//
// headID is passed explicitly because head data does not carry its own ID.
func (t *artifactOfferViewTool) generatePreview(ctx context.Context, scope memory.Scope, sess *tool.SessionContext, headID, kind string, renderer channelassets.Renderer) error {
	tree, err := t.cfg.Artifacts.RevisionTree(ctx, scope, headID)
	if err != nil {
		return err
	}
	for _, rev := range tree {
		if err := ctx.Err(); err != nil {
			return err
		}
		// Idempotency: a preview already finalized for this exact source revision
		// is a no-op (a re-offer reuses the deterministic child head).
		if _, ok, err := t.cfg.Artifacts.GetPreviewChild(ctx, scope, rev.RevisionID); err != nil {
			return err
		} else if ok {
			continue
		}
		if err := t.generatePreviewForRevision(ctx, scope, sess, headID, kind, renderer, rev.RevisionID, rev.RenderName); err != nil {
			// Best-effort: log this revision's failure and keep going so a single
			// bad revision doesn't strand the others' previews.
			slog.Info("artifact_offer_view: preview generation for revision failed",
				"session", sess.Namespace+"/"+sess.Name, "artifact", headID,
				"revision", rev.RevisionID, "err", err.Error())
			continue
		}
	}
	return nil
}

// generatePreviewForRevision composes and finalizes the internal html preview
// child for one specific source revision. It assumes the caller has already
// checked idempotency (GetPreviewChild) for this revision.
func (t *artifactOfferViewTool) generatePreviewForRevision(ctx context.Context, scope memory.Scope, sess *tool.SessionContext, headID, kind string, renderer channelassets.Renderer, revID, srcRenderName string) error {
	if srcRenderName == "" {
		return fmt.Errorf("artifact_offer_view: source revision %q has no render name", revID)
	}

	srcBytes, _, err := t.cfg.RenderFetch(ctx, sess.Namespace, sess.Name, srcRenderName)
	if err != nil {
		return err
	}

	pc, ok := renderer.(channelassets.PreviewComposer)
	if !ok {
		return fmt.Errorf("artifact_offer_view: kind %q has no preview composer", kind)
	}
	previewHTML, err := pc.PreviewHTML(ctx, srcBytes, t.cfg.MarkupGen)
	if err != nil {
		return err
	}

	cr := &spiceboxv1alpha1.ArtifactRender{
		TypeMeta: metav1.TypeMeta{APIVersion: spiceboxv1alpha1.SchemeBuilder.GroupVersion.String(), Kind: "ArtifactRender"},
		ObjectMeta: metav1.ObjectMeta{
			// Same minter as artifact_prepare's: one format, one seam. A
			// preview child's name is model-facing too, through the view URL.
			Name:      t.cfg.Artifacts.NewRenderName(sess.Name),
			Namespace: sess.Namespace,
			Labels:    map[string]string{artifacts.LabelArtifactID: t.cfg.Artifacts.PreviewChildID(revID)},
			Annotations: map[string]string{
				artifacts.AnnoInternal:          "true",
				artifacts.AnnoPreviewOf:         headID,
				artifacts.AnnoPreviewRevisionOf: revID,
			},
		},
		Spec: spiceboxv1alpha1.ArtifactRenderSpec{
			Kind: "html", Payload: previewHTML, TimeoutSeconds: 60,
		},
	}
	if sess.AgentSessionUID != "" {
		tval := true
		cr.OwnerReferences = []metav1.OwnerReference{{
			APIVersion: spiceboxv1alpha1.SchemeBuilder.GroupVersion.String(), Kind: "AgentSession",
			Name: sess.Name, UID: sess.AgentSessionUID, Controller: &tval, BlockOwnerDeletion: &tval,
		}}
	}
	if err := t.cfg.Client.Create(ctx, cr); err != nil {
		return fmt.Errorf("artifact_offer_view: create preview CR: %w", err)
	}

	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		var fresh spiceboxv1alpha1.ArtifactRender
		if err := t.cfg.Client.Get(ctx, client.ObjectKeyFromObject(cr), &fresh); err != nil {
			if errors.IsNotFound(err) {
				return fmt.Errorf("artifact_offer_view: preview CR vanished before completion")
			}
			if sleepCtx(ctx, t.cfg.PollInterval) != nil {
				return ctx.Err()
			}
			continue
		}
		switch fresh.Status.Phase {
		case spiceboxv1alpha1.ArtifactRenderPhaseReady:
			_, err := t.cfg.Artifacts.FinalizeRevision(ctx, scope, &fresh)
			return err
		case spiceboxv1alpha1.ArtifactRenderPhaseFailed:
			return fmt.Errorf("artifact_offer_view: preview render failed: %s — %s", fresh.Status.FailureReason, fresh.Status.FailureMessage)
		}
		if sleepCtx(ctx, t.cfg.PollInterval) != nil {
			return ctx.Err()
		}
	}
}

// sleepCtx sleeps for d or until ctx is done, returning ctx.Err() if the
// context was cancelled during the wait.
func sleepCtx(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
