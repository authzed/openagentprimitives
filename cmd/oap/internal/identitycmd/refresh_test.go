package identitycmd

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/aptest"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/refresh"
)

// buildRefreshFixture creates a fake client pre-populated with an
// AgentIdentity (name=ai) that has one oauth credential referencing a
// Secret (name=creds). The Secret's token_endpoint is set to srvURL.
func buildRefreshFixture(t *testing.T, srvURL string) (client.Client, *spiceboxv1alpha1.AgentIdentity) {
	t.Helper()
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "creds", Namespace: "default"},
		Data: map[string][]byte{
			"access_token":   []byte("old-at"),
			"refresh_token":  []byte("old-rt"),
			"token_endpoint": []byte(srvURL),
			"client_id":      []byte("client-x"),
		},
	}
	ai := &spiceboxv1alpha1.AgentIdentity{
		ObjectMeta: metav1.ObjectMeta{Name: "ai", Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentIdentitySpec{
			Credentials: []spiceboxv1alpha1.AgentCredential{
				{
					Name: "tok",
					Type: "oauth",
					OAuth: &spiceboxv1alpha1.OAuthCredentialSource{
						SecretRef: spiceboxv1alpha1.SecretRef{Name: "creds"},
					},
				},
			},
		},
	}
	c := aptest.ClientBuilder(t).WithObjects(sec, ai).Build()
	return c, ai
}

func TestRunIdentityRefresh_AllOAuth(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "new-at",
			"refresh_token": "new-rt",
			"expires_in":    3600,
		})
	}))
	t.Cleanup(srv.Close)
	refresh.SetHTTPClient(srv.Client())

	c, _ := buildRefreshFixture(t, srv.URL)
	var out bytes.Buffer
	require.NoError(t,
		runIdentityRefresh(context.Background(), &out, c, "default", "ai", "", true, 5*time.Minute),
		"runIdentityRefresh")
	assert.Contains(t, out.String(), "tok refreshed", "output should report refresh")

	var got corev1.Secret
	require.NoError(t,
		c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "creds"}, &got),
		"get refreshed Secret")
	assert.Equal(t, "new-at", string(got.Data["access_token"]), "access_token rotated")
}

func TestRunIdentityRefresh_SpecificCredential(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "refreshed-at",
			"expires_in":   1800,
		})
	}))
	t.Cleanup(srv.Close)
	refresh.SetHTTPClient(srv.Client())

	c, _ := buildRefreshFixture(t, srv.URL)
	var out bytes.Buffer
	require.NoError(t,
		runIdentityRefresh(context.Background(), &out, c, "default", "ai", "tok", false, 5*time.Minute),
		"runIdentityRefresh")
	assert.Contains(t, out.String(), "tok refreshed", "output should report refresh")
}

// TestRunIdentityRefresh_UnrecognizedTypeSkippedVisibly is the R15-shaped
// case for the `cr.Type != "oauth"` → `!k.NeedsRefresh()` migration: for
// every type this build DOES recognize, both the old literal comparison and
// the new registry dispatch agree (oauth needs refresh, static/federated
// don't), so no test built only from known types can tell old and new code
// apart. An UNREGISTERED type is the one input where they diverge: the old
// code silently `continue`d with no output at all, while the new code must
// print a visible skip line (never silently drop an unknown type) — and it
// must not abort the walk, so the identity's real oauth credential still
// refreshes.
func TestRunIdentityRefresh_UnrecognizedTypeSkippedVisibly(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "new-at",
			"refresh_token": "new-rt",
			"expires_in":    3600,
		})
	}))
	t.Cleanup(srv.Close)
	refresh.SetHTTPClient(srv.Client())

	c, ai := buildRefreshFixture(t, srv.URL)
	ai.Spec.Credentials = append(ai.Spec.Credentials, spiceboxv1alpha1.AgentCredential{
		Name: "mystery", Type: "nosuch",
	})
	require.NoError(t, c.Update(context.Background(), ai))

	var out bytes.Buffer
	require.NoError(t,
		runIdentityRefresh(context.Background(), &out, c, "default", "ai", "", true, 5*time.Minute),
		"runIdentityRefresh")
	assert.Contains(t, out.String(), `unrecognized credential type "nosuch", skipped`,
		"an unregistered type must print a visible skip line, unlike the old silent continue")
	assert.Contains(t, out.String(), "mystery",
		"the skip line must name the credential, not just the type")
	assert.Contains(t, out.String(), "tok refreshed",
		"the unrecognized credential must not abort the walk over the rest of the identity's credentials")
}

