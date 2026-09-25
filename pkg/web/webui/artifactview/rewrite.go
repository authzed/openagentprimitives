package artifactview

import (
	"context"
	"fmt"

	"github.com/authzed/openagentprimitives/pkg/channels/channelassets"
	assetregistry "github.com/authzed/openagentprimitives/pkg/channels/channelassets/registry"
)

// rewriteArtifactRefs rewrites a primary render's kind-specific
// `artifact:HANDLE` references to token-gated same-origin asset URLs, resolving
// each handle via av.ResolveAssetURL — SCOPED to the primary's own (ns, sess).
//
// Dispatch is generic and never branches on a kind name: the render's kind comes
// from av.RenderKind, then the channelassets registry, then a type-assert to
// channelassets.RefRewriter. A kind whose renderer doesn't implement it
// (unregistered, or with no reference concept) has content returned UNCHANGED,
// not an error.
//
// A handle that does not resolve within (ns, sess) has its reference DROPPED per
// the kind's own RewriteRefs semantics; the rest of the primary is still served.
// The only hard failure is an unresolvable render kind — the caller logs that and
// serves the untransformed bytes rather than failing the request.
func rewriteArtifactRefs(ctx context.Context, av Deps, ns, sess, renderName string, content []byte) ([]byte, error) {
	kind, err := av.RenderKind(ctx, ns, sess, renderName)
	if err != nil {
		return nil, fmt.Errorf("artifactview: resolve render kind for asset-ref rewrite: %w", err)
	}
	renderer, ok := assetregistry.ByKind(kind)
	if !ok {
		return content, nil
	}
	rw, ok := renderer.(channelassets.RefRewriter)
	if !ok {
		return content, nil
	}
	out, _ := rw.RewriteRefs(content, func(handle string) (string, bool) {
		url, ok, rerr := av.ResolveAssetURL(ctx, ns, sess, handle)
		if rerr != nil {
			av.Logger().Error(rerr, "artifactview: ResolveAssetURL errored; dropping artifact: ref",
				"ns", ns, "sess", sess, "handle", handle)
			return "", false
		}
		if !ok {
			// Degrade, not fail — but never silently: a broken or foreign
			// artifact: ref must be diagnosable, not vanish without a trace.
			av.Logger().Info("artifactview: artifact: ref did not resolve in this session; dropping from live-view",
				"ns", ns, "sess", sess, "handle", handle)
			return "", false
		}
		return url, true
	})
	return out, nil
}
