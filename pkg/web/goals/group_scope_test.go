package goals

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apiext "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	core "github.com/authzed/openagentprimitives/pkg/agent/goals"
	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	fakekind "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/fake"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/registry"
	"github.com/authzed/openagentprimitives/pkg/memory"
	goalinmem "github.com/authzed/openagentprimitives/pkg/memory/goals/inmem"
	meminmem "github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/infoleakagetaint"
	"github.com/authzed/openagentprimitives/pkg/memory/provenance"
	"github.com/authzed/openagentprimitives/pkg/memory/tokens"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// groupTestKind is the fake kind's group-goal contract under its own name,
// with a scriptable membership sync and audience. Tests touching these
// package variables must not run in parallel.
type groupTestKind struct{ fakekind.Kind }

var (
	groupTestMembershipErr error
	groupTestAudience      []string
	groupTestAudienceErr   error
)

func (groupTestKind) Name() string { return "grouptest" }
func (groupTestKind) MembershipCurrent(context.Context, client.Reader, *v1.Channel) error {
	return groupTestMembershipErr
}
func (groupTestKind) ResolveAudience(context.Context, channelkinds.SessionInfo) ([]string, error) {
	return slices.Clone(groupTestAudience), groupTestAudienceErr
}

// plainTestKind registers a kind with no group-goal contract.
type plainTestKind struct{ channelkinds.Kind }

func (plainTestKind) Name() string { return "plaintest" }

func init() {
	registry.Register(groupTestKind{})
	registry.Register(plainTestKind{})
}

const (
	groupOwner = "group:g1#member"
	groupNS    = "team"
)

// groupAuthority models the agent_goal_domain schema closely enough to tell
// members, managers and admins apart: view_memory/pause = member_group +
// class admin, manage = (manager & member_group) + class admin.
type groupAuthority struct {
	members, admins map[string]bool
	// unreadable holds "type:id:user" sources a user cannot read.
	unreadable   map[string]bool
	tuples       map[authz.Relation]bool
	deleted      []authz.Relation
	grants       []authz.SlotBinding
	checkedUsers []string
	// startDenied holds users the class start gate refuses.
	startDenied       map[string]bool
	failManagerWrites bool
	failDeletes       bool
	// failExecutionWrites fails any write of an execution grant tuple.
	failExecutionWrites bool
	// written is every relation written, in order (tuples is the set).
	written []authz.Relation
	// subjectChecks records each non-user check as "res#perm@subj".
	subjectChecks []string
	// refusedPerms holds "type:id:perm:user" checks on other resource types
	// that answer false; checkErrs fails every check on a resource type.
	refusedPerms map[string]bool
	checkErrs    map[string]error
	// interactChecks counts CheckInteract calls.
	interactChecks int
}

func newGroupAuthority() *groupAuthority {
	return &groupAuthority{members: map[string]bool{}, admins: map[string]bool{}, unreadable: map[string]bool{}, tuples: map[authz.Relation]bool{}, refusedPerms: map[string]bool{}, checkErrs: map[string]error{}}
}

func (a *groupAuthority) Relations() authz.RelWriter { return a }
func (a *groupAuthority) WriteRelationships(_ context.Context, rels []authz.Relation) error {
	for _, r := range rels {
		if r.Relation == "manager" && a.failManagerWrites {
			return fmt.Errorf("test manager tuple write failure")
		}
		if r.ResourceType == ExecutionResourceType && a.failExecutionWrites {
			return fmt.Errorf("test execution grant write failure")
		}
	}
	for _, r := range rels {
		a.tuples[r] = true
		a.written = append(a.written, r)
	}
	return nil
}

// CheckSubjectOnResource models execute_group = domain_approved & domain on
// agent_goal_execution, for a domain subject.
func (a *groupAuthority) CheckSubjectOnResource(_ context.Context, typ, id, perm, subjType, subjID string, _ bool) (bool, error) {
	a.subjectChecks = append(a.subjectChecks, typ+":"+id+"#"+perm+"@"+subjType+":"+subjID)
	if typ != ExecutionResourceType || perm != "execute_group" || subjType != ResourceType {
		return false, fmt.Errorf("unexpected subject check %s:%s#%s@%s", typ, id, perm, subjType)
	}
	return a.hasOn(typ, id, "domain_approved", subjType, subjID, "") && a.hasOn(typ, id, "domain", subjType, subjID, ""), nil
}
func (a *groupAuthority) ListUserSubjects(_ context.Context, typ, id, rel string) ([]string, error) {
	var out []string
	for t := range a.tuples {
		if t.ResourceType == typ && t.ResourceID == id && t.Relation == rel && t.SubjectType == "user" {
			out = append(out, t.SubjectID)
		}
	}
	return out, nil
}
func (a *groupAuthority) DeleteRelationships(_ context.Context, rels []authz.Relation) error {
	if a.failDeletes {
		return fmt.Errorf("test tuple delete failure")
	}
	for _, r := range rels {
		delete(a.tuples, r)
		a.deleted = append(a.deleted, r)
	}
	return nil
}
func (a *groupAuthority) GrantSlots(_ context.Context, _, _ string, b []authz.SlotBinding, _ time.Time) error {
	a.grants = append(a.grants, b...)
	return nil
}
func (a *groupAuthority) CheckInteract(context.Context, string, string, identity.CanonicalUserID, bool) (bool, error) {
	a.interactChecks++
	return true, nil
}
func (a *groupAuthority) has(id, rel, subjType, subjID, subjRel string) bool {
	return a.hasOn(ResourceType, id, rel, subjType, subjID, subjRel)
}
func (a *groupAuthority) hasOn(typ, id, rel, subjType, subjID, subjRel string) bool {
	for t := range a.tuples {
		if t.ResourceType == typ && t.ResourceID == id && t.Relation == rel && t.SubjectType == subjType && t.SubjectID == subjID && t.SubjectRel == subjRel {
			return true
		}
	}
	return false
}

