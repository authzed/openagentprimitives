package workshopprojectsrv_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	_ "github.com/authzed/openagentprimitives/pkg/agent/harness/apnative" // register the default harness for TestProjectAgent_UnresolvableSkillDropped_ResolvableOneKeptAndReachesValid's real Reconcile call
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkey"
	"github.com/authzed/openagentprimitives/pkg/controllers/agentclass"
	"github.com/authzed/openagentprimitives/pkg/controllers/testfixtures"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/tokens"
	"github.com/authzed/openagentprimitives/pkg/web/workshopprojectsrv"
)

// fakeChecker mirrors workshopthreadsrv_test.go's own — this route re-runs
// the identical ResolveAgentsInThread join, so it needs the same shape of
// fake to prove the SAME workshop:build tuple gets re-checked live.
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

func threadBinding() *spiceboxv1alpha1.ChannelBinding {
	return &spiceboxv1alpha1.ChannelBinding{
		Name:     channelName,
		Kind:     channelKind,
		Key:      channelKey,
		External: map[string]string{"channel_id": channelID, "thread_ts": threadTS},
	}
}

func threadLabels() map[string]string {
	return map[string]string{
		spiceboxv1alpha1.LabelChannelName: channelName,
		spiceboxv1alpha1.LabelChannelKey:  channelkey.LabelValue(channelKey),
	}
}

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

// peerSession is another agent bound to the SAME thread as the builder,
// naming class as its AgentClass — this is what makes class REACHABLE via
// the agents-in-thread join this route reuses.
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

// minimalPeerClass is a bare-bones AgentClass a peer session names — just
// enough to be Gettable, with an inline prompt and one skill so the
// projection's positive-path assertions have something to check.
func minimalPeerClass(name string) *spiceboxv1alpha1.AgentClass {
	return &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Namespace: sessNS, Name: name},
		Spec: spiceboxv1alpha1.AgentClassSpec{
			DisplayName:  "Support Agent",
			Description:  "Answers support questions in this thread.",
			SystemPrompt: spiceboxv1alpha1.PromptSource{Inline: "you are a helpful support agent"},
			Skills: []spiceboxv1alpha1.AgentSkill{
				{Name: "triage", Ref: "local//triage@v1"},
			},
		},
	}
}

// fullyLoadedPeerClass is a peer's AgentClass declaring EVERY field the
// whitelist must never copy — agentIdentity, mcpServers, toolBundles,
// capabilities, and authz — alongside the ones it must. This is the
// fixture for TestProjectAgent_WhitelistExcludesEverythingElse: if
// projectStandin ever copies a single one of these across, this fixture is
// what catches it.
func fullyLoadedPeerClass(name string) *spiceboxv1alpha1.AgentClass {
	ac := minimalPeerClass(name)
	ac.Spec.AgentIdentity = "peer-identity"
	ac.Spec.MCPServers = []spiceboxv1alpha1.AgentClassMCPServerRef{
		{Name: "peer-mcp", Ref: "peer-mcp-server"},
	}
	ac.Spec.ToolBundles = []spiceboxv1alpha1.ToolBundle{
		{Name: "peer-bundle", Class: "peer-spicebox-class", Toolspecs: []string{"peer-toolspec"}},
	}
	ac.Spec.Capabilities = map[string]apiextensionsv1.JSON{
		"agent_builder": {Raw: []byte(`{}`)},
	}
	ac.Spec.Authz = &spiceboxv1alpha1.AuthzBlock{
		Slots: []spiceboxv1alpha1.AuthzSlot{
			{ResourceType: "peer_resource", Description: "a peer-only slot", Permission: "use"},
		},
	}
	ac.Spec.Subagents = []string{"some-other-peer-class"}
	return ac
}

