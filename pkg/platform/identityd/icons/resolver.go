package icons

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"time"

	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/passthroughcatalog"
	embeddedtoolkits "github.com/authzed/openagentprimitives/toolkits"

	"golang.org/x/sync/singleflight"
)

// Resolver routes /icon/<credName> requests through cache + discovery,
// falling back to FallbackSVG when no real favicon can be served.
//
// Resolution order for the per-credential site URL:
//  1. MCPServerList cluster-wide → match by passthroughcatalog.CredentialNameForServer
//  2. SpiceboxToolkit (cluster-scoped) named credName
//  3. embeddedtoolkits.All() named credName
//  4. None → FallbackSVG (negative-cached)
type Resolver struct {
	K8s        client.Client
	Cache      *Cache
	Discoverer *Discoverer
	sf         singleflight.Group
}

// Lookup returns the cached or freshly-resolved Entry for credName.
// Never returns an error to the handler — every credential produces
// serveable bytes (real icon OR deterministic SVG fallback).
func (r *Resolver) Lookup(ctx context.Context, credName string) Entry {
	if e, ok := r.Cache.Get(credName); ok {
		return e
	}
	// Collapse concurrent first-loads for the same credential.
	v, _, _ := r.sf.Do(credName, func() (any, error) {
		if e, ok := r.Cache.Get(credName); ok {
			return e, nil
		}
		siteURL := r.resolveSiteURL(ctx, credName)
		if siteURL == "" {
			e := fallbackEntry(credName)
			r.Cache.Put(credName, e)
			return e, nil
		}
		fetched, err := r.Discoverer.Discover(ctx, siteURL)
		if err != nil {
			slog.Info("icons: discovery failed; serving fallback SVG",
				"cred", credName, "siteURL", siteURL, "err", err.Error())
			e := fallbackEntry(credName)
			r.Cache.Put(credName, e)
			return e, nil
		}
		fetched.ETag = computeETag(fetched.Bytes)
		r.Cache.Put(credName, fetched)
		return fetched, nil
	})
	return v.(Entry)
}

func (r *Resolver) resolveSiteURL(ctx context.Context, credName string) string {
	// 1. MCPServer cluster-wide.
	var mcps spiceboxv1alpha1.MCPServerList
	if err := r.K8s.List(ctx, &mcps); err == nil {
		for i := range mcps.Items {
			m := &mcps.Items[i]
			if passthroughcatalog.CredentialNameForServer(m) != credName {
				continue
			}
			if m.Spec.SiteURL != "" {
				return m.Spec.SiteURL
			}
		}
	}
	// 2. SpiceboxToolkit (cluster-scoped).
	var tk spiceboxv1alpha1.SpiceboxToolkit
	switch err := r.K8s.Get(ctx, client.ObjectKey{Name: credName}, &tk); {
	case err == nil:
		if tk.Spec.SiteURL != "" {
			return tk.Spec.SiteURL
		}
	case !k8serrors.IsNotFound(err):
		slog.Info("icons: SpiceboxToolkit Get failed",
			"cred", credName, "err", err.Error())
	}
	// 3. Embedded toolkit catalog.
	for _, t := range embeddedtoolkits.All() {
		if t.Name == credName && t.SiteURL != "" {
			return t.SiteURL
		}
	}
	return ""
}

func fallbackEntry(credName string) Entry {
	bytes := FallbackSVG(credName)
	return Entry{
		Bytes:       bytes,
		ContentType: "image/svg+xml",
		ETag:        computeETag(bytes),
		FetchedAt:   time.Now(),
		Negative:    true,
	}
}

func computeETag(bytes []byte) string {
	sum := sha256.Sum256(bytes)
	return fmt.Sprintf(`"%s"`, hex.EncodeToString(sum[:8]))
}
