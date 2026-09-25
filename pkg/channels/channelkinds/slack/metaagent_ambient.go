package slack

import (
	"context"
	"encoding/json"
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkey"
	"github.com/authzed/openagentprimitives/pkg/metaagent"
)

// maybeTriggerAmbient is the THIRD metaagent trigger: an ordinary inbound turn
// that nobody addressed to the metaagent.
//
// # Ordering is the whole design
//
// The prefilter runs FIRST, before anything that costs. It is a pure string
// check over the turn text, and it rejects the overwhelming majority of
// messages — so the common path through this function is a few string
// comparisons and a return. Only a turn that looks like intent pays for a
// session lookup.
//
// Reversing that order would put a Kubernetes List on every Slack message in
// every thread with an active session, which is a per-message API call to
// answer a question the prefilter answers for free. The whole reason an ambient
// trigger is affordable at all is that the expensive work happens after the
// cheap filter, not before it.
//
// # It can only ever ADD a classification
//
// This runs alongside normal message routing, never instead of it. The turn has
// already gone to the session's runner by the time this is called; publishing a
// metaagent_request is additive. A failure here therefore logs and returns —
// the user's actual message is unaffected, and surfacing an error for a
// classification nobody asked for would be noise.
//
// The one exception is the SESSION-LOOKUP failure path, which stays silent by
// the same reasoning: an ordinary sentence that happened to trip a keyword is
// not a request, and telling the user their non-request failed would be worse
// than saying nothing.
func (l *slackListener) maybeTriggerAmbient(ctx context.Context, userID, channelID, threadTS, ts, text string) {
	if text == "" {
		return
	}

	// CHEAP FIRST. Nothing below this line runs for an ordinary turn.
	//
	// mentioned=false: an explicit mention has its own path (handleMetaagentMention)
	// and reaches this function never — so the prefilter here only ever judges
	// ambient text.
	if !metaagent.ShouldExtract(text, false, l.metaagentObserver()) {
		return
	}

	if l.deps.K8sClient == nil || l.deps.Channel == nil {
		return
	}

	anchor := threadTS
	if anchor == "" {
		anchor = ts
	}
	logger := log.FromContext(ctx).WithValues("user", userID, "channel", channelID, "threadTS", anchor)

	active := l.activeSessionForThread(ctx, channelID, anchor)
	if active == nil {
		// No session here, or the lookup failed. Either way there is nothing to
		// classify against and nobody asked for anything — stay silent.
		return
	}

	// The RESOLVED trigger, off the session's effective settings — never the
	// AgentClass spec directly. A class reading its own spec could opt itself
	// past a tier default by being the only value anyone consults, which is the
	// same reason the plan gate reads EffectiveSettings.
	trigger := resolvedMetaagentTrigger(active)
	if trigger == spiceboxv1alpha1.MetaagentTriggerMention {
		// Ambient is off for this session. This is the default, and it is the
		// reason enabling the feature is a deliberate act.
		return
	}

	requester, cerr := l.resolveCanonicalForSlackUser(ctx, userID)
	if cerr != nil {
		logger.Info("metaagent_ambient: canonicalize requester failed; dropping",
			"err", cerr.Error())
		return
	}

	shadow := trigger == spiceboxv1alpha1.MetaagentTriggerShadow
	payload, err := json.Marshal(map[string]any{
		"requester": requester,
		"text":      text,
		"ambient":   true,
		"shadow":    shadow,
	})
	if err != nil {
		logger.Info("metaagent_ambient: marshal payload failed", "err", err.Error())
		return
	}
	subject := fmt.Sprintf("ap.session.%s.%s.in.metaagent_request", active.Namespace, active.Name)
	if err := l.deps.NATSPublish(subject, payload); err != nil {
		// Logged, not surfaced: the user's message reached the agent normally,
		// and they never asked for a classification. A publish failure costs a
		// classification nobody requested.
		logger.Info("metaagent_ambient: publish failed", "subject", subject, "err", err.Error())
		return
	}
	logger.Info("metaagent_ambient: published metaagent_request",
		"session", active.Namespace+"/"+active.Name, "shadow", shadow)
}

// activeSessionForThread resolves the most-recently-created active session for
// a thread, or nil. Shared by the mention and ambient paths so the two cannot
// disagree about which session a thread belongs to.
func (l *slackListener) activeSessionForThread(ctx context.Context, channelID, anchor string) *spiceboxv1alpha1.AgentSession {
	keyHash := channelkey.LabelValue("thread:" + channelID + ":" + anchor)

	var sessions spiceboxv1alpha1.AgentSessionList
	if err := l.deps.K8sClient.List(ctx, &sessions,
		client.InNamespace(l.deps.Channel.Namespace),
		client.MatchingLabels{
			spiceboxv1alpha1.LabelChannelName: l.deps.Channel.Name,
			spiceboxv1alpha1.LabelChannelKey:  keyHash,
		},
	); err != nil {
		return nil
	}

	var active *spiceboxv1alpha1.AgentSession
	for i := range sessions.Items {
		s := &sessions.Items[i]
		switch s.Status.Phase {
		case spiceboxv1alpha1.AgentSessionPhasePending,
			spiceboxv1alpha1.AgentSessionPhaseRunning,
			spiceboxv1alpha1.AgentSessionPhaseIdle,
			spiceboxv1alpha1.AgentSessionPhaseAwaitingApproval,
			spiceboxv1alpha1.AgentSessionPhaseAwaitingDecision:
			if active == nil || s.CreationTimestamp.After(active.CreationTimestamp.Time) {
				active = s
			}
		}
	}
	return active
}

// resolvedMetaagentTrigger reads the trigger off the session's RESOLVED
// settings, defaulting to mention.
//
// Fail-safe on every absent link in the chain: no status, no effective
// settings, no authz block or no metaagent block all resolve to mention, which
// is the narrowest value. A session whose settings have not resolved yet does
// not silently start classifying every turn.
func resolvedMetaagentTrigger(s *spiceboxv1alpha1.AgentSession) string {
	if s == nil || s.Status.EffectiveSettings == nil {
		return spiceboxv1alpha1.MetaagentTriggerMention
	}
	return s.Status.EffectiveSettings.Authz.Metaagent.GetMetaagentTrigger()
}

// metaagentObserver returns the prefilter's shadow counter.
//
// nil for now: the counter's storage is the module's own concern and lands with
// the reader that consumes it. ShouldExtract tolerates a nil observer by
// design — measurement must never break the path it measures — so wiring it
// later changes nothing here.
func (l *slackListener) metaagentObserver() metaagent.Observer { return nil }
