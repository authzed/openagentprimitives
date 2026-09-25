// Package schedfit answers one question about a cluster: can it EVER schedule
// a pod that requests this much?
//
// It is deliberately pure — no clients, no context, no logging — so that the
// install-time preflight (cmd/oap, reached through cloud.Strategy) and the
// operator's runtime fast-fail (pkg/controllers/agentsession) share one
// predicate instead of each carrying its own copy of the capacity math.
package schedfit

import (
	"fmt"

	"github.com/dustin/go-humanize"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// Ceiling is the largest single pod a cluster can ever schedule.
//
// Known is false when the ceiling is not provable: an elastic cluster whose
// autoscaler can add a bigger node than any present today, or a cluster whose
// nodes could not be read. Callers MUST skip their check in that case rather
// than guess — a false "unschedulable" aborts a perfectly good install, while a
// missed one only falls through to the runtime fast-fail that already exists.
type Ceiling struct {
	Known          bool
	CPUMilli       int64
	MemBytes       int64
	EphemeralBytes int64

	// Source explains the number, or explains why Known is false. Callers
	// surface it verbatim, so it must read as a complete clause:
	// "largest node oap-desktop (3.6 GiB memory allocatable)".
	Source string

	// Node is the node that set the MEMORY ceiling, or "" when Known is false.
	// It exists so a caller can compute Headroom on that same node and name it
	// in a message. It is never part of the exceeded decision.
	Node string
}

// Headroom is what would actually schedule on a node right now: its allocatable
// minus the requests already booked by the non-terminal pods assigned to it.
//
// Unlike Ceiling this is volatile — it moves as pods come and go. It is only
// ever used to SUGGEST a value to a human, never to decide something is
// impossible.
type Headroom struct {
	Known          bool
	CPUMilli       int64
	MemBytes       int64
	EphemeralBytes int64
}

// CeilingFromNodes computes the per-dimension maximum allocatable across nodes.
//
// Each dimension's maximum is taken INDEPENDENTLY. That is the fail-safe
// reading: the result answers "no single node can offer this much of dimension
// X", so anything Exceeds flags is unschedulable no matter which node it would
// land on. Taking the max over a whole node vector instead would be stricter and
// could flag a pod that some other node could actually take.
//
// Empty input, or nodes reporting no allocatable at all, yields Known false.
func CeilingFromNodes(nodes []corev1.Node) Ceiling {
	var c Ceiling
	for i := range nodes {
		alloc := nodes[i].Status.Allocatable
		if v := milli(alloc, corev1.ResourceCPU); v > c.CPUMilli {
			c.CPUMilli = v
		}
		if v := value(alloc, corev1.ResourceEphemeralStorage); v > c.EphemeralBytes {
			c.EphemeralBytes = v
		}
		if v := value(alloc, corev1.ResourceMemory); v > c.MemBytes {
			c.MemBytes = v
			c.Node = nodes[i].Name
		}
	}
	if c.CPUMilli == 0 && c.MemBytes == 0 && c.EphemeralBytes == 0 {
		return Ceiling{Source: "no node reported allocatable capacity"}
	}
	c.Known = true
	if c.Node != "" {
		c.Source = fmt.Sprintf("largest node %s (%s memory allocatable)", c.Node, HumanBytes(c.MemBytes))
	} else {
		c.Source = "the largest node in this cluster"
	}
	return c
}

// HeadroomOn returns what would still schedule on node right now: its
// allocatable minus the effective requests of every non-terminal pod assigned
// to it. pods may include pods on other nodes; those are ignored, so a caller
// can pass an unfiltered list (the fake clientset ignores field selectors,
// which is exactly the case this guards).
//
// Dimensions floor at zero: an over-committed node has no headroom, not
// negative headroom.
func HeadroomOn(node corev1.Node, pods []corev1.Pod) Headroom {
	alloc := node.Status.Allocatable
	h := Headroom{
		Known:          true,
		CPUMilli:       milli(alloc, corev1.ResourceCPU),
		MemBytes:       value(alloc, corev1.ResourceMemory),
		EphemeralBytes: value(alloc, corev1.ResourceEphemeralStorage),
	}
	for i := range pods {
		p := &pods[i]
		if p.Spec.NodeName != node.Name {
			continue
		}
		if p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
			continue
		}
		cpu, mem, eph := effectiveRequests(p)
		h.CPUMilli -= cpu
		h.MemBytes -= mem
		h.EphemeralBytes -= eph
	}
	h.CPUMilli = max(h.CPUMilli, 0)
	h.MemBytes = max(h.MemBytes, 0)
	h.EphemeralBytes = max(h.EphemeralBytes, 0)
	return h
}

