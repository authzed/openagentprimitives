package local_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/authzed/openagentprimitives/pkg/platform/cloud"
	"github.com/authzed/openagentprimitives/pkg/platform/cloud/local"
)

const gib = int64(1024 * 1024 * 1024)

func node(name, cpu, mem string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: corev1.NodeStatus{Allocatable: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse(cpu),
			corev1.ResourceMemory: resource.MustParse(mem),
		}},
	}
}

func podOn(nodeName, name, cpu, mem string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "kube-system", Name: name},
		Spec: corev1.PodSpec{
			NodeName: nodeName,
			Containers: []corev1.Container{{
				Name: "main",
				Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse(cpu),
					corev1.ResourceMemory: resource.MustParse(mem),
				}},
			}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
}

func TestLocalSchedulingCeiling(t *testing.T) {
	t.Run("largest node sets the ceiling; its booked pods set the headroom", func(t *testing.T) {
		kc := k8sfake.NewSimpleClientset(
			node("oap-desktop", "4", "4Gi"),
			node("tiny", "1", "1Gi"),
			podOn("oap-desktop", "postgres", "100m", "256Mi"),
			podOn("oap-desktop", "neo4j", "100m", "512Mi"),
			podOn("tiny", "elsewhere", "500m", "512Mi"),
		)
		c, h, err := local.Strategy{}.SchedulingCeiling(context.Background(), cloud.Clients{Typed: kc})
		require.NoError(t, err)

		assert.True(t, c.Known)
		assert.Equal(t, 4*gib, c.MemBytes)
		assert.Equal(t, "oap-desktop", c.Node)
		assert.True(t, h.Known)
		assert.Equal(t, 4*gib-768*1024*1024, h.MemBytes, "only oap-desktop's own pods are subtracted")
	})

	t.Run("node list fails: Known=false with the reason, and no error (fail-safe)", func(t *testing.T) {
		kc := k8sfake.NewSimpleClientset()
		kc.PrependReactor("list", "nodes", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, errors.New("nodes is forbidden")
		})
		c, h, err := local.Strategy{}.SchedulingCeiling(context.Background(), cloud.Clients{Typed: kc})
		require.NoError(t, err, "an unreadable cluster must not fail the install")
		assert.False(t, c.Known)
		assert.Contains(t, c.Source, "forbidden")
		assert.False(t, h.Known)
	})

	t.Run("pod list fails: ceiling still known, headroom unknown", func(t *testing.T) {
		kc := k8sfake.NewSimpleClientset(node("oap-desktop", "4", "4Gi"))
		kc.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, errors.New("pods is forbidden")
		})
		c, h, err := local.Strategy{}.SchedulingCeiling(context.Background(), cloud.Clients{Typed: kc})
		require.NoError(t, err)
		assert.True(t, c.Known, "the ceiling does not depend on the pod list")
		assert.False(t, h.Known, "only the suggestion loses precision")
	})

	t.Run("no nodes: Known=false", func(t *testing.T) {
		kc := k8sfake.NewSimpleClientset()
		c, _, err := local.Strategy{}.SchedulingCeiling(context.Background(), cloud.Clients{Typed: kc})
		require.NoError(t, err)
		assert.False(t, c.Known)
	})
}
