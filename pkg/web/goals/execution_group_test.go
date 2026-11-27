package goals

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apiext "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	core "github.com/authzed/openagentprimitives/pkg/agent/goals"
	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/permsurface"
	"github.com/authzed/openagentprimitives/pkg/authz/plangate"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/browser"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/useridentity"
)

// pinnedSkill is a skill ref pinned to a 40-hex commit.
const pinnedSkill = "github.com/acme/skills//standup@" + "0123456789abcdef0123456789abcdef01234567"

// enforcingAuthz is the guard set both private and group execution require.
func enforcingAuthz() *v1.AuthzBlock {
	return &v1.AuthzBlock{
		InformationLeakage: &v1.InformationLeakagePolicy{Mode: "enforcing"},
		ToolCalls:          &v1.ToolCallsAuthz{Mode: "enforcing"},
		PlanGate:           &v1.PlanGateConfig{Mode: "enforcing", RequirePlan: ptr.To(true), Rendering: &v1.PlanRendering{MaxAutoApproveHandles: 0}},
	}
}

// groupExecClass is an agent-mode class that qualifies for group execution
// and attaches tool runtimes, which group goals allow.
func groupExecClass() *v1.AgentClass {
	class := &v1.AgentClass{ObjectMeta: metav1.ObjectMeta{Namespace: groupNS, Name: "assistant", UID: "class-uid"}, Spec: v1.AgentClassSpec{
		Capabilities:  map[string]apiext.JSON{"goals": {Raw: []byte(`{"groups":true}`)}},
		AgentIdentity: "bot",
		MCPServers:    []v1.AgentClassMCPServerRef{{Name: "gh", Ref: "github"}},
		ToolBundles:   []v1.ToolBundle{{Name: "repo", Class: "sandbox", Toolspecs: []string{"git"}}},
		Skills:        []v1.AgentSkill{{Name: "standup", Ref: pinnedSkill}},
	}}
	class.Spec.Authz = enforcingAuthz()
	class.Spec.Authz.Slots = []v1.AuthzSlot{{ResourceType: "github_repo", Description: "a repository", Permission: "read"}}
	return class
}

func groupStandingPlan(t *testing.T) *plangate.Plan {
	t.Helper()
	frozen, problems := plangate.FreezeFrom([]plangate.AuthoredPhase{
		{ID: "read", Label: "Read the repo", Permissions: []plangate.AuthoredPermission{{Handle: "perm:read:github_repo"}}, Slots: []plangate.AuthoredSlot{{Type: "github_repo", ID: "acme/app"}}},
		{ID: "post", Label: "Post the summary", Permissions: []plangate.AuthoredPermission{{Handle: "perm:execute:agent_goal_execution"}}, Requires: []plangate.AuthoredRequires{{Phase: "read"}}},
	}, nil, []string{"github_repo"})
	require.Empty(t, problems)
	return &frozen
}

// groupStandingPhases is groupStandingPlan as a request authors it: the
// update_plan phase shape request_goal_execution's terms.standingPlan takes.
func groupStandingPhases() []core.StandingPhase {
	return []core.StandingPhase{
		{ID: "read", Label: "Read the repo", Why: "the weekly summary", Permissions: []core.StandingPermission{{Handle: "perm:read:github_repo", Why: "read"}}, Slots: []core.StandingSlot{{Type: "github_repo", ID: "acme/app", Why: "the group's repo"}}},
		{ID: "post", Label: "Post the summary", Why: "deliver it", Permissions: []core.StandingPermission{{Handle: "perm:execute:agent_goal_execution", Why: "report"}}, Requires: []core.StandingRequires{{Phase: "read", Why: "read first"}}},
	}
}

type groupExec struct {
	f     *groupFixture
	goal  core.Goal
	terms core.ExecutionTerms
	class *v1.AgentClass
	objs  []client.Object
}

