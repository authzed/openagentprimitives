package workshopthreadsrv_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkey"
	"github.com/authzed/openagentprimitives/pkg/controllers/testfixtures"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/tokens"
	"github.com/authzed/openagentprimitives/pkg/web/workshopthreadsrv"
)

// fakeChecker records every call so a test can assert the route re-checks
// the LIVE workshop:<W>#build tuple with the values it derived from the
// resolved Workshop CR, rather than trusting the bearer alone.
type fakeChecker struct {
	allow bool
	err   error
	calls []checkCall
}

type checkCall struct{ workshopID, sessNS, sessName string }

func (f *fakeChecker) CheckWorkshopBuild(_ context.Context, workshopID, sessNS, sessName string) (bool, error) {
	f.calls = append(f.calls, checkCall{workshopID, sessNS, sessName})
	return f.allow, f.err
}

const (
	sessNS      = "default"
	sessName    = "builder-x"
	wsNamespace = "ws-abc123456789"
	channelName = "demo-channel"
	channelKind = "fake"
	channelKey  = "thread:C100:T200"
	channelID   = "C100"
	threadTS    = "T200"
)

func readyWorkshop() *spiceboxv1alpha1.Workshop {
	return &spiceboxv1alpha1.Workshop{
		ObjectMeta: metav1.ObjectMeta{
			Name:      spiceboxv1alpha1.WorkshopName(sessName),
			Namespace: sessNS,
		},
		Spec: spiceboxv1alpha1.WorkshopSpec{
			Session:        spiceboxv1alpha1.NamespacedRef{Namespace: sessNS, Name: sessName},
			SidecarToolbox: "workshop",
			Limits: spiceboxv1alpha1.WorkshopLimits{
				MaxAge:              metav1.Duration{Duration: time.Hour},
				MaxObjectsPerKind:   10,
				MaxObjects:          50,
				MaxConcurrentProbes: 2,
			},
		},
		Status: spiceboxv1alpha1.WorkshopStatus{
			Namespace: wsNamespace,
			Phase:     spiceboxv1alpha1.WorkshopPhaseReady,
		},
	}
}

func provisioningWorkshop() *spiceboxv1alpha1.Workshop {
	ws := readyWorkshop()
	ws.Status.Phase = spiceboxv1alpha1.WorkshopPhaseProvisioning
	return ws
}

// threadBinding is the channel binding every genuine thread participant
// (including the builder, X, itself) carries — same Channel Name, same Key,
// same external channel_id/thread_ts.
func threadBinding() *spiceboxv1alpha1.ChannelBinding {
	return &spiceboxv1alpha1.ChannelBinding{
		Name:     channelName,
		Kind:     channelKind,
		Key:      channelKey,
		External: map[string]string{"channel_id": channelID, "thread_ts": threadTS},
	}
}

// threadLabels are the correlation labels channelsd's pipeline stamps on a
// channel-spawned AgentSession — the shape a List keys on
// (pkg/channels/channelsd/pipeline/pipeline.go:451-460).
func threadLabels() map[string]string {
	return map[string]string{
		spiceboxv1alpha1.LabelChannelName: channelName,
		spiceboxv1alpha1.LabelChannelKey:  channelkey.LabelValue(channelKey),
	}
}

// builderSession is X: the workshop's own builder session, channel-bound to
// the thread it was pulled into.
func builderSession() *spiceboxv1alpha1.AgentSession {
	return &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Namespace: sessNS, Name: sessName, Labels: threadLabels()},
		Spec: spiceboxv1alpha1.AgentSessionSpec{
			Class:        "builder-class",
			Prompt:       spiceboxv1alpha1.PromptSource{Inline: "build something for this thread"},
			InputChannel: threadBinding(),
		},
	}
}

