package main

import (
	"context"
	"fmt"
	"reflect"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// relayCachedTypes are the kinds the outbound relay reads on its hot path —
// the AgentSession on every relayed envelope, its AgentClass per stream delta,
// its Channel on a delivery outcome. Nothing else is cached: an allowlist, so
// a new read the relay grows goes to the API server until someone decides it
// belongs here, and Secrets are never held in memory cluster-wide.
var relayCachedTypes = []client.Object{
	&spiceboxv1alpha1.AgentSession{},
	&spiceboxv1alpha1.AgentClass{},
	&spiceboxv1alpha1.Channel{},
}

// relayFreshWindow is how long reads of an object this client just wrote go to
// the API server instead of the cache, unless the cache shows the write first.
// Watch delivery is normally well under a second; the window only has to
// outlast that.
const relayFreshWindow = 10 * time.Second

// newRelayCache starts an informer cache over relayCachedTypes and waits for
// it to sync. ReaderFailOnMissingInformer keeps it from lazily starting an
// informer for a type nobody registered — a Get of an uncached type through
// the cache is an error, not a silent cluster-wide watch.
func newRelayCache(ctx context.Context, restCfg *rest.Config, scheme *runtime.Scheme, syncTimeout time.Duration) (cache.Cache, error) {
	byObject := make(map[client.Object]cache.ByObject, len(relayCachedTypes))
	for _, obj := range relayCachedTypes {
		byObject[obj] = cache.ByObject{}
	}
	c, err := cache.New(restCfg, cache.Options{
		Scheme:                      scheme,
		ByObject:                    byObject,
		ReaderFailOnMissingInformer: true,
		DefaultTransform:            cache.TransformStripManagedFields(),
	})
	if err != nil {
		return nil, fmt.Errorf("relay cache: new: %w", err)
	}
	for _, obj := range relayCachedTypes {
		if _, err := c.GetInformer(ctx, obj); err != nil {
			return nil, fmt.Errorf("relay cache: informer for %T: %w", obj, err)
		}
	}
	go func() {
		// Start blocks until ctx is canceled. An error means the informers
		// never started, so WaitForCacheSync below fails startup too; logged
		// here so the cause is next to the symptom.
		if err := c.Start(ctx); err != nil {
			ctrllog.FromContext(ctx).Error(err, "relay cache failed to start")
		}
	}()
	syncCtx, cancel := context.WithTimeout(ctx, syncTimeout)
	defer cancel()
	if !c.WaitForCacheSync(syncCtx) {
		return nil, fmt.Errorf("relay cache: did not sync within %s (check list/watch RBAC on %v)", syncTimeout, relayCachedTypeNames())
	}
	return c, nil
}

func relayCachedTypeNames() []string {
	names := make([]string, 0, len(relayCachedTypes))
	for _, obj := range relayCachedTypes {
		names = append(names, reflect.TypeOf(obj).Elem().Name())
	}
	return names
}

// relayClient serves the outbound relay's reads of relayCachedTypes from an
// informer cache and everything else — every write, every other read — from
// the direct client it embeds.
//
// The relay handles every session's outbound traffic on ONE goroutine, and
// used to spend at least one API-server round trip per envelope. Under a burst
// of busy sessions that round trip, rate-limited client side, was the whole
// throughput ceiling.
//
// Read-your-writes: the relay patches the sessions it reads (the opening-sent
// marker, the thread it anchored on) and its very next envelope for that
// session must see the patch, or it opens a second thread. So a write marks the
// object, and reads of it go direct until the cache shows that write or
// relayFreshWindow passes. A cache miss also goes direct: a session created a
// moment ago may not have reached the cache yet, and dropping its first
// envelopes as "not found" would be worse than the round trip.
//
// Deliberately scoped to the relay. Other channelsd paths read-modify-write
// (status updates with optimistic concurrency) and gain nothing from a cache
// but stale-read conflicts.
type relayClient struct {
	client.Client
	cache  client.Reader
	cached map[reflect.Type]bool
	now    func() time.Time

	mu    sync.Mutex
	fresh map[freshKey]freshMark
}

type freshKey struct {
	typ reflect.Type
	key client.ObjectKey
}

type freshMark struct {
	resourceVersion string // what this client's write produced; "" if unknown
	until           time.Time
}

func newRelayClient(direct client.Client, cached client.Reader) *relayClient {
	c := &relayClient{
		Client: direct,
		cache:  cached,
		cached: make(map[reflect.Type]bool, len(relayCachedTypes)),
		now:    time.Now,
		fresh:  map[freshKey]freshMark{},
	}
	for _, obj := range relayCachedTypes {
		c.cached[reflect.TypeOf(obj)] = true
	}
	return c
}

func (c *relayClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	typ := reflect.TypeOf(obj)
	if !c.cached[typ] {
		return c.Client.Get(ctx, key, obj, opts...)
	}
	if err := c.cache.Get(ctx, key, obj, opts...); err != nil {
		if apierrors.IsNotFound(err) {
			return c.Client.Get(ctx, key, obj, opts...)
		}
		return err
	}
	if c.needsDirectRead(freshKey{typ: typ, key: key}, obj.GetResourceVersion()) {
		return c.Client.Get(ctx, key, obj, opts...)
	}
	return nil
}

// needsDirectRead reports whether a cached copy at cachedRV may predate this
// client's own write to the object, clearing the mark once it cannot.
func (c *relayClient) needsDirectRead(k freshKey, cachedRV string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	m, ok := c.fresh[k]
	if !ok {
		return false
	}
	if (m.resourceVersion != "" && cachedRV == m.resourceVersion) || !c.now().Before(m.until) {
		delete(c.fresh, k)
		return false
	}
	return true
}

// wrote marks obj as written by this client. The write's response has already
// been decoded into obj, so its resourceVersion is the one the cache has to
// show before it can be trusted again.
func (c *relayClient) wrote(obj client.Object) {
	typ := reflect.TypeOf(obj)
	if !c.cached[typ] {
		return
	}
	k := freshKey{typ: typ, key: client.ObjectKeyFromObject(obj)}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.fresh[k] = freshMark{resourceVersion: obj.GetResourceVersion(), until: c.now().Add(relayFreshWindow)}
	// Bounded by the sessions written to in the last window; sweep expired
	// marks on the way in rather than growing with every session ever seen.
	for key, mark := range c.fresh {
		if !c.now().Before(mark.until) {
			delete(c.fresh, key)
		}
	}
}

func (c *relayClient) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	defer c.wrote(obj)
	return c.Client.Create(ctx, obj, opts...)
}

func (c *relayClient) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	defer c.wrote(obj)
	return c.Client.Update(ctx, obj, opts...)
}

func (c *relayClient) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
	defer c.wrote(obj)
	return c.Client.Patch(ctx, obj, patch, opts...)
}

func (c *relayClient) Status() client.SubResourceWriter {
	return relayStatusWriter{SubResourceWriter: c.Client.Status(), c: c}
}

type relayStatusWriter struct {
	client.SubResourceWriter
	c *relayClient
}

func (w relayStatusWriter) Update(ctx context.Context, obj client.Object, opts ...client.SubResourceUpdateOption) error {
	defer w.c.wrote(obj)
	return w.SubResourceWriter.Update(ctx, obj, opts...)
}

func (w relayStatusWriter) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
	defer w.c.wrote(obj)
	return w.SubResourceWriter.Patch(ctx, obj, patch, opts...)
}
