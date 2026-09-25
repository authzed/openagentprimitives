// pkg/tools/kinds/sidecartoolbox.ValidateFile against this package's own
// declaration file: proves the SidecarToolbox/workshop artifact stays valid
// and keeps declaring exactly the plan-3b tool set as the sidecar's own
// tools_*.go files grow.
package deploy_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	v1 "github.com/authzed/authzed-go/proto/authzed/api/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/yaml"

	agenttool "github.com/authzed/openagentprimitives/pkg/agent/tool"
	mcptool "github.com/authzed/openagentprimitives/pkg/agent/tool/mcp"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/authz/guardian/schema"
	"github.com/authzed/openagentprimitives/pkg/authz/spicedb/toolcheck"
	"github.com/authzed/openagentprimitives/pkg/tools/kinds/sidecartoolbox"
	"github.com/authzed/openagentprimitives/pkg/tools/mcp/probe"
	"github.com/authzed/openagentprimitives/pkg/tools/mcp/validator"
)

// wantTools is the plan-3b tool set (design doc §2.4) plus the plan-4b R2
// test tools (test_tool, read_test_log, stop_test — run_test retired) plus
// the "try it yourself" test tools (test_link, watch_test, test_sessions)
// plus the plan-5a task-3 tool (request_credential) plus the plan-5b task-2
// handoff tools (request_install, recommend_capability) plus the plan-9a
// task-2 reproduce-lookup tool (agents_in_thread) plus the plan-9b task-2
// stand-in-projection tool (project_agent) plus the workshop-close tool
// (close_others).
var wantTools = []string{
	"inventory", "probe_mcp", "probe_image", "cli_help", "validate_spec",
	"apply", "get", "list", "delete",
	"render_summary", "export_draft", "load_draft",
	"test_tool", "test_link", "watch_test", "test_sessions", "read_test_log", "stop_test",
	"request_credential", "request_install", "recommend_capability",
	"agents_in_thread", "project_agent", "close_others",
}

func declarationPath(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	require.NoError(t, err)
	return filepath.Join(wd, "sidecartoolbox.yaml")
}

func TestDeclaration_ValidatesClean(t *testing.T) {
	k := sidecartoolbox.New()
	diags, err := k.ValidateFile(declarationPath(t))
	require.NoError(t, err, "ValidateFile")
	assert.Empty(t, diags, "the workshop declaration must validate with zero diagnostics, got %+v", diags)
}

func TestDeclaration_UpstreamAuthIsNoneSentinel(t *testing.T) {
	data, err := os.ReadFile(declarationPath(t))
	require.NoError(t, err)
	var sp spiceboxv1alpha1.SidecarToolboxSpec
	require.NoError(t, yaml.Unmarshal(data, &sp))
	assert.Equal(t, "none", sp.UpstreamAuth.Provider,
		"the workshop sidecar's credential is the controller-issued SA token + bearer, "+
			"never an AgentIdentity credential (plan-3b Ruling B)")
	assert.Empty(t, sp.UpstreamAuth.EnvVar, "no upstream credential env var is injected")
}

func TestDeclaration_DeclaresExactlyThePlan3bAndTestToolSet(t *testing.T) {
	data, err := os.ReadFile(declarationPath(t))
	require.NoError(t, err)
	var sp spiceboxv1alpha1.SidecarToolboxSpec
	require.NoError(t, yaml.Unmarshal(data, &sp))

	var got []string
	for _, tool := range sp.Tools {
		got = append(got, tool.Name)
	}
	assert.ElementsMatch(t, wantTools, got,
		"the declaration must list exactly the plan-3b tools plus the plan-4b R2 test "+
			"tools plus request_credential plus the plan-5b handoff tools (request_install, "+
			"recommend_capability)")
}

func TestDeclaration_ExternalStateImpactRequiresApproval(t *testing.T) {
	data, err := os.ReadFile(declarationPath(t))
	require.NoError(t, err)
	var sp spiceboxv1alpha1.SidecarToolboxSpec
	require.NoError(t, yaml.Unmarshal(data, &sp))

	wantExternal := map[string]bool{
		"apply": true, "delete": true, "watch_test": true, "request_credential": true,
		"request_install": true, "recommend_capability": true, "project_agent": true,
		"close_others": true,
	}
	for _, tool := range sp.Tools {
		require.NotNil(t, tool.Permission, "tool %q must declare a permission block", tool.Name)
		if wantExternal[tool.Name] {
			assert.Equal(t, "external", string(tool.Permission.StateImpact),
				"%q is a workshop mutation (or hands a human a live agent) and must be re-approved on every call", tool.Name)
			continue
		}
		assert.NotEqual(t, "external", string(tool.Permission.StateImpact),
			"%q must not require per-call approval", tool.Name)
	}
}

