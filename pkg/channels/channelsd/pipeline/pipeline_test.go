package pipeline

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr/funcr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/log"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/authz/engine"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions/categories"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/agent" // register agent kind: SpawnsSessionOnInbound()==false, gates agentsession: subject
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/bento" // register bento kind: SpawnsSessionOnInbound()==true
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/browser"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/fake"  // register fake kind for RenderMention lookups + spawn gate
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/local" // register local kind: SpawnsSessionOnInbound()==false
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/outputbind"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/slack" // register slack kind for RenderMention lookups
	"github.com/authzed/openagentprimitives/pkg/controllers/conditions"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/useridentity"
)

type fakeAuthz struct {
	grantedSlots        []authz.SlotBinding
	grantedSlotsSession string
	grantedSlotsExpiry  time.Time
	grantSlotsErr       error
	checkResult         bool
	checkErr            error
	checkInteractFn     func(canonicalID string) (bool, error)

	// delegations models the SpiceDB lineage that agentsession#converse
	// resolves over: one entry per delegation edge, "<parentNS>/<parentName>"
	// -> "<childNS>/<childName>". CheckConverse below evaluates
	// `converse = parent + child` against it EXACTLY as the schema does.
	//
	// It is deliberately NOT wired to checkResult. The blanket-true
	// checkResult is what let the agent-to-agent inbound path ship
	// permanently denied while every test covering it passed: a fake that
	// answers "authorized" for any pair cannot tell a scoped grant from a
	// blanket one. Tests on that path seed this instead.
	delegations             []delegationEdge
	converseErr             error
	converseCalls           int
	deniedResult            bool
	deniedErr               error
	startedByErr            error
	startedByTransientErr   error
	startedByTransientCount int
	participantErr          error
	lastParticipantSubject  string
	lastParticipantUser     string
	participantUsers        []string // every TouchInteractParticipantUser canonical
	participantUserErr      error
	lastDeniedUser          string
	lastStartedBy           identity.CanonicalUserID
	slackUserCalls          int
	startedByCalls          int
	checkCalls              int
	deniedCalls             int
	participantCalls        int
	deniedTouchCalls        int

	// lookupSubjectsResult / lookupSubjectsErr drive LookupSubjects — the
	// start gate's platform-admin fan-out. The nil default models "no
	// admins resolvable"; lookupSubjectsRefs records what was asked.
	lookupSubjectsResult []string
	lookupSubjectsErr    error
	lookupSubjectsRefs   []string

	// lookupResult is the boolean LookupSubjectIncludes returns by
	// default. Tests that need per-(subject, canonical) control can
	// instead set lookupFn, which takes priority.
	lookupResult    bool
	lookupErr       error
	lookupFn        func(subjectRef, canonicalID string) (bool, error)
	lookupLastSubj  string
	lookupLastCanon string
	lookupCallCount int

	// checkApproveResult / checkOwnerResult drive CheckApprove and
	// CheckOwnerOnResource respectively. Tests that need per-resource
	// control can set checkOwnerFn, which takes priority over
	// checkOwnerResult.
	checkApproveResult bool
	checkApproveErr    error
	checkOwnerResult   bool
	checkOwnerErr      error
	checkOwnerFn       func(resType, resID, canonicalID string) (bool, error)
}

func (f *fakeAuthz) TouchSlackUser(_ context.Context, _, _ string) error {
	f.slackUserCalls++
	return nil
}
func (f *fakeAuthz) TouchStartedBy(_ context.Context, _, _ string, canonical identity.CanonicalUserID) error {
	f.startedByCalls++
	f.lastStartedBy = canonical
	// startedByTransientErr fails only the first startedByTransientCount calls —
	// the shape of a SpiceDB blip (a rolling restart, a leader change) that the
	// write's bounded retry exists to ride out. startedByErr fails every call.
	if f.startedByTransientCount > 0 {
		f.startedByTransientCount--
		return f.startedByTransientErr
	}
	return f.startedByErr
}
func (f *fakeAuthz) TouchOwner(_ context.Context, _, _, _ string) error { return nil }
func (f *fakeAuthz) CheckInteract(_ context.Context, _, _ string, canonicalID identity.CanonicalUserID, _ bool) (bool, error) {
	f.checkCalls++
	// checkInteractFn gives per-user control, which the blanket checkResult
	// cannot express — an adoption test needs one author admitted by the
	// declared policy and another refused in the same thread.
	if f.checkInteractFn != nil {
		return f.checkInteractFn(canonicalID.String())
	}
	return f.checkResult, f.checkErr
}

// delegationEdge is one parent -> child delegation, in the "<ns>/<name>" form
// SpiceDB object ids take.
type delegationEdge struct{ parent, child string }

// CheckConverse models `permission converse = parent + child` on the target
// session: the sender is authorized iff it is the target's immediate parent or
// one of its immediate children. Exactly one hop, in either direction — a
// grandparent, a sibling, and an unrelated session all resolve to false, which
// is what makes a "scoped, not blanket" assertion mean something.
func (f *fakeAuthz) CheckConverse(_ context.Context, ns, name, senderNS, senderName string, _ bool) (bool, error) {
	f.converseCalls++
	if f.converseErr != nil {
		return false, f.converseErr
	}
	target, sender := ns+"/"+name, senderNS+"/"+senderName
	for _, e := range f.delegations {
		if (e.parent == sender && e.child == target) || (e.child == sender && e.parent == target) {
			return true, nil
		}
	}
	return false, nil
}

func (f *fakeAuthz) TouchInteractParticipant(_ context.Context, _, _, subject string) error {
	f.participantCalls++
	f.lastParticipantSubject = subject
	return f.participantErr
}
func (f *fakeAuthz) TouchInteractParticipantUser(_ context.Context, _, _ string, canonical identity.CanonicalUserID) error {
	if f.participantUserErr != nil {
		return f.participantUserErr
	}
	f.lastParticipantUser = canonical.String()
	f.participantUsers = append(f.participantUsers, canonical.String())
	return nil
}

// hasParticipantUser reports whether TouchInteractParticipantUser was
// called with the given canonical id.
func (f *fakeAuthz) hasParticipantUser(canonical identity.CanonicalUserID) bool {
	for _, c := range f.participantUsers {
		if c == canonical.String() {
			return true
		}
	}
	return false
}
func (f *fakeAuthz) CheckDenied(_ context.Context, _, _ string, _ identity.CanonicalUserID, _ bool) (bool, error) {
	f.deniedCalls++
	return f.deniedResult, f.deniedErr
}
func (f *fakeAuthz) TouchDeniedUser(_ context.Context, _, _ string, canonical identity.CanonicalUserID) error {
	f.deniedTouchCalls++
	f.lastDeniedUser = canonical.String()
	return nil
}
func (f *fakeAuthz) LookupSubjectIncludes(_ context.Context, subjectRef string, canonicalID identity.CanonicalUserID) (bool, error) {
	f.lookupCallCount++
	f.lookupLastSubj = subjectRef
	f.lookupLastCanon = canonicalID.String()
	if f.lookupFn != nil {
		return f.lookupFn(subjectRef, canonicalID.String())
	}
	return f.lookupResult, f.lookupErr
}

// LookupSubjects and LookupInteractSubjects satisfy engine.LookuperImpl.
// lookupSubjectsResult drives the start gate's platform-admin fan-out; the
// nil default models "no admins resolvable".
func (f *fakeAuthz) LookupSubjects(_ context.Context, subjectRef string) ([]string, error) {
	f.lookupSubjectsRefs = append(f.lookupSubjectsRefs, subjectRef)
	return f.lookupSubjectsResult, f.lookupSubjectsErr
}
func (f *fakeAuthz) LookupInteractSubjects(_ context.Context, _, _ string) ([]string, error) {
	return nil, nil
}

// CheckApprove and CheckOwnerOnResource satisfy engine.ApproverCheckerImpl.
func (f *fakeAuthz) CheckApprove(_ context.Context, _, _ string, _ identity.CanonicalUserID, _ bool) (bool, error) {
	return f.checkApproveResult, f.checkApproveErr
}
func (f *fakeAuthz) CheckOwnerOnResource(_ context.Context, resType, resID string, canonicalID identity.CanonicalUserID, _ bool) (bool, error) {
	if f.checkOwnerFn != nil {
		return f.checkOwnerFn(resType, resID, canonicalID.String())
	}
	return f.checkOwnerResult, f.checkOwnerErr
}

// CheckOnResource satisfies the approver-gate interface. These fakes model
// resource types whose approverPermission is the default (owner), so every
// permission delegates to the owner answer.
func (f *fakeAuthz) CheckOnResource(ctx context.Context, resType, resID, _ string, canonicalID identity.CanonicalUserID, fullyConsistent bool) (bool, error) {
	return f.CheckOwnerOnResource(ctx, resType, resID, canonicalID, fullyConsistent)
}

// GrantSlots records the instances thread adoption seeded, so a test can
// assert WHICH values bound rather than merely that something was written.
func (f *fakeAuthz) GrantSlots(_ context.Context, ns, name string, bindings []authz.SlotBinding, expiresAt time.Time) error {
	if f.grantSlotsErr != nil {
		return f.grantSlotsErr
	}
	f.grantedSlots = append(f.grantedSlots, bindings...)
	f.grantedSlotsSession = ns + "/" + name
	f.grantedSlotsExpiry = expiresAt
	return nil
}

// Relations satisfies pipeline.Authz for tests that never approve a JIT tool
// call (most of this package). A nil authz.RelWriter is the documented safe
// branch: authz.BindApproved narrows session_scope and logs rather than
// granting when its writer is nil. Tests that DO need to observe the grant
// tuple (slotGrantRecorder) override this.
func (f *fakeAuthz) Relations() authz.RelWriter { return nil }

type fakeMemory struct {
	appends  []memAppend
	seeded   map[string][]MemTurn // key: ns/name; preloaded for ReadAll
	err      error
	readErr  error
	msgRefs  []recordedMsgRef
	trigDel  []recordedTriggerDelivery
	envFacts []recordedEnvelopeFacts

	// uploads/uploadResult/uploadErr back UploadInboundAsset. Unused (and so
	// never called) by every pre-attachments test in this file — attachments.go's
	// own tests in attachments_test.go configure these directly.
	uploads      []recordedUpload
	uploadResult InboundAssetResult
	uploadErr    error
}

type recordedUpload struct {
	ns, sess, mime, filename string
}

type memAppend struct {
	ns, name string
	turn     MemTurn
}

type recordedMsgRef struct {
	Ns, Name, Kind, Ref string
	Idx                 int
}

type recordedTriggerDelivery struct {
	Ns, Name, Kind, Event, ChannelKey string
	Body                              []byte
}

type recordedEnvelopeFacts struct {
	Ns, Name, Kind, Event string
	Facts                 []channelkinds.TriggerFact
}

func (f *fakeMemory) Append(_ context.Context, ns, name string, t MemTurn) error {
	if f.err != nil {
		return f.err
	}
	f.appends = append(f.appends, memAppend{ns, name, t})
	return nil
}

// ReadAll returns the union of seeded turns and turns written via Append.
// Seeded turns represent pre-existing session state; appended turns are
// those written during the test. Combining them lets lastInboxTurnIndex
// see the just-appended turn without requiring tests to pre-seed it.
func (f *fakeMemory) ReadAll(_ context.Context, ns, name string) ([]MemTurn, error) {
	if f.readErr != nil {
		return nil, f.readErr
	}
	key := ns + "/" + name
	out := append([]MemTurn(nil), f.seeded[key]...)
	for _, a := range f.appends {
		if a.ns == ns && a.name == name {
			out = append(out, a.turn)
		}
	}
	return out, nil
}

func (f *fakeMemory) RecordChannelMsgRef(_ context.Context, ns, name, kind, ref string, idx int) error {
	f.msgRefs = append(f.msgRefs, recordedMsgRef{ns, name, kind, ref, idx})
	return nil
}

func (f *fakeMemory) RecordTriggerDelivery(_ context.Context, ns, name, kind, event, channelKey string, body []byte) error {
	f.trigDel = append(f.trigDel, recordedTriggerDelivery{ns, name, kind, event, channelKey, body})
	return nil
}

func (f *fakeMemory) RecordEnvelopeFacts(_ context.Context, ns, name, kind, event string, facts []channelkinds.TriggerFact) error {
	f.envFacts = append(f.envFacts, recordedEnvelopeFacts{ns, name, kind, event, facts})
	return nil
}

// UploadInboundAsset records the call and drains body (mirroring a real
// uploader, which must read to EOF) before returning the test-configured
// result/error.
func (f *fakeMemory) UploadInboundAsset(_ context.Context, ns, sess, mime, filename string, body io.Reader) (InboundAssetResult, error) {
	f.uploads = append(f.uploads, recordedUpload{ns, sess, mime, filename})
	if body != nil {
		_, _ = io.Copy(io.Discard, body)
	}
	if f.uploadErr != nil {
		return InboundAssetResult{}, f.uploadErr
	}
	return f.uploadResult, nil
}

type fakeNATS struct {
	subjects []string
	payloads [][]byte
	// failSubjectSuffix makes Publish fail for subjects ending in it, so a
	// test can exercise how a caller degrades when ONE publish fails rather
	// than when the bus is wholly down. The call is still recorded: what a
	// caller attempted is as interesting as what got through.
	failSubjectSuffix string
}

func (f *fakeNATS) Publish(subj string, p []byte) error {
	f.subjects = append(f.subjects, subj)
	f.payloads = append(f.payloads, append([]byte(nil), p...))
	if f.failSubjectSuffix != "" && strings.HasSuffix(subj, f.failSubjectSuffix) {
		return errors.New("fakeNATS: publish failed")
	}
	return nil
}

func sha256HexTest(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])[:63]
}

func newPipeline(t *testing.T, objs ...client.Object) (*Pipeline, *fakeAuthz, *fakeMemory, *fakeNATS, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	_ = spiceboxv1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	objs = withHealthyAgentClass(t, objs)
	cli := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithStatusSubresource(&spiceboxv1alpha1.AgentSession{}).
		Build()
	p, az, mem, nats := newPipelineOn(t, cli)
	return p, az, mem, nats, cli
}

// newPipelineOn builds the pipeline over a caller-supplied client, for tests
// that need to instrument the K8s reads (e.g. making the object change between
// Deliver's opening List and the later fresh Get).
func newPipelineOn(t *testing.T, cli client.Client) (*Pipeline, *fakeAuthz, *fakeMemory, *fakeNATS) {
	t.Helper()
	az := &fakeAuthz{checkResult: true}
	mem := &fakeMemory{seeded: map[string][]MemTurn{}}
	nats := &fakeNATS{}
	caps := func(_ string) []string { return []string{"text", "markdown"} }
	p := NewPipeline(cli, az, mem, nats, caps)
	p.Engine = engine.New(engine.Deps{
		SessionInteractChecker: az,
		Granter:                az,
		Lookuper:               az,
		ApproverChecker:        az,
	})
	p.Now = func() time.Time { return time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC) }
	p.MarkerSigner = testMarkerSigner
	return p, az, mem, nats
}

func newChannel(name string) *spiceboxv1alpha1.Channel {
	return &spiceboxv1alpha1.Channel{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: spiceboxv1alpha1.ChannelSpec{
			Kind: "fake", AgentClass: "ac1",
			CredentialsRef: spiceboxv1alpha1.ChannelCredentialsRef{SecretName: "s"},
			Fake:           &spiceboxv1alpha1.FakeChannelConfig{},
		},
	}
}

// newBentoChannel builds a kind=bento, role=input Channel CR with the
// supplied AuthzSubject. Used by tests that exercise the no-user-identity
// branch of Deliver — bento has no per-user attribution, so the Channel
// CR carries the SpiceDB subject the session attributes work to.
func newBentoChannel(name, authzSubject string) *spiceboxv1alpha1.Channel {
	return &spiceboxv1alpha1.Channel{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: spiceboxv1alpha1.ChannelSpec{
			Kind:           "bento",
			Role:           spiceboxv1alpha1.ChannelRoleInput,
			AgentClass:     "ac1",
			AuthzSubject:   authzSubject,
			CredentialsRef: spiceboxv1alpha1.ChannelCredentialsRef{SecretName: "s"},
		},
	}
}

// newSlackOutputChannel builds the kind=slack, role=output sibling a
// role=input Channel binds its reply to. Anchorable: it carries an
// outputDefaults.channelId, which is what OutboundAnchor needs.
func newSlackOutputChannel(name, channelID string) *spiceboxv1alpha1.Channel {
	return &spiceboxv1alpha1.Channel{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: spiceboxv1alpha1.ChannelSpec{
			Kind:           "slack",
			Role:           spiceboxv1alpha1.ChannelRoleOutput,
			AgentClass:     "ac1",
			CredentialsRef: spiceboxv1alpha1.ChannelCredentialsRef{SecretName: "s"},
			Slack: &spiceboxv1alpha1.SlackChannelConfig{
				OutputDefaults: &spiceboxv1alpha1.SlackOutputDefaults{ChannelID: channelID},
			},
		},
	}
}

// existingSession returns a Running AgentSession for channel c1 on the
// "thread:C1:1" key, with optional started-by annotation. Used by the
// many "existing session" tests so each one only varies what matters.
func existingSession(t *testing.T, name string, startedBy string, phase string) *spiceboxv1alpha1.AgentSession {
	t.Helper()
	keyHash := sha256HexTest("thread:C1:1")
	anns := map[string]string{}
	if startedBy != "" {
		anns[spiceboxv1alpha1.AnnotationStartedByExternalID] = startedBy
	}
	return &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "default",
			Labels: map[string]string{
				spiceboxv1alpha1.LabelChannelName: "c1",
				spiceboxv1alpha1.LabelChannelKey:  keyHash,
			},
			Annotations: anns,
		},
		Spec: spiceboxv1alpha1.AgentSessionSpec{
			Class: "ac1",
			InputChannel: &spiceboxv1alpha1.ChannelBinding{
				Name: "c1", Kind: "fake", Key: "thread:C1:1",
				NATSSubjectPrefix: "ap.session.default." + name,
			},
		},
		Status: spiceboxv1alpha1.AgentSessionStatus{Phase: phase},
	}
}

// archivedSession returns a Channel-attached AgentSession with the given
// terminal Phase (Succeeded/Failed), no InputChannel spec, used as the
// inheritance source for "new session inherits from archived" tests.
func archivedSession(t *testing.T, name, channelKey string, phase string, startedBy string) *spiceboxv1alpha1.AgentSession {
	t.Helper()
	keyHash := sha256HexTest(channelKey)
	anns := map[string]string{}
	if startedBy != "" {
		anns[spiceboxv1alpha1.AnnotationStartedByExternalID] = startedBy
	}
	return &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "default",
			Labels: map[string]string{
				spiceboxv1alpha1.LabelChannelName: "c1",
				spiceboxv1alpha1.LabelChannelKey:  keyHash,
			},
			Annotations: anns,
		},
		Spec:   spiceboxv1alpha1.AgentSessionSpec{Class: "ac1"},
		Status: spiceboxv1alpha1.AgentSessionStatus{Phase: phase},
	}
}

func TestDeliverNewSessionCreatesAndWritesAuthz(t *testing.T) {
	ch := newChannel("c1")
	p, az, _, _, cli := newPipeline(t, ch)
	dec, err := p.Deliver(context.Background(), channelkinds.InboundEvent{
		Channel:     ch,
		ExternalIDs: channelkinds.ExternalIdentity{Kind: "slack", ExternalID: "U1", Email: "a@example.com"},
		ChannelKey:  "thread:C1:1",
		MessageText: "hello",
	})
	require.NoError(t, err, "Deliver")
	assert.Equal(t, channelkinds.OutcomeRouted, dec.Outcome, "outcome")
	assert.Equal(t, 1, az.startedByCalls, "TouchStartedBy calls")

	var sessions spiceboxv1alpha1.AgentSessionList
	require.NoError(t, cli.List(context.Background(), &sessions), "List sessions")
	require.Len(t, sessions.Items, 1, "session count")
	got := sessions.Items[0]
	require.NotNil(t, got.Spec.InputChannel, "spec.inputChannel")
	assert.Equal(t, "c1", got.Spec.InputChannel.Name, "spec.inputChannel.Name")
	assert.NotEmpty(t, got.Labels[spiceboxv1alpha1.LabelChannelKey], "channel.key label")
}

// TestDeliverNewSession_TriggerDeliveryRecordsRawBody pins that a webhook-
// originated inbound (RawDelivery set, as a verified GitHub delivery would
// arrive) gets its trigger_delivery entry written exactly once, on the
// actual-create path, with the fields a steelthread capture needs to replay
// it: the channel kind, the provider event header, and the same ChannelKey
// the session was bound on.
func TestDeliverNewSession_TriggerDeliveryRecordsRawBody(t *testing.T) {
	ch := newChannel("c1")
	p, _, mem, _, _ := newPipeline(t, ch)

	rawBody := []byte(`{"action":"opened","number":7}`)
	dec, err := p.Deliver(context.Background(), channelkinds.InboundEvent{
		Channel:       ch,
		ChannelKey:    "pr:acme/widgets#7",
		MessageText:   "PR #7 opened",
		AuthzSubject:  "service:demo-reviewbot",
		RawDelivery:   rawBody,
		DeliveryEvent: "pull_request",
	})
	require.NoError(t, err, "Deliver")
	require.Equal(t, channelkinds.OutcomeRouted, dec.Outcome)

	require.Len(t, mem.trigDel, 1, "trigger_delivery must be recorded exactly once for the opening delivery")
	got := mem.trigDel[0]
	assert.Equal(t, "fake", got.Kind, "channel kind, not ExternalIDs.Kind — a webhook inbound has no per-user ExternalIDs")
	assert.Equal(t, "pull_request", got.Event)
	assert.Equal(t, "pr:acme/widgets#7", got.ChannelKey, "must be the same key the session bound on, not recomputed")
	assert.Equal(t, rawBody, got.Body)
}