// newGroupExec builds a group goal whose origin session no longer exists,
// with terms pinned to the live class, Channel and AgentIdentity. Alice and
// bob are members; bob decides.
func newGroupExec(t *testing.T, mutate func(*v1.AgentClass)) *groupExec {
	t.Helper()
	f := newGroupFixture(t)
	f.auth.members["bob"] = true
	f.auth.startDenied = map[string]bool{}
	class := groupExecClass()
	if mutate != nil {
		mutate(class)
	}
	ch := &v1.Channel{ObjectMeta: metav1.ObjectMeta{Namespace: groupNS, Name: "team-chat", UID: "channel-uid"}, Spec: v1.ChannelSpec{Kind: "grouptest"}}
	ai := &v1.AgentIdentity{ObjectMeta: metav1.ObjectMeta{Namespace: groupNS, Name: "bot", UID: "aid-uid"}}
	e := &groupExec{f: f, class: class, objs: []client.Object{class, ch, ai}}
	e.rebuild(t)
	hash, err := digest(class.Spec)
	require.NoError(t, err)
	now := time.Now().UTC()
	e.terms = core.ExecutionTerms{
		ClassDigest:       hash,
		SkillVersions:     []string{pinnedSkill},
		DueAt:             now.Add(time.Hour),
		ExpiresAt:         now.Add(60 * 24 * time.Hour),
		Bounds:            core.ExecutionBounds{DurationSeconds: 180, Turns: 10, Tokens: 10000, ApprovalSeconds: 90},
		AllowedOperations: []string{"respond_to_user", "report_goal_result"},
		Evidence:          []string{"weekly summary"},
		ActionApproval:    "standing_plan",
		StandingPlan:      groupStandingPlan(t),
		Group:             &core.GroupDestination{Channel: "team-chat", ChannelUID: "channel-uid", MemberGroup: groupOwner},
		AgentIdentity:     "bot",
		AgentIdentityUID:  "aid-uid",
	}
	e.goal = core.Goal{ID: "goal-1", Domain: groupDomainFixture(), State: core.Active, Execution: &core.ExecutionConsent{Session: groupNS + "/gone", SessionUID: "gone-uid"}}
	return e
}

// rebuild replaces the reader with exactly e.objs (no AgentSession at all).
func (e *groupExec) rebuild(t *testing.T) {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, v1.AddToScheme(scheme))
	e.f.s.Reader = fake.NewClientBuilder().WithScheme(scheme).WithObjects(e.objs...).Build()
}

func (e *groupExec) validate(decider string) error {
	return e.f.s.validateGroup(context.Background(), e.goal, e.terms, identity.CanonicalFromTrusted(decider, "test decider"))
}

type privateExec struct {
	f     *serverFixture
	goal  core.Goal
	terms core.ExecutionTerms
	k8s   client.Client
}

// newPrivateExec is the private reminder path that passes today: a
// userPassthrough, tool-less class, an origin session on a single-user
// browser route, and terms prepared by the server.
func newPrivateExec(t *testing.T, mutate func(*v1.AgentClass)) *privateExec {
	t.Helper()
	f := fixture(t)
	owner := identity.CanonicalFromTrusted("YWxpY2VAZXhhbXBsZS5jb20", "test human")
	class := &v1.AgentClass{ObjectMeta: metav1.ObjectMeta{Namespace: "team", Name: "assistant", UID: "class-uid"}, Spec: v1.AgentClassSpec{IdentityMode: "userPassthrough", Capabilities: map[string]apiext.JSON{"goals": {Raw: []byte(`{}`)}}}}
	class.Spec.Authz = enforcingAuthz()
	if mutate != nil {
		mutate(class)
	}
	sess := &v1.AgentSession{ObjectMeta: metav1.ObjectMeta{Namespace: "team", Name: "session", UID: "session-uid"}, Spec: v1.AgentSessionSpec{Class: "assistant", InputChannel: &v1.ChannelBinding{Name: "private", Kind: "browser", Key: "private-human"}}}
	ch := &v1.Channel{ObjectMeta: metav1.ObjectMeta{Namespace: "team", Name: "private", UID: "private-uid"}, Spec: v1.ChannelSpec{Kind: "browser"}}
	user := &v1.UserIdentity{ObjectMeta: metav1.ObjectMeta{Name: useridentity.NameForSubject(owner.Subject()), UID: "owner-catalog-uid"}, Spec: v1.UserIdentitySpec{Subject: owner.Subject().String()}}
	scheme := runtime.NewScheme()
	require.NoError(t, v1.AddToScheme(scheme))
	k8s := fake.NewClientBuilder().WithScheme(scheme).WithObjects(class, sess, ch, user).Build()
	f.s.Reader = k8s
	d := core.Domain{Namespace: "team", Owner: owner.String(), Class: "assistant", ClassUID: "class-uid"}
	a := core.Actor{Domain: d, Session: "team/session", SessionUID: "session-uid"}
	now := time.Now().UTC()
	req := core.ExecutionRequest{Terms: core.ExecutionTerms{DueAt: now.Add(time.Hour), ExpiresAt: now.Add(2 * time.Hour), Bounds: core.ExecutionBounds{DurationSeconds: 180, Turns: 10, Tokens: 10000, ApprovalSeconds: 90}, AllowedOperations: []string{"respond_to_user"}, Evidence: []string{"reminder"}}}
	require.NoError(t, f.s.PrepareExecution(context.Background(), a, &req))
	g := core.Goal{ID: "goal-1", Domain: d, State: core.Active, Execution: &core.ExecutionConsent{Session: a.Session, SessionUID: a.SessionUID}}
	return &privateExec{f: f, goal: g, terms: req.Terms, k8s: k8s}
}