// TestDeclaration_TestToolsStateImpact pins the R2 policy for the plan-4b
// test tools (task-6-brief.md) plus the "try it yourself" test tools that
// replaced run_test, and that the declared-tool count rose by exactly six
// test-related tools (test_tool, test_link, watch_test, test_sessions,
// read_test_log, stop_test) plus one (plan-5a task-3's request_credential)
// plus two (plan-5b task-2's request_install and recommend_capability) plus
// one (plan-9a task-2's agents_in_thread) plus one (plan-9b task-2's
// project_agent) over the plan-3b baseline of 12.
//
// test_tool and read_test_log were "readonly" and stop_test was "readwrite"
// until the plan-8c final-review fix (MAJOR-1): every one of those declared
// a check-requiring stateImpact with no permission.check, which the
// runner's real gate (pkg/authz/spicedb/toolcheck/check_tool_call.go:105)
// denies outright — not the admission-side validatePermissionShape this
// test used to cite, which never walks a SidecarToolbox's tools at all. The
// three are now Passthrough, matching sync_workspace's precedent
// (pkg/agent/tool/meta/workspace_tools_test.go): they touch cluster/session
// state but carry no per-call SpiceDB resource id. run_test — which used to
// stay external by deliberate ruling here — is retired: the person now
// starts their own test themselves (test_link/watch_test/test_sessions,
// pkg/tools/workshopmcp/tools_trytest.go), never as a delegated child of the
// builder. watch_test takes over the external slot: it mutates this
// session's own Workshop CR (spec.testWatch), and it is keyed on the draft —
// `perm:test:workshop_draft` — so the phase that tests the agent approves
// re-arming the watch once instead of once per arm. The five passthrough
// tools beside it stay check-less, which is what the loop below separates.
func TestDeclaration_TestToolsStateImpact(t *testing.T) {
	data, err := os.ReadFile(declarationPath(t))
	require.NoError(t, err)
	var sp spiceboxv1alpha1.SidecarToolboxSpec
	require.NoError(t, yaml.Unmarshal(data, &sp))

	const plan3bToolCount = 12
	assert.Len(t, sp.Tools, plan3bToolCount+6+1+2+1+1+1,
		"declared tool count must rise by exactly six test-related tools plus request_credential plus the two "+
			"plan-5b handoff tools plus agents_in_thread plus project_agent plus close_others over the plan-3b baseline")

	wantStateImpact := map[string]string{
		"test_tool":     "passthrough",
		"test_link":     "passthrough",
		"watch_test":    "external",
		"test_sessions": "passthrough",
		"read_test_log": "passthrough",
		"stop_test":     "passthrough",
	}
	byName := make(map[string]spiceboxv1alpha1.MCPServerTool, len(sp.Tools))
	for _, tool := range sp.Tools {
		byName[tool.Name] = tool
	}
	for name, want := range wantStateImpact {
		tool, ok := byName[name]
		require.True(t, ok, "declaration must include tool %q", name)
		require.NotNil(t, tool.Permission, "tool %q must declare a permission block", name)
		assert.Equal(t, want, string(tool.Permission.StateImpact), "%q stateImpact", name)
		if want == "passthrough" {
			assert.Nil(t, tool.Permission.Check, "%q must not declare permission.check: a passthrough tool "+
				"carries no per-call SpiceDB resource id by definition, and CheckRequired() is false for it, "+
				"so the runner gate never asks for one", name)
			continue
		}
		require.NotNil(t, tool.Permission.Check, "%q is external and keyed on the draft, so it must declare a check", name)
		assert.Equal(t, workshopDraftType, tool.Permission.Check.ResourceType, "%q check resource type", name)
	}
}

