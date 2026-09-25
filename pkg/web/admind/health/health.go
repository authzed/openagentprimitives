// Package health computes the admin Overview's cluster-health snapshot: the
// live up/down state of each first-party platform component plus a namespace
// pod/replica rollup. It is a read-only aggregator — it lists Deployments,
// StatefulSets, and Pods in the operator's namespace via the operator's client
// and (when configured) pings the Graphiti REST endpoint.
//
// Like pkg/web/admind/audit and pkg/web/admind/overview, this package does NOT import
// its parent admind: it depends only on a controller-runtime client and a
// Graphiti URL, both injected, so the parent can wire it without an import
// cycle and tests can drive it with a fake client + httptest.
//
// CPU/memory in the rollup are read best-effort from the metrics API
// (metrics.k8s.io/v1beta1 PodMetrics): the checker sums live per-container
// usage across the namespace. The metrics API is an optional cluster add-on —
// when it is absent (no metrics-server / the APIService is unregistered / List
// fails) CPU/Memory degrade to "n/a" and the snapshot still succeeds. It is the
// only signal here that is not carried on the Deployment/StatefulSet/Pod
// objects themselves.
package health

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/dustin/go-humanize"
	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	metricsv1beta1 "k8s.io/metrics/pkg/apis/metrics/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// metricsUnavailable is the CPU/Memory placeholder when the metrics API cannot
// be read (no metrics-server, unregistered APIService, or a List error). It is
// a neutral marker, never an error — CPU/Memory are best-effort.
const metricsUnavailable = "n/a"

// Namespace is where every first-party component runs.
const Namespace = "agentprimitives-system"

// graphitiTimeout bounds the graphiti liveness ping so a hung endpoint degrades
// graphiti rather than stalling the whole snapshot.
const graphitiTimeout = 2 * time.Second

// restartThreshold downgrades an otherwise-Ready component to Degraded when a
// pod has restarted at least this many times — a flapping component is not
// "Healthy" even while its replica count is satisfied.
const restartThreshold = 5

// Status is a component's health verdict.
type Status string

const (
	StatusHealthy       Status = "Healthy"
	StatusDegraded      Status = "Degraded"
	StatusDown          Status = "Down"
	StatusNotConfigured Status = "NotConfigured"
)

// Component is one platform component's health line.
type Component struct {
	// Name is the logical component name, not the workload's object name.
	Name string `json:"name"`
	// Status is the verdict; a missing workload reports Down, never an error.
	Status Status `json:"status"`
	// Detail is operator-facing supporting text; may be empty when healthy.
	Detail string `json:"detail"`
}

// Rollup is the namespace pod/replica summary.
type Rollup struct {
	// Pods is every pod in the namespace, ready or not.
	Pods int `json:"pods"`
	// Ready is the subset reporting Ready; Ready < Pods is the degraded signal.
	Ready int `json:"ready"`
	// CPU is total live millicore usage ("1200m"), or metricsUnavailable when
	// the optional metrics API cannot be read.
	CPU string `json:"cpu"`
	// Memory is total live working set ("1.5 GiB"), or metricsUnavailable.
	Memory string `json:"memory"`
}

// Report is the full cluster-health snapshot.
type Report struct {
	// Components is one line per first-party component, in reported order.
	Components []Component `json:"components"`
	// Rollup is the namespace-wide pod and resource summary.
	Rollup Rollup `json:"rollup"`
}

// workloadRef maps a logical component name to the Deployment/StatefulSet name
// the platform installs it under. Each component is matched against BOTH a
// Deployment and a StatefulSet by this name (the dependency components may be
// either, depending on the install), and an absent workload is reported Down
// rather than erroring.
type workloadRef struct {
	component string
	workload  string
}

// platformWorkloads is the first-party component set, in reported order.
// Graphiti is NOT here: it is reported via an HTTP liveness ping (Snapshot),
// because the operator talks to it over REST and an endpoint may be configured
// even when the in-cluster Deployment is managed out of band.
var platformWorkloads = []workloadRef{
	{"operator", "spicebox-operator"},
	{"channelsd", "spicebox-channelsd"},
	{"authzd", "agentprimitives-authzd"},
	{"webd", "spicebox-webd"},
	// spicedb is operator-managed (a SpiceDBCluster CR named "spicebox-spicedb"),
	// so the spicedb-operator — not this installer — creates the Deployment, and
	// names it "<cluster>-spicedb". The doubled name is therefore correct: the
	// Service is "spicebox-spicedb" but the Deployment is "spicebox-spicedb-spicedb".
	{"spicedb", "spicebox-spicedb-spicedb"},
	{"postgres", "spicebox-postgres"},
	{"nats", "spicebox-nats"},
}