func (p *privateExec) validate() error {
	return p.f.s.Validate(context.Background(), p.goal, p.terms)
}

func TestPrivateExecutionBaselinePasses(t *testing.T) {
	require.NoError(t, newPrivateExec(t, nil).validate())
}

func TestGroupExecutionBaselinePasses(t *testing.T) {
	e := newGroupExec(t, nil)
	require.NoError(t, e.validate("bob"), "agent-mode class with tool runtimes, no origin session, standing plan")
}

// TestGroupExecutionValidation has one row per line of the keep/replace/drop
// table in execution_group.go (plus the group destination and the
// standing-plan freeze).
// Each row asserts the group outcome and, where the guard is replaced or
// dropped, that the private path still enforces the original.
func TestGroupExecutionValidation(t *testing.T) {
	cases := []struct {
		name string
		// class mutates the class before terms are pinned, for both paths.
		class func(*v1.AgentClass)
		// group mutates the group fixture after pinning.
		group   func(*testing.T, *groupExec)
		decider string
		// groupErr is nil for accepted, else the sentinel; groupMsg is a
		// substring of the refusal.
		groupErr error
		groupMsg string
		// groupCause (optional) is the specific cause a dispatcher records.
		groupCause error
		// private (optional) mutates the private fixture; privateErr is what
		// the private path must still return.
		private    func(*testing.T, *privateExec)
		privateErr error
		privateMsg string
	}{
		{
			name: "kept: class UID pinned — class replaced under the same name: refused",
			group: func(t *testing.T, e *groupExec) {
				e.class.UID = "other-uid"
				e.rebuild(t)
			},
			groupErr: core.ErrDenied, groupCause: core.ErrGroupNotHostable, groupMsg: "class changed",
		},
		{
			name: "kept: class spec digest pinned — spec edited after consent: refused",
			group: func(_ *testing.T, e *groupExec) {
				e.terms.ClassDigest = "stale"
			},
			groupErr: core.ErrDenied, groupMsg: "class changed",
		},
		{
			name:     "replaced: identityMode userPassthrough — refused for group, required for private",
			class:    func(c *v1.AgentClass) { c.Spec.IdentityMode = "userPassthrough" },
			groupErr: core.ErrDenied, groupMsg: "identityMode",
			private: func(t *testing.T, p *privateExec) {
				var c v1.AgentClass
				require.NoError(t, p.k8s.Get(context.Background(), client.ObjectKey{Namespace: "team", Name: "assistant"}, &c))
				c.Spec.IdentityMode = "agent"
				require.NoError(t, p.k8s.Update(context.Background(), &c))
				p.terms.ClassDigest, _ = digest(c.Spec)
			},
			privateErr: core.ErrDenied, privateMsg: "user passthrough",
		},
		{
			name: "replaced: tool runtimes — MCPServers and ToolBundles accepted for group, refused for private",
			private: func(t *testing.T, p *privateExec) {
				var c v1.AgentClass
				require.NoError(t, p.k8s.Get(context.Background(), client.ObjectKey{Namespace: "team", Name: "assistant"}, &c))
				c.Spec.MCPServers = []v1.AgentClassMCPServerRef{{Name: "gh", Ref: "github"}}
				require.NoError(t, p.k8s.Update(context.Background(), &c))
				p.terms.ClassDigest, _ = digest(c.Spec)
			},
			privateErr: core.ErrDenied, privateMsg: "external tool runtimes",
		},
		{
			name: "kept: skills pinned to a commit — a branch ref: refused",
			class: func(c *v1.AgentClass) {
				c.Spec.Skills = []v1.AgentSkill{{Name: "standup", Ref: "github.com/acme/skills//standup@main"}}
			},
			group: func(_ *testing.T, e *groupExec) {
				e.terms.SkillVersions = []string{"github.com/acme/skills//standup@main"}
			},
			groupErr: core.ErrDenied, groupMsg: "pin a commit",
		},
		{
			name: "kept: skill versions match the terms — a different pinned list: refused",
			group: func(_ *testing.T, e *groupExec) {
				e.terms.SkillVersions = nil
			},
			groupErr: core.ErrDenied, groupMsg: "skill",
		},
		{
			name: "replaced: owner catalog pin — AgentIdentity UID mismatch: refused",
			group: func(_ *testing.T, e *groupExec) {
				e.terms.AgentIdentityUID = "recreated-uid"
			},
			groupErr: core.ErrDenied, groupCause: core.ErrIdentityChanged, groupMsg: "AgentIdentity",
		},
		{
			name: "replaced: owner catalog pin — AgentIdentity name differs from the class's: refused",
			group: func(_ *testing.T, e *groupExec) {
				e.terms.AgentIdentity = "other-bot"
			},
			groupErr: core.ErrDenied, groupCause: core.ErrIdentityChanged, groupMsg: "AgentIdentity",
		},
		{
			name: "replaced: owner catalog pin — AgentIdentity deleted: refused",
			group: func(t *testing.T, e *groupExec) {
				e.objs = e.objs[:2]
				e.rebuild(t)
			},
			groupErr: core.ErrDenied, groupCause: core.ErrIdentityChanged, groupMsg: "AgentIdentity",
		},
		{
			name: "dropped: executionPreference(owner) — unset preference accepted for group, refused for private",
			class: func(c *v1.AgentClass) {
				c.Spec.UserPreferences = []v1.UserPreferenceSchema{{Name: "goal_execution_enabled", Type: "bool"}}
			},
			privateErr: core.ErrDenied,
		},
		{
			name:     "kept: no widened session-interact permission — widened: refused",
			class:    func(c *v1.AgentClass) { c.Spec.Authz.Session = &v1.SessionAuthz{InteractPermission: "view"} },
			groupErr: core.ErrDenied, groupMsg: "widened session access",
		},
		{
			name:     "kept: enforcing info-leakage — logging: refused",
			class:    func(c *v1.AgentClass) { c.Spec.Authz.InformationLeakage.Mode = "logging" },
			groupErr: core.ErrDenied, groupMsg: "enforcing",
		},
		{
			name:     "kept: enforcing tool-call gate — logging: refused",
			class:    func(c *v1.AgentClass) { c.Spec.Authz.ToolCalls.Mode = "logging" },
			groupErr: core.ErrDenied, groupMsg: "enforcing",
		},
		{
			name:     "kept: enforcing plan gate — logging: refused",
			class:    func(c *v1.AgentClass) { c.Spec.Authz.PlanGate.Mode = "logging" },
			groupErr: core.ErrDenied, groupMsg: "enforcing",
		},
		{
			name:     "kept: RequirePlan — off: refused",
			class:    func(c *v1.AgentClass) { c.Spec.Authz.PlanGate.RequirePlan = ptr.To(false) },
			groupErr: core.ErrDenied, groupMsg: "enforcing",
		},
		{
			name:     "kept: no auto-approve — auto-approving handles: refused",
			class:    func(c *v1.AgentClass) { c.Spec.Authz.PlanGate.Rendering.MaxAutoApproveHandles = 2 },
			groupErr: core.ErrDenied, groupMsg: "enforcing",
		},
		{
			name:  "replaced: start gate checked for the decider — decider refused by the gate: refused",
			class: func(c *v1.AgentClass) { c.Spec.Authz.Session = &v1.SessionAuthz{AllowedStarters: []string{"user:bob"}} },
			group: func(_ *testing.T, e *groupExec) {
				e.f.auth.startDenied["bob"] = true
			},
			groupErr: core.ErrDenied, groupMsg: "start",
		},
		{
			name:  "replaced: start gate checked for the decider — another member lacking it does not matter",
			class: func(c *v1.AgentClass) { c.Spec.Authz.Session = &v1.SessionAuthz{AllowedStarters: []string{"user:bob"}} },
			group: func(_ *testing.T, e *groupExec) {
				e.f.auth.startDenied["alice"] = true
			},
			private: func(t *testing.T, p *privateExec) {
				var c v1.AgentClass
				require.NoError(t, p.k8s.Get(context.Background(), client.ObjectKey{Namespace: "team", Name: "assistant"}, &c))
				c.Spec.Authz.Session = &v1.SessionAuthz{AllowedStarters: []string{"user:alice"}}
				require.NoError(t, p.k8s.Update(context.Background(), &c))
				p.terms.ClassDigest, _ = digest(c.Spec)
				p.f.auth.denyStart = true
			},
			privateErr: core.ErrDenied,
		},
		{
			name:     "start gate needs a decider — none: refused",
			decider:  "-",
			groupErr: core.ErrDenied, groupMsg: "deciding manager",
		},
		{
			name: "replaced: origin session — deleted session is not consulted for group, refused for private",
			private: func(t *testing.T, p *privateExec) {
				require.NoError(t, p.k8s.Delete(context.Background(), &v1.AgentSession{ObjectMeta: metav1.ObjectMeta{Namespace: "team", Name: "session"}}))
			},
			privateErr: errAny,
		},
		{
			name: "replaced: origin Channel UID pinned — Channel recreated under the same name: refused",
			group: func(_ *testing.T, e *groupExec) {
				e.terms.Group.ChannelUID = "older-channel-uid"
			},
			groupErr: core.ErrDenied, groupCause: core.ErrGroupChannelGone, groupMsg: "channel",
		},
		{
			name: "replaced: origin Channel — Channel deleted: refused",
			group: func(t *testing.T, e *groupExec) {
				e.objs = []client.Object{e.objs[0], e.objs[2]}
				e.rebuild(t)
			},
			groupErr: core.ErrDenied, groupCause: core.ErrGroupChannelGone, groupMsg: "channel",
		},
		{
			name: "replaced: origin Channel — kind no longer hosts group goals: refused",
			group: func(t *testing.T, e *groupExec) {
				e.objs[1].(*v1.Channel).Spec.Kind = "plaintest"
				e.rebuild(t)
			},
			groupErr: core.ErrDenied, groupCause: core.ErrGroupNotHostable, groupMsg: "cannot host group goals",
		},
		{
			name: "replaced: AllowedOperations not limited — tool operations accepted for group, refused for private",
			group: func(_ *testing.T, e *groupExec) {
				e.terms.AllowedOperations = append(e.terms.AllowedOperations, "github_create_issue")
			},
			private: func(_ *testing.T, p *privateExec) {
				p.terms.AllowedOperations = append(p.terms.AllowedOperations, "github_create_issue")
			},
			privateErr: core.ErrInvalid, privateMsg: "unsupported",
		},
		{
			name: "replaced: Recipient == owner — destination member ref differs from the domain owner: refused",
			group: func(_ *testing.T, e *groupExec) {
				e.terms.Group.MemberGroup = "group:g2#member"
			},
			groupErr: core.ErrDenied, groupMsg: "group",
			private: func(_ *testing.T, p *privateExec) {
				p.terms.Destination.Recipient = "someone-else"
			},
			privateErr: core.ErrDenied,
		},
		{
			name: "replaced: Recipient == owner — member ref of a type the Channel's kind does not contribute: refused",
			group: func(_ *testing.T, e *groupExec) {
				e.goal.Domain.Owner = "other_channel:C1#member"
				e.terms.Group.MemberGroup = "other_channel:C1#member"
			},
			groupErr: core.ErrDenied, groupCause: core.ErrGroupNotHostable, groupMsg: "group",
		},
		{
			name: "standing plan frozen — a slot type the class never declared: refused",
			group: func(_ *testing.T, e *groupExec) {
				e.terms.StandingPlan.Phases[0].Slots = append(e.terms.StandingPlan.Phases[0].Slots, plangate.Slot{Type: "crm_company", ID: "acme"})
			},
			groupErr: core.ErrInvalid, groupMsg: "crm_company",
		},
		{
			name: "standing plan frozen — a requires edge to a missing phase: refused",
			group: func(_ *testing.T, e *groupExec) {
				e.terms.StandingPlan.Phases[1].Requires = []plangate.RequiresEdge{{Phase: 7}}
			},
			groupErr: core.ErrInvalid, groupMsg: "requires",
		},
		{
			name: "standing plan frozen — a duplicated slot (not the frozen form): refused",
			group: func(_ *testing.T, e *groupExec) {
				e.terms.StandingPlan.Phases[0].Slots = append(e.terms.StandingPlan.Phases[0].Slots, e.terms.StandingPlan.Phases[0].Slots[0])
			},
			groupErr: core.ErrInvalid, groupMsg: "frozen",
		},
		{
			name: "standing plan names two instances of one slot type: refused, a run holds one",
			group: func(_ *testing.T, e *groupExec) {
				e.terms.StandingPlan.Phases[0].Slots = append(e.terms.StandingPlan.Phases[0].Slots, plangate.Slot{Type: "github_repo", ID: "acme/other"})
			},
			groupErr: core.ErrInvalid, groupMsg: "a run can hold one github_repo at a time",
		},
		{
			name: "standing plan consents are operator authority — a phase carrying one: refused",
			group: func(_ *testing.T, e *groupExec) {
				e.terms.StandingPlan.Phases[0].Consents = []json.RawMessage{json.RawMessage(`{"category":"x"}`)}
			},
			groupErr: core.ErrInvalid, groupMsg: "consents",
		},
		{
			name: "standing plan with no phase that reports its result: refused",
			group: func(_ *testing.T, e *groupExec) {
				e.terms.StandingPlan.Phases[1].Permissions = nil
			},
			groupErr: core.ErrInvalid, groupMsg: "must include a phase that reports its result",
		},
		{
			name: "event watches are not available on group goals: refused",
			group: func(_ *testing.T, e *groupExec) {
				e.terms.Event = &core.EventWatch{}
			},
			groupErr: core.ErrDenied, groupMsg: "event",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newGroupExec(t, tc.class)
			if tc.group != nil {
				tc.group(t, e)
			}
			decider := "bob"
			if tc.decider == "-" {
				decider = ""
			}
			err := e.validate(decider)
			if tc.groupErr == nil {
				assert.NoError(t, err, "group")
			} else {
				assert.ErrorIs(t, err, tc.groupErr, "group")
				if tc.groupCause != nil {
					assert.ErrorIs(t, err, tc.groupCause, "group cause")
				}
				if tc.groupMsg != "" && err != nil {
					assert.Contains(t, strings.ToLower(err.Error()), strings.ToLower(tc.groupMsg), "group refusal reason")
				}
			}
			if tc.privateErr == nil {
				return
			}
			p := newPrivateExec(t, tc.class)
			if tc.private != nil {
				tc.private(t, p)
			}
			perr := p.validate()
			if tc.privateErr == errAny {
				assert.Error(t, perr, "private")
			} else {
				assert.ErrorIs(t, perr, tc.privateErr, "private")
			}
			if tc.privateMsg != "" && perr != nil {
				assert.Contains(t, perr.Error(), tc.privateMsg, "private refusal reason")
			}
		})
	}
}

