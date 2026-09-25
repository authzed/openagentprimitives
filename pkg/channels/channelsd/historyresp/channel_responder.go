// This file is the channel-wide read_channel_history path (responder.go holds
// the per-thread read_thread_history sibling), kept separate because it is
// security-sensitive: it enforces the whole-channel opt-in and the info-leakage
// authz matrix server-side, never trusting the request payload for which channel
// to read. The package doc lives in responder.go.

package historyresp

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/nats-io/nats.go"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/clock"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/resolve"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

const channelHistoryDefaultWindow = 7 * 24 * time.Hour

// MemberChecker is the read-only SpiceDB check the channel-history responder
// needs. *pkg/authz/spicedb.Client satisfies it via LookupSubjectIncludes.
type MemberChecker interface {
	LookupSubjectIncludes(ctx context.Context, subjectRef string, canonicalID identity.CanonicalUserID) (bool, error)
}

// ChannelResponder serves read_channel_history over NATS, enforcing the
// whole-channel security model server-side: the CR must opt in; the read is
// scoped to the session's INPUT channel, never caller args; and with
// info-leakage gating on, the session initiator must be authorized (SpiceDB) to
// view the channel — with it off, the read is allowed only when the output
// channel equals the input channel.
type ChannelResponder struct {
	K8s   client.Client
	Authz MemberChecker
	Clock clock.Clock

	// Resolve resolves a Channel to its credentials Secret + kind impl.
	// Defaults to resolve.ForChannel when nil; tests inject a fake
	// returning a fake ChannelHistoryReader kind.
	Resolve func(ctx context.Context, c client.Client, ch *spiceboxv1alpha1.Channel) (*corev1.Secret, channelkinds.Kind, error)
}

func (r *ChannelResponder) Start(ctx context.Context, nc *nats.Conn) error {
	_, err := nc.QueueSubscribe(channelevents.ChannelHistorySubscribeSubject, channelevents.ChannelHistoryQueueGroup, func(m *nats.Msg) {
		ns, name, ok := channelevents.ParseChannelHistorySubject(m.Subject)
		if !ok {
			r.reply(ctx, m, channelevents.HistoryResponse{Error: "malformed channel-history subject: " + m.Subject})
			return
		}
		var req channelevents.ChannelHistoryRequest
		if err := json.Unmarshal(m.Data, &req); err != nil {
			r.reply(ctx, m, channelevents.HistoryResponse{Error: "decode request: " + err.Error()})
			return
		}
		r.reply(ctx, m, r.handle(ctx, ns, name, req))
	})
	if err != nil {
		return fmt.Errorf("subscribe %s: %w", channelevents.ChannelHistorySubscribeSubject, err)
	}
	return nil
}

func (r *ChannelResponder) reply(ctx context.Context, m *nats.Msg, resp channelevents.HistoryResponse) {
	data, err := json.Marshal(resp)
	if err != nil {
		log.FromContext(ctx).Info("channelhistoryresp: marshal failed", "err", err.Error())
		return
	}
	if err := m.Respond(data); err != nil {
		log.FromContext(ctx).Info("channelhistoryresp: respond failed", "err", err.Error())
	}
}

// resolveFunc returns r.Resolve, falling back to resolve.ForChannel in
// production (tests inject a fake).
func (r *ChannelResponder) resolveFunc() func(ctx context.Context, c client.Client, ch *spiceboxv1alpha1.Channel) (*corev1.Secret, channelkinds.Kind, error) {
	if r.Resolve != nil {
		return r.Resolve
	}
	return resolve.ForChannel
}

// handle services one request for the (ns, name) session named by the
// request subject. The channel is ALWAYS derived from the session's
// InputChannel binding — never from req — so a caller can only ever read the
// channel its own session is bound to.
func (r *ChannelResponder) handle(ctx context.Context, ns, name string, req channelevents.ChannelHistoryRequest) channelevents.HistoryResponse {
	var sess spiceboxv1alpha1.AgentSession
	if err := r.K8s.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &sess); err != nil {
		return channelevents.HistoryResponse{Error: fmt.Sprintf("get session %s/%s: %v", ns, name, err)}
	}
	if sess.Spec.InputChannel == nil {
		return channelevents.HistoryResponse{Error: "session has no input channel binding"}
	}
	binding := sess.Spec.InputChannel

	var ch spiceboxv1alpha1.Channel
	if err := r.K8s.Get(ctx, client.ObjectKey{Namespace: ns, Name: binding.Name}, &ch); err != nil {
		return channelevents.HistoryResponse{Error: fmt.Sprintf("get channel: %v", err)}
	}
	if ch.Spec.ChannelHistory == nil || !ch.Spec.ChannelHistory.Enabled {
		return channelevents.HistoryResponse{Error: "channel history is not enabled for this channel"}
	}

	sec, k, err := r.resolveFunc()(ctx, r.K8s, &ch)
	if err != nil {
		return channelevents.HistoryResponse{Error: fmt.Sprintf("resolve channel kind: %v", err)}
	}
	reader, ok := k.(channelkinds.ChannelHistoryReader)
	if !ok {
		return channelevents.HistoryResponse{Error: "channel kind does not support channel history"}
	}

	if errStr := r.gate(ctx, &sess, &ch, binding, reader); errStr != "" {
		return channelevents.HistoryResponse{Error: errStr}
	}

	opts := r.clampOpts(&ch, reader.ChannelHistoryBounds(), req)
	page, err := reader.ReadChannelHistory(ctx, channelkinds.Deps{Channel: &ch, Secret: sec, K8sClient: r.K8s}, binding, opts)
	if err != nil {
		return channelevents.HistoryResponse{Error: "read channel history: " + err.Error()}
	}

	out := channelevents.HistoryResponse{HasMore: page.HasMore}
	for _, m := range page.Messages {
		dn := m.AuthorDisplayName
		if dn == "" {
			dn = m.AuthorExternalID
		}
		out.Messages = append(out.Messages, channelevents.HistoryResponseMessage{AuthorDisplayName: dn, Text: m.Text, TS: m.TS})
	}
	switch {
	case page.NextCursor != "":
		out.OldestCursor = page.NextCursor
	case len(page.Messages) > 0:
		out.OldestCursor = page.Messages[0].TS
	}
	return out
}

