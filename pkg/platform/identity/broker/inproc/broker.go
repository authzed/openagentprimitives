// Package inproc is the in-process token broker implementation. It reads the
// Secret behind each credential descriptor, gating and JIT-refreshing OAuth
// credentials, and projects the resolved token into env vars or HTTP headers.
package inproc

import (
	"context"
	"fmt"
	"sync"
	"time"

	"k8s.io/utils/clock"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/authkind"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/broker"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/credkind"
	// The broker cannot function with an empty credkind registry — every
	// Resolve goes through it. Unlike channel transports, where an operator
	// genuinely chooses which kinds to install, the credential-type set is
	// closed: it is enumerated by the CRD's kubebuilder Enum marker, so
	// there is no out-of-tree kind a binary could opt into or out of. The
	// registration therefore belongs here, at the one package every
	// consumer already imports, rather than repeated in each binary's
	// main() — that makes every consumer correct by construction instead
	// of by remembering.
	_ "github.com/authzed/openagentprimitives/pkg/platform/identity/credkind/imports"
	credkindregistry "github.com/authzed/openagentprimitives/pkg/platform/identity/credkind/registry"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/credresolve"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/federation"
)

// cacheKey identifies a resolved credential. Resource is empty for static/oauth;
// for federated it disambiguates the per-resource minted token sharing one
// IdP-identity Secret. Key is the Secret data key: one Secret can hold several
// credentials under different keys, and they must NOT collide — without Key the
// first-resolved credential's token is served for every key of that Secret.
type cacheKey struct {
	Namespace string
	Name      string // Secret name (IdP-identity Secret for federated)
	Resource  string
	Key       string // Secret data key (distinguishes co-located credentials)
}

type cacheEntry struct {
	cred      authkind.ResolvedCredential
	expiresAt time.Time
}

// usableAt reports whether the entry may still be served at now.
//
// A zero expiresAt is UNUSABLE, not eternal: an entry the broker could not bound
// is treated as absent, so a resolve path that fails to stamp an expiry degrades
// to "not cached" rather than serving one credential for the life of the process
// — which, the only other eviction being an explicit InvalidateSecret, is how a
// revoked credential keeps working. Both the read that serves an entry and the
// write that stores one go through this one predicate.
func (e cacheEntry) usableAt(now time.Time) bool {
	return !e.expiresAt.IsZero() && e.expiresAt.After(now)
}

// inflightResolve tracks one resolve that has released cacheMu to read its
// underlying source. The cache map cannot carry that signal: during the unlocked
// window there is no entry for InvalidateSecret to delete, so an invalidation
// arriving then would be silently undone by the write-back that follows. Marking
// the in-flight resolve turns that write-back into a discard-and-retry.
type inflightResolve struct {
	namespace string
	name      string
	// stale is set by InvalidateSecret and read by the write-back. Both happen
	// under cacheMu; the pointer is only ever reachable via Broker.inflight,
	// which is itself guarded by cacheMu.
	stale bool
}

const (
	// nonMintedCacheTTL bounds how long a non-minted (static or oauth) resolution
	// may be served from cache before its Secret is read again.
	//
	// A credential can stop being valid with no invalidation ever arriving: the
	// operator's proactive pre-expiry refresh rewrites the SAME Secret name in
	// place, so the revocation fingerprint (<type>:<secretRefName>) does not change
	// and no revoke is published. A bounded entry also keeps the broker's own
	// JIT-refresh-on-ErrExpired path reachable, since a cache hit never calls
	// resolveOneSource.
	//
	// The value trades a Secret re-read against how long a withdrawn credential
	// keeps working: short enough that one dies in seconds, long enough that a
	// burst of tool calls does not re-read per call. Minted credentials ignore
	// it — they carry the minter's own, shorter expiry.
	nonMintedCacheTTL = 30 * time.Second

	// maxResolveAttempts bounds the re-resolve loop that runs when an invalidation
	// lands mid-flight. Three rides out a revoke racing a resolve; exhausting it
	// means invalidations are arriving faster than the source can be read, and the
	// broker fails closed rather than return a value a revoke has superseded.
	maxResolveAttempts = 3
)