// peerSession is another agent genuinely bound to the SAME thread as X —
// the ordinary case this route exists to surface.
// sameKeyDifferentAnchorSession shares the builder's channel KEY — so the
// label-selector List returns it — but names a different conversation in its
// routing metadata. Only bindsAnchor can exclude it, which is the whole
// reason this route filters on the anchor rather than trusting the key: a
// coarse key (a DM's "dm:<user_id>", say) covers every conversation that
// ever happened with that person, so the key alone would hand the builder
// sessions from unrelated conversations.
func sameKeyDifferentAnchorSession(name string) *spiceboxv1alpha1.AgentSession {
	return &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Namespace: sessNS, Name: name, Labels: threadLabels()},
		Spec: spiceboxv1alpha1.AgentSessionSpec{
			Class:  "unrelated-conversation-class",
			Prompt: spiceboxv1alpha1.PromptSource{Inline: "an unrelated conversation under the same key"},
			InputChannel: &spiceboxv1alpha1.ChannelBinding{
				Name: channelName,
				Kind: channelKind,
				Key:  channelKey,
				// Same key, different conversation.
				External: map[string]string{"channel_id": channelID, "thread_ts": "T-unrelated"},
			},
		},
	}
}

func peerSession(name, class string) *spiceboxv1alpha1.AgentSession {
	return &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Namespace: sessNS, Name: name, Labels: threadLabels()},
		Spec: spiceboxv1alpha1.AgentSessionSpec{
			Class:        class,
			Prompt:       spiceboxv1alpha1.PromptSource{Inline: "help with this thread"},
			InputChannel: threadBinding(),
		},
	}
}

// delegatedChild is a subagent spawned WHILE working the thread: it carries
// LabelDelegationRoot naming its TREE ROOT (rootName — which, for a child two
// or more hops down, is the root's own name, not its immediate parent's) and
// Spec.Parent naming its immediate parent (parentName), exactly as buildChild
// (subagentrequest/controller.go:957-1064) stamps both on a real task/chat
// child: labels[LabelDelegationRoot] = RootNameFor(parent), and
// spec.Parent = &NamespacedRef{parent.Namespace, parent.Name}. Spec.Parent
// is load-bearing for DescendantsOf/WalkAncestors (agentsession_lineage.go),
// which climb it to decide subtree membership — a fixture that only stamped
// the label, as this one once did, silently broke that walk.
func delegatedChild(name, rootName, parentName, class string) *spiceboxv1alpha1.AgentSession {
	return &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: sessNS,
			Name:      name,
			Labels:    map[string]string{spiceboxv1alpha1.LabelDelegationRoot: rootName},
		},
		Spec: spiceboxv1alpha1.AgentSessionSpec{
			Class:  class,
			Prompt: spiceboxv1alpha1.PromptSource{Inline: "handle a subtask"},
			Parent: &spiceboxv1alpha1.NamespacedRef{Namespace: sessNS, Name: parentName},
		},
	}
}

// attendedChild returns a delegated child that is ALSO independently bound
// to the SAME conversation thread as its own root/parent — the scenario
// MAJOR-2 guards against. A real `attended` child
// (subagentrequest/controller.go's buildChild, SubagentModeAttended branch)
// copies its channel bindings from its root's own outbound binding, so it is
// found by BOTH the participant List (its own binding matches the anchor)
// AND its root's own delegation-closure walk — it must be emitted exactly
// once, as a participant.
func attendedChild(name, rootName, parentName, class string) *spiceboxv1alpha1.AgentSession {
	c := delegatedChild(name, rootName, parentName, class)
	for k, v := range threadLabels() {
		c.Labels[k] = v
	}
	c.Spec.InputChannel = threadBinding()
	return c
}

// otherThreadSession lives in the SAME namespace and the SAME Channel CR as
// X, but a DIFFERENT thread (a different thread_ts, hence a different
// spec.inputChannel.Key and a different LabelChannelKey hash). It must be
// absent from every result — see
// TestGetAgentsInThread_SameNamespaceDifferentThread_Excluded, the
// fail-first test for this.
func otherThreadSession(name string) *spiceboxv1alpha1.AgentSession {
	const otherKey = "thread:C100:T999"
	return &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: sessNS,
			Name:      name,
			Labels: map[string]string{
				spiceboxv1alpha1.LabelChannelName: channelName,
				spiceboxv1alpha1.LabelChannelKey:  channelkey.LabelValue(otherKey),
			},
		},
		Spec: spiceboxv1alpha1.AgentSessionSpec{
			Class:  "other-thread-class",
			Prompt: spiceboxv1alpha1.PromptSource{Inline: "a different conversation entirely"},
			InputChannel: &spiceboxv1alpha1.ChannelBinding{
				Name:     channelName,
				Kind:     channelKind,
				Key:      otherKey,
				External: map[string]string{"channel_id": channelID, "thread_ts": "T999"},
			},
		},
	}
}