// TestDeliverNewSession_NoRawDeliverySkipsTriggerDelivery pins the other
// half: an ordinary conversational inbound (a person typing, no RawDelivery)
// must never write a trigger_delivery entry — that Kind's absence IS the
// signal that no trigger started the session.
func TestDeliverNewSession_NoRawDeliverySkipsTriggerDelivery(t *testing.T) {
	ch := newChannel("c1")
	p, _, mem, _, _ := newPipeline(t, ch)

	dec, err := p.Deliver(context.Background(), channelkinds.InboundEvent{
		Channel:     ch,
		ExternalIDs: channelkinds.ExternalIdentity{Kind: "slack", ExternalID: "U1", Email: "a@example.com"},
		ChannelKey:  "thread:C1:1",
		MessageText: "hello",
	})
	require.NoError(t, err, "Deliver")
	require.Equal(t, channelkinds.OutcomeRouted, dec.Outcome)

	assert.Empty(t, mem.trigDel, "a person-typed inbound must never write a trigger_delivery record")
}

// githubActiveSession seeds a Running AgentSession bound to a github Channel
// on key, so a redelivery routes onto it via the active-session branch
// (~line 960) and returns long before the create/record block is ever
// reached — the same reason TestDeliverActiveOverridesArchived's activeSess
// needs no InputChannel.Role or output-channel companion. This is a DIFFERENT
// way `created` never becomes true from the AlreadyExists race below: here
// the create/record block, guard included, is simply never executed at all.
func githubActiveSession(chName, key string) *spiceboxv1alpha1.AgentSession {
	const name = "gh-active"
	return &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "default",
			Labels: map[string]string{
				spiceboxv1alpha1.LabelChannelName: chName,
				spiceboxv1alpha1.LabelChannelKey:  sha256HexTest(key),
			},
		},
		Spec: spiceboxv1alpha1.AgentSessionSpec{
			Class: "ac1",
			InputChannel: &spiceboxv1alpha1.ChannelBinding{
				Name: chName, Kind: "github", Key: key,
				NATSSubjectPrefix: "ap.session.default." + name,
			},
		},
		Status: spiceboxv1alpha1.AgentSessionStatus{Phase: spiceboxv1alpha1.AgentSessionPhaseRunning},
	}
}

// githubNameCollisionSession seeds an AgentSession at the EXACT deterministic
// name Deliver's fresh-session path will independently compute for (ch, key)
// via makeSessionName — but labelled so correlateSessions' (LabelChannelName,
// LabelChannelKey) List lookup does NOT find it. Deliver therefore proceeds
// down the fresh-session path exactly as if no session existed, builds its
// own sess with that same name, and p.K8s.Create collides on the name: the
// REAL AlreadyExists race the `created` guard exists for (another replica
// already won the Create for this session). Unlike githubActiveSession
// above, the create/record block's guard IS entered here — `created` just
// evaluates false, which is the case the guard's own comment describes.
func githubNameCollisionSession(ch *spiceboxv1alpha1.Channel, key string) *spiceboxv1alpha1.AgentSession {
	name := makeSessionName(ch, key)
	return &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: ch.Namespace,
			Labels: map[string]string{
				// Deliberately does NOT match (ch, key): proves this object is
				// findable only through the Create-time name collision, never
				// through correlateSessions' label lookup.
				spiceboxv1alpha1.LabelChannelName: "unrelated-channel",
				spiceboxv1alpha1.LabelChannelKey:  sha256HexTest("unrelated-key"),
			},
		},
		Spec: spiceboxv1alpha1.AgentSessionSpec{
			Class: "ac1",
			InputChannel: &spiceboxv1alpha1.ChannelBinding{
				Name: ch.Name, Kind: "github", Key: key,
				NATSSubjectPrefix: "ap.session.default." + name,
			},
		},
		Status: spiceboxv1alpha1.AgentSessionStatus{Phase: spiceboxv1alpha1.AgentSessionPhaseSucceeded},
	}
}

// TestSessionOpen_RecordsEnvelopeFacts asserts a verified delivery from a
// fact-providing kind lands facts, keyed to the subjects the envelope named.
//
// Each `wantCalled: false` row is a DIFFERENT reason no facts land, and
// wantTrigDel distinguishes them precisely: whether the surrounding
// `created && len(RawDelivery) > 0` guard (shared with RecordTriggerDelivery)
// was even entered. The active-session and no-RawDelivery rows never enter it
// (guard false) and the AlreadyExists-race row enters it but with
// created==false (guard false too, for the reason the guard's own comment in
// pipeline.go gives); only the no-TriggerFactProvider row enters the guard
// AND runs RecordTriggerDelivery, with only the facts half then skipped —
// wantTrigDel is the positive control that proves that row reached the block
// at all, rather than "no facts" meaning "never got there".
func TestSessionOpen_RecordsEnvelopeFacts(t *testing.T) {
	const subj = "service:demo-reviewer"
	const key = "pr:acme/widgets#7"
	// repository.full_name, number, pull_request.head.sha and
	// pull_request.head.repo.fork are the only fields github's
	// receiver.TriggerFacts reads (see receiver.go).
	prBody := []byte(`{"action":"opened","number":7,"repository":{"full_name":"acme/widgets"},` +
		`"pull_request":{"head":{"sha":"deadbeef","repo":{"fork":true}}}}`)

	cases := []struct {
		name              string
		ch                *spiceboxv1alpha1.Channel
		withOutput        bool
		seedActive        bool
		seedNameCollision bool
		rawBody           []byte
		deliveryEvent     string
		wantTrigDel       bool // the shared created&&RawDelivery guard was entered
		wantCalled        bool // envelope facts recorded
	}{
		{
			name:          "github delivery on a newly created session: facts recorded",
			ch:            newGitHubChannel("gh-in", subj),
			withOutput:    true,
			rawBody:       prBody,
			deliveryEvent: "pull_request",
			wantTrigDel:   true,
			wantCalled:    true,
		},
		{
			name:          "session already active: routed via the early-return branch, guard never reached",
			ch:            newGitHubChannel("gh-in", subj),
			withOutput:    true,
			seedActive:    true,
			rawBody:       prBody,
			deliveryEvent: "pull_request",
			wantTrigDel:   false,
			wantCalled:    false,
		},
		{
			name:              "AlreadyExists race: another replica already won the Create — guard entered, created==false",
			ch:                newGitHubChannel("gh-in", subj),
			withOutput:        true,
			seedNameCollision: true,
			rawBody:           prBody,
			deliveryEvent:     "pull_request",
			wantTrigDel:       false,
			wantCalled:        false,
		},
		{
			name:          "a kind with no TriggerFactProvider records nothing and does not error",
			ch:            newChannel("c1"), // fake kind: no TriggerFactProvider
			rawBody:       []byte(`{"anything":true}`),
			deliveryEvent: "pull_request",
			wantTrigDel:   true, // proves the guard WAS entered — only the provider check filtered it out
			wantCalled:    false,
		},
		{
			name:        "no raw delivery (a person typed): nothing recorded",
			ch:          newGitHubChannel("gh-in", subj),
			withOutput:  true,
			rawBody:     nil,
			wantTrigDel: false,
			wantCalled:  false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			objs := []client.Object{tc.ch}
			if tc.withOutput {
				objs = append(objs, newSlackOutputChannel("gh-out", "C_OUT"))
			}
			if tc.seedActive {
				objs = append(objs, githubActiveSession(tc.ch.Name, key))
			}
			if tc.seedNameCollision {
				objs = append(objs, githubNameCollisionSession(tc.ch, key))
			}
			p, _, mem, _, _ := newPipeline(t, objs...)

			dec, err := p.Deliver(context.Background(), channelkinds.InboundEvent{
				Channel:       tc.ch,
				ChannelKey:    key,
				MessageText:   "PR #7 opened",
				AuthzSubject:  subj,
				RawDelivery:   tc.rawBody,
				DeliveryEvent: tc.deliveryEvent,
			})
			require.NoError(t, err, "Deliver")
			require.Equal(t, channelkinds.OutcomeRouted, dec.Outcome)

			if tc.wantTrigDel {
				require.Len(t, mem.trigDel, 1, "trigger_delivery must be recorded — the shared guard was entered")
			} else {
				assert.Empty(t, mem.trigDel, "trigger_delivery must not be recorded on this path")
			}

			if !tc.wantCalled {
				assert.Empty(t, mem.envFacts, "no envelope facts expected")
				return
			}
			require.Len(t, mem.envFacts, 1)
			got := mem.envFacts[0]
			// The SCOPE, first. Facts written under a different session are
			// invisible to every gate in the session that actually opened —
			// and nothing else in this test would notice, because the Kind,
			// Event and Facts assertions below hold whatever scope the entry
			// landed in.
			assert.Equal(t, dec.Session.Namespace, got.Ns,
				"facts must be filed under the session this delivery opened")
			assert.Equal(t, dec.Session.Name, got.Name,
				"facts must be filed under the session this delivery opened")
			assert.Equal(t, "github", got.Kind)
			assert.Equal(t, "pull_request", got.Event)
			require.Len(t, got.Facts, 1)
			assert.Equal(t, map[string]any{"head_is_fork": true}, got.Facts[0].Facts)
			assert.Len(t, got.Facts[0].Subjects, 2, "the PR and its head commit")
		})
	}
}

// TestSessionOpen_FactDerivationFailureDoesNotStopTheSession asserts a kind
// whose TriggerFacts errors still yields a started session, with its
// trigger_delivery record intact.
//
// Fail-closed for the GATE (no facts recorded means every precondition over
// them reads undetermined and denies) but NOT fatal for the delivery: the
// session must still start, so the agent can be told why it cannot proceed
// rather than the inbound vanishing with no trace anywhere.
//
// The LOG is the load-bearing assertion here, not the two state checks. Both
// of those are satisfied by RecordTriggerDelivery, a different feature: an
// empty mem.envFacts and one mem.trigDel are exactly what a build with the
// whole fact block deleted produces, and so are a TriggerFacts that returned
// (nil, nil) and a type assertion that never matched. Asserting the error
// branch's own message is what distinguishes "derivation was attempted and
// failed loudly" from "derivation never ran", which is the repo's
// no-silent-errors rule applied to the test rather than only to the code.
func TestSessionOpen_FactDerivationFailureDoesNotStopTheSession(t *testing.T) {
	const subj = "service:demo-reviewer"
	ch := newGitHubChannel("gh-in", subj)
	p, _, mem, _, _ := newPipeline(t, ch, newSlackOutputChannel("gh-out", "C_OUT"))

	var mu sync.Mutex
	var logged []string
	capLogger := funcr.New(func(_, args string) {
		mu.Lock()
		defer mu.Unlock()
		logged = append(logged, args)
	}, funcr.Options{})
	ctx := log.IntoContext(context.Background(), capLogger)

	// A pull_request body with no repository, no PR number and no head SHA:
	// receiver.TriggerFacts refuses to key a fact from it.
	dec, err := p.Deliver(ctx, channelkinds.InboundEvent{
		Channel:       ch,
		ChannelKey:    "pr:acme/widgets#0",
		MessageText:   "PR opened",
		AuthzSubject:  subj,
		RawDelivery:   []byte(`{"action":"opened","number":0}`),
		DeliveryEvent: "pull_request",
	})
	require.NoError(t, err, "Deliver")
	assert.Equal(t, channelkinds.OutcomeRouted, dec.Outcome, "the session still starts")

	assert.Empty(t, mem.envFacts, "nothing recorded")
	require.Len(t, mem.trigDel, 1,
		"the delivery record must exist even though fact derivation failed — it is the evidence of what arrived")

	mu.Lock()
	defer mu.Unlock()
	joined := strings.Join(logged, "\n")
	assert.Contains(t, joined, "trigger fact derivation failed",
		"the refusal must be logged, or a session silently starts with no facts and nobody can tell why")
	assert.Contains(t, joined, "github", "the log must name the channel kind whose derivation failed")
}

// TestDeliverNewSession_StampsCanonicalIDAnnotation verifies that a
// newly-created AgentSession carries AnnotationStartedByCanonicalID with
// the full SpiceDB subject ("user:<canonical>") — the same value used for
// the started_by tuple write. The operator's passthrough gate reads this
// annotation to locate the starter's UserIdentity without re-computing
// the canonical.
func TestDeliverNewSession_StampsCanonicalIDAnnotation(t *testing.T) {
	ch := newChannel("c1")
	p, az, _, _, cli := newPipeline(t, ch)

	inbound := channelkinds.InboundEvent{
		Channel:     ch,
		ExternalIDs: channelkinds.ExternalIdentity{Kind: "slack", ExternalID: "U42", Email: "alice@example.com"},
		ChannelKey:  "thread:C1:canon",
		MessageText: "hello",
	}
	dec, err := p.Deliver(context.Background(), inbound)
	require.NoError(t, err, "Deliver")
	require.Equal(t, channelkinds.OutcomeRouted, dec.Outcome)

	// Compute the canonical the same way the pipeline does.
	canonical, err := identity.FromExternal(identity.Kind(inbound.ExternalIDs.Kind), identity.TeamScope(inbound.ExternalIDs.TeamScope), identity.RawExternalID(inbound.ExternalIDs.ExternalID), identity.Email(inbound.ExternalIDs.Email)).Canonical()
	require.NoError(t, err)
	wantAnnotation := "user:" + canonical.String()

	// The TouchStartedBy call must use the bare canonical (no "user:" prefix);
	// the annotation is the full SpiceDB subject form.
	assert.Equal(t, canonical, az.lastStartedBy, "TouchStartedBy must receive the bare canonical")

	var sessions spiceboxv1alpha1.AgentSessionList
	require.NoError(t, cli.List(context.Background(), &sessions), "List sessions")
	require.Len(t, sessions.Items, 1, "session count")
	sess := sessions.Items[0]
	assert.Equal(t, wantAnnotation,
		sess.Annotations[spiceboxv1alpha1.AnnotationStartedByCanonicalID],
		"AgentSession must carry the starter's canonical subject as annotation")
}

// TestDeliverBentoInboundUsesAuthzSubject asserts the no-user-identity
// branch: when an InboundEvent has empty ExternalIDs.ExternalID but a
// non-empty AuthzSubject (the bento path), the AuthzSubject becomes the
// acting subject for the decision and the inbound routes successfully.
//
// The AuthzSubject is deliberately NOT written to agentsession#started_by: a
// cron session has no human starter. Writing it would produce
// `user:service:<id>`, an object_id containing a colon that SpiceDB rejects —
// see TestDeliverServiceSubjectSkipsStartedByWrite.
func TestDeliverBentoInboundUsesAuthzSubject(t *testing.T) {
	const subj = "service:hubspot-digest-bot"
	ch := newBentoChannel("cron1", subj)
	// A role=input Channel now requires a role=output sibling to bind its
	// reply to; without one the delivery is refused as undeliverable.
	p, az, mem, _, cli := newPipeline(t, ch, newSlackOutputChannel("cron1-out", "C1"))

	dec, err := p.Deliver(context.Background(), channelkinds.InboundEvent{
		Channel:      ch,
		ExternalIDs:  channelkinds.ExternalIdentity{}, // no user identity
		ChannelKey:   "cron:cron1:1.0",
		MessageText:  "What companies were added in the last 7 days?",
		AuthzSubject: subj,
	})
	require.NoError(t, err, "Deliver")
	require.Equal(t, channelkinds.OutcomeRouted, dec.Outcome)
	assert.Equal(t, 0, az.startedByCalls,
		"a service subject must not be written to started_by (schema allows only `user`)")
	assert.Equal(t, subj, dec.RequesterCanonicalID, "RequesterCanonicalID")
	// New-session creation path does not append to memory itself —
	// the runner consumes the inline Prompt for the first turn.
	assert.Empty(t, mem.appends, "Memory.Append on new-session path")

	// Sanity-check the session created carries the bento input binding.
	var sessions spiceboxv1alpha1.AgentSessionList
	require.NoError(t, cli.List(context.Background(), &sessions))
	require.Len(t, sessions.Items, 1)
	if got := sessions.Items[0].Spec.InputChannel; assert.NotNil(t, got, "spec.inputChannel") {
		assert.Equal(t, "bento", got.Kind, "session.spec.inputChannel kind")
	}
}

// TestDeliverBentoInbound_RejectsImpersonatingAuthzSubject is the regression
// for the impersonation vuln: a Channel's authzSubject is used verbatim as the
// session's started_by, and RawSubject.Canonical() strips a leading "user:".
// So a bento Channel declaring authzSubject "user:<base64(victim)>" would run
// the cron session AS that human. Only service:<id> is permitted; any other
// form must fail closed (no started_by write, no session) at the pipeline
// trust boundary — independent of the CRD pattern (a fake client / a CR
// created before the pattern existed must still be rejected here).
func TestDeliverBentoInbound_RejectsImpersonatingAuthzSubject(t *testing.T) {
	cases := []struct {
		name string
		subj string
	}{
		{"user-subject impersonation", "user:dmljdGltQGNvcnAuY29t"},
		{"group-subject", "group:admins#member"},
		{"no type prefix", "hubspot-digest-bot"},
		{"empty id after prefix", "service:"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ch := newBentoChannel("cron1", tc.subj)
			p, az, _, _, cli := newPipeline(t, ch)

			dec, err := p.Deliver(context.Background(), channelkinds.InboundEvent{
				Channel:      ch,
				ExternalIDs:  channelkinds.ExternalIdentity{},
				ChannelKey:   "cron:cron1:1.0",
				MessageText:  "do work",
				AuthzSubject: tc.subj,
			})
			require.Error(t, err, "Deliver must reject a non-service authzSubject")
			assert.NotEqual(t, channelkinds.OutcomeRouted, dec.Outcome)
			assert.Equal(t, 0, az.startedByCalls, "must NOT write started_by for a rejected subject")
			var sessions spiceboxv1alpha1.AgentSessionList
			require.NoError(t, cli.List(context.Background(), &sessions))
			assert.Empty(t, sessions.Items, "no session created for a rejected authzSubject")
		})
	}
}

// newKindAuthzSubjectChannel builds a role-unset Channel CR of the given kind
// carrying the given AuthzSubject. Role is left at its Go zero value (never
// ChannelRoleInput), so the role=input output-sibling-binding requirement
// (see TestDeliverBindsOutputChannelForRoleInput below) never triggers —
// these tests are about which subject TYPES a kind admits, not about output
// binding.
func newKindAuthzSubjectChannel(name, kind, authzSubject string) *spiceboxv1alpha1.Channel {
	return &spiceboxv1alpha1.Channel{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: spiceboxv1alpha1.ChannelSpec{
			Kind:           kind,
			AgentClass:     "ac1",
			AuthzSubject:   authzSubject,
			CredentialsRef: spiceboxv1alpha1.ChannelCredentialsRef{SecretName: "s"},
		},
	}
}

