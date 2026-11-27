package main

import (
	"context"
	"fmt"
	"reflect"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/resourceversion"
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

// lastAppliedAnnotation is kubectl's copy of the whole applied manifest. Nothing
// in channelsd reads it, and on a kubectl-applied object it roughly doubles
// what the cache retains.
const lastAppliedAnnotation = "kubectl.kubernetes.io/last-applied-configuration"

// relayCacheTransform trims every object before the cache stores it. channelsd
// runs under a small memory limit and the cache holds every AgentSession in the
// cluster, so what one session retains is what decides whether the pod fits.
//
// Dropped from every kind: managedFields and the kubectl last-applied
// annotation. Dropped from AgentSession only: the status fields in
// trimSessionStatus, which nothing channelsd links reads.
//
// Everything else is kept, which is what the relay and the senders depend on:
// labels and annotations (the opening-sent marker, the kind's thread anchors),
// the whole spec (class, input/output binding, parent, forkedFrom), and the
// status the delivery path reads — phase, result, failureReason, conditions,
// pendingInteractions, pendingRequesters, pendingRestart, effectiveSettings.
//
// A trimmed copy is for READING. Never Update one back: the API server would
// take the missing fields as deletions. The relay only ever sends patches.
func relayCacheTransform(in any) (any, error) {
	obj, err := meta.Accessor(in)
	if err != nil {
		// Not an API object (a deletion tombstone); there is nothing to trim.
		return in, nil
	}
	if obj.GetManagedFields() != nil {
		obj.SetManagedFields(nil)
	}
	if ann := obj.GetAnnotations(); ann[lastAppliedAnnotation] != "" {
		delete(ann, lastAppliedAnnotation)
		obj.SetAnnotations(ann)
	}
	if sess, ok := in.(*spiceboxv1alpha1.AgentSession); ok {
		trimSessionStatus(&sess.Status)
	}
	return in, nil
}

// trimSessionStatus clears the AgentSession status fields the cache does not
// keep: the runner's and operator's own bookkeeping, list-shaped and growing
// with the session, none of it referenced by any package channelsd links.
//
// To add a field here, first confirm nothing in `go list -deps
// ./internal/cmd/channelsd` reads it — a field cleared here reads as empty to
// the relay and every sender, silently. TestRelayCacheTransform pins the list.
func trimSessionStatus(st *spiceboxv1alpha1.AgentSessionStatus) {
	st.BundleSessions = nil
	st.ResolvedSidecarToolboxes = nil
	st.ResolvedWorkspaceSource = nil
	st.SidecarReachability = nil
	st.ResolvedContentGuardDetectors = nil
	st.ResolvedSkillBundles = nil
	st.ObservedPins = nil
	st.SlotPins = nil
	st.CredentialAuthFailures = nil
	st.Progress = nil
	st.CompletionBypasses = nil
	st.RunnerNotes = nil
	st.SatisfiedSecretOutputs = nil
	st.AuditChainHeads = nil
	st.EstimatedCost = nil
	st.ToolGuard = nil
	st.ActiveWidgets = nil
	st.PermissionSurface = nil
	st.PassthroughCredHashes = nil
	st.InteractorTuplesWritten = nil
}

// newRelayCache starts an informer cache over relayCachedTypes and waits for
// it to sync. ReaderFailOnMissingInformer keeps it from lazily starting an
// informer for a type nobody registered — a Get of an uncached type through
// the cache is an error, not a silent cluster-wide watch.
//
// The informers are cluster-wide because channelsd is: it serves Channels and
// sessions in every namespace and no label marks "the ones channelsd serves",
// so there is no selector to narrow them by. What bounds memory instead is
// relayCacheTransform.
func newRelayCache(ctx context.Context, restCfg *rest.Config, scheme *runtime.Scheme, syncTimeout time.Duration) (cache.Cache, error) {
	byObject := make(map[client.Object]cache.ByObject, len(relayCachedTypes))
	for _, obj := range relayCachedTypes {
		byObject[obj] = cache.ByObject{}
	}
	c, err := cache.New(restCfg, cache.Options{
		Scheme:                      scheme,
		ByObject:                    byObject,
		ReaderFailOnMissingInformer: true,
		DefaultTransform:            relayCacheTransform,
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

const (
	// writeMarkMaxAge bounds how long a write is remembered when the cache
	// never shows it. It is not the read-your-writes window — a mark normally
	// clears the moment the cache reaches the write's resourceVersion, however
	// long that takes. It only stops the bookkeeping growing without limit
	// behind a watch that has stopped delivering, and a mark dropped this way
	// is logged, because from then on the cache is trusted without proof.
	writeMarkMaxAge = 5 * time.Minute
	// writeMarkSweepInterval is how often marks nobody read back are checked
	// against the cache and cleared.
	writeMarkSweepInterval = time.Minute
)

// writeTracker remembers channelsd's own writes to relayCachedTypes, so a
// cached read can tell whether the cache has caught up with them.
//
// It is shared by every client channelsd holds: trackingClient records into it
// on each write, relayClient consults it on each cached read. The split is the
// point. The writes a cached reader must see are not all made by the reader —
// the inbound pipeline and the listeners stamp a session's thread anchor, the
// relay and the senders read it back — so the record has to belong to the
// process, not to the one client that reads the cache.
//
// Only this process's writes are tracked. A write by the operator or a runner
// reaches the cache at watch speed, as it does for any informer consumer.
type writeTracker struct {
	tracked map[reflect.Type]bool
	now     func() time.Time

	mu        sync.Mutex
	marks     map[markKey]writeMark
	seq       uint64
	lastSweep time.Time
}

type markKey struct {
	typ reflect.Type
	key client.ObjectKey
}

func (k markKey) String() string {
	return k.typ.Elem().Name() + " " + k.key.String()
}

type writeMark struct {
	// resourceVersion is what the write produced: the version the cache has to
	// reach before it is trusted for this object again. "" when the write did
	// not report one (it failed part-way, or it was a delete); the next direct
	// read fills it in.
	resourceVersion string
	// seq identifies this mark, so a direct read that began before a later
	// write cannot fill in or clear that later write's mark.
	seq uint64
	at  time.Time
}

func newWriteTracker() *writeTracker {
	t := &writeTracker{
		tracked: make(map[reflect.Type]bool, len(relayCachedTypes)),
		now:     time.Now,
		marks:   map[markKey]writeMark{},
	}
	for _, obj := range relayCachedTypes {
		t.tracked[reflect.TypeOf(obj)] = true
	}
	t.lastSweep = t.now()
	return t
}

// wrote records a write to obj. rvKnown says obj now carries the version the
// write produced — true only for a write that succeeded and decoded its
// response into obj. A failed write is recorded too, without a version: a
// timeout may have landed, and a conflict is direct evidence that some copy
// this process holds is stale.
func (t *writeTracker) wrote(obj client.Object, rvKnown bool) {
	typ := reflect.TypeOf(obj)
	if !t.tracked[typ] {
		return
	}
	rv := ""
	if rvKnown {
		rv = obj.GetResourceVersion()
	}
	k := markKey{typ: typ, key: client.ObjectKeyFromObject(obj)}
	t.mu.Lock()
	defer t.mu.Unlock()
	if prev, ok := t.marks[k]; ok && rv != "" {
		// Two writes to one object can be recorded out of order. Keep the
		// later version, and keep "unknown" over any known one: the direct
		// read that resolves it reflects both writes.
		if prev.resourceVersion == "" || rvAtLeast(prev.resourceVersion, rv) {
			rv = prev.resourceVersion
		}
	}
	t.seq++
	t.marks[k] = writeMark{resourceVersion: rv, seq: t.seq, at: t.now()}
}

// rvAtLeast reports whether resourceVersion a is at or past b. Versions that
// do not compare (a storage backend without ordered versions) count only when
// identical, which is the safe direction: the caller keeps reading directly.
func rvAtLeast(a, b string) bool {
	cmp, err := resourceversion.CompareResourceVersion(a, b)
	if err != nil {
		return a == b
	}
	return cmp >= 0
}

// cacheVerdict is what the tracker makes of one cached copy.
type cacheVerdict struct {
	// trust: the cache is at or past every write this process made.
	trust bool
	// mark is the write the cache has not shown; meaningful when !trust.
	mark writeMark
	// gaveUp: the mark outlived writeMarkMaxAge and was dropped unconfirmed.
	// This read still goes direct; later ones trust the cache.
	gaveUp bool
}

// check judges a cached copy at cachedRV against the object's mark, clearing
// the mark once the cache has reached it.
func (t *writeTracker) check(k markKey, cachedRV string) cacheVerdict {
	t.mu.Lock()
	defer t.mu.Unlock()
	m, ok := t.marks[k]
	if !ok {
		return cacheVerdict{trust: true}
	}
	if m.resourceVersion != "" && rvAtLeast(cachedRV, m.resourceVersion) {
		delete(t.marks, k)
		return cacheVerdict{trust: true}
	}
	if t.now().Sub(m.at) >= writeMarkMaxAge {
		delete(t.marks, k)
		return cacheVerdict{mark: m, gaveUp: true}
	}
	return cacheVerdict{mark: m}
}

// learned fills in a mark's version from a direct read made on its behalf:
// whatever the API server returned is at or past the write, so it is a version
// the cache can be held to. A no-op when a newer write replaced the mark.
func (t *writeTracker) learned(k markKey, seq uint64, rv string) {
	if rv == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if m, ok := t.marks[k]; ok && m.seq == seq && m.resourceVersion == "" {
		m.resourceVersion = rv
		t.marks[k] = m
	}
}

// overdue returns the marks past writeMarkMaxAge, at most once per
// writeMarkSweepInterval. These are writes to objects nobody read back; the
// caller settles each against the cache and calls drop.
func (t *writeTracker) overdue() map[markKey]writeMark {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	if now.Sub(t.lastSweep) < writeMarkSweepInterval {
		return nil
	}
	t.lastSweep = now
	var out map[markKey]writeMark
	for k, m := range t.marks {
		if now.Sub(m.at) >= writeMarkMaxAge {
			if out == nil {
				out = map[markKey]writeMark{}
			}
			out[k] = m
		}
	}
	return out
}

// drop removes a mark unless a newer write has replaced it.
func (t *writeTracker) drop(k markKey, seq uint64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if m, ok := t.marks[k]; ok && m.seq == seq {
		delete(t.marks, k)
	}
}

// trackingClient is the client every channelsd component writes through. Reads
// go straight to the API server, unchanged; each write to one of
// relayCachedTypes is recorded in the shared writeTracker so relayClient's
// cached reads can see it.
//
// It wraps the process's one direct client rather than being handed to chosen
// writers: a writer that was missed would be invisible — its write simply read
// back stale, some of the time — so no writer is given the chance to be missed.
//
// Apply and DeleteAllOf pass through unrecorded: neither names a single typed
// object to key a mark by, and nothing in channelsd calls them on these kinds.
type trackingClient struct {
	client.Client
	tracker *writeTracker
}

func newTrackingClient(direct client.Client, tracker *writeTracker) *trackingClient {
	return &trackingClient{Client: direct, tracker: tracker}
}

func (c *trackingClient) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	err := c.Client.Create(ctx, obj, opts...)
	c.tracker.wrote(obj, err == nil)
	return err
}

func (c *trackingClient) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	err := c.Client.Update(ctx, obj, opts...)
	c.tracker.wrote(obj, err == nil)
	return err
}

func (c *trackingClient) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
	err := c.Client.Patch(ctx, obj, patch, opts...)
	c.tracker.wrote(obj, err == nil)
	return err
}

// Delete records the write without a version: obj still carries the version it
// was read at, and the cache is caught up only when it stops returning the
// object at all — which Get already answers with a direct read.
func (c *trackingClient) Delete(ctx context.Context, obj client.Object, opts ...client.DeleteOption) error {
	err := c.Client.Delete(ctx, obj, opts...)
	c.tracker.wrote(obj, false)
	return err
}

func (c *trackingClient) Status() client.SubResourceWriter {
	return c.SubResource("status")
}

func (c *trackingClient) SubResource(subResource string) client.SubResourceClient {
	return trackingSubResourceClient{SubResourceClient: c.Client.SubResource(subResource), tracker: c.tracker}
}

type trackingSubResourceClient struct {
	client.SubResourceClient
	tracker *writeTracker
}

// Create's response is decoded into subResource, not obj, so the parent's new
// version is not known here.
func (w trackingSubResourceClient) Create(ctx context.Context, obj, subResource client.Object, opts ...client.SubResourceCreateOption) error {
	err := w.SubResourceClient.Create(ctx, obj, subResource, opts...)
	w.tracker.wrote(obj, false)
	return err
}

func (w trackingSubResourceClient) Update(ctx context.Context, obj client.Object, opts ...client.SubResourceUpdateOption) error {
	err := w.SubResourceClient.Update(ctx, obj, opts...)
	w.tracker.wrote(obj, err == nil)
	return err
}

func (w trackingSubResourceClient) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
	err := w.SubResourceClient.Patch(ctx, obj, patch, opts...)
	w.tracker.wrote(obj, err == nil)
	return err
}

