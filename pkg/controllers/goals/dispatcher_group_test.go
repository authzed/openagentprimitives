package goals

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	domain "github.com/authzed/openagentprimitives/pkg/agent/goals"
	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/plangate"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/registry"
	"github.com/authzed/openagentprimitives/pkg/controllers/agentsession"
	"github.com/authzed/openagentprimitives/pkg/memory"
	goalsqlite "github.com/authzed/openagentprimitives/pkg/memory/goals/sqlite"
	"github.com/authzed/openagentprimitives/pkg/memory/goals/sqlstore"
	meminmem "github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/infoleakagetaint"
	memsqlite "github.com/authzed/openagentprimitives/pkg/memory/sqlite"

	fakekind "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/fake"
)

const (
	groupRunOwner   = "group:g1#member"
	groupRunChannel = "team-chat"
)

// groupRunAuthority serves group goals and allows everything except what a
// test switches off. Managers are the goal's own list.
type groupRunAuthority struct {
	sources     []domain.Source
	dispatchErr error
	// groupDispatchErr is what ValidateGroupDispatch refuses with.
	groupDispatchErr error
}

func (*groupRunAuthority) Authorize(context.Context, domain.Actor, bool) error { return nil }
func (a *groupRunAuthority) Sources(context.Context, domain.Actor) ([]domain.Source, error) {
	return a.sources, nil
}
func (*groupRunAuthority) ReadGoal(context.Context, domain.Actor, domain.Goal) error { return nil }
func (*groupRunAuthority) AuthorizeGroup(context.Context, domain.Actor, domain.GroupAccess) error {
	return nil
}
func (*groupRunAuthority) CheckManagers(context.Context, domain.Actor, domain.Goal, domain.Change) error {
	return nil
}
func (*groupRunAuthority) AuthorizeGoalManager(_ context.Context, a domain.Actor, g domain.Goal) error {
	if !slices.Contains(g.Managers, a.User) {
		return domain.ErrDenied
	}
	return nil
}
func (*groupRunAuthority) Validate(context.Context, domain.Goal, domain.ExecutionTerms) error {
	return nil
}
func (*groupRunAuthority) VerifyDecision(context.Context, domain.Goal, domain.ExecutionDecision) error {
	return nil
}
func (a *groupRunAuthority) AuthorizeDispatch(context.Context, domain.Goal) error {
	return a.dispatchErr
}
func (*groupRunAuthority) ValidateGroupExecution(context.Context, domain.Goal, domain.ExecutionTerms, string) error {
	return nil
}
func (a *groupRunAuthority) ValidateGroupDispatch(context.Context, domain.Goal, domain.ExecutionTerms) error {
	return a.groupDispatchErr
}

// groupRunFixture is a group of approved group goals ready to dispatch, with
// their pinned Channel in a fake cluster. The origin session the goals were
// created in does not exist: a group run must never need it.
type groupRunFixture struct {
	store *sqlstore.Store
	svc   *domain.Service
	auth  *groupRunAuthority
	mem   memory.Memory
	k8s   client.Client
	clock *time.Time
	alice domain.Actor
	n     int
	runs  *runGrants
}

// runGrants records the run-session grants the dispatcher writes; err, when
// set, is what the grant refuses with.
type runGrants struct {
	granted []runGrant
	err     error
}

type runGrant struct {
	goal, digest, session string
	expires               time.Time
}

func (r *runGrants) GrantGroupRun(_ context.Context, g domain.Goal, session string, expires time.Time) error {
	if r.err != nil {
		return r.err
	}
	r.granted = append(r.granted, runGrant{goal: g.ID, digest: g.Execution.Digest, session: session, expires: expires})
	return nil
}

func groupRunActor(user string) domain.Actor {
	return domain.Actor{Domain: domain.Domain{Namespace: "team", Owner: groupRunOwner, Class: "assistant", ClassUID: "class-uid"}, Session: "team/origin", Proof: "turn", User: user}
}

func newGroupRunFixture(t *testing.T, sources ...domain.Source) *groupRunFixture {
	t.Helper()
	return newGroupRunFixtureOnKind(t, "fake", sources...)
}