// gate enforces the info-leakage authz matrix. Returns "" when allowed, else
// a user-facing withheld reason. Fails closed on every ambiguous or errored
// path: an AgentClass Get error, a missing subject ref, a missing session
// initiator, a missing Authz client, or a SpiceDB error all withhold rather
// than allow.
func (r *ChannelResponder) gate(
	ctx context.Context, sess *spiceboxv1alpha1.AgentSession, ch *spiceboxv1alpha1.Channel,
	binding *spiceboxv1alpha1.ChannelBinding, reader channelkinds.ChannelHistoryReader,
) string {
	leakageOn := false
	if ch.Spec.AgentClass != "" {
		var class spiceboxv1alpha1.AgentClass
		if err := r.K8s.Get(ctx, client.ObjectKey{Namespace: ch.Namespace, Name: ch.Spec.AgentClass}, &class); err != nil {
			return "resolve agent class for gating: " + err.Error() // fail closed
		}
		leakageOn = class.Spec.GetAuthz().InformationLeakage.ResolvedMode() != "disabled"
	}

	if !leakageOn {
		if binding.SameChannelAs(sess.Spec.OutputChannel) {
			return ""
		}
		return "channel history is withheld: the output channel differs from the input channel and info-leakage gating is off"
	}

	// info-leakage on: the session initiator must be authorized to view the channel.
	ref, ok := reader.ChannelViewSubjectRef(binding)
	if !ok {
		return "channel history is withheld: cannot determine channel authorization"
	}
	canonical := spiceboxv1alpha1.StartedByCanonical(sess)
	if canonical.IsZero() {
		return "channel history is withheld: no session initiator to authorize"
	}
	if r.Authz == nil {
		return "channel history is withheld: authorization checker unavailable"
	}
	allowed, err := r.Authz.LookupSubjectIncludes(ctx, ref, canonical)
	if err != nil {
		return "channel history authorization check failed: " + err.Error() // fail closed
	}
	if !allowed {
		return "channel history is withheld: you are not authorized to view this channel"
	}
	return ""
}

// clampOpts converts the request into time-bounded opts, clamped to
// (CR ceiling ∩ kind bounds).
func (r *ChannelResponder) clampOpts(ch *spiceboxv1alpha1.Channel, bounds channelkinds.ChannelHistoryBounds, req channelevents.ChannelHistoryRequest) channelkinds.ChannelHistoryOpts {
	now := r.Clock.Now()

	maxLB := bounds.MaxLookback
	if cr := ch.Spec.ChannelHistory.MaxLookback; cr != nil && cr.Duration > 0 && cr.Duration < maxLB {
		maxLB = cr.Duration
	}
	reqLB := channelHistoryDefaultWindow
	if reqLB > maxLB {
		reqLB = maxLB
	}
	if req.LookbackDays > 0 {
		d := time.Duration(req.LookbackDays) * 24 * time.Hour
		if d < maxLB {
			reqLB = d
		} else {
			reqLB = maxLB
		}
	}
	notBefore := now.Add(-reqLB)
	notAfter := now
	if bounds.SupportsDateRange {
		if t, err := time.Parse(time.RFC3339, req.Since); err == nil && t.After(notBefore) {
			notBefore = t
		}
		if t, err := time.Parse(time.RFC3339, req.Until); err == nil && t.Before(notAfter) {
			notAfter = t
		}
	}

	limit := bounds.MaxMessages
	if cr := ch.Spec.ChannelHistory.MaxMessages; cr != nil && int(*cr) > 0 && int(*cr) < limit {
		limit = int(*cr)
	}
	if req.Limit > 0 && req.Limit < limit {
		limit = req.Limit
	}

	return channelkinds.ChannelHistoryOpts{NotBefore: notBefore, NotAfter: notAfter, Limit: limit, BeforeCursor: req.BeforeCursor}
}