// triageSkillCanonical is minimalPeerClass/fullyLoadedPeerClass's own
// skill's Ref — resolvableSkillInWorkshop mints a Skill CR matching it, in
// the WORKSHOP namespace rather than the peer source's own, since an
// AgentSkill.Ref resolves against a Skill/ClusterSkill in the REFERENCING
// class's own namespace (MAJOR-1): a stand-in projected into wsNamespace
// needs its own Skill there, not one that merely exists for the source in
// sessNS.
const triageSkillCanonical = "local//triage@v1"

// resolvableSkillInWorkshop is a namespace Skill, Valid=True, materialized
// in the WORKSHOP namespace under triageSkillCanonical — what makes
// minimalPeerClass's one skill actually resolvable for a stand-in projected
// there (MAJOR-1's positive case).
func resolvableSkillInWorkshop() *spiceboxv1alpha1.Skill {
	return &spiceboxv1alpha1.Skill{
		ObjectMeta: metav1.ObjectMeta{Name: "triage", Namespace: wsNamespace},
		Spec:       spiceboxv1alpha1.SkillSpec{CanonicalName: triageSkillCanonical, Description: "triage", Body: "body"},
		Status: spiceboxv1alpha1.SkillStatus{
			Conditions: []metav1.Condition{{
				Type: spiceboxv1alpha1.SkillConditionValid, Status: metav1.ConditionTrue, Reason: "Test",
			}},
		},
	}
}

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
		// AgentClass is registered here too (not just Workshop) so a test can
		// hand this same client to a real agentclass.Reconciler and observe a
		// genuine Status().Update — MAJOR-1's own test proves a projected
		// stand-in actually reaches Valid=True, not merely that it exists.
		WithStatusSubresource(&spiceboxv1alpha1.Workshop{}, &spiceboxv1alpha1.AgentClass{}).
		Build()
	reg := tokens.NewRegistry()

	var handler http.Handler
	if checker == nil {
		handler = workshopprojectsrv.NewHandler(c, reg, nil, logr.Discard())
	} else {
		handler = workshopprojectsrv.NewHandler(c, reg, checker, logr.Discard())
	}
	h := harness{srv: httptest.NewServer(handler), c: c, reg: reg, checker: checker}
	t.Cleanup(h.srv.Close)
	return &h
}

func (h *harness) registerBearer(token string) {
	h.reg.Set(memory.NamespacedName{Namespace: sessNS, Name: spiceboxv1alpha1.WorkshopName(sessName)}, token, "")
}

func postProjectAgent(t *testing.T, url, token, namespace, name string) *http.Response {
	t.Helper()
	var body []byte
	if namespace != "" || name != "" {
		var err error
		body, err = json.Marshal(map[string]string{"namespace": namespace, "name": name})
		require.NoError(t, err, "marshal request body")
	}
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	require.NoError(t, err, "NewRequest")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err, "Do")
	return resp
}

func decodeProjectResponse(t *testing.T, resp *http.Response) map[string]string {
	t.Helper()
	var out map[string]string
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out), "decode response body")
	return out
}

func TestProjectAgent_NoBearerHeader_401(t *testing.T) {
	checker := &fakeChecker{allow: true}
	h := newHarness(t, checker, readyWorkshop(), builderSession())

	resp := postProjectAgent(t, h.srv.URL+workshopprojectsrv.Path, "", sessNS, "support-class")
	defer resp.Body.Close()
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	assert.Empty(t, checker.calls, "an unauthenticated request never reaches the permission check")
}