// newGroupRunFixtureOnKind is newGroupRunFixture with the goal's Channel on
// the named kind.
func newGroupRunFixtureOnKind(t *testing.T, kind string, sources ...domain.Source) *groupRunFixture {
	t.Helper()
	ctx := context.Background()
	db, err := memsqlite.NewClient(filepath.Join(t.TempDir(), "runs.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	store := goalsqlite.New(db.DB())
	require.NoError(t, store.Migrate(ctx))
	clock := time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC)
	f := &groupRunFixture{store: store, auth: &groupRunAuthority{sources: sources}, clock: &clock, alice: groupRunActor("alice"), runs: &runGrants{}}
	f.svc = &domain.Service{Store: store, Auth: f.auth, ExecutionAuth: f.auth, Now: func() time.Time { return *f.clock }, GroupLinks: func() []string { return []string{"group#member"} }}
	f.mem = memory.NewLocal(meminmem.NewBackend())
	scheme := runtime.NewScheme()
	require.NoError(t, v1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	ch := &v1.Channel{ObjectMeta: metav1.ObjectMeta{Namespace: "team", Name: groupRunChannel, UID: "channel-uid"}, Spec: v1.ChannelSpec{Kind: kind}}
	created := 0
	f.k8s = fake.NewClientBuilder().WithScheme(scheme).WithObjects(ch).WithInterceptorFuncs(interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
		if sess, ok := obj.(*v1.AgentSession); ok {
			created++
			sess.UID = types.UID(fmt.Sprintf("run-uid-%d", created))
			sess.CreationTimestamp = metav1.NewTime(*f.clock)
		}
		return c.Create(ctx, obj, opts...)
	}}).Build()
	return f
}

// approve creates, activates and approves one group goal due one minute from
// now, with alice as its manager and approver, and schedules its occurrence.
func (f *groupRunFixture) approve(t *testing.T) (domain.Goal, domain.Occurrence) {
	t.Helper()
	ctx := context.Background()
	f.n++
	id := func(s string) string { return fmt.Sprintf("%s-%d", s, f.n) }
	g, err := f.svc.Create(ctx, f.alice, domain.CreateRequest{RequestID: id("create"), Title: "Standup", Outcome: "Summarize the week"})
	require.NoError(t, err)
	g, err = f.svc.Update(ctx, f.alice, domain.Change{ID: g.ID, Revision: g.Revision, RequestID: id("activate"), Action: "activate"})
	require.NoError(t, err)
	now := *f.clock
	terms := domain.ExecutionTerms{
		ClassDigest: "class-digest", DueAt: now.Add(time.Minute), ExpiresAt: now.Add(24 * time.Hour),
		Bounds:            domain.ExecutionBounds{DurationSeconds: 300, Turns: 10, Tokens: 10000, ApprovalSeconds: 90},
		AllowedOperations: []string{"respond_to_user", "report_goal_result"}, Evidence: []string{"weekly summary"},
		ActionApproval: "standing_plan", StandingPlan: &plangate.Plan{Phases: []plangate.Phase{{Label: "post"}}},
		Group:         &domain.GroupDestination{Channel: groupRunChannel, ChannelUID: "channel-uid", MemberGroup: groupRunOwner},
		AgentIdentity: "bot", AgentIdentityUID: "aid-uid",
	}
	g, err = f.svc.RequestExecution(ctx, f.alice, domain.ExecutionRequest{ID: g.ID, Revision: g.Revision, RequestID: id("request"), Terms: terms})
	require.NoError(t, err)
	g, err = f.svc.DecideExecution(ctx, f.alice.Domain, g.ID, domain.ExecutionDecision{RequestID: id("approval"), Digest: g.Execution.Digest, Owner: "alice", Approved: true, Witness: "test-only-human-witness"})
	require.NoError(t, err)
	o, err := f.store.Schedule(ctx, g)
	require.NoError(t, err)
	return g, o
}

func (f *groupRunFixture) dispatcher() *Dispatcher {
	return &Dispatcher{Service: f.svc, Store: f.store, Client: f.k8s, Reader: f.k8s, Worker: "worker", Now: func() time.Time { return *f.clock }, DeliveryMemory: f.mem, RunGrants: f.runs}
}

