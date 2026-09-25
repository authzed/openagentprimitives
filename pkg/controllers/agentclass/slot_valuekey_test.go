package agentclass

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/toolkits"
)

func classDeclaringSlots(slots ...spiceboxv1alpha1.AuthzSlot) *spiceboxv1alpha1.AgentClass {
	return &spiceboxv1alpha1.AgentClass{
		Spec: spiceboxv1alpha1.AgentClassSpec{
			Authz: &spiceboxv1alpha1.AuthzBlock{Slots: slots},
		},
	}
}

func exprTool(name, resourceType, perm string, transforms ...string) spiceboxv1alpha1.MCPServerTool {
	return spiceboxv1alpha1.MCPServerTool{
		Name: name,
		Permission: &authz.Permission{
			StateImpact: authz.Readwrite,
			Check: &authz.PermissionCheck{
				ResourceType:         resourceType,
				ResourceIDExpr:       "args.url",
				ResourceIDTransforms: transforms,
				Permission:           perm,
			},
		},
	}
}

// serverWith builds an MCPServer carrying the given tools AND a schema fragment
// declaring every resource type those tools check.
//
// The fragment is not decoration. Standing has no default, so a type no
// fragment declares is refused at resolve time — which is correct in production
// (nobody should be able to omit who may approve a type) and means a fixture
// whose tools check a type must say something about it. session-only is the
// declaration that matches these fixtures: they exercise value KEYING, not
// approval routing, and none of them seeds a tuple for anyone to hold.
func serverWith(name string, tools ...spiceboxv1alpha1.MCPServerTool) spiceboxv1alpha1.MCPServer {
	srv := spiceboxv1alpha1.MCPServer{}
	srv.Name = name
	srv.Spec.Tools = tools

	seen := map[string]bool{}
	var resources []spiceboxv1alpha1.SpiceDBResource
	declare := func(rt string) {
		if rt == "" || seen[rt] {
			return
		}
		seen[rt] = true
		resources = append(resources, spiceboxv1alpha1.SpiceDBResource{
			Name: rt, Standing: spiceboxv1alpha1.StandingSessionOnly,
		})
	}
	for _, tl := range tools {
		if tl.Permission != nil && tl.Permission.Check != nil {
			declare(tl.Permission.Check.ResourceType)
		}
	}
	if len(resources) > 0 {
		srv.Spec.SpiceDBSchema = &spiceboxv1alpha1.SpiceDBSchemaFragment{Resources: resources}
	}
	return srv
}

func TestResolveSlotValueKeying_PublishesTheChainTheToolUses(t *testing.T) {
	ac := classDeclaringSlots(spiceboxv1alpha1.AuthzSlot{
		ResourceType: "http_target", Description: "a URL", Permission: "reachable",
	})
	servers := []spiceboxv1alpha1.MCPServer{
		serverWith("net", exprTool("curl", "http_target", "reachable", "normalize_url", "sha256")),
	}

	got, reason, msg := resolveSlotValueKeying(ac, servers, nil, nil, nil)
	require.Empty(t, reason, "msg=%s", msg)
	require.Len(t, got, 1)
	assert.Equal(t, "http_target", got[0].ResourceType)
	assert.Equal(t, "reachable", got[0].Permission)
	assert.Equal(t, []string{"normalize_url", "sha256"}, got[0].ValueTransforms,
		"a grant writer must be able to mint the id the tool will Check")
}