func TestProjectAgent_UnknownBearer_401(t *testing.T) {
	checker := &fakeChecker{allow: true}
	h := newHarness(t, checker, readyWorkshop(), builderSession())
	// No bearer registered at all.

	resp := postProjectAgent(t, h.srv.URL+workshopprojectsrv.Path, "bogus-token", sessNS, "support-class")
	defer resp.Body.Close()
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

func TestProjectAgent_WorkshopNotReady_403(t *testing.T) {
	checker := &fakeChecker{allow: true}
	h := newHarness(t, checker, provisioningWorkshop())
	h.registerBearer("tok-workshop")

	resp := postProjectAgent(t, h.srv.URL+workshopprojectsrv.Path, "tok-workshop", sessNS, "support-class")
	defer resp.Body.Close()
	assert.Equal(t, http.StatusForbidden, resp.StatusCode, "not-Ready workshop denies")
	assert.Empty(t, checker.calls, "never reaches the permission check for a not-yet-provisioned workshop")
}

func TestProjectAgent_NilChecker_503DeniesWithoutPanicking(t *testing.T) {
	h := newHarness(t, nil, readyWorkshop(), builderSession())
	h.registerBearer("tok-workshop")

	assert.NotPanics(t, func() {
		resp := postProjectAgent(t, h.srv.URL+workshopprojectsrv.Path, "tok-workshop", sessNS, "support-class")
		defer resp.Body.Close()
		assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode, "an unwired checker denies, it does not panic")
	})
}

// TestProjectAgent_ClassNotReachable_403 is the fail-first test for Ruling
// A: a class that exists (Gettable) but was never surfaced as a thread
// participant or its descendant must be refused, even though nothing about
// the request itself is malformed. See this file's own report for the
// break-it-first run against a version of the handler with the
// reachability check bypassed.
func TestProjectAgent_ClassNotReachable_403(t *testing.T) {
	checker := &fakeChecker{allow: true}
	unreachableClass := minimalPeerClass("unreachable-class")
	h := newHarness(t, checker, readyWorkshop(), builderSession(), unreachableClass)
	h.registerBearer("tok-workshop")

	resp := postProjectAgent(t, h.srv.URL+workshopprojectsrv.Path, "tok-workshop", sessNS, "unreachable-class")
	defer resp.Body.Close()
	assert.Equal(t, http.StatusForbidden, resp.StatusCode,
		"a class that exists but is not in this builder's own agents-in-thread result must be refused")

	// Confirm no stand-in was created for the refused class.
	var list spiceboxv1alpha1.AgentClassList
	require.NoError(t, h.c.List(context.Background(), &list, client.InNamespace(wsNamespace)))
	assert.Empty(t, list.Items, "a refused projection must not create anything")
}

func TestProjectAgent_ReachableClass_ProjectsPromptAndSkills(t *testing.T) {
	checker := &fakeChecker{allow: true}
	peerClass := minimalPeerClass("support-class")
	h := newHarness(t, checker,
		readyWorkshop(), builderSession(),
		peerSession("peer-agent-a", "support-class"),
		peerClass, resolvableSkillInWorkshop(),
	)
	h.registerBearer("tok-workshop")

	resp := postProjectAgent(t, h.srv.URL+workshopprojectsrv.Path, "tok-workshop", sessNS, "support-class")
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	out := decodeProjectResponse(t, resp)
	wantName := "support-class"
	require.Equal(t, wantName, out["name"], "the stand-in is named EXACTLY what it stands in for — the source class's own bare name (Ruling C) — so the roster reads correctly on both sides of an install")

	var standin spiceboxv1alpha1.AgentClass
	require.NoError(t, h.c.Get(context.Background(), types.NamespacedName{Namespace: wsNamespace, Name: wantName}, &standin))

	assert.Equal(t, wsNamespace, standin.Namespace, "the stand-in lives IN the workshop namespace, an ordinary same-namespace AgentClass")
	assert.Equal(t, peerClass.Spec.DisplayName, standin.Spec.DisplayName)
	assert.Equal(t, peerClass.Spec.SystemPrompt, standin.Spec.SystemPrompt, "system prompt is carried over verbatim")
	assert.Equal(t, peerClass.Spec.Skills, standin.Spec.Skills, "skills are carried over by value")
	assert.Contains(t, standin.Spec.Description, "STAND-IN", "description's first sentence marks this as a stand-in")
	assert.Contains(t, standin.Spec.Description, "holds no credentials", "description's first sentence says it holds no credentials")
	assert.Equal(t, sessNS+"/support-class", standin.Annotations[workshopprojectsrv.AnnotationStandinSource],
		"a provenance annotation names the exact source {namespace, name}")
}