// TestDeliverAuthzSubjectPerKind proves the agentsession: widening added to
// Deliver's no-user-identity branch is a PER-KIND rule, not a blanket
// admission: agentsession: is meaningful only for a session-to-session
// (kind=agent) channel, so only that kind's check accepts it. user:/group:
// remain forbidden on every kind, including agent, and bento's pre-existing
// service: acceptance is unchanged.
//
// The slack case is the load-bearing one: if the widening were written as
// "agentsession: is now an allowed SubjectType" rather than "allowed only
// when kind==agent", this is the only case that would catch it.
func TestDeliverAuthzSubjectPerKind(t *testing.T) {
	cases := []struct {
		name  string
		kind  string
		subj  string
		check func(t *testing.T, dec channelkinds.InboundDecision, err error, sessions spiceboxv1alpha1.AgentSessionList, az *fakeAuthz)
	}{
		{
			name: "agent kind admits an agentsession: counterparty",
			kind: "agent",
			subj: "agentsession:demo-ns/lead-1",
			check: func(t *testing.T, dec channelkinds.InboundDecision, err error, _ spiceboxv1alpha1.AgentSessionList, _ *fakeAuthz) {
				t.Helper()
				require.NoError(t, err, "Deliver must accept an agentsession: subject on a kind=agent channel")
				// agent's SpawnsSessionOnInbound is false, so with no active
				// session the pipeline stops one gate short of OutcomeRouted —
				// but that stop is unreachable unless ValidateSubject accepted
				// the subject first. OutcomeInternalError (the rejection
				// outcome) is what this distinguishes from.
				assert.Equal(t, channelkinds.OutcomeNoActiveSession, dec.Outcome, "outcome")
			},
		},
		{
			name: "agent kind still refuses a user: impersonation subject",
			kind: "agent",
			subj: "user:someone@example.com",
			check: func(t *testing.T, dec channelkinds.InboundDecision, err error, sessions spiceboxv1alpha1.AgentSessionList, az *fakeAuthz) {
				t.Helper()
				require.Error(t, err, "Deliver must reject a user: authzSubject even on kind=agent")
				assert.Equal(t, channelkinds.OutcomeInternalError, dec.Outcome, "outcome")
				assert.Equal(t, 0, az.startedByCalls, "must NOT write started_by for a rejected subject")
				assert.Empty(t, sessions.Items, "no session created for a rejected authzSubject")
			},
		},
		{
			name: "slack kind refuses an agentsession: subject",
			kind: "slack",
			subj: "agentsession:demo-ns/lead-1",
			check: func(t *testing.T, dec channelkinds.InboundDecision, err error, sessions spiceboxv1alpha1.AgentSessionList, az *fakeAuthz) {
				t.Helper()
				require.Error(t, err, "Deliver must reject an agentsession: subject on a non-agent kind — it names a counterparty session that never spoke on this channel")
				assert.Equal(t, channelkinds.OutcomeInternalError, dec.Outcome, "outcome")
				assert.Equal(t, 0, az.startedByCalls, "must NOT write started_by for a rejected subject")
				assert.Empty(t, sessions.Items, "no session created for a rejected authzSubject")
			},
		},
		{
			name: "bento kind keeps accepting service: unchanged",
			kind: "bento",
			subj: "service:digest-bot",
			check: func(t *testing.T, dec channelkinds.InboundDecision, err error, sessions spiceboxv1alpha1.AgentSessionList, _ *fakeAuthz) {
				t.Helper()
				require.NoError(t, err, "Deliver")
				assert.Equal(t, channelkinds.OutcomeRouted, dec.Outcome, "outcome")
				assert.Len(t, sessions.Items, 1, "session created")
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ch := newKindAuthzSubjectChannel("c1", tc.kind, tc.subj)
			p, az, _, _, cli := newPipeline(t, ch)

			dec, err := p.Deliver(context.Background(), channelkinds.InboundEvent{
				Channel:      ch,
				ExternalIDs:  channelkinds.ExternalIdentity{},
				ChannelKey:   "thread:C1:1",
				MessageText:  "hello",
				AuthzSubject: tc.subj,
			})

			var sessions spiceboxv1alpha1.AgentSessionList
			require.NoError(t, cli.List(context.Background(), &sessions), "List sessions")

			tc.check(t, dec, err, sessions, az)
		})
	}
}

// A role=input Channel (bento cron) must bind to its role=output sibling at
// session creation. Without it, resolve.ForSession falls back to the input
// binding, hits bento's nopSender, and the reply is undeliverable.
func TestDeliverBindsOutputChannelForRoleInput(t *testing.T) {
	const subj = "service:demo-cron"
	ch := newBentoChannel("demo-input", subj)
	p, _, _, _, cli := newPipeline(t, ch, newSlackOutputChannel("test-output", "C1"))

	// Kind-aware capabilities so the assertion below genuinely proves the
	// binding took the OUTPUT kind's caps, not the input kind's.
	p.Capabilities = func(kind string) []string {
		if kind == "slack" {
			return []string{"text", "markdown"}
		}
		return nil
	}

	dec, err := p.Deliver(context.Background(), channelkinds.InboundEvent{
		Channel:      ch,
		ExternalIDs:  channelkinds.ExternalIdentity{},
		ChannelKey:   "cron:demo-input:1.0",
		MessageText:  "run the digest",
		AuthzSubject: subj,
	})
	require.NoError(t, err, "Deliver")
	require.Equal(t, channelkinds.OutcomeRouted, dec.Outcome)

	var sessions spiceboxv1alpha1.AgentSessionList
	require.NoError(t, cli.List(context.Background(), &sessions))
	require.Len(t, sessions.Items, 1)
	sess := sessions.Items[0]

	require.NotNil(t, sess.Spec.OutputChannel, "role=input session must carry an OutputChannel")
	assert.Equal(t, "test-output", sess.Spec.OutputChannel.Name)
	assert.Equal(t, "slack", sess.Spec.OutputChannel.Kind)
	assert.Equal(t, "channel:C1", sess.Spec.OutputChannel.Key)
	assert.Equal(t, "C1", sess.Spec.OutputChannel.External["channel_id"])

	// Capabilities come from the OUTPUT kind. Sourcing them from bento would
	// yield nil and tell the agent it cannot emit markdown.
	assert.Equal(t, []string{"text", "markdown"}, sess.Spec.OutputChannel.Capabilities,
		"capabilities must come from the output kind, not the input kind")

	// The thread index resolves cron sessions by this label.
	assert.Equal(t, sha256HexTest("channel:C1"),
		sess.Labels[spiceboxv1alpha1.LabelOutputChannelKey])

	// The input binding is still the bento Channel.
	require.NotNil(t, sess.Spec.InputChannel)
	assert.Equal(t, "bento", sess.Spec.InputChannel.Kind)
}

// No-regression: a role=both Channel is self-contained (both origin and
// destination) and must keep taking the untouched nil-OutputChannel path.
func TestDeliverLeavesOutputChannelNilForRoleBoth(t *testing.T) {
	ch := newChannel("c1")
	ch.Spec.Role = spiceboxv1alpha1.ChannelRoleBoth
	p, _, _, _, cli := newPipeline(t, ch)

	dec, err := p.Deliver(context.Background(), channelkinds.InboundEvent{
		Channel:     ch,
		ExternalIDs: channelkinds.ExternalIdentity{Kind: "fake", ExternalID: "U1", Email: "u@example.com"},
		ChannelKey:  "thread:C9:1.0",
		MessageText: "hello",
	})
	require.NoError(t, err, "Deliver")
	require.Equal(t, channelkinds.OutcomeRouted, dec.Outcome)

	var sessions spiceboxv1alpha1.AgentSessionList
	require.NoError(t, cli.List(context.Background(), &sessions))
	require.Len(t, sessions.Items, 1)
	sess := sessions.Items[0]

	assert.Nil(t, sess.Spec.OutputChannel, "role=both must not gain an OutputChannel")
	assert.NotContains(t, sess.Labels, spiceboxv1alpha1.LabelOutputChannelKey)
}

// An unresolvable binding must refuse the delivery loudly rather than create a
// session whose reply can never be sent.
func TestDeliverRefusesRoleInputWithNoOutputChannel(t *testing.T) {
	const subj = "service:demo-cron"
	ch := newBentoChannel("demo-input", subj)
	p, _, _, _, cli := newPipeline(t, ch) // no role=output sibling

	dec, err := p.Deliver(context.Background(), channelkinds.InboundEvent{
		Channel:      ch,
		ExternalIDs:  channelkinds.ExternalIdentity{},
		ChannelKey:   "cron:demo-input:1.0",
		MessageText:  "run the digest",
		AuthzSubject: subj,
	})
	require.Error(t, err, "an undeliverable session must be refused")
	require.ErrorIs(t, err, outputbind.ErrNoOutputChannel)
	assert.Equal(t, channelkinds.OutcomeInternalError, dec.Outcome)

	var sessions spiceboxv1alpha1.AgentSessionList
	require.NoError(t, cli.List(context.Background(), &sessions))
	assert.Empty(t, sessions.Items, "no undeliverable session may be created")
}

func TestDeliverExistingSessionAllowed(t *testing.T) {
	ch := newChannel("c1")
	existing := existingSession(t, "c1-abc", "", spiceboxv1alpha1.AgentSessionPhaseRunning)
	p, _, mem, nats, _ := newPipeline(t, ch, existing)
	dec, err := p.Deliver(context.Background(), channelkinds.InboundEvent{
		Channel:     ch,
		ExternalIDs: channelkinds.ExternalIdentity{Kind: "slack", ExternalID: "U1", Email: "a@example.com"},
		ChannelKey:  "thread:C1:1",
		MessageText: "follow-up",
	})
	require.NoError(t, err, "Deliver")
	assert.Equal(t, channelkinds.OutcomeRouted, dec.Outcome, "outcome")
	require.Len(t, mem.appends, 1, "memory appends")
	assert.Equal(t, "user", mem.appends[0].turn.Role, "memory append role")
	assert.Len(t, nats.subjects, 2, "nats publishes: wakeup + mid-turn enqueue ack (session is Running)")
}

func TestDeliverExistingSessionDenied(t *testing.T) {
	ch := newChannel("c1")
	// started-by "U_original": the join-request path addresses its approver
	// from the started-by annotations, so a session without one yields an
	// interaction_request with no addressable approver (now rejected by
	// Validate). A human-started session always records a starter.
	existing := existingSession(t, "c1-abc", "U_original", spiceboxv1alpha1.AgentSessionPhaseRunning)
	// Strip InputChannel for legacy session shape.
	existing.Spec.InputChannel = nil
	p, az, mem, nats, _ := newPipeline(t, ch, existing)
	az.checkResult = false
	dec, err := p.Deliver(context.Background(), channelkinds.InboundEvent{
		Channel:     ch,
		ExternalIDs: channelkinds.ExternalIdentity{Kind: "slack", ExternalID: "U_other", Email: "b@example.com"},
		ChannelKey:  "thread:C1:1",
		MessageText: "i'm a different user",
	})
	require.NoError(t, err, "Deliver")
	assert.Equal(t, channelkinds.OutcomeDeniedByPermission, dec.Outcome)
	// This legacy session has no started-by-external-id annotation, so the
	// deny message uses the un-tagged fallback ("Only the original requester
	// can talk to this session"). Sessions WITH the annotation get richer
	// @-tagged messages — see TestDeliverFastPathDenied.
	assert.Empty(t, mem.appends, "denied path should not append memory")
	// The deny path publishes a permission_request envelope on NATS.
	assert.Len(t, nats.subjects, 1, "denied path should publish 1 permission_request")
}

// TestDeliverFastPath_DifferentExternalUser_ConsultsSpiceDB collapses the
// two near-identical tests (SpiceDB permits vs. SpiceDB denies) for the
// active-session, started-by-mismatch path. Both share fixture; only
// az.checkResult and the assertion shape differ.
func TestDeliverFastPath_DifferentExternalUser_ConsultsSpiceDB(t *testing.T) {
	cases := []struct {
		name        string
		checkResult bool
		wantOutcome channelkinds.Outcome
		wantNATS    int
	}{
		{
			name:        "SpiceDB permits (e.g. post-approve participant grant): Routed, no permission_request",
			checkResult: true,
			wantOutcome: channelkinds.OutcomeRouted,
			wantNATS:    1, // wakeup envelope on .in.user_message
		},
		{
			name:        "SpiceDB denies: DeniedByPermission, a suppressed notice, permission_request published",
			checkResult: false,
			wantOutcome: channelkinds.OutcomeDeniedByPermission,
			wantNATS:    1, // permission_request envelope
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ch := newChannel("c1")
			existing := existingSession(t, "c1-abc", "U_original", spiceboxv1alpha1.AgentSessionPhaseIdle)
			if !tc.checkResult {
				// Denied case exercises a session shape with no InputChannel spec.
				existing.Spec.InputChannel = nil
			}
			p, az, mem, nats, _ := newPipeline(t, ch, existing)
			az.checkResult = tc.checkResult

			dec, err := p.Deliver(context.Background(), channelkinds.InboundEvent{
				Channel:     ch,
				ExternalIDs: channelkinds.ExternalIdentity{Kind: "slack", ExternalID: "U_coworker", Email: "coworker@example.com"},
				ChannelKey:  "thread:C1:1",
				MessageText: "from coworker",
			})
			require.NoError(t, err, "Deliver")
			require.Equal(t, tc.wantOutcome, dec.Outcome)
			assert.Equal(t, 1, az.checkCalls,
				"SpiceDB CheckInteract calls = %d; want 1 — mismatch path must consult SpiceDB", az.checkCalls)

			if !tc.checkResult {
				// the notice must be suppressed when permission_request is published —
				// the sub-channel sender owns the in-thread rejection. Non-empty
				// a notice here would double-post.
				assert.True(t, dec.Notice.IsSuppressed(),
					"notice must be suppressed when permission_request is published (would double-post)")
				assert.Empty(t, mem.appends, "denied path should not append memory")
			}
			assert.Len(t, nats.subjects, tc.wantNATS, "NATS publishes")
		})
	}
}

// TestDeliverPermissionDeny_NonSlackKindStillConsultsBlocklist: Authz.CheckDenied
// must not be fenced behind a "kind == slack" test. The blocklist is keyed by
// canonical (kind, externalID) and applies to every channel kind, so fencing it
// lets any non-slack kind silently bypass the blocklist. A denied requester
// from any channel kind gets the same silent-drop as a slack requester.
func TestDeliverPermissionDeny_NonSlackKindStillConsultsBlocklist(t *testing.T) {
	ch := newChannel("c1")
	existing := existingSession(t, "c1-abc", "U_original", spiceboxv1alpha1.AgentSessionPhaseIdle)
	existing.Spec.InputChannel = nil
	p, az, _, nats, _ := newPipeline(t, ch, existing)
	az.checkResult = false // SpiceDB denies
	az.deniedResult = true // …and the requester is on the per-session blocklist

	dec, err := p.Deliver(context.Background(), channelkinds.InboundEvent{
		Channel: ch,
		// "fake" stands in for any non-slack channel kind. The blocklist
		// must still apply.
		ExternalIDs: channelkinds.ExternalIdentity{Kind: "fake", ExternalID: "U_blocked", Email: "blocked@example.com"},
		ChannelKey:  "thread:C1:1",
		MessageText: "blocked user trying again",
	})
	require.NoError(t, err, "Deliver")
	require.Equal(t, channelkinds.OutcomeDeniedByPermission, dec.Outcome)
	// Blocklisted → silent drop (no in-thread post, no permission_request publish).
	assert.True(t, dec.Notice.IsSuppressed(), "notice must be suppressed for a blocklisted requester")
	assert.Equal(t, 1, az.deniedCalls,
		"CheckDenied calls = %d; want 1 — non-slack kinds must hit the blocklist too", az.deniedCalls)
	assert.Empty(t, nats.subjects, "blocklisted requester should publish 0 envelopes")
}

// TestDeliverActiveSessionAlwaysConsultsSpiceDB verifies that every
// inbound on an active session goes through SpiceDB, even when the
// session's started-by-external-id annotation matches the requester.
// We intentionally removed the Kube-annotation fast-path: bypassing
// SpiceDB hid bugs (started_by tuple shape, canonical-id divergence)
// because the bypass made the SpiceDB write redundant for the most
// common case. Single source of truth: SpiceDB.
func TestDeliverActiveSessionAlwaysConsultsSpiceDB(t *testing.T) {
	ch := newChannel("c1")
	// Annotation matches the requester — but this MUST NOT short-circuit.
	existing := existingSession(t, "c1-abc", "U_original", spiceboxv1alpha1.AgentSessionPhaseRunning)
	p, az, mem, _, _ := newPipeline(t, ch, existing)
	az.checkResult = true // SpiceDB grants

	dec, err := p.Deliver(context.Background(), channelkinds.InboundEvent{
		Channel:     ch,
		ExternalIDs: channelkinds.ExternalIdentity{Kind: "slack", ExternalID: "U_original", Email: "original@example.com"},
		ChannelKey:  "thread:C1:1",
		MessageText: "follow-up from the same user",
	})
	require.NoError(t, err, "Deliver")
	require.Equal(t, channelkinds.OutcomeRouted, dec.Outcome)
	assert.Equal(t, 1, az.checkCalls,
		"every inbound must go through SpiceDB, even when the requester matches the started-by annotation")
	assert.Len(t, mem.appends, 1, "expected 1 memory append (user reply)")
}

// TestDeliverActiveSession_InteractCheckError_InternalError verifies that a
// TRANSIENT SpiceDB check error on the interact gate maps to
// OutcomeInternalError (drop/retry) with the underlying error surfaced — NOT a
// permission request and NOT a silent allow. This preserves the pre-pipeline
// inline `if err != nil { return OutcomeInternalError, err }` semantics
// (resolved Ambiguity 3): a SpiceDB blip must not post a permission request.
func TestDeliverActiveSession_InteractCheckError_InternalError(t *testing.T) {
	ch := newChannel("c1")
	existing := existingSession(t, "c1-abc", "U_original", spiceboxv1alpha1.AgentSessionPhaseRunning)
	p, az, mem, nats, _ := newPipeline(t, ch, existing)
	az.checkErr = assert.AnError // SpiceDB transiently unavailable

	dec, err := p.Deliver(context.Background(), channelkinds.InboundEvent{
		Channel:     ch,
		ExternalIDs: channelkinds.ExternalIdentity{Kind: "slack", ExternalID: "U_original", Email: "original@example.com"},
		ChannelKey:  "thread:C1:1",
		MessageText: "follow-up during a SpiceDB blip",
	})
	require.Error(t, err, "a transient interact check error must surface (drop/retry), not be swallowed")
	assert.Equal(t, channelkinds.OutcomeInternalError, dec.Outcome,
		"transient check error → OutcomeInternalError, NOT DeniedByPermission")
	assert.Equal(t, 1, az.checkCalls, "the interact check is still consulted exactly once")
	assert.Empty(t, mem.appends, "internal-error path must not append memory")
	assert.Empty(t, nats.subjects, "transient error must NOT publish a permission_request")
}

// TestDeliverDifferentExternalUserTakesOverArchivedSession pins the
// different-user takeover: a coworker hitting a terminal thread started by
// someone else is not turned away — they get a NEW session they own, in the
// same thread. Because a finished Succeeded is an ORDINARY terminal state, the
// takeover inherits the prior transcript; that cross-user exposure is a
// deliberate product decision.
// Policy/security halts (which do NOT inherit) and the SAME-owner paths are
// covered by resume_matrix_test.go's TestDifferentUserTakeoverOfTerminalThread.
func TestDeliverDifferentExternalUserTakesOverArchivedSession(t *testing.T) {
	ch := newChannel("c1")
	archived := archivedSession(t, "c1-old", "thread:C1:1", spiceboxv1alpha1.AgentSessionPhaseSucceeded, "U_original")
	p, _, mem, _, cli := newPipeline(t, ch, archived)

	dec, err := p.Deliver(context.Background(), channelkinds.InboundEvent{
		Channel:     ch,
		ExternalIDs: channelkinds.ExternalIdentity{Kind: "slack", ExternalID: "U_coworker", Email: "coworker@example.com"},
		ChannelKey:  "thread:C1:1",
		MessageText: "let me pick this up",
	})
	require.NoError(t, err, "Deliver")

	require.Equal(t, channelkinds.OutcomeForkPending, dec.Outcome,
		"a different user takes over a terminal thread (new session they own), not denied")
	assert.NotEmpty(t, dec.Notice, "takeover surfaces an in-thread notice")
	assert.Empty(t, mem.appends, "channelsd writes a fork-trigger, not a memory append (the operator fork copies)")

	// A takeover fork-trigger is stamped on the archived parent: ownership
	// transfers to the coworker; an ordinary terminal (Succeeded) inherits.
	pr := pendingRestartOf(t, cli, archived.Name)
	require.NotNil(t, pr, "takeover fork-trigger written on the terminal parent")
	assert.Equal(t, spiceboxv1alpha1.PendingRestartModeTakeover, pr.Mode)
	assert.True(t, pr.InheritHistory, "ordinary terminal (Succeeded) inherits the transcript")
	assert.Equal(t, "U_coworker", pr.NewOwnerExternalID, "the coworker becomes the new owner")

	// channelsd does not directly create the child; the operator fork does.
	var sessions spiceboxv1alpha1.AgentSessionList
	require.NoError(t, cli.List(context.Background(), &sessions))
	assert.Len(t, sessions.Items, 1, "only the archived parent exists at the channelsd layer")
}

// pendingRestartOf re-Gets the named session and returns its
// status.pendingRestart (nil if unset). Used to assert the operator-driven
// inherit fork-trigger channelsd writes on a terminal parent.
func pendingRestartOf(t *testing.T, cli client.Client, name string) *spiceboxv1alpha1.PendingRestart {
	t.Helper()
	var got spiceboxv1alpha1.AgentSession
	require.NoError(t, cli.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: name}, &got), "re-Get session")
	return got.Status.PendingRestart
}

