// Best-effort session-view link minter for
// `oap agent chat`. Reads the webd base-URL ConfigMap once at chat startup —
// the same source buildChatViewMinter reads for the artifact-view minter —
// but, unlike the artifact minter, needs no passthroughlink signing key: the
// session-view page is a durable plain-path link, not a signed capability
// (CheckInteract at open time is the sole authorization boundary). When the
// ConfigMap can't be read at all, the minter stays nil and the caller
// (startSession) prints a startup notice, same as the artifact minter's; the
// session_view_offer sub-channel sender also surfaces a nil minter loudly
// per-offer, so the notice and the sender's own error are belt-and-suspenders,
// not the only signal.
package chatcmd

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/kube"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/x/externalurl"
)

// tuiSessionViewMinter implements channelkinds.SessionViewMinter for the TUI.
// It wraps a frozen base-URL string (read once at chat startup — a single
// read is sufficient for a single chat session, matching buildChatViewMinter's
// artifact minter) and composes URLs via the shared
// channelkinds.ComposeSessionViewURL (also used by internal/cmd/channelsd and
// internal/cmd/webd, so the URL shape is defined in exactly one place).
type tuiSessionViewMinter struct {
	webdBaseURL string
}

func (m *tuiSessionViewMinter) MintSessionViewLink(sessionRef string, _ identity.Principal, _ string) (string, error) {
	return channelkinds.ComposeSessionViewURL(m.webdBaseURL, sessionRef)
}

// buildChatSessionViewMinter tries to construct the session-view link minter
// for the TUI. Returns (minter, "") on success and (nil, reason) when the
// webd external-URL ConfigMap can't be read at all.
//
// Unlike buildChatViewMinter, a missing/empty URL *inside* an otherwise
// readable ConfigMap is not treated as a build failure here: the minter is
// still constructed (with an empty webdBaseURL), and
// channelkinds.ComposeSessionViewURL's own ("", nil) clean-skip contract
// means the session_view_offer sender's own empty-URL branch — not this
// constructor — is what surfaces the "external URL not configured yet"
// condition loudly, at the moment a widget actually needs it.
func buildChatSessionViewMinter(ctx context.Context, b *kube.Bundle) (channelkinds.SessionViewMinter, string) {
	base, reason := readWebdTrustedBaseURL(ctx, b)
	if reason != "" {
		return nil, reason
	}
	return &tuiSessionViewMinter{webdBaseURL: base}, ""
}

// readWebdTrustedBaseURL reads webd's externally reachable trusted base URL
// for the TUI's link minters. Returns (url, "") on success — where url may
// legitimately be empty, meaning the source exists but the operator has not
// populated it yet — and ("", reason) when it cannot be read at all.
//
// Both TUI minters built at chat startup (session-view and agent-UI) call
// this, so they share the read's LOGIC and its failure semantics: they cannot
// disagree about what an absent source versus an unpopulated key means, or
// word the same failure two different ways. They do still issue one Get each,
// against an uncached client, so two live reads a moment apart can genuinely
// return different answers — that residual case is what the
// reason-comparison in startSession's notices handles.
//
// The controller client is used rather than the typed clientset because `oap`
// needs a point-in-time read, not live updates, and it is the client already
// set up for oap's use.
//
// The reason is USER-FACING (startSession renders it into the TUI timeline),
// so it carries neither the Kubernetes object kind nor the raw API error —
// both name cluster internals the reader cannot act on. The error's class is
// kept, because "not installed" and "installed but I was refused" call for
// different actions, and it is all that survives: a bubbletea program owns
// the terminal, so there is no log sink here to put the detail in.
func readWebdTrustedBaseURL(ctx context.Context, b *kube.Bundle) (string, string) {
	var cm corev1.ConfigMap
	if err := b.Controller.Get(ctx, client.ObjectKey{
		Namespace: externalurl.Namespace,
		Name:      spiceboxv1alpha1.WebdExternalURLConfigMap,
	}, &cm); err != nil {
		if apierrors.IsNotFound(err) {
			return "", "this cluster's web UI is not installed, so browser links are unavailable"
		}
		if reason := apierrors.ReasonForError(err); reason != metav1.StatusReasonUnknown {
			return "", fmt.Sprintf("this cluster's web UI could not be reached (%s)", strings.ToLower(string(reason)))
		}
		return "", "this cluster's web UI could not be reached"
	}
	return cm.Data[spiceboxv1alpha1.WebdTrustedURLKey], ""
}
