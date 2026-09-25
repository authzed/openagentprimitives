package icons

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

func newScheme(t *testing.T) *runtime.Scheme {
	s := runtime.NewScheme()
	require.NoError(t, spiceboxv1alpha1.AddToScheme(s))
	return s
}

func TestResolver_MCPServerSiteURL_Hit(t *testing.T) {
	iconBytes := []byte("\x89PNG\r\n\x1a\nfake")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			w.Header().Set("Content-Type", "text/html")
			_, _ = fmt.Fprint(w, `<html><head><link rel="icon" type="image/png" href="/icon.png"></head></html>`)
		case "/icon.png":
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write(iconBytes)
		default:
			w.WriteHeader(404)
		}
	}))
	defer upstream.Close()

	srv := &spiceboxv1alpha1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Name: "linear", Namespace: "default"},
		Spec: spiceboxv1alpha1.MCPServerSpec{
			SiteURL: upstream.URL,
			Auth:    spiceboxv1alpha1.MCPServerAuth{Credential: "linear"},
		},
	}
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(srv).Build()

	r := &Resolver{
		K8s:        c,
		Cache:      NewCache(CacheConfig{Cap: 10, PosTTL: time.Hour, NegTTL: time.Minute}),
		Discoverer: &Discoverer{HTTPClient: &http.Client{Timeout: 2 * time.Second}},
	}
	e := r.Lookup(context.Background(), "linear")
	assert.Equal(t, "image/png", e.ContentType)
	assert.Equal(t, iconBytes, e.Bytes)
	assert.False(t, e.Negative)
}

func TestResolver_NoSiteURL_ReturnsFallback(t *testing.T) {
	srv := &spiceboxv1alpha1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Name: "linear", Namespace: "default"},
		Spec: spiceboxv1alpha1.MCPServerSpec{
			Auth: spiceboxv1alpha1.MCPServerAuth{Credential: "linear"},
			// SiteURL empty
		},
	}
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(srv).Build()

	r := &Resolver{
		K8s:        c,
		Cache:      NewCache(CacheConfig{Cap: 10, PosTTL: time.Hour, NegTTL: time.Minute}),
		Discoverer: &Discoverer{HTTPClient: &http.Client{Timeout: 2 * time.Second}},
	}
	e := r.Lookup(context.Background(), "linear")
	assert.True(t, e.Negative, "no SiteURL → negative entry")
	assert.Equal(t, "image/svg+xml", e.ContentType)
	assert.Contains(t, string(e.Bytes), "<svg ")
}

func TestResolver_DiscoveryFails_ReturnsFallback(t *testing.T) {
	// Upstream that 404s everything.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(404)
	}))
	defer upstream.Close()

	srv := &spiceboxv1alpha1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Name: "linear", Namespace: "default"},
		Spec: spiceboxv1alpha1.MCPServerSpec{
			SiteURL: upstream.URL,
			Auth:    spiceboxv1alpha1.MCPServerAuth{Credential: "linear"},
		},
	}
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(srv).Build()

	r := &Resolver{
		K8s:        c,
		Cache:      NewCache(CacheConfig{Cap: 10, PosTTL: time.Hour, NegTTL: time.Minute}),
		Discoverer: &Discoverer{HTTPClient: &http.Client{Timeout: 2 * time.Second}},
	}
	e := r.Lookup(context.Background(), "linear")
	assert.True(t, e.Negative, "discovery failed → negative entry")
	assert.Equal(t, "image/svg+xml", e.ContentType)
}

func TestResolver_CacheHitSkipsClusterAndUpstream(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).Build()
	cache := NewCache(CacheConfig{Cap: 10, PosTTL: time.Hour, NegTTL: time.Minute})
	cache.Put("linear", Entry{Bytes: []byte("cached"), ContentType: "image/png", ETag: `"abc"`})
	r := &Resolver{K8s: c, Cache: cache, Discoverer: &Discoverer{HTTPClient: &http.Client{Timeout: 2 * time.Second}}}

	e := r.Lookup(context.Background(), "linear")
	assert.Equal(t, []byte("cached"), e.Bytes)
}

func TestResolver_NoBackingObject_ReturnsFallback(t *testing.T) {
	// No MCPServer / SpiceboxToolkit / embedded entry for "unknown".
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).Build()
	r := &Resolver{
		K8s:        c,
		Cache:      NewCache(CacheConfig{Cap: 10, PosTTL: time.Hour, NegTTL: time.Minute}),
		Discoverer: &Discoverer{HTTPClient: &http.Client{Timeout: 2 * time.Second}},
	}
	e := r.Lookup(context.Background(), "unknown-cred")
	assert.True(t, e.Negative)
	assert.Equal(t, "image/svg+xml", e.ContentType)
}

// Ensure the fake.NewClientBuilder().Build() result satisfies client.Client.
// Compile-time only; the blank import of fake ensures the package is loaded.
var _ = fake.NewClientBuilder