// started runs one dispatch tick at the occurrence's due time and returns the
// session it created.
func (f *groupRunFixture) started(t *testing.T, d *Dispatcher, o domain.Occurrence) *v1.AgentSession {
	t.Helper()
	*f.clock = o.DueAt.Add(time.Second)
	require.NoError(t, d.Tick(context.Background()))
	return f.session(t, o)
}

func (f *groupRunFixture) session(t *testing.T, o domain.Occurrence) *v1.AgentSession {
	t.Helper()
	var sess v1.AgentSession
	require.NoError(t, f.k8s.Get(context.Background(), client.ObjectKey{Namespace: "team", Name: o.SessionName}, &sess))
	return &sess
}

func TestGroupRunSessionShape(t *testing.T) {
	sources := []domain.Source{{ResourceType: "agentsession", ResourceID: "team/origin", Permission: "read_transcript"}, {ResourceType: "github_repo", ResourceID: "acme/app", Permission: "read"}}
	f := newGroupRunFixture(t, sources...)
	g, o := f.approve(t)
	require.Equal(t, sources, g.Sources)
	sess := f.started(t, f.dispatcher(), o)

	for _, key := range []string{v1.AnnotationStartedByCanonicalID, v1.AnnotationStartedByEmail, v1.AnnotationStartedByExternalID} {
		assert.NotContains(t, sess.Annotations, key, "a group run is started by no one")
	}
	require.NotNil(t, sess.Spec.InputChannel)
	assert.Equal(t, sess.Spec.InputChannel, sess.Spec.OutputChannel, "the run's conversation is its delivery route")
	host, ok := registry.GroupGoalHostFor("fake")
	require.True(t, ok)
	ref, ok := host.ConversationMemberRef(sess.Spec.OutputChannel)
	assert.True(t, ok)
	assert.Equal(t, groupRunOwner, ref, "the run is anchored in the owning group")
	assert.Equal(t, groupRunChannel, sess.Spec.OutputChannel.Name)
	assert.Equal(t, "fake", sess.Spec.OutputChannel.Kind)
	assert.True(t, strings.HasPrefix(sess.Spec.OutputChannel.Key, "group-run:"), "the kind's RunAnchor chose the key: %q", sess.Spec.OutputChannel.Key)
	assert.Equal(t, "ap.session.team."+o.SessionName, sess.Spec.OutputChannel.NATSSubjectPrefix)
	assert.Equal(t, "fake", sess.Labels[v1.LabelChannelKind])
	assert.Empty(t, sess.Spec.AgentIdentity, "no override: the class's pinned AgentIdentity, re-checked per dispatch, supplies the credentials")
	assert.Contains(t, sess.Spec.Prompt.Inline, "group goal")
	assert.NotContains(t, sess.Spec.Prompt.Inline, "private")

	taints, err := f.mem.Query(memory.WithCaller(memory.WithSystemApproval(context.Background(), "system:operator"), "system:operator"), memory.Query{Scope: memory.Scope{Kind: "session", ID: "team/" + o.SessionName}, Kinds: []string{infoleakagetaint.KindName}, Limit: 100})
	require.NoError(t, err)
	var seeded []domain.Source
	for _, e := range taints.Entries {
		var rec infoleakagetaint.TaintRecord
		require.NoError(t, json.Unmarshal(e.Content, &rec))
		seeded = append(seeded, domain.Source{ResourceType: rec.ResourceType, ResourceID: rec.ResourceID, Permission: rec.Permission})
	}
	assert.ElementsMatch(t, sources, seeded, "one taint per goal source, so the leakage gate checks every post against the conversation")

	require.NoError(t, f.dispatcher().ValidateGoalSession(context.Background(), sess), "the session the dispatcher created validates")
}