// Checker computes cluster-health snapshots. Construct with New.
type Checker struct {
	// K8s reads Deployments, StatefulSets, and Pods in Namespace. It is an
	// UNCACHED reader (mgr.GetAPIReader()): the health snapshot is an occasional
	// admin read, so a one-shot direct List is correct — it needs only get;list
	// (the granted RBAC) and avoids a cached client's list+watch informer (which
	// would log "forbidden" watch retries for the operator's lifetime, since
	// watch is intentionally not granted on these resources).
	K8s client.Reader
	// GraphitiURL is the Graphiti REST base URL. Empty => graphiti reported as
	// NotConfigured (a neutral state, not an error).
	GraphitiURL string
	// Namespace overrides the default (Namespace) — used by tests.
	Namespace string
	// HTTPClient pings GraphitiURL. Defaults to a graphitiTimeout client.
	HTTPClient *http.Client
	Logger     logr.Logger
}

// New builds a Checker reading Deployments/StatefulSets/Pods via k8s and, when
// graphitiURL is non-empty, pinging it for graphiti's status.
func New(k8s client.Reader, graphitiURL string, logger logr.Logger) *Checker {
	return &Checker{
		K8s:         k8s,
		GraphitiURL: graphitiURL,
		Namespace:   Namespace,
		HTTPClient:  &http.Client{Timeout: graphitiTimeout},
		Logger:      logger,
	}
}

func (c *Checker) namespace() string {
	if c.Namespace != "" {
		return c.Namespace
	}
	return Namespace
}

func (c *Checker) httpClient() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	return &http.Client{Timeout: graphitiTimeout}
}

// Snapshot lists Deployments, StatefulSets, and Pods in the namespace, maps
// each platform component to a status, appends the graphiti ping result, and
// returns the components plus a pod/replica rollup. A list error is returned
// (never swallowed); a graphiti ping failure degrades only graphiti.
func (c *Checker) Snapshot(ctx context.Context) (Report, error) {
	ns := c.namespace()

	var deps appsv1.DeploymentList
	if err := c.K8s.List(ctx, &deps, client.InNamespace(ns)); err != nil {
		return Report{}, fmt.Errorf("health: list deployments in %s: %w", ns, err)
	}
	var ssets appsv1.StatefulSetList
	if err := c.K8s.List(ctx, &ssets, client.InNamespace(ns)); err != nil {
		return Report{}, fmt.Errorf("health: list statefulsets in %s: %w", ns, err)
	}
	var pods corev1.PodList
	if err := c.K8s.List(ctx, &pods, client.InNamespace(ns)); err != nil {
		return Report{}, fmt.Errorf("health: list pods in %s: %w", ns, err)
	}

	depByName := make(map[string]*appsv1.Deployment, len(deps.Items))
	for i := range deps.Items {
		depByName[deps.Items[i].Name] = &deps.Items[i]
	}
	ssByName := make(map[string]*appsv1.StatefulSet, len(ssets.Items))
	for i := range ssets.Items {
		ssByName[ssets.Items[i].Name] = &ssets.Items[i]
	}

	cpu, mem := c.podMetricsUsage(ctx, ns)
	report := Report{
		Rollup: Rollup{Pods: len(pods.Items), Ready: countReady(pods.Items), CPU: cpu, Memory: mem},
	}

	for _, ref := range platformWorkloads {
		switch {
		case depByName[ref.workload] != nil:
			d := depByName[ref.workload]
			report.Components = append(report.Components, componentFromCounts(
				ref.component, desired(d.Spec.Replicas), d.Status.AvailableReplicas, d.Status.ReadyReplicas,
				matchingPods(d.Spec.Selector, pods.Items)))
		case ssByName[ref.workload] != nil:
			s := ssByName[ref.workload]
			// StatefulSet has no AvailableReplicas in every served version; its
			// ReadyReplicas is the reliable "available" signal.
			report.Components = append(report.Components, componentFromCounts(
				ref.component, desired(s.Spec.Replicas), s.Status.ReadyReplicas, s.Status.ReadyReplicas,
				matchingPods(s.Spec.Selector, pods.Items)))
		default:
			report.Components = append(report.Components, Component{
				Name: ref.component, Status: StatusDown, Detail: "not found",
			})
		}
	}

	report.Components = append(report.Components, c.graphiti(ctx))
	return report, nil
}

// podMetricsUsage sums live per-container CPU and memory usage across the
// namespace via the metrics API (metrics.k8s.io/v1beta1 PodMetrics) and returns
// them formatted as millicores (e.g. "1200m") and a human-readable working-set
// (e.g. "1.5 GiB").
//
// The metrics API is an optional cluster add-on: when it is absent (no
// metrics-server, an unregistered APIService, a NotFound, or any List error)
// this reports both as "n/a" and logs at Info — it NEVER fails the health
// snapshot. That is why the caller ignores the error path entirely: CPU/Memory
// are best-effort adornments on a rollup that stands on the replica/pod counts.
func (c *Checker) podMetricsUsage(ctx context.Context, ns string) (cpu, memory string) {
	var pm metricsv1beta1.PodMetricsList
	if err := c.K8s.List(ctx, &pm, client.InNamespace(ns)); err != nil {
		c.Logger.Info("health: pod metrics unavailable; reporting CPU/Memory as n/a",
			"namespace", ns, "err", err.Error())
		return metricsUnavailable, metricsUnavailable
	}
	var cpuQty, memQty resource.Quantity
	for i := range pm.Items {
		for _, cont := range pm.Items[i].Containers {
			if v, ok := cont.Usage[corev1.ResourceCPU]; ok {
				cpuQty.Add(v)
			}
			if v, ok := cont.Usage[corev1.ResourceMemory]; ok {
				memQty.Add(v)
			}
		}
	}
	memBytes := memQty.Value()
	if memBytes < 0 {
		memBytes = 0
	}
	return fmt.Sprintf("%dm", cpuQty.MilliValue()), humanize.IBytes(uint64(memBytes))
}

