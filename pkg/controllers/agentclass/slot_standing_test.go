package agentclass

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

func toolkitDeclaring(name, standing string) spiceboxv1alpha1.SpiceboxToolkit {
	return spiceboxv1alpha1.SpiceboxToolkit{
		Spec: spiceboxv1alpha1.SpiceboxToolkitSpec{
			SpiceDBSchema: &spiceboxv1alpha1.SpiceDBSchemaFragment{
				Resources: []spiceboxv1alpha1.SpiceDBResource{resourceDeclaring(name, standing)},
			},
		},
	}
}

// serverDeclaring is toolkitDeclaring's MCPServer analog. MCPServer fragments
// are the primary carrier of `standing` in production — most SpiceDB resource
// definitions arrive via an MCPServer's spicedbSchema, not a toolkit's — so any
// coverage that only ever passes toolkits misses that half of standingSources.
func serverDeclaring(name, standing string) spiceboxv1alpha1.MCPServer {
	return spiceboxv1alpha1.MCPServer{
		Spec: spiceboxv1alpha1.MCPServerSpec{
			SpiceDBSchema: &spiceboxv1alpha1.SpiceDBSchemaFragment{
				Resources: []spiceboxv1alpha1.SpiceDBResource{resourceDeclaring(name, standing)},
			},
		},
	}
}

// sidecarDeclaring is the SidecarToolbox analog. A sidecar tool's tainted data
// needs its resource type's standing exactly as an MCP tool's does; a sidecar
// fragment left out of standingSources reads as "no standing" and fail-closes
// the per-datum leak approval (observed live in the per-datum-egress demo).
func sidecarDeclaring(name, standing string) spiceboxv1alpha1.SidecarToolbox {
	return spiceboxv1alpha1.SidecarToolbox{
		Spec: spiceboxv1alpha1.SidecarToolboxSpec{
			SpiceDBSchema: &spiceboxv1alpha1.SpiceDBSchemaFragment{
				Resources: []spiceboxv1alpha1.SpiceDBResource{resourceDeclaring(name, standing)},
			},
		},
	}
}

// TestResolveStandingFor_IncludesSidecarToolboxFragments pins that SidecarToolbox
// spicedbSchema fragments contribute standings just like MCPServer/toolkit ones.
// Without this a sidecar-declared type resolves to "no standing", and the leak
// gate — having detected a real leak — fails closed building the approval
// ("declares no standing, so there is no way to know who may vouch").
func TestResolveStandingFor_IncludesSidecarToolboxFragments(t *testing.T) {
	scs := []spiceboxv1alpha1.SidecarToolbox{sidecarDeclaring("pde_record", spiceboxv1alpha1.StandingSessionOnly)}
	standing, _, err := resolveStandingFor("pde_record", nil, nil, scs, nil)
	require.NoError(t, err, "a type declared only by a SidecarToolbox fragment must resolve its standing, not error 'no standing'")
	assert.Equal(t, spiceboxv1alpha1.StandingSessionOnly, standing)

	// And the same type surfaces in the full resolution the controller writes to status.
	out, err := resolveResourceStandings(nil, nil, scs, nil)
	require.NoError(t, err)
	require.Len(t, out, 1)
	assert.Equal(t, "pde_record", out[0].ResourceType)
	assert.Equal(t, spiceboxv1alpha1.StandingSessionOnly, out[0].Standing)
}