func TestGroupRunValidateGoalSession(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(t *testing.T, f *groupRunFixture, sess *v1.AgentSession)
		ok     bool
	}{
		{name: "as created: accepted", ok: true},
		{name: "anchor written back on first post (key and thread changed, same group): accepted", ok: true, mutate: func(_ *testing.T, _ *groupRunFixture, sess *v1.AgentSession) {
			sess.Spec.OutputChannel.Key = "thread:g1:111.222"
			sess.Spec.OutputChannel.External["thread_ts"] = "111.222"
		}},
		{name: "output in another group: refused", mutate: func(_ *testing.T, _ *groupRunFixture, sess *v1.AgentSession) {
			sess.Spec.OutputChannel.External["group"] = "g2"
		}},
		{name: "input in another group: refused", mutate: func(_ *testing.T, _ *groupRunFixture, sess *v1.AgentSession) {
			sess.Spec.InputChannel.External["group"] = "g2"
		}},
		{name: "output on another Channel: refused", mutate: func(_ *testing.T, _ *groupRunFixture, sess *v1.AgentSession) {
			sess.Spec.OutputChannel.Name = "elsewhere"
		}},
		{name: "pinned Channel recreated (UID changed): refused", mutate: func(t *testing.T, f *groupRunFixture, _ *v1.AgentSession) {
			var ch v1.Channel
			require.NoError(t, f.k8s.Get(context.Background(), client.ObjectKey{Namespace: "team", Name: groupRunChannel}, &ch))
			require.NoError(t, f.k8s.Delete(context.Background(), &ch))
			ch.ResourceVersion, ch.UID = "", "other-uid"
			require.NoError(t, f.k8s.Create(context.Background(), &ch))
		}},
		{name: "a started-by user: refused", mutate: func(_ *testing.T, _ *groupRunFixture, sess *v1.AgentSession) {
			sess.Annotations = map[string]string{v1.AnnotationStartedByCanonicalID: "user:alice"}
		}},
		{name: "an identity override: refused", mutate: func(_ *testing.T, _ *groupRunFixture, sess *v1.AgentSession) {
			sess.Spec.AgentIdentity = "bot"
		}},
		{name: "no channel at all: refused", mutate: func(_ *testing.T, _ *groupRunFixture, sess *v1.AgentSession) {
			sess.Spec.InputChannel, sess.Spec.OutputChannel = nil, nil
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newGroupRunFixture(t)
			_, o := f.approve(t)
			d := f.dispatcher()
			sess := f.started(t, d, o)
			if tc.mutate != nil {
				tc.mutate(t, f, sess)
			}
			err := d.ValidateGoalSession(context.Background(), sess)
			if tc.ok {
				assert.NoError(t, err)
				return
			}
			assert.ErrorIs(t, err, domain.ErrDenied)
		})
	}
}

func TestGroupRunOwners(t *testing.T) {
	f := newGroupRunFixture(t)
	g, o := f.approve(t)
	d := f.dispatcher()
	sess := f.started(t, d, o)
	owners, err := d.GoalSessionOwners(context.Background(), sess)
	require.NoError(t, err)
	assert.Equal(t, "agent_goal:"+g.ID+"#manage", owners.Owner, "the run is owned by that goal's managers, not the domain's")
	assert.Equal(t, groupRunOwner, owners.Participant, "members can see and talk in the run")

	impostor := sess.DeepCopy()
	impostor.UID = "impostor"
	_, err = d.GoalSessionOwners(context.Background(), impostor)
	assert.ErrorIs(t, err, domain.ErrDenied, "a session the occurrence never attached gets no goal owners")
}

func TestPrivateRunHasNoGoalOwners(t *testing.T) {
	now := time.Date(2026, 10, 3, 5, 0, 0, 0, time.UTC)
	f := newRunFixture(t, &now, nil)
	owners, err := f.dispatcher("worker").GoalSessionOwners(context.Background(), f.sess)
	require.NoError(t, err)
	assert.Equal(t, agentsession.GoalRunOwners{}, owners, "a private run keeps the channel-policy owner resolution")
}

