package portforward

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
)

func TestReadyPodAfterServiceRecovery(t *testing.T) {
	ready := corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{
		{Type: corev1.PodReady, Status: corev1.ConditionTrue},
	}}
	now := metav1.Now()
	pods := []corev1.Pod{
		{ObjectMeta: metav1.ObjectMeta{Name: "evicted"}, Status: corev1.PodStatus{Phase: corev1.PodFailed, Reason: "Evicted"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "completed"}, Status: corev1.PodStatus{Phase: corev1.PodSucceeded}},
		{ObjectMeta: metav1.ObjectMeta{Name: "terminating", DeletionTimestamp: &now}, Status: ready},
		{ObjectMeta: metav1.ObjectMeta{Name: "starting"}, Status: corev1.PodStatus{Phase: corev1.PodRunning}},
		{ObjectMeta: metav1.ObjectMeta{Name: "serving"}, Status: ready},
	}
	got := readyPod(pods)
	require.NotNil(t, got)
	assert.Equal(t, "serving", got.Name)
	assert.Nil(t, readyPod(pods[:4]), "unavailable replicas must not receive forwards")
	assert.Nil(t, readyPod(nil))
}

func TestPortForwarderConstruction(t *testing.T) {
	cases := []struct {
		name       string
		cfg        *rest.Config
		namespace  string
		service    string
		selector   string
		targetPort uint16
		localPort  uint16
		wantErr    bool
		wantLocal  uint16
	}{
		{
			name:       "ok / operator forward, OS-assigned local",
			cfg:        &rest.Config{Host: "https://example.invalid"},
			namespace:  "agentprimitives-system",
			service:    "spicebox-operator",
			selector:   "app.kubernetes.io/name=spicebox-operator",
			targetPort: 8082,
			localPort:  0,
			wantLocal:  0,
		},
		{
			name:       "ok / spicedb forward, fixed local",
			cfg:        &rest.Config{Host: "https://example.invalid"},
			namespace:  "agentprimitives-system",
			service:    "spicebox-spicedb",
			selector:   "app.kubernetes.io/name=spicebox-spicedb",
			targetPort: 50051,
			localPort:  60061,
			wantLocal:  60061,
		},
		{
			name:       "nil cfg: returns error",
			cfg:        nil,
			namespace:  "x",
			service:    "y",
			selector:   "z",
			targetPort: 1,
			wantErr:    true,
		},
		{
			name:       "empty namespace: returns error",
			cfg:        &rest.Config{},
			namespace:  "",
			service:    "y",
			selector:   "z",
			targetPort: 1,
			wantErr:    true,
		},
		{
			name:       "empty service: returns error",
			cfg:        &rest.Config{},
			namespace:  "x",
			service:    "",
			selector:   "z",
			targetPort: 1,
			wantErr:    true,
		},
		{
			name:       "empty selector: returns error",
			cfg:        &rest.Config{},
			namespace:  "x",
			service:    "y",
			selector:   "",
			targetPort: 1,
			wantErr:    true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pf, err := New(tc.cfg, tc.namespace, tc.service, tc.selector, tc.targetPort, tc.localPort)
			if tc.wantErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.NotNil(t, pf)
			assert.Equal(t, tc.selector, pf.selector, "selector round-trips through New")
			assert.Equal(t, tc.wantLocal, pf.requestedPort, "requested local port round-trips through New")
		})
	}
}