// managerTuple reports agent_goal:<goalID>#manager@user:<user>.
func (a *groupAuthority) managerTuple(goalID, user string) bool {
	return a.hasOn(GoalResourceType, goalID, "manager", "user", user, "")
}

// goalDomain is the domain agent_goal:<goalID>#domain names, or "".
func (a *groupAuthority) goalDomain(goalID string) string {
	for t := range a.tuples {
		if t.ResourceType == GoalResourceType && t.ResourceID == goalID && t.Relation == "domain" && t.SubjectType == ResourceType {
			return t.SubjectID
		}
	}
	return ""
}
func (a *groupAuthority) CheckOnResource(_ context.Context, typ, id, perm string, u identity.CanonicalUserID, _ bool) (bool, error) {
	a.checkedUsers = append(a.checkedUsers, u.String())
	user := u.String()
	if err := a.checkErrs[typ]; err != nil {
		return false, err
	}
	switch typ {
	case "agentclass":
		if perm == "administer" {
			return a.admins[user], nil
		}
		return !a.startDenied[user], nil
	case "group":
		return id == "g1" && perm == "member" && a.members[user], nil
	case ResourceType:
		inGroup := a.has(id, "member_group", "group", "g1", "member") && a.members[user]
		admin := a.has(id, "class", "agentclass", groupNS+"/assistant", "") && a.admins[user]
		owner := a.has(id, "owner", "user", user, "")
		switch perm {
		case "view_memory", "pause":
			return owner || inGroup || admin, nil
		case "manage":
			return owner || admin, nil
		}
		return false, fmt.Errorf("unexpected permission %q", perm)
	case GoalResourceType:
		if perm != "manage" {
			return false, fmt.Errorf("unexpected permission %q", perm)
		}
		d := a.goalDomain(id)
		if d == "" {
			return false, nil
		}
		inGroup := a.has(d, "member_group", "group", "g1", "member") && a.members[user]
		admin := a.has(d, "class", "agentclass", groupNS+"/assistant", "") && a.admins[user]
		return (inGroup && a.managerTuple(id, user)) || admin, nil
	}
	if a.refusedPerms[typ+":"+id+":"+perm+":"+user] {
		return false, nil
	}
	return !a.unreadable[typ+":"+id+":"+user], nil
}

type groupFixture struct {
	*serverFixture
	auth *groupAuthority
}

// newGroupFixture is a session in a group conversation ("grouptest" kind,
// group g1) whose class opts in to group goals. Alice is a member.
func newGroupFixture(t *testing.T) *groupFixture {
	t.Helper()
	groupTestMembershipErr, groupTestAudience, groupTestAudienceErr = nil, nil, nil
	t.Cleanup(func() { groupTestMembershipErr, groupTestAudience, groupTestAudienceErr = nil, nil, nil })
	scheme := runtime.NewScheme()
	require.NoError(t, v1.AddToScheme(scheme))
	class := &v1.AgentClass{ObjectMeta: metav1.ObjectMeta{Namespace: groupNS, Name: "assistant", UID: "class-uid"}, Spec: v1.AgentClassSpec{Capabilities: map[string]apiext.JSON{"goals": {Raw: []byte(`{"groups":true}`)}}}}
	class.Spec.Authz = &v1.AuthzBlock{InformationLeakage: &v1.InformationLeakagePolicy{Mode: "enforcing"}}
	binding := &v1.ChannelBinding{Name: "team-chat", Kind: "grouptest", Key: "thread:1", External: map[string]string{"group": "g1"}}
	sess := &v1.AgentSession{ObjectMeta: metav1.ObjectMeta{Namespace: groupNS, Name: "session", UID: "session-uid"}, Spec: v1.AgentSessionSpec{Class: "assistant", InputChannel: binding}}
	ch := &v1.Channel{ObjectMeta: metav1.ObjectMeta{Namespace: groupNS, Name: "team-chat", UID: "channel-uid"}, Spec: v1.ChannelSpec{Kind: "grouptest"}}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	signer := provenance.NewSigner(priv, "system:channelsd")
	keys := provenance.MapKeyLookup{{Publisher: "system:channelsd", KeyID: signer.KeyID()}: pub}
	mem := memory.NewLocal(meminmem.NewBackend(), memory.WithProvenanceVerifier(provenance.NewWriteVerifier(keys)))
	auth := newGroupAuthority()
	auth.members["alice"] = true
	toks := tokens.NewRegistry()
	toks.Set(memory.NamespacedName{Namespace: groupNS, Name: "session"}, "token", "")
	s := &Server{ActorWait: -1, Reader: fake.NewClientBuilder().WithScheme(scheme).WithObjects(class, sess, ch).Build(), Memory: mem, Tokens: toks, Keys: keys, Auth: auth,
		Service: &core.Service{Store: goalinmem.New(), GroupLinks: registry.GroupMemberLinks}}
	s.Service.Auth = s
	return &groupFixture{serverFixture: &serverFixture{s: s, mem: mem, signed: provenance.NewSigningMemory(mem, signer), keys: keys}, auth: auth}
}