// TestGroupClaimLimits: two concurrent runs per group, one per private owner,
// as the owner key of a group domain is the whole group.
func TestGroupClaimLimits(t *testing.T) {
	t.Run("group: three due goals, two run at once", func(t *testing.T) {
		f := newGroupRunFixture(t)
		var occs []domain.Occurrence
		for range 3 {
			_, o := f.approve(t)
			occs = append(occs, o)
		}
		*f.clock = occs[0].DueAt.Add(time.Second)
		require.NoError(t, f.dispatcher().Tick(context.Background()))
		running := 0
		for _, o := range occs {
			current, err := f.store.Occurrence(context.Background(), o.ID)
			require.NoError(t, err)
			if current.State == domain.OccurrenceRunning {
				running++
			}
		}
		assert.Equal(t, 2, running)
	})
	t.Run("private: two due goals, one runs at once", func(t *testing.T) {
		now := time.Date(2026, 10, 3, 5, 0, 0, 0, time.UTC)
		f := newRunFixture(t, &now, nil)
		ctx := context.Background()
		// Release the fixture's claimed run so both new goals compete.
		_, err := f.store.Finish(ctx, f.occ, domain.OccurrenceCancelled, now)
		require.NoError(t, err)
		var occs []domain.Occurrence
		for i := range 2 {
			g, err := f.svc.Create(ctx, f.actor, domain.CreateRequest{RequestID: fmt.Sprintf("create-%d", i), Title: "T", Outcome: "O"})
			require.NoError(t, err)
			g, err = f.svc.Update(ctx, f.actor, domain.Change{ID: g.ID, Revision: g.Revision, RequestID: fmt.Sprintf("activate-%d", i), Action: "activate"})
			require.NoError(t, err)
			terms := f.goal.Execution.Terms
			terms.DueAt = now
			g, err = f.svc.RequestExecution(ctx, f.actor, domain.ExecutionRequest{ID: g.ID, Revision: g.Revision, RequestID: fmt.Sprintf("request-%d", i), Terms: terms})
			require.NoError(t, err)
			g, err = f.svc.DecideExecution(ctx, f.actor.Domain, g.ID, domain.ExecutionDecision{RequestID: fmt.Sprintf("approve-%d", i), Digest: g.Execution.Digest, Owner: f.actor.Domain.Owner, Approved: true, Witness: "test-human"})
			require.NoError(t, err)
			o, err := f.store.Schedule(ctx, g)
			require.NoError(t, err)
			occs = append(occs, o)
		}
		now = now.Add(time.Second)
		_ = f.dispatcher("worker").Tick(ctx) // activation fails without an origin session; the claims are what is measured
		claimed := 0
		for _, o := range occs {
			current, err := f.store.Occurrence(ctx, o.ID)
			require.NoError(t, err)
			if current.State != domain.OccurrenceQueued {
				claimed++
			}
		}
		assert.Equal(t, 1, claimed)
	})
}

// TestGroupRunPausedByMemberMidFlight: any member may pause.
// The pause clears the goal's Execution, so the run's next check is refused
// and the occurrence ends with an outcome every member can list.
func TestGroupRunPausedByMemberMidFlight(t *testing.T) {
	f := newGroupRunFixture(t)
	g, o := f.approve(t)
	d := f.dispatcher()
	sess := f.started(t, d, o)
	ctx := context.Background()
	require.NoError(t, d.ValidateGoalSession(ctx, sess))

	bob := groupRunActor("bob") // a member, not a manager
	current, err := f.store.Get(ctx, g.Domain, g.ID)
	require.NoError(t, err)
	paused, err := f.svc.Update(ctx, bob, domain.Change{ID: g.ID, Revision: current.Revision, RequestID: "pause", Action: "pause"})
	require.NoError(t, err)
	require.Nil(t, paused.Execution, "pause clears the standing consent")

	assert.ErrorIs(t, d.ValidateGoalSession(ctx, sess), domain.ErrDenied, "every gate check of the run goes through ValidateGoalSession")
	assert.Error(t, f.svc.DispatchOccurrence(ctx, current, o), "derivation re-checks dispatch and is refused")

	*f.clock = f.clock.Add(6 * time.Second) // past the first tick's lease
	require.NoError(t, d.Tick(ctx))
	page, err := f.svc.Runs(ctx, bob, g.ID, domain.ListRequest{})
	require.NoError(t, err)
	require.Len(t, page.Runs, 1)
	require.NotNil(t, page.Runs[0].Outcome)
	assert.Equal(t, domain.RunPaused, page.Runs[0].Outcome.Reason, "the member who paused sees why the run ended")
}

// refusingReader models today's owner-as-user source check, which cannot read
// as a group's subject set.
type refusingReader struct{ *groupRunAuthority }

