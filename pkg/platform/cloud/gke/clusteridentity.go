package gke

import (
	"context"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"

	"github.com/authzed/openagentprimitives/pkg/platform/cloud"
)

// defaultMetadataBaseURL is the in-cluster GCE metadata server. GKE nodes
// expose the cluster's own name + location under instance/attributes, which is
// authoritative — the node-name heuristic below is only a fallback.
const defaultMetadataBaseURL = "http://metadata.google.internal"

// metadataTimeout bounds each metadata attribute fetch. The server is a
// link-local endpoint (169.254.169.254); a couple of seconds is generous and
// keeps a caller's request responsive when the server is absent.
const metadataTimeout = 2 * time.Second

// nodepoolLabel marks a node's GKE node pool; its presence confirms the node
// is GKE-managed and (with the node name) lets us recover the cluster name.
const nodepoolLabel = "cloud.google.com/gke-nodepool"

// ResolveClusterIdentity derives the GKE cluster's name and Cloud Console deep
// link from a gce:// node. The project comes from the providerID; the name and
// location come from the GCE metadata server (authoritative), falling back to
// the node-name heuristic and the providerID's zone when that server is
// unreachable. The console link is built only when project, location, and name
// are all present, so a partially-derived identity never links somewhere wrong.
func (Strategy) ResolveClusterIdentity(ctx context.Context, p cloud.ClusterIdentityParams) cloud.ClusterIdentity {
	project, zone, ok := parseGCEProviderID(p.Node.Spec.ProviderID)
	if !ok {
		return cloud.ClusterIdentity{}
	}
	baseURL := p.MetadataBaseURL
	if baseURL == "" {
		baseURL = defaultMetadataBaseURL
	}
	name := metadataAttr(ctx, baseURL, "cluster-name", p.Logger)
	location := metadataAttr(ctx, baseURL, "cluster-location", p.Logger)
	if name == "" {
		name = clusterNameFromNode(&p.Node)
	}
	if location == "" {
		location = zone
	}
	id := cloud.ClusterIdentity{Name: name}
	if project != "" && location != "" && name != "" {
		id.ConsoleURL = "https://console.cloud.google.com/kubernetes/clusters/details/" +
			location + "/" + name + "?project=" + project
	}
	return id
}

// metadataAttr fetches one instance attribute from the GCE metadata server.
// The Metadata-Flavor: Google header is mandatory — the server rejects
// requests without it. Returns "" (logged) on any error, so an off-GKE or
// firewalled caller degrades to the heuristic instead of failing.
func metadataAttr(ctx context.Context, baseURL, attr string, logger logr.Logger) string {
	ctx, cancel := context.WithTimeout(ctx, metadataTimeout)
	defer cancel()
	url := strings.TrimRight(baseURL, "/") + "/computeMetadata/v1/instance/attributes/" + attr
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		logger.Info("gke: build metadata request failed", "attr", attr, "err", err.Error())
		return ""
	}
	req.Header.Set("Metadata-Flavor", "Google")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		logger.Info("gke: metadata query failed; falling back to the node-name heuristic",
			"attr", attr, "err", err.Error())
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		logger.Info("gke: metadata non-200", "attr", attr, "status", resp.StatusCode)
		return ""
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		logger.Info("gke: metadata read failed", "attr", attr, "err", err.Error())
		return ""
	}
	return strings.TrimSpace(string(b))
}

// clusterNameFromNode best-effort-recovers the cluster name from a node. GKE
// names nodes "gke-<cluster>-<nodepool>-<hash>"; with the node pool known from
// the gke-nodepool label, the cluster segment is the prefix before the last
// "-<nodepool>-". This is heuristic — GKE truncates long node names — so any
// mismatch (no label, or a name that does not fit the pattern) returns "" and
// leaves the name blank rather than guessing wrong.
func clusterNameFromNode(node *corev1.Node) string {
	pool := node.Labels[nodepoolLabel]
	if pool == "" {
		return ""
	}
	rest, ok := strings.CutPrefix(node.Name, "gke-")
	if !ok {
		return ""
	}
	i := strings.LastIndex(rest, "-"+pool+"-")
	if i <= 0 {
		return ""
	}
	return rest[:i]
}