// do posts r and returns the status, the decoded response, and the body text
// (the refusal reason on a non-200).
func (f *groupFixture) do(t *testing.T, r core.Request) (int, core.Response, string) {
	t.Helper()
	b, err := json.Marshal(r)
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/goals/team/session", bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer token")
	w := httptest.NewRecorder()
	f.s.ServeHTTP(w, req)
	var res core.Response
	if w.Code == 200 {
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &res))
	}
	return w.Code, res, w.Body.String()
}

// as runs request r on behalf of the attested author of turnRef.
func (f *groupFixture) as(t *testing.T, turnRef string, r core.Request) (int, core.Response, string) {
	t.Helper()
	r.TurnRef = turnRef
	return f.do(t, r)
}

func (f *groupFixture) taint(t *testing.T, typ, id, perm string) {
	t.Helper()
	b, err := json.Marshal(infoleakagetaint.TaintRecord{ResourceType: typ, ResourceID: id, Permission: perm})
	require.NoError(t, err)
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	signer := provenance.NewSigner(priv, "session:team/session")
	f.keys[provenance.PubKeyRef{Publisher: signer.Publisher(), KeyID: signer.KeyID()}] = pub
	_, err = provenance.NewSigningMemory(f.mem, signer).Put(memory.WithSystemApproval(context.Background(), "test"), memory.Entry{Scope: memory.Scope{Kind: "session", ID: "team/session"}, Kind: infoleakagetaint.KindName, ID: "ilt-" + id, CreatedAt: time.Now().UTC(), Content: b})
	require.NoError(t, err)
}

func groupDomainFixture() core.Domain {
	return core.Domain{Namespace: groupNS, Owner: groupOwner, Class: "assistant", ClassUID: "class-uid"}
}

func groupResource() string { return ResourceType + ":" + groupDomainFixture().ID() }

func groupCreate(id string) core.Request {
	return core.Request{Operation: "create", Scope: "group", Resource: groupResource(), Create: core.CreateRequest{RequestID: id, Title: "Standup", Outcome: "Summarize the week"}}
}

func groupUpdate(id string, g core.Goal, action string) core.Request {
	return core.Request{Operation: "update", Scope: "group", Resource: groupResource(), Change: core.Change{RequestID: id, ID: g.ID, Revision: g.Revision, Action: action}}
}

func managerChange(id string, g core.Goal, action, who string) core.Request {
	r := groupUpdate(id, g, action)
	r.Change.Manager = &who
	return r
}

// createAsAlice creates a group goal as alice (turn r1) and returns it.
func (f *groupFixture) createAsAlice(t *testing.T) core.Goal {
	t.Helper()
	f.turnProof(t, "alice", "r1")
	status, r, body := f.as(t, "r1", groupCreate("create"))
	require.Equal(t, 200, status, body)
	return *r.Goal
}