// harness bundles the handler under test with the fakes a case inspects
// after the request.
type harness struct {
	srv     *httptest.Server
	c       client.Client
	reg     *tokens.Registry
	checker *fakeChecker
}

func newHarness(t *testing.T, checker *fakeChecker, objs ...client.Object) *harness {
	t.Helper()
	scheme := testfixtures.NewScheme(t)
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithStatusSubresource(&spiceboxv1alpha1.Workshop{}).
		Build()
	reg := tokens.NewRegistry()

	var handler http.Handler
	if checker == nil {
		handler = workshopthreadsrv.NewHandler(c, reg, nil, logr.Discard())
	} else {
		handler = workshopthreadsrv.NewHandler(c, reg, checker, logr.Discard())
	}
	h := harness{srv: httptest.NewServer(handler), c: c, reg: reg, checker: checker}
	t.Cleanup(h.srv.Close)
	return &h
}

// registerBearer registers a workshop bearer keyed the way plan 2's
// AgentSession reconciler does: the SYNTHETIC {sessNS, WorkshopName(sessName)}
// pair — never the builder session {sessNS, sessName} itself.
func (h *harness) registerBearer(token string) {
	h.reg.Set(memory.NamespacedName{Namespace: sessNS, Name: spiceboxv1alpha1.WorkshopName(sessName)}, token, "")
}

func getAgentsInThread(t *testing.T, url, token string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	require.NoError(t, err, "NewRequest")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err, "Do")
	return resp
}

func decodeAgents(t *testing.T, resp *http.Response) []workshopthreadsrv.AgentInThread {
	t.Helper()
	var out []workshopthreadsrv.AgentInThread
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out), "decode []AgentInThread")
	return out
}

func TestGetAgentsInThread_TwoParticipantsAndOneDescendant(t *testing.T) {
	checker := &fakeChecker{allow: true}
	h := newHarness(t, checker,
		readyWorkshop(),
		builderSession(),
		peerSession("peer-agent-a", "support-class"),
		peerSession("peer-agent-b", "ops-class"),
		delegatedChild("peer-agent-a-sub", "peer-agent-a", "peer-agent-a", "sub-class"),
	)
	h.registerBearer("tok-workshop")

	resp := getAgentsInThread(t, h.srv.URL+workshopthreadsrv.Path, "tok-workshop")
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode, "workshop:build holds -> 200")

	out := decodeAgents(t, resp)
	require.Len(t, out, 3, "two peers + one delegated descendant; the builder itself is excluded")
	assert.Equal(t, []workshopthreadsrv.AgentInThread{
		{Namespace: sessNS, Name: "peer-agent-a", Class: "support-class", Role: workshopthreadsrv.RoleParticipant},
		{Namespace: sessNS, Name: "peer-agent-a-sub", Class: "sub-class", Role: workshopthreadsrv.RoleDescendant},
		{Namespace: sessNS, Name: "peer-agent-b", Class: "ops-class", Role: workshopthreadsrv.RoleParticipant},
	}, out)

	// The route re-checked the LIVE workshop:build tuple with the values it
	// derived from the resolved Workshop CR — the workshop id W and the
	// BUILDER session as subject — not the synthetic bearer key.
	require.Len(t, checker.calls, 1, "CheckWorkshopBuild called exactly once")
	assert.Equal(t, checkCall{wsNamespace, sessNS, sessName}, checker.calls[0])
}