func TestResolveStandingFor(t *testing.T) {
	cases := []struct {
		name     string
		declared string
		veto     map[string]struct{}
		want     string
	}{
		{
			name:     "fragment declaring session-only is honored",
			declared: spiceboxv1alpha1.StandingSessionOnly, want: spiceboxv1alpha1.StandingSessionOnly,
		},
		{
			name:     "fragment declaring required is honored",
			declared: spiceboxv1alpha1.StandingRequired, want: spiceboxv1alpha1.StandingRequired,
		},
		{
			// A veto on a type that ALREADY declares required is a no-op confirm.
			// Vetoing a session-only type is a different matter — it names no
			// permission to route to, so the veto is unsatisfiable and refused;
			// see TestResolveStandingFor_VetoOnATypeWithNoApproverPermissionIsRefused.
			name:     "admin veto on an already-required type confirms it",
			declared: spiceboxv1alpha1.StandingRequired,
			veto:     map[string]struct{}{"git_repo": {}},
			want:     spiceboxv1alpha1.StandingRequired,
		},
		{
			name:     "veto on an unrelated type does not apply",
			declared: spiceboxv1alpha1.StandingSessionOnly,
			veto:     map[string]struct{}{"crm_company": {}},
			want:     spiceboxv1alpha1.StandingSessionOnly,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tks := []spiceboxv1alpha1.SpiceboxToolkit{toolkitDeclaring("git_repo", tc.declared)}
			got := standingOf(t, "git_repo", nil, tks, tc.veto)
			assert.Equal(t, tc.want, got)
		})
	}
}

// Two fragments declaring the same type must not resolve to the laxer answer,
// regardless of which order they're supplied in. Composition is global — two
// toolkits can contribute the same resource definition — so resolving to the
// laxer answer would let adding an unrelated toolkit silently weaken a type
// somebody deliberately marked required.
func TestResolveStandingFor_StrictestFragmentWins(t *testing.T) {
	sessionOnlyFirst := []spiceboxv1alpha1.SpiceboxToolkit{
		toolkitDeclaring("git_repo", spiceboxv1alpha1.StandingSessionOnly),
		toolkitDeclaring("git_repo", spiceboxv1alpha1.StandingRequired),
	}
	assert.Equal(t, spiceboxv1alpha1.StandingRequired,
		standingOf(t, "git_repo", nil, sessionOnlyFirst, nil),
		"session-only fragment listed first must not win over a later required fragment")

	requiredFirst := []spiceboxv1alpha1.SpiceboxToolkit{
		toolkitDeclaring("git_repo", spiceboxv1alpha1.StandingRequired),
		toolkitDeclaring("git_repo", spiceboxv1alpha1.StandingSessionOnly),
	}
	assert.Equal(t, spiceboxv1alpha1.StandingRequired,
		standingOf(t, "git_repo", nil, requiredFirst, nil),
		"required fragment listed first must not be weakened by a later session-only fragment")
}

// MCPServer fragments carry standing too — standingSources flattens both CR
// kinds into one list, and every case above only ever passed toolkits, so the
// MCPServer half of that walk (slot_standing.go's standingSources) was never
// exercised. This mirrors TestResolveStandingFor with an MCPServer fragment.
func TestResolveStandingFor_MCPServerFragment(t *testing.T) {
	cases := []struct {
		name     string
		declared string
		want     string
	}{
		{
			name:     "MCPServer fragment declaring required is honored",
			declared: spiceboxv1alpha1.StandingRequired, want: spiceboxv1alpha1.StandingRequired,
		},
		{
			name:     "MCPServer fragment declaring session-only is honored",
			declared: spiceboxv1alpha1.StandingSessionOnly, want: spiceboxv1alpha1.StandingSessionOnly,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srvs := []spiceboxv1alpha1.MCPServer{serverDeclaring("crm_company", tc.declared)}
			got := standingOf(t, "crm_company", srvs, nil, nil)
			assert.Equal(t, tc.want, got)
		})
	}
}