// specFromDeclaration converts the shipped SidecarToolboxSpec's Tools
// ([]MCPServerTool) into the pkg/tools/mcp/validator runtime spec shape via
// the same JSON round trip production dispatch uses:
// pkg/agent/tool/sidecartoolbox/synthesize.go's synthCR wraps a resolved
// sidecar's Spec.Tools into an MCPServerSpec, and MCPServerSpec.ToSpec
// converts that into an mcpspec.Spec for the validator. Doing the identical
// conversion here means this test exercises the SAME code path a real
// tool call goes through, not a hand-built spec that only resembles it.
func specFromDeclaration(t *testing.T, sp spiceboxv1alpha1.SidecarToolboxSpec) *validator.Decision {
	t.Helper()
	mcpSpec, err := (&spiceboxv1alpha1.MCPServerSpec{Tools: sp.Tools}).ToSpec()
	require.NoError(t, err, "convert declared tools to the validator's runtime spec shape")

	decision, err := validator.Check(mcpSpec, validator.Invocation{
		ToolName: "apply",
		Args:     map[string]any{"manifest": map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": "x"}}},
	})
	require.NoError(t, err, "Check")
	return decision
}

// TestApply_ManifestArgumentIsAllowed is the regression guard for the
// production outage this task fixes: on a real cluster, EVERY workshop tool
// that takes an argument was denied, because deploy/sidecartoolbox.yaml
// declared 21 tools and zero `args` blocks — pkg/tools/mcp/validator's
// checkAllowedFields is fail-closed, so an empty/unset AllowedFields denies
// any argument at all (see that phase's own doc). workshop_apply is the
// tool that creates every CR a builder authors, so this was total: the
// shipped agent builder could report inventory and do nothing else.
//
// Before the fix (args.allowedFields added to the apply entry below), this
// test failed with:
//
//	argument(s) [manifest] denied: tool has no allowedFields configured (set unconstrainedArgs to accept free-form args)
//
// captured verbatim from a run against the unfixed declaration — see this
// task's report. It now asserts the opposite (Allow=true) so a future edit
// that drops or empties apply's allowedFields fails HERE, by name, instead
// of silently reintroducing the outage.
func TestApply_ManifestArgumentIsAllowed(t *testing.T) {
	data, err := os.ReadFile(declarationPath(t))
	require.NoError(t, err)
	var sp spiceboxv1alpha1.SidecarToolboxSpec
	require.NoError(t, yaml.Unmarshal(data, &sp))

	decision := specFromDeclaration(t, sp)
	assert.True(t, decision.Allow, "apply's manifest argument must be allowed by the shipped declaration; got reason %q", decision.Reason)
	assert.Equal(t, "allowed", decision.Reason)
}

// synthesizeDeclaredTool builds one of the shipped declaration's tools
// through mcptool.Synthesize -- the SAME synthesizer
// pkg/agent/tool/sidecartoolbox.Synthesize calls in production -- rather
// than constructing an authz.Permission literal by hand, so this test
// exercises the identical Permission a real runner dispatch would see.
func synthesizeDeclaredTool(t *testing.T, sp spiceboxv1alpha1.SidecarToolboxSpec, name string) agenttool.Tool {
	t.Helper()
	cr := &spiceboxv1alpha1.MCPServer{Spec: spiceboxv1alpha1.MCPServerSpec{Name: sp.Name, Tools: sp.Tools}}
	live := make([]probe.Tool, len(sp.Tools))
	for i, tool := range sp.Tools {
		live[i] = probe.Tool{Name: tool.Name}
	}
	res, err := mcptool.Synthesize(cr, live)
	require.NoError(t, err, "Synthesize")
	for _, tl := range res.LLMTools {
		if tl.Name() == name {
			return tl
		}
	}
	t.Fatalf("declaration does not synthesize a tool named %q", name)
	return nil
}

// TestInventory_RunnerGateAllowsWithNoApproval is MAJOR-1's regression guard
// -- the SECOND production outage this task fixes, distinct from
// TestApply_ManifestArgumentIsAllowed's above. All 21 shipped tools declared
// a stateImpact (readonly/readwrite/external) with NO permission.check. The
// admission-side validatePermissionShape (pkg/controllers/agentclass/
// permission_validation.go) would have caught that -- but it never runs
// against a SidecarToolbox's tools at all: validatePermissions there ranges
// only mcpServers and toolkits, so a per-task review that checked only that
// function concluded this was "accurate rather than a live second defect."
// The gate that actually fires on a real cluster is the RUNNER's:
// pkg/authz/spicedb/toolcheck/check_tool_call.go:105 denies outright
// whenever StateImpact.CheckRequired() is true and Check is nil, and
// loop_dispatch.go:822 routes every such call through it (ToolCallAuthz
// registers whenever toolAuthMode != disabled, and the shipped
// agentclass.yaml sets none, so the default -- enforcing -- applies). Result
// on a real cluster: 13 of the 21 tools were hard-denied with no approval
// card and no path through, `inventory` among them.
//
// This test builds "inventory" the way production does (mcptool.Synthesize
// over the shipped declaration, see synthesizeDeclaredTool above) and drives
// it through the runner's real gate (toolcheck.Checker.CheckToolCall) --
// not a hand-built authz.Permission.
//
// Before the fix (stateImpact: readonly, no check), this failed with:
//
//	internal: stateImpact requires a check but none was supplied
//
// captured verbatim from a run against the unfixed declaration -- see this
// task's report. inventory now declares Passthrough (a tool that touches
// cluster state but names no per-call SpiceDB resource -- see the
// declaration header and this task's report for why Passthrough rather than
// an invented check), so CheckRequired() is false and the gate never asks
// for one. This asserts Allowed, so a future edit that puts a workshop
// meta-tool back on Readonly/Readwrite/External with no check fails HERE,
// by name, instead of silently reintroducing the outage.
func TestInventory_RunnerGateAllowsWithNoApproval(t *testing.T) {
	data, err := os.ReadFile(declarationPath(t))
	require.NoError(t, err)
	var sp spiceboxv1alpha1.SidecarToolboxSpec
	require.NoError(t, yaml.Unmarshal(data, &sp))

	inventory := synthesizeDeclaredTool(t, sp, "inventory")
	assert.Equal(t, authz.Passthrough, inventory.Permission().StateImpact)

	result := (toolcheck.Checker{}).CheckToolCall(context.Background(), inventory.Permission(), authz.Inputs{Subject: "builder"})
	assert.Equal(t, authz.OutcomeAllowed, result.Outcome,
		"inventory must be allowed with no approval/check under the runner's real gate; got denial %q", result.Message)
}

// ── The draft: the instance the workshop's repeatable tools are approved for ──

// draftKeyedTools is the four tools this declaration keys on the session's own
// draft, each with the permission its check names. Repeatable work — a build
// phase applies many times, re-arms a watch, replaces an object it got wrong —
// so one approval naming the draft is what a person should have to give, not
// one card per call.
//
// Written out rather than derived from the file: the whole point of the pin is
// that a future edit which drops a check, or moves one onto a different
// permission, fails HERE by name. Deriving the expectation from the same file
// under test would assert nothing.
var draftKeyedTools = map[string]string{
	"apply":         "change",
	"delete":        "remove",
	"watch_test":    "test",
	"project_agent": "project",
}

// alwaysPromptTools is the other half of the ruling: four external tools that
// deliberately carry NO check, so every call costs a card. Pinned as a set so
// "this tool was left check-less" stays a decision somebody wrote down rather
// than a gap nobody noticed.
var alwaysPromptTools = []string{
	"request_credential", "request_install", "recommend_capability", "close_others",
}

const (
	// workshopDraftType and workshopDraftID are the instance every check above
	// resolves to. The id is a CONSTANT, which is what makes a phase approval
	// able to bind it: a phase can bind only an instance it can name up front.
	// git_repo's `workspace` is the same shape (toolkits/git.yaml).
	workshopDraftType = "workshop_draft"
	workshopDraftID   = "draft"

	// needsApproval is checkExternalSlotGrant's verbatim denial — the one that
	// routes a call to a human card. Distinct from the internal
	// "stateImpact requires a check but none was supplied", which is a
	// misdeclaration and reaches no human at all.
	needsApproval = "external tool: human approval required"
	internalDeny  = "stateImpact requires a check but none was supplied"
)

// draftGrantSpiceDB answers the single question checkExternalSlotGrant asks:
// does this session hold `<type>:<id>#slot_grant_<permission>`? Keyed on the
// composed string so a grant written for one permission cannot accidentally
// answer for another — the property the per-permission relation name exists to
// give (authz.SlotGrantRelationName).
//
// Modelled on pkg/authz/spicedb/toolcheck's own recordingSpiceDB; a
// package-local copy because that one is unexported test code there.
type draftGrantSpiceDB struct {
	mu      sync.Mutex
	granted map[string]bool
	asked   []string
}

func (f *draftGrantSpiceDB) CheckPermission(_ context.Context, in *v1.CheckPermissionRequest) (*v1.CheckPermissionResponse, error) {
	key := in.Resource.ObjectType + ":" + in.Resource.ObjectId + "#" + in.Permission
	f.mu.Lock()
	f.asked = append(f.asked, key+"@"+in.Subject.Object.ObjectType+":"+in.Subject.Object.ObjectId)
	has := f.granted[key]
	f.mu.Unlock()
	ans := v1.CheckPermissionResponse_PERMISSIONSHIP_NO_PERMISSION
	if has {
		ans = v1.CheckPermissionResponse_PERMISSIONSHIP_HAS_PERMISSION
	}
	return &v1.CheckPermissionResponse{Permissionship: ans, CheckedAt: &v1.ZedToken{Token: "zt"}}, nil
}

// grantFor renders the slot-grant tuple a human's approval writes on the draft
// for one permission — the exact key checkExternalSlotGrant looks up.
func grantFor(permission string) string {
	return workshopDraftType + ":" + workshopDraftID + "#" + authz.SlotGrantRelationName(permission)
}

// runnerGate drives one of the shipped declaration's tools through the runner's
// real gate with the grants a session is holding, as
// pkg/agent/runner/loop_dispatch does: the session ref is what the grant leg is
// asked about, and SlotResourceTypes is the class's declared slot set (without
// workshop_draft in it the gate denies before it ever queries — see
// checkExternalSlotGrant).
func runnerGate(t *testing.T, tool string, grants ...string) (authz.Result, *draftGrantSpiceDB) {
	t.Helper()
	return runnerGateWithSlots(t, tool, []string{workshopDraftType}, grants...)
}

// runnerGateWithSlots is runnerGate with the class's declared slot set as a
// parameter, so a test can put the gate in the state a cluster is in BEFORE the
// AgentClass declares the slot — the one input runnerGate fixes.
func runnerGateWithSlots(t *testing.T, tool string, slotTypes []string, grants ...string) (authz.Result, *draftGrantSpiceDB) {
	t.Helper()
	data, err := os.ReadFile(declarationPath(t))
	require.NoError(t, err)
	var sp spiceboxv1alpha1.SidecarToolboxSpec
	require.NoError(t, yaml.Unmarshal(data, &sp))

	cli := &draftGrantSpiceDB{granted: map[string]bool{}}
	for _, g := range grants {
		cli.granted[g] = true
	}
	called := synthesizeDeclaredTool(t, sp, tool)
	res := (toolcheck.Checker{Cli: cli}).CheckToolCall(context.Background(), called.Permission(), authz.Inputs{
		Subject:           "builder",
		AgentSessionRef:   "default/sess-1",
		SlotResourceTypes: slotTypes,
	})
	return res, cli
}

// With nothing approved, a draft-keyed tool must deny the way an external tool
// always has — to a human card.
//
// Two of the three assertions are about telling that denial apart from the two
// other things a denied outcome could mean. The internal
// "stateImpact requires a check but none was supplied" is what a MISDECLARED
// tool gets, and it reaches no human at all, so a lost check would look
// identical in the outcome alone — named explicitly so a future edit that
// loosens the message comparison still fails here. And the QUERY is asserted
// because outcome and message are the same whether the check names the draft
// or nothing at all: what proves this tool has one is that the grant leg was
// asked about `workshop_draft:draft`, on this tool's own permission, as the
// session.
func TestDraftKeyedTools_WithNoGrantRouteToAHumanCard(t *testing.T) {
	for tool, permission := range draftKeyedTools {
		t.Run(tool+": no grant, denied to a human card and not as an internal misdeclaration", func(t *testing.T) {
			res, cli := runnerGate(t, tool)
			assert.Equal(t, authz.OutcomeDenied, res.Outcome)
			assert.Equal(t, needsApproval, res.Message)
			assert.NotContains(t, res.Message, internalDeny,
				"this denial must be the one that raises a card, never the internal one a check-less tool gets")
			assert.Equal(t, []string{grantFor(permission) + "@agentsession:default/sess-1"}, cli.asked,
				"the grant leg is asked about the SESSION, on this tool's own permission")
		})
	}
}

// One approval binds one permission on the draft, and nothing else. This is the
// whole claim of the design — a build phase that approved `change` may apply
// all it likes and still stops at a delete — so it is asserted per tool in both
// directions: its own grant allows, and the neighbouring `change` grant does
// not.
func TestDraftKeyedTools_AGrantAuthorizesItsOwnPermissionAndNoOther(t *testing.T) {
	cases := []struct {
		name  string
		tool  string
		grant string
		want  authz.Outcome
	}{
		{"apply under a change grant: allowed, no card", "apply", "change", authz.OutcomeAllowed},
		{"delete under the remove grant: allowed, no card", "delete", "remove", authz.OutcomeAllowed},
		{"watch_test under the test grant: allowed, no card", "watch_test", "test", authz.OutcomeAllowed},
		{"project_agent under the project grant: allowed, no card", "project_agent", "project", authz.OutcomeAllowed},
		{"delete under only a change grant: denied — changing the draft is not removing from it", "delete", "change", authz.OutcomeDenied},
		{"watch_test under only a change grant: denied — the test phase approves its own permission", "watch_test", "change", authz.OutcomeDenied},
		{"project_agent under only a change grant: denied — rehearsing a hand-off is its own approval", "project_agent", "change", authz.OutcomeDenied},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, _ := runnerGate(t, tc.tool, grantFor(tc.grant))
			assert.Equal(t, tc.want, res.Outcome, "denial was %q", res.Message)
			if tc.want == authz.OutcomeDenied {
				assert.Equal(t, needsApproval, res.Message)
			}
		})
	}
}

