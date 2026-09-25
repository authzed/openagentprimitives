package gke

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-logr/logr/testr"
	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/authzed/openagentprimitives/pkg/platform/cloud"
)

// identityNode builds a Node with the given name, providerID, and node-pool label.
func identityNode(t *testing.T, name, providerID, pool string) corev1.Node {
	t.Helper()
	n := corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       corev1.NodeSpec{ProviderID: providerID},
	}
	if pool != "" {
		n.Labels = map[string]string{nodepoolLabel: pool}
	}
	return n
}

// metadataServer serves the two GKE cluster attributes, recording the
// Metadata-Flavor header the real server requires.
func metadataServer(t *testing.T, name, location string, gotFlavor *string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*gotFlavor = r.Header.Get("Metadata-Flavor")
		switch {
		case strings.HasSuffix(r.URL.Path, "/cluster-name"):
			_, _ = w.Write([]byte(name + "\n"))
		case strings.HasSuffix(r.URL.Path, "/cluster-location"):
			_, _ = w.Write([]byte(location))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestResolveClusterIdentity(t *testing.T) {
	// An unroutable metadata base URL fails fast (connection refused) so the
	// heuristic cases never make a real network call.
	const refusedMetadata = "http://127.0.0.1:1"

	cases := []struct {
		name        string
		node        corev1.Node
		metadata    string
		wantName    string
		wantConsole string
	}{
		{
			name:     "non-gce providerID: nothing derivable, no probe",
			node:     identityNode(t, "ip-10-0-0-1", "aws:///us-west-2a/i-0abc", ""),
			metadata: refusedMetadata,
		},
		{
			name:     "gce providerID, no node-pool label: name stays blank rather than guessed",
			node:     identityNode(t, "gke-x-p-abc", "gce://proj/us-central1-a/gke-x-p-abc", ""),
			metadata: refusedMetadata,
		},
		{
			name:        "metadata unreachable: node-name heuristic + providerID zone build the console URL",
			node:        identityNode(t, "gke-my-cluster-default-pool-abc123", "gce://my-proj/us-central1-a/gke-my-cluster-default-pool-abc123", "default-pool"),
			metadata:    refusedMetadata,
			wantName:    "my-cluster",
			wantConsole: "https://console.cloud.google.com/kubernetes/clusters/details/us-central1-a/my-cluster?project=my-proj",
		},
		{
			name:     "malformed gce providerID: nothing derivable",
			node:     identityNode(t, "gke-x-p-abc", "gce://proj", "default-pool"),
			metadata: refusedMetadata,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Strategy{}.ResolveClusterIdentity(context.Background(), cloud.ClusterIdentityParams{
				Node:            tc.node,
				MetadataBaseURL: tc.metadata,
				Logger:          testr.New(t),
			})
			assert.Equal(t, tc.wantName, got.Name)
			assert.Equal(t, tc.wantConsole, got.ConsoleURL)
		})
	}
}

// TestResolveClusterIdentity_MetadataServerWins proves the metadata server is
// authoritative over both the node-name heuristic and the providerID's zone,
// and that the mandatory Metadata-Flavor header is sent.
func TestResolveClusterIdentity_MetadataServerWins(t *testing.T) {
	var gotFlavor string
	base := metadataServer(t, "prod-cluster", "us-central1", &gotFlavor)

	got := Strategy{}.ResolveClusterIdentity(context.Background(), cloud.ClusterIdentityParams{
		// The node's heuristic name and its us-central1-a zone must both lose.
		Node: identityNode(t, "gke-heuristic-name-default-pool-abc123",
			"gce://my-proj/us-central1-a/gke-heuristic-name-default-pool-abc123", "default-pool"),
		MetadataBaseURL: base,
		Logger:          testr.New(t),
	})

	assert.Equal(t, "prod-cluster", got.Name, "metadata cluster-name wins over the heuristic")
	assert.Equal(t,
		"https://console.cloud.google.com/kubernetes/clusters/details/us-central1/prod-cluster?project=my-proj",
		got.ConsoleURL, "console URL uses the metadata location + name and the providerID project")
	assert.Equal(t, "Google", gotFlavor, "the mandatory Metadata-Flavor header is sent")
}
