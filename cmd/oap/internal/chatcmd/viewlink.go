// Best-effort artifact view-link minter for
// `oap agent chat`. Reads the passthroughlink signing-key Secret and the webd
// base URL ConfigMap once at chat startup. Either missing → minter stays nil
// and the TUI omits browser links (a single startup notice explains why).
package chatcmd

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/kube"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/passthroughlink"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/passthroughlink/viewlink"
	"github.com/authzed/openagentprimitives/pkg/x/externalurl"
)

// buildChatViewMinter tries to construct the artifact view-link minter for
// the TUI. It reads the passthroughlink signing-key Secret and the webd
// base-URL ConfigMap once (read-once is sufficient for a single chat session).
//
// Returns (minter, nil) when both are available. Returns (nil, reason) when
// either is absent or malformed — the caller prints a single startup notice
// and sets Deps.ArtifactViewMinter = nil (TUI degrades gracefully: no browser
// links). A non-nil error indicates a transient fetch failure that the user
// should know about; a nil minter with nil error should never happen.
func buildChatViewMinter(ctx context.Context, b *kube.Bundle) (channelkinds.ArtifactViewMinter, string) {
	// Read the passthroughlink signing-key Secret.
	var sec corev1.Secret
	if err := b.Controller.Get(ctx, client.ObjectKey{
		Namespace: "agentprimitives-system",
		Name:      spiceboxv1alpha1.PassthroughLinkSigningKeySecret,
	}, &sec); err != nil {
		return nil, fmt.Sprintf("passthrough signing key Secret not found (%s): %v", spiceboxv1alpha1.PassthroughLinkSigningKeySecret, err)
	}
	// Shares the loader contract with webd and channelsd: trim, hex-decode,
	// and refuse anything below the minimum-length floor. This reader already
	// rejected a wholly empty value, but not a short one — the floor now comes
	// from the one place all three readers call.
	keyBytes, err := passthroughlink.DecodeHexKey(sec.Data["key"])
	if err != nil {
		return nil, fmt.Sprintf("passthrough signing key Secret %q: %v", spiceboxv1alpha1.PassthroughLinkSigningKeySecret, err)
	}
	signer := passthroughlink.New(keyBytes,
		passthroughlink.WithIssuer(passthroughlink.IssuerChannelsd),
		passthroughlink.WithAudience(passthroughlink.AudienceIdentityd),
	)

	// Read the webd base URL ConfigMap once. Use the controller client (not
	// the typed clientset): oap only needs a single read, not live updates,
	// and the controller client is what's already set up for oap's use.
	var cm corev1.ConfigMap
	if err := b.Controller.Get(ctx, client.ObjectKey{
		Namespace: externalurl.Namespace,
		Name:      spiceboxv1alpha1.WebdExternalURLConfigMap,
	}, &cm); err != nil {
		return nil, fmt.Sprintf("webd external-URL ConfigMap not found: %v", err)
	}
	webdURL := cm.Data[spiceboxv1alpha1.WebdTrustedURLKey]
	// webdURL may be empty (ConfigMap exists but operator hasn't set the URL
	// yet). That's fine: the minter returns ("", nil) for an empty URL, which
	// the sender treats as a clean skip.
	frozenURL := webdURL // capture for the closure

	minter := &viewlink.Minter{
		Signer:      signer,
		WebdBaseURL: func() string { return frozenURL },
	}
	return minter, ""
}
