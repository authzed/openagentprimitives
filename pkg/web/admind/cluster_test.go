package admind_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-logr/logr/testr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/web/admind"
	// Blank-imported for their cloud.Strategy registrations: admind classifies
	// a cluster by asking the pkg/platform/cloud registry which kind owns a
	// node's providerID prefix, so this test binary must link the kinds the
	// operator does (internal/cmd/operator/cloudimports.go) or every cluster reports
	// type "unknown".
	_ "github.com/authzed/openagentprimitives/pkg/platform/cloud/aks"
	_ "github.com/authzed/openagentprimitives/pkg/platform/cloud/eks"
	_ "github.com/authzed/openagentprimitives/pkg/platform/cloud/gke"
	_ "github.com/authzed/openagentprimitives/pkg/platform/cloud/local"
	_ "github.com/authzed/openagentprimitives/pkg/platform/cloud/unmanaged"
)

// countingReader wraps a client.Reader and counts List calls, so a test can
// prove detectCluster's result is memoized (the second GET /cluster does not
// re-list Nodes).
type countingReader struct {
	client.Reader
	lists int
}

func (c *countingReader) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	c.lists++
	return c.Reader.List(ctx, list, opts...)
}

// node builds a corev1.Node with the given name, providerID, and labels.
func node(name, providerID string, labels map[string]string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels},
		Spec:       corev1.NodeSpec{ProviderID: providerID},
	}
}

// getCluster GETs /admin/v1/cluster as the admin subject and decodes the body.
func getCluster(t *testing.T, a *admind.Admind) (int, admind.ClusterInfo) {
	t.Helper()
	w := do(t, a.Handler(), http.MethodGet, "/admin/v1/cluster", "test-token", "user:YWRtaW4", "")
	var info admind.ClusterInfo
	if w.Code == http.StatusOK {
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &info))
	}
	return w.Code, info
}

func TestClusterInfo_DetectsTypeByProviderID(t *testing.T) {
	cases := []struct {
		name        string
		node        *corev1.Node
		wantType    string
		wantDistro  string
		wantName    string
		wantConsole string
	}{
		{
			name:     "gce:// providerID → gke",
			node:     node("gke-x-p-abc", "gce://proj/us-central1-a/gke-x-p-abc", nil),
			wantType: "gke",
		},
		{
			name:        "gke node name + nodepool label → name + console URL",
			node:        node("gke-my-cluster-default-pool-abc123", "gce://my-proj/us-central1-a/gke-my-cluster-default-pool-abc123", map[string]string{"cloud.google.com/gke-nodepool": "default-pool"}),
			wantType:    "gke",
			wantName:    "my-cluster",
			wantConsole: "https://console.cloud.google.com/kubernetes/clusters/details/us-central1-a/my-cluster?project=my-proj",
		},
		{
			name:     "aws:// providerID → eks",
			node:     node("ip-10-0-0-1", "aws:///us-west-2a/i-0abc", nil),
			wantType: "eks",
		},
		{
			name:     "azure:// providerID → aks",
			node:     node("aks-np-0", "azure:///subscriptions/x/vm/0", nil),
			wantType: "aks",
		},
		{
			name:       "kind:// providerID → local (kind)",
			node:       node("kind-control-plane", "kind://docker/kind/kind-control-plane", nil),
			wantType:   "local",
			wantDistro: "kind",
		},
		{
			name:     "empty providerID → local",
			node:     node("docker-desktop", "", nil),
			wantType: "local",
		},
		{
			name:     "unrecognized providerID → unknown",
			node:     node("weird", "openstack:///abc", nil),
			wantType: "unknown",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			k8s := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(tc.node).Build()
			a := newTestAdmind(t, k8s)
			code, info := getCluster(t, a)
			require.Equal(t, http.StatusOK, code)
			assert.Equal(t, tc.wantType, info.Type)
			assert.Equal(t, tc.wantDistro, info.Distribution)
			assert.Equal(t, tc.wantName, info.Name)
			assert.Equal(t, tc.wantConsole, info.ConsoleURL)
		})
	}
}