// TestDeliverInheritanceAllowsOriginalUser verifies that the original
// requester replying to their own archived (Succeeded) thread writes an
// inherit fork-trigger on the terminal parent (the operator materializes the
// fresh session) — channelsd never routes into the dead CR and never copies
// the transcript itself.
func TestDeliverInheritanceAllowsOriginalUser(t *testing.T) {
	ch := newChannel("c1")
	archived := archivedSession(t, "c1-old", "thread:C1:1", spiceboxv1alpha1.AgentSessionPhaseSucceeded, "U_original")
	p, _, mem, _, cli := newPipeline(t, ch, archived)

	dec, err := p.Deliver(context.Background(), channelkinds.InboundEvent{
		Channel:     ch,
		ExternalIDs: channelkinds.ExternalIdentity{Kind: "slack", ExternalID: "U_original"},
		ChannelKey:  "thread:C1:1",
		MessageText: "follow-up from the original requester",
	})
	require.NoError(t, err, "Deliver")
	require.Equal(t, channelkinds.OutcomeForkPending, dec.Outcome,
		"a continuation of a terminal session forks; it is never routed into the dead CR")
	assert.False(t, dec.NewSession, "channelsd creates no session on the fork path")
	assert.NotEmpty(t, dec.Notice, "user must get a visible fork acknowledgement")

	// Fork-trigger written on the terminal parent, carrying the new message +
	// inherit mode. channelsd wrote no memory (the operator copies the
	// transcript) and created no new session.
	pr := pendingRestartOf(t, cli, "c1-old")
	require.NotNil(t, pr, "inherit fork-trigger must be written on the terminal parent")
	assert.Equal(t, spiceboxv1alpha1.PendingRestartModeInherit, pr.Mode, "Mode=inherit")
	assert.Equal(t, "follow-up from the original requester", pr.NewUserText, "carries the new message")
	assert.NotEmpty(t, pr.TargetSessionName, "carries a non-colliding child name")
	assert.NotEqual(t, "c1-old", pr.TargetSessionName, "child name must not collide with the terminal parent")
	assert.Empty(t, mem.appends, "channelsd must not copy the transcript itself")

	var sessions spiceboxv1alpha1.AgentSessionList
	require.NoError(t, cli.List(context.Background(), &sessions))
	assert.Len(t, sessions.Items, 1, "only the terminal parent exists; the child is the operator's to create")
}

// TestDeliverFreshInheritanceWithNoAnnotationFallback covers the archived
// session with no started-by annotation. Rather than trust any sender,
// the inheritance path falls back to a SpiceDB interact check on the terminal
// parent (defense in depth, mirroring the active-session path). A requester WITH
// interact is allowed and the fork-trigger is written; one WITHOUT is denied.
func TestDeliverFreshInheritanceWithNoAnnotationFallback(t *testing.T) {
	t.Run("interact granted: ForkPending, fork-trigger written", func(t *testing.T) {
		ch := newChannel("c1")
		// Legacy archived: no StartedByExternalID annotation.
		archived := archivedSession(t, "c1-legacy", "thread:C1:1", spiceboxv1alpha1.AgentSessionPhaseSucceeded, "")
		p, az, _, _, cli := newPipeline(t, ch, archived)
		az.checkResult = true // sender has interact on the terminal parent
		dec, err := p.Deliver(context.Background(), channelkinds.InboundEvent{
			Channel:     ch,
			ExternalIDs: channelkinds.ExternalIdentity{Kind: "slack", ExternalID: "U_anyone"},
			ChannelKey:  "thread:C1:1",
			MessageText: "first inbound on legacy archived thread",
		})
		require.NoError(t, err, "Deliver")
		require.Equal(t, channelkinds.OutcomeForkPending, dec.Outcome,
			"want ForkPending when archived has no annotation but the sender has interact")
		assert.NotNil(t, pendingRestartOf(t, cli, "c1-legacy"), "fork-trigger written")
	})

	t.Run("interact denied: DeniedByPermission, no fork-trigger", func(t *testing.T) {
		ch := newChannel("c1")
		archived := archivedSession(t, "c1-legacy", "thread:C1:1", spiceboxv1alpha1.AgentSessionPhaseSucceeded, "")
		p, az, _, _, cli := newPipeline(t, ch, archived)
		az.checkResult = false // sender lacks interact — no annotation vouches for them
		dec, err := p.Deliver(context.Background(), channelkinds.InboundEvent{
			Channel:     ch,
			ExternalIDs: channelkinds.ExternalIdentity{Kind: "slack", ExternalID: "U_anyone"},
			ChannelKey:  "thread:C1:1",
			MessageText: "first inbound on legacy archived thread",
		})
		require.NoError(t, err, "Deliver")
		require.Equal(t, channelkinds.OutcomeDeniedByPermission, dec.Outcome,
			"an unverifiable sender (no annotation, no interact) must be denied — defense in depth")
		assert.Nil(t, pendingRestartOf(t, cli, "c1-legacy"), "no fork-trigger written for a denied sender")
	})
}

func TestDeliverIdleAnnotationPatch(t *testing.T) {
	ch := newChannel("c1")
	existing := existingSession(t, "c1-abc", "", spiceboxv1alpha1.AgentSessionPhaseIdle)
	existing.Spec.InputChannel = nil
	p, _, _, _, cli := newPipeline(t, ch, existing)
	_, err := p.Deliver(context.Background(), channelkinds.InboundEvent{
		Channel:     ch,
		ExternalIDs: channelkinds.ExternalIdentity{Kind: "slack", ExternalID: "U1", Email: "a@example.com"},
		ChannelKey:  "thread:C1:1",
		MessageText: "wake up",
	})
	require.NoError(t, err, "Deliver")

	got := &spiceboxv1alpha1.AgentSession{}
	require.NoError(t, cli.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "c1-abc"}, got))
	assert.NotEmpty(t, got.Annotations[spiceboxv1alpha1.AnnotationWakeRequestedAt],
		"wake annotation not patched on Idle session")
}

// A SpiceDB blip on the started_by write must not be TERMINAL. Treated as
// terminal, channelsd sets status.startFailure, the operator drives the session
// to Failed, and because isTerminalPhase short-circuits every later reconcile
// nothing ever retries the write: the user's first message — already durable in
// spec.prompt.inline of that dead session — is never processed, and all they
// see is a generic "internal error" ephemeral. One rolling restart of SpiceDB
// (two replicas in production, restarted on every upgrade) is enough to
// trigger it.
//
// The write is a WriteRelationships, not a permission check: SpiceDB expresses
// "denied" as a permissionship on a Check response, never as an error here, so
// there is no denial for a retry to loop on — and TOUCH is idempotent. A
// bounded retry is therefore safe, and it is the difference between losing the
// session and riding out the blip.
func TestDeliver_TransientAuthzWriteError_RetriesRatherThanKillingTheSession(t *testing.T) {
	ch := newChannel("c1")
	p, az, _, _, cli := newPipeline(t, ch)
	az.startedByTransientErr = errors.New("rpc error: code = Unavailable desc = connection refused")
	az.startedByTransientCount = 1

	_, err := p.Deliver(context.Background(), channelkinds.InboundEvent{
		Channel:     ch,
		ExternalIDs: channelkinds.ExternalIdentity{Kind: "slack", ExternalID: "U1", Email: "a@example.com"},
		ChannelKey:  "thread:C1:1",
		MessageText: "hi",
	})
	require.NoError(t, err, "a single transient authz-write failure must not fail the inbound")

	assert.Equal(t, 2, az.startedByCalls, "the write must be retried after a transient failure")
	var list spiceboxv1alpha1.AgentSessionList
	require.NoError(t, cli.List(context.Background(), &list))
	require.Len(t, list.Items, 1, "session count")
	assert.Nil(t, list.Items[0].Status.StartFailure,
		"a ridden-out blip must leave no startFailure — that signal is terminal, and the operator never retries it")
}

func TestDeliverAuthzWriteFailure(t *testing.T) {
	ch := newChannel("c1")
	p, az, _, _, cli := newPipeline(t, ch)
	az.startedByErr = errors.New("spicedb down")
	_, err := p.Deliver(context.Background(), channelkinds.InboundEvent{
		Channel:     ch,
		ExternalIDs: channelkinds.ExternalIdentity{Kind: "slack", ExternalID: "U1", Email: "a@example.com"},
		ChannelKey:  "thread:C1:1",
		MessageText: "hi",
	})
	require.Error(t, err, "expected Deliver to error on authz write failure")
	assert.Greater(t, az.startedByCalls, 1,
		"a persistent failure must still be RETRIED before giving up — the retry is bounded, not skipped")

	// channelsd sets status.startFailure (a channelsd-owned signal field) so the
	// authz-write failure is visible on status, not just logged. The operator reads
	// startFailure on the next reconcile and produces the Failed state — channelsd
	// never writes phase/failureReason/finishedAt itself (L2 ownership split). The
	// error also propagates to the caller above (no-silent-errors rule).
	var list spiceboxv1alpha1.AgentSessionList
	require.NoError(t, cli.List(context.Background(), &list))
	require.Len(t, list.Items, 1, "session count")
	got := list.Items[0]
	require.NotNil(t, got.Status.StartFailure,
		"channelsd must set startFailure on authz write failure")
	assert.Equal(t, spiceboxv1alpha1.ReasonAgentSessionAuthzWriteFail, got.Status.StartFailure.Reason,
		"startFailure.reason must carry the authz write failure reason")
	assert.NotEmpty(t, got.Status.StartFailure.Message,
		"startFailure.message must carry the error text")
	assert.Empty(t, got.Status.Phase,
		"channelsd must NOT write phase — the operator derives Failed from startFailure")
	assert.Empty(t, got.Status.FailureReason,
		"channelsd must NOT write failureReason — the operator derives it from startFailure")
	assert.Nil(t, got.Status.FinishedAt,
		"channelsd must NOT write finishedAt — the operator stamps it on the Failed transition")
}

// TestDeliverFreshNoPrior verifies: no prior sessions → new session created,
// InheritFrom is empty, no memory copy.
func TestDeliverFreshNoPrior(t *testing.T) {
	ch := newChannel("c1")
	p, _, mem, _, cli := newPipeline(t, ch)
	dec, err := p.Deliver(context.Background(), channelkinds.InboundEvent{
		Channel:     ch,
		ExternalIDs: channelkinds.ExternalIdentity{Kind: "fake", ExternalID: "U1", Email: "a@example.com"},
		ChannelKey:  "thread:C1:fresh",
		MessageText: "first message",
	})
	require.NoError(t, err, "Deliver")
	assert.Equal(t, channelkinds.OutcomeRouted, dec.Outcome)

	var sessions spiceboxv1alpha1.AgentSessionList
	require.NoError(t, cli.List(context.Background(), &sessions))
	require.Len(t, sessions.Items, 1, "sessions created")
	got := sessions.Items[0]
	require.NotNil(t, got.Spec.InputChannel, "spec.inputChannel")
	assert.Empty(t, got.Spec.InputChannel.InheritFrom, "InheritFrom should be empty for fresh session")
	// No memory copy for a fresh session with no archived predecessor.
	assert.Empty(t, mem.appends, "memory appends on fresh session")
}

// newLocalChannel builds a kind=local Channel CR. The `local` kind's
// SpawnsSessionOnInbound() is false — `oap agent chat` owns exactly one
// pre-created session — so the pipeline must NOT spawn a session for an
// inbound that finds no active session.
func newLocalChannel(name string) *spiceboxv1alpha1.Channel {
	return &spiceboxv1alpha1.Channel{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: spiceboxv1alpha1.ChannelSpec{
			Kind: "local", AgentClass: "ac1",
			Role:           spiceboxv1alpha1.ChannelRoleBoth,
			CredentialsRef: spiceboxv1alpha1.ChannelCredentialsRef{SecretName: "s"},
		},
	}
}

// TestDeliver_NoSpawn_WhenKindForbidsSpawn is the airtight guarantee for
// the `local` TUI kind: an inbound that reaches Deliver with no active
// session must NOT spawn a phantom `<channel>-<uuid>` session — the
// pipeline returns OutcomeNoActiveSession instead. The companion case
// asserts a spawning kind (fake) still creates a session on the same
// no-active-session input, so the gate is the kind's choice, not a
// blanket change.
func TestDeliver_NoSpawn_WhenKindForbidsSpawn(t *testing.T) {
	cases := []struct {
		name        string
		channel     *spiceboxv1alpha1.Channel
		wantOutcome channelkinds.Outcome
		wantCreated bool
	}{
		{
			name:        "local kind: no active session → OutcomeNoActiveSession, no session created",
			channel:     newLocalChannel("local-chan"),
			wantOutcome: channelkinds.OutcomeNoActiveSession,
			wantCreated: false,
		},
		{
			name:        "fake kind: no active session → OutcomeRouted, session spawned",
			channel:     newChannel("c1"),
			wantOutcome: channelkinds.OutcomeRouted,
			wantCreated: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, az, mem, nats, cli := newPipeline(t, tc.channel)
			dec, err := p.Deliver(context.Background(), channelkinds.InboundEvent{
				Channel:     tc.channel,
				ExternalIDs: channelkinds.ExternalIdentity{Kind: "local", ExternalID: "alice"},
				ChannelKey:  "local:" + tc.channel.Name + ":1",
				MessageText: "anyone there?",
			})
			require.NoError(t, err, "Deliver must not error — OutcomeNoActiveSession is a clean result")
			assert.Equal(t, tc.wantOutcome, dec.Outcome, "decision outcome")

			var sessions spiceboxv1alpha1.AgentSessionList
			require.NoError(t, cli.List(context.Background(), &sessions))
			if tc.wantCreated {
				assert.Len(t, sessions.Items, 1, "spawning kind must create the session")
				assert.Equal(t, 1, az.startedByCalls, "spawning kind writes started_by")
			} else {
				assert.Empty(t, sessions.Items,
					"non-spawning kind must NOT create a phantom session")
				assert.Equal(t, 0, az.startedByCalls,
					"no started_by write when no session is created")
				assert.Empty(t, mem.appends, "no memory append when not delivered")
				assert.Empty(t, nats.subjects, "no NATS publish when not delivered")
			}
		})
	}
}

// TestDeliver_NoSpawn_RoutesToPreCreatedLocalSession verifies the
// no-spawn gate does NOT break the normal path: when `oap agent chat` has
// pre-created the local session and it is still active, a follow-up
// inbound routes to it (the gate only fires when no active session
// exists).
func TestDeliver_NoSpawn_RoutesToPreCreatedLocalSession(t *testing.T) {
	ch := newLocalChannel("local-chan")
	keyHash := sha256HexTest("local:local-chan:1")
	active := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{
			Name: "local-chan-sess", Namespace: "default",
			Labels: map[string]string{
				spiceboxv1alpha1.LabelChannelName: "local-chan",
				spiceboxv1alpha1.LabelChannelKey:  keyHash,
			},
		},
		Spec: spiceboxv1alpha1.AgentSessionSpec{
			Class: "ac1",
			InputChannel: &spiceboxv1alpha1.ChannelBinding{
				Name: "local-chan", Kind: "local", Key: "local:local-chan:1",
				NATSSubjectPrefix: "ap.session.default.local-chan-sess",
			},
		},
		Status: spiceboxv1alpha1.AgentSessionStatus{Phase: spiceboxv1alpha1.AgentSessionPhaseRunning},
	}
	p, _, mem, _, _ := newPipeline(t, ch, active)
	dec, err := p.Deliver(context.Background(), channelkinds.InboundEvent{
		Channel:     ch,
		ExternalIDs: channelkinds.ExternalIdentity{Kind: "local", ExternalID: "alice"},
		ChannelKey:  "local:local-chan:1",
		MessageText: "follow-up",
	})
	require.NoError(t, err, "Deliver")
	assert.Equal(t, channelkinds.OutcomeRouted, dec.Outcome,
		"follow-up to the active pre-created local session routes normally")
	assert.Equal(t, "local-chan-sess", dec.Session.Name, "routed session")
	require.Len(t, mem.appends, 1, "the follow-up turn is appended")
}

// TestDeliverInheritsFromArchivedPhase collapses the Succeeded/Failed
// inheritance-source tests — both verify that an archived session for the
// same channelKey acts as the inheritance source, with InheritFrom set
// and memory turns copied. Only the predecessor's Phase varies.
// TestDeliverInheritsFromArchivedPhase: a new inbound for a terminal session a
// clean retry can continue from (Succeeded, or Failed with a transient boot
// reason) writes an inherit fork-trigger on that parent — never routed into
// the dead CR, never copied by channelsd. The operator's fork reconciler owns
// the transcript copy + child materialization.
func TestDeliverInheritsFromArchivedPhase(t *testing.T) {
	cases := []struct {
		name          string
		phase         string
		failureReason string // set for the Failed (transient) case
		archiveNm     string
		key           string
	}{
		{
			name:      "Phase=Succeeded: forks an inheriting continuation of c1-old",
			phase:     spiceboxv1alpha1.AgentSessionPhaseSucceeded,
			archiveNm: "c1-old",
			key:       "thread:C1:1",
		},
		{
			name:          "Phase=Failed (transient): forks an inheriting continuation of c1-failed",
			phase:         spiceboxv1alpha1.AgentSessionPhaseFailed,
			failureReason: "MemoryUnavailable", // a transient boot failure → NewInheriting, not Refuse
			archiveNm:     "c1-failed",
			key:           "thread:C1:2",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ch := newChannel("c1")
			archived := archivedSession(t, tc.archiveNm, tc.key, tc.phase, "")
			archived.Status.FailureReason = tc.failureReason
			p, _, mem, _, cli := newPipeline(t, ch, archived)

			dec, err := p.Deliver(context.Background(), channelkinds.InboundEvent{
				Channel:     ch,
				ExternalIDs: channelkinds.ExternalIdentity{Kind: "fake", ExternalID: "U1", Email: "a@example.com"},
				ChannelKey:  tc.key,
				MessageText: "continue",
			})
			require.NoError(t, err, "Deliver")
			require.Equal(t, channelkinds.OutcomeForkPending, dec.Outcome)

			// No new session created by channelsd; only the fork-trigger written.
			var sessions spiceboxv1alpha1.AgentSessionList
			require.NoError(t, cli.List(context.Background(), &sessions))
			assert.Len(t, sessions.Items, 1, "channelsd creates no child; the operator forks")

			pr := pendingRestartOf(t, cli, tc.archiveNm)
			require.NotNil(t, pr, "inherit fork-trigger must be written on the terminal parent")
			assert.Equal(t, spiceboxv1alpha1.PendingRestartModeInherit, pr.Mode, "Mode=inherit")
			assert.Equal(t, "continue", pr.NewUserText, "carries the new message")
			assert.Empty(t, mem.appends, "channelsd must not copy the transcript itself")
			assert.NotEmpty(t, dec.Notice, "a fork that WAS triggered still acks the user once")
		})
	}
}

// TestDeliverArchivedSession_ResumesInSameThread pins that a reply to a session
// the archive sweep parked wakes that same session, in that same thread — no
// fork, no new thread, no inherit-fork notice.
func TestDeliverArchivedSession_ResumesInSameThread(t *testing.T) {
	ch := newChannel("c1")
	parked := archivedSession(t, "c1-parked", "thread:C1:1", spiceboxv1alpha1.AgentSessionPhaseSucceeded, "U1")
	parked.Status.Conditions = []metav1.Condition{{
		Type: spiceboxv1alpha1.AgentSessionConditionIdle, Status: metav1.ConditionFalse,
		Reason: spiceboxv1alpha1.ReasonAgentSessionArchived, LastTransitionTime: metav1.Now(),
	}}
	p, _, mem, _, cli := newPipeline(t, ch, parked)
	mem.seeded["default/c1-parked"] = []MemTurn{{Index: 0, Role: "user"}}

	dec, err := p.Deliver(context.Background(), channelkinds.InboundEvent{
		Channel:     ch,
		ExternalIDs: channelkinds.ExternalIdentity{Kind: "fake", ExternalID: "U1", Email: "a@example.com"},
		ChannelKey:  "thread:C1:1",
		MessageText: "continue please",
	})
	require.NoError(t, err, "Deliver")

	assert.Equal(t, channelkinds.OutcomeRouted, dec.Outcome, "an archived session is parked, not finished")
	assert.Equal(t, "c1-parked", dec.Session.Name, "routes into the SAME session")
	assert.Empty(t, dec.Notice, "no fresh-session ack: nothing forked")
	assert.Nil(t, pendingRestartOf(t, cli, "c1-parked"), "no fork-trigger written")

	// Routing the message in is only half the job: a swept session's pod was
	// reaped, so without the wake annotation the operator never respawns the
	// runner and the turn strands silently with no reply.
	var got spiceboxv1alpha1.AgentSession
	require.NoError(t, cli.Get(context.Background(),
		client.ObjectKey{Namespace: "default", Name: "c1-parked"}, &got))
	assert.NotEmpty(t, got.Annotations[spiceboxv1alpha1.AnnotationWakeRequestedAt],
		"the operator must be asked to respawn the reaped runner")
}

// TestDeliverArchivedSession_ForksWhenTranscriptIsGone pins the fallback: a
// swept session whose memory retention already reclaimed the transcript cannot
// be resumed into an amnesiac agent, so it forks and seeds from what remains.
func TestDeliverArchivedSession_ForksWhenTranscriptIsGone(t *testing.T) {
	ch := newChannel("c1")
	parked := archivedSession(t, "c1-parked", "thread:C1:1", spiceboxv1alpha1.AgentSessionPhaseSucceeded, "U1")
	parked.Status.Conditions = []metav1.Condition{{
		Type: spiceboxv1alpha1.AgentSessionConditionIdle, Status: metav1.ConditionFalse,
		Reason: spiceboxv1alpha1.ReasonAgentSessionArchived, LastTransitionTime: metav1.Now(),
	}}
	p, _, _, _, cli := newPipeline(t, ch, parked) // mem.seeded left empty: no turns

	dec, err := p.Deliver(context.Background(), channelkinds.InboundEvent{
		Channel:     ch,
		ExternalIDs: channelkinds.ExternalIdentity{Kind: "fake", ExternalID: "U1", Email: "a@example.com"},
		ChannelKey:  "thread:C1:1",
		MessageText: "continue please",
	})
	require.NoError(t, err, "Deliver")

	assert.Equal(t, channelkinds.OutcomeForkPending, dec.Outcome, "no transcript to resume into")
	require.NotNil(t, pendingRestartOf(t, cli, "c1-parked"), "falls back to the inheriting fork")
}

