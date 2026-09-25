package wait

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/kube"
)

func TestForGatewayAddress(t *testing.T) {
	gw := &gatewayv1.Gateway{}
	gw.Namespace, gw.Name = "agentprimitives-system", "spicebox-webd"
	gw.Status.Addresses = []gatewayv1.GatewayStatusAddress{{Value: "203.0.113.10"}}
	c := fake.NewClientBuilder().WithScheme(kube.Scheme).WithObjects(gw).Build()

	addr, err := ForGatewayAddress(context.Background(), c, "agentprimitives-system", "spicebox-webd", 10*time.Millisecond, time.Second)
	require.NoError(t, err)
	assert.Equal(t, "203.0.113.10", addr)
}

func TestForGatewayAddress_Timeout(t *testing.T) {
	gw := &gatewayv1.Gateway{}
	gw.Namespace, gw.Name = "agentprimitives-system", "spicebox-webd" // no addresses
	c := fake.NewClientBuilder().WithScheme(kube.Scheme).WithObjects(gw).Build()

	_, err := ForGatewayAddress(context.Background(), c, "agentprimitives-system", "spicebox-webd", 10*time.Millisecond, 50*time.Millisecond)
	require.Error(t, err)
}

// gwWithConditions builds an address-less Gateway carrying the given status
// conditions, as the GKE managed-gateway controller would set them.
func gwWithConditions(conds ...metav1.Condition) *gatewayv1.Gateway {
	gw := &gatewayv1.Gateway{}
	gw.Namespace, gw.Name = "agentprimitives-system", "spicebox-webd"
	gw.Status.Conditions = conds
	return gw
}

func TestDiagnoseGatewayAddress(t *testing.T) {
	// The exact message GKE emits when a duplicate/zombie GCE insert wedges the
	// backend service so the URL map can never be programmed and no address is
	// assigned. This is the case the operator most needs surfaced.
	const wedgeMsg = "error cause: gceSync: generic::deadline_exceeded: Insert: context deadline exceeded"

	cases := []struct {
		name        string
		gw          *gatewayv1.Gateway
		wantEmpty   bool // no diagnosis at all (address present / race)
		wantWedged  bool // Headline flags the stuck-LB case
		wantCondTyp string
	}{
		{
			name: "address present: no diagnosis to report",
			gw: func() *gatewayv1.Gateway {
				gw := gwWithConditions()
				gw.Status.Addresses = []gatewayv1.GatewayStatusAddress{{Value: "203.0.113.10"}}
				return gw
			}(),
			wantEmpty: true,
		},
		{
			name: "still provisioning (Programmed=True, no address yet): not wedged",
			gw: gwWithConditions(
				metav1.Condition{Type: "Accepted", Status: metav1.ConditionTrue, Reason: "Accepted"},
				metav1.Condition{Type: "Programmed", Status: metav1.ConditionTrue, Reason: "Programmed"},
			),
			wantWedged: false,
		},
		{
			name: "wedged GCE program: Programmed=False/Invalid with gceSync deadline → flagged wedged",
			gw: gwWithConditions(
				metav1.Condition{Type: "Accepted", Status: metav1.ConditionTrue, Reason: "Accepted"},
				metav1.Condition{Type: "Programmed", Status: metav1.ConditionFalse, Reason: "Invalid", Message: wedgeMsg},
				metav1.Condition{Type: "networking.gke.io/GatewayHealthy", Status: metav1.ConditionFalse, Reason: "Error", Message: wedgeMsg},
			),
			wantWedged:  true,
			wantCondTyp: "Programmed",
		},
		{
			name: "wedged via RESOURCE_NOT_READY backend service → flagged wedged",
			gw: gwWithConditions(
				metav1.Condition{Type: "Programmed", Status: metav1.ConditionFalse, Reason: "Invalid",
					Message: "The resource 'projects/p/global/backendServices/x' is not ready (RESOURCE_NOT_READY)"},
			),
			wantWedged:  true,
			wantCondTyp: "Programmed",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := fake.NewClientBuilder().WithScheme(kube.Scheme).WithObjects(tc.gw).Build()
			diag, err := DiagnoseGatewayAddress(context.Background(), c, "agentprimitives-system", "spicebox-webd")
			require.NoError(t, err)

			if tc.wantEmpty {
				assert.Empty(t, diag.Headline)
				assert.Empty(t, diag.Conditions)
				return
			}
			if tc.wantWedged {
				assert.NotEmpty(t, diag.Headline, "wedged LB program must produce a headline")
				assert.Contains(t, diag.Headline, "wedged")
				found := false
				for _, cn := range diag.Conditions {
					if cn.Type == tc.wantCondTyp {
						found = true
					}
				}
				assert.True(t, found, "blocking condition %q must be surfaced", tc.wantCondTyp)
			} else {
				assert.Empty(t, diag.Headline, "non-wedged provisioning must not flag a wedge")
			}
		})
	}
}

func TestDiagnoseGatewayAddress_GetError(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(kube.Scheme).Build() // no gateway object
	_, err := DiagnoseGatewayAddress(context.Background(), c, "agentprimitives-system", "spicebox-webd")
	require.Error(t, err)
}