// The declaration side of the same fact: each draft-keyed tool resolves to the
// constant instance, on its own permission, with no argument involved. A
// constant resolves from empty args — that is what lets a phase name it before
// any call exists.
func TestDeclaration_DraftKeyedToolsCheckTheConstantInstance(t *testing.T) {
	data, err := os.ReadFile(declarationPath(t))
	require.NoError(t, err)
	var sp spiceboxv1alpha1.SidecarToolboxSpec
	require.NoError(t, yaml.Unmarshal(data, &sp))

	for tool, permission := range draftKeyedTools {
		t.Run(tool+": keys workshop_draft:draft on its own permission", func(t *testing.T) {
			check := synthesizeDeclaredTool(t, sp, tool).Permission().Check
			require.NotNil(t, check, "%q must declare a permission.check", tool)
			assert.Equal(t, workshopDraftType, check.ResourceType)
			assert.Equal(t, permission, check.Permission)
			got, rerr := authz.ResolveResourceID(*check, nil)
			require.NoError(t, rerr, "a constant id must resolve with no arguments at all")
			assert.Equal(t, workshopDraftID, got)
		})
	}
	for _, tool := range alwaysPromptTools {
		t.Run(tool+": carries no check, so every call costs a card", func(t *testing.T) {
			assert.Nil(t, synthesizeDeclaredTool(t, sp, tool).Permission().Check,
				"%q is deliberately always-prompt — see this package's sidecartoolbox.yaml header", tool)
		})
	}
}

