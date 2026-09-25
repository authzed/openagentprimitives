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

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/meta"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// TestRespond_AttachedHandle drives the three handle-resolution scenarios
// for respond_to_user's `attached` field. Each case seeds the fake client
// with one ArtifactRender CR (varying status/owner) and asserts both the
// tool result and whether an envelope was published.
func TestRespond_AttachedHandle(t *testing.T) {
	const sessUID = types.UID("u1")

	cases := []struct {
		name           string
		cr             *spiceboxv1alpha1.ArtifactRender
		handle         string
		wantIsError    bool
		wantPublishes  int
		wantContainsIn string // substring expected in res.Content (when IsError) or published bytes (otherwise)
	}{
		{
			name: "ready handle owned by session: publishes envelope referencing handle",
			cr: &spiceboxv1alpha1.ArtifactRender{
				ObjectMeta: metav1.ObjectMeta{
					Name: "ar-handle-1", Namespace: "default",
					OwnerReferences: []metav1.OwnerReference{{
						Kind: "AgentSession", Name: "sess1", UID: sessUID,
					}},
				},
				Status: spiceboxv1alpha1.ArtifactRenderStatus{
					Phase:          spiceboxv1alpha1.ArtifactRenderPhaseReady,
					OutputMIME:     "text/html",
					OutputFilename: "x.html",
				},
			},
			handle:         "ar-handle-1",
			wantIsError:    false,
			wantPublishes:  1,
			wantContainsIn: "ar-handle-1",
		},
		{
			name: "pending handle: IsError naming the bad handle, no publish",
			cr: &spiceboxv1alpha1.ArtifactRender{
				ObjectMeta: metav1.ObjectMeta{
					Name: "ar-pending", Namespace: "default",
					OwnerReferences: []metav1.OwnerReference{{
						Kind: "AgentSession", Name: "sess1", UID: sessUID,
					}},
				},
				Status: spiceboxv1alpha1.ArtifactRenderStatus{Phase: spiceboxv1alpha1.ArtifactRenderPhasePending},
			},
			handle:         "ar-pending",
			wantIsError:    true,
			wantPublishes:  0,
			wantContainsIn: "ar-pending",
		},
		{
			name: "ready handle owned by a different session: IsError, no publish",
			cr: &spiceboxv1alpha1.ArtifactRender{
				ObjectMeta: metav1.ObjectMeta{
					Name: "ar-other", Namespace: "default",
					OwnerReferences: []metav1.OwnerReference{{
						Kind: "AgentSession", Name: "OTHER-SESS", UID: types.UID("u-other"),
					}},
				},
				Status: spiceboxv1alpha1.ArtifactRenderStatus{
					Phase:          spiceboxv1alpha1.ArtifactRenderPhaseReady,
					OutputMIME:     "text/html",
					OutputFilename: "x.html",
				},
			},
			handle:         "ar-other",
			wantIsError:    true,
			wantPublishes:  0,
			wantContainsIn: "ar-other",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			scheme := runtime.NewScheme()
			require.NoError(t, spiceboxv1alpha1.AddToScheme(scheme), "AddToScheme")
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(tc.cr).Build()

			var published []byte
			publishCalls := 0
			tl := meta.New(meta.RespondConfig{
				Capabilities: []string{"text", "asset:text/html"},
				ChannelKind:  "slack",
				Client:       c,
				NATSPublish: func(_ context.Context, _ string, payload []byte) error {
					publishCalls++
					published = append([]byte(nil), payload...)
					return nil
				},
				NATSSubjectPrefix: "ap.session.default.sess1",
			})

			args, err := json.Marshal(map[string]any{
				"text":     "Here you go",
				"attached": []string{tc.handle},
			})
			require.NoError(t, err, "marshal args")

			res, _ := tl.Execute(context.Background(), args, &tool.SessionContext{
				Namespace:       "default",
				Name:            "sess1",
				AgentSessionUID: sessUID,
			})

			assert.Equal(t, tc.wantIsError, res.IsError, "IsError mismatch; content=%s", res.Content)
			assert.Equal(t, tc.wantPublishes, publishCalls, "publish call count mismatch")
			if tc.wantIsError {
				assert.Contains(t, res.Content, tc.wantContainsIn, "error must name the bad handle")
			} else {
				assert.Contains(t, string(published), tc.wantContainsIn, "envelope must reference the handle")
			}
		})
	}
}