func (refusingReader) ReadGoal(context.Context, domain.Actor, domain.Goal) error {
	return domain.ErrDenied
}

// TestReplySourcesGroupSkipsOwnerCheck: a group goal has no owning user to
// read its sources as; the leakage gate covers its posts instead. A private
// goal keeps the owner check.
func TestReplySourcesGroupSkipsOwnerCheck(t *testing.T) {
	reply := &domain.RunReply{Sources: []domain.Source{{ResourceType: "github_repo", ResourceID: "acme/app", Permission: "read"}}}
	d := &Dispatcher{Service: &domain.Service{Auth: refusingReader{&groupRunAuthority{}}}}
	group := domain.Occurrence{Domain: groupRunActor("alice").Domain, Reply: reply}
	assert.NoError(t, d.checkReplySources(context.Background(), group))
	private := domain.Occurrence{Domain: domain.Domain{Namespace: "team", Owner: "alice", Class: "assistant", ClassUID: "class-uid"}, Reply: reply}
	assert.ErrorIs(t, d.checkReplySources(context.Background(), private), domain.ErrDenied)
}

// staleMembershipKind is a group-hosting kind whose membership is no longer
// kept current; unresolvableRefKind one that can no longer build the group's
// binding.
type staleMembershipKind struct{ fakekind.Kind }

func (staleMembershipKind) Name() string { return "fake-stale" }
func (staleMembershipKind) MembershipCurrent(context.Context, client.Reader, *v1.Channel) error {
	return errors.New("directory sync is behind")
}

type unresolvableRefKind struct{ fakekind.Kind }

func (unresolvableRefKind) Name() string { return "fake-noref" }
func (unresolvableRefKind) GroupBinding(*v1.Channel, string) (*v1.ChannelBinding, error) {
	return nil, errors.New("member ref does not resolve")
}

func init() {
	registry.Register(staleMembershipKind{})
	registry.Register(unresolvableRefKind{})
}