// The resource the checks name has to exist, and its standing has to be the one
// that lets the session's own approvers decide: the builder's only participant
// IS the person, and nothing local governs a draft.
func TestDeclaration_DeclaresTheDraftAsASessionOnlyResource(t *testing.T) {
	data, err := os.ReadFile(declarationPath(t))
	require.NoError(t, err)
	var sp spiceboxv1alpha1.SidecarToolboxSpec
	require.NoError(t, yaml.Unmarshal(data, &sp))

	require.NotNil(t, sp.SpiceDBSchema, "the toolbox must declare the resource its checks name")
	require.Len(t, sp.SpiceDBSchema.Resources, 1, "one resource: the draft")
	draft := sp.SpiceDBSchema.Resources[0]
	assert.Equal(t, workshopDraftType, draft.Name)
	assert.Equal(t, spiceboxv1alpha1.StandingSessionOnly, draft.Standing)
	assert.NoError(t, draft.ValidateStanding(), "session-only forbids naming an approver permission")
	assert.Equal(t, []spiceboxv1alpha1.SpiceDBRelation{{Name: "session", SubjectType: "agentsession"}}, draft.Relations)
	assert.Empty(t, sp.SpiceDBSchema.RawZed, "the structured form expresses this definition completely")

	assert.NoError(t, schema.ValidateFragment(sp.SpiceDBSchema),
		"the guardian rejects a bad fragment before it reaches a cluster-wide WriteSchema")
}

