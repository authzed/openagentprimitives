package historyresp

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

// buildScheme registers the schemes the fake client needs.
func buildScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, spiceboxv1alpha1.AddToScheme(s))
	return s
}

func TestHandle_DerivesChannelKeyFromSessionAndDefaultsCursor(t *testing.T) {
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:   "default",
			Name:        "sess-1",
			Annotations: map[string]string{spiceboxv1alpha1.AnnotationBackfilledFromTS: "5.0"},
		},
		Spec: spiceboxv1alpha1.AgentSessionSpec{
			InputChannel: &spiceboxv1alpha1.ChannelBinding{
				Name: "ch", Kind: "slack", Key: "thread:C1:9.9",
			},
		},
	}
	ch := &spiceboxv1alpha1.Channel{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "ch"}}
	cli := fake.NewClientBuilder().WithScheme(buildScheme(t)).WithObjects(sess, ch).Build()

	var gotKey string
	var gotOpts channelkinds.ReadHistoryOpts
	r := &Responder{
		K8s: cli,
		ReadHistory: func(_ context.Context, _ *spiceboxv1alpha1.Channel, key string, opts channelkinds.ReadHistoryOpts) (channelkinds.HistoryPage, error) {
			gotKey, gotOpts = key, opts
			return channelkinds.HistoryPage{
				HasMore:  true,
				Messages: []channelkinds.HistoryMessage{{AuthorDisplayName: "Alice", Text: "old", TS: "4.0"}},
			}, nil
		},
	}

	resp := r.handle(context.Background(), "default", "sess-1", channelevents.HistoryRequest{Limit: 20})

	assert.Empty(t, resp.Error)
	assert.Equal(t, "thread:C1:9.9", gotKey, "channelKey derived from the session, not caller input")
	assert.Equal(t, "5.0", gotOpts.BeforeTS, "empty cursor defaults to BackfilledFromTS")
	assert.Equal(t, 20, gotOpts.Limit)
	require.Len(t, resp.Messages, 1)
	assert.Equal(t, "Alice", resp.Messages[0].AuthorDisplayName)
	assert.Equal(t, "4.0", resp.OldestCursor)
	assert.True(t, resp.HasMore)
}

func TestHandle_ExplicitCursorOverridesDefault(t *testing.T) {
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "sess-1",
			Annotations: map[string]string{spiceboxv1alpha1.AnnotationBackfilledFromTS: "5.0"}},
		Spec: spiceboxv1alpha1.AgentSessionSpec{
			InputChannel: &spiceboxv1alpha1.ChannelBinding{Name: "ch", Key: "thread:C1:9.9"},
		},
	}
	ch := &spiceboxv1alpha1.Channel{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "ch"}}
	cli := fake.NewClientBuilder().WithScheme(buildScheme(t)).WithObjects(sess, ch).Build()

	var gotOpts channelkinds.ReadHistoryOpts
	r := &Responder{K8s: cli, ReadHistory: func(_ context.Context, _ *spiceboxv1alpha1.Channel, _ string, opts channelkinds.ReadHistoryOpts) (channelkinds.HistoryPage, error) {
		gotOpts = opts
		return channelkinds.HistoryPage{}, nil
	}}
	resp := r.handle(context.Background(), "default", "sess-1", channelevents.HistoryRequest{BeforeCursor: "3.0"})
	assert.Empty(t, resp.Error)
	assert.Equal(t, "3.0", gotOpts.BeforeTS)
}

func TestHandle_MissingSessionReturnsError(t *testing.T) {
	cli := fake.NewClientBuilder().WithScheme(buildScheme(t)).Build()
	r := &Responder{K8s: cli, ReadHistory: func(context.Context, *spiceboxv1alpha1.Channel, string, channelkinds.ReadHistoryOpts) (channelkinds.HistoryPage, error) {
		return channelkinds.HistoryPage{}, nil
	}}
	resp := r.handle(context.Background(), "default", "nope", channelevents.HistoryRequest{})
	assert.NotEmpty(t, resp.Error)
}