func TestGroupQualifies(t *testing.T) {
	cases := []struct {
		name      string
		goals     string
		mode      string
		kind      string
		syncErr   error
		ceiling   *v1.OwnerCeiling
		wantInMsg string
	}{
		{name: "opted in, agent identity, synced kind: qualifies", goals: `{"groups":true}`, kind: "grouptest"},
		{name: "explicit agent identity: qualifies", goals: `{"groups":true}`, mode: "agent", kind: "grouptest"},
		{name: "groups unset: not enabled", goals: `{}`, kind: "grouptest", wantInMsg: "group goals are not enabled for this agent"},
		{name: "userPassthrough: refused naming the mode", goals: `{"groups":true}`, mode: "userPassthrough", kind: "grouptest", wantInMsg: "userPassthrough"},
		{name: "ask: refused naming the mode", goals: `{"groups":true}`, mode: "ask", kind: "grouptest", wantInMsg: `"ask"`},
		{name: "kind without the contract: refused naming the kind", goals: `{"groups":true}`, kind: "plaintest", wantInMsg: `channel kind "plaintest" cannot host group goals`},
		{name: "membership not synced: refused with the kind's reason", goals: `{"groups":true}`, kind: "grouptest", syncErr: errors.New("directory sync is not Ready"), wantInMsg: "directory sync is not Ready"},
		{name: "empty owner ceiling (default mode): qualifies", goals: `{"groups":true}`, kind: "grouptest", ceiling: &v1.OwnerCeiling{}},
		{name: "fixed owner ceiling: refused, it would own the runs instead of the managers", goals: `{"groups":true}`, kind: "grouptest", ceiling: &v1.OwnerCeiling{Fixed: "group:admins#member"},
			wantInMsg: "group goals need runs owned by the goal's managers; this agent's session-owner policy (fixed) would override them"},
		{name: "starterOnly owner ceiling: refused, runs have no starter to own them", goals: `{"groups":true}`, kind: "grouptest", ceiling: &v1.OwnerCeiling{StarterOnly: true},
			wantInMsg: "group goals need runs owned by the goal's managers; this agent's session-owner policy (starterOnly) would override them"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newGroupFixture(t)
			groupTestMembershipErr = tc.syncErr
			class := &v1.AgentClass{Spec: v1.AgentClassSpec{IdentityMode: tc.mode, Capabilities: map[string]apiext.JSON{"goals": {Raw: []byte(tc.goals)}}}}
			if tc.ceiling != nil {
				class.Spec.Authz = &v1.AuthzBlock{OwnerCeiling: tc.ceiling}
			}
			ch := &v1.Channel{ObjectMeta: metav1.ObjectMeta{Namespace: groupNS, Name: "c"}, Spec: v1.ChannelSpec{Kind: tc.kind}}
			err := f.s.groupQualifies(context.Background(), class, ch)
			if tc.wantInMsg == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorIs(t, err, core.ErrDenied)
			assert.Contains(t, err.Error(), tc.wantInMsg)
		})
	}
}

func TestGroupDomainRefusesAOneToOneConversation(t *testing.T) {
	f := newGroupFixture(t)
	var sess v1.AgentSession
	require.NoError(t, f.s.Reader.Get(context.Background(), client.ObjectKey{Namespace: groupNS, Name: "session"}, &sess))
	var class v1.AgentClass
	require.NoError(t, f.s.Reader.Get(context.Background(), client.ObjectKey{Namespace: groupNS, Name: "assistant"}, &class))

	d, err := f.s.groupDomain(context.Background(), &sess, &class)
	require.NoError(t, err)
	assert.Equal(t, groupDomainFixture(), d, "the owner is the conversation's member ref, resolved from the binding")

	delete(sess.Spec.InputChannel.External, "group")
	_, err = f.s.groupDomain(context.Background(), &sess, &class)
	require.ErrorIs(t, err, core.ErrDenied)
	assert.Contains(t, err.Error(), "this conversation is not a group")
}

func TestGroupCreateWritesGroupTuplesNotOwner(t *testing.T) {
	f := newGroupFixture(t)
	g := f.createAsAlice(t)
	id := groupDomainFixture().ID()
	assert.Equal(t, groupOwner, g.Domain.Owner)
	assert.Equal(t, []string{"alice"}, g.Managers)
	assert.True(t, f.auth.has(id, "member_group", "group", "g1", "member"), "member_group names the conversation's members")
	assert.True(t, f.auth.has(id, "class", "agentclass", groupNS+"/assistant", ""), "class anchors the platform-admin fallback")
	assert.True(t, f.auth.managerTuple(g.ID, "alice"), "the creator is the first manager, on the goal")
	assert.True(t, f.auth.hasOn(GoalResourceType, g.ID, "domain", ResourceType, id, ""), "the goal names its domain")
	for tuple := range f.auth.tuples {
		if tuple.ResourceType == ResourceType {
			assert.NotEqual(t, "manager", tuple.Relation, "managers are per goal, never on the domain: %+v", tuple)
		}
	}
	for tuple := range f.auth.tuples {
		assert.NotEqual(t, "owner", tuple.Relation, "a group domain never gets an #owner tuple: %+v", tuple)
	}
	require.NotEmpty(t, f.auth.grants)
	for _, b := range f.auth.grants {
		assert.Equal(t, authz.SlotOccupancyMulti, b.Occupancy, "the group binding never takes the session's single private slot")
	}
}

func TestPrivateAndGroupGoalsInOneSession(t *testing.T) {
	f := newGroupFixture(t)
	f.turnProof(t, "alice", "r1")
	status, private, body := f.as(t, "r1", core.Request{Operation: "list"})
	require.Equal(t, 200, status, body)
	status, r, body := f.as(t, "r1", core.Request{Operation: "create", Resource: private.Resource, Create: core.CreateRequest{RequestID: "private", Title: "Mine", Outcome: "Just me"}})
	require.Equal(t, 200, status, body)
	assert.Equal(t, "alice", r.Goal.Domain.Owner)
	assert.Empty(t, r.Goal.Managers)

	status, r, body = f.as(t, "r1", groupCreate("group"))
	require.Equal(t, 200, status, body)
	assert.Equal(t, groupOwner, r.Goal.Domain.Owner)
	assert.NotEqual(t, private.Resource, r.Resource)

	var classTuples int
	for tuple := range f.auth.tuples {
		if tuple.Relation == "class" {
			classTuples++
			assert.Equal(t, groupDomainFixture().ID(), tuple.ResourceID, "class is written on the group domain only, never a private one")
		}
	}
	assert.Equal(t, 1, classTuples)
	var single, multi int
	for _, b := range f.auth.grants {
		if b.Occupancy == authz.SlotOccupancyMulti {
			multi++
		} else {
			single++
		}
	}
	assert.Positive(t, single, "private scope keeps its pinned single slot")
	assert.Positive(t, multi, "group scope binds its own, unpinned slot")

	status, r, body = f.as(t, "r1", core.Request{Operation: "list", Scope: "group"})
	require.Equal(t, 200, status, body)
	require.Len(t, r.Page.Goals, 1, "group list shows only the group's goal")
	assert.Equal(t, "Standup", r.Page.Goals[0].Title)
}