// relayClient serves reads of relayCachedTypes from an informer cache and
// everything else — every write, every other read — from the trackingClient it
// embeds.
//
// The relay handles every session's outbound traffic on ONE goroutine, and
// used to spend at least one API-server round trip per envelope. Under a burst
// of busy sessions that round trip, rate-limited client side, was the whole
// throughput ceiling.
//
// Read-your-writes: the relay's very next envelope for a session must see what
// channelsd just wrote to it — the opening-sent marker, or it posts the opening
// twice; the thread anchor the inbound pipeline stamped, or the reply lands
// under the previous message. So a cached copy is served only once it is at or
// past the resourceVersion of channelsd's last write to that object (see
// writeTracker); until then the read goes to the API server. No timer decides
// this: a watch that lags for a minute costs a minute of direct reads, not a
// stale one. A cache miss also goes direct: a session created a moment ago may
// not have reached the cache yet, and dropping its first envelopes as "not
// found" would be worse than the round trip.
//
// Who holds it: the outbound relay and the sender resolver, which passes it to
// every Sender it builds and uses it for its own lookups. All of them only read
// through it, plus the relay's own patches. So a Sender sees the same cached
// AgentSession, AgentClass and Channel the relay does: current with respect to
// channelsd's own writes, and behind the operator's and the runner's by watch
// lag, like any informer consumer.
//
// Everything else in channelsd — the inbound pipeline, the listeners, the
// watchdog, the session watcher — holds the trackingClient and reads directly.
// Those paths read-modify-write (status updates with optimistic concurrency)
// and would gain nothing from a cache but stale-read conflicts. Do not hand
// them this client. For the same reason, code holding this client must not
// Update an object it read through it: cached copies are trimmed (see
// relayCacheTransform).
type relayClient struct {
	client.Client
	cache   client.Reader
	tracker *writeTracker
}

