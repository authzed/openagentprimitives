package admind

import (
	"context"
	"net/http"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/pkg/platform/cloud"
)

// clusterTypeUnknown labels a cluster admind could not positively identify —
// nodes it could not list, a cluster with no nodes, or a providerID no
// registered cloud kind claims. It is admind's own sentinel, not a cloud kind:
// the UI header widget renders it gracefully rather than mislabeling the
// cluster as one of the kinds in pkg/platform/cloud.
const clusterTypeUnknown = "unknown"

// kindProviderIDPrefix is the providerID a kind (kubernetes-in-docker) node
// carries. No cloud.Strategy claims it: `local` deliberately reports no
// ProviderIDPrefix so cloud.Detect can never auto-select the developer profile
// on real infrastructure, which leaves this the one identification admind must
// make itself.
const kindProviderIDPrefix = "kind://"

// clusterCacheTTL bounds how long a resolved ClusterInfo is served from the
// in-memory cache before detectCluster re-probes. A cluster's identity does not
// change over a pod's lifetime, so a few minutes eliminates the per-request
// node list (and, on GKE, the two metadata fetches) that every Overview header
// render would otherwise trigger, while still picking up a rebuild within a
// bounded window.
const clusterCacheTTL = 5 * time.Minute

// ClusterInfo is the best-effort cluster identity for the admin Overview
// header. Type is detected from node spec.providerID prefixes and is always
// set. Name/ConsoleURL/Distribution are filled only when cleanly derivable and
// are omitted otherwise — never guessed.
type ClusterInfo struct {
	// Name is the cluster's display name; empty when not cleanly derivable.
	Name string `json:"name,omitempty"`
	// Type is the cloud kind that owns this cluster (a pkg/platform/cloud key),
	// or "unknown" when nothing claimed it.
	Type string `json:"type"`
	// ConsoleURL deep-links the cloud console's cluster page; empty when the
	// cloud cannot derive one.
	ConsoleURL string `json:"consoleURL,omitempty"`
	// Distribution is a finer flavor when known (e.g. "kind" for a kind
	// cluster); omitted otherwise.
	Distribution string `json:"distribution,omitempty"`
}

// handleCluster reports the cluster's cloud type + best-effort name/console URL.
// It degrades to {type:"unknown"} (never a 500) on any detection failure so a
// header widget can't take down the Overview page — the failure is logged for
// an operator, per the no-silent-errors rule.
func (a *Admind) handleCluster(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, a.detectCluster(r.Context()))
}

// detectCluster returns the cluster identity, served from a short-TTL in-memory
// cache (clusterCacheTTL) when warm. On a miss it runs the real detection once
// and memoizes the result — success OR negative — so a non-GKE cluster does not
// re-probe on every header render. Thread-safe: the cache is guarded by
// clusterMu, but the (potentially slow) probe runs WITHOUT the lock held so
// concurrent /cluster requests are not serialized behind one metadata fetch.
func (a *Admind) detectCluster(ctx context.Context) ClusterInfo {
	a.clusterMu.Lock()
	if a.clusterCached != nil && time.Now().Before(a.clusterExpires) {
		info := *a.clusterCached
		a.clusterMu.Unlock()
		return info
	}
	a.clusterMu.Unlock()

	info := a.detectClusterUncached(ctx)

	a.clusterMu.Lock()
	cached := info
	a.clusterCached = &cached
	a.clusterExpires = time.Now().Add(clusterCacheTTL)
	a.clusterMu.Unlock()
	return info
}

// detectClusterUncached lists a few Nodes through the UNCACHED APIReader
// (get;list only — no watch informer is started on Nodes; falls back to the
// cached client in tests) and classifies the cluster from their providerIDs.
func (a *Admind) detectClusterUncached(ctx context.Context) ClusterInfo {
	reader := a.cfg.APIReader
	if reader == nil {
		reader = a.cfg.K8s
	}
	var nodes corev1.NodeList
	if err := reader.List(ctx, &nodes, client.Limit(5)); err != nil {
		a.cfg.Logger.Info("admind: cluster node list failed; reporting type unknown", "err", err.Error())
		return ClusterInfo{Type: clusterTypeUnknown}
	}
	if len(nodes.Items) == 0 {
		return ClusterInfo{Type: clusterTypeUnknown}
	}
	if len(cloud.RegisteredKeys()) == 0 {
		// No managed cloud can ever match, so a GKE/EKS/AKS cluster would be
		// reported as if its nodes had been inspected and rejected. Say why —
		// the local fallbacks below are registry-independent and still apply.
		a.cfg.Logger.Info("admind: no cloud kinds registered in this binary " +
			"(missing a blank import of pkg/platform/cloud/...); a managed cluster will report type unknown")
	}
	return a.clusterInfoFromNodes(ctx, nodes.Items)
}

// clusterInfoFromNodes classifies a cluster by asking the pkg/platform/cloud
// registry which kind owns each node's spec.providerID prefix, then asking that
// kind to name the cluster and link to its console. Every cloud-owned fact —
// the prefix, the kind key, the console URL — comes from the Strategy, so a new
// cloud is a new registration and not a case here.
//
// The two fallbacks below are the cases the registry deliberately cannot
// express: `local` reports no providerID prefix (it is opt-in only, so Detect
// can never land a developer profile on real infrastructure), which leaves
// admind to recognize a developer cluster from the node itself.
func (a *Admind) clusterInfoFromNodes(ctx context.Context, nodes []corev1.Node) ClusterInfo {
	for i := range nodes {
		id := nodes[i].Spec.ProviderID
		if strings.HasPrefix(id, kindProviderIDPrefix) {
			return ClusterInfo{Type: cloud.KeyLocal, Distribution: "kind"}
		}
		s, ok := cloud.ForProviderID(id)
		if !ok {
			continue
		}
		ident := s.ResolveClusterIdentity(ctx, cloud.ClusterIdentityParams{
			Node:            nodes[i],
			MetadataBaseURL: a.cfg.MetadataBaseURL,
			Logger:          a.cfg.Logger,
		})
		return ClusterInfo{Type: s.Key(), Name: ident.Name, ConsoleURL: ident.ConsoleURL}
	}
	// No registered cloud claimed any node. An empty or docker-desktop
	// providerID is a developer cluster; an unrecognized non-empty one stays
	// unknown rather than being labeled as something it may not be.
	if id := nodes[0].Spec.ProviderID; id == "" || strings.HasPrefix(id, "docker") {
		return ClusterInfo{Type: cloud.KeyLocal}
	}
	return ClusterInfo{Type: clusterTypeUnknown}
}