// The declaration is only useful if the draft can actually CARRY the four slot
// grants: composition replaces each permission line with
// slot_grant_<perm>->interact, and a definition that cannot take that arm would
// fail at WriteSchema, cluster-wide, long after this file was edited.
//
// The pairs are DERIVED from the declaration's own checks rather than
// transcribed, so this cannot drift from the four tools above; the AgentClass
// that declares the matching slots is wired separately.
func TestDeclaration_TheDraftComposesEverySlotItsChecksName(t *testing.T) {
	data, err := os.ReadFile(declarationPath(t))
	require.NoError(t, err)
	var sp spiceboxv1alpha1.SidecarToolboxSpec
	require.NoError(t, yaml.Unmarshal(data, &sp))

	var pairs []schema.SlotPair
	for _, tool := range sp.Tools {
		if tool.Permission == nil || tool.Permission.Check == nil {
			continue
		}
		pairs = append(pairs, schema.SlotPair{
			ResourceType: tool.Permission.Check.ResourceType,
			Permission:   tool.Permission.Check.Permission,
		})
	}
	require.Len(t, pairs, len(draftKeyedTools), "every declared check is a slot the class can grant")

	composed, err := schema.ComposeAll([]schema.IdentifiedFragment{{Key: "workshop", Fragment: sp.SpiceDBSchema}}, nil, nil)
	require.NoError(t, err, "compose the fragment over the base scaffold")
	withSlots, changed, skipped, err := schema.ComposeSlots(composed, pairs)
	require.NoError(t, err)
	assert.True(t, changed, "every pair names a declared type, so composition must rewrite the definition")
	assert.Empty(t, skipped, "a skipped slot is a grant nobody can ever write")
	assert.NoError(t, schema.ValidateComposedSchema(withSlots),
		"the composed schema is what SpiceDB is asked to accept for the whole cluster")
	// Asserted against the draft's OWN definition block, not the whole composed
	// schema: a miss there prints every definition in the cluster, which is a
	// failure message nobody reads.
	block := draftDefinition(t, withSlots)
	for _, permission := range draftKeyedTools {
		assert.Contains(t, block, "permission "+permission+" = "+authz.SlotGrantRelationName(permission)+"->interact",
			"a session-only draft has no local standing, so the composed arm is the grant alone")
	}
}