// Strictest-wins must hold ACROSS the two CR kinds, not just within one:
// standingSources flattens MCPServer and SpiceboxToolkit fragments into a
// single list before resolveStandingFor walks it, so a required MCPServer
// fragment must survive a session-only toolkit fragment for the same type,
// and the reverse.
func TestResolveStandingFor_StrictestFragmentWins_AcrossMCPServerAndToolkit(t *testing.T) {
	requiredServer := []spiceboxv1alpha1.MCPServer{serverDeclaring("crm_company", spiceboxv1alpha1.StandingRequired)}
	sessionOnlyToolkit := []spiceboxv1alpha1.SpiceboxToolkit{toolkitDeclaring("crm_company", spiceboxv1alpha1.StandingSessionOnly)}
	assert.Equal(t, spiceboxv1alpha1.StandingRequired,
		standingOf(t, "crm_company", requiredServer, sessionOnlyToolkit, nil),
		"a required MCPServer fragment must not be weakened by a toolkit's session-only fragment for the same type")

	sessionOnlyServer := []spiceboxv1alpha1.MCPServer{serverDeclaring("crm_company", spiceboxv1alpha1.StandingSessionOnly)}
	requiredToolkit := []spiceboxv1alpha1.SpiceboxToolkit{toolkitDeclaring("crm_company", spiceboxv1alpha1.StandingRequired)}
	assert.Equal(t, spiceboxv1alpha1.StandingRequired,
		standingOf(t, "crm_company", sessionOnlyServer, requiredToolkit, nil),
		"a required toolkit fragment must not be weakened by the MCPServer's session-only fragment for the same type")
}

// TestResolveStandingFor_EmbeddedGhToolkit_PullRequestHasStanding is the
// regression test for the defect this corrective task exists to fix: a
// `github_pull_request` type declared only in gh.yaml's RawZed composed fine
// (ComposeBase/ComposeSlots work on rendered schema TEXT, agnostic of which
// half produced it) but was invisible to standingSources, which walks only
// SpiceDBSchema.Resources — the STRUCTURED half. A slot naming
// github_pull_request/write_memory was refused with "declares no standing",
// so no AgentClass could ever reach the resource's memory pool, neither to
// read nor to write.
//
// Drives the REAL refusal path end to end, through the embedded gh toolkit
// (embeddedToolkitAsCR, the same conversion toolkitsForKeying uses for a
// shipped toolkit with no CR on a real cluster — see
// TestToolkitsForKeying_findsAnEmbeddedToolkitWithNoCR, the git analog this
// mirrors) rather than a hand-built fixture resource: a fixture would pass
// whether or not gh.yaml itself declares the type structurally, which is
// exactly the gap that let the defect ship.
func TestResolveStandingFor_EmbeddedGhToolkit_PullRequestHasStanding(t *testing.T) {
	gh, found, err := embeddedToolkitAsCR("gh")
	require.NoError(t, err)
	require.True(t, found, "the gh toolkit must be registered as a built-in")

	standing, _, err := resolveStandingFor("github_pull_request", nil, []spiceboxv1alpha1.SpiceboxToolkit{gh}, nil, nil)
	require.NoError(t, err,
		"github_pull_request must declare standing through gh.yaml's structured spicedbSchema.resources; "+
			"a type left only in rawZed resolves to \"declares no standing\"")
	assert.Equal(t, spiceboxv1alpha1.StandingSessionOnly, standing)

	// And the slot-declaration path a real AgentClass drives: a slot naming
	// github_pull_request/write_memory must be ACCEPTED, not refused for lack
	// of standing — the exact shape 03-agent.yaml's dossier-pool-write-e2e
	// fixture exercises for `dossier`, applied here to the shipped gh toolkit.
	ac := classDeclaringSlots(spiceboxv1alpha1.AuthzSlot{
		ResourceType: "github_pull_request", Description: "a pull request", Permission: "write_memory",
	})
	slots, reason, msg := resolveSlotValueKeying(ac, nil, []spiceboxv1alpha1.SpiceboxToolkit{gh}, nil, nil)
	require.Empty(t, reason, "msg=%s", msg)
	require.Len(t, slots, 1)
	assert.Equal(t, spiceboxv1alpha1.StandingSessionOnly, slots[0].Standing)
}

