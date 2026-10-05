package goals

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	core "github.com/authzed/openagentprimitives/pkg/agent/goals"
	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/memory"
	goalinmem "github.com/authzed/openagentprimitives/pkg/memory/goals/inmem"
	meminmem "github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/goalactor"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/infoleakagetaint"
	"github.com/authzed/openagentprimitives/pkg/memory/provenance"
	"github.com/authzed/openagentprimitives/pkg/memory/tokens"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apiext "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

type authority struct {
	denied     bool
	denySource bool
	owners     map[string]string
}

func (a *authority) Relations() authz.RelWriter { return a }
func (a *authority) WriteRelationships(_ context.Context, rels []authz.Relation) error {
	for _, r := range rels {
		a.owners[r.ResourceID] = r.SubjectID
	}
	return nil
}
func (a *authority) DeleteRelationships(context.Context, []authz.Relation) error { return nil }
func (a *authority) GrantSlots(context.Context, string, string, []authz.SlotBinding, time.Time) error {
	return nil
}
func (a *authority) CheckInteract(context.Context, string, string, identity.CanonicalUserID, bool) (bool, error) {
	return !a.denied, nil
}
func (a *authority) CheckOnResource(_ context.Context, typ, id, perm string, u identity.CanonicalUserID, _ bool) (bool, error) {
	if a.denied {
		return false, nil
	}
	if typ == ResourceType {
		return a.owners[id] == u.String(), nil
	}
	return !a.denySource, nil
}

type serverFixture struct {
	s      *Server
	auth   *authority
	mem    *memory.Local
	signed *provenance.SigningMemory
	keys   provenance.MapKeyLookup
	n      int
}

