package meta_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/meta"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/memory"
)

// unsortedWarnings is the shape a render controller running an OLD build would
// have written onto the CR: the right set, in whatever order a map walk gave.
var unsortedWarnings = []spiceboxv1alpha1.SanitizerWarning{
	{Kind: "tag", Name: "title", Action: "removed", Count: 1},
	{Kind: "attr", Name: "lang", Action: "stripped", Count: 1},
	{Kind: "tag", Name: "meta", Action: "unwrapped", Count: 1, Note: "content kept"},
	{Kind: "attr", Name: "charset", Action: "stripped", Count: 1},
}

// readyWithWarnings flips a created ArtifactRender to Ready carrying ws, so the
// tool composes a real result from a status it did not choose the order of.
func readyWithWarnings(uid types.UID, ws []spiceboxv1alpha1.SanitizerWarning) interceptor.Funcs {
	return interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if ar, ok := obj.(*spiceboxv1alpha1.ArtifactRender); ok {
				ar.UID = uid
				ar.Status.Phase = spiceboxv1alpha1.ArtifactRenderPhaseReady
				ar.Status.OutputRef = "mem://out"
				ar.Status.OutputMIME = "text/html"
				ar.Status.OutputSize = 10
				ar.Status.OutputFilename = "out.html"
				ar.Status.Warnings = ws
			}
			return c.Create(ctx, obj, opts...)
		},
	}
}

// TestArtifactPrepare_ResultWarningsAreCanonical pins that the ENCODER, not its
// caller, decides the order the model sees.
//
// The renderer sorts too, so for a fresh render this is belt and braces. It
// matters because meta.CanonicalizeResult rests on the encoder being the
// definition of canonical bytes: a capture re-expresses a result recorded by an
// older build by running it back through here, and that is only sound if this
// sorts whatever it is handed.
func TestArtifactPrepare_ResultWarningsAreCanonical(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(newPrepareArtifactScheme(t)).
		WithInterceptorFuncs(readyWithWarnings("uid-w", unsortedWarnings)).Build()
	tl := meta.NewArtifactPrepare(meta.ArtifactPrepareConfig{
		Client: c, Artifacts: newPrepareSvc(), AvailableKinds: []string{"html"},
		MaxInputBytes: 1 << 20, PollInterval: time.Millisecond,
	})

	args, _ := json.Marshal(map[string]any{"kind": "html", "payload": "<h1>hi</h1>", "name": "report"})
	res, err := tl.Execute(memory.WithSystemApproval(context.Background(), "test"), args, &tool.SessionContext{Namespace: "default", Name: "sess1"})
	require.NoError(t, err)
	require.False(t, res.IsError, "want success, got: %s", res.Content)

	var got struct {
		Warnings []spiceboxv1alpha1.SanitizerWarning `json:"warnings"`
	}
	require.NoError(t, json.Unmarshal([]byte(res.Content), &got))
	require.Len(t, got.Warnings, 4, "no warning may be dropped by the ordering")

	names := make([]string, 0, 4)
	for _, w := range got.Warnings {
		names = append(names, w.Kind+"/"+w.Name)
	}
	assert.Equal(t, []string{"attr/charset", "attr/lang", "tag/meta", "tag/title"}, names,
		"the encoder emits channelassets' canonical order whatever order it was handed")

	for _, w := range got.Warnings {
		if w.Name == "meta" {
			assert.Equal(t, "content kept", w.Note, "a warning's note travels with it, not with its old index")
		}
	}
}
