package main

import (
	"context"
	"net/url"

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
		link, ok := p.OriginLink(b)
		if !ok || link.URL == "" || seen[link.URL] {
			return
		}
		// Generic backstop: these hrefs derive from the user-writable
		// External map and are rendered as <a href>. A kind vouches for its
		// own host (slack: *.slack.com), but every link must at least be an
		// https URL — never a javascript:/data: scheme or a relative ref —
		// before it reaches the toolbar. Fail-closed: drop anything that is
		// not.
		if !isHTTPSOriginHref(link.URL) {
			return
		}
		seen[link.URL] = true
		out = append(out, artifactview.OriginLink{Href: link.URL, Label: link.Label, Icon: link.Icon})
	}
	add(sess.Spec.InputChannel)
	add(sess.Spec.OutputChannel)
	return out
}

// isHTTPSOriginHref reports whether href is an https URL with a host — the
// scheme backstop every origin link must pass before it is rendered as an
// <a href>, independent of any per-kind host check. Rejects javascript:/data:
// schemes, plain http, and relative refs.
func isHTTPSOriginHref(href string) bool {
	parsed, err := url.Parse(href)
	return err == nil && parsed.Scheme == "https" && parsed.Host != ""
}

// OriginLinks implements artifactview.Deps: it loads the session and derives its
// origin links. A Get failure logs and returns no links — never an error, the
// toolbar simply shows nothing. Best-effort by design: an origin link is a
// convenience affordance, never a gate, so no failure here is surfaced as one.
func (d *artifactViewDeps) OriginLinks(ctx context.Context, ns, sess string) []artifactview.OriginLink {
	var cur spiceboxv1alpha1.AgentSession
	if err := d.K8s().Get(ctx, client.ObjectKey{Namespace: ns, Name: sess}, &cur); err != nil {
		d.logger.Error(err, "artifactview: OriginLinks session lookup failed; no origin links",
			"ns", ns, "sess", sess)
		return nil
	}
	return originLinksFrom(&cur)
}
