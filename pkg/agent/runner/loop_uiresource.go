package runner

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/platform/artifacts"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"

	k8sclient "sigs.k8s.io/controller-runtime/pkg/client"
)

// uiResourceStatusWriter is the subset of StatusPatcher that applyUIResource
// needs to record a durable status.activeWidgets ref. Defined here (mirroring
// secretOutputStatusWriter in loop_secretout.go) so tests can provide a minimal
// stub without constructing a full StatusPatcher/k8s client.
type uiResourceStatusWriter interface {
	AppendActiveWidget(ctx context.Context, ref spiceboxv1alpha1.WidgetRef) error
}

// uiResourceDeps bundles the backend wiring applyUIResource needs to turn a
// widget into a durable artifact. Client and Artifacts are Loop.Client /
// Loop.Artifacts (either may be nil — see their doc comments); Status is
// Loop.Status (satisfies uiResourceStatusWriter); PollInterval <= 0 defaults
// to uiResourcePollInterval.
type uiResourceDeps struct {
	Client       k8sclient.Client
	Artifacts    *artifacts.Service
	Status       uiResourceStatusWriter
	PollInterval time.Duration
}

// uiResourcePollInterval / uiResourceRenderTimeout mirror artifact_prepare's
// defaults (250ms poll, generous ceiling). mcpui is an identity (pass-
// through) renderer — no sanitize/CSP work — so the operator's
// artifactrender controller completes it near-instantly in practice; the
// timeout is a safety ceiling, not an expected wait.
const (
	uiResourcePollInterval  = 250 * time.Millisecond
	uiResourceRenderTimeout = 30 * time.Second
)

// applyUIResource reacts to a tool result carrying an interactive ui:// widget
// (Result.UIResource, set by MCP dispatch). It never touches r.Content — that
// already holds a trusted, LLM-safe summary — and always clears the side-band
// field before returning, on every path, so the widget HTML never leaks to a
// later dispatch or reaches the LLM.
//
// Three things happen, in order, for a non-nil UIResource:
//
//  1. Persist: the widget's raw HTML becomes a durable artifact through the
//     same ArtifactRender CR → operator → artifactstore pipeline
//     artifact_prepare uses (create the CR with Kind:"mcpui" — the identity
//     renderer, pkg/channels/channelassets/mcpui — poll to Ready,
//     FinalizeRevision). Failure (deps.Client/deps.Artifacts nil, CR
//     create/poll/finalize error) degrades: logged, no durable ref, no
//     widget_offer — but step 3 still runs, because the escalation anchor
//     should open the page even when this widget failed to persist.
//  2. Record + signal: the artifact ref is appended to status.activeWidgets
//     (durable — a user clicking the anchor minutes later still sees it) and a
//     KindWidgetOffer envelope is published (live). Unlike step 3 this is NOT
//     soft-muted: every persisted widget gets its own ref + live offer.
//  3. Escalate: the KindSessionViewOffer anchor publish, unconditional on step
//     1/2's outcome. escalated soft-mutes it to at most one anchor per
//     escalation window (the caller's *Loop, for the life of one Run) — a later
//     widget in the same window skips the repost, but still completes 1 and 2.
func applyUIResource(
	ctx context.Context,
	deps uiResourceDeps,
	publish channelevents.PublishFunc,
	signer *channelevents.EnvelopeSigner,
	ns, name string,
	sessionUID types.UID,
	escalated *bool,
	r tool.Result,
) tool.Result {
	if r.UIResource == nil {
		return r
	}
	widget := r.UIResource
	r.UIResource = nil

	logger := ctrllog.FromContext(ctx)
	sessionRef := ns + "/" + name
	const rendererKind = "mcpui"

	switch {
	case deps.Client == nil || deps.Artifacts == nil:
		logger.Info("widget_offer: no persistence backend wired; widget not durably stored",
			"session", sessionRef)
	default:
		artifactID, err := persistWidget(ctx, deps, ns, name, sessionUID, widget)
		if err != nil {
			logger.Info("widget_offer: persisting widget failed; session_view_offer anchor still published",
				"session", sessionRef, "err", err.Error())
		} else {
			if deps.Status != nil {
				// Origin travels with the ref because the browser has to be
				// told which server authored this widget, and only the runner
				// knows: it is on the resource spec here and is dropped
				// everywhere downstream. webd resolves it back from this list
				// to pin the widget's app-tool calls to its own server.
				if serr := deps.Status.AppendActiveWidget(ctx, spiceboxv1alpha1.WidgetRef{
					ArtifactID: artifactID, Tool: widget.Tool, RendererKind: rendererKind,
					Origin: widget.Origin,
				}); serr != nil {
					logger.Info("widget_offer: recording status.activeWidgets failed",
						"session", sessionRef, "artifactID", artifactID, "err", serr.Error())
				}
			}
			if publish != nil {
				if perr := signer.PublishOut(publish, ns, name, channelevents.KindWidgetOffer,
					channelevents.WidgetOfferPayload{ArtifactID: artifactID, Tool: widget.Tool, RendererKind: rendererKind}); perr != nil {
					logger.Info("widget_offer: publish failed", "session", sessionRef, "err", perr.Error())
				}
			}
		}
	}

	// session_view_offer escalation anchor — soft-muted, unconditional on the
	// persistence outcome above; see the doc comment.
	if escalated != nil && *escalated {
		// Already escalated this window: no repost.
		return r
	}
	if publish == nil {
		logger.Info("session_view_offer: no publisher wired; widget escalation skipped",
			"session", sessionRef)
		return r
	}
	if err := signer.PublishOut(publish, ns, name, channelevents.KindSessionViewOffer,
		channelevents.SessionViewOfferPayload{SessionRef: sessionRef}); err != nil {
		logger.Info("session_view_offer: publish failed",
			"session", sessionRef, "err", err.Error())
		return r
	}
	if escalated != nil {
		*escalated = true
	}
	return r
}

