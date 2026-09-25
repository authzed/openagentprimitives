package sandbox

import (
	"context"
	"time"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/tools/toolspec/spec"
)

// PickStreamingSubcommand exposes the unexported pickStreamingSubcommand
// to the external sandbox_test package so its match logic can be tested
// directly without constructing a full Synthesize call.
var PickStreamingSubcommand = pickStreamingSubcommand

// TimeoutForSubcommand exposes the unexported timeoutForSubcommand
// to the external sandbox_test package so timeout logic can be tested
// directly without constructing a full Synthesize call.
var TimeoutForSubcommand = timeoutForSubcommand

// GenerateStreamTokenAndHash exposes the unexported generateStreamTokenAndHash
// so the external sandbox_test package can assert the stream-token commitment
// (32 bytes of entropy, hex(sha256(token)) hash) in isolation.
var GenerateStreamTokenAndHash = generateStreamTokenAndHash

// ResolveCallTimeout exposes the unexported per-call timeout resolution so the
// external sandbox_test package can assert which subcommand budget an argv
// selects without a full ExecuteWithIDs round-trip.
func (s *SandboxTool) ResolveCallTimeout(args []string) time.Duration {
	return s.resolveCallTimeout(args)
}

// ComposeResult exposes the unexported composeResult to the external
// sandbox_test package so secret-output result composition can be tested
// directly without constructing a full ExecuteWithIDs round-trip. Drops the
// raw-stdout second return (added for evaluateObserves' benefit) so every
// existing single-value call site (`res := sandbox.ComposeResult(...)`) is
// unaffected.
func ComposeResult(ctx context.Context, tc *spiceboxv1alpha1.ToolCall, art ArtifactClient, secretOut *spec.SecretOutputSpec) tool.Result {
	res, _ := composeResult(ctx, tc, art, secretOut)
	return res
}

// ComposeStreamResult exposes the unexported composeStreamResult so the external
// sandbox_test package can assert the streaming-tool result envelope in
// isolation (status header, payload, IsError, stderr tail) without a full bridge
// round-trip.
var ComposeStreamResult = composeStreamResult

// ToolkitEnvDefaults exposes the unexported toolkitEnvDefaults so the external
// sandbox_test package can assert the floor semantics directly, without
// driving a full dispatch for every merge case.
var ToolkitEnvDefaults = toolkitEnvDefaults
