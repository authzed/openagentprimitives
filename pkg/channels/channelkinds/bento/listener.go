package bento

import (
	"context"
	"fmt"
	"sync"

	"github.com/warpstreamlabs/bento/public/service"
	"sigs.k8s.io/controller-runtime/pkg/log"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

// listener owns one *service.Stream per Channel CR. The map is keyed
// by NamespacedName.String() and each entry tracks the source YAML
// so Reconcile knows whether to recreate the stream.
type listener struct {
	deps    channelkinds.Deps
	mu      sync.Mutex
	streams map[string]*streamEntry
}

type streamEntry struct {
	stream *service.Stream
	cancel context.CancelFunc
	yaml   string // the stream's currently-running YAML; identity key for "needs rebuild"
}

func newListener(deps channelkinds.Deps) channelkinds.Listener {
	return &listener{
		deps:    deps,
		streams: map[string]*streamEntry{},
	}
}

// Start is a no-op; Reconcile is the meaningful entry point and is
// invoked per Channel CR by channelsd.
func (l *listener) Start(_ context.Context) error { return nil }

// Stop tears down every running stream.
func (l *listener) Stop(ctx context.Context) error {
	// Snapshot under the lock, then release before calling Stop on
	// each stream so we don't hold mu across the (potentially slow)
	// stream shutdown.
	l.mu.Lock()
	entries := l.streams
	l.streams = map[string]*streamEntry{}
	l.mu.Unlock()
	logger := log.FromContext(ctx)
	for key, e := range entries {
		e.cancel()
		if err := e.stream.Stop(ctx); err != nil {
			logger.V(1).Info("bento listener: stream Stop errored on teardown",
				"key", key, "err", err.Error())
		}
	}
	return nil
}

// Reconcile ensures the listener owns exactly one running Bento
// stream that matches the supplied Channel's spec. New spec → start;
// updated spec → tear-down + restart; unchanged → no-op.
func (l *listener) Reconcile(ctx context.Context, ch *spiceboxv1alpha1.Channel) error {
	yaml, err := buildStreamYAML(ch)
	if err != nil {
		return err
	}
	key := ch.Namespace + "/" + ch.Name
	logger := log.FromContext(ctx)
	l.mu.Lock()
	prev, ok := l.streams[key]
	l.mu.Unlock()
	if ok && prev.yaml == yaml {
		return nil
	}
	if ok {
		prev.cancel()
		if err := prev.stream.Stop(ctx); err != nil {
			logger.V(1).Info("bento listener: stream Stop errored on restart",
				"key", key, "err", err.Error())
		}
	}

	b := service.NewStreamBuilder()
	if err := b.SetYAML(yaml); err != nil {
		return fmt.Errorf("listener: SetYAML for %s: %w", key, err)
	}
	stream, err := b.Build()
	if err != nil {
		return fmt.Errorf("listener: build stream for %s: %w", key, err)
	}
	runCtx, cancel := context.WithCancel(ctx)
	go func() {
		if err := stream.Run(runCtx); err != nil {
			// The stream errored out (interval ended, mapping panic, …).
			// Logging here keeps channelsd informed; reflecting it onto the
			// CR's status is the operator's job and is not wired yet. Use the
			// structured logger, not stdout, so the failing stream is grep-able
			// by channel ns/name+key.
			log.FromContext(runCtx).Error(err, "bento listener: stream exited",
				"channel", key, "namespace", ch.Namespace, "name", ch.Name)
		}
	}()

	l.mu.Lock()
	l.streams[key] = &streamEntry{stream: stream, cancel: cancel, yaml: yaml}
	l.mu.Unlock()
	return nil
}

// Forget tears down the stream for the supplied Channel name and
// drops the tracking entry. Used when a Channel CR is deleted.
func (l *listener) Forget(ctx context.Context, namespace, name string) {
	key := namespace + "/" + name
	l.mu.Lock()
	e, ok := l.streams[key]
	delete(l.streams, key)
	l.mu.Unlock()
	if ok {
		e.cancel()
		if err := e.stream.Stop(ctx); err != nil {
			log.FromContext(ctx).V(1).Info("bento listener: stream Stop errored on forget",
				"key", key, "err", err.Error())
		}
	}
}
