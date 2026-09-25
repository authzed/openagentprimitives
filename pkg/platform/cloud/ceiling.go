package cloud

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/authzed/openagentprimitives/pkg/platform/schedfit"
)

// NodeSchedulingCeiling answers Strategy.SchedulingCeiling for a cluster whose
// node set is FIXED — kind, the desktop k3s VM, a fixed bare-metal pool — where
// the largest node present IS the ceiling and a class asking for more memory
// than it has goes Pending forever with no autoscaler to rescue it.
//
// The autoscaling managed clouds must NOT use this: today's largest node does
// not bound what can eventually schedule there, and a false "unschedulable"
// would abort a valid install. They return an unknown ceiling with a Source
// explaining the skip.
//
// Neither read is fatal. Nodes that cannot be listed mean no PROVABLE ceiling,
// so this reports unknown rather than aborting an install over a missing RBAC
// verb — the operator's runtime fast-fail still covers a pod that cannot land.
// A pod-list failure costs only headroom precision, which the caller warns about.
func NodeSchedulingCeiling(ctx context.Context, cl Clients) (schedfit.Ceiling, schedfit.Headroom, error) {
	nodes, err := cl.Typed.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return schedfit.Ceiling{
			Source: fmt.Sprintf("could not list nodes (%v)", err),
		}, schedfit.Headroom{}, nil
	}

	ceiling := schedfit.CeilingFromNodes(nodes.Items)
	if !ceiling.Known || ceiling.Node == "" {
		return ceiling, schedfit.Headroom{}, nil
	}

	var ceilingNode corev1.Node
	for i := range nodes.Items {
		if nodes.Items[i].Name == ceiling.Node {
			ceilingNode = nodes.Items[i]
			break
		}
	}

	pods, err := cl.Typed.CoreV1().Pods("").List(ctx, metav1.ListOptions{
		FieldSelector: "spec.nodeName=" + ceiling.Node,
	})
	if err != nil {
		return ceiling, schedfit.Headroom{}, nil
	}
	return ceiling, schedfit.HeadroomOn(ceilingNode, pods.Items), nil
}
