package installcmd

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/cli/tui"
)

func TestMonitoringScreen_KeepDefaultWhenExisting(t *testing.T) {
	existing := []spiceboxv1alpha1.Channel{{ObjectMeta: metav1.ObjectMeta{Name: "demo-mon"}, Spec: spiceboxv1alpha1.ChannelSpec{Kind: "slack"}}}
	st := tui.NewState()
	st.Set(keyMonitoring, monitoringKeepValue) // accept-all seed
	g, err := newMonitoringScreen(existing, DetectedSettings{MonitoringChannel: "demo-mon"}).Prepare(context.Background(), st)
	require.NoError(t, err)
	assert.Nil(t, g)

	g2, err := newMonitoringScreen(existing, DetectedSettings{MonitoringChannel: "demo-mon"}).Prepare(context.Background(), tui.NewState())
	require.NoError(t, err)
	assert.NotNil(t, g2)
}
