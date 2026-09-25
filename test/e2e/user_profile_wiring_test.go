//go:build e2e

package e2e_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	apiruntime "k8s.io/apimachinery/pkg/runtime"
	clientfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/untrusted"
	"github.com/authzed/openagentprimitives/test/e2e"
)

// TestUserProfileWiring_PromptAndFetcherNeverDiverge locks the wiring
// invariant userprofilegate.Offer's godoc names: internal/cmd/runner/main.go and this
// in-process factory both derive TWO consumers — ComposeSystem's
// untrusted-profile prompt paragraph, and the Loop's FetchSpeakerProfile /
// SpeakerProfileFields fields — from the SAME Offer call, "one call, two
// consumers" specifically so they cannot drift apart.
//
// Every existing test covers exactly one side in isolation:
// pkg/agent/runner/prompt_test.go passes the bool to ComposeSystem directly;
// pkg/agent/runner/speakerprofile_test.go sets FetchSpeakerProfile on a
// hand-built Loop directly. Neither exercises the wiring in
// buildLoop/internal/cmd/runner/main.go that derives both from one decision, so a
// future edit that gated one consumer on an extra condition the other lacks
// (e.g. "&& channelAttached" added to only one call site) would pass the
// entire suite. This test drives the REAL InProcessRunnerFactory construction
// path (BuildLoopUserProfileWiringForTest -> buildLoop, the same path
// internal/cmd/runner/main.go's Task 8 wiring mirrors) and inspects both outputs of
// the one Offer call together.
//
// The two failure directions are not symmetric:
//   - paragraph without blocks wastes context (a confusing instruction about
//     a marker the agent never receives).
//   - blocks without paragraph is the serious one — the agent would receive
//     attacker-influenceable, self-reported profile text (job titles, status
//     lines) with NO instruction that it is untrusted, self-reported, or
//     grants no permission.
//
// Both directions are asserted below.
func TestUserProfileWiring_PromptAndFetcherNeverDiverge(t *testing.T) {
	scheme := apiruntime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme), "corev1.AddToScheme")
	require.NoError(t, spiceboxv1alpha1.AddToScheme(scheme), "spiceboxv1alpha1.AddToScheme")

	// A slack-bound Channel + credentials Secret. slack is the only channel
	// kind that implements channelkinds.UserProfileProvider today
	// (pkg/channels/channelkinds/slack/userprofile.go), so it is the only kind that can
	// ever drive the "active" side of Offer's decision. The SAME fake Channel +
	// Secret pair is reused for every case below — only the AgentClass's
	// capability grant differs between cases, isolating the one variable that
	// is supposed to control both consumers.
	channel := &spiceboxv1alpha1.Channel{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-channel", Namespace: "default"},
		Spec: spiceboxv1alpha1.ChannelSpec{
			Kind:           "slack",
			AgentClass:     "demo-agent",
			CredentialsRef: spiceboxv1alpha1.ChannelCredentialsRef{SecretName: "demo-channel-secret"},
		},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-channel-secret", Namespace: "default"},
		Data: map[string][]byte{
			"bot-token": []byte("xoxb-fake"),
			"app-token": []byte("xapp-fake"),
		},
	}
	fakeCli := clientfake.NewClientBuilder().WithScheme(scheme).WithObjects(channel, secret).Build()

	scripted := e2e.NewScriptedLLM(t)
	f := &e2e.InProcessRunnerFactory{LLM: scripted, K8s: fakeCli}

	baseClass := func() *spiceboxv1alpha1.AgentClass {
		return &spiceboxv1alpha1.AgentClass{
			ObjectMeta: metav1.ObjectMeta{Name: "demo-agent", Namespace: "default"},
			Spec: spiceboxv1alpha1.AgentClassSpec{
				Model: &spiceboxv1alpha1.ModelConfig{
					Provider: "test", Name: "scripted",
					APIKey: spiceboxv1alpha1.SecretKeyRef{Name: "placeholder", Key: "api-key"},
				},
				SystemPrompt: spiceboxv1alpha1.PromptSource{Inline: "you are a test agent"},
			},
		}
	}
	newSession := func() *spiceboxv1alpha1.AgentSession {
		return &spiceboxv1alpha1.AgentSession{
			ObjectMeta: metav1.ObjectMeta{Name: "demo-session", Namespace: "default"},
			Spec: spiceboxv1alpha1.AgentSessionSpec{
				Class:  "demo-agent",
				Prompt: spiceboxv1alpha1.PromptSource{Inline: "hello"},
				InputChannel: &spiceboxv1alpha1.ChannelBinding{
					Name: "demo-channel",
					Kind: "slack",
					Key:  "dm:U-demo",
				},
			},
		}
	}

	cases := []struct {
		name       string
		grant      bool // whether the AgentClass grants the user_profile capability
		wantActive bool
	}{
		{
			name:       "user_profile granted on a slack-bound session: prompt paragraph AND fetcher wired together",
			grant:      true,
			wantActive: true,
		},
		{
			name:       "user_profile NOT granted on the same slack-bound session: no prompt mention and no fetcher",
			grant:      false,
			wantActive: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			class := baseClass()
			if tc.grant {
				class.Spec.Capabilities = map[string]apiextensionsv1.JSON{
					"user_profile": {Raw: []byte(`{}`)},
				}
			}
			sess := newSession()

			system, fetchWired, fields, err := f.BuildLoopUserProfileWiringForTest(sess, class)
			require.NoError(t, err, "buildLoop must succeed for a slack-bound session")

			if tc.wantActive {
				assert.Contains(t, system, untrusted.ProfileTag,
					"active capability must emit the untrusted-profile prompt paragraph")
				assert.True(t, fetchWired,
					"active capability must wire a non-nil FetchSpeakerProfile")
				assert.NotEmpty(t, fields,
					"active capability must wire a non-empty SpeakerProfileFields")
				return
			}
			assert.NotContains(t, system, untrusted.ProfileTag,
				"declined capability must NOT mention the untrusted-profile tag")
			assert.NotContains(t, system, "self-reported",
				"declined capability must NOT leak the self-reported profile guidance")
			assert.False(t, fetchWired,
				"declined capability must leave FetchSpeakerProfile nil")
			assert.Empty(t, fields,
				"declined capability must leave SpeakerProfileFields empty")
		})
	}
}