// TestProjectAgent_WhitelistExcludesEverythingElse is the test that matters
// most (Task 1's Global Constraints): a source class declaring
// agentIdentity, mcpServers, toolBundles, capabilities and authz must
// produce a stand-in with NONE of them. Written so it fails if ANY single
// one of those fields leaked. See this file's own report for the
// break-it-first run against a version of projectStandin that copies one
// of them across.
func TestProjectAgent_WhitelistExcludesEverythingElse(t *testing.T) {
	checker := &fakeChecker{allow: true}
	peerClass := fullyLoadedPeerClass("loaded-class")
	h := newHarness(t, checker,
		readyWorkshop(), builderSession(),
		peerSession("peer-agent-a", "loaded-class"),
		peerClass, resolvableSkillInWorkshop(),
	)
	h.registerBearer("tok-workshop")

	resp := postProjectAgent(t, h.srv.URL+workshopprojectsrv.Path, "tok-workshop", sessNS, "loaded-class")
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	out := decodeProjectResponse(t, resp)
	var standin spiceboxv1alpha1.AgentClass
	require.NoError(t, h.c.Get(context.Background(), types.NamespacedName{Namespace: wsNamespace, Name: out["name"]}, &standin))

	// PROJECTED: the stand-in still carries the prompt and skills.
	assert.Equal(t, peerClass.Spec.SystemPrompt, standin.Spec.SystemPrompt)
	assert.Equal(t, peerClass.Spec.Skills, standin.Spec.Skills)

	// NEVER PROJECTED — the whole point of this test.
	assert.Empty(t, standin.Spec.AgentIdentity, "agentIdentity must never be copied onto a stand-in")
	assert.Empty(t, standin.Spec.MCPServers, "mcpServers must never be copied onto a stand-in")
	assert.Empty(t, standin.Spec.ToolBundles, "toolBundles must never be copied onto a stand-in")
	assert.Empty(t, standin.Spec.Capabilities, "capabilities must never be copied onto a stand-in")
	assert.Nil(t, standin.Spec.Authz, "authz must never be copied onto a stand-in")
	assert.Empty(t, standin.Spec.Subagents, "the roster must never be copied onto a stand-in (dropped, not rewritten)")
	assert.Empty(t, standin.Spec.SidecarToolboxes)
	assert.Nil(t, standin.Spec.Channels)
	assert.Nil(t, standin.Spec.Model)
	assert.Nil(t, standin.Spec.Budget)
	assert.Nil(t, standin.Spec.WorkspaceSource)
	assert.Nil(t, standin.Spec.AgentUI)
}

// TestProjectAgent_ReprojectingSameSource_UpdatesInPlace covers this
// route's own write discipline (mirroring workshopdraftsrv's digest-keyed
// idempotency): projecting the SAME source class twice must converge on
// the SAME stand-in object rather than accumulating a second one, and must
// pick up a changed source description on the second call.
func TestProjectAgent_ReprojectingSameSource_UpdatesInPlace(t *testing.T) {
	checker := &fakeChecker{allow: true}
	peerClass := minimalPeerClass("support-class")
	h := newHarness(t, checker,
		readyWorkshop(), builderSession(),
		peerSession("peer-agent-a", "support-class"),
		peerClass,
	)
	h.registerBearer("tok-workshop")

	resp1 := postProjectAgent(t, h.srv.URL+workshopprojectsrv.Path, "tok-workshop", sessNS, "support-class")
	defer resp1.Body.Close()
	require.Equal(t, http.StatusOK, resp1.StatusCode)
	name1 := decodeProjectResponse(t, resp1)["name"]

	// Mutate the source class's description and re-project.
	var liveSource spiceboxv1alpha1.AgentClass
	require.NoError(t, h.c.Get(context.Background(), types.NamespacedName{Namespace: sessNS, Name: "support-class"}, &liveSource))
	liveSource.Spec.Description = "an updated description"
	require.NoError(t, h.c.Update(context.Background(), &liveSource))

	resp2 := postProjectAgent(t, h.srv.URL+workshopprojectsrv.Path, "tok-workshop", sessNS, "support-class")
	defer resp2.Body.Close()
	require.Equal(t, http.StatusOK, resp2.StatusCode)
	name2 := decodeProjectResponse(t, resp2)["name"]
	require.Equal(t, name1, name2, "re-projecting the same source class converges on the same stand-in identity")

	var list spiceboxv1alpha1.AgentClassList
	require.NoError(t, h.c.List(context.Background(), &list, client.InNamespace(wsNamespace)))
	require.Len(t, list.Items, 1, "no second object accumulates")

	var standin spiceboxv1alpha1.AgentClass
	require.NoError(t, h.c.Get(context.Background(), types.NamespacedName{Namespace: wsNamespace, Name: name1}, &standin))
	assert.Contains(t, standin.Spec.Description, "an updated description", "the second projection picks up the changed source")
}