// TestClusterInfo_GKEMetadataServerWins proves the GCE metadata server is
// authoritative for a GKE cluster's name + location (over the node-name
// heuristic), the Metadata-Flavor: Google header is sent, and the console URL
// uses the metadata location. A metadata failure falls back to the heuristic.
func TestClusterInfo_GKEMetadataServerWins(t *testing.T) {
	var gotFlavor string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotFlavor = r.Header.Get("Metadata-Flavor")
		switch {
		case strings.HasSuffix(r.URL.Path, "/cluster-name"):
			_, _ = w.Write([]byte("prod-cluster\n"))
		case strings.HasSuffix(r.URL.Path, "/cluster-location"):
			_, _ = w.Write([]byte("us-central1"))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	// The node's providerID zone (us-central1-a) and heuristic name
	// (heuristic-name) must both LOSE to the metadata name/location.
	n := node("gke-heuristic-name-default-pool-abc123",
		"gce://my-proj/us-central1-a/gke-heuristic-name-default-pool-abc123",
		map[string]string{"cloud.google.com/gke-nodepool": "default-pool"})
	k8s := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(n).Build()

	mem := memory.NewLocal(inmem.NewBackend())
	a, err := admind.New(admind.Config{
		Mem:             mem,
		K8s:             k8s,
		Checker:         stubChecker{allow: map[string]bool{"YWRtaW4": true}},
		Token:           "test-token",
		Logger:          testr.New(t),
		MetadataBaseURL: srv.URL,
	})
	require.NoError(t, err)

	code, info := getCluster(t, a)
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, "gke", info.Type)
	assert.Equal(t, "prod-cluster", info.Name, "metadata cluster-name wins over the heuristic")
	assert.Equal(t,
		"https://console.cloud.google.com/kubernetes/clusters/details/us-central1/prod-cluster?project=my-proj",
		info.ConsoleURL, "console URL uses the metadata location + name + providerID project")
	assert.Equal(t, "Google", gotFlavor, "the mandatory Metadata-Flavor header is sent")
}

func TestClusterInfo_NoNodes_IsUnknown(t *testing.T) {
	k8s := fake.NewClientBuilder().WithScheme(newScheme(t)).Build()
	a := newTestAdmind(t, k8s)
	code, info := getCluster(t, a)
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, "unknown", info.Type, "no nodes must degrade to unknown, never 500")
	assert.Empty(t, info.Name)
	assert.Empty(t, info.ConsoleURL)
}

// TestClusterInfo_ResultCachedAcrossRequests proves detectCluster memoizes its
// result (clusterCacheTTL): a second GET /cluster is served from cache — the
// Nodes are listed exactly once — for BOTH a positive (GKE) and a negative
// (local, non-GKE) detection, and returns the identical ClusterInfo.
func TestClusterInfo_ResultCachedAcrossRequests(t *testing.T) {
	cases := []struct {
		name     string
		node     *corev1.Node
		wantType string
		wantName string
	}{
		{
			name:     "gke result cached — nodes listed once across two calls",
			node:     node("gke-my-cluster-default-pool-abc123", "gce://my-proj/us-central1-a/gke-my-cluster-default-pool-abc123", map[string]string{"cloud.google.com/gke-nodepool": "default-pool"}),
			wantType: "gke",
			wantName: "my-cluster",
		},
		{
			name:     "non-gke negative result cached — no re-probe on second call",
			node:     node("docker-desktop", "", nil),
			wantType: "local",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			k8s := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(tc.node).Build()
			cr := &countingReader{Reader: k8s}
			mem := memory.NewLocal(inmem.NewBackend())
			a, err := admind.New(admind.Config{
				Mem:       mem,
				K8s:       k8s,
				APIReader: cr,
				Checker:   stubChecker{allow: map[string]bool{"YWRtaW4": true}},
				Token:     "test-token",
				Logger:    testr.New(t),
				// Refused metadata port → GKE detection falls back to the node-name
				// heuristic fast, without a real network call.
				MetadataBaseURL: "http://127.0.0.1:1",
			})
			require.NoError(t, err)

			code1, info1 := getCluster(t, a)
			code2, info2 := getCluster(t, a)
			require.Equal(t, http.StatusOK, code1)
			require.Equal(t, http.StatusOK, code2)
			assert.Equal(t, tc.wantType, info1.Type)
			assert.Equal(t, tc.wantName, info1.Name)
			assert.Equal(t, info1, info2, "second call returns the cached ClusterInfo")
			assert.Equal(t, 1, cr.lists, "nodes are listed once — the second call is served from cache")
		})
	}
}

func TestClusterInfo_GatedByViewOverview(t *testing.T) {
	k8s := fake.NewClientBuilder().WithScheme(newScheme(t)).
		WithObjects(node("gke-x-p-abc", "gce://proj/us-central1-a/gke-x-p-abc", nil)).Build()
	a := newTestAdmind(t, k8s)
	// Non-admin subject lacks view_overview → 403.
	w := do(t, a.Handler(), http.MethodGet, "/admin/v1/cluster", "test-token", "user:bm9wZQ", "")
	assert.Equal(t, http.StatusForbidden, w.Code, "cluster endpoint needs view_overview")
}
