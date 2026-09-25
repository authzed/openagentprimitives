package toolcheck_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	v1 "github.com/authzed/authzed-go/proto/authzed/api/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/authz/scope"
	"github.com/authzed/openagentprimitives/pkg/authz/spicedb/toolcheck"
)

// fakeSpiceDB records every CheckPermission call. Production Check
// fans out concurrent goroutines in "both" mode, so the slice append
// is guarded by a mutex to keep -race quiet. The slice holds pointers
// (not values) because CheckPermissionRequest embeds protoimpl.MessageState
// which contains a sync.Mutex — copying it would trip `go vet` copylocks.
type fakeSpiceDB struct {
	mu         sync.Mutex
	checkCalls []*v1.CheckPermissionRequest
	answer     v1.CheckPermissionResponse_Permissionship
	zedToken   string
}

func (f *fakeSpiceDB) CheckPermission(ctx context.Context, req *v1.CheckPermissionRequest) (*v1.CheckPermissionResponse, error) {
	f.mu.Lock()
	f.checkCalls = append(f.checkCalls, req)
	f.mu.Unlock()
	return &v1.CheckPermissionResponse{
		Permissionship: f.answer,
		CheckedAt:      &v1.ZedToken{Token: f.zedToken},
	}, nil
}

// TestCheck_StateImpactShortCircuits covers the state-impact gates that resolve
// before any SpiceDB call: Stateless/Passthrough always allow, and External
// always denies here — the deny is what routes the call to human approval.
func TestCheck_StateImpactShortCircuits(t *testing.T) {
	cases := []struct {
		name        string
		impact      authz.StateImpact
		wantOutcome authz.Outcome
		wantMsgSub  string // substring; empty means no message check
	}{
		{
			name:        "stateless: allowed without SpiceDB",
			impact:      authz.Stateless,
			wantOutcome: authz.OutcomeAllowed,
		},
		{
			name:        "passthrough: allowed without SpiceDB",
			impact:      authz.Passthrough,
			wantOutcome: authz.OutcomeAllowed,
		},
		{
			name:        "external stateImpact: Denied, message names the approval requirement",
			impact:      authz.External,
			wantOutcome: authz.OutcomeDenied,
			wantMsgSub:  "approval",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cli := &fakeSpiceDB{}
			res := toolcheck.Checker{Cli: cli}.CheckToolCall(context.Background(), authz.Permission{StateImpact: tc.impact}, authz.Inputs{Subject: "u-1"})
			assert.Equal(t, tc.wantOutcome, res.Outcome)
			assert.Empty(t, cli.checkCalls, "state-impact short-circuit should not call SpiceDB")
			if tc.wantMsgSub != "" {
				assert.Contains(t, res.Message, tc.wantMsgSub)
			}
		})
	}
}

// TestCheck_NilCli_StatelessAllowed pins the "no Cli needed for short-circuit
// StateImpacts" property: Stateless/Passthrough must not consult Cli, so a
// nil Cli is harmless. Documents the contract that nil Cli is only fatal
// for StateImpacts that actually need to call SpiceDB.
func TestCheck_NilCli_StatelessAllowed(t *testing.T) {
	for _, impact := range []authz.StateImpact{authz.Stateless, authz.Passthrough} {
		t.Run(string(impact), func(t *testing.T) {
			res := toolcheck.Checker{}.CheckToolCall(context.Background(), authz.Permission{StateImpact: impact}, authz.Inputs{Subject: "u-1"})
			assert.Equal(t, authz.OutcomeAllowed, res.Outcome, "Stateless/Passthrough must not need a Cli")
		})
	}
}