func TestGroupCreateRefusesANonMember(t *testing.T) {
	f := newGroupFixture(t)
	f.turnProof(t, "mallory", "r1")
	status, _, body := f.as(t, "r1", groupCreate("create"))
	assert.Equal(t, http.StatusForbidden, status)
	assert.Contains(t, body, "not a member of this conversation's group")
}

func TestGroupMemberAccess(t *testing.T) {
	f := newGroupFixture(t)
	g := f.createAsAlice(t)
	status, r, body := f.as(t, "r1", groupUpdate("activate", g, "activate"))
	require.Equal(t, 200, status, body)
	g = *r.Goal

	f.auth.members["bob"] = true
	f.turnProof(t, "bob", "r2")
	status, r, body = f.as(t, "r2", core.Request{Operation: "list", Scope: "group"})
	require.Equal(t, 200, status, body)
	assert.Len(t, r.Page.Goals, 1, "a member lists the group's goals")
	status, _, body = f.as(t, "r2", core.Request{Operation: "get", Scope: "group", ID: g.ID})
	assert.Equal(t, 200, status, body)

	revise := groupUpdate("revise", g, "revise")
	title := "Retitled"
	revise.Change.Title = &title
	status, _, body = f.as(t, "r2", revise)
	assert.Equal(t, http.StatusForbidden, status, "a member who is not a manager cannot revise: %s", body)

	status, r, body = f.as(t, "r2", groupUpdate("pause", g, "pause"))
	require.Equal(t, 200, status, "any member may pause: %s", body)
	g = *r.Goal
	assert.Equal(t, core.Paused, g.State)

	status, _, body = f.as(t, "r2", groupUpdate("resume", g, "resume"))
	assert.Equal(t, http.StatusForbidden, status, "resume needs manage: %s", body)

	f.turnProof(t, "alice", "r3")
	status, r, body = f.as(t, "r3", groupUpdate("resume", g, "resume"))
	require.Equal(t, 200, status, "the manager may resume: %s", body)
	assert.Equal(t, core.Active, r.Goal.State)
}

func TestGroupRefusalReasonReachesTheCaller(t *testing.T) {
	f := newGroupFixture(t)
	f.turnProof(t, "alice", "r1")
	f.auth.members["bob"] = true
	f.turnProof(t, "bob", "r2")
	status, _, body := f.as(t, "r1", core.Request{Operation: "list", Scope: "group"})
	assert.Equal(t, http.StatusForbidden, status)
	assert.Contains(t, body, "ask again", "the actor's refusal text is the tool result, not a generic 404")
}