// TestDeliverArchivedSession_ForksWhenTranscriptReadErrors verifies that when
// a swept session's transcript cannot be read from memory (transient I/O error),
// the pipeline logs and fails closed to the fork rather than silently stranding
// the inbound.
func TestDeliverArchivedSession_ForksWhenTranscriptReadErrors(t *testing.T) {
	ch := newChannel("c1")
	parked := archivedSession(t, "c1-parked", "thread:C1:1", spiceboxv1alpha1.AgentSessionPhaseSucceeded, "U1")
	parked.Status.Conditions = []metav1.Condition{{
		Type: spiceboxv1alpha1.AgentSessionConditionIdle, Status: metav1.ConditionFalse,
		Reason: spiceboxv1alpha1.ReasonAgentSessionArchived, LastTransitionTime: metav1.Now(),
	}}
	p, _, mem, _, cli := newPipeline(t, ch, parked)
	mem.readErr = errors.New("memory backend unavailable")

	dec, err := p.Deliver(context.Background(), channelkinds.InboundEvent{
		Channel:     ch,
		ExternalIDs: channelkinds.ExternalIdentity{Kind: "fake", ExternalID: "U1", Email: "a@example.com"},
		ChannelKey:  "thread:C1:1",
		MessageText: "continue please",
	})
	require.NoError(t, err, "Deliver must not error on memory read failure (fails closed to fork)")

	assert.Equal(t, channelkinds.OutcomeForkPending, dec.Outcome, "transcript read error falls back to the inheriting fork")
	assert.NotNil(t, pendingRestartOf(t, cli, "c1-parked"), "fork-trigger written on the terminal parent")
	assert.NotEmpty(t, dec.Notice, "user receives notice that a fresh session is being prepared")
}

// TestDeliverTerminalContinuation_StaleForkTrigger_PostsNoDuplicateNotice pins
// that the inherit-fork ack is posted only when this inbound actually wrote a
// fork-trigger.
//
// writeInheritForkTrigger is first-writer-wins: a parent that already carries a
// PendingRestart is left untouched, and Deliver must learn that it wrote
// nothing. Acking unconditionally promises a fresh session that never arrives,
// once per reply to a terminal thread whose marker has not been consumed — and
// with a marker that never clears, once per message the user sends.
func TestDeliverTerminalContinuation_StaleForkTrigger_PostsNoDuplicateNotice(t *testing.T) {
	ch := newChannel("c1")
	archived := archivedSession(t, "c1-old", "thread:C1:1", spiceboxv1alpha1.AgentSessionPhaseSucceeded, "")
	archived.Status.PendingRestart = &spiceboxv1alpha1.PendingRestart{
		Mode:              spiceboxv1alpha1.PendingRestartModeInherit,
		NewUserText:       "continue",
		TriggeredBy:       "user:alice",
		TargetSessionName: "c1-old-icabc12",
	}
	p, _, _, _, cli := newPipeline(t, ch, archived)

	dec, err := p.Deliver(context.Background(), channelkinds.InboundEvent{
		Channel:     ch,
		ExternalIDs: channelkinds.ExternalIdentity{Kind: "fake", ExternalID: "U1", Email: "a@example.com"},
		ChannelKey:  "thread:C1:1",
		MessageText: "continue",
	})
	require.NoError(t, err, "Deliver")

	assert.Empty(t, dec.Notice,
		"no fork-trigger was written, so the user must not be told a fresh session is starting")

	pr := pendingRestartOf(t, cli, "c1-old")
	require.NotNil(t, pr, "first-writer-wins: the existing trigger stays intact")
	assert.Equal(t, "c1-old-icabc12", pr.TargetSessionName, "the original trigger must not be clobbered")
}

// TestDeliverActiveOverridesArchived verifies: when both an active (Idle)
// session AND an older archived (Succeeded) session exist for the same
// channelKey, the active session wins and no memory copy occurs.
func TestDeliverActiveOverridesArchived(t *testing.T) {
	ch := newChannel("c1")
	keyHash := sha256HexTest("thread:C1:3")

	archivedSess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{
			Name: "c1-old", Namespace: "default",
			Labels: map[string]string{
				spiceboxv1alpha1.LabelChannelName: "c1",
				spiceboxv1alpha1.LabelChannelKey:  keyHash,
			},
		},
		Spec:   spiceboxv1alpha1.AgentSessionSpec{Class: "ac1"},
		Status: spiceboxv1alpha1.AgentSessionStatus{Phase: spiceboxv1alpha1.AgentSessionPhaseSucceeded},
	}
	activeSess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{
			Name: "c1-active", Namespace: "default",
			Labels: map[string]string{
				spiceboxv1alpha1.LabelChannelName: "c1",
				spiceboxv1alpha1.LabelChannelKey:  keyHash,
			},
		},
		Spec: spiceboxv1alpha1.AgentSessionSpec{
			Class: "ac1",
			InputChannel: &spiceboxv1alpha1.ChannelBinding{
				Name: "c1", Kind: "fake", Key: "thread:C1:3",
				NATSSubjectPrefix: "ap.session.default.c1-active",
			},
		},
		Status: spiceboxv1alpha1.AgentSessionStatus{Phase: spiceboxv1alpha1.AgentSessionPhaseIdle},
	}

	p, _, mem, nats, _ := newPipeline(t, ch, archivedSess, activeSess)
	// Seed some memory for the archived session (should not be copied).
	mem.seeded["default/c1-old"] = []MemTurn{
		{Index: 1, Role: "user", Content: []MemContent{{Type: "text", Text: "old turn"}}},
	}

	dec, err := p.Deliver(context.Background(), channelkinds.InboundEvent{
		Channel:     ch,
		ExternalIDs: channelkinds.ExternalIdentity{Kind: "fake", ExternalID: "U1", Email: "a@example.com"},
		ChannelKey:  "thread:C1:3",
		MessageText: "reply to active",
	})
	require.NoError(t, err, "Deliver")
	assert.Equal(t, channelkinds.OutcomeRouted, dec.Outcome)
	// Must route to the active session, not create a new one.
	assert.Equal(t, "c1-active", dec.Session.Name, "routed session")
	// Only the single user-turn append for the active session path.
	require.Len(t, mem.appends, 1, "memory appends (active-session user turn)")
	assert.Equal(t, "c1-active", mem.appends[0].name, "turn appended to wrong session")
	// NATS wakeup published (Idle path).
	assert.Len(t, nats.subjects, 1, "nats publishes")
}

// TestDeliverArchivedNewestWins verifies that when two archived sessions exist
// for the same channelKey with distinct CreationTimestamps, the newer one is
// selected as the inheritance source.
func TestDeliverArchivedNewestWins(t *testing.T) {
	ch := newChannel("c1")
	keyHash := sha256HexTest("thread:C1:newest")

	older := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{
			Name: "c1-older", Namespace: "default",
			CreationTimestamp: metav1.NewTime(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)),
			Labels: map[string]string{
				spiceboxv1alpha1.LabelChannelName: "c1",
				spiceboxv1alpha1.LabelChannelKey:  keyHash,
			},
		},
		Spec:   spiceboxv1alpha1.AgentSessionSpec{Class: "ac1"},
		Status: spiceboxv1alpha1.AgentSessionStatus{Phase: spiceboxv1alpha1.AgentSessionPhaseSucceeded},
	}
	newer := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{
			Name: "c1-newer", Namespace: "default",
			CreationTimestamp: metav1.NewTime(time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)),
			Labels: map[string]string{
				spiceboxv1alpha1.LabelChannelName: "c1",
				spiceboxv1alpha1.LabelChannelKey:  keyHash,
			},
		},
		Spec:   spiceboxv1alpha1.AgentSessionSpec{Class: "ac1"},
		Status: spiceboxv1alpha1.AgentSessionStatus{Phase: spiceboxv1alpha1.AgentSessionPhaseSucceeded},
	}

	p, _, _, _, cli := newPipeline(t, ch, older, newer)

	dec, err := p.Deliver(context.Background(), channelkinds.InboundEvent{
		Channel:     ch,
		ExternalIDs: channelkinds.ExternalIdentity{Kind: "fake", ExternalID: "U1", Email: "a@example.com"},
		ChannelKey:  "thread:C1:newest",
		MessageText: "new message",
	})
	require.NoError(t, err, "Deliver")
	assert.Equal(t, channelkinds.OutcomeForkPending, dec.Outcome)

	// The fork-trigger lands on the NEWER archived session; the older one is
	// left untouched.
	assert.NotNil(t, pendingRestartOf(t, cli, "c1-newer"),
		"the newer archived session is the inheritance source")
	assert.Nil(t, pendingRestartOf(t, cli, "c1-older"),
		"the older archived session must not be triggered")
}

// agentClassForTest returns an AgentClass with the given
// SessionInteractPermission, ready to drop into newPipeline.
func agentClassForTest(t *testing.T, sessionInteractPermission string) *spiceboxv1alpha1.AgentClass {
	t.Helper()
	return &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: "ac1", Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentClassSpec{
			Authz: &spiceboxv1alpha1.AuthzBlock{
				Session: &spiceboxv1alpha1.SessionAuthz{
					InteractPermission: sessionInteractPermission,
				},
			},
			Model: &spiceboxv1alpha1.ModelConfig{
				Provider: "anthropic",
				Name:     "claude-3-opus-20240229",
				APIKey:   spiceboxv1alpha1.SecretKeyRef{Name: "s", Key: "k"},
			},
			SystemPrompt: spiceboxv1alpha1.PromptSource{Inline: "you are a test agent"},
			Budget: &spiceboxv1alpha1.BudgetConfig{
				MaxTurns:    10,
				MaxTokens:   1000,
				MaxDuration: metav1.Duration{Duration: 30 * time.Minute},
			},
		},
	}
}

// TestDeliver_WritesParticipantWhenAgentClassHasSessionInteractPermission
// verifies that when the AgentClass has a non-empty SessionInteractPermission,
// the pipeline calls TouchInteractParticipant with that subject on the create
// path.
func TestDeliver_WritesParticipantWhenAgentClassHasSessionInteractPermission(t *testing.T) {
	ch := newChannel("c1")
	agentClass := agentClassForTest(t, "group:engineering#member")
	p, az, _, _, _ := newPipeline(t, ch, agentClass)

	dec, err := p.Deliver(context.Background(), channelkinds.InboundEvent{
		Channel:     ch,
		ExternalIDs: channelkinds.ExternalIdentity{Kind: "fake", ExternalID: "U1", Email: "a@example.com"},
		ChannelKey:  "thread:C1:1",
		MessageText: "hello",
	})
	require.NoError(t, err, "Deliver")
	assert.Equal(t, channelkinds.OutcomeRouted, dec.Outcome)
	assert.Equal(t, 1, az.participantCalls, "TouchInteractParticipant calls")
	assert.Equal(t, "group:engineering#member", az.lastParticipantSubject, "lastParticipantSubject")
}

// TestDeliver_StampsInteractPolicyAppliedCondition verifies that after a
// successful participant write the AgentSession status carries
// AppliedInteractPermission, AppliedInteractPermissionAt, and the
// InteractPolicyApplied condition with Status=True, Reason=AppliedFromAgentClass.
func TestDeliver_StampsInteractPolicyAppliedCondition(t *testing.T) {
	ch := newChannel("c1")
	agentClass := agentClassForTest(t, "group:engineering#member")
	p, _, _, _, cli := newPipeline(t, ch, agentClass)

	dec, err := p.Deliver(context.Background(), channelkinds.InboundEvent{
		Channel:     ch,
		ExternalIDs: channelkinds.ExternalIdentity{Kind: "fake", ExternalID: "U1", Email: "a@example.com"},
		ChannelKey:  "thread:C1:2",
		MessageText: "hello",
	})
	require.NoError(t, err, "Deliver")
	require.Equal(t, channelkinds.OutcomeRouted, dec.Outcome)

	// Read back the session to check status.
	var sess spiceboxv1alpha1.AgentSession
	require.NoError(t, cli.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: dec.Session.Name}, &sess),
		"get session")

	assert.Equal(t, "group:engineering#member", sess.Status.AppliedInteractPermission,
		"AppliedInteractPermission")
	wantAt := time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC)
	if assert.NotNil(t, sess.Status.AppliedInteractPermissionAt, "AppliedInteractPermissionAt") {
		assert.True(t, sess.Status.AppliedInteractPermissionAt.Time.Equal(wantAt),
			"AppliedInteractPermissionAt = %v; want %v",
			sess.Status.AppliedInteractPermissionAt.Time, wantAt)
	}

	cond := conditions.Find(sess.Status.Conditions, spiceboxv1alpha1.AgentSessionConditionInteractPolicyApplied)
	if assert.NotNil(t, cond, "condition %q (have: %+v)",
		spiceboxv1alpha1.AgentSessionConditionInteractPolicyApplied, sess.Status.Conditions) {
		assert.Equal(t, metav1.ConditionTrue, cond.Status, "condition.Status")
		assert.Equal(t, spiceboxv1alpha1.ReasonInteractPolicyApplied, cond.Reason, "condition.Reason")
	}
}

// TestDeliverRefusesUnrecoverablyFailedSession: a new inbound for a session
// that failed in a way a plain retry cannot fix (a hard failure reason not on
// the transient-boot list) is REFUSED with a loud, reason-bearing notice — no
// fork-trigger, no new session, no "starting…" status stranding the thread.
func TestDeliverRefusesUnrecoverablyFailedSession(t *testing.T) {
	ch := newChannel("c1")
	archived := archivedSession(t, "c1-dead", "thread:C1:4", spiceboxv1alpha1.AgentSessionPhaseFailed, "")
	archived.Status.FailureReason = "BudgetExhausted" // hard failure → Refuse

	p, _, mem, _, cli := newPipeline(t, ch, archived)

	dec, err := p.Deliver(context.Background(), channelkinds.InboundEvent{
		Channel:     ch,
		ExternalIDs: channelkinds.ExternalIdentity{Kind: "fake", ExternalID: "U1", Email: "a@example.com"},
		ChannelKey:  "thread:C1:4",
		MessageText: "please keep going",
	})
	require.NoError(t, err, "Deliver")
	require.Equal(t, channelkinds.OutcomeRefused, dec.Outcome,
		"an unrecoverably-failed session refuses the continuation")
	require.False(t, dec.Notice.IsSuppressed(), "refusal must be loud, not silent")
	// The failure reason is externally derived, so it rides the Excerpt, which
	// every surface renders inert.
	require.NotNil(t, dec.Notice.Args().Excerpt, "refusal names the failure reason")
	assert.Contains(t, dec.Notice.Args().Excerpt.Content, "BudgetExhausted")

	// Nothing materialized: no fork-trigger, no new session, no memory append.
	assert.Nil(t, pendingRestartOf(t, cli, "c1-dead"), "refusal must not write a fork-trigger")
	assert.Empty(t, mem.appends, "refusal must not append memory")
	var sessions spiceboxv1alpha1.AgentSessionList
	require.NoError(t, cli.List(context.Background(), &sessions))
	assert.Len(t, sessions.Items, 1, "refusal must not create a session")
}

// ---------------------------------------------------------------------------
// Permission-request flow tests
// ---------------------------------------------------------------------------

// sessKeyForFixture returns the ObjectKey for the session created by
// newFakeK8sClientWithExistingSession.
func sessKeyForFixture() client.ObjectKey {
	return client.ObjectKey{Namespace: "default", Name: "c1-abc"}
}

// newFakeK8sClientWithExistingSession builds a fake Kubernetes client
// containing a Channel and an active (Running) AgentSession for key
// "thread:C1:1" whose started-by annotation is startedByExternalID.
// The session name is always "c1-abc" (accessible via sessKeyForFixture).
func newFakeK8sClientWithExistingSession(t *testing.T, startedByExternalID string) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	_ = spiceboxv1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	keyHash := sha256HexTest("thread:C1:1")
	ch := &spiceboxv1alpha1.Channel{
		ObjectMeta: metav1.ObjectMeta{Name: "c1", Namespace: "default"},
		Spec: spiceboxv1alpha1.ChannelSpec{
			Kind: "slack", AgentClass: "ac1",
			CredentialsRef: spiceboxv1alpha1.ChannelCredentialsRef{SecretName: "s"},
		},
	}
	annotations := map[string]string{
		spiceboxv1alpha1.AnnotationStartedByExternalID: startedByExternalID,
	}
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{
			Name: "c1-abc", Namespace: "default",
			Labels: map[string]string{
				spiceboxv1alpha1.LabelChannelName: "c1",
				spiceboxv1alpha1.LabelChannelKey:  keyHash,
			},
			Annotations: annotations,
		},
		Spec: spiceboxv1alpha1.AgentSessionSpec{
			Class: "ac1",
			InputChannel: &spiceboxv1alpha1.ChannelBinding{
				Name: "c1", Kind: "slack", Key: "thread:C1:1",
				NATSSubjectPrefix: "ap.session.default.c1-abc",
			},
		},
		Status: spiceboxv1alpha1.AgentSessionStatus{Phase: spiceboxv1alpha1.AgentSessionPhaseRunning},
	}
	return fake.NewClientBuilder().
		WithScheme(scheme).
		// A Valid AgentClass, for the same reason newPipeline seeds one: the
		// Channel binds to "ac1", and a Channel bound to a nonexistent agent is
		// now (correctly) an explained stall rather than a silent one.
		WithObjects(ch, sess, agentClass(t, metav1.ConditionTrue, spiceboxv1alpha1.ReasonAllReferencesResolve, "")).
		WithStatusSubresource(&spiceboxv1alpha1.AgentSession{}).
		Build()
}

// seedApprovalStatus persists sess's channelsd-owned approval surface (pending
// queues + approval conditions + interact fields) through the same WriteOwned
// path that production uses. It reads the current client state as `original`
// (the pre-mutation snapshot) and passes it alongside the mutated `sess` so
// WriteOwned can diff and send only what changed.
func seedApprovalStatus(t *testing.T, cli client.Client, sess *spiceboxv1alpha1.AgentSession) {
	t.Helper()
	var original spiceboxv1alpha1.AgentSession
	require.NoError(t, cli.Get(context.Background(), client.ObjectKeyFromObject(sess), &original),
		"seed: get original session state from client")
	require.NoError(t, applyApprovalStatus(context.Background(), cli, sess, &original),
		"seed approval status via WriteOwned")
}

// samPostsInThread returns a channelkinds.InboundEvent representing U_SAM
// posting in the thread keyed by "thread:C1:1" on channel "c1" (slack kind).
func samPostsInThread(t *testing.T, text string) channelkinds.InboundEvent {
	t.Helper()
	ch := &spiceboxv1alpha1.Channel{
		ObjectMeta: metav1.ObjectMeta{Name: "c1", Namespace: "default"},
		Spec: spiceboxv1alpha1.ChannelSpec{
			Kind: "slack", AgentClass: "ac1",
			CredentialsRef: spiceboxv1alpha1.ChannelCredentialsRef{SecretName: "s"},
		},
	}
	return channelkinds.InboundEvent{
		Channel:     ch,
		ExternalIDs: channelkinds.ExternalIdentity{Kind: "slack", ExternalID: "U_SAM", Email: "sam@example.com"},
		ChannelKey:  "thread:C1:1",
		MessageText: text,
	}
}

// newTestPipeline constructs a Pipeline wired with the given client, authz, and
// NATS recorder. Memory is a no-op fakeMemory.
func newTestPipeline(t *testing.T, cli client.Client, az *fakeAuthz, nats *fakeNATS) *Pipeline {
	t.Helper()
	mem := &fakeMemory{seeded: map[string][]MemTurn{}}
	return newTestPipelineWithMemory(t, cli, az, nats, mem)
}

// newTestPipelineWithMemory is newTestPipeline with a caller-supplied
// memory implementation, so tests that need to assert against
// memory.Append calls can pass their own recorder.
func newTestPipelineWithMemory(t *testing.T, cli client.Client, az *fakeAuthz, nats *fakeNATS, mem Memory) *Pipeline {
	t.Helper()
	caps := func(_ string) []string { return []string{"text", "markdown"} }
	p := NewPipeline(cli, az, mem, nats, caps)
	p.Engine = engine.New(engine.Deps{
		SessionInteractChecker: az,
		Granter:                az,
		Lookuper:               az,
		ApproverChecker:        az,
	})
	p.Now = func() time.Time { return time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC) }
	p.MarkerSigner = testMarkerSigner
	return p
}

