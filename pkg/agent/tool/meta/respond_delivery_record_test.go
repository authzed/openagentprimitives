package meta_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/authzed/openagentprimitives/pkg/agent/session/state"
	"github.com/authzed/openagentprimitives/pkg/agent/session/state/deliveries"
	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/meta"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/artifacts"
)

// TestRespond_RecordsDeliveryAfterThePublish pins the write side of the
// delivery record, and its ordering.
//
// It is what makes the artifact-delivered completion requirement mean anything:
// respond_to_user is the ONLY thing that carries an artifact to a user, so if
// it does not record, every delivered artifact reads as outstanding.
//
// The ordering is asserted from INSIDE the publish rather than by failing one:
// a record written before the envelope is on the wire would report that a user
// saw something they did not, and this catches that directly — a publish-error
// case would only catch it after the tool's ten-second publish retry budget.
func TestRespond_RecordsDeliveryAfterThePublish(t *testing.T) {
	const sessUID = types.UID("u-deliver")

	cr := &spiceboxv1alpha1.ArtifactRender{
		ObjectMeta: metav1.ObjectMeta{
			Name: "ar-deliver-1", Namespace: "default",
			OwnerReferences: []metav1.OwnerReference{{
				Kind: "AgentSession", Name: "sess-deliver", UID: sessUID,
			}},
		},
		Status: spiceboxv1alpha1.ArtifactRenderStatus{
			Phase: spiceboxv1alpha1.ArtifactRenderPhaseReady, OutputMIME: "text/html",
		},
	}
	scheme := runtime.NewScheme()
	require.NoError(t, spiceboxv1alpha1.AddToScheme(scheme), "AddToScheme")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cr).Build()

	sess := &tool.SessionContext{
		Namespace: "default", Name: "sess-deliver", AgentSessionUID: sessUID,
		State: state.NewRegistry(state.Deps{}),
	}
	store, ok := deliveries.TryFrom(sess)
	require.True(t, ok, "the deliveries state kind must be registered")

	var recordedAtPublishTime []string
	tl := meta.New(meta.RespondConfig{
		Capabilities: []string{"text", "asset:text/html"},
		ChannelKind:  "fake",
		Client:       c,
		NATSPublish: func(context.Context, string, []byte) error {
			recordedAtPublishTime = store.All()
			return nil
		},
	})

	args := json.RawMessage(`{"text":"full report attached below","attached":["ar-deliver-1"]}`)
	res, err := tl.Execute(context.Background(), args, sess)
	require.NoError(t, err)
	require.False(t, res.IsError, "content: %s", res.Content)

	assert.Empty(t, recordedAtPublishTime,
		"nothing may be marked delivered until the envelope has actually gone out")
	assert.Equal(t, []string{"ar-deliver-1"}, store.All(),
		"and once it has, the render is on the record the completion gate reads")
}

// TestRespond_TextOnlyReplyRecordsNoDelivery guards the exact defect: a reply
// that CLAIMS an attachment but passes none delivers nothing, and the record
// must say so rather than being fooled by the prose.
func TestRespond_TextOnlyReplyRecordsNoDelivery(t *testing.T) {
	tl := meta.New(meta.RespondConfig{
		Capabilities: []string{"text", "asset:text/html"},
		ChannelKind:  "fake",
		NATSPublish:  func(context.Context, string, []byte) error { return nil },
	})
	sess := &tool.SessionContext{
		Namespace: "default", Name: "sess-deliver",
		State: state.NewRegistry(state.Deps{}),
	}

	res, err := tl.Execute(context.Background(),
		json.RawMessage(`{"text":"…Full report attached below…"}`), sess)
	require.NoError(t, err)
	require.False(t, res.IsError, "a text-only reply is perfectly legal; content: %s", res.Content)

	store, ok := deliveries.TryFrom(sess)
	require.True(t, ok)
	assert.Empty(t, store.All(), "saying 'attached below' attaches nothing")
}

// TestRespond_RecordsTheArtifactIDNotOnlyTheRenderName spans a contract that no
// single component can see on its own.
//
// respond_to_user reads both halves off the same ArtifactRender CR, and
// conclude_trigger_status later asks the delivery record for the LOGICAL
// artifact id so it can compose the durable link a check run carries. Nothing
// fails if only the render name is recorded: the completion gate stays
// satisfied, every deliveries test stays green, and the only symptom is a check
// run that quietly stopped carrying a link.
func TestRespond_RecordsTheArtifactIDNotOnlyTheRenderName(t *testing.T) {
	const sessUID = types.UID("u-deliver-id")

	cr := &spiceboxv1alpha1.ArtifactRender{
		ObjectMeta: metav1.ObjectMeta{
			Name: "ar-report", Namespace: "default",
			// The label respond_to_user reads the logical artifact id from.
			Labels: map[string]string{artifacts.LabelArtifactID: "artifact-report"},
			OwnerReferences: []metav1.OwnerReference{{
				Kind: "AgentSession", Name: "sess-deliver-id", UID: sessUID,
			}},
		},
		Status: spiceboxv1alpha1.ArtifactRenderStatus{
			Phase: spiceboxv1alpha1.ArtifactRenderPhaseReady, OutputMIME: "text/html",
		},
	}
	scheme := runtime.NewScheme()
	require.NoError(t, spiceboxv1alpha1.AddToScheme(scheme), "AddToScheme")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cr).Build()

	sess := &tool.SessionContext{
		Namespace: "default", Name: "sess-deliver-id", AgentSessionUID: sessUID,
		State: state.NewRegistry(state.Deps{}),
	}
	tl := meta.New(meta.RespondConfig{
		Capabilities: []string{"text", "asset:text/html"},
		ChannelKind:  "fake",
		Client:       c,
		NATSPublish:  func(context.Context, string, []byte) error { return nil },
	})

	res, err := tl.Execute(context.Background(),
		json.RawMessage(`{"text":"report attached","attached":["ar-report"]}`), sess)
	require.NoError(t, err)
	require.False(t, res.IsError, "content: %s", res.Content)

	store, ok := deliveries.TryFrom(sess)
	require.True(t, ok)
	assert.Equal(t, []string{"ar-report"}, store.All(), "the render name, for the completion gate")
	assert.Equal(t, "artifact-report", store.LastArtifactID(),
		"and the logical artifact id, for the durable link a trigger's status surface carries")
}
