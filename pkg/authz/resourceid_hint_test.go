package authz_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/authz/spicedb/toolcheck"
)

// Readwrite, not External: External routes to human approval before any
// resource resolution happens, so the check path is the readwrite/readonly one.
//
// When a resource id cannot be resolved because the CALL did not name the
// resource, the agent is the only party that can fix it — by retrying with the
// argument supplied. The message it got was `internal: authz: template
// references arg "repo" which is not present`, which reads as a system bug and
// tells it nothing to do.
//
// The hint lives on the check because only the toolkit author knows what the
// caller should have written.
func TestCheckToolCall_unresolvableIDSurfacesTheAuthorsHint(t *testing.T) {
	res := (toolcheck.Checker{}).CheckToolCall(t.Context(), authz.Permission{
		StateImpact: authz.Readwrite,
		Check: &authz.PermissionCheck{
			ResourceType:   "git_repo",
			Permission:     "push",
			ResourceIDExpr: `has(args.remote) && args.remote.startsWith("https://") ? args.remote : ""`,
			ResourceIDHint: "name the remote as a full https:// URL, not a shorthand like `origin`",
		},
	}, authz.Inputs{Args: map[string]any{"remote": "origin"}, Subject: "user:u"})

	assert.Equal(t, authz.OutcomeDenied, res.Outcome)
	assert.Contains(t, res.Message, "https:// URL",
		"the author's hint must reach the agent verbatim — it is the only actionable part")
	assert.NotContains(t, res.Message, "internal:",
		"this is a caller error, not a system fault; labelling it internal tells the agent not to retry")
}

// Without a hint the behaviour is unchanged: still a denial, still fail-closed.
// The hint improves the message; it must not become a requirement.
func TestCheckToolCall_unresolvableIDWithoutAHintStillDenies(t *testing.T) {
	res := (toolcheck.Checker{}).CheckToolCall(t.Context(), authz.Permission{
		StateImpact: authz.Readwrite,
		Check: &authz.PermissionCheck{
			ResourceType:   "git_repo",
			Permission:     "push",
			ResourceIDExpr: `has(args.remote) ? args.remote : ""`,
		},
	}, authz.Inputs{Args: map[string]any{}, Subject: "user:u"})

	assert.Equal(t, authz.OutcomeDenied, res.Outcome)
}

// A call that never named its resource is the CALLER's error, not a permission
// an approver could grant — and the dispatch hook turns an ordinary readwrite
// denial into an approval card. For this denial that card is unanswerable: it
// asks a human to approve the resource type with an EMPTY object id, and
// approving it changes nothing, because the next call fails at resolution again
// before SpiceDB is ever consulted. The Result has to say which kind of denial
// it is so the hook can send this one straight back to the agent.
func TestCheckToolCall_unresolvableIDIsNotSomethingAnApproverCanFix(t *testing.T) {
	cases := []struct {
		name  string
		check authz.PermissionCheck
		args  map[string]any // nil uses the default no-remote args
	}{
		{
			name: "guard expression refused the call: UnresolvedResource",
			check: authz.PermissionCheck{
				ResourceType:   "git_repo",
				Permission:     "fetch",
				ResourceIDExpr: `has(args.remote) && args.remote.startsWith("https://") ? args.remote : ""`,
			},
		},
		{
			name: "template referenced an absent arg: UnresolvedResource",
			check: authz.PermissionCheck{
				ResourceType:       "git_repo",
				Permission:         "fetch",
				ResourceIDTemplate: "{remote}",
			},
		},
		{
			// The two template cases take DIFFERENT routes and both must set the
			// flag: an absent arg errors out of ResolveTemplate, a present-but-
			// empty one substitutes cleanly to "". The second is the only way to
			// reach the empty-id guard at all — CEL rejects an empty result as an
			// error — so without this row that branch is never exercised.
			name: "template arg present but empty: UnresolvedResource",
			check: authz.PermissionCheck{
				ResourceType:       "git_repo",
				Permission:         "fetch",
				ResourceIDTemplate: "{remote}",
			},
			args: map[string]any{"remote": ""},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			args := tc.args
			if args == nil {
				args = map[string]any{"origin": "origin"}
			}
			res := (toolcheck.Checker{}).CheckToolCall(t.Context(), authz.Permission{
				StateImpact: authz.Readwrite,
				Check:       &tc.check,
			}, authz.Inputs{Args: args, Subject: "user:u"})

			assert.Equal(t, authz.OutcomeDenied, res.Outcome)
			assert.True(t, res.UnresolvedResource,
				"an unresolvable resource id must not be routed to an approver")
		})
	}
}

// The contrast case, and what keeps the flag honest: a call that DID name its
// resource and merely lacks the permission is exactly what approval exists for.
// A nil client denies fail-closed without resolving anything in SpiceDB, which
// is enough to show the flag tracks resolution rather than the outcome.
func TestCheckToolCall_aDenialWithAResolvedResourceStaysApprovable(t *testing.T) {
	res := (toolcheck.Checker{}).CheckToolCall(t.Context(), authz.Permission{
		StateImpact: authz.Readwrite,
		Check: &authz.PermissionCheck{
			ResourceType:   "git_repo",
			Permission:     "fetch",
			ResourceIDExpr: `has(args.remote) && args.remote.startsWith("https://") ? args.remote : ""`,
		},
	}, authz.Inputs{
		Args:    map[string]any{"remote": "https://github.com/demo-org/demo-repo"},
		Subject: "user:u",
	})

	assert.Equal(t, authz.OutcomeDenied, res.Outcome)
	assert.False(t, res.UnresolvedResource,
		"the resource resolved; this denial is the kind an approver CAN act on")
}