// persistWidget creates an ArtifactRender{Kind:"mcpui"} CR for widget, polls
// it to Ready/Failed, and finalizes the revision — mirroring the
// CR-create+poll+finalize dance in pkg/agent/tool/meta/artifact_prepare.go.
// Returns the finalized artifact head ID.
func persistWidget(ctx context.Context, deps uiResourceDeps, ns, name string, sessionUID types.UID, widget *tool.UIResourceSpec) (string, error) {
	scope := memory.Scope{Kind: "session", ID: ns + "/" + name}
	headID := deps.Artifacts.NewArtifactID()

	suffix, err := widgetCRSuffix()
	if err != nil {
		return "", fmt.Errorf("generate CR name suffix: %w", err)
	}
	cr := artifacts.NewRender(fmt.Sprintf("arw-%s-%s", name, suffix), ns, name, sessionUID, headID,
		spiceboxv1alpha1.ArtifactRenderSpec{
			Kind:           "mcpui",
			Filename:       "widget.html",
			TimeoutSeconds: int32(uiResourceRenderTimeout / time.Second),
			Payload:        widget.HTML,
			CSP:            widgetDeclaredCSP(ctx, widget.Meta),
		}, nil)
	if err := deps.Client.Create(ctx, cr); err != nil {
		return "", fmt.Errorf("create ArtifactRender: %w", err)
	}

	pollInterval := deps.PollInterval
	if pollInterval <= 0 {
		pollInterval = uiResourcePollInterval
	}
	deadline := time.Now().Add(uiResourceRenderTimeout)
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		var fresh spiceboxv1alpha1.ArtifactRender
		if err := deps.Client.Get(ctx, k8sclient.ObjectKeyFromObject(cr), &fresh); err != nil {
			if apierrors.IsNotFound(err) {
				return "", fmt.Errorf("ArtifactRender vanished before completion")
			}
			time.Sleep(pollInterval)
			continue
		}
		switch fresh.Status.Phase {
		case spiceboxv1alpha1.ArtifactRenderPhaseReady:
			rev, err := deps.Artifacts.FinalizeRevision(ctx, scope, &fresh)
			if err != nil {
				return "", fmt.Errorf("render Ready but recording the revision failed: %w", err)
			}
			return rev.ArtifactID, nil
		case spiceboxv1alpha1.ArtifactRenderPhaseFailed:
			return "", fmt.Errorf("%s — %s", fresh.Status.FailureReason, fresh.Status.FailureMessage)
		}
		time.Sleep(pollInterval)
	}
	return "", fmt.Errorf("timed out waiting for ArtifactRender to complete")
}

// widgetDeclaredCSP looks for the mcp-ui / MCP Apps `_meta.ui.csp` shape —
// {"ui":{"csp":{"connectDomains":[...],"resourceDomains":[...],"frameDomains":[...]}}}
// — in a widget's MCP-server-authored, therefore untrusted, `_meta`. Returns nil
// (never an error) when rawMeta is empty, malformed, or carries no `ui.csp`: the
// restrictive default CSP (buildWidgetCSP(nil, …)) is the safe fallback, so a
// malformed declaration degrades to "no extra domains" rather than failing
// widget persistence. A parse failure is logged for operator visibility.
func widgetDeclaredCSP(ctx context.Context, rawMeta json.RawMessage) *spiceboxv1alpha1.WidgetCSP {
	if len(rawMeta) == 0 {
		return nil
	}
	var parsed struct {
		UI struct {
			CSP *spiceboxv1alpha1.WidgetCSP `json:"csp"`
		} `json:"ui"`
	}
	if err := json.Unmarshal(rawMeta, &parsed); err != nil {
		ctrllog.FromContext(ctx).Info("persistWidget: malformed _meta; falling back to restrictive default CSP",
			"err", err.Error())
		return nil
	}
	return parsed.UI.CSP
}

// widgetCRSuffix returns a 6-character random hex suffix for the
// ArtifactRender CR name, from the same crypto/rand source
// newUntrustedOutputNonce (loop_untrusted.go) uses.
func widgetCRSuffix() (string, error) {
	var b [3]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
