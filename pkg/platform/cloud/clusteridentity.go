package cloud

import (
	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
)

// ClusterIdentity is a cluster's best-effort display identity for an admin
// surface: what to call the cluster, and where its cloud console page is.
// Every field is optional — a cloud that cannot derive one leaves it empty
// rather than guessing, so a header widget renders what is known and nothing
// more.
type ClusterIdentity struct {
	// Name is the cluster's own name as the cloud knows it.
	Name string
	// ConsoleURL deep-links the cloud console's page for this cluster.
	ConsoleURL string
}

// ClusterIdentityParams carries what a Strategy needs to resolve a cluster's
// display identity.
type ClusterIdentityParams struct {
	// Node is one node of the connected cluster: its spec.providerID and its
	// labels are what every cloud derives cluster identity from.
	Node corev1.Node
	// MetadataBaseURL overrides the base URL of the cloud's instance-metadata
	// endpoint. Empty means the cloud's own default; a test points it at an
	// httptest server, and clouds with no metadata endpoint ignore it.
	MetadataBaseURL string
	// Logger records probe failures. Resolution is best-effort and returns no
	// error, so this is the only place a failed probe is reported.
	Logger logr.Logger
}