func TestGroupManagers(t *testing.T) {
	t.Run("a manager adds a member: tuple written", func(t *testing.T) {
		f := newGroupFixture(t)
		g := f.createAsAlice(t)
		f.auth.members["bob"] = true
		status, r, body := f.as(t, "r1", managerChange("add-bob", g, "add_manager", "bob"))
		require.Equal(t, 200, status, body)
		assert.Equal(t, []string{"alice", "bob"}, r.Goal.Managers)
		assert.True(t, f.auth.managerTuple(g.ID, "bob"))
	})
	t.Run("adding a non-member: refused", func(t *testing.T) {
		f := newGroupFixture(t)
		g := f.createAsAlice(t)
		status, _, body := f.as(t, "r1", managerChange("add-carol", g, "add_manager", "carol"))
		assert.Equal(t, http.StatusForbidden, status)
		assert.Contains(t, body, "not a member of this conversation's group")
	})
	t.Run("a member who is not a manager cannot add one", func(t *testing.T) {
		f := newGroupFixture(t)
		g := f.createAsAlice(t)
		f.auth.members["bob"] = true
		f.turnProof(t, "bob", "r2")
		status, _, body := f.as(t, "r2", managerChange("add-self", g, "add_manager", "bob"))
		assert.Equal(t, http.StatusForbidden, status, body)
	})
	t.Run("removing the last manager: refused even with a platform admin", func(t *testing.T) {
		f := newGroupFixture(t)
		g := f.createAsAlice(t)
		f.auth.admins["root"] = true
		status, _, body := f.as(t, "r1", managerChange("remove-self", g, "remove_manager", "alice"))
		assert.Equal(t, http.StatusBadRequest, status)
		assert.Contains(t, body, core.LastManagerRefusal)
	})
	t.Run("removing the last manager still in the group: refused", func(t *testing.T) {
		f := newGroupFixture(t)
		g := f.createAsAlice(t)
		f.auth.members["bob"] = true
		status, r, body := f.as(t, "r1", managerChange("add-bob", g, "add_manager", "bob"))
		require.Equal(t, 200, status, body)
		g = *r.Goal
		f.auth.members["bob"] = false // bob left the group; he is listed but no longer manages
		f.auth.admins["root"] = true
		status, _, body = f.as(t, "r1", managerChange("remove-self", g, "remove_manager", "alice"))
		assert.Equal(t, http.StatusBadRequest, status)
		assert.Contains(t, body, core.LastManagerRefusal)
	})
	t.Run("removing one of two managers: tuple deleted", func(t *testing.T) {
		f := newGroupFixture(t)
		g := f.createAsAlice(t)
		f.auth.members["bob"] = true
		status, r, body := f.as(t, "r1", managerChange("add-bob", g, "add_manager", "bob"))
		require.Equal(t, 200, status, body)
		status, r, body = f.as(t, "r1", managerChange("remove-bob", *r.Goal, "remove_manager", "bob"))
		require.Equal(t, 200, status, body)
		assert.Equal(t, []string{"alice"}, r.Goal.Managers)
		assert.False(t, f.auth.managerTuple(g.ID, "bob"))
	})
	t.Run("a retry after a failed tuple write rewrites the tuple from Goal.Managers", func(t *testing.T) {
		f := newGroupFixture(t)
		g := f.createAsAlice(t)
		f.auth.members["bob"] = true
		f.auth.failManagerWrites = true
		add := managerChange("add-bob", g, "add_manager", "bob")
		status, _, _ := f.as(t, "r1", add)
		require.Equal(t, http.StatusInternalServerError, status, "the commit landed but the tuple did not")
		assert.False(t, f.auth.managerTuple(g.ID, "bob"))
		f.auth.failManagerWrites = false
		status, r, body := f.as(t, "r1", add)
		require.Equal(t, 200, status, body)
		assert.Equal(t, []string{"alice", "bob"}, r.Goal.Managers)
		assert.Equal(t, g.Revision, r.Goal.Revision, "a manager change keeps the revision its runs are bound to; the retry replays the receipt")
		assert.True(t, f.auth.managerTuple(g.ID, "bob"))
	})
	t.Run("a replayed old request never restores a manager removed since", func(t *testing.T) {
		f := newGroupFixture(t)
		g := f.createAsAlice(t)
		f.auth.members["carol"] = true
		add := managerChange("add-carol", g, "add_manager", "carol")
		status, r, body := f.as(t, "r1", add)
		require.Equal(t, 200, status, body)
		status, _, body = f.as(t, "r1", managerChange("remove-carol", *r.Goal, "remove_manager", "carol"))
		require.Equal(t, 200, status, body)
		require.False(t, f.auth.managerTuple(g.ID, "carol"))
		status, _, body = f.as(t, "r1", add) // the receipt still holds the old snapshot
		require.Equal(t, 200, status, body)
		assert.False(t, f.auth.managerTuple(g.ID, "carol"), "tuples follow the goal's current managers, not a replayed snapshot")
	})
	t.Run("a removal whose tuple delete fails: the removed manager is still refused, and the next write deletes the tuple", func(t *testing.T) {
		f := newGroupFixture(t)
		g := f.createAsAlice(t)
		f.auth.members["bob"] = true
		status, r, body := f.as(t, "r1", managerChange("add-bob", g, "add_manager", "bob"))
		require.Equal(t, 200, status, body)
		f.auth.failDeletes = true
		status, _, _ = f.as(t, "r1", managerChange("remove-bob", *r.Goal, "remove_manager", "bob"))
		require.Equal(t, http.StatusInternalServerError, status, "the commit landed but the delete did not")
		require.True(t, f.auth.managerTuple(g.ID, "bob"), "the stale tuple is still there")
		f.auth.failDeletes = false

		var current core.Goal
		status, rr, body := f.as(t, "r1", core.Request{Operation: "get", Scope: "group", ID: g.ID})
		require.Equal(t, 200, status, body)
		current = *rr.Goal
		assert.Equal(t, []string{"alice"}, current.Managers)

		f.turnProof(t, "bob", "r2")
		status, _, body = f.as(t, "r2", groupUpdate("bob-activates", current, "activate"))
		assert.Equal(t, http.StatusForbidden, status, "a stale tuple alone never grants manage: %s", body)

		f.turnProof(t, "alice", "r3")
		status, _, body = f.as(t, "r3", groupUpdate("activate", current, "activate"))
		require.Equal(t, 200, status, body)
		assert.False(t, f.auth.managerTuple(g.ID, "bob"), "the next write reconciles the tuple away")
	})
	t.Run("a platform admin who is not listed may still step in", func(t *testing.T) {
		f := newGroupFixture(t)
		g := f.createAsAlice(t)
		f.auth.admins["root"] = true
		f.turnProof(t, "root", "r2")
		status, _, body := f.as(t, "r2", groupUpdate("activate", g, "activate"))
		assert.Equal(t, 200, status, body)
	})
	t.Run("a tuple left behind by an interleaved add and remove is reconciled away", func(t *testing.T) {
		f := newGroupFixture(t)
		g := f.createAsAlice(t)
		// The losing side of a race wrote carol's tuple after her removal.
		f.auth.tuples[authz.Relation{ResourceType: GoalResourceType, ResourceID: g.ID, Relation: "manager", SubjectType: "user", SubjectID: "carol"}] = true
		status, _, body := f.as(t, "r1", groupUpdate("activate", g, "activate"))
		require.Equal(t, 200, status, body)
		assert.False(t, f.auth.managerTuple(g.ID, "carol"))
		assert.True(t, f.auth.managerTuple(g.ID, "alice"))
	})
	t.Run("execution consent survives a manager change", func(t *testing.T) {
		f := newGroupFixture(t)
		g := f.createAsAlice(t)
		// Plant consent directly: this is about the manager path not clearing
		// it, not about how it was requested.
		stored, err := f.s.Service.Store.Get(context.Background(), g.Domain, g.ID)
		require.NoError(t, err)
		prior, err := core.EncodePrior(stored)
		require.NoError(t, err)
		g.Execution = &core.ExecutionConsent{RequestID: "consent", RequestRevision: g.Revision, Digest: "d"}
		g.Revision++
		g, err = f.s.Service.Store.Commit(context.Background(), core.Mutation{Goal: g, Expected: g.Revision - 1, Prior: prior, RequestID: "plant", Hash: "plant", Event: core.Event{ID: "goalev-plant", Goal: g, Action: "request_execution", Session: "team/session", Proof: "p"}})
		require.NoError(t, err)
		f.auth.members["bob"] = true
		status, r, body := f.as(t, "r1", managerChange("add-bob", g, "add_manager", "bob"))
		require.Equal(t, 200, status, body)
		require.NotNil(t, r.Goal.Execution, "a manager change keeps execution consent")
		assert.Equal(t, g.Execution.Digest, r.Goal.Execution.Digest)
	})
}

