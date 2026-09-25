// pkg/controllers/agentclass/toolkit_shape_validation_test.go
//
// A toolkit's permission checks must clear the same shape rules an MCP tool's
// do. They did not: validateToolkitSubcommands only asked whether a
// state-mutating subcommand HAD a permission and whether its stateImpact was
// sane, so the transform rules never ran over a toolkit at all.
//
// The rule that went missing is the one that matters most. An expr-keyed check
// mints an object id from a free-form value, so a lossy transform there means
// two different targets become one resource and an approval for the first
// silently covers the second. That is why the shipped git and gh toolkits could
// carry `spicedb_object_id` under a `resourceIDExpr` — the validator that
// forbids it never looked at them.
package agentclass

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz"
	embeddedtoolkits "github.com/authzed/openagentprimitives/toolkits"
)

// toolkitWithCheck wraps one check in a non-mutating subcommand, so what the
// test exercises is the SHAPE walk and not the state-mutating rule that already
// existed.
func toolkitWithCheck(name string, chk *authz.PermissionCheck) spiceboxv1alpha1.SpiceboxToolkit {
	tk := spiceboxv1alpha1.SpiceboxToolkit{
		Spec: spiceboxv1alpha1.SpiceboxToolkitSpec{
			Name: name,
			Subcommands: []spiceboxv1alpha1.ToolkitSubcommand{{
				Path:       []string{"clone"},
				Permission: &authz.Permission{StateImpact: authz.Readwrite, Check: chk},
			}},
		},
	}
	// The error refs identify a toolkit by its OBJECT name, which is what a
	// human has to `kubectl get` — so the fixture has to carry one.
	tk.Name = name
	return tk
}

func TestValidatePermissions_ToolkitCheckShapeIsEnforced(t *testing.T) {
	cases := []struct {
		name    string
		check   *authz.PermissionCheck
		reason  string
		msgPart string
	}{
		{
			name: "expr-keyed with a lossy terminal: rejected, since two targets would share one id",
			check: &authz.PermissionCheck{
				ResourceType: "git_repo", Permission: "fetch",
				ResourceIDExpr:       "args.repository",
				ResourceIDTransforms: []string{"normalize_url", "spicedb_object_id"},
			},
			reason:  spiceboxv1alpha1.ReasonPermissionSpecInvalid,
			msgPart: "every transform must preserve resource identity",
		},
		{
			name: "unknown transform: rejected rather than silently skipped at runtime",
			check: &authz.PermissionCheck{
				ResourceType: "git_repo", Permission: "fetch",
				ResourceIDExpr: "args.repository", ResourceIDTransforms: []string{"no_such_transform"},
			},
			reason:  spiceboxv1alpha1.ReasonPermissionTransformUnknown,
			msgPart: "unknown transform",
		},
		{
			name: "empty resourceType: rejected",
			check: &authz.PermissionCheck{
				Permission: "fetch", ResourceIDExpr: "args.repository",
			},
			reason:  spiceboxv1alpha1.ReasonPermissionSpecInvalid,
			msgPart: "resourceType is empty",
		},
		{
			name: "template-keyed with a lossy transform: allowed, the id is already a distinct resource",
			check: &authz.PermissionCheck{
				ResourceType: "git_repo", Permission: "write",
				ResourceIDTemplate: "{repo}", ResourceIDTransforms: []string{"lowercase"},
			},
		},
		{
			name: "expr-keyed, fully injective: allowed",
			check: &authz.PermissionCheck{
				ResourceType: "git_repo", Permission: "fetch",
				ResourceIDExpr:       "args.repository",
				ResourceIDTransforms: []string{"normalize_url", "spicedb_escape"},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// permissive, deliberately: a lossy id is a defect in the spec, not
			// a property of the mode. Gating this on enforcing would let a class
			// carry the defect until the day someone flips the mode.
			reason, msg := validatePermissions(classInMode(toolAuthModePermissive), nil,
				[]spiceboxv1alpha1.SpiceboxToolkit{toolkitWithCheck("git", tc.check)}, nil)

			if tc.reason == "" {
				assert.Empty(t, reason, "expected admission; msg=%s", msg)
				return
			}
			assert.Equal(t, tc.reason, reason)
			assert.Contains(t, msg, tc.msgPart)
			assert.Contains(t, msg, "SpiceboxToolkit/git", "the message must name the offending toolkit")
		})
	}
}