// Broker is the in-process broker.Broker implementation.
type Broker struct {
	// Client reads Secrets and, for JIT refresh, patches them. In the operator and
	// runner this is the controller-runtime client.
	Client client.Client

	// LiveReader is an UNFILTERED reader (the manager's uncached APIReader) used
	// for ONE thing: telling apart the two conditions Client collapses into a
	// single NotFound.
	//
	// In the operator, Client is the manager's cached client, whose Secret
	// informer is label-filtered to objects the operator has ADOPTED. A Secret an
	// operator deleted and recreated to rotate a token comes back without that
	// label, so it is invisible here and the resolve reports "secret missing"
	// about a Secret that is right there — see credresolve.ExplainSecretMissing,
	// which this feeds.
	//
	// It is never a value read: the probe asks for object METADATA only, so a
	// Secret the operator has not adopted never has its bytes pulled into
	// memory. Nil (the runner, whose client is already live) simply leaves the
	// unrefined message in place.
	LiveReader client.Reader

	// Minter mints federated (ID-JAG) credentials. Nil when the cluster IdP has no
	// federation configured, and a federated credential then fails closed.
	Minter federation.Minter

	// GitHubApp mints GitHub App installation access tokens. Nil when no
	// GitHub App minter is configured, and a githubApp credential then fails
	// closed — the exact mirror of Minter above.
	GitHubApp credkind.GitHubAppMinter

	// clk is the clock every cache-expiry decision reads. New / NewWithMinter set
	// the real one; a zero-value Broker literal leaves it nil and now() falls back
	// to the real clock. Overridden only by tests, which step past a cache TTL
	// without sleeping.
	clk clock.Clock

	cacheMu sync.Mutex
	// cache maps a credential's (namespace, name, resource, key) to its last
	// successfully resolved cacheEntry. Populated on first Resolve, dropped by
	// InvalidateSecret, never served past its expiresAt (see cacheEntry.usableAt).
	cache map[cacheKey]cacheEntry
	// inflight holds the resolves currently between a cache miss and their
	// write-back, so InvalidateSecret can reach the ones the cache map cannot.
	// Bounded by the number of concurrent resolves — every entry is removed by the
	// resolve that registered it — never by the number of Secrets seen.
	inflight map[*inflightResolve]struct{}
}

// New returns an in-process broker backed by c with no federation minter, so
// federated credentials fail closed.
func New(c client.Client) *Broker { return &Broker{Client: c, clk: clock.RealClock{}} }

// NewWithMinter returns a broker that can mint federated credentials.
func NewWithMinter(c client.Client, m federation.Minter) *Broker {
	return &Broker{Client: c, Minter: m, clk: clock.RealClock{}}
}

// now reads the broker's clock, falling back to the real one so a Broker
// built as a bare struct literal still expires its cache entries.
func (b *Broker) now() time.Time {
	if b.clk == nil {
		return time.Now()
	}
	return b.clk.Now()
}

var _ broker.Broker = (*Broker)(nil)

// Resolve implements broker.Broker.
//
// Each credential descriptor is resolved independently. Successful resolutions
// are cached by (namespace, Secret name, resource, key) for a bounded lifetime —
// the minting kind's own expiry for a minted credential, nonMintedCacheTTL
// otherwise — and a call arriving within it returns the cached ResolvedCredential
// without re-reading the Secret. InvalidateSecret drops a Secret's entries ahead
// of that, so the next Resolve re-hits the underlying source.
func (b *Broker) Resolve(ctx context.Context, req broker.Request) (broker.Resolution, error) {
	out := broker.Resolution{EnvVars: map[string]string{}, HTTPHeaders: map[string]string{}}
	for i := range req.Credentials {
		d := req.Credentials[i]
		resolved, err := b.resolveOneCached(ctx, d.Source)
		if err != nil {
			return broker.Resolution{}, fmt.Errorf("broker: resolve %s/%s: %w", d.Source.Namespace, d.Source.Name, err)
		}
		switch {
		case d.Inject.EnvVar != "":
			out.EnvVars[d.Inject.EnvVar] = string(resolved.AccessToken.UnderlyingValue())
		case d.Inject.Header != nil:
			out.HTTPHeaders[d.Inject.Header.Name] = d.Inject.Header.ValuePrefix + string(resolved.AccessToken.UnderlyingValue())
		default:
			return broker.Resolution{}, fmt.Errorf("broker: descriptor for %s/%s declares no injection", d.Source.Namespace, d.Source.Name)
		}
	}
	return out, nil
}