func fixture(t *testing.T) *serverFixture {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, v1.AddToScheme(scheme))
	class := &v1.AgentClass{ObjectMeta: metav1.ObjectMeta{Namespace: "team", Name: "assistant", UID: "class-uid"}, Spec: v1.AgentClassSpec{Capabilities: map[string]apiext.JSON{"goals": {Raw: []byte(`{}`)}}}}
	class.Spec.Authz = &v1.AuthzBlock{InformationLeakage: &v1.InformationLeakagePolicy{Mode: "enforcing"}}
	sess := &v1.AgentSession{ObjectMeta: metav1.ObjectMeta{Namespace: "team", Name: "session", UID: "session-uid"}, Spec: v1.AgentSessionSpec{Class: "assistant"}}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	signer := provenance.NewSigner(priv, "system:channelsd")
	keys := provenance.MapKeyLookup{{Publisher: "system:channelsd", KeyID: signer.KeyID()}: pub}
	mem := memory.NewLocal(meminmem.NewBackend(), memory.WithProvenanceVerifier(provenance.NewWriteVerifier(keys)))
	auth := &authority{owners: map[string]string{}}
	toks := tokens.NewRegistry()
	toks.Set(memory.NamespacedName{Namespace: "team", Name: "session"}, "token", "")
	toks.Set(memory.NamespacedName{Namespace: "team", Name: "foreign"}, "foreign-token", "", memory.NamespacedName{Namespace: "team", Name: "session"})
	s := &Server{ActorWait: -1, Reader: fake.NewClientBuilder().WithScheme(scheme).WithObjects(class, sess).Build(), Memory: mem, Tokens: toks, Keys: keys, Auth: auth, Service: &core.Service{Store: goalinmem.New()}}
	s.Service.Auth = s
	return &serverFixture{s: s, auth: auth, mem: mem, signed: provenance.NewSigningMemory(mem, signer), keys: keys}
}
func (f *serverFixture) proof(t *testing.T, owner string) {
	t.Helper()
	f.n++
	b, err := json.Marshal(goalactor.Content{Owner: owner, SessionUID: "session-uid", ClassUID: "class-uid"})
	require.NoError(t, err)
	ctx := memory.WithCaller(memory.WithSystemApproval(context.Background(), "system:channelsd"), "system:channelsd")
	_, err = f.signed.Put(ctx, memory.Entry{Scope: memory.Scope{Kind: "session", ID: "team/session"}, Kind: goalactor.KindName, ID: fmt.Sprintf("goalactor-%d", f.n), CreatedAt: time.Now().UTC(), Content: b})
	require.NoError(t, err)
}
func (f *serverFixture) call(t *testing.T, token string, r core.Request) (int, core.Response) {
	return f.callSession(t, token, "session", r)
}
func (f *serverFixture) callSession(t *testing.T, token, name string, r core.Request) (int, core.Response) {
	t.Helper()
	b, err := json.Marshal(r)
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/goals/team/"+name, bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	f.s.ServeHTTP(w, req)
	var res core.Response
	if w.Code == 200 {
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &res))
	}
	return w.Code, res
}
func TestServerBoundariesAndLifecycle(t *testing.T) {
	f := fixture(t)
	list := core.Request{Operation: "list"}
	status, _ := f.call(t, "token", list)
	assert.Equal(t, 404, status, "no human proof")
	f.proof(t, "alice")
	status, r := f.call(t, "token", list)
	require.Equal(t, 200, status)
	assert.False(t, r.ExecutionAvailable)
	assert.Empty(t, r.Page.Goals)
	status, _ = f.call(t, "foreign-token", list)
	assert.Equal(t, 401, status, "foreign artifact read token cannot act as this session")
	create := core.Request{Operation: "create", Resource: r.Resource, Create: core.CreateRequest{RequestID: "create", Title: "Meeting", Outcome: "Prepare agenda"}}
	spoof := create
	spoof.Resource = ResourceType + ":bob"
	status, _ = f.call(t, "token", spoof)
	assert.Equal(t, 404, status)
	status, r = f.call(t, "token", create)
	require.Equal(t, 200, status)
	goal := *r.Goal
	pending, err := f.s.Service.Store.Pending(context.Background(), 100)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	var actorEntry memory.Entry
	require.NoError(t, json.Unmarshal(pending[0].ActorProof, &actorEntry))
	require.NoError(t, provenance.VerifyEntrySignature(f.keys, actorEntry))
	status, r = f.call(t, "token", create)
	require.Equal(t, 200, status)
	assert.Equal(t, goal, *r.Goal)
	update := core.Request{Operation: "update", Resource: r.Resource, Change: core.Change{ID: goal.ID, Revision: 1, RequestID: "activate", Action: "activate"}}
	status, r = f.call(t, "token", update)
	require.Equal(t, 200, status)
	assert.Equal(t, core.Active, r.Goal.State)
	update.Change.RequestID = "stale"
	status, _ = f.call(t, "token", update)
	assert.Equal(t, 409, status)
	f.auth.denied = true
	status, _ = f.call(t, "token", create)
	assert.Equal(t, 404, status, "idempotent retries reauthorize")
	f.auth.denied = false
	// Proof remains bound to the first actor: Bob cannot select his own domain
	// using a conversation already pinned to Alice.
	f.proof(t, "bob")
	status, _ = f.call(t, "token", list)
	assert.Equal(t, 404, status)
}
func TestRetainedSourceRevocation(t *testing.T) {
	f := fixture(t)
	f.proof(t, "alice")
	ctx := memory.WithSystemApproval(context.Background(), "test")
	b, err := json.Marshal(infoleakagetaint.TaintRecord{ResourceType: "document", ResourceID: "secret", Permission: "view"})
	require.NoError(t, err)
	// Session taints use a session key, matching the production runner.
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	signer := provenance.NewSigner(priv, "session:team/session")
	f.keys[provenance.PubKeyRef{Publisher: signer.Publisher(), KeyID: signer.KeyID()}] = pub
	_, err = provenance.NewSigningMemory(f.mem, signer).Put(ctx, memory.Entry{Scope: memory.Scope{Kind: "session", ID: "team/session"}, Kind: infoleakagetaint.KindName, ID: "ilt-source", CreatedAt: time.Now().UTC(), Content: b})
	require.NoError(t, err)
	status, r := f.call(t, "token", core.Request{Operation: "list"})
	require.Equal(t, 200, status)
	create := core.Request{Operation: "create", Resource: r.Resource, Create: core.CreateRequest{RequestID: "sourced", Title: "Private", Outcome: "Uses document"}}
	status, r = f.call(t, "token", create)
	require.Equal(t, 200, status)
	id := r.Goal.ID
	require.Len(t, r.Goal.Sources, 1)
	f.auth.denySource = true
	status, _ = f.call(t, "token", core.Request{Operation: "get", ID: id})
	assert.Equal(t, 404, status)
	status, r = f.call(t, "token", core.Request{Operation: "list"})
	require.Equal(t, 200, status)
	assert.Empty(t, r.Page.Goals)
	status, _ = f.call(t, "token", create)
	assert.Equal(t, 404, status)
}

