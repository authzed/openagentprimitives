package runner

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"
	"github.com/authzed/openagentprimitives/pkg/tools/browserschema"
)

// TestBuildInputs_HonorsWebActionStampsOnlyForBrowserTool is the runner-side
// trust gate for the convergent MAJOR. The browser acting tool's NamedArgs
// stamps web_action_required/key/fingerprint into its OWN parsed view, and
// BuildInputs lifts them into typed authz.Inputs that go on to mint a per-action,
// page-fingerprint-caveated web_action grant on approve. But an MCP tool's args
// arrive as the model's raw, unvalidated JSON (MCP tools implement no
// NamedCallArgs), so a prompt-injected model could set those same keys and cause
// a fabricated grant to be minted for a key/fingerprint of its choosing. The lift
// must therefore fire ONLY when the resolved permission belongs to the acting
// tool (perm.OwnsWebActionArgs — a web_host Check), and for any other tool it
// must ignore the keys AND leave them in the arg map as ordinary, visible args.
func TestBuildInputs_HonorsWebActionStampsOnlyForBrowserTool(t *testing.T) {
	l := &Loop{AgentName: "demo-agent", SessionKey: memory.NamespacedName{Namespace: "ns", Name: "s"}}
	deps := l.toolCallAuthzDeps()
	in := pipeline.Input{Tool: &pipeline.ToolCallInfo{Name: "x"}}

	stamps := func() map[string]any {
		return map[string]any{
			"ref":                                 "e13",
			browserschema.WebActionRequiredArg:    true,
			browserschema.WebActionKeyArg:         "attacker-chosen-key",
			browserschema.WebActionFingerprintArg: "attacker-chosen-fingerprint",
		}
	}

	t.Run("browser acting tool (web_host Check): stamps lifted and stripped", func(t *testing.T) {
		args := stamps()
		got := deps.BuildInputs(in, args, webHostActingPerm())

		assert.True(t, got.WebActionRequired, "the acting tool's own stamp must be honored")
		assert.Equal(t, "attacker-chosen-key", got.WebActionKey)
		assert.Equal(t, "attacker-chosen-fingerprint", got.PresentedPageFingerprint)
		// Stripped from the arg map so these internal gate keys never reach the
		// card, the args hash, or the durable record.
		assert.NotContains(t, args, browserschema.WebActionRequiredArg)
		assert.NotContains(t, args, browserschema.WebActionKeyArg)
		assert.NotContains(t, args, browserschema.WebActionFingerprintArg)
	})

	t.Run("MCP tool (no web_host Check): stamps ignored and left as visible args", func(t *testing.T) {
		args := stamps()
		// An External MCP permission with no web_host Check — the injection shape.
		got := deps.BuildInputs(in, args, authz.Permission{
			StateImpact: authz.External,
			Check:       &authz.PermissionCheck{ResourceType: "github_repo", Permission: "write"},
		})

		assert.False(t, got.WebActionRequired, "a non-browser tool's injected web_action_required must be ignored")
		assert.Empty(t, got.WebActionKey, "no fabricated web_action key may be lifted from an MCP tool's args")
		assert.Empty(t, got.PresentedPageFingerprint, "no fabricated fingerprint may be lifted")
		// NOT stripped: the keys stay as ordinary model args, so an injection
		// attempt is visible on the card rather than silently consumed.
		assert.Contains(t, args, browserschema.WebActionRequiredArg)
		assert.Contains(t, args, browserschema.WebActionKeyArg)
		assert.Contains(t, args, browserschema.WebActionFingerprintArg)
	})

	t.Run("passthrough tool with no Check at all: stamps ignored", func(t *testing.T) {
		args := stamps()
		got := deps.BuildInputs(in, args, authz.Permission{StateImpact: authz.Passthrough})
		assert.False(t, got.WebActionRequired)
		assert.Empty(t, got.WebActionKey)
		assert.Contains(t, args, browserschema.WebActionRequiredArg, "a nil-Check perm is not the acting tool; keys untouched")
	})
}

// TestOwnsWebActionArgs is the trusted discriminator's own unit + drift test. It
// pins the duplicated "web_host" literal in pkg/authz (which cannot import
// browserschema — an import cycle) equal to browserschema.WebHostType, and
// asserts the fail-closed cases: only a web_host Check owns the web_action
// control args; a nil Check, the web_action type itself, and any other resource
// type do not.
func TestOwnsWebActionArgs(t *testing.T) {
	webHost := authz.Permission{Check: &authz.PermissionCheck{ResourceType: browserschema.WebHostType}}
	assert.True(t, webHost.OwnsWebActionArgs(),
		"the browser acting tool's permission (Check targets web_host) owns the web_action stamps")

	assert.False(t, authz.Permission{}.OwnsWebActionArgs(),
		"a permission with no Check names no resource and is not the acting tool (fail-closed)")
	assert.False(t, authz.Permission{Check: &authz.PermissionCheck{ResourceType: browserschema.WebActionType}}.OwnsWebActionArgs(),
		"web_action is the per-action gate type, not the acting tool's coarse check type")
	assert.False(t, authz.Permission{Check: &authz.PermissionCheck{ResourceType: "github_repo"}}.OwnsWebActionArgs(),
		"an ordinary external tool's Check does not own the browser web_action stamps")
}