func TestGroupReadsNeverCheckTheSubjectSetAsAUser(t *testing.T) {
	f := newGroupFixture(t)
	f.taint(t, "document", "notes", "view")
	groupTestAudience = []string{"alice"}
	g := f.createAsAlice(t)
	status, _, body := f.as(t, "r1", core.Request{Operation: "get", Scope: "group", ID: g.ID})
	require.Equal(t, 200, status, body)
	status, _, body = f.as(t, "r1", core.Request{Operation: "list", Scope: "group"})
	require.Equal(t, 200, status, body)
	assert.NotContains(t, f.auth.checkedUsers, groupOwner, "the member subject-set is never checked as if it were a user")

	a, err := f.s.groupActor(operatorCtx(), groupNS, "session", "r1")
	require.NoError(t, err)
	f.auth.checkedUsers = nil
	require.NoError(t, f.s.ReadGoal(operatorCtx(), a, core.Goal{Sources: []core.Source{{ResourceType: "document", ResourceID: "notes", Permission: "view"}}}))
	assert.Equal(t, []string{"alice"}, f.auth.checkedUsers, "group reads check the acting member")
	_, err = f.s.Sources(operatorCtx(), a)
	require.NoError(t, err)
	assert.NotContains(t, f.auth.checkedUsers, groupOwner)
}

func TestGroupSourcesAgainstAudience(t *testing.T) {
	cases := []struct {
		name        string
		audience    []string
		audienceErr error
		unreadable  string
		wantInMsg   string
	}{
		{name: "everyone in the audience can read the session's sources: ok", audience: []string{"alice", "dave"}},
		{name: "an audience member cannot read a source: denied naming its type", audience: []string{"alice", "dave"}, unreadable: "document:secret:dave", wantInMsg: "document"},
		{name: "audience unavailable: denied", audienceErr: errors.New("resolver not wired"), wantInMsg: "audience"},
		{name: "audience empty: denied", audience: nil, wantInMsg: "audience"},
		{name: "SpiceDB-form subjects are compared as users", audience: []string{"user:alice", "user:dave"}, unreadable: "document:secret:dave", wantInMsg: "document"},
		{name: "a wildcard audience cannot be verified: denied", audience: []string{"*"}, wantInMsg: "audience"},
	}
	for _, tc := range cases {
		for _, op := range []string{"create", "request_execution"} {
			t.Run(op+": "+tc.name, func(t *testing.T) {
				f := newGroupFixture(t)
				f.turnProof(t, "alice", "r1")
				var g core.Goal
				if op == "request_execution" {
					groupTestAudience = []string{"alice"}
					status, r, body := f.as(t, "r1", groupCreate("create"))
					require.Equal(t, 200, status, body)
					g = *r.Goal
				}
				f.taint(t, "document", "secret", "view")
				groupTestAudience, groupTestAudienceErr = tc.audience, tc.audienceErr
				if tc.unreadable != "" {
					f.auth.unreadable[tc.unreadable] = true
				}
				req := groupCreate("create-sourced")
				if op == "request_execution" {
					req = core.Request{Operation: "request_execution", Scope: "group", Resource: groupResource(), Execution: core.ExecutionRequest{RequestID: "exec", ID: g.ID, Revision: g.Revision}}
				}
				status, _, body := f.as(t, "r1", req)
				if tc.wantInMsg == "" {
					if op == "create" {
						assert.Equal(t, 200, status, body)
					} else {
						// Group execution itself arrives with its own validation;
						// the source check must not be what refuses it.
						assert.NotContains(t, body, "cannot read")
					}
					return
				}
				assert.Equal(t, http.StatusForbidden, status)
				assert.Contains(t, body, tc.wantInMsg)
				assert.NotContains(t, body, "dave", "the refusal names the resource type, not who cannot read it")
			})
		}
	}
}