// TestGroupRunThatCannotStartEndsWithAReason: when what the
// group's terms pin is definitively gone, the occurrence ends with a reason
// every member can list, releases its capacity, and is not retried each tick.
func TestGroupRunThatCannotStartEndsWithAReason(t *testing.T) {
	setKind := func(kind string) func(*testing.T, *groupRunFixture) {
		return func(t *testing.T, f *groupRunFixture) {
			var ch v1.Channel
			require.NoError(t, f.k8s.Get(context.Background(), client.ObjectKey{Namespace: "team", Name: groupRunChannel}, &ch))
			ch.Spec.Kind = kind
			require.NoError(t, f.k8s.Update(context.Background(), &ch))
		}
	}
	cases := []struct {
		name    string
		arrange func(*testing.T, *groupRunFixture)
		reason  domain.RunReason
	}{
		{
			name: "Channel deleted: group_channel_gone",
			arrange: func(t *testing.T, f *groupRunFixture) {
				require.NoError(t, f.k8s.Delete(context.Background(), &v1.Channel{ObjectMeta: metav1.ObjectMeta{Namespace: "team", Name: groupRunChannel}}))
			},
			reason: domain.RunGroupChannelGone,
		},
		{
			name: "Channel recreated under the same name: group_channel_gone",
			arrange: func(t *testing.T, f *groupRunFixture) {
				var ch v1.Channel
				require.NoError(t, f.k8s.Get(context.Background(), client.ObjectKey{Namespace: "team", Name: groupRunChannel}, &ch))
				require.NoError(t, f.k8s.Delete(context.Background(), &ch))
				ch.ResourceVersion, ch.UID = "", "recreated-uid"
				require.NoError(t, f.k8s.Create(context.Background(), &ch))
			},
			reason: domain.RunGroupChannelGone,
		},
		{
			name:    "Channel's kind no longer hosts group goals: group_not_hostable",
			arrange: setKind("no-such-kind"),
			reason:  domain.RunGroupNotHostable,
		},
		{
			name: "validation: class deleted: group_not_hostable",
			arrange: func(_ *testing.T, f *groupRunFixture) {
				f.auth.groupDispatchErr = fmt.Errorf("%w: %w: execution class no longer exists", domain.ErrDenied, domain.ErrGroupNotHostable)
			},
			reason: domain.RunGroupNotHostable,
		},
		{
			name:    "group's member ref no longer resolves: group_not_hostable",
			arrange: setKind("fake-noref"),
			reason:  domain.RunGroupNotHostable,
		},
		{
			name: "validation: owner ceiling would now own the run: group_not_hostable",
			arrange: func(_ *testing.T, f *groupRunFixture) {
				f.auth.groupDispatchErr = fmt.Errorf("%w: %w: session-owner policy would override", domain.ErrDenied, domain.ErrGroupNotHostable)
			},
			reason: domain.RunGroupNotHostable,
		},
		{
			name: "validation: AgentIdentity deleted or recreated: identity_changed",
			arrange: func(_ *testing.T, f *groupRunFixture) {
				f.auth.groupDispatchErr = fmt.Errorf("%w: %w: AgentIdentity bot", domain.ErrDenied, domain.ErrIdentityChanged)
			},
			reason: domain.RunIdentityChanged,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			f := newGroupRunFixture(t)
			g, o := f.approve(t)
			tc.arrange(t, f)
			d := f.dispatcher()

			*f.clock = o.DueAt.Add(time.Second)
			require.NoError(t, d.Tick(ctx), "a definitive refusal is recorded, not returned for retry")

			page, err := f.svc.Runs(ctx, groupRunActor("bob"), g.ID, domain.ListRequest{})
			require.NoError(t, err)
			require.Len(t, page.Runs, 1)
			require.NotNil(t, page.Runs[0].Outcome, "list_goal_runs shows why the run did not start")
			assert.Equal(t, tc.reason, page.Runs[0].Outcome.Reason)
			assert.Equal(t, domain.OccurrenceCancelled, page.Runs[0].State, "the group's capacity is released")
			var sessions v1.AgentSessionList
			require.NoError(t, f.k8s.List(ctx, &sessions))
			assert.Empty(t, sessions.Items, "no session was created")

			*f.clock = f.clock.Add(10 * time.Second)
			require.NoError(t, d.Tick(ctx), "the next tick has nothing left to retry")
			again, err := f.store.Occurrence(ctx, o.ID)
			require.NoError(t, err)
			assert.Equal(t, page.Runs[0].Outcome, again.Outcome, "the outcome is not rewritten")
		})
	}
}

// TestGroupRunMembershipLagStillRetries: a membership that is not current, or a
// pinned object that could not be read, clears on its own, so it neither records an outcome nor releases capacity.
func TestGroupRunMembershipLagStillRetries(t *testing.T) {
	for name, arrange := range map[string]func(*testing.T, *groupRunFixture){
		"kind's own check in the dispatcher": func(t *testing.T, f *groupRunFixture) {
			var ch v1.Channel
			require.NoError(t, f.k8s.Get(context.Background(), client.ObjectKey{Namespace: "team", Name: groupRunChannel}, &ch))
			ch.Spec.Kind = "fake-stale"
			require.NoError(t, f.k8s.Update(context.Background(), &ch))
		},
		"validation refusal": func(_ *testing.T, f *groupRunFixture) {
			f.auth.groupDispatchErr = fmt.Errorf("%w: %w: directory sync is behind", domain.ErrDenied, domain.ErrMembershipNotCurrent)
		},
		"validation: a pinned object could not be read": func(_ *testing.T, f *groupRunFixture) {
			f.auth.groupDispatchErr = fmt.Errorf("%w: AgentIdentity bot: %w: apiserver timeout", domain.ErrDenied, domain.ErrLookupFailed)
		},
	} {
		t.Run(name+": error returned, no outcome, capacity held", func(t *testing.T) {
			ctx := context.Background()
			f := newGroupRunFixture(t)
			_, o := f.approve(t)
			arrange(t, f)
			*f.clock = o.DueAt.Add(time.Second)
			require.Error(t, f.dispatcher().Tick(ctx))
			got, err := f.store.Occurrence(ctx, o.ID)
			require.NoError(t, err)
			assert.Nil(t, got.Outcome)
			assert.NotEqual(t, domain.OccurrenceCancelled, got.State)
		})
	}
}