// TestCheck_NilCli_ReadonlyDenied: a Readonly tool whose StateImpact requires
// a real SpiceDB call must FAIL CLOSED when Cli is nil rather than panicking
// or silently allowing. The deny message must mention authz so a model that
// retries can see why.
func TestCheck_NilCli_ReadonlyDenied(t *testing.T) {
	p := authz.Permission{
		StateImpact: authz.Readonly,
		Check: &authz.PermissionCheck{
			ResourceType: "github_repo", ResourceIDTemplate: "{repo}", Permission: "read",
		},
	}
	res := toolcheck.Checker{}.CheckToolCall(context.Background(), p, authz.Inputs{Args: map[string]any{"repo": "spicedb"}, Subject: "u-1"})
	assert.Equal(t, authz.OutcomeDenied, res.Outcome, "nil Cli + Readonly must deny, not panic or allow")
	assert.Contains(t, res.Message, "authz", "deny message should mention authz so the caller knows why")
}

// TestCheck_NilCli_ReadwriteDenied: same as Readonly variant — Readwrite with
// a nil Cli must deny.
func TestCheck_NilCli_ReadwriteDenied(t *testing.T) {
	p := authz.Permission{
		StateImpact: authz.Readwrite,
		Check: &authz.PermissionCheck{
			ResourceType: "github_repo", ResourceIDTemplate: "{repo}", Permission: "admin",
		},
	}
	res := toolcheck.Checker{}.CheckToolCall(context.Background(), p, authz.Inputs{Args: map[string]any{"repo": "spicedb"}, Subject: "u-1"})
	assert.Equal(t, authz.OutcomeDenied, res.Outcome, "nil Cli + Readwrite must deny, not panic or allow")
	assert.Contains(t, res.Message, "authz")
}

func TestCheck_ReadonlyAllow(t *testing.T) {
	cli := &fakeSpiceDB{answer: v1.CheckPermissionResponse_PERMISSIONSHIP_HAS_PERMISSION}
	p := authz.Permission{
		StateImpact: authz.Readonly,
		Check: &authz.PermissionCheck{
			ResourceType: "github_repo", ResourceIDTemplate: "{repo}", Permission: "read",
		},
	}
	res := toolcheck.Checker{Cli: cli}.CheckToolCall(context.Background(), p, authz.Inputs{Args: map[string]any{"repo": "spicedb"}, Subject: "u-1"})
	assert.Equal(t, authz.OutcomeAllowed, res.Outcome)
	assert.Len(t, cli.checkCalls, 1, "expected exactly 1 SpiceDB call")
}

func TestCheck_ReadwriteDeny(t *testing.T) {
	cli := &fakeSpiceDB{answer: v1.CheckPermissionResponse_PERMISSIONSHIP_NO_PERMISSION}
	p := authz.Permission{
		StateImpact: authz.Readwrite,
		Check: &authz.PermissionCheck{
			ResourceType: "github_repo", ResourceIDTemplate: "{repo}", Permission: "admin",
		},
	}
	res := toolcheck.Checker{Cli: cli}.CheckToolCall(context.Background(), p, authz.Inputs{Args: map[string]any{"repo": "spicedb"}, Subject: "u-1"})
	assert.Equal(t, authz.OutcomeDenied, res.Outcome)
	assert.Contains(t, res.Message, "permission denied")
	assert.Contains(t, res.Message, "github_repo:spicedb", "message should name the resource")
}

func TestCheck_StoresZedTokenInCacheOnSuccess(t *testing.T) {
	cache := toolcheck.NewZedTokenCache()
	cli := &fakeSpiceDB{
		answer:   v1.CheckPermissionResponse_PERMISSIONSHIP_HAS_PERMISSION,
		zedToken: "tok-after-check",
	}
	p := authz.Permission{
		StateImpact: authz.Readonly,
		Check: &authz.PermissionCheck{
			ResourceType: "x", ResourceIDTemplate: "{a}", Permission: "read",
		},
	}
	toolcheck.Checker{Cli: cli, Cache: cache}.CheckToolCall(context.Background(), p, authz.Inputs{Args: map[string]any{"a": "1"}, Subject: "u-1"})
	assert.Equal(t, "tok-after-check", cache.Get("x", "1"), "cache should hold the post-check ZedToken")
}