// draftDefinition cuts the `definition workshop_draft { … }` block out of a
// composed schema, so an assertion about the draft fails with the draft's own
// text rather than the whole cluster's.
func draftDefinition(t *testing.T, composed string) string {
	t.Helper()
	start := strings.Index(composed, "definition "+workshopDraftType+" {")
	require.GreaterOrEqual(t, start, 0, "the composed schema must declare %q", workshopDraftType)
	end := strings.Index(composed[start:], "\n}")
	require.GreaterOrEqual(t, end, 0, "the %q definition must be closed", workshopDraftType)
	return composed[start : start+end+2]
}

// Each of the four permissions the draft's checks key on declares its own
// prose, and both halves reach an AgentClass's status through
// resolvePermissionTitles.
//
// Without them the walk publishes NOTHING for workshop_draft, because this
// toolbox is the type's only declarer: the approval card falls back to
// detokenizing `perm:change:workshop_draft` at the person deciding, and
// PlanningNotesOf has no line to give the builder while it writes the phase.
// toolkits/git.yaml is the precedent — permissions declared with an inert expr
// purely to carry `title` + `planningNote`.
//
// `expr` is INERT here and that is deliberate. schema.ComposeSlots replaces the
// expression of every permission an AgentClass slots with
// slot_grant_<permission>->interact, so what is written here survives only for
// a class that failed to slot the permission — and `session` is a relation
// nothing ever writes a tuple for, so the uncomposed form resolves to nobody.
// The composer owns the EXPR; the prose is published exactly as declared.
func TestDeclaration_TheDraftsPermissionsCarryTheirOwnWords(t *testing.T) {
	data, err := os.ReadFile(declarationPath(t))
	require.NoError(t, err)
	var sp spiceboxv1alpha1.SidecarToolboxSpec
	require.NoError(t, yaml.Unmarshal(data, &sp))

	require.NotNil(t, sp.SpiceDBSchema)
	require.Len(t, sp.SpiceDBSchema.Resources, 1)
	declared := map[string]spiceboxv1alpha1.SpiceDBPermission{}
	for _, p := range sp.SpiceDBSchema.Resources[0].Permissions {
		declared[p.Name] = p
	}

	// Keyed off draftKeyedTools so the set cannot drift from the checks: a
	// fifth tool keyed on a new permission fails here until its words exist.
	require.Len(t, declared, len(draftKeyedTools),
		"one declaration per permission the toolbox's own checks key on")
	for tool, permission := range draftKeyedTools {
		p, ok := declared[permission]
		require.True(t, ok, "%q keys %q and nothing declares its words", tool, permission)
		assert.Equal(t, "session", p.Expr,
			"the expr is inert: the composer replaces it for a slotted permission, and this relation carries no tuples")
		assert.NotEmpty(t, p.Title, "%q must carry the phrase a card shows in place of its handle", permission)
		assert.NotEmpty(t, p.PlanningNote, "%q must carry the line the builder reads while planning", permission)
	}

	// The one note whose exact content is load-bearing: a phase listing the
	// handle and NOT declaring the slot is refused outright
	// (plangate.MissingSlotDeclarations), and a phase declaring the slot with
	// no handle binds `change` silently (boundPermissionsFor's declared[0]
	// fallback). A planner told only "name the handle" hits the first on its
	// first update_plan of every build.
	assert.Equal(t, "Change this workshop's draft (any of its definitions)", declared["change"].Title)
	assert.Contains(t, declared["change"].PlanningNote, "workshop_draft slot",
		"the change note must name the slot the phase declares beside the handle")

	assert.NoError(t, schema.ValidateFragment(sp.SpiceDBSchema),
		"an inert expr still has to compile and resolve: `session` is a relation this definition declares")
}