// TestGroupRunTransientReadErrorStillRetries: a failed Channel read says
// nothing about the Channel, so it is neither mapped nor does it end the run.
func TestGroupRunTransientReadErrorStillRetries(t *testing.T) {
	ctx := context.Background()
	f := newGroupRunFixture(t)
	g, o := f.approve(t)
	d := f.dispatcher()
	failing := true
	d.Reader = interceptor.NewClient(f.k8s.(client.WithWatch), interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
		if _, ok := obj.(*v1.Channel); ok && failing {
			return apierrors.NewServiceUnavailable("apiserver restarting")
		}
		return c.Get(ctx, key, obj, opts...)
	}})
	*f.clock = o.DueAt.Add(time.Second)
	require.Error(t, d.Tick(ctx), "a transient error is returned so the occurrence is retried")
	got, err := f.store.Occurrence(ctx, o.ID)
	require.NoError(t, err)
	assert.Nil(t, got.Outcome, "nothing is recorded about the group")
	assert.NotEqual(t, domain.OccurrenceCancelled, got.State)

	failing = false
	*f.clock = f.clock.Add(6 * time.Second) // past the lease
	require.NoError(t, d.Tick(ctx))
	page, err := f.svc.Runs(ctx, groupRunActor("bob"), g.ID, domain.ListRequest{})
	require.NoError(t, err)
	require.Len(t, page.Runs, 1)
	assert.Nil(t, page.Runs[0].Outcome, "once the read works the run starts")
	f.session(t, o)
}

// TestGroupRunGrantsItsOwnSession: a group run acts as its own session, so
// the dispatcher grants that session execute on the execution (expiring with
// the run) before creating it — and only once dispatch is authorized. With no
// grant writer, or a refused grant, no session is created.
func TestGroupRunGrantsItsOwnSession(t *testing.T) {
	cases := []struct {
		name      string
		setup     func(f *groupRunFixture, d *Dispatcher)
		started   bool
		granted   bool
		tickError bool
		// ended is the outcome recorded for a run that cannot start; empty
		// means the occurrence is left to retry.
		ended domain.RunReason
	}{
		{name: "authorized: grant for team/<run> expiring with the run, then the session", started: true, granted: true},
		{name: "dispatch refused: no grant, no session", setup: func(f *groupRunFixture, _ *Dispatcher) { f.auth.dispatchErr = domain.ErrDenied }},
		{name: "grant refused: no session, run ends as authority_denied", setup: func(f *groupRunFixture, _ *Dispatcher) { f.runs.err = domain.ErrDenied }, ended: domain.RunAuthorityDenied},
		{name: "no grant writer: no session, error", setup: func(_ *groupRunFixture, d *Dispatcher) { d.RunGrants = nil }, tickError: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newGroupRunFixture(t)
			g, o := f.approve(t)
			d := f.dispatcher()
			if tc.setup != nil {
				tc.setup(f, d)
			}
			*f.clock = o.DueAt.Add(time.Second)
			err := d.Tick(context.Background())
			if tc.tickError {
				assert.Error(t, err)
			}
			var sess v1.AgentSession
			getErr := f.k8s.Get(context.Background(), client.ObjectKey{Namespace: "team", Name: o.SessionName}, &sess)
			assert.Equal(t, tc.started, getErr == nil, "session created")
			if tc.ended != "" {
				ended, err := f.store.Occurrence(context.Background(), o.ID)
				require.NoError(t, err)
				require.NotNil(t, ended.Outcome, "the refused grant is recorded as the run's outcome")
				assert.Equal(t, tc.ended, ended.Outcome.Reason)
				assert.Equal(t, domain.OccurrenceCancelled, ended.State, "the group's capacity is released")
			}
			if !tc.granted {
				assert.Empty(t, f.runs.granted)
				return
			}
			require.Len(t, f.runs.granted, 1)
			assert.Equal(t, runGrant{goal: g.ID, digest: g.Execution.Digest, session: "team/" + o.SessionName, expires: f.clock.Add(time.Duration(g.Execution.Terms.Bounds.DurationSeconds) * time.Second)}, f.runs.granted[0])
		})
	}
}