// TestProjectAgent_TwoDifferentSources_BothProjectIndependently projects
// two DIFFERENT reachable sources that happen to share no name, to confirm
// two independent stand-ins coexist in W without colliding with each
// other — the ordinary case the collision rule (Ruling D) must not get in
// the way of.
func TestProjectAgent_TwoDifferentSources_BothProjectIndependently(t *testing.T) {
	checker := &fakeChecker{allow: true}
	classA := minimalPeerClass("support-class")
	classB := minimalPeerClass("ops-class")
	classB.Spec.DisplayName = "Ops Agent"
	h := newHarness(t, checker,
		readyWorkshop(), builderSession(),
		peerSession("peer-agent-a", "support-class"),
		peerSession("peer-agent-b", "ops-class"),
		classA, classB,
	)
	h.registerBearer("tok-workshop")

	respA := postProjectAgent(t, h.srv.URL+workshopprojectsrv.Path, "tok-workshop", sessNS, "support-class")
	defer respA.Body.Close()
	require.Equal(t, http.StatusOK, respA.StatusCode)

	respB := postProjectAgent(t, h.srv.URL+workshopprojectsrv.Path, "tok-workshop", sessNS, "ops-class")
	defer respB.Body.Close()
	require.Equal(t, http.StatusOK, respB.StatusCode)

	var list spiceboxv1alpha1.AgentClassList
	require.NoError(t, h.c.List(context.Background(), &list, client.InNamespace(wsNamespace)))
	require.Len(t, list.Items, 2, "two independently-named stand-ins coexist without colliding")
}

// TestProjectAgent_NameCollidesWithBuilderAuthoredClass_409Refused is the
// fail-first test for Ruling D: the bare-name design (Ruling C) means a
// stand-in's name can collide with something the workshop's builder
// authored itself. That must be refused, never overwritten. Seeds an
// AgentClass in W, at the EXACT identity the projection would use, that
// carries NO AnnotationStandinSource — i.e. something the builder wrote,
// not a prior stand-in.
func TestProjectAgent_NameCollidesWithBuilderAuthoredClass_409Refused(t *testing.T) {
	checker := &fakeChecker{allow: true}
	peerClass := minimalPeerClass("support-class")
	builderAuthored := &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Namespace: wsNamespace, Name: "support-class"},
		Spec: spiceboxv1alpha1.AgentClassSpec{
			DisplayName:  "The Builder's Own Draft",
			SystemPrompt: spiceboxv1alpha1.PromptSource{Inline: "this is real, authored work"},
		},
	}
	h := newHarness(t, checker,
		readyWorkshop(), builderSession(),
		peerSession("peer-agent-a", "support-class"),
		peerClass, builderAuthored,
	)
	h.registerBearer("tok-workshop")

	resp := postProjectAgent(t, h.srv.URL+workshopprojectsrv.Path, "tok-workshop", sessNS, "support-class")
	defer resp.Body.Close()
	assert.Equal(t, http.StatusConflict, resp.StatusCode,
		"a bare name colliding with something the builder authored itself must be refused, not overwritten")

	var stillThere spiceboxv1alpha1.AgentClass
	require.NoError(t, h.c.Get(context.Background(), types.NamespacedName{Namespace: wsNamespace, Name: "support-class"}, &stillThere))
	assert.Equal(t, "The Builder's Own Draft", stillThere.Spec.DisplayName, "the builder's own authored class must be completely untouched")
	assert.Equal(t, builderAuthored.Spec.SystemPrompt, stillThere.Spec.SystemPrompt, "never clobbered by the projection attempt")
}