// TestResolveSlotValueKeying_DivergentToolsRejectTheClass is the rule worth
// having. Two tools keying one type differently means the same URL is two
// different resources, so a grant written for one call cannot match the other —
// and the symptom a human sees is "approval randomly does not work". There is
// no safe way to pick between them.
func TestResolveSlotValueKeying_DivergentToolsRejectTheClass(t *testing.T) {
	ac := classDeclaringSlots(spiceboxv1alpha1.AuthzSlot{
		ResourceType: "http_target", Description: "a URL", Permission: "reachable",
	})
	servers := []spiceboxv1alpha1.MCPServer{
		serverWith("net",
			exprTool("curl", "http_target", "reachable", "normalize_url", "sha256"),
			exprTool("fetch", "http_target", "reachable", "sha256"),
		),
	}

	_, reason, msg := resolveSlotValueKeying(ac, servers, nil, nil, nil)
	assert.Equal(t, spiceboxv1alpha1.ReasonSlotDeclarationInvalid, reason)
	assert.Contains(t, msg, "two tools key the same value differently")
	assert.Contains(t, msg, "curl")
	assert.Contains(t, msg, "fetch")
}

func TestResolveSlotValueKeying_IdenticalChainsAcrossToolsAreFine(t *testing.T) {
	ac := classDeclaringSlots(spiceboxv1alpha1.AuthzSlot{
		ResourceType: "http_target", Description: "a URL", Permission: "reachable",
	})
	servers := []spiceboxv1alpha1.MCPServer{
		serverWith("net", exprTool("curl", "http_target", "reachable", "normalize_url", "sha256")),
		serverWith("net2", exprTool("wget", "http_target", "reachable", "normalize_url", "sha256")),
	}

	got, reason, _ := resolveSlotValueKeying(ac, servers, nil, nil, nil)
	require.Empty(t, reason, "agreeing tools must not be a conflict")
	require.Len(t, got, 1)
	assert.Equal(t, []string{"normalize_url", "sha256"}, got[0].ValueTransforms)
}

// A variant can key the type differently from its base, and validatePermissions
// has historically walked only tool.Permission — so this is exactly where a
// divergence would otherwise go unnoticed.
func TestResolveSlotValueKeying_VariantsParticipate(t *testing.T) {
	tool := exprTool("curl", "http_target", "reachable", "normalize_url", "sha256")
	tool.PermissionVariants = []authz.PermissionVariant{{
		When: `args.method == "POST"`,
		Check: authz.Permission{
			StateImpact: authz.Readwrite,
			Check: &authz.PermissionCheck{
				ResourceType: "http_target", ResourceIDExpr: "args.url",
				ResourceIDTransforms: []string{"sha256"}, Permission: "reachable",
			},
		},
	}}
	ac := classDeclaringSlots(spiceboxv1alpha1.AuthzSlot{
		ResourceType: "http_target", Description: "a URL", Permission: "reachable",
	})

	_, reason, msg := resolveSlotValueKeying(ac, []spiceboxv1alpha1.MCPServer{serverWith("net", tool)}, nil, nil, nil)
	assert.Equal(t, spiceboxv1alpha1.ReasonSlotDeclarationInvalid, reason,
		"a variant that keys the same type differently must be caught")
	assert.Contains(t, msg, "two tools key the same value differently")
}

func TestResolveSlotValueKeying_NonValueSlotsPublishNoChain(t *testing.T) {
	ac := classDeclaringSlots(spiceboxv1alpha1.AuthzSlot{
		ResourceType: "tracker_issue", Description: "an issue", Permission: "write",
	})
	// A template-keyed tool: its id is a named argument that is already a
	// distinct resource, so there is no value→id mapping to reproduce.
	tool := spiceboxv1alpha1.MCPServerTool{
		Name: "update_issue",
		Permission: &authz.Permission{
			StateImpact: authz.Readwrite,
			Check: &authz.PermissionCheck{
				ResourceType: "tracker_issue", ResourceIDTemplate: "{issueId}",
				ResourceIDTransforms: []string{"lowercase"}, Permission: "write",
			},
		},
	}

	got, reason, _ := resolveSlotValueKeying(ac, []spiceboxv1alpha1.MCPServer{serverWith("tracker", tool)}, nil, nil, nil)
	require.Empty(t, reason)
	require.Len(t, got, 1)
	assert.Empty(t, got[0].ValueTransforms,
		"a template-keyed slot is not value-keyed; publishing lowercase here would be a lie")
}