func TestCheck_BothModeRequiresAllSubjectsAllow(t *testing.T) {
	cli := &fakeSpiceDB{answer: v1.CheckPermissionResponse_PERMISSIONSHIP_HAS_PERMISSION}
	p := authz.Permission{
		StateImpact: authz.Readonly,
		Check: &authz.PermissionCheck{
			ResourceType: "x", ResourceIDTemplate: "{a}", Permission: "read",
		},
	}
	res := toolcheck.Checker{Cli: cli}.CheckToolCall(context.Background(), p, authz.Inputs{Args: map[string]any{"a": "1"}, Subjects: []string{"u-1", "u-2"}})
	assert.Equal(t, authz.OutcomeAllowed, res.Outcome)
	assert.Len(t, cli.checkCalls, 2, "both-mode should fire 2 calls")
}

func TestCheck_TemplateResolutionErrorDenies(t *testing.T) {
	cli := &fakeSpiceDB{}
	p := authz.Permission{
		StateImpact: authz.Readonly,
		Check: &authz.PermissionCheck{
			ResourceType: "x", ResourceIDTemplate: "{missing}", Permission: "read",
		},
	}
	res := toolcheck.Checker{Cli: cli}.CheckToolCall(context.Background(), p, authz.Inputs{Args: map[string]any{}, Subject: "u-1"})
	assert.Equal(t, authz.OutcomeDenied, res.Outcome)
	assert.Empty(t, cli.checkCalls, "missing arg should fail BEFORE SpiceDB call")
}

// TestCheckWithVariants_ContactsObjectTypeRoutes verifies that the first
// matching variant's Check (here: a Readonly check against crm_company)
// is the one that gets routed to SpiceDB. We seed the fake to deny all,
// so the only way to observe the route is the resulting deny message
// (which names crm_company:acme — proving the contacts-variant was hit
// and that ResourceIDExpr resolution worked end-to-end).
func TestCheckWithVariants_ContactsObjectTypeRoutes(t *testing.T) {
	contactsCheck := authz.Permission{StateImpact: authz.Readonly, Check: &authz.PermissionCheck{
		ResourceType: "crm_company", ResourceIDExpr: `"acme"`, Permission: "contact_access",
	}}
	companiesCheck := authz.Permission{StateImpact: authz.Passthrough}
	variants := []authz.PermissionVariant{
		{When: `args.objectType == "contacts"`, Check: contactsCheck},
		{When: `args.objectType == "companies"`, Check: companiesCheck},
	}
	cli := &fakeSpiceDB{answer: v1.CheckPermissionResponse_PERMISSIONSHIP_NO_PERMISSION}
	res := toolcheck.Checker{Cli: cli}.CheckWithVariants(context.Background(), authz.Permission{}, variants, authz.Inputs{
		Args:    map[string]any{"objectType": "contacts"},
		Subject: "alice",
	})
	require.Equal(t, authz.OutcomeDenied, res.Outcome, "expected OutcomeDenied (fake denies all)")
	assert.Contains(t, res.Message, "crm_company:acme", "deny message should name the contacts-variant resource")
	assert.Len(t, cli.checkCalls, 1, "expected exactly 1 SpiceDB call")
}

// TestCheckWithVariants_FallsBackToBasePermissionWhenNoMatch verifies
// that when no variant's When matches, the base Permission is used.
// Here the base is Passthrough (allowed without SpiceDB) so we also
// assert the fake was never called.
func TestCheckWithVariants_FallsBackToBasePermissionWhenNoMatch(t *testing.T) {
	base := authz.Permission{StateImpact: authz.Passthrough}
	variants := []authz.PermissionVariant{
		{When: `args.objectType == "contacts"`, Check: authz.Permission{StateImpact: authz.Readonly, Check: &authz.PermissionCheck{
			ResourceType: "x", ResourceIDExpr: `"x"`, Permission: "read",
		}}},
	}
	cli := &fakeSpiceDB{}
	res := toolcheck.Checker{Cli: cli}.CheckWithVariants(context.Background(), base, variants, authz.Inputs{Args: map[string]any{"objectType": "deals"}})
	require.Equal(t, authz.OutcomeAllowed, res.Outcome, "expected OutcomeAllowed (passthrough fallback)")
	assert.Empty(t, cli.checkCalls, "passthrough fallback should not call SpiceDB")
}