func TestRunIdentityRefresh_MissingCredential(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	t.Cleanup(srv.Close)
	refresh.SetHTTPClient(srv.Client())

	c, _ := buildRefreshFixture(t, srv.URL)
	var out bytes.Buffer
	err := runIdentityRefresh(context.Background(), &out, c, "default", "ai", "nonexistent", false, 5*time.Minute)
	assert.Error(t, err, "nonexistent credential should error")
}

func TestRunIdentityRefresh_ThresholdSkipsNotExpiring(t *testing.T) {
	// Set expires_at to 1 hour in the future — should be skipped under default 5m threshold.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("should not call token endpoint for non-expiring credential")
	}))
	t.Cleanup(srv.Close)
	refresh.SetHTTPClient(srv.Client())

	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "creds2", Namespace: "default"},
		Data: map[string][]byte{
			"access_token":   []byte("at"),
			"refresh_token":  []byte("rt"),
			"token_endpoint": []byte(srv.URL),
			"expires_at":     []byte(time.Now().Add(1 * time.Hour).UTC().Format(time.RFC3339)),
		},
	}
	ai := &spiceboxv1alpha1.AgentIdentity{
		ObjectMeta: metav1.ObjectMeta{Name: "ai2", Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentIdentitySpec{
			Credentials: []spiceboxv1alpha1.AgentCredential{
				{
					Name: "tok2",
					Type: "oauth",
					OAuth: &spiceboxv1alpha1.OAuthCredentialSource{
						SecretRef: spiceboxv1alpha1.SecretRef{Name: "creds2"},
					},
				},
			},
		},
	}
	c := aptest.ClientBuilder(t).WithObjects(sec, ai).Build()
	var out bytes.Buffer
	require.NoError(t,
		runIdentityRefresh(context.Background(), &out, c, "default", "ai2", "", false, 5*time.Minute),
		"runIdentityRefresh")
	assert.Contains(t, out.String(), "skipped", "output should report skip")
}

func TestRunIdentityRefresh_ThresholdRefreshesExpiring(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "fresh-at",
			"expires_in":   3600,
		})
	}))
	t.Cleanup(srv.Close)
	refresh.SetHTTPClient(srv.Client())

	// Expires 1 minute from now — within the 5m default threshold.
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "creds3", Namespace: "default"},
		Data: map[string][]byte{
			"access_token":   []byte("old-at"),
			"refresh_token":  []byte("old-rt"),
			"token_endpoint": []byte(srv.URL),
			"expires_at":     []byte(time.Now().Add(1 * time.Minute).UTC().Format(time.RFC3339)),
		},
	}
	ai := &spiceboxv1alpha1.AgentIdentity{
		ObjectMeta: metav1.ObjectMeta{Name: "ai3", Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentIdentitySpec{
			Credentials: []spiceboxv1alpha1.AgentCredential{
				{
					Name: "tok3",
					Type: "oauth",
					OAuth: &spiceboxv1alpha1.OAuthCredentialSource{
						SecretRef: spiceboxv1alpha1.SecretRef{Name: "creds3"},
					},
				},
			},
		},
	}
	c := aptest.ClientBuilder(t).WithObjects(sec, ai).Build()
	var out bytes.Buffer
	require.NoError(t,
		runIdentityRefresh(context.Background(), &out, c, "default", "ai3", "", false, 5*time.Minute),
		"runIdentityRefresh")
	assert.Contains(t, out.String(), "tok3 refreshed", "output should report refresh")

	var got corev1.Secret
	require.NoError(t,
		c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "creds3"}, &got),
		"get refreshed Secret")
	assert.Equal(t, "fresh-at", string(got.Data["access_token"]), "access_token rotated")
}
