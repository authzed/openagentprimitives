package identityd

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/conditions"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/idp"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/idp/registry"
)

// ErrIdPNotConfigured distinguishes "no ClusterIdentityProvider exists"
// (fall through to channel-kind authenticators) from a real failure
// (misconfigured IdP — error out, never silently downgrade).
var ErrIdPNotConfigured = errors.New("identityd: no cluster identity provider configured")

// defaultIdPSessionTTL is the idd_session lifetime for IdP-verified
// logins when the CR doesn't set spec.sessionTTL.
const defaultIdPSessionTTL = 12 * time.Hour

// idpNegativeCacheTTL is how long a FAILED resolve is remembered — deliberately
// much shorter than the positive cacheTTL. Long enough to collapse the burst of
// logins queued behind one slow construction (kind.New for the oidc/google
// kinds is an issuer-discovery round trip on a 30s-timeout client, run with the
// loader's mutex held); short enough that a recovered IdP is usable again
// almost immediately. A config fix need not wait it out at all: the cache key
// carries the CR generation and the Secret's resourceVersion.
const idpNegativeCacheTTL = 5 * time.Second

// resolvedIdP is the loaded provider + the policy identityd enforces.
type resolvedIdP struct {
	provider       idp.Provider
	allowedDomains []string
	allowAny       bool
	sessionTTL     time.Duration
}

// idpLoader resolves the singleton CR + Secret into a Provider, cached for
// cacheTTL keyed on (CR generation, secret resourceVersion) — so an IdP edit
// applies within a tick without rebuilding the provider (and its OIDC discovery
// round-trip) on every login.
type idpLoader struct {
	k8s         client.Client
	externalURL func() string
	cacheTTL    time.Duration
	now         func() time.Time

	mu     sync.Mutex
	cached *resolvedIdP
	// cachedErr is the failure cached for cachedKey, if the last resolve for
	// that key failed. Exactly one of cached / cachedErr is ever set.
	cachedErr error
	cachedKey string
	cachedAt  time.Time
}

// cachedLocked reports whether cacheKey has a live cached outcome, and returns
// it. Failures are cached too — see idpNegativeCacheTTL for why, and for the
// much shorter window they get. Caller holds l.mu.
func (l *idpLoader) cachedLocked(cacheKey string) (hit bool, resolved *resolvedIdP, err error) {
	if l.cachedKey != cacheKey {
		return false, nil, nil
	}
	ttl := l.cacheTTL
	if l.cachedErr != nil {
		ttl = min(ttl, idpNegativeCacheTTL)
	}
	if !l.now().Before(l.cachedAt.Add(ttl)) {
		return false, nil, nil
	}
	return true, l.cached, l.cachedErr
}

// storeLocked records the outcome of a resolve for cacheKey. Caller holds l.mu.
func (l *idpLoader) storeLocked(cacheKey string, resolved *resolvedIdP, err error) {
	l.cached = resolved
	l.cachedErr = err
	l.cachedKey = cacheKey
	l.cachedAt = l.now()
}

// failLocked caches err for cacheKey and returns it. EVERY failure raised while
// the mutex is held must go through here: all four login entries share one
// loader, so otherwise each login queued behind a slow failure re-runs it in
// turn, serially. Caller holds l.mu.
func (l *idpLoader) failLocked(cacheKey string, err error) error {
	l.storeLocked(cacheKey, nil, err)
	return err
}

