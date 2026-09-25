package icons

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

func newResolverForTest(t *testing.T) *Resolver {
	srv := &spiceboxv1alpha1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Name: "linear", Namespace: "default"},
		Spec:       spiceboxv1alpha1.MCPServerSpec{}, // empty SiteURL → fallback path
	}
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(srv).Build()
	return &Resolver{
		K8s:        c,
		Cache:      NewCache(CacheConfig{Cap: 10, PosTTL: time.Hour, NegTTL: time.Minute}),
		Discoverer: &Discoverer{HTTPClient: &http.Client{Timeout: time.Second}},
	}
}

func TestHandler_ServesFallbackForUnknown(t *testing.T) {
	h := NewHandler(newResolverForTest(t))
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/icon/linear")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "image/svg+xml", resp.Header.Get("Content-Type"))
	body, _ := io.ReadAll(resp.Body)
	assert.Contains(t, string(body), "<svg ")
}

func TestHandler_NegativeCacheCacheControl(t *testing.T) {
	h := NewHandler(newResolverForTest(t))
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/icon/linear")
	require.NoError(t, err)
	defer resp.Body.Close()
	cc := resp.Header.Get("Cache-Control")
	assert.Contains(t, cc, "max-age=3600", "negative response uses negTTL (1h default)")
}

func TestHandler_IfNoneMatchReturns304(t *testing.T) {
	h := NewHandler(newResolverForTest(t))
	srv := httptest.NewServer(h)
	defer srv.Close()

	first, err := http.Get(srv.URL + "/icon/linear")
	require.NoError(t, err)
	_, _ = io.Copy(io.Discard, first.Body)
	first.Body.Close()
	etag := first.Header.Get("ETag")
	require.NotEmpty(t, etag)

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/icon/linear", nil)
	req.Header.Set("If-None-Match", etag)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusNotModified, resp.StatusCode)
}

func TestHandler_RejectsInvalidCredName(t *testing.T) {
	h := NewHandler(newResolverForTest(t))
	srv := httptest.NewServer(h)
	defer srv.Close()

	cases := []string{
		"/icon/UPPER", // uppercase
		"/icon/has space",
		"/icon/../etc/passwd", // path traversal
		"/icon/",              // empty
	}
	for _, path := range cases {
		t.Run(path, func(t *testing.T) {
			resp, err := http.Get(srv.URL + path)
			require.NoError(t, err)
			defer resp.Body.Close()
			// Anything 400 or 404 is acceptable; just MUST NOT be 200.
			assert.GreaterOrEqual(t, resp.StatusCode, 400)
		})
	}
}

func TestHandler_Rejects405OnPOST(t *testing.T) {
	h := NewHandler(newResolverForTest(t))
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/icon/linear", "text/plain", nil)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusMethodNotAllowed, resp.StatusCode)
}

// Compile-time check the handler accepts the resolver shape used by main.go.
var _ interface {
	Lookup(context.Context, string) Entry
} = (*Resolver)(nil)