func newRelayClient(direct *trackingClient, cached client.Reader) *relayClient {
	return &relayClient{Client: direct, cache: cached, tracker: direct.tracker}
}

func (c *relayClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	typ := reflect.TypeOf(obj)
	if !c.tracker.tracked[typ] {
		return c.Client.Get(ctx, key, obj, opts...)
	}
	c.sweep(ctx)

	// Read the cache into a scratch object, not obj. If the read then has to
	// go direct, the API response is decoded into obj as the caller passed it;
	// decoding it over a cached copy would leave behind every field the cached
	// copy had and the live object no longer does.
	scratch := reflect.New(typ.Elem()).Interface().(client.Object)
	if err := c.cache.Get(ctx, key, scratch, opts...); err != nil {
		if apierrors.IsNotFound(err) {
			return c.Client.Get(ctx, key, obj, opts...)
		}
		return err
	}
	k := markKey{typ: typ, key: key}
	v := c.tracker.check(k, scratch.GetResourceVersion())
	if v.trust {
		reflect.ValueOf(obj).Elem().Set(reflect.ValueOf(scratch).Elem())
		return nil
	}
	if v.gaveUp {
		logUnconfirmedMark(ctx, k, v.mark, scratch.GetResourceVersion(), c.tracker.now().Sub(v.mark.at))
	}
	if err := c.Client.Get(ctx, key, obj, opts...); err != nil {
		return err
	}
	if !v.gaveUp {
		c.tracker.learned(k, v.mark.seq, obj.GetResourceVersion())
	}
	return nil
}