// TestCheckWithVariants_EmptyVariantsUsesBase verifies that an empty
// (or nil) variants slice is equivalent to a direct Check on the base
// Permission.
func TestCheckWithVariants_EmptyVariantsUsesBase(t *testing.T) {
	cli := &fakeSpiceDB{answer: v1.CheckPermissionResponse_PERMISSIONSHIP_HAS_PERMISSION}
	base := authz.Permission{
		StateImpact: authz.Readonly,
		Check: &authz.PermissionCheck{
			ResourceType: "github_repo", ResourceIDTemplate: "{repo}", Permission: "read",
		},
	}
	res := toolcheck.Checker{Cli: cli}.CheckWithVariants(context.Background(), base, nil, authz.Inputs{Args: map[string]any{"repo": "spicedb"}, Subject: "u-1"})
	require.Equal(t, authz.OutcomeAllowed, res.Outcome)
	assert.Len(t, cli.checkCalls, 1, "expected 1 SpiceDB call")
}

// TestCheckWithVariants_BadCELDenies verifies that a malformed variant
// `when` clause surfaces as a deny (fail-closed) rather than panicking
// or silently falling through to the base permission.
func TestCheckWithVariants_BadCELDenies(t *testing.T) {
	cli := &fakeSpiceDB{}
	variants := []authz.PermissionVariant{
		{When: `this is not valid CEL`, Check: authz.Permission{StateImpact: authz.Passthrough}},
	}
	res := toolcheck.Checker{Cli: cli}.CheckWithVariants(context.Background(), authz.Permission{StateImpact: authz.Passthrough}, variants, authz.Inputs{Args: map[string]any{}})
	require.Equal(t, authz.OutcomeDenied, res.Outcome, "expected OutcomeDenied on bad CEL")
	assert.Contains(t, res.Message, "variant resolution failed", "deny message should mention variant resolution")
}

// TestCheck_ResourceIDExpr_NestedExtraction verifies that the slice-4
// ResourceIDExpr form on a PermissionCheck pulls a nested value out of
// args via CEL filter/index and passes it as the SpiceDB object id.
func TestCheck_ResourceIDExpr_NestedExtraction(t *testing.T) {
	cli := &fakeSpiceDB{answer: v1.CheckPermissionResponse_PERMISSIONSHIP_HAS_PERMISSION}
	p := authz.Permission{StateImpact: authz.Readonly, Check: &authz.PermissionCheck{
		ResourceType:   "crm_company",
		ResourceIDExpr: `args.filterGroups[0].filters.filter(f, f.propertyName == "associations.company")[0].value`,
		Permission:     "contact_access",
	}}
	res := toolcheck.Checker{Cli: cli}.CheckToolCall(context.Background(), p, authz.Inputs{Subject: "alice",
		Args: map[string]any{
			"filterGroups": []any{map[string]any{
				"filters": []any{map[string]any{"propertyName": "associations.company", "value": "5083"}},
			}},
		},
	})
	require.Equal(t, authz.OutcomeAllowed, res.Outcome)
	require.Len(t, cli.checkCalls, 1, "expected 1 SpiceDB call")
	got := cli.checkCalls[0].GetResource()
	assert.Equal(t, "crm_company", got.GetObjectType(), "ResourceIDExpr extraction: object type")
	assert.Equal(t, "5083", got.GetObjectId(), "ResourceIDExpr extraction: object id")
}

