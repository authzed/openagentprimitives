package channelkinds

import (
	"context"

	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// ShowsAssistantStream reports whether the session's AgentClass opts into
// rendering the agent's live LLM stream (spec.channels.showAssistantStream).
//
// Fail-closed: a nil session, an empty class, a lookup error, or no channels
// config all DISABLE streaming. The raw assistant stream is verbose,
// debug-only, free-form model narrative — it must never leak to a channel
// (Slack or the built-in web chat) unless an AgentClass explicitly turns it on.
// Every stream consumer gates its StreamDeltaSink on this so the flag is
// honored uniformly across channel kinds.
func ShowsAssistantStream(ctx context.Context, cli client.Client, sess *spiceboxv1alpha1.AgentSession) bool {
	if sess == nil || sess.Spec.Class == "" || cli == nil {
		return false
	}
	var class spiceboxv1alpha1.AgentClass
	if err := cli.Get(ctx, client.ObjectKey{Namespace: sess.Namespace, Name: sess.Spec.Class}, &class); err != nil {
		return false
	}
	if class.Spec.Channels == nil {
		return false
	}
	return class.Spec.Channels.ShowAssistantStream
}