// sweep clears marks for objects nobody read back. Each overdue mark is settled
// against the cache: an object the cache no longer has, or has at or past the
// write, is dropped quietly; anything else is dropped and logged.
func (c *relayClient) sweep(ctx context.Context) {
	for k, m := range c.tracker.overdue() {
		scratch := reflect.New(k.typ.Elem()).Interface().(client.Object)
		err := c.cache.Get(ctx, k.key, scratch)
		switch {
		case apierrors.IsNotFound(err):
			// Gone from the cache too; a read of it goes direct regardless.
		case err != nil:
			ctrllog.FromContext(ctx).Info("relay cache: could not check an overdue write against the cache; keeping it",
				"object", k.String(), "err", err.Error())
			continue
		case m.resourceVersion != "" && rvAtLeast(scratch.GetResourceVersion(), m.resourceVersion):
		default:
			logUnconfirmedMark(ctx, k, m, scratch.GetResourceVersion(), c.tracker.now().Sub(m.at))
		}
		c.tracker.drop(k, m.seq)
	}
}

// logUnconfirmedMark reports a write channelsd stopped tracking before the
// cache ever showed it. From here on the cache is served for this object on
// trust, so a duplicated opening or a mis-threaded reply after this line is
// explained by it.
func logUnconfirmedMark(ctx context.Context, k markKey, m writeMark, cachedRV string, age time.Duration) {
	writeRV := m.resourceVersion
	if writeRV == "" {
		writeRV = "unknown"
	}
	ctrllog.FromContext(ctx).Info("relay cache: gave up waiting for the cache to show channelsd's own write; cached reads of this object may be stale (is the watch delivering?)",
		"object", k.String(), "writeResourceVersion", writeRV, "cachedResourceVersion", cachedRV,
		"age", age.Round(time.Second).String())
}