// TestCheck_ResourceIDExpr_CompileErrorDenies verifies that a malformed
// ResourceIDExpr fails-closed BEFORE any SpiceDB call.
func TestCheck_ResourceIDExpr_CompileErrorDenies(t *testing.T) {
	cli := &fakeSpiceDB{}
	p := authz.Permission{StateImpact: authz.Readonly, Check: &authz.PermissionCheck{
		ResourceType:   "x",
		ResourceIDExpr: `this is not valid CEL`,
		Permission:     "read",
	}}
	res := toolcheck.Checker{Cli: cli}.CheckToolCall(context.Background(), p, authz.Inputs{Subject: "u-1", Args: map[string]any{}})
	assert.Equal(t, authz.OutcomeDenied, res.Outcome)
	assert.Empty(t, cli.checkCalls, "compile error should fail BEFORE SpiceDB call")
}

// ── Layer 2 tests (D3) ──────────────────────────────────────────────────────

// fakeCheckClient is a minimal Client used by Layer 2 + Layer 3 tests.
// Unlike fakeSpiceDB it tracks call count (not the full slice) and
// lets callers script a fixed response.
type fakeCheckClient struct {
	resp  *v1.CheckPermissionResponse
	err   error
	calls int
}

func (f *fakeCheckClient) CheckPermission(_ context.Context, _ *v1.CheckPermissionRequest) (*v1.CheckPermissionResponse, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	if f.resp != nil {
		return f.resp, nil
	}
	return &v1.CheckPermissionResponse{Permissionship: v1.CheckPermissionResponse_PERMISSIONSHIP_HAS_PERMISSION}, nil
}

func TestCheckToolCall_Layer2_ToolDenyShortCircuits(t *testing.T) {
	// Tool is denied by SessionScope; no SpiceDB call is made.
	fake := &fakeCheckClient{}
	p := authz.Permission{
		StateImpact: authz.Readonly,
		ToolName:    "github.delete_repo",
		Check: &authz.PermissionCheck{
			ResourceType:       "github_repo",
			Permission:         "read",
			ResourceIDTemplate: "{repo}",
		},
	}
	in := authz.Inputs{
		Args:    map[string]any{"repo": "foo/bar"},
		Subject: "user:alice",
		SessionScope: scope.Scope{
			Tools: scope.ScopeTools{Deny: []string{"github.delete_*"}},
		},
	}
	r := toolcheck.Checker{Cli: fake}.CheckToolCall(context.Background(), p, in)
	assert.Equal(t, authz.OutcomeDenied, r.Outcome)
	assert.Contains(t, r.Message, "denied by session policy")
	assert.Equal(t, 0, fake.calls, "no SpiceDB calls should occur when Layer 2 denies")
}

func TestCheckToolCall_Layer2_EmptyScope_PassesToSpiceDB(t *testing.T) {
	// Empty SessionScope — Layer 2 should be a no-op pass-through.
	fake := &fakeCheckClient{
		resp: &v1.CheckPermissionResponse{Permissionship: v1.CheckPermissionResponse_PERMISSIONSHIP_HAS_PERMISSION},
	}
	p := authz.Permission{
		StateImpact: authz.Readonly,
		ToolName:    "github.list_issues",
		Check: &authz.PermissionCheck{
			ResourceType:       "github_repo",
			Permission:         "read",
			ResourceIDTemplate: "{repo}",
		},
	}
	in := authz.Inputs{
		Args:    map[string]any{"repo": "foo/bar"},
		Subject: "user:alice",
		// SessionScope is the zero value
	}
	r := toolcheck.Checker{Cli: fake}.CheckToolCall(context.Background(), p, in)
	assert.Equal(t, authz.OutcomeAllowed, r.Outcome)
	assert.GreaterOrEqual(t, fake.calls, 1, "SpiceDB called at least once")
}