// errAny marks a private row whose refusal is any error (a NotFound lookup).
var errAny = errors.New("any error")

// TestGroupStartGateChecksTheDeciderNotTheOwner proves the gate's subject.
func TestGroupStartGateChecksTheDeciderNotTheOwner(t *testing.T) {
	e := newGroupExec(t, func(c *v1.AgentClass) {
		c.Spec.Authz.Session = &v1.SessionAuthz{AllowedStarters: []string{"user:bob"}}
	})
	e.f.auth.checkedUsers = nil
	require.NoError(t, e.validate("bob"))
	assert.Contains(t, e.f.auth.checkedUsers, "bob")
	assert.NotContains(t, e.f.auth.checkedUsers, groupOwner, "a member subject-set is never checked as a user")
}

// TestGroupValidateDispatch routes a group-owned goal to the group rules
// from the ExecutionAuthority entry point, which has no decider yet: it
// refuses rather than skipping the start gate.
func TestGroupValidateDispatch(t *testing.T) {
	e := newGroupExec(t, nil)
	err := e.f.s.Validate(context.Background(), e.goal, e.terms)
	require.ErrorIs(t, err, core.ErrDenied)
	assert.Contains(t, err.Error(), "deciding manager", "never the private owner-catalog lookup")
}

// TestGroupDispatchRefusesOwnerCeiling: a class that gains an owner ceiling
// after consent stops dispatching group runs, which its managers could no
// longer own.
func TestGroupDispatchRefusesOwnerCeiling(t *testing.T) {
	for _, tc := range []struct {
		name    string
		ceiling *v1.OwnerCeiling
		mode    string
	}{
		{name: "no ceiling (default mode): dispatches"},
		{name: "fixed ceiling: refused naming fixed", ceiling: &v1.OwnerCeiling{Fixed: "group:admins#member"}, mode: "fixed"},
		{name: "starterOnly ceiling: refused naming starterOnly", ceiling: &v1.OwnerCeiling{StarterOnly: true}, mode: "starterOnly"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newGroupExec(t, func(c *v1.AgentClass) { c.Spec.Authz.OwnerCeiling = tc.ceiling })
			err := e.f.s.ValidateGroupDispatch(context.Background(), e.goal, e.terms)
			if tc.mode == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorIs(t, err, core.ErrDenied)
			assert.ErrorIs(t, err, core.ErrGroupNotHostable, "the dispatcher records group_not_hostable")
			assert.Contains(t, err.Error(), "this agent's session-owner policy ("+tc.mode+") would override them")
		})
	}
}

