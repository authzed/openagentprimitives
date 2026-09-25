package sessioncmd

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/meta/capability"
	"github.com/authzed/openagentprimitives/pkg/authz"
)

// The names the two fixture capabilities below register under, and the tools
// they contribute. Deliberately unlike any real capability: a case here must
// fail because the CHECK stopped following a declaration, never because a
// production capability happened to move.
const (
	capDeclaresProviderSurface = "demo_provider_surface"
	capDeclaresNothing         = "demo_local"
	toolOnProviderSurface      = "demo_provider_write"
	toolOnNoProviderSurface    = "demo_local_write"
)

// Registered process-wide because capability.Register is the only way into the
// registry and there is no exported removal. Both are OPT-IN (DefaultOn false)
// and no other test's class grants them by name, so they contribute nothing to
// any assembly but the ones below, which grant them deliberately.
func init() {
	capability.Register(providerSurfaceFixtureCapability{
		fixtureCapability{name: capDeclaresProviderSurface, contributes: toolOnProviderSurface},
	})
	capability.Register(fixtureCapability{name: capDeclaresNothing, contributes: toolOnNoProviderSurface})
}

// fixtureCapability contributes exactly one named tool whenever the class
// grants it, and declares nothing about where that tool lands.
type fixtureCapability struct {
	name        string
	contributes string
}

func (c fixtureCapability) Name() string                                           { return c.name }
func (c fixtureCapability) DefaultOn() bool                                        { return false }
func (c fixtureCapability) Infrastructural() bool                                  { return false }
func (c fixtureCapability) ParseConfig(json.RawMessage) (capability.Config, error) { return nil, nil }
func (c fixtureCapability) Offer(capability.OfferContext) ([]tool.Tool, *capability.SkipReason) {
	return []tool.Tool{fixtureTool{name: c.contributes}}, nil
}

// providerSurfaceFixtureCapability is the same capability, DECLARING that its
// tool acts on a third party's own surface. The declaration is the only
// difference between the two, which is what makes the pair a controlled
// experiment on the check.
type providerSurfaceFixtureCapability struct{ fixtureCapability }

func (providerSurfaceFixtureCapability) ActsOnProviderSurface() {}

var _ capability.ProviderSurface = providerSurfaceFixtureCapability{}

// fixtureTool is a minimal tool.Tool; nothing here is ever executed, only
// offered and named.
type fixtureTool struct{ name string }

func (f fixtureTool) Name() string                 { return f.name }
func (f fixtureTool) Kind() tool.Kind              { return tool.KindMeta }
func (f fixtureTool) Description() string          { return "" }
func (f fixtureTool) InputSchema() json.RawMessage { return json.RawMessage(`{}`) }
func (f fixtureTool) Execute(context.Context, json.RawMessage, *tool.SessionContext) (tool.Result, error) {
	return tool.Result{}, nil
}
func (f fixtureTool) Permission() authz.Permission {
	return authz.Permission{StateImpact: authz.Stateless}
}
func (f fixtureTool) PermissionVariants() []authz.PermissionVariant { return nil }

// TestExternalSurfaceTools_FollowsTheDeclaringCapability pins the PROPERTY the
// values in TestFixtureMetaTools_MarksTheInputKindsOwnProviderTools rest on:
// the external-surface set is whatever the declaring capabilities contributed,
// asked of them, and not a list of tool names kept next to the check.
//
// A list would pass that values test forever and fail here the moment a
// declaring capability's contribution moved — which is the failure worth
// catching, since that is the moment a bundle starts claiming a write to
// someone else's system is replayable.
func TestExternalSurfaceTools_FollowsTheDeclaringCapability(t *testing.T) {
	grantBoth := map[string]apiextensionsv1.JSON{
		capDeclaresProviderSurface: {Raw: []byte(`{}`)},
		capDeclaresNothing:         {Raw: []byte(`{}`)},
	}

	t.Run("a declared contribution is marked external, whatever it is called", func(t *testing.T) {
		fixture, sess, cli := triggerFixture(t, grantBoth)
		external, ordinary := surfaceSplit(t, fixture, sess, cli)

		assert.Contains(t, external, toolOnProviderSurface,
			"the check must report what the declaring capability offered, not a set of "+
				"names it was written with")
		assert.Contains(t, external, "claim_trigger_status",
			"and it must keep reporting the production declaration alongside it")
		assert.Contains(t, ordinary, toolOnNoProviderSurface,
			"a capability that declares nothing reaches nothing external, however much "+
				"its tool looks like a write")
		assert.NotContains(t, external, toolOnNoProviderSurface)
	})

	t.Run("an ungranted capability contributes to neither half", func(t *testing.T) {
		fixture, sess, cli := triggerFixture(t, nil)
		external, ordinary := surfaceSplit(t, fixture, sess, cli)

		// Both fixture capabilities are opt-in, so an ungranted class assembles
		// exactly as it did before they were registered. Without this, the case
		// above could be passing on a capability every other test in this
		// package is silently carrying too.
		assert.NotContains(t, external, toolOnProviderSurface)
		assert.NotContains(t, ordinary, toolOnProviderSurface)
		assert.NotContains(t, external, toolOnNoProviderSurface)
		assert.NotContains(t, ordinary, toolOnNoProviderSurface)
	})
}
