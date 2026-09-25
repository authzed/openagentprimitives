package hooks_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/authz/hooks"
	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"
)

// The hook must hand the CHECK and the APPROVAL ASK the same view of the call.
//
// It did not. A sandbox tool arrives as {operation_id, _reason, args: [argv]};
// unwrapEnvelope leaves that untouched because `args` is an ARRAY, not a map.
// BuildInputs then converted it to the tool's PARSED named args for the Check,
// while BuildApprovalAsk was handed the raw envelope — so the ask resolved
// `{remote}` against a map with no `remote` in it and produced an EMPTY resource
// id.
//
// Observed live approving a real push: the card rendered the URL correctly (it
// shows argv), the human approved, and channelsd then refused the decision with
//
//	authz: slot binding needs resourceType, resourceID and permission:
//	{ResourceType:git_repo ResourceID: Permission:push}
//
// The approval was unusable, minutes after the fact, in a different process,
// with nothing naming the cause.
func TestToolCallAuthz_theCheckAndTheApprovalSeeTheSameArgs(t *testing.T) {
	var checkArgs, askArgs map[string]any

	// Stands in for a sandbox tool's parser: argv -> named args.
	named := map[string]any{"remote": "https://github.com/demo-org/demo-repo", "subcommand": "push"}

	h := hooks.NewToolCallAuthz(hooks.ToolCallAuthzDeps{
		Mode:   "enforcing",
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		ResolvePermission: func(string, map[string]any) (authz.Permission, error) {
			return authz.Permission{
				StateImpact: authz.External,
				Check: &authz.PermissionCheck{
					ResourceType: "git_repo", Permission: "push",
					ResourceIDTemplate: "{remote}",
				},
			}, nil
		},
		NormalizeArgs: func(_ pipeline.Input, _ map[string]any) map[string]any { return named },
		BuildInputs: func(_ pipeline.Input, argsMap map[string]any, _ authz.Permission) authz.Inputs {
			checkArgs = argsMap
			return authz.Inputs{Args: argsMap, Subject: "u"}
		},
		Checker: &fakeChecker{result: authz.Result{Outcome: authz.OutcomeDenied}},
		BuildApprovalAsk: func(_ context.Context, _ pipeline.Input, argsMap map[string]any,
			_ authz.Permission, _ string) (*pipeline.ApprovalAsk, error) {
			askArgs = argsMap
			return &pipeline.ApprovalAsk{}, nil
		},
	})

	envelope, err := json.Marshal(map[string]any{
		"operation_id": "op-1", "_reason": "push the branch",
		"args": []any{"push", "https://github.com/demo-org/demo-repo", "main"},
	})
	require.NoError(t, err)

	_ = h.Eval(context.Background(), pipeline.Input{
		Point: pipeline.PreToolCall,
		Tool:  &pipeline.ToolCallInfo{Name: "gitlike_git", Args: envelope, UseID: "tu_1"},
	})

	require.NotNil(t, checkArgs, "the check must have been given args")
	require.NotNil(t, askArgs, "the approval ask must have been given args")
	assert.Equal(t, checkArgs, askArgs,
		"the check and the approval must resolve the same resource; two views of one call is how "+
			"a card renders a URL and the decision writes an empty id")

	// And the id the ask would resolve must be the real one.
	id, err := authz.ResolveResourceID(authz.PermissionCheck{
		ResourceType: "git_repo", Permission: "push", ResourceIDTemplate: "{remote}",
	}, askArgs)
	require.NoError(t, err)
	assert.Equal(t, "https://github.com/demo-org/demo-repo", id)
}

// Permission resolution needs the RAW argv; the check needs the NAMED args. They
// are different views and the ORDER matters.
//
// Normalizing before ResolvePermission removed the `args` key that
// PermissionForCall -> IdentifySubcommand -> argvFrom reads, so no subcommand
// ever matched and every sandbox call fell back to the toolkit default —
// `passthrough`. A total authorization bypass for sandbox tools, produced while
// fixing a different bug about those same two views.
//
// Caught by four bronze bundles reporting no authz decision at all, because the
// call was executing instead of being gated.
func TestToolCallAuthz_resolvesThePermissionFromRawArgvNotNamedArgs(t *testing.T) {
	var sawRaw map[string]any
	named := map[string]any{"remote": "origin", "subcommand": "fetch"}

	h := hooks.NewToolCallAuthz(hooks.ToolCallAuthzDeps{
		Mode:   "enforcing",
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		ResolvePermission: func(_ string, argsMap map[string]any) (authz.Permission, error) {
			sawRaw = argsMap
			return authz.Permission{StateImpact: authz.Readonly,
				Check: &authz.PermissionCheck{ResourceType: "git_repo", Permission: "read"}}, nil
		},
		NormalizeArgs: func(_ pipeline.Input, _ map[string]any) map[string]any { return named },
		BuildInputs: func(_ pipeline.Input, argsMap map[string]any, _ authz.Permission) authz.Inputs {
			return authz.Inputs{Args: argsMap, Subject: "u"}
		},
		Checker: &fakeChecker{result: authz.Result{Outcome: authz.OutcomeAllowed}},
	})

	envelope, err := json.Marshal(map[string]any{
		"operation_id": "op-1", "_reason": "r",
		"args": []any{"fetch", "origin"},
	})
	require.NoError(t, err)

	_ = h.Eval(context.Background(), pipeline.Input{
		Point: pipeline.PreToolCall,
		Tool:  &pipeline.ToolCallInfo{Name: "gitlike_git", Args: envelope, UseID: "tu_1"},
	})

	require.NotNil(t, sawRaw, "ResolvePermission must be called")
	assert.Contains(t, sawRaw, "args",
		"the permission resolver identifies the SUBCOMMAND from argv; strip that key and "+
			"every sandbox call falls back to the toolkit default, which is passthrough")
	assert.NotContains(t, sawRaw, "subcommand",
		"it must see the raw envelope, not the parsed view")
}
