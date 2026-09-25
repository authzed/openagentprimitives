package registry_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/registry"

	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/bento"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/browser"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/fake"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/github"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/local"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/slack"
)

// TestEveryKindDeclaresUsableSupportedRoles pins the shape of the answer.
// SupportedRoles is read by callers holding no Channel at all
// (pkg/platform/oap/channelplan, linting a bundle's requires.channels), so a
// nil or misspelled answer is not a nil-check away from being noticed: it
// silently makes every role illegal for that kind, or one role illegal
// forever.
func TestEveryKindDeclaresUsableSupportedRoles(t *testing.T) {
	kinds := registry.All()
	require.NotEmpty(t, kinds, "the kind registry must be populated by init")

	legal := spiceboxv1alpha1.AllChannelRoles()
	for _, k := range kinds {
		t.Run(k.Name()+": non-empty, legal, no duplicates", func(t *testing.T) {
			roles := k.SupportedRoles()
			require.NotEmpty(t, roles,
				"kind %q serves no role at all; a kind meaning 'all of them' returns AllChannelRoles()", k.Name())
			seen := map[string]bool{}
			for _, r := range roles {
				assert.Contains(t, legal, r,
					"kind %q declares %q, which is not a ChannelSpec.Role value", k.Name(), r)
				assert.False(t, seen[r], "kind %q declares role %q twice", k.Name(), r)
				seen[r] = true
			}
		})
	}
}

// The other half of the contract — that a kind's ValidateSpec actually
// ENFORCES the role set it declares — is deliberately NOT asserted here, and
// the reason is worth stating so nobody adds it back.
//
// A registry sweep can only build a Channel carrying a kind and a role, since
// it cannot know any kind's spec block. On such a Channel every kind that
// needs one refuses for THAT reason (github wants spec.github, slack
// spec.slack), so `assert.Error` holds no matter what the role gate does: the
// assertion passes for the wrong reason. Verified, not assumed — with github's
// role gate deleted while its SupportedRoles stayed input-only, a sweep of
// exactly that shape stayed green.
//
// So enforcement is asserted where a complete spec can honestly be built: each
// kind's own TestValidateSpec (github/kind_test.go, bento/kind_test.go), which
// walks every role outside its SupportedRoles against a well-formed Channel.
// Those tests DO redden on the same break.
