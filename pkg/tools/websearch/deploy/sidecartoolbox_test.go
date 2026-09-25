// pkg/tools/kinds/sidecartoolbox.ValidateFile against this package's own
// declaration file: proves the SidecarToolbox/websearchd artifact stays
// valid, keeps declaring exactly the search+fetch tool set, and keeps both
// of plan-8c's lessons applied (real args.allowedFields; a stateImpact that
// does not demand a check the toolbox cannot supply).
//
// This package does NOT walk the live ap-websearchd server — that daemon is
// package main (internal/cmd/websearchd), unimportable from here, and its
// own package hosts the derivation test that DOES boot the real server (see
// that package's own test file for why it lives there instead of here).
package deploy_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/yaml"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/tools/kinds/sidecartoolbox"
)

func declarationPath(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	require.NoError(t, err)
	return filepath.Join(wd, "sidecartoolbox.yaml")
}

func loadSpec(t *testing.T) spiceboxv1alpha1.SidecarToolboxSpec {
	t.Helper()
	data, err := os.ReadFile(declarationPath(t))
	require.NoError(t, err)
	var sp spiceboxv1alpha1.SidecarToolboxSpec
	require.NoError(t, yaml.Unmarshal(data, &sp))
	return sp
}

func TestDeclaration_ValidatesClean(t *testing.T) {
	k := sidecartoolbox.New()
	diags, err := k.ValidateFile(declarationPath(t))
	require.NoError(t, err, "ValidateFile")
	assert.Empty(t, diags, "the websearchd declaration must validate with zero diagnostics, got %+v", diags)
}

func TestDeclaration_IsolatedSoTheCredentialNeverEntersTheRunnerPod(t *testing.T) {
	sp := loadSpec(t)
	assert.Equal(t, spiceboxv1alpha1.SidecarToolboxIsolationIsolated, sp.Isolation,
		"plan 0a's isolation field is what buys this sidecar its own pod, own NetworkPolicy, and keeps "+
			"WEBSEARCH_API_KEY out of the runner's own container — absent/auto would run it in-pod")
}

func TestDeclaration_UpstreamAuthNamesARealCredential(t *testing.T) {
	sp := loadSpec(t)
	assert.NotEqual(t, spiceboxv1alpha1.UpstreamAuthProviderNone, sp.UpstreamAuth.Provider,
		"unlike the workshop sidecar, ap-websearchd needs a real search-API credential, not a controller-issued token")
	assert.NotEmpty(t, sp.UpstreamAuth.EnvVar, "the resolved credential must land in an env var ap-websearchd's main.go actually reads")
}

func TestDeclaration_DeclaresExactlySearchAndFetch(t *testing.T) {
	sp := loadSpec(t)
	var got []string
	for _, tool := range sp.Tools {
		got = append(got, tool.Name)
	}
	assert.ElementsMatch(t, []string{"search", "fetch"}, got)
}

// TestDeclaration_EveryToolHasRealAllowedFields is this task's own version
// of the plan-8c regression guard (TestApply_ManifestArgumentIsAllowed in
// pkg/tools/workshopmcp/deploy): every tool that takes an argument must
// declare args.allowedFields, or pkg/tools/mcp/validator's checkAllowedFields
// (fail-closed: empty AllowedFields denies every argument) refuses the
// model's first real call. The LIVE-SERVER derivation half of this guard —
// asserting these lists match the running server's own InputSchema
// properties — lives in internal/cmd/websearchd's own test file; this
// assertion is the static half: no tool ships with an empty allowlist.
func TestDeclaration_EveryToolHasRealAllowedFields(t *testing.T) {
	sp := loadSpec(t)
	for _, tool := range sp.Tools {
		if tool.Args.UnconstrainedArgs {
			continue
		}
		assert.NotEmpty(t, tool.Args.AllowedFields, "%q must declare args.allowedFields (or unconstrainedArgs) or every argument is denied fail-closed", tool.Name)
	}
}

// TestDeclaration_StateImpactIsReachable is this task's version of the
// plan-8c MAJOR-1 regression guard (TestInventory_RunnerGateAllowsWithNoApproval
// in pkg/tools/workshopmcp/deploy): every tool's declared stateImpact must
// not demand a permission.check this toolbox cannot supply. Both tools here
// are Passthrough with no Check (see this file's header comment for why
// Passthrough was chosen over Readonly/Readwrite/External), so
// CheckRequired() is false and check_tool_call.go's gate never asks for one.
func TestDeclaration_StateImpactIsReachable(t *testing.T) {
	sp := loadSpec(t)
	for _, tool := range sp.Tools {
		require.NotNil(t, tool.Permission, "%q must declare a permission block", tool.Name)
		assert.Equal(t, authz.Passthrough, tool.Permission.StateImpact, "%q", tool.Name)
		assert.False(t, tool.Permission.StateImpact.CheckRequired(), "%q's stateImpact must not require a check this toolbox cannot supply", tool.Name)
		assert.Nil(t, tool.Permission.Check, "%q must not declare permission.check: no per-call SpiceDB resource id exists for an arbitrary query/URL", tool.Name)
	}
}

// TestDeclaration_BothToolsDeclareUntrustedSource pins design doc §6: a
// SidecarToolbox can carry the SEP-1913 returnMetadata.source declaration
// that marks a tool's result as untrusted-public content (the per-datum
// taint's only source today) — an in-runner meta tool cannot. Both tools
// read from the open web, the least ambiguous untrusted source this
// platform has, so both must carry the declaration; neither should rely on
// "no declaration" (read as NOT untrusted — see
// pkg/agent/runner/leakagewiring.LookupMCPToolUntrustedSource's own doc
// comment on that default).
func TestDeclaration_BothToolsDeclareUntrustedSource(t *testing.T) {
	sp := loadSpec(t)
	for _, tool := range sp.Tools {
		require.NotNil(t, tool.Trust.ReturnMetadata, "%q must declare trust.returnMetadata.source", tool.Name)
		assert.Contains(t, string(tool.Trust.ReturnMetadata.Raw), "untrustedPublic", "%q", tool.Name)
	}
}