func TestGoalsSurviveNewSessionWithTheSameHumanAndClass(t *testing.T) {
	f := fixture(t)
	f.proof(t, "alice")
	status, r := f.call(t, "token", core.Request{Operation: "list"})
	require.Equal(t, 200, status)
	status, r = f.call(t, "token", core.Request{Operation: "create", Resource: r.Resource, Create: core.CreateRequest{RequestID: "durable", Title: "Agenda", Outcome: "Prepare meeting"}})
	require.Equal(t, 200, status)
	g := *r.Goal
	sess := &v1.AgentSession{ObjectMeta: metav1.ObjectMeta{Namespace: "team", Name: "second", UID: "second-uid"}, Spec: v1.AgentSessionSpec{Class: "assistant"}}
	require.NoError(t, f.s.Reader.(client.Client).Create(context.Background(), sess))
	f.s.Tokens.Set(memory.NamespacedName{Namespace: "team", Name: "second"}, "second-token", "")
	require.NoError(t, goalactor.Record(context.Background(), f.signed, memory.Scope{Kind: "session", ID: "team/second"}, goalactor.Content{Owner: "alice", SessionUID: "second-uid", ClassUID: "class-uid"}))
	status, r = f.callSession(t, "second-token", "second", core.Request{Operation: "get", ID: g.ID})
	require.Equal(t, 200, status)
	assert.Equal(t, g, *r.Goal)
	// An identically named, recreated class cannot inherit the old domain.
	var class v1.AgentClass
	require.NoError(t, f.s.Reader.Get(context.Background(), client.ObjectKey{Namespace: "team", Name: "assistant"}, &class))
	class.UID = "replacement-uid"
	require.NoError(t, f.s.Reader.(client.Client).Update(context.Background(), &class))
	status, _ = f.callSession(t, "second-token", "second", core.Request{Operation: "get", ID: g.ID})
	assert.Equal(t, 404, status)
}

func TestColdRegistryGrace(t *testing.T) {
	f := fixture(t)
	f.s.ColdRegistryUntil = time.Now().Add(time.Minute)
	for _, tc := range []struct {
		token string
		want  int
	}{
		{"unknown-token", http.StatusServiceUnavailable},
		{"", http.StatusUnauthorized},
		{"foreign-token", http.StatusUnauthorized},
	} {
		t.Run(tc.token, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/goals/team/session", bytes.NewBufferString(`{"operation":"list"}`))
			r.Header.Set("Authorization", "Bearer "+tc.token)
			w := httptest.NewRecorder()
			f.s.ServeHTTP(w, r)
			assert.Equal(t, tc.want, w.Code)
			if tc.want == http.StatusServiceUnavailable {
				assert.Equal(t, "1", w.Header().Get("Retry-After"))
			}
		})
	}
	f.s.ColdRegistryUntil = time.Now().Add(-time.Second)
	r := httptest.NewRequest(http.MethodPost, "/goals/team/session", bytes.NewBufferString(`{"operation":"list"}`))
	r.Header.Set("Authorization", "Bearer unknown-token")
	w := httptest.NewRecorder()
	f.s.ServeHTTP(w, r)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}