// resolveOneCached returns the cached ResolvedCredential for the given source
// if one is present and still usable, or calls resolveOneSource and caches
// the result on success.
//
// The underlying resolve runs without cacheMu held — it does I/O (a Secret read,
// a 30s-bounded JIT refresh, an ID-JAG mint) — so an InvalidateSecret can land
// while it is in flight. Such a resolve holds a value that predates the revoke,
// so its result is discarded and the source read again rather than written back
// over the invalidation.
func (b *Broker) resolveOneCached(ctx context.Context, src spiceboxv1alpha1.CredentialSource) (authkind.ResolvedCredential, error) {
	key := cacheKey{Namespace: src.Namespace, Name: src.Name, Resource: src.Resource, Key: src.Key}

	for attempt := 0; attempt < maxResolveAttempts; attempt++ {
		b.cacheMu.Lock()
		if e, ok := b.cache[key]; ok && e.usableAt(b.now()) {
			b.cacheMu.Unlock()
			return e.cred, nil
		}
		fl := &inflightResolve{namespace: src.Namespace, name: src.Name}
		if b.inflight == nil {
			b.inflight = make(map[*inflightResolve]struct{})
		}
		b.inflight[fl] = struct{}{}
		b.cacheMu.Unlock()

		resolved, exp, err := b.resolveOneSource(ctx, src)

		b.cacheMu.Lock()
		delete(b.inflight, fl)
		stale := fl.stale
		if err == nil && !stale {
			// usableAt gates the store as well as the read, so an entry the broker
			// could not bound is never written rather than written and then ignored.
			if e := (cacheEntry{cred: resolved, expiresAt: exp}); e.usableAt(b.now()) {
				if b.cache == nil {
					b.cache = make(map[cacheKey]cacheEntry)
				}
				b.cache[key] = e
			}
		}
		b.cacheMu.Unlock()

		if err != nil {
			return authkind.ResolvedCredential{}, err
		}
		if !stale {
			return resolved, nil
		}
	}
	return authkind.ResolvedCredential{}, fmt.Errorf(
		"broker: %s/%s was invalidated during each of %d resolve attempts; refusing to return a credential a revocation has superseded",
		src.Namespace, src.Name, maxResolveAttempts)
}

// resolveOneSource resolves one credential source through its registered
// credkind, then bounds the cache entry.
//
// The zero-expiry guard is enforced HERE, at the single choke point every
// minted kind passes through, rather than trusting each implementation of the
// interface. A minted token's short life is the only thing bounding a revoked
// token's usefulness, so a kind that stamps no expiry is a defect worth
// surfacing loudly rather than a resolve to serve uncached.
func (b *Broker) resolveOneSource(ctx context.Context, src spiceboxv1alpha1.CredentialSource) (authkind.ResolvedCredential, time.Time, error) {
	k, err := credkindregistry.Get(src.Type)
	if err != nil {
		return authkind.ResolvedCredential{}, time.Time{}, err
	}
	cred, exp, err := k.Resolve(ctx, b.credDeps(), src)
	if err != nil {
		// A "secret missing" from a read through an adoption-filtered client can
		// equally mean "present but unadopted"; ExplainSecretMissing settles it
		// against LiveReader. src.Name is the backing Secret's name for every
		// kind that can produce this error, and src.Namespace its namespace for
		// all but the federated kind, whose IdP-identity Secret lives in the
		// shared identities namespace: the probe then finds nothing and leaves
		// the original verdict standing, so a mislocated lookup under-explains
		// rather than mis-explains.
		return authkind.ResolvedCredential{}, time.Time{},
			credresolve.ExplainSecretMissing(ctx, b.LiveReader, src.Namespace, src.Name, err)
	}
	if k.Minted() && exp.IsZero() {
		return authkind.ResolvedCredential{}, time.Time{}, fmt.Errorf(
			"broker: %s credential %s/%s: kind returned no expiry (a minted token must never be cached indefinitely)",
			src.Type, src.Namespace, src.Name)
	}
	if exp.IsZero() {
		// A stored credential carries no expiry of its own, so the broker
		// bounds the entry. Every re-resolve then re-applies the kind's own
		// gate, and a credential withdrawn without a revoke stops being served
		// within the TTL.
		exp = b.now().Add(nonMintedCacheTTL)
	}
	return cred, exp, nil
}

// credDeps builds the credkind.Deps for this broker. Federation and GitHubApp
// are declared as interfaces, not assigned from a possibly-nil pointer, so a
// broker built without one hands its kind a genuine nil it can detect.
func (b *Broker) credDeps() credkind.Deps {
	return credkind.Deps{Client: b.Client, Federation: b.Minter, GitHubApp: b.GitHubApp}
}

// InvalidateSecret implements broker.Broker.
//
// It drops all cached resolutions sharing the backing Secret (namespace, name) —
// including every per-resource federated entry — so the next Resolve re-reads the
// underlying source. A call matching no entries is a safe no-op.
//
// Resolves already in flight are marked too. Deleting alone would leave the
// invalidation racing their write-back: a resolve that missed the cache has
// nothing to delete yet, and would then store the value it read BEFORE this call,
// resurrecting exactly the credential being withdrawn.
func (b *Broker) InvalidateSecret(namespace, name string) error {
	b.cacheMu.Lock()
	for k := range b.cache {
		if k.Namespace == namespace && k.Name == name {
			delete(b.cache, k) // drops every resource entry sharing this Secret
		}
	}
	for fl := range b.inflight {
		if fl.namespace == namespace && fl.name == name {
			fl.stale = true
		}
	}
	b.cacheMu.Unlock()
	return nil
}
