// monitoringRelay subscribes to the cluster-wide
// channelevents.MonitoringEventSubject and fans every framework
// MonitoringEvent out to every role=monitoring Channel via the kind's
// MonitoringSender.
//
// Mirrors the SessionAttached subscription pattern in main.go: a fixed
// subject, a plain JSON payload (no Envelope), one publisher (the
// operator's monitoring watcher) to many subscriber-side deliveries.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/nats-io/nats.go"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/registry"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/resolve"
)

// monitoringRelay delivers MonitoringEvents to every role=monitoring Channel.
type monitoringRelay struct {
	cli client.Client
	nc  *nats.Conn

	mu  sync.Mutex
	sub *nats.Subscription
}

func newMonitoringRelay(cli client.Client, nc *nats.Conn) *monitoringRelay {
	return &monitoringRelay{cli: cli, nc: nc}
}

// Start subscribes to MonitoringEventSubject. Safe to call once.
func (r *monitoringRelay) Start(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.sub != nil {
		return nil
	}
	sub, err := r.nc.Subscribe(channelevents.MonitoringEventSubject, func(m *nats.Msg) {
		r.handle(ctx, m)
	})
	if err != nil {
		return fmt.Errorf("subscribe %s: %w", channelevents.MonitoringEventSubject, err)
	}
	r.sub = sub
	return nil
}

// Stop drains the subscription and clears r.sub so a subsequent Start
// re-subscribes instead of returning immediately via the r.sub != nil guard.
func (r *monitoringRelay) Stop(_ context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.sub == nil {
		return nil
	}
	err := r.sub.Drain()
	r.sub = nil
	return err
}

// handle decodes one MonitoringEvent and fans it out to every
// role=monitoring Channel.
func (r *monitoringRelay) handle(ctx context.Context, m *nats.Msg) {
	logger := log.FromContext(ctx).WithName("monitoring-relay")

	var ev channelevents.MonitoringEvent
	if err := json.Unmarshal(m.Data, &ev); err != nil {
		logger.Info("drop, malformed monitoring event", "err", err.Error())
		return
	}
	if err := ev.Validate(); err != nil {
		logger.Info("drop, invalid monitoring event", "err", err.Error())
		return
	}

	var list spiceboxv1alpha1.ChannelList
	if err := r.cli.List(ctx, &list); err != nil {
		logger.Error(err, "list channels for monitoring fan-out")
		return
	}
	for i := range list.Items {
		ch := &list.Items[i]
		if ch.Spec.Role != spiceboxv1alpha1.ChannelRoleMonitoring {
			continue
		}
		sender, err := r.monitoringSenderFor(ctx, ch)
		if err != nil {
			logger.Info("monitoring fan-out: resolve sender failed",
				"channel", ch.Namespace+"/"+ch.Name, "err", err.Error())
			continue
		}
		if sender == nil {
			// The kind said SupportsMonitoring() but handed back nil. That is
			// an internal contradiction in the kind (the two are one fact —
			// see channelkinds.Kind.SupportsMonitoring), not a configuration
			// the operator chose, so it is LOUD. It also matters beyond this
			// loop: the credential-update watcher trusts SupportsMonitoring to
			// decide whether an admin can be reached at all, so a kind that
			// lies here makes that watcher claim a delivery nobody received.
			logger.Info("monitoring fan-out: kind claims SupportsMonitoring but returned a nil MonitoringSender (kind bug); dropping event",
				"channel", ch.Namespace+"/"+ch.Name, "kind", ch.Spec.Kind)
			continue
		}
		if err := sender.SendMonitoring(ctx, ev); err != nil {
			logger.Info("monitoring fan-out: send failed",
				"channel", ch.Namespace+"/"+ch.Name, "err", err.Error())
		}
	}
}

// monitoringSenderFor resolves a fresh MonitoringSender for ch. It does
// not cache: monitoring Channels are few and events are infrequent
// (transitions only), so resolving fresh keeps Channel spec edits
// effective without a cache-invalidation path.
//
// A kind that does not do monitoring at all (bento, local, builtin) is
// filtered by SupportsMonitoring BEFORE the Secret is read — the same
// predicate pkg/channels/channelsd/pipeline's deliverability pre-check uses, which is
// what keeps the two in agreement.
//
// That case returns an ERROR, not (nil, nil): a Channel labelled
// role=monitoring whose kind cannot deliver monitoring is a misconfiguration
// an operator should hear about, and it is exactly the configuration that used
// to be an expected silent skip while the credential-update watcher counted it
// as a delivered card. The cost is one log line per event for as long as the
// misconfigured Channel exists — deliberate. A nil sender from a kind that DID
// claim support never reaches here as an error; the caller treats that as a
// kind bug.
func (r *monitoringRelay) monitoringSenderFor(ctx context.Context, ch *spiceboxv1alpha1.Channel) (channelkinds.MonitoringSender, error) {
	if k, ok := registry.Get(ch.Spec.Kind); ok && !k.SupportsMonitoring() {
		return nil, fmt.Errorf("kind %q does not deliver monitoring events", ch.Spec.Kind)
	}
	sec, k, err := resolve.ForChannel(ctx, r.cli, ch)
	if err != nil {
		return nil, err
	}
	return k.NewMonitoringSender(channelkinds.Deps{
		Channel:   ch,
		Secret:    sec,
		K8sClient: r.cli,
		NATSPublish: func(subj string, p []byte) error {
			if r.nc == nil {
				return fmt.Errorf("monitoring relay: NATS unavailable")
			}
			return r.nc.Publish(subj, p)
		},
	}), nil
}