func TestCheckToolCall_Layer2_ArgConstraintDenies_NoSpiceDBCall(t *testing.T) {
	fake := &fakeCheckClient{}
	p := authz.Permission{
		StateImpact: authz.Readonly,
		ToolName:    "github.create_pr",
		Check: &authz.PermissionCheck{
			ResourceType:       "github_repo",
			Permission:         "write",
			ResourceIDTemplate: "{repo}",
		},
	}
	in := authz.Inputs{
		Args:    map[string]any{"repo": "foo/bar", "force_push": "true"},
		Subject: "user:alice",
		SessionScope: scope.Scope{
			ArgConstraints: []scope.ArgConstraint{{
				Tool:   "github.create_pr",
				Forbid: map[string]string{"force_push": "true"},
			}},
		},
	}
	r := toolcheck.Checker{Cli: fake}.CheckToolCall(context.Background(), p, in)
	assert.Equal(t, authz.OutcomeDenied, r.Outcome)
	assert.Equal(t, 0, fake.calls)
}

// Hard-deny is enforced entirely at Layer 2 by the Scope hook
// (pkg/authz/hooks/scope.go), covered by TestE2E_ScopeDisallow_* in
// pkg/agent/runner. CheckToolCall performs no disallow sub-check at all.

// recordingSpiceDB answers per (resource#permission) so a test can allow the
// SLOT-GRANT leg while denying ambient owner, and vice versa — the distinction
// a single unioned Check cannot express.
type recordingSpiceDB struct {
	answers map[string]v1.CheckPermissionResponse_Permissionship
	err     error
	calls   []string
}

func (f *recordingSpiceDB) CheckPermission(_ context.Context, in *v1.CheckPermissionRequest) (*v1.CheckPermissionResponse, error) {
	key := in.Resource.ObjectType + ":" + in.Resource.ObjectId + "#" + in.Permission
	f.calls = append(f.calls, key+"@"+in.Subject.Object.ObjectType)
	if f.err != nil {
		return nil, f.err
	}
	ans, ok := f.answers[key]
	if !ok {
		ans = v1.CheckPermissionResponse_PERMISSIONSHIP_NO_PERMISSION
	}
	return &v1.CheckPermissionResponse{Permissionship: ans, CheckedAt: &v1.ZedToken{Token: "zt"}}, nil
}

func externalPushPerm() authz.Permission {
	return authz.Permission{
		StateImpact: authz.External,
		Check: &authz.PermissionCheck{
			ResourceType: "git_repo", ResourceIDTemplate: "{repo}", Permission: "push",
		},
	}
}

// An external call is satisfied by a SLOT GRANT for this session — the thing a
// human created by approving a card that named this instance and permission.
//
// This is the plan gate's central promise ("name the resource and the phase
// runs without interrupting them again") finally holding for push and
// gh pr create, the two External permissions it never held for.
func TestCheck_External_AllowedByASlotGrantForThisSession(t *testing.T) {
	cli := &recordingSpiceDB{answers: map[string]v1.CheckPermissionResponse_Permissionship{
		"git_repo:acme/widgets#" + authz.SlotGrantRelationName("push"): v1.CheckPermissionResponse_PERMISSIONSHIP_HAS_PERMISSION,
	}}
	res := toolcheck.Checker{Cli: cli}.CheckToolCall(context.Background(), externalPushPerm(), authz.Inputs{
		Subject:           "u-1",
		Args:              map[string]any{"repo": "acme/widgets"},
		AgentSessionRef:   "default/sess-1",
		SlotResourceTypes: []string{"git_repo"},
	})
	assert.Equal(t, authz.OutcomeAllowed, res.Outcome)
	require.NotEmpty(t, cli.calls)
	assert.Contains(t, cli.calls[0], "agentsession",
		"the grant leg is asked about the SESSION, not the user — that is what makes it an approval and not ownership")
}

