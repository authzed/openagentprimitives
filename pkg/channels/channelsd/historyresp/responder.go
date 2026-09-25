// Package historyresp serves the read_thread_history runner tool over a
// NATS request/reply subject. The runner has no channel credentials, so
// the tool round-trips through channelsd, which resolves the bound kind
// and calls its ConversationReader.
package historyresp

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/nats-io/nats.go"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/channels/channelsd/pipeline"
)

// Responder handles HistoryRequest messages. ReadHistory is the same
// HistoryReadFunc the pipeline uses (wired in internal/cmd/channelsd).
type Responder struct {
	K8s         client.Client
	ReadHistory pipeline.HistoryReadFunc
}

// Start subscribes to every session's history request subject with a
// queue group.
func (r *Responder) Start(ctx context.Context, nc *nats.Conn) error {
	_, err := nc.QueueSubscribe(channelevents.HistorySubscribeSubject, channelevents.HistoryQueueGroup, func(m *nats.Msg) {
		ns, name, ok := channelevents.ParseHistorySubject(m.Subject)
		if !ok {
			r.reply(ctx, m, channelevents.HistoryResponse{Error: "malformed history subject: " + m.Subject})
			return
		}
		var req channelevents.HistoryRequest
		if err := json.Unmarshal(m.Data, &req); err != nil {
			r.reply(ctx, m, channelevents.HistoryResponse{Error: "decode request: " + err.Error()})
			return
		}
		r.reply(ctx, m, r.handle(ctx, ns, name, req))
	})
	if err != nil {
		return fmt.Errorf("subscribe %s: %w", channelevents.HistorySubscribeSubject, err)
	}
	return nil
}

func (r *Responder) reply(ctx context.Context, m *nats.Msg, resp channelevents.HistoryResponse) {
	data, err := json.Marshal(resp)
	if err != nil {
		log.FromContext(ctx).Info("historyresp: marshal response failed", "err", err.Error())
		return
	}
	if err := m.Respond(data); err != nil {
		log.FromContext(ctx).Info("historyresp: respond failed", "err", err.Error())
	}
}

// handle services one request for the (ns, name) session named by the
// request subject. It derives the channelKey from that session (never
// from caller-supplied data) so a runner can only read the thread its
// own session is bound to.
func (r *Responder) handle(ctx context.Context, ns, name string, req channelevents.HistoryRequest) channelevents.HistoryResponse {
	var sess spiceboxv1alpha1.AgentSession
	if err := r.K8s.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &sess); err != nil {
		return channelevents.HistoryResponse{Error: fmt.Sprintf("get session %s/%s: %v", ns, name, err)}
	}
	if sess.Spec.InputChannel == nil {
		return channelevents.HistoryResponse{Error: "session has no input channel binding"}
	}
	var ch spiceboxv1alpha1.Channel
	if err := r.K8s.Get(ctx, client.ObjectKey{Namespace: ns, Name: sess.Spec.InputChannel.Name}, &ch); err != nil {
		return channelevents.HistoryResponse{Error: fmt.Sprintf("get channel: %v", err)}
	}

	before := req.BeforeCursor
	if before == "" {
		// Default to the floor cursor so the agent pages strictly older
		// than what is already in its memory.
		before = sess.Annotations[spiceboxv1alpha1.AnnotationBackfilledFromTS]
	}

	page, err := r.ReadHistory(ctx, &ch, sess.Spec.InputChannel.Key, channelkinds.ReadHistoryOpts{
		BeforeTS: before,
		Limit:    req.Limit,
	})
	if err != nil {
		return channelevents.HistoryResponse{Error: "read history: " + err.Error()}
	}

	out := channelevents.HistoryResponse{HasMore: page.HasMore}
	for _, m := range page.Messages {
		name := m.AuthorDisplayName
		if name == "" {
			name = m.AuthorExternalID
		}
		out.Messages = append(out.Messages, channelevents.HistoryResponseMessage{
			AuthorDisplayName: name, Text: m.Text, TS: m.TS,
		})
	}
	if len(page.Messages) > 0 {
		out.OldestCursor = page.Messages[0].TS
	}
	return out
}