// TestDeliver_DenyEmitsPermissionRequestOnce verifies that the first
// non-permitted post stamps PendingRequesters and publishes the envelope;
// a second post from the same requester is silent-dropped (a suppressed notice,
// no second envelope, no second patch).
func TestDeliver_DenyEmitsPermissionRequestOnce(t *testing.T) {
	cli := newFakeK8sClientWithExistingSession(t, "U_ALICE")
	natsRec := &fakeNATS{}
	az := &fakeAuthz{checkResult: false}
	p := newTestPipeline(t, cli, az, natsRec)

	ev1 := samPostsInThread(t, "hey what's up?")
	dec1, err := p.Deliver(context.Background(), ev1)
	require.NoError(t, err, "Deliver1")
	assert.Equal(t, channelkinds.OutcomeDeniedByPermission, dec1.Outcome, "dec1.Outcome")
	// the notice is suppressed on success: the interaction_request envelope was
	// published and the "interaction" sub-channel sender owns the
	// in-thread rejection.
	assert.True(t, dec1.Notice.IsSuppressed(),
		"first post suppresses its notice (the interaction sender owns the in-thread rejection)")
	require.Len(t, natsRec.subjects, 1, "expected 1 NATS publish")
	assert.True(t, strings.HasSuffix(natsRec.subjects[0], ".out."+string(channelevents.KindInteractionRequest)),
		"NATS subject = %q, want a %q suffix", natsRec.subjects[0], ".out."+string(channelevents.KindInteractionRequest))

	// Second post — same requester. Pipeline re-Gets the channel.
	ev2 := samPostsInThread(t, "still there?")
	dec2, err := p.Deliver(context.Background(), ev2)
	require.NoError(t, err, "Deliver2")
	assert.Equal(t, channelkinds.OutcomeDeniedByPermission, dec2.Outcome, "dec2.Outcome")
	assert.True(t, dec2.Notice.IsSuppressed(), "second post suppresses its notice (duplicate request)")
	assert.Len(t, natsRec.subjects, 1, "expected still 1 NATS publish after second post")
}

// TestDeliver_DenyPropagatesIdentityModeToEnvelope pins security-critical
// plumbing: when the joined session's AgentClass has
// identityMode=userPassthrough, the published interaction_request's Body MUST
// carry the elevated-risk warning (":warning:" lead — see
// permissionRequestBody) so the approver understands that approving grants the
// requester proxy use of the approver's connected accounts. A regression here
// shows the breezy "wants to talk to your session" DM for a session where
// approval hands over identity by proxy. The mode is baked into the rendered
// Body text at publish time, not carried as a structured field.
func TestDeliver_DenyPropagatesIdentityModeToEnvelope(t *testing.T) {
	cases := []struct {
		name         string
		identityMode string
		wantWarning  bool
	}{
		{
			name:         "userPassthrough: envelope Body carries the elevated-risk warning",
			identityMode: spiceboxv1alpha1.IdentityModeUserPassthrough,
			wantWarning:  true,
		},
		{
			name:         "default identity (empty): envelope Body has no warning",
			identityMode: "",
			wantWarning:  false,
		},
		{
			name:         "missing AgentClass: envelope falls back to no warning (safe default)",
			identityMode: "<no-ac>", // sentinel meaning "do not seed the AgentClass"
			wantWarning:  false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cli := newFakeK8sClientWithExistingSession(t, "U_ALICE")
			// Seed the AgentClass the session references (Spec.Class == "ac1"
			// per the fixture).
			if tc.identityMode != "<no-ac>" {
				ac := &spiceboxv1alpha1.AgentClass{
					ObjectMeta: metav1.ObjectMeta{Name: "ac1", Namespace: "default"},
					Spec:       spiceboxv1alpha1.AgentClassSpec{IdentityMode: tc.identityMode},
				}
				seedAgentClass(t, cli, ac)
			}

			natsRec := &fakeNATS{}
			az := &fakeAuthz{checkResult: false}
			p := newTestPipeline(t, cli, az, natsRec)

			ev := samPostsInThread(t, "can I join?")
			_, err := p.Deliver(context.Background(), ev)
			require.NoError(t, err, "Deliver")
			require.Len(t, natsRec.subjects, 1, "exactly one envelope published")

			env := findEnvelopeBySubjectSuffix(t, natsRec, ".out."+string(channelevents.KindInteractionRequest))
			var pl channelevents.InteractionRequestPayload
			require.NoError(t, json.Unmarshal(env.Payload, &pl))
			if tc.wantWarning {
				assert.Contains(t, pl.Body, ":warning:",
					"userPassthrough must propagate from AgentClass.Spec.IdentityMode through to the interaction_request Body's elevated-risk warning")
			} else {
				assert.NotContains(t, pl.Body, ":warning:",
					"non-userPassthrough sessions must NOT show the elevated-risk warning")
			}
		})
	}
}

// TestDeliver_DenyPropagatesLinkedServicesUnderUserPassthrough: under
// userPassthrough, the published interaction_request's Body renders the
// resolved list of providers the session's started-by user has linked
// (restricted to credentials the joining AgentClass actually uses) as a bullet
// list — see permissionRequestBody. The list is baked into the rendered Body
// text at publish time, not carried as a structured field.
//
// Each subcase pins a different failure mode → degradation rule:
// missing UserIdentity, missing started-by annotation, lookup miss
// — all resolve an empty list and fall back to the generic warning
// phrasing (no bullets).
func TestDeliver_DenyPropagatesLinkedServicesUnderUserPassthrough(t *testing.T) {
	cases := []struct {
		name        string
		setupExtras func(*testing.T, client.Client, string)
		wantLinked  []string
		// wantAbsent lists labels/credential names that must NOT appear as a
		// bullet in the rendered Body — the ghost-leak guard. Populated only
		// for the "filtered out" subcase below: a filtering regression that
		// let a credential with no backing MCPServer leak into the approver
		// warning (e.g. falling back to the raw credential name instead of
		// skipping it) would otherwise go uncaught, since the happy-path-only
		// Contains assertions never look for what should be missing.
		wantAbsent []string
	}{
		{
			name: "happy path: approver's linked services intersected with class",
			setupExtras: func(t *testing.T, cli client.Client, canonical string) {
				// Seed an AgentClass that references one MCPServer
				// (matching the fixture's class name "ac1").
				ac := &spiceboxv1alpha1.AgentClass{
					ObjectMeta: metav1.ObjectMeta{Name: "ac1", Namespace: "default"},
					Spec: spiceboxv1alpha1.AgentClassSpec{
						IdentityMode: spiceboxv1alpha1.IdentityModeUserPassthrough,
					},
				}
				seedAgentClass(t, cli, ac)
				// Seed the approver's UserIdentity with two linked
				// credentials. The class-credentials filter is nil here
				// (no remap entries) → ResolveLinkedServiceLabels
				// surfaces both.
				ui := &spiceboxv1alpha1.UserIdentity{
					ObjectMeta: metav1.ObjectMeta{Name: useridentity.NameForSubject(identity.Subject(canonical))},
					Spec: spiceboxv1alpha1.UserIdentitySpec{
						Subject: canonical,
						Credentials: []spiceboxv1alpha1.AgentCredential{
							{Name: "linear-oauth", Type: "oauth"},
							{Name: "github-token", Type: "static"},
						},
					},
				}
				require.NoError(t, cli.Create(context.Background(), ui))
				// MCPServers — provider labels are what appear in the DM.
				lin := &spiceboxv1alpha1.MCPServer{
					ObjectMeta: metav1.ObjectMeta{Name: "linear-mcp", Namespace: "default"},
					Spec: spiceboxv1alpha1.MCPServerSpec{
						Auth: spiceboxv1alpha1.MCPServerAuth{
							Type: "oauth", Credential: "linear-oauth", Provider: "Linear",
						},
					},
				}
				gh := &spiceboxv1alpha1.MCPServer{
					ObjectMeta: metav1.ObjectMeta{Name: "github-mcp", Namespace: "default"},
					Spec: spiceboxv1alpha1.MCPServerSpec{
						Auth: spiceboxv1alpha1.MCPServerAuth{
							Type: "static", Credential: "github-token", Provider: "GitHub",
						},
					},
				}
				require.NoError(t, cli.Create(context.Background(), lin))
				require.NoError(t, cli.Create(context.Background(), gh))
			},
			wantLinked: []string{"GitHub", "Linear"}, // alphabetized
		},
		{
			name: "no UserIdentity: empty list → generic warning fallback",
			setupExtras: func(t *testing.T, cli client.Client, canonical string) {
				// Seed only the AgentClass (mode = passthrough) so the
				// mode is set, but no UserIdentity → no linked services
				// can be resolved.
				ac := &spiceboxv1alpha1.AgentClass{
					ObjectMeta: metav1.ObjectMeta{Name: "ac1", Namespace: "default"},
					Spec:       spiceboxv1alpha1.AgentClassSpec{IdentityMode: spiceboxv1alpha1.IdentityModeUserPassthrough},
				}
				seedAgentClass(t, cli, ac)
			},
			wantLinked: nil,
		},
		{
			name: "credential with no backing MCPServer: filtered out",
			setupExtras: func(t *testing.T, cli client.Client, canonical string) {
				ac := &spiceboxv1alpha1.AgentClass{
					ObjectMeta: metav1.ObjectMeta{Name: "ac1", Namespace: "default"},
					Spec:       spiceboxv1alpha1.AgentClassSpec{IdentityMode: spiceboxv1alpha1.IdentityModeUserPassthrough},
				}
				seedAgentClass(t, cli, ac)
				ui := &spiceboxv1alpha1.UserIdentity{
					ObjectMeta: metav1.ObjectMeta{Name: useridentity.NameForSubject(identity.Subject(canonical))},
					Spec: spiceboxv1alpha1.UserIdentitySpec{
						Subject: canonical,
						Credentials: []spiceboxv1alpha1.AgentCredential{
							{Name: "linear-oauth", Type: "oauth"},
							{Name: "ghost", Type: "static"},
						},
					},
				}
				require.NoError(t, cli.Create(context.Background(), ui))
				// Only linear has an MCPServer → ghost is filtered.
				lin := &spiceboxv1alpha1.MCPServer{
					ObjectMeta: metav1.ObjectMeta{Name: "linear-mcp", Namespace: "default"},
					Spec: spiceboxv1alpha1.MCPServerSpec{
						Auth: spiceboxv1alpha1.MCPServerAuth{
							Type: "oauth", Credential: "linear-oauth", Provider: "Linear",
						},
					},
				}
				require.NoError(t, cli.Create(context.Background(), lin))
			},
			wantLinked: []string{"Linear"},
			// "ghost" has no backing MCPServer and must be filtered out by
			// ResolveLinkedServiceLabels — it must never reach the approver's
			// warning Body as a bullet.
			wantAbsent: []string{"ghost"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cli := newFakeK8sClientWithExistingSession(t, "U_ALICE")
			// The existing session fixture stamps the external ID but
			// not the canonical — set it on the AgentSession so the
			// pipeline can resolve the approver's UserIdentity.
			var sess spiceboxv1alpha1.AgentSession
			require.NoError(t, cli.Get(context.Background(),
				client.ObjectKey{Namespace: "default", Name: "c1-abc"}, &sess))
			canonical := canonicalID(channelkinds.ExternalIdentity{
				Kind: "slack", ExternalID: "U_ALICE",
			})
			if sess.Annotations == nil {
				sess.Annotations = map[string]string{}
			}
			sess.Annotations[spiceboxv1alpha1.AnnotationStartedByCanonicalID] = canonical.String()
			require.NoError(t, cli.Update(context.Background(), &sess))

			tc.setupExtras(t, cli, canonical.String())

			natsRec := &fakeNATS{}
			az := &fakeAuthz{checkResult: false}
			p := newTestPipeline(t, cli, az, natsRec)
			ev := samPostsInThread(t, "can I join?")
			_, err := p.Deliver(context.Background(), ev)
			require.NoError(t, err)
			require.Len(t, natsRec.subjects, 1)

			env := findEnvelopeBySubjectSuffix(t, natsRec, ".out."+string(channelevents.KindInteractionRequest))
			var pl channelevents.InteractionRequestPayload
			require.NoError(t, json.Unmarshal(env.Payload, &pl))

			assert.Contains(t, pl.Body, ":warning:", "sanity: userPassthrough warning should propagate")
			if len(tc.wantLinked) == 0 {
				assert.Contains(t, pl.Body, "YOUR connected accounts",
					"empty linked-services list falls back to the generic warning phrasing")
				assert.NotContains(t, pl.Body, "• ", "no bullets when no linked services were resolved")
			} else {
				for _, label := range tc.wantLinked {
					assert.Contains(t, pl.Body, "• "+label,
						"LinkedServices is the security-critical bullet-list source — regression here silently weakens the warning UI (missing %q)", label)
				}
			}
			for _, ghostLabel := range tc.wantAbsent {
				assert.NotContains(t, pl.Body, "• "+ghostLabel,
					"filtered-out credential %q must never leak into the approver's warning Body as a bullet — a filtering regression here would expose a credential with no backing MCPServer", ghostLabel)
			}
		})
	}
}

// TestDeliver_DenyReturnsInternalErrorWhenStatusPatchPrunes verifies
// the verify-readback in handlePermissionDeny: kube-apiserver silently
// prunes status fields the CRD's OpenAPI schema doesn't declare, and
// PATCH returns no error in that case. The pipeline must NOT trust a
// no-error patch — it must re-read and confirm. This bug shipped to
// production once: a stale CRD (missing status.pendingRequesters)
// silently dropped every patch, breaking the entire approve flow.
// Simulated here by a Kubernetes client that swallows the slice on
// status PATCH.
func TestDeliver_DenyReturnsInternalErrorWhenStatusPatchPrunes(t *testing.T) {
	ch := newChannel("c1")
	existing := existingSession(t, "c1-abc", "U_original", spiceboxv1alpha1.AgentSessionPhaseIdle)
	existing.Spec.InputChannel = nil
	p, az, _, _, baseCli := newPipeline(t, ch, existing)
	az.checkResult = false
	// Wrap the fake client: status patch returns nil, but a follow-up
	// Get returns the session WITHOUT the just-written PendingRequesters
	// — exactly what kube-apiserver does on stale CRD pruning.
	p.K8s = pruningStatusClient{Client: baseCli}

	dec, err := p.Deliver(context.Background(), channelkinds.InboundEvent{
		Channel:     ch,
		ExternalIDs: channelkinds.ExternalIdentity{Kind: "slack", ExternalID: "U_coworker", Email: "coworker@example.com"},
		ChannelKey:  "thread:C1:1",
		MessageText: "needs approval",
	})
	require.Error(t, err,
		"Deliver returned nil error; want pruned-CRD error (status patch silently dropped pendingRequesters)")
	assert.Contains(t, err.Error(), "pendingRequesters did not persist",
		"err should mention pendingRequesters did not persist")
	assert.Equal(t, channelkinds.OutcomeInternalError, dec.Outcome, "Outcome")
}

// pruningStatusClient wraps a controller-runtime client and simulates
// kube-apiserver silently pruning status fields on PATCH (which is
// what stale CRD schemas do). Status().Patch returns nil but the
// pruned field never lands in the cached object Get returns.
type pruningStatusClient struct {
	client.Client
}

func (c pruningStatusClient) Status() client.SubResourceWriter {
	return pruningStatusWriter{}
}

type pruningStatusWriter struct{}

func (pruningStatusWriter) Create(_ context.Context, _ client.Object, _ client.Object, _ ...client.SubResourceCreateOption) error {
	return nil
}
func (pruningStatusWriter) Update(_ context.Context, _ client.Object, _ ...client.SubResourceUpdateOption) error {
	return nil
}
func (pruningStatusWriter) Patch(_ context.Context, _ client.Object, _ client.Patch, _ ...client.SubResourcePatchOption) error {
	// "Success" — but the field was pruned. The next Get returns the
	// session without the patched-in PendingRequesters.
	return nil
}
func (pruningStatusWriter) Apply(_ context.Context, _ runtime.ApplyConfiguration, _ ...client.SubResourceApplyOption) error {
	return nil
}

// TestDeliver_DenyDoesNotDuplicateInThreadRejection locks the contract
// that prevents the duplicate "permission has been requested" the user
// observed: when the permission_request envelope is published, the notice
// MUST be empty so the listener silent-drops on its end and the
// permission_request sub-channel sender is the sole source of the
// in-thread rejection. A regression here re-introduces the double-post.
func TestDeliver_DenyDoesNotDuplicateInThreadRejection(t *testing.T) {
	cli := newFakeK8sClientWithExistingSession(t, "U_ALICE")
	natsRec := &fakeNATS{}
	az := &fakeAuthz{checkResult: false}
	p := newTestPipeline(t, cli, az, natsRec)

	dec, err := p.Deliver(context.Background(), samPostsInThread(t, "tell me a joke"))
	require.NoError(t, err, "Deliver")
	require.Equal(t, channelkinds.OutcomeDeniedByPermission, dec.Outcome)
	// Exactly one of the two in-thread-rejection sources must fire:
	//   - listener posts dec.Notice
	//   - the "interaction" sub-channel sender posts on the published
	//     interaction_request envelope
	// Both = duplicate. Neither = silent denial. The contract is:
	// publish-success → a suppressed notice, NATS publish.
	pubCount := 0
	interactionRequestSuffix := ".out." + string(channelevents.KindInteractionRequest)
	for _, s := range natsRec.subjects {
		if strings.HasSuffix(s, interactionRequestSuffix) {
			pubCount++
		}
	}
	if pubCount == 1 {
		assert.True(t, dec.Notice.IsSuppressed(),
			"interaction_request was published AND a notice was set; both would render in-thread (duplicate)")
	}
	if pubCount == 0 {
		assert.False(t, dec.Notice.IsSuppressed(),
			"no interaction_request published AND a suppressed notice → the user gets no signal at all")
	}
}

// TestDeliver_DenyStampsPendingRequestersAndCondition verifies that
// status.PendingRequesters and the PermissionRequestPending condition
// are written on the first deny.
func TestDeliver_DenyStampsPendingRequestersAndCondition(t *testing.T) {
	cli := newFakeK8sClientWithExistingSession(t, "U_ALICE")
	natsRec := &fakeNATS{}
	az := &fakeAuthz{checkResult: false}
	p := newTestPipeline(t, cli, az, natsRec)

	_, _ = p.Deliver(context.Background(), samPostsInThread(t, "hello"))

	var got spiceboxv1alpha1.AgentSession
	require.NoError(t, cli.Get(context.Background(), sessKeyForFixture(), &got), "Get session")
	require.Len(t, got.Status.PendingRequesters, 1, "PendingRequesters")
	pr := got.Status.PendingRequesters[0]
	assert.Equal(t, "slack", pr.Kind, "PendingRequester[0].Kind")
	assert.Equal(t, "U_SAM", pr.ExternalID, "PendingRequester[0].ExternalID")

	cond := conditions.Find(got.Status.Conditions, spiceboxv1alpha1.AgentSessionConditionPermissionRequestPending)
	if assert.NotNil(t, cond, "PermissionRequestPending condition") {
		assert.Equal(t, metav1.ConditionTrue, cond.Status, "expected PermissionRequestPending=True")
	}
}

// TestDeliverResolvesCronSessionByOutputChannelKeyLabel asserts that when
// an inbound slack reply's ChannelKey matches an existing session's
// LabelOutputChannelKey (not LabelChannelKey), the pipeline still
// resolves the session and routes the message to it. This is the
// wakeable-cron-spawned-thread case: bento spawned the session with
// InputChannel=bento (its LabelChannelKey hashes the cron key); after
// the first slack send the outbound relay patches OutputChannel.Key
// and LabelOutputChannelKey to the thread anchor.
func TestDeliverResolvesCronSessionByOutputChannelKeyLabel(t *testing.T) {
	ch := newChannel("slack-ch")
	cronInputKey := "cron:bento-in:42"
	threadKey := "thread:C0123:1234.5678"
	cronInputHash := sha256HexTest(cronInputKey)
	threadHash := sha256HexTest(threadKey)

	existing := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cron-session",
			Namespace: "default",
			Labels: map[string]string{
				// Cron-spawned sessions stamp LabelChannelName from the bento
				// input Channel (the one that spawned the session), not the
				// slack output Channel that delivers replies. The fallback
				// lookup uses LabelOutputChannelKey, not LabelChannelName.
				spiceboxv1alpha1.LabelChannelName: "bento-in",
				spiceboxv1alpha1.LabelChannelKey:  cronInputHash,
				// Output-side: hashes the slack thread anchor, patched
				// by the outbound relay after the first slack post.
				spiceboxv1alpha1.LabelOutputChannelKey: threadHash,
			},
			Annotations: map[string]string{
				spiceboxv1alpha1.AnnotationStartedByExternalID: "U1",
			},
		},
		Spec: spiceboxv1alpha1.AgentSessionSpec{
			Class: "ac1",
			InputChannel: &spiceboxv1alpha1.ChannelBinding{
				Name: "bento-in", Kind: "bento", Key: cronInputKey,
				NATSSubjectPrefix: "ap.session.default.cron-session",
			},
			OutputChannel: &spiceboxv1alpha1.ChannelBinding{
				Name: "slack-ch", Kind: "slack",
				Key: threadKey,
				External: map[string]string{
					"channel_id": "C0123",
					"thread_ts":  "1234.5678",
				},
			},
		},
		Status: spiceboxv1alpha1.AgentSessionStatus{
			Phase: spiceboxv1alpha1.AgentSessionPhaseRunning,
		},
	}
	p, _, mem, nats, _ := newPipeline(t, ch, existing)

	dec, err := p.Deliver(context.Background(), channelkinds.InboundEvent{
		Channel:     ch,
		ExternalIDs: channelkinds.ExternalIdentity{Kind: "slack", ExternalID: "U1", Email: "a@example.com"},
		ChannelKey:  threadKey, // matches LabelOutputChannelKey, NOT LabelChannelKey
		MessageText: "thanks!",
	})
	require.NoError(t, err, "Deliver")
	assert.Equal(t, channelkinds.OutcomeRouted, dec.Outcome,
		"cron-spawned session should resolve via LabelOutputChannelKey")
	assert.Equal(t, "cron-session", dec.Session.Name, "Session.Name")
	// Routed inbound on an active session appends a user turn to memory.
	require.Len(t, mem.appends, 1, "memory appends")
	assert.Equal(t, "cron-session", mem.appends[0].name, "memory append name")
	assert.Equal(t, "user", mem.appends[0].turn.Role, "memory append role")
	// Wakeup must be published so the (potentially Idle) runner picks
	// up the reply, plus a mid-turn enqueue ack since this session is Running.
	assert.Len(t, nats.subjects, 2, "NATS publishes (wakeup + enqueue ack)")
}