func TestResolveSlotValueKeying_NoSlots_NothingPublished(t *testing.T) {
	got, reason, _ := resolveSlotValueKeying(&spiceboxv1alpha1.AgentClass{}, nil, nil, nil, nil)
	assert.Empty(t, reason)
	assert.Empty(t, got)
}

func TestResolveSlotValueKeying_SortedForStableStatus(t *testing.T) {
	ac := classDeclaringSlots(
		spiceboxv1alpha1.AuthzSlot{ResourceType: "zeta_thing", Description: "z", Permission: "read"},
		spiceboxv1alpha1.AuthzSlot{ResourceType: "alpha_thing", Description: "a", Permission: "read"},
	)
	got, reason, _ := resolveSlotValueKeying(ac, declaringTypes("zeta_thing", "alpha_thing"), nil, nil, nil)
	require.Empty(t, reason)
	require.Len(t, got, 2)
	assert.Equal(t, "alpha_thing", got[0].ResourceType,
		"unstable ordering would rewrite status on every reconcile")
	assert.Equal(t, "zeta_thing", got[1].ResourceType)
}

// toolkitWith is serverWith's SpiceboxToolkit analog, and declares a schema
// fragment for every resource type its subcommands check for the same reason:
// standing has no default, so an undeclared type is refused rather than
// resolved. session-only matches these fixtures, which exercise value keying
// and seed no tuples for anyone to hold.
func toolkitWith(name string, subs ...spiceboxv1alpha1.ToolkitSubcommand) spiceboxv1alpha1.SpiceboxToolkit {
	tk := spiceboxv1alpha1.SpiceboxToolkit{}
	tk.Name = name
	tk.Spec.Name = name
	tk.Spec.Subcommands = subs

	seen := map[string]bool{}
	var resources []spiceboxv1alpha1.SpiceDBResource
	for _, sc := range subs {
		if sc.Permission == nil || sc.Permission.Check == nil {
			continue
		}
		rt := sc.Permission.Check.ResourceType
		if rt == "" || seen[rt] {
			continue
		}
		seen[rt] = true
		resources = append(resources, spiceboxv1alpha1.SpiceDBResource{
			Name: rt, Standing: spiceboxv1alpha1.StandingSessionOnly,
		})
	}
	if len(resources) > 0 {
		tk.Spec.SpiceDBSchema = &spiceboxv1alpha1.SpiceDBSchemaFragment{Resources: resources}
	}
	return tk
}

func exprSub(path []string, resourceType, perm string, transforms ...string) spiceboxv1alpha1.ToolkitSubcommand {
	return spiceboxv1alpha1.ToolkitSubcommand{
		Path: path,
		Permission: &authz.Permission{
			StateImpact: authz.Readwrite,
			Check: &authz.PermissionCheck{
				ResourceType:         resourceType,
				ResourceIDExpr:       "args.repository",
				ResourceIDTransforms: transforms,
				Permission:           perm,
			},
		},
	}
}

// The live gap. git/gh are TOOLKITS, not MCPServers, so walking only mcpServers
// left every toolkit-keyed slot with an empty published chain — and an empty
// chain is not "no opinion", it is read as "the raw value IS the object id".
//
// The consequence is the silent drift this whole derivation exists to prevent:
// a thread seed writes the git_repo grant on `https://github.com/o/n` while
// `git clone` Checks `spicedb_object_id(normalize_url(...))`. The grant is
// written, never matched, and nothing errors.
func TestResolveSlotValueKeying_ToolkitSubcommandsPublishTheirChain(t *testing.T) {
	ac := classDeclaringSlots(spiceboxv1alpha1.AuthzSlot{
		ResourceType: "git_repo", Description: "a repository", Permission: "fetch",
	})
	toolkits := []spiceboxv1alpha1.SpiceboxToolkit{
		toolkitWith("git", exprSub([]string{"clone"}, "git_repo", "fetch", "normalize_url", "sha256")),
	}

	got, reason, msg := resolveSlotValueKeying(ac, nil, toolkits, nil, nil)
	require.Empty(t, reason, "msg=%s", msg)
	require.Len(t, got, 1)
	assert.Equal(t, []string{"normalize_url", "sha256"}, got[0].ValueTransforms,
		"a toolkit keys a slot exactly as an MCP tool does; the source it came from is not a reason to publish nothing")
}