// AMBIENT OWNERSHIP does NOT satisfy an external call. The composed permission
// is `slot_grant_<p>->interact + owner`, so checking it would conflate "a human
// approved this" with "the requester owns this" — and an agent could then take
// irreversible outbound actions on anything its user owns with nobody ever
// seeing them.
//
// Caught in exactly that form by the gh-api-is-not-a-bypass bundle, whose
// fixture seeds an `owner` tuple and declares no slot: `gh api -X POST
// .../pulls` went from refused to allowed.
func TestCheck_External_NotAllowedByAmbientOwnership(t *testing.T) {
	cli := &recordingSpiceDB{answers: map[string]v1.CheckPermissionResponse_Permissionship{
		// The user owns it outright, and the unioned permission would say yes.
		"git_repo:acme/widgets#push":  v1.CheckPermissionResponse_PERMISSIONSHIP_HAS_PERMISSION,
		"git_repo:acme/widgets#owner": v1.CheckPermissionResponse_PERMISSIONSHIP_HAS_PERMISSION,
		// No slot_grant_push: nobody approved THIS call for THIS session.
	}}
	res := toolcheck.Checker{Cli: cli}.CheckToolCall(context.Background(), externalPushPerm(), authz.Inputs{
		Subject:           "u-1",
		Args:              map[string]any{"repo": "acme/widgets"},
		AgentSessionRef:   "default/sess-1",
		SlotResourceTypes: []string{"git_repo"},
	})
	assert.Equal(t, authz.OutcomeDenied, res.Outcome,
		"owning a repository is not the same as having approved this call")
	assert.Contains(t, res.Message, "approval")
}

// With no session ref there is no grant to look for, so external denies and
// routes to a human — the pre-existing behaviour, kept for any caller that has
// not threaded the session through.
func TestCheck_External_NoSessionRefStillDenies(t *testing.T) {
	cli := &recordingSpiceDB{}
	res := toolcheck.Checker{Cli: cli}.CheckToolCall(context.Background(), externalPushPerm(), authz.Inputs{
		Subject: "u-1", Args: map[string]any{"repo": "acme/widgets"},
	})
	assert.Equal(t, authz.OutcomeDenied, res.Outcome)
	assert.Empty(t, cli.calls, "with no session there is nothing to ask SpiceDB")
}

// An external tool whose resource type is not a declared slot has no instance
// axis at all: ComposeSlots never wrote a grant relation for it, so there is
// nothing a human could have approved and nothing to ask SpiceDB about. It
// denies to a human approval exactly as external always has — the point of the
// assertion is that it costs ZERO round trips to get there, because the query
// it would otherwise send can only error.
func TestCheck_External_UndeclaredSlotTypeDeniesWithoutQuerying(t *testing.T) {
	cli := &recordingSpiceDB{answers: map[string]v1.CheckPermissionResponse_Permissionship{
		"git_repo:acme/widgets#" + authz.SlotGrantRelationName("push"): v1.CheckPermissionResponse_PERMISSIONSHIP_HAS_PERMISSION,
	}}
	res := toolcheck.Checker{Cli: cli}.CheckToolCall(context.Background(), externalPushPerm(), authz.Inputs{
		Subject:         "u-1",
		Args:            map[string]any{"repo": "acme/widgets"},
		AgentSessionRef: "default/sess-1",
		// SlotResourceTypes omitted: this class declares no slots.
	})
	assert.Equal(t, authz.OutcomeDenied, res.Outcome)
	assert.Empty(t, cli.calls,
		"a type with no declared slot cannot carry a grant; querying it would only error")
}

// A SpiceDB failure must still deny, but must say why. Reporting the generic
// approval message here would hide an outage behind a card that looks routine.
func TestCheck_External_SpiceDBErrorDeniesAndSaysSo(t *testing.T) {
	cli := &recordingSpiceDB{err: errors.New("connection refused")}
	res := toolcheck.Checker{Cli: cli}.CheckToolCall(context.Background(), externalPushPerm(), authz.Inputs{
		Subject:           "u-1",
		Args:              map[string]any{"repo": "acme/widgets"},
		AgentSessionRef:   "default/sess-1",
		SlotResourceTypes: []string{"git_repo"},
	})
	assert.Equal(t, authz.OutcomeDenied, res.Outcome)
	assert.Contains(t, res.Message, "connection refused",
		"the operator needs the cause, not just the outcome")
}