func TestPrepareGroupExecution(t *testing.T) {
	authored := []plangate.AuthoredPhase{{ID: "read", Label: "Read the repo", Permissions: []plangate.AuthoredPermission{{Handle: "perm:execute:agent_goal_execution"}, {Handle: "perm:read:github_repo"}}, Slots: []plangate.AuthoredSlot{{Type: "github_repo", ID: "acme/app"}}}}
	want, problems := plangate.FreezeFrom(authored, nil, []string{"github_repo"})
	require.Empty(t, problems)
	newRequest := func() *core.ExecutionRequest {
		now := time.Now().UTC()
		return &core.ExecutionRequest{
			Terms: core.ExecutionTerms{
				DueAt: now.Add(time.Hour), ExpiresAt: now.Add(48 * time.Hour),
				AllowedOperations: []string{"respond_to_user"}, Evidence: []string{"summary"},
				ActionApproval: "standing_plan",
				// Model-supplied pins are overwritten from authoritative objects.
				Group: &core.GroupDestination{Channel: "elsewhere", ChannelUID: "x", MemberGroup: "group:g9#member"}, AgentIdentityUID: "forged",
			},
			// The authored shape request_goal_execution's terms.standingPlan takes.
			StandingPlan: []core.StandingPhase{{
				ID: "read", Label: "Read the repo", Why: "the weekly summary",
				Permissions: []core.StandingPermission{{Handle: "perm:execute:agent_goal_execution", Why: "report"}, {Handle: "perm:read:github_repo", Why: "read"}},
				Slots:       []core.StandingSlot{{Type: "github_repo", ID: "acme/app", Why: "the group's repo"}},
			}},
		}
	}
	setup := func(t *testing.T) (*groupExec, core.Actor) {
		e := newGroupExec(t, nil)
		binding := &v1.ChannelBinding{Name: "team-chat", Kind: "grouptest", Key: "thread:1", External: map[string]string{"group": "g1"}}
		e.objs = append(e.objs, &v1.AgentSession{ObjectMeta: metav1.ObjectMeta{Namespace: groupNS, Name: "session", UID: "session-uid"}, Spec: v1.AgentSessionSpec{Class: "assistant", InputChannel: binding}})
		e.rebuild(t)
		return e, core.Actor{Domain: groupDomainFixture(), Session: groupNS + "/session", SessionUID: "session-uid", User: "bob"}
	}

	t.Run("fills group pins from the session's conversation and freezes the authored plan", func(t *testing.T) {
		e, a := setup(t)
		r := newRequest()
		require.NoError(t, e.f.s.PrepareExecution(context.Background(), a, r))
		assert.Equal(t, &core.GroupDestination{Channel: "team-chat", ChannelUID: "channel-uid", MemberGroup: groupOwner}, r.Terms.Group)
		assert.Equal(t, core.PrivateDestination{}, r.Terms.Destination)
		assert.Empty(t, r.Terms.OwnerCatalogUID)
		assert.Equal(t, "bot", r.Terms.AgentIdentity)
		assert.Equal(t, "aid-uid", r.Terms.AgentIdentityUID)
		assert.Equal(t, []string{pinnedSkill}, r.Terms.SkillVersions)
		require.NotNil(t, r.Terms.StandingPlan)
		assert.Equal(t, want.Digest(), r.Terms.StandingPlan.Digest())
		require.Len(t, r.Terms.StandingPlan.Phases, 1)
		ph := r.Terms.StandingPlan.Phases[0]
		assert.Equal(t, []string{"perm:execute:agent_goal_execution", "perm:read:github_repo"}, handleStrings(ph.Permissions))
		assert.Equal(t, []plangate.Slot{{Type: "github_repo", ID: "acme/app", Why: "the group's repo"}}, ph.Slots)
		assert.Equal(t, 1, ph.Max.Count, "freezing applies the default entry count")
		assert.Empty(t, r.StandingPlan, "the authored plan is consumed by the freeze")
		e.goal.Execution = &core.ExecutionConsent{Session: a.Session, SessionUID: a.SessionUID}
		require.NoError(t, e.validate("bob"), "prepared terms validate")
	})
	t.Run("conversation member ref differs from the domain owner: refused", func(t *testing.T) {
		e, a := setup(t)
		a.Domain.Owner = "group:g2#member"
		require.ErrorIs(t, e.f.s.PrepareExecution(context.Background(), a, newRequest()), core.ErrDenied)
	})
	t.Run("AgentIdentity being deleted: refused", func(t *testing.T) {
		e, a := setup(t)
		ai := e.objs[2].(*v1.AgentIdentity)
		ai.Finalizers = []string{"test"}
		ai.DeletionTimestamp = ptr.To(metav1.Now())
		e.rebuild(t)
		err := e.f.s.PrepareExecution(context.Background(), a, newRequest())
		require.ErrorIs(t, err, core.ErrDenied)
		assert.Contains(t, err.Error(), "AgentIdentity")
	})

	refusals := []struct {
		name   string
		mutate func(r *core.ExecutionRequest)
		msg    string
	}{
		{
			name: "a frozen plan in terms (the caller skipping the freeze): refused",
			mutate: func(r *core.ExecutionRequest) {
				r.Terms.StandingPlan = &want
				r.StandingPlan = nil
			},
			msg: "give the standing plan as terms.standingPlan phases",
		},
		{
			name:   "no phase that reports its result: refused at freeze",
			mutate: func(r *core.ExecutionRequest) { r.StandingPlan[0].Permissions = r.StandingPlan[0].Permissions[1:] },
			msg:    "a group goal's standing plan must include a phase that reports its result",
		},
		{
			name: "an undeclared slot type: refused at freeze",
			mutate: func(r *core.ExecutionRequest) {
				r.StandingPlan[0].Slots = append(r.StandingPlan[0].Slots, core.StandingSlot{Type: "crm_company", ID: "acme", Why: "x"})
			},
			msg: "crm_company",
		},
		{
			name: "two instances of one slot type in a phase: refused, a run holds one",
			mutate: func(r *core.ExecutionRequest) {
				r.StandingPlan[0].Slots = append(r.StandingPlan[0].Slots, core.StandingSlot{Type: "github_repo", ID: "acme/other", Why: "x"})
			},
			msg: "a run can hold one github_repo at a time",
		},
		{
			name: "two instances of one slot type across phases: refused, a run holds one",
			mutate: func(r *core.ExecutionRequest) {
				r.StandingPlan = append(r.StandingPlan, core.StandingPhase{ID: "more", Label: "More", Why: "x",
					Permissions: []core.StandingPermission{{Handle: "perm:read:github_repo", Why: "x"}},
					Slots:       []core.StandingSlot{{Type: "github_repo", ID: "acme/other", Why: "x"}}})
			},
			msg: "a run can hold one github_repo at a time",
		},
	}
	for _, tc := range refusals {
		t.Run(tc.name, func(t *testing.T) {
			e, a := setup(t)
			r := newRequest()
			tc.mutate(r)
			err := e.f.s.PrepareExecution(context.Background(), a, r)
			require.ErrorIs(t, err, core.ErrInvalid)
			assert.Contains(t, err.Error(), tc.msg)
		})
	}
}

func handleStrings(hs []permsurface.Handle) []string {
	out := make([]string, len(hs))
	for i, h := range hs {
		out[i] = h.String()
	}
	return out
}