// A toolkit and an MCP tool are two writers of one object id. Whether they
// disagree ACROSS that boundary is precisely the case a per-source walk cannot
// see, so it is the one worth asserting.
func TestResolveSlotValueKeying_ToolkitDisagreeingWithAnMCPToolRejectsTheClass(t *testing.T) {
	ac := classDeclaringSlots(spiceboxv1alpha1.AuthzSlot{
		ResourceType: "git_repo", Description: "a repository", Permission: "fetch",
	})
	servers := []spiceboxv1alpha1.MCPServer{
		serverWith("codehost", exprTool("get_repo", "git_repo", "fetch", "normalize_url", "sha256")),
	}
	toolkits := []spiceboxv1alpha1.SpiceboxToolkit{
		toolkitWith("git", exprSub([]string{"clone"}, "git_repo", "fetch", "spicedb_object_id")),
	}

	_, reason, msg := resolveSlotValueKeying(ac, servers, toolkits, nil, nil)
	assert.Equal(t, spiceboxv1alpha1.ReasonSlotDeclarationInvalid, reason,
		"one value becoming two object ids must not be runnable, whichever source declared it")
	assert.Contains(t, msg, "get_repo")
	assert.Contains(t, msg, "git")
}

func TestResolveSlotValueKeying_ToolkitVariantsParticipate(t *testing.T) {
	sub := exprSub([]string{"api"}, "github_repo", "read", "normalize_url", "sha256")
	sub.PermissionVariants = []authz.PermissionVariant{{
		When: `args.method == "GET"`,
		Check: authz.Permission{
			StateImpact: authz.Readonly,
			Check: &authz.PermissionCheck{
				ResourceType: "github_repo", ResourceIDExpr: "args.endpoint",
				ResourceIDTransforms: []string{"spicedb_object_id"}, Permission: "read",
			},
		},
	}}
	ac := classDeclaringSlots(spiceboxv1alpha1.AuthzSlot{
		ResourceType: "github_repo", Description: "a repo", Permission: "read",
	})

	_, reason, msg := resolveSlotValueKeying(ac, nil, []spiceboxv1alpha1.SpiceboxToolkit{toolkitWith("gh", sub)}, nil, nil)
	assert.Equal(t, spiceboxv1alpha1.ReasonSlotDeclarationInvalid, reason,
		"a toolkit variant keying its own type differently is the same defect as an MCP variant doing so")
	assert.Contains(t, msg, "two tools key the same value differently")
}

// `git add` keys git_repo as the literal "workspace" via resourceIDTemplate —
// no value→id mapping at all. Template-keyed checks are excluded by design, and
// comparing one against an expr-keyed sibling reads as a conflict that is not
// there. Asserting it keeps that exclusion from being "fixed" into a false
// rejection of every real toolkit.
func TestResolveSlotValueKeying_TemplateKeyedToolkitSubcommandIsNotAConflict(t *testing.T) {
	workspaceSub := spiceboxv1alpha1.ToolkitSubcommand{
		Path: []string{"add"},
		Permission: &authz.Permission{
			StateImpact: authz.Readwrite,
			Check: &authz.PermissionCheck{
				ResourceType: "git_repo", ResourceIDTemplate: "workspace", Permission: "write",
			},
		},
	}
	ac := classDeclaringSlots(spiceboxv1alpha1.AuthzSlot{
		ResourceType: "git_repo", Description: "a repository", Permission: "fetch",
	})
	toolkits := []spiceboxv1alpha1.SpiceboxToolkit{
		toolkitWith("git", workspaceSub,
			exprSub([]string{"clone"}, "git_repo", "fetch", "normalize_url", "sha256")),
	}

	got, reason, msg := resolveSlotValueKeying(ac, nil, toolkits, nil, nil)
	require.Empty(t, reason, "a template-keyed sibling is not a competing keying; msg=%s", msg)
	require.Len(t, got, 1)
	assert.Equal(t, []string{"normalize_url", "sha256"}, got[0].ValueTransforms,
		"the expr-keyed check still establishes the chain")
}