// TestProjectAgent_ReprojectingSameSource_IsIdempotentNotACollision proves
// the OTHER side of Ruling D: re-projecting the SAME source (which already
// left its own stand-in, carrying AnnotationStandinSource, at that bare
// name) is an ordinary idempotent update, never a 409 — the collision rule
// only fires against a NON-stand-in occupant.
func TestProjectAgent_ReprojectingSameSource_IsIdempotentNotACollision(t *testing.T) {
	checker := &fakeChecker{allow: true}
	peerClass := minimalPeerClass("support-class")
	h := newHarness(t, checker,
		readyWorkshop(), builderSession(),
		peerSession("peer-agent-a", "support-class"),
		peerClass,
	)
	h.registerBearer("tok-workshop")

	resp1 := postProjectAgent(t, h.srv.URL+workshopprojectsrv.Path, "tok-workshop", sessNS, "support-class")
	defer resp1.Body.Close()
	require.Equal(t, http.StatusOK, resp1.StatusCode)

	resp2 := postProjectAgent(t, h.srv.URL+workshopprojectsrv.Path, "tok-workshop", sessNS, "support-class")
	defer resp2.Body.Close()
	require.Equal(t, http.StatusOK, resp2.StatusCode, "re-projecting the same source hits its OWN prior stand-in, not a collision")
}

func TestProjectAgent_MissingNamespaceOrName_400(t *testing.T) {
	checker := &fakeChecker{allow: true}
	h := newHarness(t, checker, readyWorkshop(), builderSession())
	h.registerBearer("tok-workshop")

	resp := postProjectAgent(t, h.srv.URL+workshopprojectsrv.Path, "tok-workshop", "", "")
	defer resp.Body.Close()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestProjectAgent_CheckErrors_403(t *testing.T) {
	checker := &fakeChecker{err: errors.New("spicedb unavailable")}
	h := newHarness(t, checker, readyWorkshop(), builderSession())
	h.registerBearer("tok-workshop")

	resp := postProjectAgent(t, h.srv.URL+workshopprojectsrv.Path, "tok-workshop", sessNS, "support-class")
	defer resp.Body.Close()
	assert.Equal(t, http.StatusForbidden, resp.StatusCode, "check error is fail-closed, not fail-open")
}

// TestProjectAgent_SourceHasConfigMapPrompt_UnprocessableEntity:
// a reachable source whose prompt sources from a ConfigMap would project a
// stand-in that can never resolve it (the ConfigMap lives in the SOURCE's
// own namespace, not the workshop's) — refused outright, naming the reason,
// rather than minting a stand-in that cannot work.
func TestProjectAgent_SourceHasConfigMapPrompt_UnprocessableEntity(t *testing.T) {
	checker := &fakeChecker{allow: true}
	peerClass := minimalPeerClass("support-class")
	peerClass.Spec.SystemPrompt = spiceboxv1alpha1.PromptSource{
		ConfigMapRef: &spiceboxv1alpha1.ConfigMapKeyRef{Name: "prompt-cm", Key: "prompt.txt"},
	}
	h := newHarness(t, checker,
		readyWorkshop(), builderSession(),
		peerSession("peer-agent-a", "support-class"),
		peerClass,
	)
	h.registerBearer("tok-workshop")

	resp := postProjectAgent(t, h.srv.URL+workshopprojectsrv.Path, "tok-workshop", sessNS, "support-class")
	defer resp.Body.Close()
	assert.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode,
		"a source prompting from a ConfigMap must be refused rather than projected as an unresolvable stand-in")

	var list spiceboxv1alpha1.AgentClassList
	require.NoError(t, h.c.List(context.Background(), &list, client.InNamespace(wsNamespace)))
	assert.Empty(t, list.Items, "a refused projection must not create anything")
}