// componentFromCounts derives a status from desired-vs-available replicas and
// the component's pods. Down when nothing is available (or it is scaled to
// zero); Degraded on a shortfall or a crash-looping/flapping pod; Healthy only
// when every desired replica is available and no pod is in trouble.
func componentFromCounts(name string, desired, available, ready int32, pods []corev1.Pod) Component {
	detail := fmt.Sprintf("%d/%d ready", ready, desired)

	if desired == 0 {
		return Component{Name: name, Status: StatusDown, Detail: "scaled to 0 replicas"}
	}
	trouble, restarts := podTrouble(pods)
	switch {
	case available <= 0:
		return Component{Name: name, Status: StatusDown, Detail: withReason(detail, trouble)}
	case available < desired:
		return Component{Name: name, Status: StatusDegraded, Detail: withReason(detail, trouble)}
	default:
		if trouble != "" {
			return Component{Name: name, Status: StatusDegraded, Detail: withReason(detail, trouble)}
		}
		if restarts >= restartThreshold {
			return Component{Name: name, Status: StatusDegraded, Detail: fmt.Sprintf("%s (restarts=%d)", detail, restarts)}
		}
		return Component{Name: name, Status: StatusHealthy, Detail: detail}
	}
}

func withReason(detail, reason string) string {
	if reason == "" {
		return detail
	}
	return detail + " (" + reason + ")"
}

// graphiti pings the configured Graphiti endpoint. Empty URL => NotConfigured.
// A 2xx is Healthy; any other status, a transport error, or a timeout is
// Degraded (the core stack runs without graphiti, so this never errors the
// snapshot).
func (c *Checker) graphiti(ctx context.Context) Component {
	const name = "graphiti"
	if c.GraphitiURL == "" {
		return Component{Name: name, Status: StatusNotConfigured, Detail: "not configured"}
	}
	url := c.GraphitiURL + "/healthcheck"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		c.Logger.Info("health: building graphiti ping request failed", "url", url, "err", err.Error())
		return Component{Name: name, Status: StatusDegraded, Detail: "ping request error: " + err.Error()}
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		c.Logger.Info("health: graphiti ping failed", "url", url, "err", err.Error())
		return Component{Name: name, Status: StatusDegraded, Detail: "unreachable: " + err.Error()}
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return Component{Name: name, Status: StatusHealthy, Detail: fmt.Sprintf("HTTP %d", resp.StatusCode)}
	}
	return Component{Name: name, Status: StatusDegraded, Detail: fmt.Sprintf("HTTP %d", resp.StatusCode)}
}

// matchingPods returns the pods selected by sel. A nil/invalid selector matches
// nothing (so component status falls back to replica counts only).
func matchingPods(sel *metav1.LabelSelector, pods []corev1.Pod) []corev1.Pod {
	if sel == nil {
		return nil
	}
	selector, err := metav1.LabelSelectorAsSelector(sel)
	if err != nil {
		return nil
	}
	var out []corev1.Pod
	for _, p := range pods {
		if selector.Matches(labels.Set(p.Labels)) {
			out = append(out, p)
		}
	}
	return out
}

// podTrouble returns the first waiting-state reason (e.g. CrashLoopBackOff)
// among the pods and the max container restart count, so an all-replicas-up
// component that is actually flapping is caught.
func podTrouble(pods []corev1.Pod) (reason string, maxRestarts int32) {
	for _, p := range pods {
		for _, cs := range p.Status.ContainerStatuses {
			if cs.RestartCount > maxRestarts {
				maxRestarts = cs.RestartCount
			}
			if reason == "" && cs.State.Waiting != nil && cs.State.Waiting.Reason != "" {
				reason = cs.State.Waiting.Reason
			}
		}
	}
	return reason, maxRestarts
}

// countReady counts pods whose PodReady condition is True.
func countReady(pods []corev1.Pod) int {
	n := 0
	for _, p := range pods {
		for _, cond := range p.Status.Conditions {
			if cond.Type == corev1.PodReady && cond.Status == corev1.ConditionTrue {
				n++
				break
			}
		}
	}
	return n
}

func desired(replicas *int32) int32 {
	if replicas == nil {
		return 1
	}
	return *replicas
}
