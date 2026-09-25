package agentclass

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/controllers/testfixtures"
)

// The WIRING, which is where this broke.
//
// resolveSlotValueKeying was taught to walk toolkits, and its unit tests passed
// the embedded git toolkit in directly — so they proved the function and said
// nothing about where the caller gets its toolkits from. The caller used
// listToolkitsFor, which is scoped to CR-authored toolkits ON PURPOSE (it feeds
// validation, and failing a class on a spec no operator can see with kubectl
// would be unhelpful).
//
// git and gh ship EMBEDDED. A real cluster has no SpiceboxToolkit CRs at all,
// so the walk saw nothing and published no chain for the git_repo slot — and an
// empty chain is not "unknown" downstream: threadseed and the approval backfill
// read it as "the raw value IS the object id". Verified on a live cluster,
// where status.resolvedSlots came back {"permission":"push","resourceType":"git_repo"}
// with no valueTransforms.
func TestToolkitsForKeying_findsAnEmbeddedToolkitWithNoCR(t *testing.T) {
	scheme := testfixtures.NewScheme(t)

	ts := &spiceboxv1alpha1.SpiceboxToolspec{}
	ts.Name = "git-rw"
	ts.Spec.Toolkit.Name = "git"

	ac := classDeclaringSlots(spiceboxv1alpha1.AuthzSlot{
		ResourceType: "git_repo", Description: "a repository", Permission: "push",
	})
	ac.Name = "codebot"
	ac.Namespace = "default"
	ac.Spec.ToolBundles = []spiceboxv1alpha1.ToolBundle{{Name: "gitlike", Toolspecs: []string{"git-rw"}}}

	// Deliberately NO SpiceboxToolkit object: this is what a real cluster looks
	// like for a shipped toolkit.
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ts).Build()

	got, err := toolkitsForKeying(context.Background(), c, ac)
	require.NoError(t, err)
	require.Len(t, got, 1, "the embedded git toolkit must be reachable without a CR")
	assert.Equal(t, "git", got[0].Name)

	// And end to end: the chain must actually publish for the slot.
	slots, reason, msg := resolveSlotValueKeying(ac, nil, got, nil, nil)
	require.Empty(t, reason, "msg=%s", msg)
	require.Len(t, slots, 1)
	assert.NotEmpty(t, slots[0].ValueTransforms,
		"this is the value status.resolvedSlots carried as empty on a live cluster")
	// UnsafeSlotTransforms, not NonInjectiveTransforms: the property that
	// matters is "the repository on the card is the only one that reaches the
	// grant", and injectivity is a PROXY for it. github_repo_id folds .git and
	// scp spellings of ONE repository, which is identity-preserving, not a
	// collision — and it is the standard the controller itself enforces
	// (slot_valuekey.go, permission_validation.go). Asserting the stricter
	// proxy here made this test reject a chain production accepts.
	assert.Empty(t, authz.UnsafeSlotTransforms(slots[0].ValueTransforms),
		"every transform must be injective or identity-preserving, or the repository on the card is not the only one that reaches the grant")
}

// A CR-authored toolkit still wins: an operator who authors one is overriding
// the embedded copy, and silently preferring the embedded spec would make that
// override do nothing.
func TestToolkitsForKeying_aCRAuthoredToolkitTakesPrecedence(t *testing.T) {
	scheme := testfixtures.NewScheme(t)

	ts := &spiceboxv1alpha1.SpiceboxToolspec{}
	ts.Name = "git-rw"
	ts.Spec.Toolkit.Name = "git"

	authored := &spiceboxv1alpha1.SpiceboxToolkit{}
	authored.Name = "git"
	authored.Spec.Name = "git"
	// Declared because standing has no default: a toolkit whose subcommands
	// check git_repo must say who may approve it. session-only is the truthful
	// answer here and in toolkits/git.yaml — nothing writes a SpiceDB tuple per
	// git remote.
	authored.Spec.SpiceDBSchema = &spiceboxv1alpha1.SpiceDBSchemaFragment{
		Resources: []spiceboxv1alpha1.SpiceDBResource{
			{Name: "git_repo", Standing: spiceboxv1alpha1.StandingSessionOnly},
		},
	}
	authored.Spec.Subcommands = []spiceboxv1alpha1.ToolkitSubcommand{
		exprSub([]string{"clone"}, "git_repo", "fetch", "normalize_url", "sha256"),
	}

	ac := classDeclaringSlots(spiceboxv1alpha1.AuthzSlot{
		ResourceType: "git_repo", Description: "a repository", Permission: "push",
	})
	ac.Name, ac.Namespace = "codebot", "default"
	ac.Spec.ToolBundles = []spiceboxv1alpha1.ToolBundle{{Name: "gitlike", Toolspecs: []string{"git-rw"}}}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ts, authored).Build()

	got, err := toolkitsForKeying(context.Background(), c, ac)
	require.NoError(t, err)
	require.Len(t, got, 1)

	slots, reason, _ := resolveSlotValueKeying(ac, nil, got, nil, nil)
	require.Empty(t, reason)
	require.Len(t, slots, 1)
	assert.Equal(t, []string{"normalize_url", "sha256"}, slots[0].ValueTransforms,
		"the authored CR's chain, not the embedded one")
}
