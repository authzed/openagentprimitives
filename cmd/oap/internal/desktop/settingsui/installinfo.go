package settingsui

import (
	"fmt"
	"net/http"
	"sort"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	"github.com/authzed/openagentprimitives/pkg/platform/cloud"
)

// operatorEnvAllowlist is the exact set of operator container env var names
// GET /api/cluster/install-info will echo back to the settings UI. It is an
// ALLOWLIST, not a denylist, on purpose: a future env var added to the
// operator Deployment (a new secret-bearing connection string, say) must be
// invisible here by default, not visible until someone remembers to add it
// to a denylist. Only plain Value entries ever qualify — see
// allowedOperatorEnv — a ValueFrom (Secret/ConfigMap ref) is excluded
// unconditionally regardless of name.
var operatorEnvAllowlist = map[string]bool{
	"MEMORY_BACKEND":        true,
	"MEMORY_READ_SOURCE":    true,
	"KG_INGESTION_STRATEGY": true,
	cloud.ClusterKindEnvVar: true, // "AP_CLUSTER_KIND"
	"WATCH_NAMESPACES":      true,
	"SECRET_GUARD_MODE":     true,
	"ARTIFACT_STORE_URL":    true,
}

// installInfoResponse is the GET /api/cluster/install-info wire shape: a
// read-only snapshot of what's actually installed and running, for
// support/debugging. Like clusterSettingsResponse, ClusterDown is a STATE
// (the desktop cluster is not up), not an error — see
// handleClusterInstallInfoGet.
type installInfoResponse struct {
	ClusterDown bool            `json:"clusterDown"`
	ClusterKind string          `json:"clusterKind"`
	Components  []componentInfo `json:"components"` // one per Deployment in the system namespace
	OperatorEnv []envEntry      `json:"operatorEnv"`
	Node        *nodeInfo       `json:"node,omitempty"`
	AppVersion  string          `json:"appVersion"`
}

// componentInfo is one Deployment in the system namespace.
type componentInfo struct {
	Name  string `json:"name"`
	Image string `json:"image"`
	Ready string `json:"ready"` // "1/1"
}

// envEntry is one allowlisted operator env var.
type envEntry struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// nodeInfo is the first node reported by the cluster.
type nodeInfo struct {
	Name           string `json:"name"`
	KubeletVersion string `json:"kubeletVersion"`
	Ready          bool   `json:"ready"`
}

// handleClusterInstallInfoGet serves a read-only snapshot of what's actually
// running: every Deployment in the system namespace (sorted by name), the
// operator Deployment's allowlisted env (see operatorEnvAllowlist), the
// AP_CLUSTER_KIND it carries, the first node, and the oap build's own
// version (Deps.AppVersion — see its doc; there is no exported "oap's own
// version" helper reachable from this package, so the wiring task supplies
// it). A down cluster is a STATE, not an error — same 200-with-a-flag shape
// as handleClusterSettingsGet.
//
// The operator Deployment being absent (a cluster installed before it
// existed, or a non-standard namespace) is likewise not an error: it just
// means there is nothing to read env/clusterKind from, so those fields come
// back empty while components and node are still reported.
func (s *Server) handleClusterInstallInfoGet(w http.ResponseWriter, r *http.Request) {
	b, err := s.deps.Clients()
	if err != nil {
		s.writeJSON(w, http.StatusOK, installInfoResponse{
			ClusterDown: true,
			Components:  []componentInfo{},
			OperatorEnv: []envEntry{},
		})
		return
	}
	ctx := r.Context()

	deployments, err := b.Typed.AppsV1().Deployments(apcmd.SystemNamespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		s.deps.Logf("settingsui: list deployments in %s: %v", apcmd.SystemNamespace, err)
		http.Error(w, fmt.Sprintf("settings: list deployments: %v", err), http.StatusInternalServerError)
		return
	}

	components := make([]componentInfo, 0, len(deployments.Items))
	operatorEnv := make([]envEntry, 0) // non-nil like components: "[]" on the wire, never JSON null, whether the operator is absent or just has no matching env
	for _, dep := range deployments.Items {
		components = append(components, componentInfo{
			Name:  dep.Name,
			Image: firstContainerImage(dep),
			Ready: fmt.Sprintf("%d/%d", dep.Status.ReadyReplicas, dep.Status.Replicas),
		})
		if dep.Name == apcmd.OperatorDeployment {
			operatorEnv = allowedOperatorEnv(dep)
		}
	}
	sort.Slice(components, func(i, j int) bool { return components[i].Name < components[j].Name })

	var clusterKind string
	for _, e := range operatorEnv {
		if e.Name == cloud.ClusterKindEnvVar {
			clusterKind = e.Value
			break
		}
	}

	nodes, err := b.Typed.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		s.deps.Logf("settingsui: list nodes: %v", err)
		http.Error(w, fmt.Sprintf("settings: list nodes: %v", err), http.StatusInternalServerError)
		return
	}
	var node *nodeInfo
	if len(nodes.Items) > 0 {
		first := nodes.Items[0]
		node = &nodeInfo{
			Name:           first.Name,
			KubeletVersion: first.Status.NodeInfo.KubeletVersion,
			Ready:          nodeReady(first),
		}
	}

	s.writeJSON(w, http.StatusOK, installInfoResponse{
		ClusterKind: clusterKind,
		Components:  components,
		OperatorEnv: operatorEnv,
		Node:        node,
		AppVersion:  s.deps.AppVersion,
	})
}

// firstContainerImage returns the image of dep's first container, or "" if
// it has none — a Deployment with no containers wouldn't pass admission on a
// real cluster, but a defensive default beats an index panic here.
func firstContainerImage(dep appsv1.Deployment) string {
	containers := dep.Spec.Template.Spec.Containers
	if len(containers) == 0 {
		return ""
	}
	return containers[0].Image
}

// allowedOperatorEnv walks every container's env on the operator Deployment
// and returns the entries whose name is in operatorEnvAllowlist AND whose
// value is a plain Value, never ValueFrom — see operatorEnvAllowlist's doc
// for why this is an allowlist rather than a denylist. Mirrors
// cloud.Stamped's container-env walk (pkg/platform/cloud/stamped.go).
func allowedOperatorEnv(dep appsv1.Deployment) []envEntry {
	out := make([]envEntry, 0)
	for _, c := range dep.Spec.Template.Spec.Containers {
		for _, e := range c.Env {
			if e.ValueFrom != nil || !operatorEnvAllowlist[e.Name] {
				continue
			}
			out = append(out, envEntry{Name: e.Name, Value: e.Value})
		}
	}
	return out
}

// nodeReady reports whether n's NodeReady condition is True.
func nodeReady(n corev1.Node) bool {
	for _, c := range n.Status.Conditions {
		if c.Type == corev1.NodeReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}