// declaringTypes builds a bare MCPServer fragment that declares the given
// resource types as session-only and nothing else.
//
// For tests whose subject is something OTHER than standing — ordering, chain
// publication — but which still need each type classified, because standing has
// no default and an undeclared type is refused before those properties are ever
// reached.
func declaringTypes(types ...string) []spiceboxv1alpha1.MCPServer {
	resources := make([]spiceboxv1alpha1.SpiceDBResource, 0, len(types))
	for _, t := range types {
		resources = append(resources, spiceboxv1alpha1.SpiceDBResource{
			Name: t, Standing: spiceboxv1alpha1.StandingSessionOnly,
		})
	}
	srv := spiceboxv1alpha1.MCPServer{}
	srv.Name = "declaring"
	srv.Spec.SpiceDBSchema = &spiceboxv1alpha1.SpiceDBSchemaFragment{Resources: resources}
	return []spiceboxv1alpha1.MCPServer{srv}
}

// Every BUILTIN toolkit must key each resource type ONE way.
//
// A type keyed two ways cannot carry a slot: the same repository becomes two
// object ids depending on which subcommand ran, so a grant written for one call
// does not match the other and the approval silently buys nothing. That is
// checked per-class at resolve time, but only for types some class declares as
// a slot — so an inconsistency sits latent in a shipped toolkit until the day
// someone declares the slot, and then it surfaces as a mysteriously
// unbindable grant rather than as a toolkit defect.
//
// github_repo shipped exactly like that: 18 subcommands on [lowercase], 3 on
// [spicedb_escape], 1 on [spicedb_object_id]. Harmless while nothing declared
// the slot; load-bearing the moment codebot did.
func TestBuiltinToolkits_KeyEachResourceTypeConsistently(t *testing.T) {
	for _, tk := range toolkits.All() {
		if tk.SpiceDBSchema == nil {
			continue
		}
		chains := map[string][]string{}
		firstRef := map[string]string{}
		for _, sub := range tk.Subcommands {
			for _, chk := range checksFrom(sub.Permission, sub.PermissionVariants) {
				rt := chk.ResourceType
				// Only checks that DERIVE an id from arguments need agree. A
				// constant template (git_repo:"workspace",
				// github_repo_url:"search") names one fixed object and has
				// nothing to normalize, so whatever chain it carries — gh's
				// sentinels carry the type's chain, which passes them through
				// untouched — is not in conflict with anything.
				derives := chk.ResourceIDExpr != "" || strings.Contains(chk.ResourceIDTemplate, "{")
				if rt == "" || !derives {
					continue
				}
				ref := tk.Name + " " + strings.Join(sub.Path, " ")
				prev, seen := chains[rt]
				if !seen {
					chains[rt] = append([]string(nil), chk.ResourceIDTransforms...)
					firstRef[rt] = ref
					continue
				}
				assert.Equal(t, prev, chk.ResourceIDTransforms,
					"%s keys %s as %v but %s keys it as %v — one value would become two object ids",
					firstRef[rt], rt, prev, ref, chk.ResourceIDTransforms)
			}
		}
	}
}