// resourceDeclaring builds one resource with a COMPLETE standing declaration:
// `required` must name the permission an approver holds, or ValidateStanding
// refuses it. Centralized so a test never accidentally asserts against a
// half-declared resource, which the resolver rejects rather than resolves.
func resourceDeclaring(name, standing string) spiceboxv1alpha1.SpiceDBResource {
	r := spiceboxv1alpha1.SpiceDBResource{Name: name, Standing: standing}
	if standing == spiceboxv1alpha1.StandingRequired {
		r.ApproverPermission = "owner"
	}
	return r
}

// standingOf is the standing half of resolveStandingFor for cases that assert
// only on it. It requires no error, so a case that expects one must call
// resolveStandingFor directly rather than reaching for this.
func standingOf(
	t *testing.T,
	resourceType string,
	srvs []spiceboxv1alpha1.MCPServer,
	tks []spiceboxv1alpha1.SpiceboxToolkit,
	veto map[string]struct{},
) string {
	t.Helper()
	got, _, err := resolveStandingFor(resourceType, srvs, tks, nil, veto)
	require.NoError(t, err)
	return got
}

// An undeclared type is REFUSED, never defaulted. This is the property the whole
// no-default change rests on: guessing session-only silently widens who may
// approve a type nobody classified, and guessing required makes a
// forge-governed type permanently unbindable. Both are silent.
func TestResolveStandingFor_UndeclaredTypeIsRefused(t *testing.T) {
	tks := []spiceboxv1alpha1.SpiceboxToolkit{toolkitDeclaring("git_repo", spiceboxv1alpha1.StandingSessionOnly)}
	_, _, err := resolveStandingFor("crm_company", nil, tks, nil, nil)
	require.Error(t, err, "no fragment declares crm_company, so there is no answer to publish")
	assert.Contains(t, err.Error(), "crm_company", "the message must name the type the author has to fix")
	assert.Contains(t, err.Error(), "no default")
}

// A `required` type carries its approver permission through, because that is
// what the router routes to. Resolving the standing but dropping the permission
// would leave the router with "<type>:<id>#" and no subject-set at all.
func TestResolveStandingFor_CarriesTheApproverPermission(t *testing.T) {
	srvs := []spiceboxv1alpha1.MCPServer{serverDeclaring("crm_company", spiceboxv1alpha1.StandingRequired)}
	standing, perm, err := resolveStandingFor("crm_company", srvs, nil, nil, nil)
	require.NoError(t, err)
	assert.Equal(t, spiceboxv1alpha1.StandingRequired, standing)
	assert.Equal(t, "owner", perm)
}

// A session-only type names no permission, and must not invent one.
func TestResolveStandingFor_SessionOnlyCarriesNoPermission(t *testing.T) {
	tks := []spiceboxv1alpha1.SpiceboxToolkit{toolkitDeclaring("git_repo", spiceboxv1alpha1.StandingSessionOnly)}
	standing, perm, err := resolveStandingFor("git_repo", nil, tks, nil, nil)
	require.NoError(t, err)
	assert.Equal(t, spiceboxv1alpha1.StandingSessionOnly, standing)
	assert.Empty(t, perm, "nothing consults a permission here, so naming one would be governance that never runs")
}

// The admin veto can be UNSATISFIABLE: forcing `required` onto a type that
// names no approverPermission leaves the router no pool, which would dead-end
// every approval for it. Refused out loud rather than written to status.
func TestResolveStandingFor_VetoOnATypeWithNoApproverPermissionIsRefused(t *testing.T) {
	tks := []spiceboxv1alpha1.SpiceboxToolkit{toolkitDeclaring("git_repo", spiceboxv1alpha1.StandingSessionOnly)}
	_, _, err := resolveStandingFor("git_repo", nil, tks, nil, map[string]struct{}{"git_repo": {}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no approverPermission")
	assert.Contains(t, err.Error(), "git_repo")
}
