package wait

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func crashedStatus(name, msg string, exit int32) corev1.ContainerStatus {
	return corev1.ContainerStatus{
		Name:  name,
		State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
		LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
			ExitCode: exit, Reason: "Error", Message: msg,
		}},
	}
}

func podWith(init, app []corev1.ContainerStatus) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "pod-1", Namespace: diagNS},
		Status: corev1.PodStatus{
			InitContainerStatuses: init,
			ContainerStatuses:     app,
		},
	}
}

func TestTerminatedNote(t *testing.T) {
	cases := []struct {
		name  string
		pod   *corev1.Pod
		check func(t *testing.T, got *TerminatedNote)
	}{
		{
			name: "app container crashloop: reports its exit + termination message",
			pod:  podWith(nil, []corev1.ContainerStatus{crashedStatus("spicedb", "bootstrap data already exists", 1)}),
			check: func(t *testing.T, got *TerminatedNote) {
				require.NotNil(t, got)
				assert.Equal(t, "spicedb", got.Container)
				assert.Equal(t, int32(1), got.ExitCode)
				assert.Equal(t, "bootstrap data already exists", got.Message)
				assert.Empty(t, got.Logs, "no log fetch needed when a termination message exists")
			},
		},
		{
			// An init container that keeps failing is why the app container never
			// starts; reporting the app container would name the symptom.
			name: "failing init container wins over a failing app container",
			pod: podWith(
				[]corev1.ContainerStatus{crashedStatus("migrate", "migration failed", 1)},
				[]corev1.ContainerStatus{crashedStatus("spicedb", "could not connect", 1)},
			),
			check: func(t *testing.T, got *TerminatedNote) {
				require.NotNil(t, got)
				assert.Equal(t, "migrate", got.Container)
				assert.Equal(t, "migration failed", got.Message)
			},
		},
		{
			name: "no termination message: falls back to the container's logs",
			pod:  podWith(nil, []corev1.ContainerStatus{crashedStatus("spicedb", "", 1)}),
			check: func(t *testing.T, got *TerminatedNote) {
				require.NotNil(t, got)
				assert.Empty(t, got.Message)
				assert.NotEmpty(t, got.Logs, "the exited container's log is the last resort and must be read")
			},
		},
		{
			name: "clean exit(0) is not a fault",
			pod:  podWith(nil, []corev1.ContainerStatus{crashedStatus("spicedb", "done", 0)}),
			check: func(t *testing.T, got *TerminatedNote) {
				assert.Nil(t, got)
			},
		},
		{
			name: "container that never terminated: nothing to report",
			pod: podWith(nil, []corev1.ContainerStatus{{
				Name:  "spicedb",
				State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerCreating"}},
			}}),
			check: func(t *testing.T, got *TerminatedNote) {
				assert.Nil(t, got)
			},
		},
		{
			name: "terminated right now (never restarted) is still reported",
			pod: podWith(nil, []corev1.ContainerStatus{{
				Name: "spicedb",
				State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
					ExitCode: 2, Reason: "Error", Message: "bad flag",
				}},
			}}),
			check: func(t *testing.T, got *TerminatedNote) {
				require.NotNil(t, got)
				assert.Equal(t, int32(2), got.ExitCode)
				assert.Equal(t, "bad flag", got.Message)
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.check(t, terminatedNote(context.Background(), fake.NewSimpleClientset(), diagNS, tc.pod))
		})
	}
}

func TestLastN(t *testing.T) {
	assert.Equal(t, []string{"b", "c"}, lastN([]string{"a", "b", "c"}, 2))
	assert.Equal(t, []string{"a"}, lastN([]string{"a"}, 5), "fewer items than the cap returns them all")
	assert.Nil(t, lastN(nil, 3))
}