// TestGetAgentsInThread_SameNamespaceDifferentThread_Excluded is the
// load-bearing test that proves this route is a THREAD lookup and not a
// namespace (or even same-Channel) dump: otherThreadSession lives in the
// bearer's own namespace, on the SAME Channel CR, but a DIFFERENT thread. It
// must never appear in the result.
//
// This was made to FAIL FIRST during development: the List's
// MatchingLabels was temporarily widened to {LabelChannelName} alone (the
// LabelChannelKey requirement dropped) with the anchor filter (bindsAnchor)
// also temporarily bypassed, which let otherThreadSession leak into the
// result — "other-thread-agent" appeared in names alongside the genuine
// peer, failing the NotContains assertion below. The scoping was then
// restored and this test passes against the real implementation.
func TestGetAgentsInThread_SameNamespaceDifferentThread_Excluded(t *testing.T) {
	checker := &fakeChecker{allow: true}
	h := newHarness(t, checker,
		readyWorkshop(),
		builderSession(),
		peerSession("peer-agent-a", "support-class"),
		otherThreadSession("other-thread-agent"),
	)
	h.registerBearer("tok-workshop")

	resp := getAgentsInThread(t, h.srv.URL+workshopthreadsrv.Path, "tok-workshop")
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	out := decodeAgents(t, resp)
	names := make([]string, 0, len(out))
	for _, a := range out {
		names = append(names, a.Name)
	}
	assert.NotContains(t, names, "other-thread-agent",
		"a session on a DIFFERENT thread in the same namespace must never be returned")
	assert.Contains(t, names, "peer-agent-a", "the genuine thread peer is still returned")
}

// TestGetAgentsInThread_SameKeyDifferentAnchor_Excluded is the assertion that
// proves this route is a THREAD lookup and not a namespace dump. Its sibling
// above uses a different channel key, so the label selector alone excludes
// that one and the anchor filter is never exercised; here the key is
// IDENTICAL and only the anchor can tell the two conversations apart.
// Verified to bite: with the bindsAnchor filter disabled, this test fails.
func TestGetAgentsInThread_SameKeyDifferentAnchor_Excluded(t *testing.T) {
	checker := &fakeChecker{allow: true}
	h := newHarness(t, checker,
		readyWorkshop(),
		builderSession(),
		peerSession("peer-agent-a", "support-class"),
		sameKeyDifferentAnchorSession("unrelated-conversation-agent"),
	)
	h.registerBearer("tok-workshop")

	resp := getAgentsInThread(t, h.srv.URL+workshopthreadsrv.Path, "tok-workshop")
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	out := decodeAgents(t, resp)
	names := make([]string, 0, len(out))
	for _, a := range out {
		names = append(names, a.Name)
	}
	assert.NotContains(t, names, "unrelated-conversation-agent",
		"a session sharing the channel key but NOT the conversation must never be returned")
	assert.Contains(t, names, "peer-agent-a", "the genuine thread peer is still returned")
}

func TestGetAgentsInThread_BuildDoesNotHold_403(t *testing.T) {
	checker := &fakeChecker{allow: false}
	h := newHarness(t, checker, readyWorkshop(), builderSession(), peerSession("peer-agent-a", "support-class"))
	h.registerBearer("tok-workshop")

	resp := getAgentsInThread(t, h.srv.URL+workshopthreadsrv.Path, "tok-workshop")
	defer resp.Body.Close()
	assert.Equal(t, http.StatusForbidden, resp.StatusCode, "workshop:build false -> 403")
	require.Len(t, checker.calls, 1, "the permission WAS re-checked")
}

func TestGetAgentsInThread_CheckErrors_403(t *testing.T) {
	checker := &fakeChecker{err: errors.New("spicedb unavailable")}
	h := newHarness(t, checker, readyWorkshop(), builderSession())
	h.registerBearer("tok-workshop")

	resp := getAgentsInThread(t, h.srv.URL+workshopthreadsrv.Path, "tok-workshop")
	defer resp.Body.Close()
	assert.Equal(t, http.StatusForbidden, resp.StatusCode, "check error is fail-closed, not fail-open")
}

func TestGetAgentsInThread_NilChecker_503DeniesWithoutPanicking(t *testing.T) {
	h := newHarness(t, nil, readyWorkshop(), builderSession())
	h.registerBearer("tok-workshop")

	assert.NotPanics(t, func() {
		resp := getAgentsInThread(t, h.srv.URL+workshopthreadsrv.Path, "tok-workshop")
		defer resp.Body.Close()
		assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode, "an unwired checker denies, it does not panic")
	})
}

func TestGetAgentsInThread_UnknownBearer_401(t *testing.T) {
	checker := &fakeChecker{allow: true}
	h := newHarness(t, checker, readyWorkshop(), builderSession())
	// No bearer registered at all.

	resp := getAgentsInThread(t, h.srv.URL+workshopthreadsrv.Path, "bogus-token")
	defer resp.Body.Close()
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	assert.Empty(t, checker.calls, "an unauthenticated request never reaches the permission check")
}

