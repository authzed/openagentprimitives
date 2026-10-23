package slack

import (
	"context"
	"errors"
	"strings"
	"testing"

	slackapi "github.com/slack-go/slack"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

type instructionsAuthority struct {
	channelkinds.AuthzReader
	allowed bool
	err     error
	subject identity.CanonicalUserID
}

type instructionsClient struct {
	listenerAPIClient
	views []slackapi.ModalViewRequest
}

func (c *instructionsClient) OpenViewContext(_ context.Context, _ string, view slackapi.ModalViewRequest) (*slackapi.ViewResponse, error) {
	c.views = append(c.views, view)
	return &slackapi.ViewResponse{}, nil
}

func (a *instructionsAuthority) CheckInteract(_ context.Context, ns, name string, subject identity.CanonicalUserID, consistent bool) (bool, error) {
	if ns != "default" || name != "async" || !consistent {
		return false, errors.New("wrong session check")
	}
	a.subject = subject
	return a.allowed, a.err
}

func TestInstructionsModal_PagesPreserveExactInertText(t *testing.T) {
	text := "  <!here> <https://example.com|click> `literal`\n" + strings.Repeat("🦊", 230000) + "\n  end  "
	var restored strings.Builder
	for page := 0; page < 3; page++ {
		view := instructionModal(text, "default/async", page)
		require.LessOrEqual(t, len(view.Blocks.BlockSet), 42)
		for _, block := range view.Blocks.BlockSet {
			if s, ok := block.(*slackapi.SectionBlock); ok {
				require.Equal(t, "plain_text", s.Text.Type)
				require.LessOrEqual(t, len([]rune(s.Text.Text)), 2800)
				restored.WriteString(s.Text.Text)
			}
		}
		require.True(t, hasActionBlockWithID(view.Blocks.BlockSet, sessionInstructionsActionID+"_next") || hasActionBlockWithID(view.Blocks.BlockSet, sessionInstructionsActionID+"_previous"))
		if page == 1 {
			require.True(t, hasActionBlockWithID(view.Blocks.BlockSet, sessionInstructionsActionID+"_next"))
			require.True(t, hasActionBlockWithID(view.Blocks.BlockSet, sessionInstructionsActionID+"_previous"))
		}
	}
	require.Equal(t, text, restored.String())
}

func TestInstructionsClick_AuthorizesTheClicker(t *testing.T) {
	for _, tc := range []struct {
		name    string
		allowed bool
		err     error
		missing bool
	}{
		{name: "allowed", allowed: true}, {name: "denied"}, {name: "check error", err: errors.New("unavailable")}, {name: "missing authority", missing: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := &instructionsClient{}
			auth := &instructionsAuthority{allowed: tc.allowed, err: tc.err}
			sess := &v1.AgentSession{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "async"}, Spec: v1.AgentSessionSpec{Prompt: v1.PromptSource{Inline: "  exact private instructions\n<@OTHER>  "}}}
			l := &slackListener{api: api, idents: NewIdentityCache(8), installedTeamID: "T_TEST", deps: channelkinds.Deps{K8sClient: fake.NewClientBuilder().WithScheme(settingsTestScheme(t)).WithObjects(sess).Build(), AuthzReader: auth}}
			if tc.missing {
				l.deps.AuthzReader = nil
			}
			l.idents.Put(userInfo{UserID: "U_CLICKER", Email: "clicker@example.com", TeamID: "T_TEST"})
			canonical, err := l.resolveCanonicalForSlackUser(context.Background(), "U_CLICKER")
			require.NoError(t, err)
			cb := slackapi.InteractionCallback{TriggerID: "trigger", ActionCallback: slackapi.ActionCallbacks{BlockActions: []*slackapi.BlockAction{{ActionID: sessionInstructionsActionID, Value: instructionsButton("default/async", "View exact instructions", 0).Value}}}}
			cb.User.ID = "U_CLICKER"
			handled, err := l.handleSessionInstructionsAction(context.Background(), cb)
			require.NoError(t, err)
			require.True(t, handled)
			require.Len(t, api.views, 1)
			text := blocksText(api.views[0].Blocks.BlockSet)
			if tc.allowed {
				require.Contains(t, text, sess.Spec.Prompt.Inline)
			} else {
				require.NotContains(t, text, "exact private instructions")
				require.Contains(t, text, "unavailable")
			}
			if !tc.missing {
				require.Equal(t, strings.TrimPrefix(canonical, "user:"), auth.subject.String())
			}
		})
	}
}

func TestOpeningBlocks_AlwaysExposeInstructions(t *testing.T) {
	blocks := openingBlocks("<@USER> <https://bad.example> *summary*", "default/async")
	require.Equal(t, "plain_text", blocks[0].(*slackapi.SectionBlock).Text.Type)
	require.True(t, hasActionBlockWithID(blocks, sessionInstructionsActionID))
	v, ok := decodeApprovalButtonValue(instructionsButton("default/async", "View exact instructions", 0).Value)
	require.True(t, ok)
	require.Equal(t, discSessionInstructions, v.V)
	require.Equal(t, "default/async", v.S)
}
