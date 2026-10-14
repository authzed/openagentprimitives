package main

import (
	"context"

	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	channelregistry "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/registry"
	"github.com/authzed/openagentprimitives/pkg/web/webui/artifactview"
)

// originLinksFrom walks the session's input then output channel bindings,
// dispatches each through its kind's OriginLinkProvider (the registry, never an
// `if kind == …` switch), and returns the resolved links de-duplicated by Href
// in input-first order. A kind implementing no OriginLinkProvider, or a binding
// with no resolvable origin yet (a Slack thread not rooted, a permalink that
// failed to resolve), contributes nothing. Pure function of the session so it is
// unit-testable without a cluster.
func originLinksFrom(sess *spiceboxv1alpha1.AgentSession) []artifactview.OriginLink {
	var out []artifactview.OriginLink
	seen := map[string]bool{}
	add := func(b *spiceboxv1alpha1.ChannelBinding) {
		if b == nil {
			return
		}
		k, ok := channelregistry.Get(b.Kind)
		if !ok {
			return
		}
		p, ok := k.(channelkinds.OriginLinkProvider)
		if !ok {
			return
		}
		u, ok := p.OriginLink(b)
		if !ok || u == "" || seen[u] {
			return
		}
		seen[u] = true
		out = append(out, artifactview.OriginLink{Href: u, ChannelKind: b.Kind})
	}
	add(sess.Spec.InputChannel)
	add(sess.Spec.OutputChannel)
	return out
}

// OriginLinks implements artifactview.Deps: it loads the session and derives its
// origin links. A Get failure logs and returns no links — never an error, the
// toolbar simply shows nothing — matching ChannelKind's best-effort contract.
func (d *artifactViewDeps) OriginLinks(ctx context.Context, ns, sess string) []artifactview.OriginLink {
	var cur spiceboxv1alpha1.AgentSession
	if err := d.K8s().Get(ctx, client.ObjectKey{Namespace: ns, Name: sess}, &cur); err != nil {
		d.logger.Error(err, "artifactview: OriginLinks session lookup failed; no origin links",
			"ns", ns, "sess", sess)
		return nil
	}
	return originLinksFrom(&cur)
}