// Current resolves the singleton ClusterIdentityProvider CR + its
// client-secret Secret into a cached Provider + policy.
//
// Error semantics:
//   - errors.Is(err, ErrIdPNotConfigured) — no CR exists; caller may
//     fall through to channel-kind authenticators.
//   - any other non-nil error — misconfiguration (invalid CR, missing
//     Secret, unknown kind) — fail closed, never silently downgrade.
func (l *idpLoader) Current(ctx context.Context) (*resolvedIdP, error) {
	// 1. Fetch the singleton CR.
	var cr spiceboxv1alpha1.ClusterIdentityProvider
	if err := l.k8s.Get(ctx, client.ObjectKey{Name: spiceboxv1alpha1.ClusterIdentityProviderName}, &cr); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, ErrIdPNotConfigured
		}
		return nil, fmt.Errorf("identityd: get ClusterIdentityProvider: %w", err)
	}

	// 2. Validity must be True, checked BEFORE the cache so a CR flipped to
	//    Valid=False takes effect immediately rather than after the TTL.
	if !conditions.IsTrue(cr.Status.Conditions, spiceboxv1alpha1.ConditionIdPValid) {
		c := conditions.Find(cr.Status.Conditions, spiceboxv1alpha1.ConditionIdPValid)
		if c == nil {
			return nil, errors.New("identityd: cluster identity provider has not yet been validated")
		}
		return nil, fmt.Errorf("identityd: cluster identity provider is not valid: %s: %s", c.Reason, c.Message)
	}

	// 3. Fetch the client-secret Secret.
	ref := cr.Spec.ClientSecretRef
	var secret corev1.Secret
	if err := l.k8s.Get(ctx, client.ObjectKey{Namespace: ref.Namespace, Name: ref.Name}, &secret); err != nil {
		return nil, fmt.Errorf("identityd: get client secret %s/%s: %w", ref.Namespace, ref.Name, err)
	}

	// 4. Cache check keyed on (CR generation, secret resourceVersion).
	cacheKey := fmt.Sprintf("%d/%s", cr.Generation, secret.ResourceVersion)
	l.mu.Lock()
	defer l.mu.Unlock()
	if hit, resolved, cachedErr := l.cachedLocked(cacheKey); hit {
		return resolved, cachedErr
	}

	// 5. Resolve the kind from the registry.
	kind, ok := registry.Get(cr.Spec.Kind)
	if !ok {
		return nil, l.failLocked(cacheKey, fmt.Errorf("identityd: unknown IdP kind %q (registered: %s)",
			cr.Spec.Kind, strings.Join(registry.Names(), ", ")))
	}

	// 6. Extract the secret value and build the config.
	secretVal, ok := secret.Data[ref.Key]
	if !ok || len(secretVal) == 0 {
		return nil, l.failLocked(cacheKey,
			fmt.Errorf("identityd: client secret %s/%s key %q is missing or empty", ref.Namespace, ref.Name, ref.Key))
	}
	base := strings.TrimRight(l.externalURL(), "/")
	if base == "" {
		return nil, l.failLocked(cacheKey,
			errors.New("identityd: external base URL not configured; cannot derive the IdP redirect URL"))
	}
	loginHintDomain := ""
	if len(cr.Spec.AllowedEmailDomains) > 0 {
		loginHintDomain = cr.Spec.AllowedEmailDomains[0]
	}
	federation := cr.Spec.Federation != nil && cr.Spec.Federation.Enabled
	cfg := idp.Config{
		Issuer:          cr.Spec.Issuer,
		ClientID:        cr.Spec.ClientID,
		ClientSecret:    string(secretVal),
		Scopes:          cr.Spec.Scopes,
		RedirectURL:     base + "/oidc/callback/idp",
		LoginHintDomain: loginHintDomain,
		Federation:      federation,
	}
	provider, err := kind.New(ctx, cfg)
	if err != nil {
		return nil, l.failLocked(cacheKey,
			fmt.Errorf("identityd: construct IdP provider (kind=%s): %w", cr.Spec.Kind, err))
	}

	// 7. Build resolved IdP + populate sessionTTL.
	sessionTTL := defaultIdPSessionTTL
	if cr.Spec.SessionTTL != nil {
		sessionTTL = cr.Spec.SessionTTL.Duration
	}
	resolved := &resolvedIdP{
		provider:       provider,
		allowedDomains: cr.Spec.AllowedEmailDomains,
		allowAny:       cr.Spec.AllowAnyEmail,
		sessionTTL:     sessionTTL,
	}

	// Store in cache.
	l.storeLocked(cacheKey, resolved, nil)
	return resolved, nil
}