// TestProjectAgent_UnresolvableSkillDropped_ResolvableOneKeptAndReachesValid
// is MAJOR-1's own test: a source with one skill resolvable in the WORKSHOP
// namespace and one that is not must yield a stand-in carrying EXACTLY the
// resolvable one — and, unlike a byte-identical copy of every skill (which
// would park the stand-in Valid=False/AgentClassSkillMissing forever, per
// this package's own doc), the projected stand-in must actually reach
// Valid=True when reconciled for real. See this file's own report for the
// break-it-first run against a version of the route that still copies
// every skill verbatim.
func TestProjectAgent_UnresolvableSkillDropped_ResolvableOneKeptAndReachesValid(t *testing.T) {
	checker := &fakeChecker{allow: true}
	peerClass := minimalPeerClass("support-class")
	peerClass.Spec.Skills = append(peerClass.Spec.Skills,
		spiceboxv1alpha1.AgentSkill{Name: "unresolvable", Ref: "local//nowhere@v1"})
	h := newHarness(t, checker,
		readyWorkshop(), builderSession(),
		peerSession("peer-agent-a", "support-class"),
		peerClass, resolvableSkillInWorkshop(),
	)
	h.registerBearer("tok-workshop")

	resp := postProjectAgent(t, h.srv.URL+workshopprojectsrv.Path, "tok-workshop", sessNS, "support-class")
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	out := decodeProjectResponse(t, resp)

	var standin spiceboxv1alpha1.AgentClass
	require.NoError(t, h.c.Get(context.Background(), types.NamespacedName{Namespace: wsNamespace, Name: out["name"]}, &standin))

	require.Len(t, standin.Spec.Skills, 1, "exactly the resolvable skill survives projection")
	assert.Equal(t, "local//triage@v1", standin.Spec.Skills[0].Ref, "the resolvable skill is the one kept")
	assert.Contains(t, standin.Spec.Description, "local//nowhere@v1",
		"the stand-in's own description names what was dropped, so a builder reading it is not misled")

	// The whole point of dropping rather than copying verbatim: the
	// projected stand-in must actually be able to START, not merely exist.
	r := &agentclass.Reconciler{Client: h.c, AllowTestProvider: true}
	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: wsNamespace, Name: out["name"]},
	})
	require.NoError(t, err, "Reconcile must not itself error")

	var reconciled spiceboxv1alpha1.AgentClass
	require.NoError(t, h.c.Get(context.Background(), types.NamespacedName{Namespace: wsNamespace, Name: out["name"]}, &reconciled))
	cond := findAgentClassValidCondition(&reconciled)
	require.NotNil(t, cond, "Valid condition must be set")
	assert.Equal(t, metav1.ConditionTrue, cond.Status,
		"a stand-in that dropped its unresolvable skill must reach Valid=True — reason=%s msg=%q", cond.Reason, cond.Message)
}

// findAgentClassValidCondition is a tiny local helper — this package does
// not otherwise depend on pkg/controllers/conditions' typed accessors.
func findAgentClassValidCondition(ac *spiceboxv1alpha1.AgentClass) *metav1.Condition {
	for i := range ac.Status.Conditions {
		if ac.Status.Conditions[i].Type == spiceboxv1alpha1.AgentClassConditionValid {
			return &ac.Status.Conditions[i]
		}
	}
	return nil
}
