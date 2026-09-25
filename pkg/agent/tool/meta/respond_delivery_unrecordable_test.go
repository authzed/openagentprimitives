package meta_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/go-logr/logr/funcr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/meta"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// TestRespond_SaysSoWhenTheDeliveryCannotBeRecorded covers the bypass path
// through the delivery record: the reply publishes, carries its attachment, and
// the session has nowhere to write the fact down.
//
// Nothing fails here and nothing should — the user HAS the artifact, and
// failing the call over bookkeeping would be worse than the consequence. The
// consequence is what makes silence unacceptable: the artifact-delivered
// completion requirement reads this record and nothing else, so the next
// agent_work_complete is refused over an artifact the user is already looking
// at. Diagnosed from the refusal alone, that reads as a bug in the gate.
//
// conclude_trigger_status's recordConcluded already logs the identical branch
// for the same reason. This is the same fact stated for the other store.
func TestRespond_SaysSoWhenTheDeliveryCannotBeRecorded(t *testing.T) {
	const sessUID = types.UID("u-no-state")

	cr := &spiceboxv1alpha1.ArtifactRender{
		ObjectMeta: metav1.ObjectMeta{
			Name: "ar-orphan", Namespace: "default",
			OwnerReferences: []metav1.OwnerReference{{
				Kind: "AgentSession", Name: "sess-no-state", UID: sessUID,
			}},
		},
		Status: spiceboxv1alpha1.ArtifactRenderStatus{
			Phase: spiceboxv1alpha1.ArtifactRenderPhaseReady, OutputMIME: "text/html",
		},
	}
	scheme := runtime.NewScheme()
	require.NoError(t, spiceboxv1alpha1.AddToScheme(scheme), "AddToScheme")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cr).Build()

	// State left nil: deliveries.TryFrom reports !ok, which is the branch under
	// test. A runner always wires the registry, so this stands in for the
	// wiring gap rather than for anything a user can do.
	sess := &tool.SessionContext{
		Namespace: "default", Name: "sess-no-state", AgentSessionUID: sessUID,
	}

	var logged []string
	ctx := ctrllog.IntoContext(context.Background(),
		funcr.New(func(_, args string) { logged = append(logged, args) }, funcr.Options{}))

	tl := meta.New(meta.RespondConfig{
		Capabilities: []string{"text", "asset:text/html"},
		ChannelKind:  "fake",
		Client:       c,
		NATSPublish:  func(context.Context, string, []byte) error { return nil },
	})

	res, err := tl.Execute(ctx,
		json.RawMessage(`{"text":"report attached","attached":["ar-orphan"]}`), sess)
	require.NoError(t, err)
	require.False(t, res.IsError,
		"the reply went out and the user has the artifact; bookkeeping must not fail the call. content: %s", res.Content)

	var line string
	for _, l := range logged {
		if strings.Contains(l, "artifact-delivery record") {
			line = l
			break
		}
	}
	require.NotEmpty(t, line,
		"an unrecordable delivery must be logged, not passed over: the visible symptom "+
			"arrives much later as agent_work_complete refusing over an artifact the user already has "+
			"(logged: %v)", logged)
	assert.Contains(t, line, "default/sess-no-state", "the line must name the session")
	assert.Contains(t, line, "ar-orphan", "and the renders whose delivery went unrecorded")
}
