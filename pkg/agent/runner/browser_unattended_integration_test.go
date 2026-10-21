//go:build integration

// pkg/agent/runner/browser_unattended_integration_test.go
//
// Proves C8's unattended grant end to end against a REAL SpiceDB, using a REAL
// DOTTED hostname (app.example.com): given a ResolvedBrowserToolbox whose spec
// names one unattended site and one attended site,
// runner.UnattendedBrowserSlotBindings derives exactly the unattended one ON ITS
// ESCAPED OBJECT ID, and engine.Engine.GrantUnattendedSlots writes a real
// slot_grant_act relationship for it -- with no Check, no human card -- while the
// attended site gets none.
//
// This is the regression guard the dots bug (C-R4) lacked. A bare dotted
// hostname is NOT a legal SpiceDB object id (SpiceDB's charset is
// [a-zA-Z0-9_/\-=|+] -- "." is not in it), so the pre-fix code's
// authz.NewObjectID(host, nil) wrote the grant on the raw "app.example.com" and
// WriteRelationships rejected it with "object_id: does not match regex pattern".
// The fix threads the class's published web_host chain (spicedb_escape) through
// UnattendedBrowserSlotBindings so the grant is written on "app=2Eexample=2Ecom",
// the identical id the acting tool's Check resolves. Revert that thread and this
// test fails on the GrantUnattendedSlots write.
//
// This deliberately exercises the SAME mechanism the ruling (C-R3) requires:
// authz.GrantSlots called unconditionally, never authz.BindClassDefaults /
// BindClassDefaults's admissibleCandidates gate, which would Check web_host's
// inert owner leg and drop every candidate before any grant existed.
package runner_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/runner"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/authz/engine"
	"github.com/authzed/openagentprimitives/pkg/authz/spicedb"
	"github.com/authzed/openagentprimitives/pkg/authz/spicedb/toolcheck"
	"github.com/authzed/openagentprimitives/pkg/tools/browserschema"
	"github.com/authzed/openagentprimitives/test/testspicedb"
)

// webHostComposedFragment is the web_host definition as ComposeSlots emits it
// for a declared web_host#act slot -- the same fixture
// pkg/authz/spicedb/web_host_slot_integration_test.go uses, duplicated here
// rather than imported because it is a small schema literal, not a function,
// and the two tests belong to different packages.
const webHostComposedFragment = `
definition web_host {
    relation slot_grant_act: agentsession with expiration
    relation owner: user
    permission act = slot_grant_act->interact + owner
}
`

// webHostTransforms is the per-type chain the AgentClass publishes on
// status.resolvedSlots for web_host (runner.SlotTransformsOf), passed into
// UnattendedBrowserSlotBindings exactly as the runner cold start passes
// runner.SlotTransformsOf(&class).
var webHostTransforms = map[string][]string{browserschema.WebHostType: browserschema.WebHostValueTransforms()}

// actPerm is the acting tool's External web_host#act check, including the
// spicedb_escape chain the tool's Permission() now carries -- so the check path
// resolves the SAME escaped object id the grant was written on.
func actPerm() authz.Permission {
	return authz.Permission{
		StateImpact: authz.External,
		Check: &authz.PermissionCheck{
			ResourceType:         browserschema.WebHostType,
			Permission:           browserschema.ActPermission,
			ResourceIDTemplate:   "{host}",
			ResourceIDTransforms: browserschema.WebHostValueTransforms(),
		},
	}
}