func TestDeliverActiveSession_RoutesToLiveInteractiveToolCall(t *testing.T) {
	ch := newChannel("c1")
	existing := existingSession(t, "c1-abc", "", spiceboxv1alpha1.AgentSessionPhaseRunning)
	// A live interactive ToolCall labeled with agentsession=<sessName>,
	// Running=True, no terminal condition.
	tc := &spiceboxv1alpha1.ToolCall{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "c1-abc-1-tu1",
			Namespace: "default",
			Labels:    map[string]string{"agentsession": existing.Name},
		},
		Spec: spiceboxv1alpha1.ToolCallSpec{
			Session: "c1-abc-code",
			Tool:    "claude",
			Mode:    spiceboxv1alpha1.ToolCallModeInteractive,
		},
		Status: spiceboxv1alpha1.ToolCallStatus{
			Conditions: []metav1.Condition{{
				Type:   spiceboxv1alpha1.ToolCallConditionRunning,
				Status: metav1.ConditionTrue,
				Reason: spiceboxv1alpha1.ReasonExecStarted,
			}},
		},
	}
	p, _, mem, nats, _ := newPipeline(t, ch, existing, tc)

	dec, err := p.Deliver(context.Background(), channelkinds.InboundEvent{
		Channel:     ch,
		ExternalIDs: channelkinds.ExternalIdentity{Kind: "slack", ExternalID: "U1", Email: "a@example.com"},
		ChannelKey:  "thread:C1:1",
		MessageText: "fix the failing test please",
	})
	require.NoError(t, err)
	assert.Equal(t, channelkinds.OutcomeRouted, dec.Outcome)
	assert.Empty(t, mem.appends, "liveness-routed inbound must NOT append agent memory")

	require.Len(t, nats.subjects, 1, "exactly one publish")
	assert.Contains(t, nats.subjects[0], ".in."+string(channelevents.KindToolSessionInput),
		"should publish KindToolSessionInput, got %s", nats.subjects[0])

	var env channelevents.Envelope
	require.NoError(t, json.Unmarshal(nats.payloads[0], &env))
	var pl channelevents.ToolSessionInputPayload
	require.NoError(t, json.Unmarshal(env.Payload, &pl))
	assert.Equal(t, "c1-abc-1-tu1", pl.ToolCallRef)
	assert.Equal(t, "fix the failing test please\n", string(pl.Data))
}

// TestDeliverActiveSession_LiveToolRouting_AttachmentsNotCarried_UserToldAndLogged
// covers live interactive-tool routing: only ev.MessageText is forwarded
// into the live tool's stdin (toolPl.Data below), so an attachment on the
// same message has nowhere to go. This decision reports OutcomeRouted, which
// no channel kind's Deliver caller reads Notice from, so the assertion is on
// the envelope actually published to NATS, not on decision state, which
// would pass even if delivery were broken.
func TestDeliverActiveSession_LiveToolRouting_AttachmentsNotCarried_UserToldAndLogged(t *testing.T) {
	ch := newChannel("c1")
	existing := existingSession(t, "c1-abc-attach", "", spiceboxv1alpha1.AgentSessionPhaseRunning)
	tc := &spiceboxv1alpha1.ToolCall{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "c1-abc-attach-1-tu1",
			Namespace: "default",
			Labels:    map[string]string{"agentsession": existing.Name},
		},
		Spec: spiceboxv1alpha1.ToolCallSpec{
			Session: "c1-abc-attach-code",
			Tool:    "claude",
			Mode:    spiceboxv1alpha1.ToolCallModeInteractive,
		},
		Status: spiceboxv1alpha1.ToolCallStatus{
			Conditions: []metav1.Condition{{
				Type:   spiceboxv1alpha1.ToolCallConditionRunning,
				Status: metav1.ConditionTrue,
				Reason: spiceboxv1alpha1.ReasonExecStarted,
			}},
		},
	}
	p, _, mem, nats, _ := newPipeline(t, ch, existing, tc)

	dec, err := p.Deliver(context.Background(), channelkinds.InboundEvent{
		Channel:     ch,
		ExternalIDs: channelkinds.ExternalIdentity{Kind: "slack", ExternalID: "U1", Email: "a@example.com"},
		ChannelKey:  "thread:C1:1",
		MessageText: "fix the failing test please",
		Attachments: oneAttachment("F1", "trace.txt", "text/plain", 512),
	})
	require.NoError(t, err)
	assert.Equal(t, channelkinds.OutcomeRouted, dec.Outcome)
	assert.Empty(t, mem.appends, "liveness-routed inbound must NOT append agent memory")
	assert.Nil(t, dec.Notice, "OutcomeRouted's Notice is never read by any channel kind — the notice is published directly instead")

	// The tool-stdin publish is unaffected — attachments still have nowhere
	// to go on that stream.
	toolEnv := findEnvelopeBySubjectSuffix(t, nats, ".in."+string(channelevents.KindToolSessionInput))
	var toolPl channelevents.ToolSessionInputPayload
	require.NoError(t, json.Unmarshal(toolEnv.Payload, &toolPl), "unmarshal ToolSessionInputPayload")
	assert.Equal(t, "c1-abc-attach-1-tu1", toolPl.ToolCallRef)
	assert.Equal(t, "fix the failing test please\n", string(toolPl.Data))

	// The dropped attachment is told to the user via a directly-published
	// notice, independent of the tool-stdin publish above.
	noticeEnv := findEnvelopeBySubjectSuffix(t, nats, ".out.interaction_request")
	require.Equal(t, channelevents.KindInteractionRequest, noticeEnv.Kind, "envelope kind")
	var pl channelevents.InteractionRequestPayload
	require.NoError(t, json.Unmarshal(noticeEnv.Payload, &pl), "unmarshal InteractionRequestPayload")
	assert.Equal(t, categories.AttachmentReadFailed, pl.Category, "Category")
	assert.Contains(t, pl.Body, "doesn't carry file attachments", "must tell the user the file did not come along")
}

func TestDeliverActiveSession_TerminalInteractiveToolCall_NormalWakeup(t *testing.T) {
	ch := newChannel("c1")
	existing := existingSession(t, "c1-abc", "", spiceboxv1alpha1.AgentSessionPhaseRunning)
	// Terminal interactive ToolCall — should NOT trigger liveness routing.
	tc := &spiceboxv1alpha1.ToolCall{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "c1-abc-1-tu1",
			Namespace: "default",
			Labels:    map[string]string{"agentsession": existing.Name},
		},
		Spec: spiceboxv1alpha1.ToolCallSpec{Mode: spiceboxv1alpha1.ToolCallModeInteractive},
		Status: spiceboxv1alpha1.ToolCallStatus{
			Conditions: []metav1.Condition{
				{Type: spiceboxv1alpha1.ToolCallConditionRunning, Status: metav1.ConditionFalse},
				{Type: spiceboxv1alpha1.ToolCallConditionSucceeded, Status: metav1.ConditionTrue},
			},
		},
	}
	p, _, mem, nats, _ := newPipeline(t, ch, existing, tc)

	dec, err := p.Deliver(context.Background(), channelkinds.InboundEvent{
		Channel:     ch,
		ExternalIDs: channelkinds.ExternalIdentity{Kind: "slack", ExternalID: "U1", Email: "a@example.com"},
		ChannelKey:  "thread:C1:1",
		MessageText: "follow-up",
	})
	require.NoError(t, err)
	assert.Equal(t, channelkinds.OutcomeRouted, dec.Outcome)
	require.Len(t, mem.appends, 1, "no live interactive → normal memory append + wakeup")
	require.Len(t, nats.subjects, 2, "normal wakeup + mid-turn enqueue ack published (session is Running)")
	assert.Contains(t, nats.subjects[0], ".in."+string(channelevents.KindUserMessage),
		"normal wakeup subject, got %s", nats.subjects[0])
	assert.Contains(t, nats.subjects[1], ".out."+string(channelevents.KindEnqueueAck),
		"mid-turn ack subject, got %s", nats.subjects[1])
}

// TestDeliverActiveSession_RunningPhase_EmitsEnqueueAck verifies that an
// inbound landing on a genuinely mid-turn (Running) session publishes a
// KindEnqueueAck on the OUT subject alongside the normal memory append +
// wakeup, with a freshly-minted RequestID and the requester populated from
// the inbound sender's channel-native identity (so the channel kind's
// queued_messages sender has a recipient for the proactive ephemeral ack).
func TestDeliverActiveSession_RunningPhase_EmitsEnqueueAck(t *testing.T) {
	ch := newChannel("c1")
	existing := existingSession(t, "c1-abc", "", spiceboxv1alpha1.AgentSessionPhaseRunning)
	p, _, mem, nats, _ := newPipeline(t, ch, existing)

	dec, err := p.Deliver(context.Background(), channelkinds.InboundEvent{
		Channel:     ch,
		ExternalIDs: channelkinds.ExternalIdentity{Kind: "slack", ExternalID: "U1", Email: "a@example.com"},
		ChannelKey:  "thread:C1:1",
		MessageText: "are you still there?",
	})
	require.NoError(t, err, "Deliver")
	assert.Equal(t, channelkinds.OutcomeRouted, dec.Outcome)
	require.Len(t, mem.appends, 1, "memory appends")

	var ackIdx = -1
	for i, s := range nats.subjects {
		if strings.Contains(s, ".out."+string(channelevents.KindEnqueueAck)) {
			ackIdx = i
			break
		}
	}
	require.GreaterOrEqual(t, ackIdx, 0,
		"a KindEnqueueAck must be published for a mid-turn (Running) session, got subjects %v", nats.subjects)

	var env channelevents.Envelope
	require.NoError(t, json.Unmarshal(nats.payloads[ackIdx], &env))
	var pl channelevents.EnqueueAckPayload
	require.NoError(t, json.Unmarshal(env.Payload, &pl))
	assert.Len(t, pl.RequestID, 32, "RequestID is 32-hex (16 random bytes)")
	assert.Equal(t, "slack", pl.Requester.Kind.String(), "requester kind — the message sender, not the session's original starter")
	assert.Equal(t, "U1", pl.Requester.ExternalID.String(), "requester external id — so the sender can be the ephemeral ack recipient")
	assert.Equal(t, "a@example.com", pl.Requester.Email.String(), "requester email")
	assert.Equal(t, "default/c1-abc", pl.SessionRef, "sessionRef")
}

// queuedAckSubjects returns the subjects on which a mid-turn "your message is
// queued" ack was published, in EITHER of the two shapes Deliver's gate feeds:
// the legacy KindEnqueueAck, and the Slack-only
// interaction_request(queued_messages). Both are the same ack — which one goes
// out is chosen below the gate, by the session's channel kind — so a test that
// looked for only one of them would be blind to the other arm.
func queuedAckSubjects(t *testing.T, rec *fakeNATS) []string {
	t.Helper()
	var out []string
	for i, subj := range rec.subjects {
		if strings.HasSuffix(subj, ".out."+string(channelevents.KindEnqueueAck)) {
			out = append(out, subj)
			continue
		}
		if !strings.HasSuffix(subj, ".out."+string(channelevents.KindInteractionRequest)) {
			continue
		}
		var env channelevents.Envelope
		require.NoError(t, json.Unmarshal(rec.payloads[i], &env), "unmarshal envelope on %s", subj)
		var pl channelevents.InteractionRequestPayload
		require.NoError(t, json.Unmarshal(env.Payload, &pl), "unmarshal InteractionRequestPayload on %s", subj)
		if pl.Category == categories.QueuedMessages {
			out = append(out, subj)
		}
	}
	return out
}

// TestRunningPhase_EnqueueAckOnlyWhenThereIsARequesterToAddress pins the second
// term of Deliver's mid-turn ack gate — the one that skips the ack for a
// non-human acting subject — against the two shapes an inbound into a Running
// session can have.
//
// The ack is addressed to a REQUESTER: both of its payloads carry one, and the
// Slack shape fails its own Validate without one. A session-to-session inbound
// supplies none, by construction — Deliver's no-user-identity branch is
// reached precisely because ev.ExternalIDs is empty — so the ack could only be
// an unaddressable card or a log line, once per message. The message itself
// still queues; only the announcement is skipped.
//
// The path is shared by every channel kind, which is why both rows are here:
// the gate reads plausibly under either one alone, and the human row is the
// control that keeps the session row from passing for a fixture that never
// reached the gate at all.
func TestRunningPhase_EnqueueAckOnlyWhenThereIsARequesterToAddress(t *testing.T) {
	t.Run("session-to-session inbound into a Running target: queued, but no ack of either shape", func(t *testing.T) {
		ch, child := delegationPair(t, "parent-1", "child-1")
		rootCh := newSlackChannel("root-slack", "")
		root := boundSession(t, "parent-1", "slack", "root-slack", "thread:C1:1")
		p, az, mem, nats, _ := newPipeline(t, ch, child, rootCh, root)
		seedDelegation(t, az, "default/parent-1", "default/child-1")

		env := agentMessageSendEnvelope(t, "default", "parent-1", "default", "child-1", "use the staging cluster")
		require.NoError(t, p.HandleAgentMessageSend(context.Background(), env), "HandleAgentMessageSend")
		// Without this the row proves nothing: an inbound that never reached
		// the active-session slot publishes no ack for an unrelated reason.
		require.Len(t, mem.appends, 1, "the message must have been delivered into the Running child")

		assert.Empty(t, queuedAckSubjects(t, nats),
			"a session-to-session inbound has no requester to address the ack to, got subjects %v", nats.subjects)
	})

	t.Run("human inbound into a Running session: the ack is still published", func(t *testing.T) {
		ch := newChannel("c1")
		existing := existingSession(t, "c1-abc", "", spiceboxv1alpha1.AgentSessionPhaseRunning)
		p, _, mem, nats, _ := newPipeline(t, ch, existing)

		dec, err := p.Deliver(context.Background(), channelkinds.InboundEvent{
			Channel:     ch,
			ExternalIDs: channelkinds.ExternalIdentity{Kind: "slack", ExternalID: "U1", Email: "a@example.com"},
			ChannelKey:  "thread:C1:1",
			MessageText: "are you still there?",
		})
		require.NoError(t, err, "Deliver")
		assert.Equal(t, channelkinds.OutcomeRouted, dec.Outcome)
		require.Len(t, mem.appends, 1, "the message must have been delivered into the Running session")

		assert.Len(t, queuedAckSubjects(t, nats), 1,
			"a human who messaged mid-turn is told their message queued, got subjects %v", nats.subjects)
	})
}

// TestDeliverActiveSession_IdlePhase_NoEnqueueAck verifies that a parked
// (Idle) session — respawnOnWake==true, no in-flight turn — gets only the
// normal wake/annotation flow, never a mid-turn enqueue ack: there is
// nothing in flight to queue behind.
func TestDeliverActiveSession_IdlePhase_NoEnqueueAck(t *testing.T) {
	ch := newChannel("c1")
	existing := existingSession(t, "c1-abc", "", spiceboxv1alpha1.AgentSessionPhaseIdle)
	p, _, mem, nats, _ := newPipeline(t, ch, existing)

	dec, err := p.Deliver(context.Background(), channelkinds.InboundEvent{
		Channel:     ch,
		ExternalIDs: channelkinds.ExternalIdentity{Kind: "slack", ExternalID: "U1", Email: "a@example.com"},
		ChannelKey:  "thread:C1:1",
		MessageText: "wake up please",
	})
	require.NoError(t, err, "Deliver")
	assert.Equal(t, channelkinds.OutcomeRouted, dec.Outcome)
	require.Len(t, mem.appends, 1, "memory appends")

	for _, s := range nats.subjects {
		assert.NotContains(t, s, ".out."+string(channelevents.KindEnqueueAck),
			"a parked (Idle) session must NOT get an enqueue_ack, got subjects %v", nats.subjects)
	}
}

// TestDeliverActiveSession_RunningPhase_BrowserKind_StillEmitsLegacyEnqueueAck:
// a non-Slack (browser) session's mid-turn enqueue-ack must stay
// KindEnqueueAck, never an interaction_request. queued_messages is deliberately
// Slack-scoped, so this pins that Deliver's branch really selects on
// active.Spec.InputChannel.Kind rather than applying to every kind at once.
func TestDeliverActiveSession_RunningPhase_BrowserKind_StillEmitsLegacyEnqueueAck(t *testing.T) {
	ch := &spiceboxv1alpha1.Channel{
		ObjectMeta: metav1.ObjectMeta{Name: "c1", Namespace: "default"},
		Spec: spiceboxv1alpha1.ChannelSpec{
			Kind: browser.KindName, AgentClass: "ac1",
			CredentialsRef: spiceboxv1alpha1.ChannelCredentialsRef{SecretName: "s"},
		},
	}
	keyHash := sha256HexTest("thread:C1:1")
	existing := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{
			Name: "c1-abc", Namespace: "default",
			Labels: map[string]string{
				spiceboxv1alpha1.LabelChannelName: "c1",
				spiceboxv1alpha1.LabelChannelKey:  keyHash,
			},
		},
		Spec: spiceboxv1alpha1.AgentSessionSpec{
			Class: "ac1",
			InputChannel: &spiceboxv1alpha1.ChannelBinding{
				Name: "c1", Kind: browser.KindName, Key: "thread:C1:1",
				NATSSubjectPrefix: "ap.session.default.c1-abc",
			},
		},
		Status: spiceboxv1alpha1.AgentSessionStatus{Phase: spiceboxv1alpha1.AgentSessionPhaseRunning},
	}
	p, _, mem, nats, _ := newPipeline(t, ch, existing)

	dec, err := p.Deliver(context.Background(), channelkinds.InboundEvent{
		Channel:     ch,
		ExternalIDs: channelkinds.ExternalIdentity{Kind: identity.Kind(browser.KindName), ExternalID: "U1"},
		ChannelKey:  "thread:C1:1",
		MessageText: "are you still there?",
	})
	require.NoError(t, err, "Deliver")
	assert.Equal(t, channelkinds.OutcomeRouted, dec.Outcome)
	require.Len(t, mem.appends, 1, "memory appends")

	var sawAck, sawInteraction bool
	for _, s := range nats.subjects {
		if strings.Contains(s, ".out."+string(channelevents.KindEnqueueAck)) {
			sawAck = true
		}
		if strings.Contains(s, ".out."+string(channelevents.KindInteractionRequest)) {
			sawInteraction = true
		}
	}
	assert.True(t, sawAck, "a non-Slack (browser) session must still publish the legacy KindEnqueueAck, got subjects %v", nats.subjects)
	assert.False(t, sawInteraction, "a non-Slack (browser) session must NOT publish an interaction_request, got subjects %v", nats.subjects)
}

// TestDeliverActiveSession_RunningPhase_SlackKind_EmitsQueuedMessagesInteractionRequest:
// a Slack-kind session on the Running-phase inbound branch must publish
// interaction_request(queued_messages), NOT KindEnqueueAck — addressed to the
// inbound sender (AudienceRequester), carrying the ack's exact Lead text and a
// single ActionKindDecision "interrupt" action, correlated by a
// mintRequestID()-produced RequestRef.
func TestDeliverActiveSession_RunningPhase_SlackKind_EmitsQueuedMessagesInteractionRequest(t *testing.T) {
	// No awaitingUserInputSince: a genuine mid-turn barge-in, not an awaited reply.
	ch, existing := slackSessionFixture(t, spiceboxv1alpha1.AgentSessionPhaseRunning, nil)
	p, _, mem, nats, _ := newPipeline(t, ch, existing)

	dec, err := p.Deliver(context.Background(), channelkinds.InboundEvent{
		Channel:     ch,
		ExternalIDs: channelkinds.ExternalIdentity{Kind: "slack", ExternalID: "U1", Email: "a@example.com"},
		ChannelKey:  "thread:C1:1",
		MessageText: "are you still there?",
	})
	require.NoError(t, err, "Deliver")
	assert.Equal(t, channelkinds.OutcomeRouted, dec.Outcome)
	require.Len(t, mem.appends, 1, "memory appends")
	assert.True(t, dec.Queued, "a genuine mid-turn barge-in IS queued")

	for _, s := range nats.subjects {
		assert.NotContains(t, s, ".out."+string(channelevents.KindEnqueueAck),
			"a Slack session must NOT publish the legacy KindEnqueueAck, got subjects %v", nats.subjects)
	}

	idx := -1
	for i, s := range nats.subjects {
		if strings.Contains(s, ".out."+string(channelevents.KindInteractionRequest)) {
			idx = i
			break
		}
	}
	require.GreaterOrEqual(t, idx, 0,
		"a Slack session's mid-turn ack must publish interaction_request(queued_messages), got subjects %v", nats.subjects)

	var env channelevents.Envelope
	require.NoError(t, json.Unmarshal(nats.payloads[idx], &env))
	var pl channelevents.InteractionRequestPayload
	require.NoError(t, json.Unmarshal(env.Payload, &pl))

	assert.Equal(t, categories.QueuedMessages, pl.Category, "category")
	assert.Equal(t, channelevents.AudienceRequester, pl.Audience.Scope, "audience scope")
	require.NotNil(t, pl.Audience.Requester, "audience requester")
	assert.Equal(t, "slack", pl.Audience.Requester.Kind.String(), "requester kind — the message sender")
	assert.Equal(t, "U1", pl.Audience.Requester.ExternalID.String(), "requester external id")
	assert.Equal(t, "a@example.com", pl.Audience.Requester.Email.String(), "requester email")
	assert.Equal(t, "You messaged while I'm working — your message is queued.", pl.Lead, "lead — byte-identical to the legacy ack text")
	require.Len(t, pl.Actions, 1, "one decision action")
	assert.Equal(t, "interrupt", pl.Actions[0].ID, "action id")
	assert.Equal(t, "Interrupt & Send Now", pl.Actions[0].Label, "action label")
	assert.Equal(t, channelevents.ActionKindDecision, pl.Actions[0].Kind, "action kind")
	assert.Empty(t, pl.Actions[0].Style, "legacy interrupt button has no style")
	assert.Len(t, pl.RequestRef, 32, "requestRef is the minted 32-hex request id")
}