func TestGetAgentsInThread_NoBearerHeader_401(t *testing.T) {
	checker := &fakeChecker{allow: true}
	h := newHarness(t, checker, readyWorkshop(), builderSession())

	resp := getAgentsInThread(t, h.srv.URL+workshopthreadsrv.Path, "")
	defer resp.Body.Close()
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

// TestGetAgentsInThread_WorkshopNotReady_403 covers a bearer that resolves
// to a real Workshop CR that has not finished provisioning: a stale/racing
// bearer must still be refused rather than trusted.
func TestGetAgentsInThread_WorkshopNotReady_403(t *testing.T) {
	checker := &fakeChecker{allow: true}
	h := newHarness(t, checker, provisioningWorkshop())
	h.registerBearer("tok-workshop")

	resp := getAgentsInThread(t, h.srv.URL+workshopthreadsrv.Path, "tok-workshop")
	defer resp.Body.Close()
	assert.Equal(t, http.StatusForbidden, resp.StatusCode, "not-Ready workshop denies")
	assert.Empty(t, checker.calls, "never reaches the permission check for a not-yet-provisioned workshop")
}

// TestGetAgentsInThread_BearerWithNoWorkshopCR_403 covers a registered
// bearer whose synthetic key names no Workshop CR at all (e.g. torn down
// between token registration and this request).
func TestGetAgentsInThread_BearerWithNoWorkshopCR_403(t *testing.T) {
	checker := &fakeChecker{allow: true}
	h := newHarness(t, checker) // no Workshop object seeded
	h.registerBearer("tok-workshop")

	resp := getAgentsInThread(t, h.srv.URL+workshopthreadsrv.Path, "tok-workshop")
	defer resp.Body.Close()
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	assert.Empty(t, checker.calls)
}

// TestGetAgentsInThread_BuilderNotOnAChannel_EmptyNotError covers a builder
// session with no spec.inputChannel/outputChannel at all (kubectl-driven, or
// not yet bound) — there is no thread to reproduce, which is an ordinary
// empty result, not a failure. MINOR-9: the wire bytes must be the JSON array
// `[]`, not `null` — asserting only assert.Empty on the DECODED slice cannot
// tell the two apart, since json.Decode happily unmarshals `null` into a nil
// []AgentInThread too.
func TestGetAgentsInThread_BuilderNotOnAChannel_EmptyNotError(t *testing.T) {
	checker := &fakeChecker{allow: true}
	bare := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Namespace: sessNS, Name: sessName},
		Spec: spiceboxv1alpha1.AgentSessionSpec{
			Class:  "builder-class",
			Prompt: spiceboxv1alpha1.PromptSource{Inline: "build something"},
		},
	}
	h := newHarness(t, checker, readyWorkshop(), bare, peerSession("peer-agent-a", "support-class"))
	h.registerBearer("tok-workshop")

	resp := getAgentsInThread(t, h.srv.URL+workshopthreadsrv.Path, "tok-workshop")
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err, "read response body")
	assert.Equal(t, "[]\n", string(body), "an empty result must encode as the JSON array `[]`, not `null`")

	var out []workshopthreadsrv.AgentInThread
	require.NoError(t, json.Unmarshal(body, &out), "decode []AgentInThread")
	assert.Empty(t, out, "no channel binding on the builder means nothing to reproduce, not an error")
}

// outputBoundPeer is a peer discoverable ONLY via LabelOutputChannelKey — a
// cron- or output-anchored session (see outbound/relay.go's
// patchOutputChannel) whose own LabelChannelName/LabelChannelKey pair was
// never stamped at all, but whose spec.outputChannel genuinely names the
// SAME thread anchor as the builder's own binding.
func outputBoundPeer(name, class string) *spiceboxv1alpha1.AgentSession {
	return &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: sessNS,
			Name:      name,
			Labels:    map[string]string{spiceboxv1alpha1.LabelOutputChannelKey: channelkey.LabelValue(channelKey)},
		},
		Spec: spiceboxv1alpha1.AgentSessionSpec{
			Class:         class,
			Prompt:        spiceboxv1alpha1.PromptSource{Inline: "a cron-spawned reply in this thread"},
			OutputChannel: threadBinding(),
		},
	}
}

