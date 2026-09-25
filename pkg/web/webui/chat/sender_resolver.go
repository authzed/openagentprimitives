// The Registry is the outbound.SenderResolver for the single process-wide
// outbound.Relay started by startRelay (see registry.go), and this file is
// the leak boundary.
//
// browser.Host IGNORES the AgentSession argument its
// SenderFor/SubChannelSenderFor/StreamDeltaSinkFor methods receive — it always
// routes to "this one in-process host", true for `oap agent chat`'s
// single-session TUI but false here, where N conversations are live in one
// webd. Every method below therefore resolves the envelope's AgentSession in
// the registry's live-session table FIRST (entryForSession) and only then
// delegates to that session's own Host. A session that does not resolve —
// foreign namespace, torn down, still mid-wiring — gets (nil, nil), telling
// outbound.Relay to drop the envelope rather than deliver it anywhere. Without
// that scoping, every conversation's messages, plan updates, tool sessions and
// approval prompts reach every other conversation's sink.
package chat

import (
	"context"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/channels/channelsd/outbound"
)

var _ outbound.SenderResolver = (*Registry)(nil)

// entryForSession resolves the live sessionEntry sess belongs to, or
// (nil, false): this registry never wired it (foreign namespace, or a
// Slack/CLI session), it was torn down, or it is still mid-wiring. r.lookup
// treats a reserved-but-unwired slot as absent, so a wiring in flight cannot
// receive envelopes before it has a Host to deliver them to.
func (r *Registry) entryForSession(sess *spiceboxv1alpha1.AgentSession) (*sessionEntry, bool) {
	if sess == nil {
		return nil, false
	}
	return r.lookup(sessionKey{Namespace: sess.Namespace, Name: sess.Name})
}

// SenderFor implements outbound.SenderResolver: scope to the owning
// session's Host, then delegate.
func (r *Registry) SenderFor(ctx context.Context, sess *spiceboxv1alpha1.AgentSession) (channelkinds.Sender, error) {
	entry, ok := r.entryForSession(sess)
	if !ok || entry.senders == nil {
		return nil, nil
	}
	return entry.senders.SenderFor(ctx, sess)
}

// SubChannelSenderFor implements outbound.SenderResolver: scope to the
// owning session's Host, then delegate.
func (r *Registry) SubChannelSenderFor(ctx context.Context, sess *spiceboxv1alpha1.AgentSession, name string) (channelkinds.Sender, error) {
	entry, ok := r.entryForSession(sess)
	if !ok || entry.senders == nil {
		return nil, nil
	}
	return entry.senders.SubChannelSenderFor(ctx, sess, name)
}

// StreamDeltaSinkFor implements outbound.SenderResolver: scope to the
// owning session's Host, then delegate.
func (r *Registry) StreamDeltaSinkFor(ctx context.Context, sess *spiceboxv1alpha1.AgentSession) (channelkinds.StreamDeltaSink, error) {
	entry, ok := r.entryForSession(sess)
	if !ok || entry.senders == nil {
		return nil, nil
	}
	return entry.senders.StreamDeltaSinkFor(ctx, sess)
}