// effectiveRequests implements the Kubernetes effective pod request:
// max(largest init-container request, sum of container requests), per
// dimension. Init containers run to completion before the containers start, so
// the scheduler reserves whichever is larger — not their sum.
//
// Native sidecars are the exception: an init container with restartPolicy
// Always keeps running alongside the app containers for the pod's lifetime, so
// it ADDS to the sum like an app container instead of merely setting a floor.
func effectiveRequests(p *corev1.Pod) (cpuMilli, memBytes, ephBytes int64) {
	for i := range p.Spec.Containers {
		req := p.Spec.Containers[i].Resources.Requests
		cpuMilli += milli(req, corev1.ResourceCPU)
		memBytes += value(req, corev1.ResourceMemory)
		ephBytes += value(req, corev1.ResourceEphemeralStorage)
	}
	// Sidecars first: they belong IN the sum that the init-container floor below
	// is then measured against, not compared to it.
	for i := range p.Spec.InitContainers {
		if !isSidecar(&p.Spec.InitContainers[i]) {
			continue
		}
		req := p.Spec.InitContainers[i].Resources.Requests
		cpuMilli += milli(req, corev1.ResourceCPU)
		memBytes += value(req, corev1.ResourceMemory)
		ephBytes += value(req, corev1.ResourceEphemeralStorage)
	}
	for i := range p.Spec.InitContainers {
		if isSidecar(&p.Spec.InitContainers[i]) {
			continue
		}
		req := p.Spec.InitContainers[i].Resources.Requests
		cpuMilli = max(cpuMilli, milli(req, corev1.ResourceCPU))
		memBytes = max(memBytes, value(req, corev1.ResourceMemory))
		ephBytes = max(ephBytes, value(req, corev1.ResourceEphemeralStorage))
	}
	return cpuMilli, memBytes, ephBytes
}

// isSidecar reports whether an init container is a native sidecar — restartPolicy
// Always, meaning it runs for the pod's whole lifetime rather than completing
// before the app containers start.
func isSidecar(c *corev1.Container) bool {
	return c.RestartPolicy != nil && *c.RestartPolicy == corev1.ContainerRestartPolicyAlways
}

// ExceedsPod reports whether a POD can never be scheduled on this cluster.
//
// Prefer this over calling Exceeds per container. Kubernetes places a pod
// atomically, so what has to fit is the pod's effective request — see
// effectiveRequests — and checking containers one at a time under-detects: three
// 2Gi containers each clear a 4Gi ceiling while the pod they belong to needs 6Gi
// and can never be placed. Sharing effectiveRequests with HeadroomOn keeps the
// "what does this pod actually cost" rule in exactly one place.
func ExceedsPod(label string, pod *corev1.Pod, c Ceiling) (string, bool) {
	if pod == nil || !c.Known {
		return "", false
	}
	cpuMilli, memBytes, ephBytes := effectiveRequests(pod)
	req := corev1.ResourceList{}
	if cpuMilli > 0 {
		req[corev1.ResourceCPU] = *resource.NewMilliQuantity(cpuMilli, resource.DecimalSI)
	}
	if memBytes > 0 {
		req[corev1.ResourceMemory] = *resource.NewQuantity(memBytes, resource.BinarySI)
	}
	if ephBytes > 0 {
		req[corev1.ResourceEphemeralStorage] = *resource.NewQuantity(ephBytes, resource.BinarySI)
	}
	return Exceeds(label, req, c)
}

// Exceeds reports whether req asks for more of any dimension than c can ever
// offer, with a human-readable reason when it does. label names the thing being
// checked — a container name at runtime, a SpiceboxClass name at install time —
// and appears in that reason.
//
// It is FAIL-SAFE in two load-bearing ways: a Ceiling with Known=false never
// exceeds (missing data must not block anything), and a zero ceiling dimension
// is skipped rather than read as "this node offers none of it".
func Exceeds(label string, req corev1.ResourceList, c Ceiling) (string, bool) {
	if !c.Known {
		return "", false
	}
	if c.CPUMilli > 0 {
		if q, ok := req[corev1.ResourceCPU]; ok && q.MilliValue() > c.CPUMilli {
			return fmt.Sprintf("%s requests cpu=%s but the largest node has only %dm allocatable",
				label, q.String(), c.CPUMilli), true
		}
	}
	if c.MemBytes > 0 {
		if q, ok := req[corev1.ResourceMemory]; ok && q.Value() > c.MemBytes {
			return fmt.Sprintf("%s requests memory=%s but the largest node has only %s allocatable",
				label, q.String(), HumanBytes(c.MemBytes)), true
		}
	}
	if c.EphemeralBytes > 0 {
		if q, ok := req[corev1.ResourceEphemeralStorage]; ok && q.Value() > c.EphemeralBytes {
			return fmt.Sprintf("%s requests ephemeral-storage=%s but the largest node has only %s allocatable",
				label, q.String(), HumanBytes(c.EphemeralBytes)), true
		}
	}
	return "", false
}

// HumanBytes renders a byte count for a human ("3.6 GiB"). Kubernetes'
// Quantity.String() is exact but prints raw digits for the non-round numbers
// node allocatable always produces, which reads badly in a prompt.
func HumanBytes(n int64) string {
	if n <= 0 {
		return "0 B"
	}
	return humanize.IBytes(uint64(n))
}

func milli(rl corev1.ResourceList, name corev1.ResourceName) int64 {
	if q, ok := rl[name]; ok {
		return q.MilliValue()
	}
	return 0
}

func value(rl corev1.ResourceList, name corev1.ResourceName) int64 {
	if q, ok := rl[name]; ok {
		return q.Value()
	}
	return 0
}