// A variant can key its resource type differently from the base check, and it is
// the branch least likely to be read closely — so it is the one most worth
// asserting reaches the same rules.
func TestValidatePermissions_ToolkitVariantCheckShapeIsEnforced(t *testing.T) {
	tk := toolkitWithCheck("gh", &authz.PermissionCheck{
		ResourceType: "github_repo", Permission: "read",
		ResourceIDExpr: "args.repo", ResourceIDTransforms: []string{"spicedb_escape"},
	})
	tk.Spec.Subcommands[0].PermissionVariants = []authz.PermissionVariant{{
		When: `args.method == "GET"`,
		Check: authz.Permission{
			StateImpact: authz.Readonly,
			Check: &authz.PermissionCheck{
				ResourceType: "github_repo", Permission: "read",
				ResourceIDExpr: "args.endpoint", ResourceIDTransforms: []string{"basename"},
			},
		},
	}}

	reason, msg := validatePermissions(classInMode(toolAuthModePermissive), nil,
		[]spiceboxv1alpha1.SpiceboxToolkit{tk}, nil)

	assert.Equal(t, spiceboxv1alpha1.ReasonPermissionSpecInvalid, reason,
		"a variant keying a free-form value with basename collapses /a/x and /b/x onto one id")
	assert.Contains(t, msg, "variant[0]", "the message must point at the variant, not the base check")
}

// The regression guard for the toolkits we actually ship. This is what proves
// the git/gh chain fix was complete rather than partial: every embedded toolkit
// has to clear the rules that were just pointed at them.
func TestValidatePermissions_EveryEmbeddedToolkitClearsShapeValidation(t *testing.T) {
	all := embeddedtoolkits.All()
	require.NotEmpty(t, all, "embedded toolkits must load, or this guard silently asserts nothing")

	for _, lib := range all {
		t.Run(lib.Name, func(t *testing.T) {
			// The CR spec is byte-compatible with the library type by design
			// (SpiceboxToolkitSpec.ToToolkit round-trips the other way), so the
			// validator sees exactly what a cluster would.
			data, err := json.Marshal(lib)
			require.NoError(t, err)
			var spec spiceboxv1alpha1.SpiceboxToolkitSpec
			require.NoError(t, json.Unmarshal(data, &spec))

			tk := spiceboxv1alpha1.SpiceboxToolkit{Spec: spec}
			tk.Name = lib.Name

			reason, msg := validatePermissions(classInMode(toolAuthModePermissive), nil,
				[]spiceboxv1alpha1.SpiceboxToolkit{tk}, nil)
			assert.Empty(t, reason, "shipped toolkit %q fails shape validation: %s", lib.Name, msg)
		})
	}
}

// The configuration a real coding agent runs: a git_repo slot declared against
// the SHIPPED git toolkit. This is the combination the whole plan-as-approval
// surface rests on — without a declared slot a plan can only name a resource
// TYPE, so the approver agrees to a category and every instance costs another
// prompt.
//
// It was not declarable before. git's expr-keyed checks terminated in
// spicedb_object_id, which folds distinct URLs together, and resolveSlotValueKeying
// refuses to publish a chain that could map two repositories onto one object
// id. Asserting it against the embedded toolkit rather than a fixture is the
// point: a fabricated toolkit would keep passing if the shipped one regressed.
func TestResolveSlotValueKeying_aGitRepoSlotIsDeclarableAgainstTheShippedToolkit(t *testing.T) {
	var git spiceboxv1alpha1.SpiceboxToolkit
	for _, lib := range embeddedtoolkits.All() {
		if lib.Name != "git" {
			continue
		}
		data, err := json.Marshal(lib)
		require.NoError(t, err)
		require.NoError(t, json.Unmarshal(data, &git.Spec))
		git.Name = lib.Name
	}
	require.NotEmpty(t, git.Name, "the embedded git toolkit must load, or this asserts nothing")

	ac := classDeclaringSlots(spiceboxv1alpha1.AuthzSlot{
		ResourceType: "git_repo",
		Description:  "a repository",
		Permission:   "push",
	})
	toolkits := []spiceboxv1alpha1.SpiceboxToolkit{git}

	got, reason, msg := resolveSlotValueKeying(ac, nil, toolkits, nil, nil)
	require.Empty(t, reason, "a git_repo slot must be declarable; msg=%s", msg)
	require.Len(t, got, 1)
	assert.NotEmpty(t, got[0].ValueTransforms,
		"an empty chain is read downstream as 'the raw value IS the object id', so a seeded "+
			"grant would be written on a URL the check never computes")
	// Same standard the controller enforces: injective OR identity-preserving.
	// git_repo's chain leads with github_repo_id, which folds the spellings of
	// one repository (.git suffix, scp form, case) so git and gh land on one id.
	// That is an alias fold, never two repositories meeting.
	assert.Empty(t, authz.UnsafeSlotTransforms(got[0].ValueTransforms),
		"every transform must be injective or identity-preserving, or the repository shown on the "+
			"card is not the only one that can spend the approval")

	// And the class as a whole must admit, which is the other half: shape
	// validation now runs over toolkit checks.
	reason, msg = validatePermissions(classInMode(toolAuthModePermissive), nil, toolkits, nil)
	assert.Empty(t, reason, "the shipped git toolkit must clear shape validation: %s", msg)
}