// The state a cluster is in BEFORE the AgentClass declares the slot: every
// draft-keyed tool denies to a human card, and SpiceDB is never asked.
//
// This is the half of checkExternalSlotGrant nothing else pins, because every
// other row here passes the declared set. A class with no `workshop_draft` slot
// cannot carry a grant for it — the composed schema has no
// slot_grant_<permission> relation to write one into — so the gate refuses
// BEFORE the query rather than asking a question whose answer could only be no.
// The empty `asked` list is the assertion that matters: outcome and message
// alone read identically to a grant that was asked for and denied, and those
// are the two states an upgrade moves between.
func TestDraftKeyedTools_WithNoSlotDeclaredDenyWithoutAskingSpiceDB(t *testing.T) {
	for tool := range draftKeyedTools {
		t.Run(tool+": no slot on the class, denied to a card and SpiceDB untouched", func(t *testing.T) {
			res, cli := runnerGateWithSlots(t, tool, nil)
			assert.Equal(t, authz.OutcomeDenied, res.Outcome)
			assert.Equal(t, needsApproval, res.Message,
				"the person still gets a card; what they lost is the ability to approve the phase once instead")
			assert.Empty(t, cli.asked,
				"an undeclared slot type is refused before the grant leg runs, so no query is made")
		})
	}
}

// The symmetric direction of the isolation claim: `apply` is the tool the whole
// design exists for, and a grant for a NEIGHBOURING permission on the same
// instance must not satisfy it.
//
// TestDraftKeyedTools_AGrantAuthorizesItsOwnPermissionAndNoOther already runs
// the other three tools against a `change` grant; apply itself was the one tool
// never run against a foreign grant, so "change satisfies everything" would
// have passed every row. `remove` is the pointed choice: it is the permission a
// delete holds, so this is the claim that approving the teardown of a draft
// does not silently buy the authority to rewrite it.
func TestApply_UnderANeighbouringGrantIsStillDenied(t *testing.T) {
	for _, grant := range []string{"remove", "test", "project"} {
		t.Run("apply under only a "+grant+" grant: denied — each permission is approved on its own", func(t *testing.T) {
			res, _ := runnerGate(t, "apply", grantFor(grant))
			assert.Equal(t, authz.OutcomeDenied, res.Outcome)
			assert.Equal(t, needsApproval, res.Message)
		})
	}
}
