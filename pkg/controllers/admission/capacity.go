package admission

import (
	"cmp"
	"context"
	"fmt"
	"slices"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/pkg/platform/schedfit"
)

// PlacedPod is a pod reserved on a specific node, so it can be freed later.
type PlacedPod struct {
	Node string
	PodDemand
}

// CapacityModel is a mutable view of what a cluster can still place.
type CapacityModel interface {
	// Known reports whether the model has real capacity data. An unknown model
	// admits everything: missing data must not block sessions.
	Known() bool
	// Reserve places every pod of d or none of them, committing only on
	// success.
	Reserve(d Demand) bool
	// Free returns placed pods' requests to their nodes.
	Free(pods []PlacedPod)
	// Clone returns an independent copy.
	Clone() CapacityModel
}

// CapacitySource produces a CapacityModel snapshot. It is the seam a future
// non-Kubernetes backend plugs into.
type CapacitySource interface {
	Snapshot(ctx context.Context) (CapacityModel, error)
}

// KubeCapacitySource snapshots node headroom from the Kubernetes API.
type KubeCapacitySource struct {
	Reader client.Reader
}

// Snapshot lists nodes and all pods and builds a node model. A list error is
// returned for the caller to treat as unknown capacity.
func (s KubeCapacitySource) Snapshot(ctx context.Context) (CapacityModel, error) {
	var nodes corev1.NodeList
	if err := s.Reader.List(ctx, &nodes); err != nil {
		return nil, fmt.Errorf("list nodes: %w", err)
	}
	var pods corev1.PodList
	if err := s.Reader.List(ctx, &pods); err != nil {
		return nil, fmt.Errorf("list pods: %w", err)
	}
	return NewNodeModel(nodes.Items, pods.Items), nil
}

type nodeSlot struct {
	name                               string
	cpuMilli, memBytes, ephemeralBytes int64
	// pods is the remaining pod slots; unlimitedPods is set when the node
	// reports no `pods` allocatable (fake or unusual nodes), so it is not
	// read as zero.
	pods          int64
	unlimitedPods bool
}

func (n *nodeSlot) fits(p PodDemand) bool {
	return p.CPUMilli <= n.cpuMilli && p.MemBytes <= n.memBytes &&
		p.EphemeralBytes <= n.ephemeralBytes && (n.unlimitedPods || n.pods >= 1)
}

func (n *nodeSlot) take(p PodDemand) {
	n.cpuMilli -= p.CPUMilli
	n.memBytes -= p.MemBytes
	n.ephemeralBytes -= p.EphemeralBytes
	n.pods--
}

type nodeModel struct {
	known bool
	nodes []nodeSlot
}

// NewNodeModel builds a model from node headroom. Unschedulable nodes are
// skipped. With no nodes, or none reporting allocatable, the model is unknown.
func NewNodeModel(nodes []corev1.Node, pods []corev1.Pod) CapacityModel {
	m := &nodeModel{}
	if !schedfit.CeilingFromNodes(nodes).Known {
		return m
	}
	m.known = true
	for i := range nodes {
		if nodes[i].Spec.Unschedulable {
			continue
		}
		h := schedfit.HeadroomOn(nodes[i], pods)
		_, hasPods := nodes[i].Status.Allocatable[corev1.ResourcePods]
		m.nodes = append(m.nodes, nodeSlot{
			name:           nodes[i].Name,
			cpuMilli:       h.CPUMilli,
			memBytes:       h.MemBytes,
			ephemeralBytes: h.EphemeralBytes,
			pods:           h.Pods,
			unlimitedPods:  !hasPods,
		})
	}
	return m
}

func (m *nodeModel) Known() bool { return m.known }

// Reserve places pods first-fit-decreasing by cpu then memory, on a scratch
// copy so a failure leaves the model untouched.
func (m *nodeModel) Reserve(d Demand) bool {
	if !m.known {
		return true
	}
	order := slices.Clone(d.Pods)
	slices.SortStableFunc(order, func(a, b PodDemand) int {
		return cmp.Or(cmp.Compare(b.CPUMilli, a.CPUMilli), cmp.Compare(b.MemBytes, a.MemBytes))
	})
	scratch := slices.Clone(m.nodes)
	for _, p := range order {
		placed := false
		for i := range scratch {
			if scratch[i].fits(p) {
				scratch[i].take(p)
				placed = true
				break
			}
		}
		if !placed {
			return false
		}
	}
	m.nodes = scratch
	return true
}

func (m *nodeModel) Free(pods []PlacedPod) {
	if !m.known {
		return
	}
	for _, p := range pods {
		for i := range m.nodes {
			if m.nodes[i].name != p.Node {
				continue
			}
			m.nodes[i].cpuMilli += p.CPUMilli
			m.nodes[i].memBytes += p.MemBytes
			m.nodes[i].ephemeralBytes += p.EphemeralBytes
			m.nodes[i].pods++
		}
	}
}

func (m *nodeModel) Clone() CapacityModel {
	return &nodeModel{known: m.known, nodes: slices.Clone(m.nodes)}
}