// TestGetAgentsInThread_DescendantOfNonRootParticipant_Included is the
// fail-first test for MAJOR-1. peerNonRoot is a genuine thread participant
// that is ITSELF a non-root delegated session (labelled with some OTHER
// root's name), and it has its own child. Because a descendant's
// LabelDelegationRoot names the TREE ROOT, never its immediate parent
// (RootNameFor's own doc), the child is labelled with that SAME other root,
// never with peerNonRoot's own name — so ListClosure(ns, peerNonRoot.Name)
// (the prior shape here) filtered on a value nothing carries and silently
// dropped the child. DescendantsOf resolves peerNonRoot's own root first and
// is correct whether a participant is itself a root or not.
//
// The named root ("some-other-root") is never materialized as a real
// AgentSession: DescendantsOf's own ancestor walk from the child stops the
// moment it reaches peerNonRoot (the session it was asked about), so it never
// needs to fetch peerNonRoot's OWN parent to conclude the child is its
// descendant.
func TestGetAgentsInThread_DescendantOfNonRootParticipant_Included(t *testing.T) {
	checker := &fakeChecker{allow: true}
	peerNonRoot := peerSession("peer-nonroot", "peer-class")
	peerNonRoot.Labels[spiceboxv1alpha1.LabelDelegationRoot] = "some-other-root"
	peerNonRoot.Spec.Parent = &spiceboxv1alpha1.NamespacedRef{Namespace: sessNS, Name: "some-other-root"}
	child := delegatedChild("peer-nonroot-sub", "some-other-root", "peer-nonroot", "sub-class")

	h := newHarness(t, checker, readyWorkshop(), builderSession(), peerNonRoot, child)
	h.registerBearer("tok-workshop")

	resp := getAgentsInThread(t, h.srv.URL+workshopthreadsrv.Path, "tok-workshop")
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	out := decodeAgents(t, resp)
	names := make([]string, 0, len(out))
	for _, a := range out {
		names = append(names, a.Name)
	}
	assert.Contains(t, names, "peer-nonroot", "the non-root participant itself is still returned")
	assert.Contains(t, names, "peer-nonroot-sub",
		"a non-root participant's OWN child must be returned: ListClosure(ns, p.Name) silently drops it "+
			"because the child's LabelDelegationRoot names the TREE ROOT, not p's own name")
}

// TestGetAgentsInThread_ParticipantAlsoDescendant_EmittedOnceAsParticipant is
// the fail-first test for MAJOR-2: attendedChild is independently bound to
// the thread (so the participant List finds it) AND is peerRoot's own
// delegated child (so peerRoot's closure walk finds it too). It must appear
// in the response exactly once, with role "participant".
func TestGetAgentsInThread_ParticipantAlsoDescendant_EmittedOnceAsParticipant(t *testing.T) {
	checker := &fakeChecker{allow: true}
	peerRoot := peerSession("peer-root", "root-class")
	child := attendedChild("peer-root-attended-child", "peer-root", "peer-root", "attended-class")

	h := newHarness(t, checker, readyWorkshop(), builderSession(), peerRoot, child)
	h.registerBearer("tok-workshop")

	resp := getAgentsInThread(t, h.srv.URL+workshopthreadsrv.Path, "tok-workshop")
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	out := decodeAgents(t, resp)
	var matches []workshopthreadsrv.AgentInThread
	for _, a := range out {
		if a.Name == "peer-root-attended-child" {
			matches = append(matches, a)
		}
	}
	require.Len(t, matches, 1,
		"a session that is BOTH an independent thread participant and a descendant of another "+
			"participant must be emitted exactly once, not once per role")
	assert.Equal(t, workshopthreadsrv.RoleParticipant, matches[0].Role,
		"participant wins: independent thread membership is the more specific fact")
}

