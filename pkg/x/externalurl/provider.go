// Package externalurl provides a live-updating accessor for a service's
// externally reachable base URL.
//
// The URL lives in a ConfigMap that `oap install` creates and `oap init --local`
// mutates when an ngrok tunnel comes up; webd publishes its own URL in
// spicebox-webd-external-url for channelsd to consume. Kubernetes evaluates an
// env-var mount at pod start, so a running pod never sees those later edits
// without a rollout restart — hence Provider, which polls the ConfigMap and
// hands consumers the current value on every URL build via Get() (backed by
// atomic.Pointer, so callers need no mutex).
//
// Polling, and a typed clientset rather than controller-runtime's cached
// client, are both RBAC decisions: a watch — or even a single cached Get —
// transparently lists+watches configmaps, while a literal GET on one named
// ConfigMap needs only "get" on that name. A 10s tick is well below any
// human-visible latency for an operator running `oap init --local`.
package externalurl

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// DefaultPollInterval is the polling cadence when callers don't override it:
// well below the time it takes an operator who just patched the ConfigMap to
// switch back to their channel and press a button.
const DefaultPollInterval = 10 * time.Second

// Namespace is the ConfigMap's namespace, created by `oap install`.
const Namespace = "agentprimitives-system"

// EnvWebdBaseURL is the env var carrying webd's externally reachable base URL
// to a pod that cannot read the ConfigMap itself.
//
// A runner pod is exactly that case: it lives in the session's namespace,
// while the ConfigMap lives in Namespace, and a pod may only reference
// ConfigMaps in its own namespace. So the operator — which already tracks the
// value with a Provider — stamps it onto the pod, the same way it carries
// OPERATOR_MEMORY_URL across the same boundary.
//
// The name lives here, next to the ConfigMap this value comes from, because
// the stamping side (pkg/controllers/agentsession's podspec) and the reading
// side (internal/cmd/runner's flag binding) are in different binaries. A
// literal on either side could drift from the other, and the symptom would be
// a runner that silently never has a URL rather than any error at all.
const EnvWebdBaseURL = "WEBD_BASE_URL"

// Provider tracks a ConfigMap's URL value and exposes it via Get().
// Safe for concurrent readers.
type Provider struct {
	cs            kubernetes.Interface
	logger        logr.Logger
	interval      time.Duration
	configMapName string
	dataKey       string
	url           atomic.Pointer[string]
}

// NewProvider returns a Provider reading the identityd external-URL ConfigMap
// (spicebox-identityd-external-url / "url"). bootstrapURL seeds the accessor
// with whatever the binary read at startup from its env-var mount; when it is
// empty Get() returns "" until the first poll lands.
//
// cs comes from kubernetes.NewForConfig — deliberately not the
// controller-runtime client, for the RBAC reason in the package doc.
func NewProvider(cs kubernetes.Interface, logger logr.Logger, bootstrapURL string) *Provider {
	return NewProviderFor(cs, logger, spiceboxv1alpha1.IdentitydExternalURLConfigMap, "url", bootstrapURL)
}

// NewProviderFor reads the URL from the given ConfigMap name and data key, for
// services (webd, channelsd) that publish their base URL under their own name.
func NewProviderFor(cs kubernetes.Interface, logger logr.Logger, configMapName, dataKey, bootstrapURL string) *Provider {
	p := &Provider{
		cs:            cs,
		logger:        logger,
		interval:      DefaultPollInterval,
		configMapName: configMapName,
		dataKey:       dataKey,
	}
	if bootstrapURL != "" {
		s := bootstrapURL
		p.url.Store(&s)
	}
	return p
}

// WithInterval overrides DefaultPollInterval (mostly a test seam).
// Returns the provider for chaining.
func (p *Provider) WithInterval(d time.Duration) *Provider {
	if d > 0 {
		p.interval = d
	}
	return p
}

// Get returns the most recently observed URL. Empty when no value
// has been observed yet (no bootstrap, no successful poll).
func (p *Provider) Get() string {
	if v := p.url.Load(); v != nil {
		return *v
	}
	return ""
}

// Run polls the ConfigMap until ctx is canceled, atomically swapping the stored
// URL when the value changes. Get errors are logged but do NOT clear the stored
// URL — a transient API blip shouldn't strand the link minter on an empty
// value. Call once, in a dedicated goroutine.
func (p *Provider) Run(ctx context.Context) {
	// Read immediately so a freshly-started binary picks up a URL that landed
	// between its env-var mount and Run starting.
	p.refresh(ctx)
	t := time.NewTicker(p.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.refresh(ctx)
		}
	}
}

func (p *Provider) refresh(ctx context.Context) {
	cm, err := p.cs.CoreV1().ConfigMaps(Namespace).Get(ctx, p.configMapName, metav1.GetOptions{})
	if err != nil {
		// NotFound is expected before `oap install` has run; RBAC and API-down
		// errors are not, and get the louder log.
		if apierrors.IsNotFound(err) {
			p.logger.V(1).Info("externalurl: ConfigMap not found yet", "name", p.configMapName)
			return
		}
		p.logger.Info("externalurl: ConfigMap Get failed", "name", p.configMapName, "err", err.Error())
		return
	}
	url, ok := cm.Data[p.dataKey]
	if !ok || url == "" {
		// Key removed — keep the cached URL rather than clobbering it with an
		// empty one.
		return
	}
	prev := p.Get()
	if url == prev {
		return
	}
	u := url
	p.url.Store(&u)
	p.logger.Info("externalurl: updated from ConfigMap",
		"prev", prev, "now", url, "interval", p.interval.String(),
		"reason", fmt.Sprintf("ConfigMap %s/%s key %q changed", Namespace, p.configMapName, p.dataKey))
}