func TestIntegration_UnattendedBrowserGrant_RealSpiceDB(t *testing.T) {
	endpoint := testspicedb.Endpoint(t)
	token := testspicedb.UniqueToken(t)
	testspicedb.WriteSchemaWithExtra(t, endpoint, token, webHostComposedFragment)
	c, err := spicedb.NewClient(endpoint, token, true)
	require.NoError(t, err, "NewClient")
	t.Cleanup(func() { _ = c.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	const ns, sessName = "ns", "s1"
	const unattendedHost = "app.example.com"        // dotted: real hostname shape
	const attendedHost = "portal.example.com"       // dotted: must NOT bind
	const escapedUnattended = "app=2Eexample=2Ecom" // what spicedb_escape produces
	sess := authz.SessionRef{Namespace: ns, Name: sessName}
	t.Cleanup(func() { _ = c.DeleteSlotGrants(context.Background(), ns, sessName) })

	toolboxes := []spiceboxv1alpha1.ResolvedBrowserToolbox{{
		Name: "browser",
		Spec: spiceboxv1alpha1.BrowserToolboxSpec{
			Access: spiceboxv1alpha1.BrowserAccess{
				Mode: "allowlist",
				Sites: []spiceboxv1alpha1.BrowserSite{
					{Host: unattendedHost, Unattended: true}, // unattended: must bind
					{Host: attendedHost},                     // attended: must not bind
				},
				Identity: "id1",
			},
		},
	}}

	bindings := runner.UnattendedBrowserSlotBindings(toolboxes, webHostTransforms)
	require.Len(t, bindings, 1, "only the unattended site derives a binding")
	assert.Equal(t, "web_host", bindings[0].ResourceType)
	assert.Equal(t, "act", bindings[0].Permission)
	// By VALUE: the grant must be written on the escaped id, so a regression to a
	// raw dotted id is caught here rather than only by the write failing.
	assert.Equal(t, escapedUnattended, bindings[0].ResourceID.String(),
		"the grant must be written on the spicedb_escaped host, not the raw dotted one")

	eng := engine.New(engine.Deps{RelWriter: c.Relations(), SessionExpiration: time.Hour})
	require.NoError(t, eng.GrantUnattendedSlots(ctx, sess, bindings),
		"GrantUnattendedSlots must accept the escaped id (the raw dotted id is rejected by SpiceDB)")

	// *Client satisfies toolcheck.Client (CheckPermission) directly, so the
	// real external-check path runs against real SpiceDB -- the same path
	// browserTool.Permission()'s External check uses at dispatch.
	checker := toolcheck.Checker{Cli: c}

	// The unattended host: a real slot_grant_act relationship now exists on the
	// escaped id, scoped to this session -- written with no Check and no human
	// card. The acting check resolves the SAME escaped id and is allowed.
	allowed := checker.CheckToolCall(ctx, actPerm(), authz.Inputs{
		Subject:           "u-1",
		Args:              map[string]any{"host": unattendedHost},
		AgentSessionRef:   ns + "/" + sessName,
		SlotResourceTypes: []string{browserschema.WebHostType},
	})
	assert.Equal(t, authz.OutcomeAllowed, allowed.Outcome,
		"web_host:app=2Eexample=2Ecom#act@agentsession:ns/s1 must be permitted at session start with nobody asked")

	// The attended host: UnattendedBrowserSlotBindings derived no binding for it,
	// so no grant was ever written and acting on it is still denied.
	denied := checker.CheckToolCall(ctx, actPerm(), authz.Inputs{
		Subject:           "u-1",
		Args:              map[string]any{"host": attendedHost},
		AgentSessionRef:   ns + "/" + sessName,
		SlotResourceTypes: []string{browserschema.WebHostType},
	})
	assert.Equal(t, authz.OutcomeDenied, denied.Outcome,
		"an attended site must get no implicit grant")
}

// TestIntegration_UnattendedBrowserGrant_BlocklistRefusesEvenAgainstRealSpiceDB
// is the defensive backstop's own proof: UnattendedBrowserSlotBindings
// refuses a blocklist toolbox's unattended site (the CRD's own XValidation
// rule should already make this combination unreachable; this proves the
// derivation does not ALSO need that rule to hold -- it writes nothing of its
// own accord), so no grant ever reaches SpiceDB and the host stays denied.
func TestIntegration_UnattendedBrowserGrant_BlocklistRefusesEvenAgainstRealSpiceDB(t *testing.T) {
	endpoint := testspicedb.Endpoint(t)
	token := testspicedb.UniqueToken(t)
	testspicedb.WriteSchemaWithExtra(t, endpoint, token, webHostComposedFragment)
	c, err := spicedb.NewClient(endpoint, token, true)
	require.NoError(t, err, "NewClient")
	t.Cleanup(func() { _ = c.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	const ns, sessName = "ns", "s2"
	sess := authz.SessionRef{Namespace: ns, Name: sessName}
	t.Cleanup(func() { _ = c.DeleteSlotGrants(context.Background(), ns, sessName) })

	toolboxes := []spiceboxv1alpha1.ResolvedBrowserToolbox{{
		Name: "browser",
		Spec: spiceboxv1alpha1.BrowserToolboxSpec{
			Access: spiceboxv1alpha1.BrowserAccess{
				Mode:  "blocklist",
				Sites: []spiceboxv1alpha1.BrowserSite{{Host: "app.example.com", Unattended: true}},
			},
		},
	}}

	bindings := runner.UnattendedBrowserSlotBindings(toolboxes, webHostTransforms)
	require.Empty(t, bindings, "a blocklist toolbox must derive no bindings even when a site sets unattended")

	eng := engine.New(engine.Deps{RelWriter: c.Relations(), SessionExpiration: time.Hour})
	require.NoError(t, eng.GrantUnattendedSlots(ctx, sess, bindings), "GrantUnattendedSlots on an empty batch is a no-op")

	checker := toolcheck.Checker{Cli: c}
	denied := checker.CheckToolCall(ctx, actPerm(), authz.Inputs{
		Subject:           "u-1",
		Args:              map[string]any{"host": "app.example.com"},
		AgentSessionRef:   ns + "/" + sessName,
		SlotResourceTypes: []string{browserschema.WebHostType},
	})
	assert.Equal(t, authz.OutcomeDenied, denied.Outcome,
		"a blocklist toolbox's unattended site must never be granted")
}