// slackSessionFixture returns a Slack-kind Channel + active AgentSession pair
// in the given phase. awaitingSince, when non-nil, stands the session up as
// having yielded to the user via await_user_message (status.awaitingUserInputSince
// set, phase deliberately still Running — see Loop.OnAwaitYield).
func slackSessionFixture(t *testing.T, phase string, awaitingSince *metav1.Time) (*spiceboxv1alpha1.Channel, *spiceboxv1alpha1.AgentSession) {
	t.Helper()
	ch := &spiceboxv1alpha1.Channel{
		ObjectMeta: metav1.ObjectMeta{Name: "c1", Namespace: "default"},
		Spec: spiceboxv1alpha1.ChannelSpec{
			Kind: "slack", AgentClass: "ac1",
			CredentialsRef: spiceboxv1alpha1.ChannelCredentialsRef{SecretName: "s"},
		},
	}
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{
			Name: "c1-abc", Namespace: "default",
			Labels: map[string]string{
				spiceboxv1alpha1.LabelChannelName: "c1",
				spiceboxv1alpha1.LabelChannelKey:  sha256HexTest("thread:C1:1"),
			},
		},
		Spec: spiceboxv1alpha1.AgentSessionSpec{
			Class: "ac1",
			InputChannel: &spiceboxv1alpha1.ChannelBinding{
				Name: "c1", Kind: "slack", Key: "thread:C1:1",
				NATSSubjectPrefix: "ap.session.default.c1-abc",
			},
		},
		Status: spiceboxv1alpha1.AgentSessionStatus{
			Phase:                  phase,
			AwaitingUserInputSince: awaitingSince,
		},
	}
	return ch, sess
}

// TestDeliverActiveSession_AwaitingUserInput_SlackKind_NoQueuedInteraction
// pins the distinction between "the user barged into a live turn" and "the
// user answered the question the agent asked".
//
// await_user_message deliberately does NOT leave phase=Running — the pod stays
// alive and the yield is recorded in status.awaitingUserInputSince instead (see
// Loop.OnAwaitYield). A phase==Running test alone therefore cannot tell the two
// apart, and treating the awaited reply as an interruption tells the user "You
// messaged while I'm working — your message is queued." and offers an
// "Interrupt & Send Now" button, while the runner is in fact parked waiting for
// exactly that message with nothing in flight to interrupt.
//
// internal/cmd/channelsd's silence watchdog draws this same line
// (AwaitingUserInputSince != nil → WaitVisible); the enqueue-ack gate must too.
func TestDeliverActiveSession_AwaitingUserInput_SlackKind_NoQueuedInteraction(t *testing.T) {
	awaiting := metav1.NewTime(time.Now().Add(-30 * time.Second))
	ch, existing := slackSessionFixture(t, spiceboxv1alpha1.AgentSessionPhaseRunning, &awaiting)
	p, _, mem, nats, _ := newPipeline(t, ch, existing)

	dec, err := p.Deliver(context.Background(), channelkinds.InboundEvent{
		Channel:     ch,
		ExternalIDs: channelkinds.ExternalIdentity{Kind: "slack", ExternalID: "U1", Email: "a@example.com"},
		ChannelKey:  "thread:C1:1",
		MessageText: "eu-west",
	})
	require.NoError(t, err, "Deliver")
	assert.Equal(t, channelkinds.OutcomeRouted, dec.Outcome)
	require.Len(t, mem.appends, 1, "the reply is still appended and the runner still woken")

	assert.False(t, dec.Queued,
		"a reply to the agent's own await_user_message is not a mid-turn interruption; Queued=true also suppresses the channel kind's turn-start status and leaves the silence watchdog unarmed")

	for _, s := range nats.subjects {
		assert.NotContains(t, s, ".out."+string(channelevents.KindInteractionRequest),
			"no interaction_request(queued_messages) for an awaited reply, got subjects %v", nats.subjects)
		assert.NotContains(t, s, ".out."+string(channelevents.KindEnqueueAck),
			"no legacy enqueue ack either, got subjects %v", nats.subjects)
	}
}

// TestDeliverActiveSession_AwaitingUserInput_NonSlackKind_NoEnqueueAck is the
// same fact on the KindEnqueueAck path: the awaiting-user gate belongs to the
// shared `queued` decision, not to the Slack branch alone, so a kind on
// KindEnqueueAck must stay just as quiet.
func TestDeliverActiveSession_AwaitingUserInput_NonSlackKind_NoEnqueueAck(t *testing.T) {
	ch := newChannel("c1")
	existing := existingSession(t, "c1-abc", "", spiceboxv1alpha1.AgentSessionPhaseRunning)
	awaiting := metav1.NewTime(time.Now().Add(-30 * time.Second))
	existing.Status.AwaitingUserInputSince = &awaiting
	p, _, mem, nats, _ := newPipeline(t, ch, existing)

	dec, err := p.Deliver(context.Background(), channelkinds.InboundEvent{
		Channel:     ch,
		ExternalIDs: channelkinds.ExternalIdentity{Kind: "slack", ExternalID: "U1", Email: "a@example.com"},
		ChannelKey:  "thread:C1:1",
		MessageText: "eu-west",
	})
	require.NoError(t, err, "Deliver")
	assert.Equal(t, channelkinds.OutcomeRouted, dec.Outcome)
	require.Len(t, mem.appends, 1, "the reply is still appended and the runner still woken")

	assert.False(t, dec.Queued, "an awaited reply is not queued behind an in-flight turn")

	for _, s := range nats.subjects {
		assert.NotContains(t, s, ".out."+string(channelevents.KindEnqueueAck),
			"no enqueue ack for an awaited reply, got subjects %v", nats.subjects)
	}
}

// TestInbound_RecordsChannelMsgRef verifies that the inbound pipeline writes
// a channel_msg_ref entry after appending the inbox turn. The ref index lets
// the Slack restart UI map a clicked message back to a turn index.
func TestInbound_RecordsChannelMsgRef(t *testing.T) {
	ch := newChannel("c1")
	existing := existingSession(t, "c1-abc", "U1", spiceboxv1alpha1.AgentSessionPhaseRunning)
	p, _, mem, _, _ := newPipeline(t, ch, existing)

	dec, err := p.Deliver(context.Background(), channelkinds.InboundEvent{
		Channel:     ch,
		ExternalIDs: channelkinds.ExternalIdentity{Kind: "slack", ExternalID: "U1", Email: "a@example.com"},
		ChannelKey:  "thread:C1:1",
		MessageText: "restart from here test",
		External: map[string]string{
			"channel_id": "C1",
			"thread_ts":  "1.0",
			"message_ts": "1.5",
		},
	})
	require.NoError(t, err, "Deliver")
	assert.Equal(t, channelkinds.OutcomeRouted, dec.Outcome, "outcome")

	// Memory append must have happened.
	require.Len(t, mem.appends, 1, "expected one memory append")

	// A channel_msg_ref entry must have been recorded.
	require.Len(t, mem.msgRefs, 1, "expected one channel_msg_ref record")
	got := mem.msgRefs[0]
	assert.Equal(t, "default", got.Ns, "namespace")
	assert.Equal(t, "c1-abc", got.Name, "session name")
	assert.Equal(t, "slack", got.Kind, "kind")
	assert.Equal(t, "C1:1.0:1.5", got.Ref, "ref format: channel_id:thread_ts:message_ts")
	// Index must be the just-appended turn's position (0 since the session
	// has no prior turns; the append is the only entry ReadAll returns).
	assert.Equal(t, 0, got.Idx, "turn index")
}

func TestAuthorSubject(t *testing.T) {
	cases := []struct {
		name string
		ev   channelkinds.InboundEvent
		want identity.Subject
	}{
		{
			name: "slack user with verified email: user:<b64 email>",
			ev:   channelkinds.InboundEvent{ExternalIDs: channelkinds.ExternalIdentity{Kind: "slack", TeamScope: "T0COMPANY", ExternalID: "U0ALICE", Email: "alice@example.com"}},
			want: canonicalID((channelkinds.ExternalIdentity{Kind: "slack", TeamScope: "T0COMPANY", ExternalID: "U0ALICE", Email: "alice@example.com"})).Subject(),
		},
		{
			name: "slack user without email: synthetic slack:<team>:<user>",
			ev:   channelkinds.InboundEvent{ExternalIDs: channelkinds.ExternalIdentity{Kind: "slack", TeamScope: "T0COMPANY", ExternalID: "U0GUEST"}},
			want: canonicalID((channelkinds.ExternalIdentity{Kind: "slack", TeamScope: "T0COMPANY", ExternalID: "U0GUEST"})).Subject(),
		},
		{
			name: "bento/service inbound (no external id): empty author",
			ev:   channelkinds.InboundEvent{AuthzSubject: "service:cron-bot"},
			want: identity.Subject(""),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, authorSubject(tc.ev))
		})
	}
}

// ChannelBinding.capabilities is a REQUIRED field in the AgentSession CRD. A
// kind that advertises none (bento — input-only, no rendering surface) returns
// a nil slice, which marshals to `null` and makes the apiserver reject the
// whole AgentSession with `spec.inputChannel.capabilities: Required value`.
//
// This is not hypothetical: it made bento sessions uncreatable in a real
// cluster, and because Bento's output retries on error it turned into a ~1/s
// hot loop against the apiserver. A kind with no capabilities must serialize
// as [], never null.
func TestDeliverBindingCapabilitiesSerializeAsEmptyNotNull(t *testing.T) {
	const subj = "service:demo-cron"
	ch := newBentoChannel("demo-input", subj)
	p, _, _, _, cli := newPipeline(t, ch, newSlackOutputChannel("test-output", "C1"))

	// Mirror reality: bento advertises nothing, slack advertises formats.
	p.Capabilities = func(kind string) []string {
		if kind == "slack" {
			return []string{"text", "markdown"}
		}
		return nil
	}

	_, err := p.Deliver(context.Background(), channelkinds.InboundEvent{
		Channel:      ch,
		ExternalIDs:  channelkinds.ExternalIdentity{},
		ChannelKey:   "cron:demo-input:1.0",
		MessageText:  "run the digest",
		AuthzSubject: subj,
	})
	require.NoError(t, err, "Deliver")

	var sessions spiceboxv1alpha1.AgentSessionList
	require.NoError(t, cli.List(context.Background(), &sessions))
	require.Len(t, sessions.Items, 1)
	sess := sessions.Items[0]

	require.NotNil(t, sess.Spec.InputChannel)
	assert.NotNil(t, sess.Spec.InputChannel.Capabilities,
		"nil capabilities marshals to null and the apiserver rejects the session")

	// The serialized form is what the apiserver validates against.
	raw, err := json.Marshal(sess.Spec.InputChannel)
	require.NoError(t, err)
	assert.Contains(t, string(raw), `"capabilities":[]`,
		"input binding must serialize capabilities as [], got: %s", raw)

	require.NotNil(t, sess.Spec.OutputChannel)
	rawOut, err := json.Marshal(sess.Spec.OutputChannel)
	require.NoError(t, err)
	assert.NotContains(t, string(rawOut), `"capabilities":null`,
		"output binding must never serialize capabilities as null, got: %s", rawOut)
}

// A service subject (bento cron) must NOT be written to agentsession#started_by.
// The schema declares `relation started_by: user`, and the SpiceDB write
// hardcodes ObjectType "user" with the canonical as ObjectId — so a
// "service:<id>" canonical yields `user:service:<id>`, whose object_id contains
// a colon and is rejected by SpiceDB's object-id regex. Every cron session then
// fails AuthzWriteFailed, and Bento's retry turns that into a storm.
//
// A cron session has no human starter, so started_by is simply absent — the
// same state kubectl-driven sessions are already in (see StartedByCanonical's
// "Returns \"\" when the annotation is absent"). Ownership still arrives via
// the operator's owner resolver.
func TestDeliverServiceSubjectSkipsStartedByWrite(t *testing.T) {
	const subj = "service:demo-cron"
	ch := newBentoChannel("demo-input", subj)
	p, az, _, _, cli := newPipeline(t, ch, newSlackOutputChannel("test-output", "C1"))

	dec, err := p.Deliver(context.Background(), channelkinds.InboundEvent{
		Channel:      ch,
		ExternalIDs:  channelkinds.ExternalIdentity{},
		ChannelKey:   "cron:demo-input:1.0",
		MessageText:  "run the digest",
		AuthzSubject: subj,
	})
	require.NoError(t, err, "Deliver must succeed for a service subject")
	require.Equal(t, channelkinds.OutcomeRouted, dec.Outcome)

	assert.Equal(t, 0, az.startedByCalls,
		"a service subject must not be written to started_by (schema allows only `user`)")

	var sessions spiceboxv1alpha1.AgentSessionList
	require.NoError(t, cli.List(context.Background(), &sessions))
	require.Len(t, sessions.Items, 1)
	sess := sessions.Items[0]

	assert.Empty(t, sess.Annotations[spiceboxv1alpha1.AnnotationStartedByCanonicalID],
		"no started-by annotation for a session with no human starter")
	assert.Empty(t, spiceboxv1alpha1.StartedByCanonical(&sess),
		"StartedByCanonical must read empty, as for kubectl-driven sessions")
}

// A human subject still writes started_by — B must not disable it generally.
func TestDeliverHumanSubjectStillWritesStartedBy(t *testing.T) {
	ch := newChannel("c1")
	p, az, _, _, cli := newPipeline(t, ch)

	_, err := p.Deliver(context.Background(), channelkinds.InboundEvent{
		Channel:     ch,
		ExternalIDs: channelkinds.ExternalIdentity{Kind: "fake", ExternalID: "U1", Email: "u@example.com"},
		ChannelKey:  "thread:C9:1.0",
		MessageText: "hello",
	})
	require.NoError(t, err)
	assert.Equal(t, 1, az.startedByCalls, "human inbound must still write started_by")

	var sessions spiceboxv1alpha1.AgentSessionList
	require.NoError(t, cli.List(context.Background(), &sessions))
	require.Len(t, sessions.Items, 1)
	assert.NotEmpty(t, sessions.Items[0].Annotations[spiceboxv1alpha1.AnnotationStartedByCanonicalID])
}

// A cron-spawned session has no started-by annotations, and the join-approval
// path builds its approver from exactly those. Before the addressability
// guard, a human replying in a cron thread produced an interaction_request
// addressed to nobody: parked durably, never deliverable, never approvable,
// and no error anywhere. The requester simply got silence.
//
// Fail loudly instead. (The monitoring-channel fallback that turns this from
// "loud error" into "an admin can actually approve it" is the next layer;
// this pins the surfacing so the failure can never be silent again.)
func TestDeliverUnapprovableJoinRequestFailsLoudly(t *testing.T) {
	ch := newChannel("c1")
	// No started-by: exactly the shape of a cron/bento-spawned session.
	existing := existingSession(t, "c1-abc", "", spiceboxv1alpha1.AgentSessionPhaseIdle)
	existing.Spec.InputChannel = nil
	p, az, _, _, cli := newPipeline(t, ch, existing)
	az.checkResult = false // requester lacks interact -> join-request path

	dec, err := p.Deliver(context.Background(), channelkinds.InboundEvent{
		Channel:     ch,
		ExternalIDs: channelkinds.ExternalIdentity{Kind: "slack", ExternalID: "U_other", Email: "b@example.com"},
		ChannelKey:  "thread:C1:1",
		MessageText: "contacts please",
	})

	// Refused cleanly, NOT returned as an error: this is a permanent config
	// condition, and an error would make a bento inbound retry it forever
	// (forward_to_pipeline retries on error) — one misconfiguration becoming
	// a request-per-second flood.
	require.NoError(t, err, "a permanent config condition must not be a retryable error")
	assert.Equal(t, channelkinds.OutcomeDeniedByPermission, dec.Outcome)

	// The requester must be TOLD. A suppressed notice posts nothing, which is
	// precisely the silence being fixed here.
	require.False(t, dec.Notice.IsSuppressed(), "the requester must get a user-visible explanation")
	assert.Equal(t, categories.JoinNoApprover, dec.Notice.Category())
	assert.Contains(t, dec.Notice.Args().Body, "no owner",
		"the message should say why, not just that it failed")
	assert.NotEmpty(t, dec.Notice.Args().NextStep,
		"a danger notice must tell the user what to do instead")

	// And it must not leave a durable pending record that nobody can ever act
	// on — that record is what made the original failure look like success.
	var got spiceboxv1alpha1.AgentSession
	require.NoError(t, cli.Get(context.Background(),
		client.ObjectKey{Namespace: "default", Name: "c1-abc"}, &got))
	assert.Empty(t, got.Status.PendingRequesters,
		"no unapprovable pending requester may be left behind")
}

// The unapprovable-join surfaces are read by humans under pressure: the
// requester in-thread, and an admin in the monitoring channel. Pin their shape.
func TestUnapprovableJoinHint_NamesTheInputChannelNotAHardcodedKind(t *testing.T) {
	cases := []struct {
		name        string
		input       *spiceboxv1alpha1.ChannelBinding
		wantContain []string
		wantAbsent  []string
	}{
		{
			name:        "bento input: names the kind and Channel, not a hardcoded string",
			input:       &spiceboxv1alpha1.ChannelBinding{Kind: "bento", Name: "demo-input"},
			wantContain: []string{"bento", `"demo-input"`, "spec.owner.explicit"},
		},
		{
			name:  "a different input kind is named accurately",
			input: &spiceboxv1alpha1.ChannelBinding{Kind: "fake", Name: "other-input"},
			// The old text said "a cron/bento Channel" regardless of kind,
			// which would send an admin to the wrong CR.
			wantContain: []string{"fake", `"other-input"`},
			wantAbsent:  []string{"bento"},
		},
		{
			name:        "no input binding (kubectl-driven): kind-agnostic, still actionable",
			input:       nil,
			wantContain: []string{"no input Channel", "spec.owner.explicit"},
			wantAbsent:  []string{"bento"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sess := &spiceboxv1alpha1.AgentSession{
				Spec: spiceboxv1alpha1.AgentSessionSpec{InputChannel: tc.input},
			}
			got := unapprovableJoinHint(sess)
			for _, want := range tc.wantContain {
				assert.Contains(t, got, want)
			}
			for _, absent := range tc.wantAbsent {
				assert.NotContains(t, got, absent, "must not hardcode an unrelated kind")
			}
		})
	}
}

// The requester-facing message must carry the same ⚠️ affordance every other
// can't-do message in this package uses, or it reads as plain prose in a busy
// thread.
func TestUnapprovableJoinNoticeCarriesDangerSeverity(t *testing.T) {
	existing := existingSession(t, "c1-abc", "", spiceboxv1alpha1.AgentSessionPhaseIdle)
	existing.Spec.InputChannel = nil
	plain := newChannel("c1")
	p, az, _, _, _ := newPipeline(t, plain, existing)
	az.checkResult = false

	dec, err := p.Deliver(context.Background(), channelkinds.InboundEvent{
		Channel:     plain,
		ExternalIDs: channelkinds.ExternalIdentity{Kind: "slack", ExternalID: "U_other", Email: "b@example.com"},
		ChannelKey:  "thread:C1:1",
		MessageText: "contacts please",
	})
	require.NoError(t, err)
	// The affordance is the category's severity, not a leading emoji glued on by
	// the publisher: each surface paints it in its own idiom (Slack a red chip,
	// the text floor "[!]", the TUI red). Asserting the severity rather than the
	// glyph is what makes this test survive a change of house style.
	require.False(t, dec.Notice.IsSuppressed(), "the requester must be told")
	cat, ok := channelinteractions.Get(dec.Notice.Category())
	require.True(t, ok, "the notice's category must be registered")
	assert.Equal(t, channelinteractions.ToneCritical, cat.Tone,
		"an unapprovable join is permanent, so it must carry the critical tone")
	assert.True(t, cat.Terminal,
		"and it is terminal: no retry in this thread will ever find an approver")
}
