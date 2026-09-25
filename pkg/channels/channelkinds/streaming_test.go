package channelkinds

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

func TestShowsAssistantStream(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, spiceboxv1alpha1.AddToScheme(scheme))

	class := func(name string, show, hasChannels bool) *spiceboxv1alpha1.AgentClass {
		c := &spiceboxv1alpha1.AgentClass{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: name}}
		if hasChannels {
			c.Spec.Channels = &spiceboxv1alpha1.ChannelsConfig{ShowAssistantStream: show}
		}
		return c
	}
	sess := func(className string) *spiceboxv1alpha1.AgentSession {
		s := &spiceboxv1alpha1.AgentSession{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "s"}}
		s.Spec.Class = className
		return s
	}
	cli := func(objs ...client.Object) client.Client {
		return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	}
	ctx := context.Background()

	assert.True(t, ShowsAssistantStream(ctx, cli(class("on", true, true)), sess("on")),
		"explicit showAssistantStream=true → stream enabled")
	assert.False(t, ShowsAssistantStream(ctx, cli(class("off", false, true)), sess("off")),
		"explicit false → disabled")
	assert.False(t, ShowsAssistantStream(ctx, cli(class("nochan", false, false)), sess("nochan")),
		"no channels config → fail-closed disabled")
	assert.False(t, ShowsAssistantStream(ctx, cli(), sess("missing")),
		"class not found → fail-closed disabled")
	assert.False(t, ShowsAssistantStream(ctx, cli(), sess("")), "empty class → disabled")
	assert.False(t, ShowsAssistantStream(ctx, cli(), nil), "nil session → disabled")
	assert.False(t, ShowsAssistantStream(ctx, nil, sess("x")), "nil client → disabled")
}