// sidecarWith is serverWith's SidecarToolbox analog: the toolbox carries the
// given tools AND a fragment declaring every resource type they check, because
// standing has no default and an undeclared type is refused before any keying
// property is reached.
func sidecarWith(name string, tools ...spiceboxv1alpha1.MCPServerTool) spiceboxv1alpha1.SidecarToolbox {
	sc := spiceboxv1alpha1.SidecarToolbox{}
	sc.Name = name
	sc.Spec.Tools = tools

	seen := map[string]bool{}
	var resources []spiceboxv1alpha1.SpiceDBResource
	for _, tl := range tools {
		for _, chk := range checksFrom(tl.Permission, tl.PermissionVariants) {
			if chk.ResourceType == "" || seen[chk.ResourceType] {
				continue
			}
			seen[chk.ResourceType] = true
			resources = append(resources, spiceboxv1alpha1.SpiceDBResource{
				Name: chk.ResourceType, Standing: spiceboxv1alpha1.StandingSessionOnly,
			})
		}
	}
	if len(resources) > 0 {
		sc.Spec.SpiceDBSchema = &spiceboxv1alpha1.SpiceDBSchemaFragment{Resources: resources}
	}
	return sc
}

// A SidecarToolbox's tools key a slot exactly as an MCPServer's do —
// SidecarToolboxSpec.Tools IS []MCPServerTool, same checks, same transforms —
// so a walk that skips them republishes the gap toolkits already hit once: an
// EMPTY chain is not "no opinion", it is read downstream (threadseed, the
// approval backfill) as "the raw value IS the object id". The grant is then
// written on the raw value while the tool Checks the transformed one: written,
// never matched, nothing errors.
func TestResolveSlotValueKeying_SidecarToolboxToolsPublishTheirChain(t *testing.T) {
	ac := classDeclaringSlots(spiceboxv1alpha1.AuthzSlot{
		ResourceType: "http_target", Description: "a URL", Permission: "reachable",
	})
	sidecars := []spiceboxv1alpha1.SidecarToolbox{
		sidecarWith("probe", exprTool("fetch", "http_target", "reachable", "normalize_url", "sha256")),
	}

	got, reason, msg := resolveSlotValueKeying(ac, nil, nil, sidecars, nil)
	require.Empty(t, reason, "msg=%s", msg)
	require.Len(t, got, 1)
	assert.Equal(t, []string{"normalize_url", "sha256"}, got[0].ValueTransforms,
		"a sidecar tool mints the object id the same way an MCP tool does; the CR kind it arrived in is not a reason to publish nothing")
}

// A sidecar tool and an MCP tool are two writers of one object id, and whether
// they disagree ACROSS that boundary is exactly what a per-source walk cannot
// see. One value becoming two object ids must not be runnable, whichever kind
// declared it.
func TestResolveSlotValueKeying_SidecarDisagreeingWithAnMCPToolRejectsTheClass(t *testing.T) {
	ac := classDeclaringSlots(spiceboxv1alpha1.AuthzSlot{
		ResourceType: "http_target", Description: "a URL", Permission: "reachable",
	})
	servers := []spiceboxv1alpha1.MCPServer{
		serverWith("net", exprTool("curl", "http_target", "reachable", "normalize_url", "sha256")),
	}
	sidecars := []spiceboxv1alpha1.SidecarToolbox{
		sidecarWith("probe", exprTool("fetch", "http_target", "reachable", "sha256")),
	}

	_, reason, msg := resolveSlotValueKeying(ac, servers, nil, sidecars, nil)
	assert.Equal(t, spiceboxv1alpha1.ReasonSlotDeclarationInvalid, reason,
		"a sidecar keying the type differently from an MCP tool is the same defect as two MCP tools doing so")
	assert.Contains(t, msg, "curl")
	assert.Contains(t, msg, "fetch")
}

// TestResolveSlotValueKeying_TriggerInstanceCompileErrorRejectsTheClass proves
// admission-time compilation: a slot whose triggerInstance fails to compile
// must not reach a class marked ready, since that failure would otherwise
// surface only at delivery time, per-webhook, fail-closed and silent.
func TestResolveSlotValueKeying_TriggerInstanceCompileErrorRejectsTheClass(t *testing.T) {
	ac := classDeclaringSlots(spiceboxv1alpha1.AuthzSlot{
		ResourceType: "pull_request", Description: "a pull request", Permission: "reachable",
		TriggerInstance: "payload.(",
	})
	servers := []spiceboxv1alpha1.MCPServer{
		serverWith("gh", exprTool("comment", "pull_request", "reachable")),
	}

	_, reason, msg := resolveSlotValueKeying(ac, servers, nil, nil, nil)
	assert.Equal(t, spiceboxv1alpha1.ReasonSlotDeclarationInvalid, reason)
	assert.Contains(t, msg, "pull_request")
	assert.Contains(t, msg, "triggerInstance")
}