func TestGroupScopeRefusedWhenNotOptedIn(t *testing.T) {
	f := newGroupFixture(t)
	var class v1.AgentClass
	require.NoError(t, f.s.Reader.Get(context.Background(), client.ObjectKey{Namespace: groupNS, Name: "assistant"}, &class))
	class.Spec.Capabilities["goals"] = apiext.JSON{Raw: []byte(`{}`)}
	require.NoError(t, f.s.Reader.(client.Client).Update(context.Background(), &class))
	f.turnProof(t, "alice", "r1")
	status, _, body := f.as(t, "r1", core.Request{Operation: "list", Scope: "group"})
	assert.Equal(t, http.StatusForbidden, status)
	assert.Contains(t, body, "group goals are not enabled for this agent")
	status, _, body = f.as(t, "r1", core.Request{Operation: "list"})
	assert.Equal(t, 200, status, "private scope is unaffected: %s", body)
}

func TestGroupScopeIsSelectedByResource(t *testing.T) {
	f := newGroupFixture(t)
	g := f.createAsAlice(t)
	// An update naming the group resource with no scope addresses the group
	// domain; it must not fall into the private path and be refused there.
	req := groupUpdate("activate", g, "activate")
	req.Scope = ""
	status, r, body := f.as(t, "r1", req)
	require.Equal(t, 200, status, body)
	assert.Equal(t, core.Active, r.Goal.State)

	status, _, body = f.as(t, "r1", core.Request{Operation: "list", Scope: "everyone"})
	assert.Equal(t, http.StatusBadRequest, status, body)
	assert.True(t, strings.Contains(body, "scope"), body)
}

func TestGroupPauseIsNeverBlockedBySources(t *testing.T) {
	cases := []struct {
		name  string
		setup func(f *groupFixture)
	}{
		{name: "audience resolver errors", setup: func(*groupFixture) { groupTestAudienceErr = errors.New("resolver down") }},
		{name: "an audience member cannot read a session source", setup: func(f *groupFixture) {
			groupTestAudience = []string{"alice", "dave"}
			f.auth.unreadable["document:secret:dave"] = true
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newGroupFixture(t)
			g := f.createAsAlice(t)
			status, r, body := f.as(t, "r1", groupUpdate("activate", g, "activate"))
			require.Equal(t, 200, status, body)
			g = *r.Goal
			f.taint(t, "document", "secret", "view")
			tc.setup(f)

			revise := groupUpdate("revise", g, "revise")
			title := "Retitled"
			revise.Change.Title = &title
			status, _, body = f.as(t, "r1", revise)
			assert.Equal(t, http.StatusForbidden, status, "a revise still carries the session's sources: %s", body)

			status, r, body = f.as(t, "r1", groupUpdate("pause", g, "pause"))
			require.Equal(t, 200, status, "pause is an emergency brake: %s", body)
			assert.Equal(t, core.Paused, r.Goal.State)
			assert.Empty(t, r.Goal.Sources, "a pause carries no session source into the goal")

			status, r, body = f.as(t, "r1", groupUpdate("cancel", *r.Goal, "cancel"))
			require.Equal(t, 200, status, "cancel only removes authority: %s", body)
			assert.Equal(t, core.Cancelled, r.Goal.State)
		})
	}
}

func TestGroupManagerOfOneGoalCannotManageAnother(t *testing.T) {
	f := newGroupFixture(t)
	x := f.createAsAlice(t)
	f.auth.members["bob"] = true
	f.turnProof(t, "bob", "r2")
	status, r, body := f.as(t, "r2", groupCreate("bobs"))
	require.Equal(t, 200, status, body)
	y := *r.Goal
	assert.Equal(t, []string{"bob"}, y.Managers)

	status, _, body = f.as(t, "r2", groupUpdate("bob-on-x", x, "activate"))
	assert.Equal(t, http.StatusForbidden, status, "bob manages his goal, not alice's: %s", body)
	status, _, body = f.as(t, "r2", groupUpdate("bob-on-y", y, "activate"))
	assert.Equal(t, 200, status, body)
}