// TestGetAgentsInThread_OutputChannelOnlyPeer_Included is the fail-first test
// for MAJOR-4: outputPeer carries NO LabelChannelName/LabelChannelKey at
// all — only LabelOutputChannelKey, hashed from the identical
// "thread:<channel_id>:<thread_ts>" key format the builder's own binding
// uses (outbound/relay.go's patchOutputChannel) — and its spec.outputChannel
// genuinely binds the SAME anchor. The primary List (keyed on
// LabelChannelKey alone) can never find it.
func TestGetAgentsInThread_OutputChannelOnlyPeer_Included(t *testing.T) {
	checker := &fakeChecker{allow: true}
	outputPeer := outputBoundPeer("cron-reply-peer", "cron-class")

	h := newHarness(t, checker, readyWorkshop(), builderSession(), outputPeer)
	h.registerBearer("tok-workshop")

	resp := getAgentsInThread(t, h.srv.URL+workshopthreadsrv.Path, "tok-workshop")
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	out := decodeAgents(t, resp)
	names := make([]string, 0, len(out))
	for _, a := range out {
		names = append(names, a.Name)
	}
	assert.Contains(t, names, "cron-reply-peer",
		"a peer anchored to the thread ONLY via its own output channel must still be found")
}

// closureCountingClient wraps a client.Client and counts every List call
// shaped like a delegation-closure walk (spiceboxv1alpha1.ListClosure /
// DescendantsOf's own List, MatchingLabels{LabelDelegationRoot: ...}) — the
// observable that distinguishes "the walk stopped early" from "the walk ran
// to completion and the reply was merely truncated afterward", which a
// response-length assertion alone cannot tell apart.
type closureCountingClient struct {
	client.Client
	closureListCalls *int64
}

func (c *closureCountingClient) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	for _, o := range opts {
		if ml, ok := o.(client.MatchingLabels); ok {
			if _, ok := ml[spiceboxv1alpha1.LabelDelegationRoot]; ok {
				atomic.AddInt64(c.closureListCalls, 1)
			}
		}
	}
	return c.Client.List(ctx, list, opts...)
}

// TestGetAgentsInThread_CapStopsWalkingEarly is the fail-first test for
// MAJOR-3. It seeds MaxAgentsInThread+10 thread-bound peers, none of which
// have any descendants of their own, so each contributes exactly one row.
// With the cap enforced INSIDE the loop, the walk must stop calling
// DescendantsOf once exactly MaxAgentsInThread peers have been processed —
// the remaining ones are never even asked about. Before the fix, every
// peer's closure gets listed regardless, and only the FINAL slice is
// truncated: the response length alone (asserted by both the fixed and the
// unfixed code) cannot tell that apart, so this test additionally counts the
// closure-shaped List calls, which is the only place the two behaviors
// diverge — 60 calls, one per peer, versus exactly 50.
func TestGetAgentsInThread_CapStopsWalkingEarly(t *testing.T) {
	checker := &fakeChecker{allow: true}
	scheme := testfixtures.NewScheme(t)
	objs := []client.Object{readyWorkshop(), builderSession()}
	const numPeers = workshopthreadsrv.MaxAgentsInThread + 10
	for i := 0; i < numPeers; i++ {
		objs = append(objs, peerSession(fmt.Sprintf("peer-%03d", i), "peer-class"))
	}
	fakeC := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithStatusSubresource(&spiceboxv1alpha1.Workshop{}).
		Build()
	var closureCalls int64
	counting := &closureCountingClient{Client: fakeC, closureListCalls: &closureCalls}

	reg := tokens.NewRegistry()
	reg.Set(memory.NamespacedName{Namespace: sessNS, Name: spiceboxv1alpha1.WorkshopName(sessName)}, "tok-workshop", "")
	srv := httptest.NewServer(workshopthreadsrv.NewHandler(counting, reg, checker, logr.Discard()))
	t.Cleanup(srv.Close)

	resp := getAgentsInThread(t, srv.URL+workshopthreadsrv.Path, "tok-workshop")
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	out := decodeAgents(t, resp)
	assert.Len(t, out, workshopthreadsrv.MaxAgentsInThread, "the reply is still capped at MaxAgentsInThread")
	assert.EqualValues(t, workshopthreadsrv.MaxAgentsInThread, atomic.LoadInt64(&closureCalls),
		"the walk must stop calling DescendantsOf once the cap is reached, not after enumerating every participant's closure")
}