// TestResolveSlotValueKeying_TriggerInstanceCompilesIsAccepted is the
// companion case: a slot whose triggerInstance compiles cleanly does not
// refuse the class over it.
func TestResolveSlotValueKeying_TriggerInstanceCompilesIsAccepted(t *testing.T) {
	ac := classDeclaringSlots(spiceboxv1alpha1.AuthzSlot{
		ResourceType: "pull_request", Description: "a pull request", Permission: "reachable",
		TriggerInstance: "payload.pull_request.node_id",
	})
	servers := []spiceboxv1alpha1.MCPServer{
		serverWith("gh", exprTool("comment", "pull_request", "reachable")),
	}

	got, reason, msg := resolveSlotValueKeying(ac, servers, nil, nil, nil)
	require.Empty(t, reason, "msg=%s", msg)
	require.Len(t, got, 1)
}

// TestResolveSlotValueKeying_ReviewbotShapedClass_GithubPullRequestPublishesNoTransformChain
// is the transform-skew verification the reviewbot demo depends on: a
// reviewbot-shaped class — a slot on github_pull_request/write_memory,
// fillFrom trigger, the exact declaration examples/reviewbot/manifests/
// agentclass.yaml ships — resolves github_pull_request's ValueTransforms as
// EMPTY against the shipped gh toolkit (embeddedToolkitAsCR, the same
// conversion toolkitsForKeying itself uses), not a hand-built fixture.
//
// This must stay true. BindTriggerSlots
// (pkg/channels/channelsd/pipeline/triggerslots.go) binds the trigger-derived
// instance id VERBATIM — nil transforms, by design — while every other fill
// source runs the resourceType's published chain. github_pull_request is
// value-keyed by NO tool's PermissionCheck (gh.yaml's pr view/create/edit/
// close all check github_repo_url; the pull request's own identity is
// established by a writesRelationships block, which this derivation never
// walks), so `keying` never gains an entry for it and ValueTransforms stays
// empty. If github_pull_request ever gained a value-keyed check, the trigger
// path's verbatim id and that chain's transformed id would mint two
// different spellings of the same pull request — one instance reachable
// under two different object ids, silently. TriggerSlotRequestsFor's own
// exclusion of a resourceType publishing a non-empty chain
// (triggerslots.go) is the runtime backstop for exactly that collision; this
// test is the structural proof that the backstop's premise — this type
// publishes nothing to collide with — actually holds for the shipped demo.
func TestResolveSlotValueKeying_ReviewbotShapedClass_GithubPullRequestPublishesNoTransformChain(t *testing.T) {
	gh, found, err := embeddedToolkitAsCR("gh")
	require.NoError(t, err, "embed the gh toolkit as a CR")
	require.True(t, found, "the gh toolkit must be registered as a built-in")

	ac := classDeclaringSlots(spiceboxv1alpha1.AuthzSlot{
		ResourceType: "github_pull_request",
		Description:  "The pull request this session was triggered for",
		Permission:   "write_memory",
		FillFrom:     []string{"trigger"},
	})

	got, reason, msg := resolveSlotValueKeying(ac, nil, []spiceboxv1alpha1.SpiceboxToolkit{gh}, nil, nil)
	require.Empty(t, reason, "msg=%s", msg)
	require.Len(t, got, 1)
	assert.Equal(t, "github_pull_request", got[0].ResourceType)
	assert.Empty(t, got[0].ValueTransforms,
		"github_pull_request must publish no transform chain, or the trigger path's verbatim id and "+
			"this chain's transformed id would mint two different spellings of the same pull request")
}