// TestGetAgentsInThread_DanglingParentInParticipantClosure_DoesNotFailTheWholeLookup
// is the fail-first test for MAJOR-3. peerWithOrphan is a genuine thread
// participant whose own delegation closure contains a member (orphanChild)
// whose Spec.Parent names an intermediate session that no longer exists —
// exactly the shape delegate.go's SubagentRequest leaves behind when an
// intermediate session is deleted or reaped: the request carries no
// OwnerReference back to the PARENT AgentSession, only forward to the CHILD
// it creates, so nothing ever cleans up the dangling Spec.Parent. That makes
// this a reachable and PERMANENT failure mode, not a transient one. Before
// the fix, WalkAncestors' failed Get on the vanished intermediate propagated
// straight out of DescendantsOf and 500'd the ENTIRE request — including
// peerOK, an unrelated participant with no broken links of its own at all.
func TestGetAgentsInThread_DanglingParentInParticipantClosure_DoesNotFailTheWholeLookup(t *testing.T) {
	checker := &fakeChecker{allow: true}
	peerWithOrphan := peerSession("peer-with-orphan", "peer-class")
	// orphanChild is a genuine member of peer-with-orphan's own delegation
	// closure (LabelDelegationRoot names peer-with-orphan, the tree ROOT, per
	// RootNameFor's own doc) whose immediate parent, "vanished-intermediate",
	// is deliberately never seeded as a real AgentSession: WalkAncestors'
	// climb from orphanChild fails the moment it tries to Get that parent.
	orphanChild := delegatedChild("orphan-child", "peer-with-orphan", "vanished-intermediate", "sub-class")
	peerOK := peerSession("peer-ok", "ok-class")

	h := newHarness(t, checker, readyWorkshop(), builderSession(), peerWithOrphan, orphanChild, peerOK)
	h.registerBearer("tok-workshop")

	resp := getAgentsInThread(t, h.srv.URL+workshopthreadsrv.Path, "tok-workshop")
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode,
		"one participant's dangling-parent closure walk must not 500 the whole lookup")

	out := decodeAgents(t, resp)
	names := make([]string, 0, len(out))
	for _, a := range out {
		names = append(names, a.Name)
	}
	assert.Contains(t, names, "peer-with-orphan", "the participant itself is still emitted despite its broken closure walk")
	assert.Contains(t, names, "peer-ok", "an unrelated participant's own walk is unaffected")
	assert.NotContains(t, names, "orphan-child", "a member reached only through the broken walk is never resolved as a descendant")
}

// TestGetAgentsInThread_WorkshopIDFromStatusSessionFromSpec_Invariant is the
// fail-first test for MAJOR-4: the invariant this whole route's
// authorization rests on is that workshopID comes from the OPERATOR-ONLY,
// set-once status.Namespace, while the session subject comes from the
// MUTABLE spec.session — the two are never derived from one another. Every
// OTHER fixture in this file happens to have spec.session name the very
// session the bearer's own registration key targets, so none of them can
// tell that derivation apart from a wrong one that computed workshopID FROM
// spec (e.g. WorkshopName(spec.session.Name)) instead of reading
// status.Namespace, or that resolved the session before making the check.
// Naming spec.session something else here means a wrong derivation would
// send the checker a DIFFERENT tuple than the one asserted below, and this
// test would catch it.
func TestGetAgentsInThread_WorkshopIDFromStatusSessionFromSpec_Invariant(t *testing.T) {
	checker := &fakeChecker{allow: false}
	ws := readyWorkshop()
	const otherSessionName = "a-different-builder-session"
	ws.Spec.Session = spiceboxv1alpha1.NamespacedRef{Namespace: sessNS, Name: otherSessionName}

	h := newHarness(t, checker, ws)
	h.registerBearer("tok-workshop")

	resp := getAgentsInThread(t, h.srv.URL+workshopthreadsrv.Path, "tok-workshop")
	defer resp.Body.Close()
	assert.Equal(t, http.StatusForbidden, resp.StatusCode, "build denies -> 403")

	require.Len(t, checker.calls, 1, "CheckWorkshopBuild called exactly once")
	assert.Equal(t, checkCall{wsNamespace, sessNS, otherSessionName}, checker.calls[0],
		"the route must ask about the SPEC-named session against the STATUS-named workshop id -- "+
			"workshopID from status.Namespace (operator-only), the subject from spec.session (mutable), "+
			"never derived from one another")
}
