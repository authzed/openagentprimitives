package relationshipsource

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	slackapi "github.com/slack-go/slack"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	v1 "github.com/authzed/authzed-go/proto/authzed/api/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/adoptguard"
	"github.com/authzed/openagentprimitives/pkg/authz/spicedb"
	"github.com/authzed/openagentprimitives/pkg/authz/spicedb/relsource"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/controllers/conditions"
	"github.com/authzed/openagentprimitives/pkg/platform/relsync"

	// Registers the static/oauth/federated credkind Kinds: resolveCreds's
	// credkindregistry.Get dispatch resolves in this package's (untagged)
	// test binary.
	_ "github.com/authzed/openagentprimitives/pkg/platform/identity/credkind/imports"
)

// -----------------------------------------------------------------------
// relsync.Kind fixture
// -----------------------------------------------------------------------

// fakeKind is a relsync.Kind fixture: pages served off a slice, addressed by
// CURSOR TOKEN (a decimal page index) rather than call count, mirroring
// pkg/platform/relsync/sync_test.go's own fakePassKind — the zero Cursor
// always serves pages[0], so a fresh relsync.Pass (every reconcile restarts
// enumeration from Cursor{}) sees the same page[0] every time, exactly like
// production pagination.
type fakeKind struct {
	mu sync.Mutex

	name   string
	source relsource.Source

	pages     []relsync.ScopePage
	members   map[relsync.ScopeID][]spicedb.Tuple
	fetchErrs map[relsync.ScopeID]error
	// listErr, when set, makes ListScopes fail outright regardless of
	// cursor/pages — the "revoked token, missing_scope, network outage"
	// shape TestReconcile_TotalEnumerationFailureSetsReadyFalse drives.
	listErr error
	// joinMisses lets a test set relsync.ScopeContent.JoinMisses per scope,
	// so PassResult.JoinMisses is reachable through a real Reconcile — fix
	// round 1 (IMPORTANT 2): before this field existed nothing could drive
	// the join-miss count-change rule end to end, so a regression collapsing
	// it to "emit whenever nonzero" (the alert-storm shape the brief warns
	// against) would have passed the whole suite.
	joinMisses map[relsync.ScopeID]int

	fetchCalls []relsync.ScopeID

	// listCreds records the SourceParams every ListScopes call actually
	// received — task 4b's TestReconcile_PassesTheCRsBaseURLToTheKind
	// asserts on SourceParams.Endpoint here, since the endpoint has no
	// separate observable effect through this fixture's own behavior.
	listCreds []relsync.SourceParams
}

func (k *fakeKind) Name() string             { return k.name }
func (k *fakeKind) Source() relsource.Source { return k.source }

func (k *fakeKind) ListScopes(_ context.Context, creds relsync.SourceParams, after relsync.Cursor) (relsync.ScopePage, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.listCreds = append(k.listCreds, creds)
	if k.listErr != nil {
		return relsync.ScopePage{}, k.listErr
	}
	idx := 0
	if after != (relsync.Cursor{}) {
		n, err := strconv.Atoi(after.Token)
		if err != nil {
			return relsync.ScopePage{}, fmt.Errorf("fakeKind: bad cursor token %q: %w", after.Token, err)
		}
		idx = n
	}
	if idx >= len(k.pages) {
		return relsync.ScopePage{Complete: true}, nil
	}
	return k.pages[idx], nil
}

func (k *fakeKind) FetchScope(_ context.Context, _ relsync.SourceParams, s relsync.Scope) (relsync.ScopeContent, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.fetchCalls = append(k.fetchCalls, s.ID)
	if err, ok := k.fetchErrs[s.ID]; ok {
		return relsync.ScopeContent{}, err
	}
	return relsync.ScopeContent{Tuples: k.members[s.ID], JoinMisses: k.joinMisses[s.ID]}, nil
}

func (k *fakeKind) fetchedIDs() []relsync.ScopeID {
	k.mu.Lock()
	defer k.mu.Unlock()
	out := make([]relsync.ScopeID, len(k.fetchCalls))
	copy(out, k.fetchCalls)
	return out
}

func (k *fakeKind) receivedCreds() []relsync.SourceParams {
	k.mu.Lock()
	defer k.mu.Unlock()
	out := make([]relsync.SourceParams, len(k.listCreds))
	copy(out, k.listCreds)
	return out
}

// -----------------------------------------------------------------------
// SpiceDBClient / RelWriter / Reader fixtures
// -----------------------------------------------------------------------

// fakeSpiceDB is the Reconciler's SpiceDBClient fixture: Writer records
// which relsource.Source it was bound to on every call, so
// TestReconcile_BindsTheWriterToTheKindsOwnSource can assert on it directly,
// rather than on some downstream effect that would pass even if the wrong
// source were bound.
type fakeSpiceDB struct {
	mu          sync.Mutex
	writerCalls []relsource.Source

	writer spicedb.RelWriter
	reader relsync.Reader
}

func (f *fakeSpiceDB) Writer(src relsource.Source) spicedb.RelWriter {
	f.mu.Lock()
	f.writerCalls = append(f.writerCalls, src)
	f.mu.Unlock()
	return f.writer
}

func (f *fakeSpiceDB) ReadRelationships(ctx context.Context, req *v1.ReadRelationshipsRequest) (v1.PermissionsService_ReadRelationshipsClient, error) {
	return f.reader.ReadRelationships(ctx, req)
}

// fakeRelWriter records every WriteRelationships/DeleteRelationships call
// verbatim, and stubs the three unguarded pass-through methods
// spicedb.RelWriter also declares (relsync.Pass never calls them).
type fakeRelWriter struct {
	mu sync.Mutex

	writes  []*v1.WriteRelationshipsRequest
	deletes []*v1.DeleteRelationshipsRequest

	// writeErr, when non-nil, is returned by every WriteRelationships call
	// INSTEAD of recording it — fix round 1 (IMPORTANT 4): lets a test drive
	// a guard refusal or a CAS/precondition failure through a real
	// Reconcile exactly as relWriter.WriteRelationships (or a real SpiceDB
	// write) would return them, so a future change to processScope's
	// "write: %w" wrap, or to what WriteRelationships returns, actually
	// breaks a test instead of only the isolated classifyScopeError ones.
	writeErr error
}

func (w *fakeRelWriter) WriteRelationships(_ context.Context, req *v1.WriteRelationshipsRequest) (*v1.WriteRelationshipsResponse, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.writeErr != nil {
		return nil, w.writeErr
	}
	w.writes = append(w.writes, req)
	return &v1.WriteRelationshipsResponse{}, nil
}

func (w *fakeRelWriter) DeleteRelationships(_ context.Context, req *v1.DeleteRelationshipsRequest) (*v1.DeleteRelationshipsResponse, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.deletes = append(w.deletes, req)
	return &v1.DeleteRelationshipsResponse{}, nil
}

func (w *fakeRelWriter) CheckBulkPermissions(_ context.Context, _ *v1.CheckBulkPermissionsRequest, _ ...grpc.CallOption) (*v1.CheckBulkPermissionsResponse, error) {
	return &v1.CheckBulkPermissionsResponse{}, nil
}

func (w *fakeRelWriter) LookupSubjects(_ context.Context, _ *v1.LookupSubjectsRequest, _ ...grpc.CallOption) (v1.PermissionsService_LookupSubjectsClient, error) {
	return nil, nil
}

func (w *fakeRelWriter) ExpandPermissionTree(_ context.Context, _ *v1.ExpandPermissionTreeRequest, _ ...grpc.CallOption) (*v1.ExpandPermissionTreeResponse, error) {
	return &v1.ExpandPermissionTreeResponse{}, nil
}

// fakeReadStream is the minimal v1.PermissionsService_ReadRelationshipsClient
// fixture — same shape as pkg/platform/relsync/sync_test.go's own: serves
// canned responses off a slice, then io.EOF.
type fakeReadStream struct {
	grpc.ClientStream
	resps []*v1.ReadRelationshipsResponse
	idx   int
}

func (s *fakeReadStream) Recv() (*v1.ReadRelationshipsResponse, error) {
	if s.idx >= len(s.resps) {
		return nil, io.EOF
	}
	r := s.resps[s.idx]
	s.idx++
	return r, nil
}

// fakeReader answers relsync's two read shapes — a narrow #relhash lookup
// and a full per-scope/reap-scan read — off two independent, pre-seeded
// sets. Mirrors pkg/platform/relsync/sync_test.go's own fakeReader.
type fakeReader struct {
	mu sync.Mutex

	owned  []spicedb.Tuple
	hashes map[string]string // "resourceType:scopeID" -> current sentinel value
}

func (r *fakeReader) ReadRelationships(_ context.Context, req *v1.ReadRelationshipsRequest) (v1.PermissionsService_ReadRelationshipsClient, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	filter := req.GetRelationshipFilter()

	if filter.GetOptionalRelation() == "relhash" {
		key := filter.GetResourceType() + ":" + filter.GetOptionalResourceId()
		if h, ok := r.hashes[key]; ok {
			return &fakeReadStream{resps: []*v1.ReadRelationshipsResponse{
				sentinelResponse(filter.GetResourceType(), filter.GetOptionalResourceId(), h),
			}}, nil
		}
		return &fakeReadStream{}, nil
	}

	var resps []*v1.ReadRelationshipsResponse
	for _, t := range r.owned {
		if t.ResourceType != filter.GetResourceType() {
			continue
		}
		if filter.GetOptionalResourceId() != "" && t.ResourceID != filter.GetOptionalResourceId() {
			continue
		}
		if filter.GetOptionalRelation() != "" && t.Relation != filter.GetOptionalRelation() {
			continue
		}
		resps = append(resps, tupleResponse(t))
	}
	return &fakeReadStream{resps: resps}, nil
}

func tupleResponse(t spicedb.Tuple) *v1.ReadRelationshipsResponse {
	subj := &v1.SubjectReference{Object: &v1.ObjectReference{ObjectType: t.SubjectType, ObjectId: t.SubjectID}}
	if t.SubjectRelation != "" {
		subj.OptionalRelation = t.SubjectRelation
	}
	return &v1.ReadRelationshipsResponse{
		Relationship: &v1.Relationship{
			Resource: &v1.ObjectReference{ObjectType: t.ResourceType, ObjectId: t.ResourceID},
			Relation: t.Relation,
			Subject:  subj,
		},
	}
}

func sentinelResponse(resourceType, resourceID, hash string) *v1.ReadRelationshipsResponse {
	return &v1.ReadRelationshipsResponse{
		Relationship: &v1.Relationship{
			Resource: &v1.ObjectReference{ObjectType: resourceType, ObjectId: resourceID},
			Relation: "relhash",
			Subject:  &v1.SubjectReference{Object: &v1.ObjectReference{ObjectType: "string", ObjectId: hash}},
		},
	}
}

// tup builds a fake_scope:<scopeID>#member@fake_user:<subjectID> tuple.
func tup(scopeID, subjectID string) spicedb.Tuple {
	return spicedb.Tuple{
		ResourceType: "fake_scope",
		ResourceID:   scopeID,
		Relation:     "member",
		SubjectType:  "fake_user",
		SubjectID:    subjectID,
	}
}

// -----------------------------------------------------------------------
// K8s object fixtures
// -----------------------------------------------------------------------

func newScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, spiceboxv1alpha1.AddToScheme(s))
	require.NoError(t, corev1.AddToScheme(s))
	return s
}

func newClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	return fake.NewClientBuilder().WithScheme(newScheme(t)).
		WithObjects(objs...).
		WithStatusSubresource(&spiceboxv1alpha1.RelationshipSource{}).
		Build()
}

func newSecretReader(c client.Client) *adoptguard.SecretReader {
	return adoptguard.NewSecretReader(c, c, adoptguard.Warn, func(types.NamespacedName) bool { return false })
}

// newSrc builds a minimal RelationshipSource naming kind, with a static
// credential binding that resolves against authFixtures's AgentIdentity/Secret.
func newSrc(ns, name, kind string) *spiceboxv1alpha1.RelationshipSource {
	return &spiceboxv1alpha1.RelationshipSource{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, UID: types.UID(name + "-uid")},
		Spec: spiceboxv1alpha1.RelationshipSourceSpec{
			Kind: kind,
			Auth: spiceboxv1alpha1.RelationshipSourceAuth{AgentIdentity: "id", Credential: "cred"},
		},
	}
}

// authFixtures returns the AgentIdentity + Secret newSrc's spec.auth resolves
// against, in namespace ns.
//
// allowedHosts scopes the credential. Left empty it is UNSCOPED, which is the
// shape every pre-baseURL source has and must keep working — resolveCreds only
// demands a scope when spec.baseURL names a tenant-writable destination.
func authFixtures(ns string, allowedHosts ...string) (*spiceboxv1alpha1.AgentIdentity, *corev1.Secret) {
	id := &spiceboxv1alpha1.AgentIdentity{
		ObjectMeta: metav1.ObjectMeta{Name: "id", Namespace: ns},
		Spec: spiceboxv1alpha1.AgentIdentitySpec{
			Credentials: []spiceboxv1alpha1.AgentCredential{{
				Name: "cred", Type: "static",
				AllowedHosts: allowedHosts,
				Static: &spiceboxv1alpha1.StaticCredentialSource{
					SecretRef: spiceboxv1alpha1.SecretKeyRef{Name: "tok", Key: "token"},
				},
			}},
		},
	}
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "tok", Namespace: ns},
		Data:       map[string][]byte{"token": []byte("xoxb-fake")},
	}
	adoptguard.WithAdoptedLabel(sec)
	return id, sec
}

// -----------------------------------------------------------------------
// Tests
// -----------------------------------------------------------------------

// The writer is bound to the KIND's own source, not to a generic one: the
// guard from sub-project 2 refuses a kind writing a relation it did not
// claim, and that interlock is what makes the hash sentinel trustworthy.
func TestReconcile_BindsTheWriterToTheKindsOwnSource(t *testing.T) {
	const kindName = "fakekind-bind"
	src := newSrc("ns", "src", kindName)
	id, sec := authFixtures("ns")
	c := newClient(t, src, id, sec)

	fk := &fakeKind{
		name:   kindName,
		source: relsource.Source{Name: "fakekindbindsync", Claims: []string{"fake_scope#member"}},
		pages:  []relsync.ScopePage{{Complete: true}}, // no scopes; only the binding matters here
	}
	relsync.Register(fk)

	sdb := &fakeSpiceDB{writer: &fakeRelWriter{}, reader: &fakeReader{}}
	r := &Reconciler{Client: c, SecretReader: newSecretReader(c), SpiceDB: sdb}

	key := types.NamespacedName{Namespace: "ns", Name: "src"}
	_, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: key})
	require.NoError(t, err)

	require.Len(t, sdb.writerCalls, 1, "Writer must be called exactly once per pass")
	assert.Equal(t, fk.Source(), sdb.writerCalls[0],
		"the writer must be bound to the KIND's own Source, never a hardcoded/generic one")
}

// A customer-hosted bridge has no constant endpoint, so the CR carries it and
// the controller hands it to the kind. Without this the kind fails closed and
// can never reach a bridge — a feature that exists and cannot run.
func TestReconcile_PassesTheCRsBaseURLToTheKind(t *testing.T) {
	const kindName = "fakekind-baseurl"
	src := newSrc("ns", "src", kindName)
	src.Spec.BaseURL = "https://scim.example.com"
	// Scoped to the destination it is sent to: with spec.baseURL set, an
	// unscoped credential is refused before the Secret is read (see
	// TestResolveCreds_RefusesACredentialTheBaseURLIsNotScopedTo).
	id, sec := authFixtures("ns", "scim.example.com")
	c := newClient(t, src, id, sec)

	fk := &fakeKind{
		name:   kindName,
		source: relsource.Source{Name: "fakekindbaseurlsync"},
		pages:  []relsync.ScopePage{{Complete: true}}, // no scopes; only the threaded credential matters here
	}
	relsync.Register(fk)

	sdb := &fakeSpiceDB{writer: &fakeRelWriter{}, reader: &fakeReader{}}
	r := &Reconciler{Client: c, SecretReader: newSecretReader(c), SpiceDB: sdb}

	key := types.NamespacedName{Namespace: "ns", Name: "src"}
	_, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: key})
	require.NoError(t, err)

	creds := fk.receivedCreds()
	require.Len(t, creds, 1, "precondition: ListScopes must have been called exactly once")
	assert.Equal(t, "https://scim.example.com", creds[0].Endpoint,
		"the controller must thread spec.baseURL through to the kind as SourceParams.Endpoint")
}

// spec.config reaches the kind verbatim. A kind is a process-wide singleton,
// so per-CR configuration cannot live on it — the same reason Endpoint rides
// here rather than on *SyncKind.
func TestReconcile_PassesTheCRsConfigToTheKind(t *testing.T) {
	const kindName = "fakekind-config"
	src := newSrc("ns", "src", kindName)
	src.Spec.Config = &apiextensionsv1.JSON{Raw: []byte(`{"orgs":["acme"]}`)}
	id, sec := authFixtures("ns")
	c := newClient(t, src, id, sec)

	fk := &fakeKind{
		name:   kindName,
		source: relsource.Source{Name: "fakekindconfigsync"},
		pages:  []relsync.ScopePage{{Complete: true}}, // no scopes; only the threaded config matters here
	}
	relsync.Register(fk)

	sdb := &fakeSpiceDB{writer: &fakeRelWriter{}, reader: &fakeReader{}}
	r := &Reconciler{Client: c, SecretReader: newSecretReader(c), SpiceDB: sdb}

	key := types.NamespacedName{Namespace: "ns", Name: "src"}
	_, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: key})
	require.NoError(t, err)

	creds := fk.receivedCreds()
	require.Len(t, creds, 1, "precondition: ListScopes must have been called exactly once")
	assert.Equal(t, `{"orgs":["acme"]}`, string(creds[0].Config),
		"the controller must thread spec.config through to the kind as SourceParams.Config, byte-for-byte")
}

// A kind needing no config must see nil, not an empty object that its
// ParseConfig would then have to special-case.
func TestReconcile_AbsentConfigArrivesNil(t *testing.T) {
	const kindName = "fakekind-noconfig"
	src := newSrc("ns", "src", kindName)
	// src.Spec.Config left unset.
	id, sec := authFixtures("ns")
	c := newClient(t, src, id, sec)

	fk := &fakeKind{
		name:   kindName,
		source: relsource.Source{Name: "fakekindnoconfigsync"},
		pages:  []relsync.ScopePage{{Complete: true}}, // no scopes; only the threaded config matters here
	}
	relsync.Register(fk)

	sdb := &fakeSpiceDB{writer: &fakeRelWriter{}, reader: &fakeReader{}}
	r := &Reconciler{Client: c, SecretReader: newSecretReader(c), SpiceDB: sdb}

	key := types.NamespacedName{Namespace: "ns", Name: "src"}
	_, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: key})
	require.NoError(t, err)

	creds := fk.receivedCreds()
	require.Len(t, creds, 1, "precondition: ListScopes must have been called exactly once")
	assert.Nil(t, creds[0].Config,
		"an unset spec.config must arrive as nil, not an empty object the kind would have to special-case")
}

// spec.baseURL and spec.auth are two independent fields on the same
// tenant-writable CR. Unlinked, `baseURL: https://attacker.example` plus any
// credential in the namespace made the operator send that credential's raw
// value as a bearer token to the attacker's host — reading the Secret with its
// OWN cluster-wide credentials, so the actor never needed `get secrets`. This
// is the SkillSource incident (pkg/platform/identity/credhost's package doc) on
// a second CR of the same shape.
//
// Each case asserts three things, and the Secret one is the load-bearing
// assertion: the refusal must happen BEFORE the Secret is adopted or read, so
// it never touches the value. The fixture Secret is deliberately NOT
// pre-adopted, so an unstamped AdoptedLabel after the reconcile proves
// adoptkit.AdoptSecret was never reached.
func TestResolveCreds_RefusesACredentialTheBaseURLIsNotScopedTo(t *testing.T) {
	cases := []struct {
		name         string
		baseURL      string
		allowedHosts []string
		wantInMsg    []string
	}{
		{
			name:         "a host outside allowedHosts: the exfiltration itself",
			baseURL:      "https://attacker.example",
			allowedHosts: []string{"scim.example.com"},
			wantInMsg:    []string{"ns/src", "attacker.example", "scim.example.com"},
		},
		{
			name:    "an unscoped credential: no safe default destination for a bearer token",
			baseURL: "https://scim.example.com",
			// credhost.Check treats empty allowedHosts as permissive, so this
			// path has to demand the scope rather than inherit that default.
			allowedHosts: nil,
			wantInMsg:    []string{"declares no allowedHosts", "spec.credentials[].allowedHosts", "cred", "id"},
		},
		{
			name:         "a scoped credential is not sent to the same name on a different port",
			baseURL:      "https://scim.example.com:9999",
			allowedHosts: []string{"scim.example.com:8443"},
			wantInMsg:    []string{"scim.example.com:9999"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			kindName := "fakekind-credhost-" + strings.ReplaceAll(tc.name[:12], " ", "-")
			src := newSrc("ns", "src", kindName)
			src.Spec.BaseURL = tc.baseURL
			id, sec := authFixtures("ns", tc.allowedHosts...)
			// Un-adopt: the label's absence after the reconcile is what proves
			// the refusal came before adoptkit.AdoptSecret.
			sec.Labels = nil
			c := newClient(t, src, id, sec)

			fk := &fakeKind{
				name:   kindName,
				source: relsource.Source{Name: "fakekindcredhostsync"},
				pages:  []relsync.ScopePage{{Complete: true}},
			}
			relsync.Register(fk)

			sdb := &fakeSpiceDB{writer: &fakeRelWriter{}, reader: &fakeReader{}}
			r := &Reconciler{Client: c, SecretReader: newSecretReader(c), SpiceDB: sdb}

			key := types.NamespacedName{Namespace: "ns", Name: "src"}
			_, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: key})
			require.NoError(t, err, "an auth refusal is condition-surfaced, never returned")

			assert.Empty(t, fk.receivedCreds(),
				"a refused credential must never reach the kind, which would dial the destination with it")

			var gotSec corev1.Secret
			require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "tok"}, &gotSec))
			assert.NotContains(t, gotSec.Labels, adoptguard.AdoptedLabel,
				"the Secret must not be adopted (nor read) on a refused destination")

			var got spiceboxv1alpha1.RelationshipSource
			require.NoError(t, c.Get(context.Background(), key, &got))
			cond := conditions.Find(got.Status.Conditions, spiceboxv1alpha1.RelationshipSourceConditionReady)
			require.NotNil(t, cond, "the refusal must surface on Ready like every other auth-resolve failure")
			assert.Equal(t, metav1.ConditionFalse, cond.Status)
			assert.Equal(t, spiceboxv1alpha1.ReasonRelationshipSourceAuthResolveFailed, cond.Reason)
			for _, want := range tc.wantInMsg {
				assert.Contains(t, cond.Message, want,
					"the message must tell the operator what to fix, not just that something failed")
			}
		})
	}
}

// The regression that matters most: a Slack-shaped source has no
// tenant-writable destination — the kind dials its own constant host — so an
// unscoped credential must keep working exactly as before. A check that fired
// here would break every existing RelationshipSource on upgrade.
func TestReconcile_NoBaseURLMeansNoDestinationCheck(t *testing.T) {
	const kindName = "fakekind-nobaseurl"
	src := newSrc("ns", "src", kindName) // spec.baseURL deliberately unset
	id, sec := authFixtures("ns")        // and the credential deliberately unscoped
	c := newClient(t, src, id, sec)

	fk := &fakeKind{
		name:   kindName,
		source: relsource.Source{Name: "fakekindnobaseurlsync"},
		pages:  []relsync.ScopePage{{Complete: true}},
	}
	relsync.Register(fk)

	sdb := &fakeSpiceDB{writer: &fakeRelWriter{}, reader: &fakeReader{}}
	r := &Reconciler{Client: c, SecretReader: newSecretReader(c), SpiceDB: sdb}

	key := types.NamespacedName{Namespace: "ns", Name: "src"}
	_, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: key})
	require.NoError(t, err)

	creds := fk.receivedCreds()
	require.Len(t, creds, 1, "an unscoped credential with no baseURL must still reach the kind")
	assert.Equal(t, "", creds[0].Endpoint, "there is no endpoint to carry")

	var got spiceboxv1alpha1.RelationshipSource
	require.NoError(t, c.Get(context.Background(), key, &got))
	cond := conditions.Find(got.Status.Conditions, spiceboxv1alpha1.RelationshipSourceConditionReady)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionTrue, cond.Status, "reason=%s message=%s", cond.Reason, cond.Message)
}

// Resumption round-trips through status: a budgeted pass records where it
// stopped, and the next reconcile starts there.
func TestReconcile_PersistsResumeAfterAndResumesFromIt(t *testing.T) {
	const kindName = "fakekind-resume"
	src := newSrc("ns", "src", kindName)
	src.Spec.Sync.MaxScopesPerPass = 1
	id, sec := authFixtures("ns")
	c := newClient(t, src, id, sec)

	fk := &fakeKind{
		name:   kindName,
		source: relsource.Source{Name: "fakekindresumesync"},
		pages: []relsync.ScopePage{
			// Two pages so a MaxScopesPerPass=1 budget genuinely cuts
			// enumeration off mid-pagination (a single page whose own Next is
			// already zero can never be truncated by a budget — see
			// relsync's enumerate doc).
			{Scopes: []relsync.Scope{{ID: "C1", ResourceType: "fake_scope"}}, Next: relsync.Cursor{Token: "1"}, Complete: false},
			{Scopes: []relsync.Scope{{ID: "C2", ResourceType: "fake_scope"}}, Next: relsync.Cursor{}, Complete: true},
		},
		members: map[relsync.ScopeID][]spicedb.Tuple{
			"C1": {tup("C1", "alice")},
			"C2": {tup("C2", "bob")},
		},
	}
	relsync.Register(fk)

	sdb := &fakeSpiceDB{writer: &fakeRelWriter{}, reader: &fakeReader{}}
	r := &Reconciler{Client: c, SecretReader: newSecretReader(c), SpiceDB: sdb}

	key := types.NamespacedName{Namespace: "ns", Name: "src"}
	_, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: key})
	require.NoError(t, err)

	var afterFirst spiceboxv1alpha1.RelationshipSource
	require.NoError(t, c.Get(context.Background(), key, &afterFirst))
	require.Equal(t, "C1", afterFirst.Status.Sync.ResumeAfter, "precondition: the budgeted pass records where it stopped")
	require.Equal(t, []relsync.ScopeID{"C1"}, fk.fetchedIDs(), "precondition: only C1 was fetched on the budgeted first pass")

	_, err = r.Reconcile(context.Background(), reconcile.Request{NamespacedName: key})
	require.NoError(t, err)

	assert.Equal(t, []relsync.ScopeID{"C1", "C2"}, fk.fetchedIDs(),
		"the second reconcile must resume from C1 and fetch ONLY C2 next — a re-fetch of C1 or a restart would prove it did not resume")

	var afterSecond spiceboxv1alpha1.RelationshipSource
	require.NoError(t, c.Get(context.Background(), key, &afterSecond))
	assert.Equal(t, "", afterSecond.Status.Sync.ResumeAfter, "the cycle completes on the second pass and the cursor resets")
}

// A pass that could not enumerate fully must not clear the marker that gates
// reaping.
func TestReconcile_DoesNotSetEnumCompleteOnATruncatedEnumeration(t *testing.T) {
	const kindName = "fakekind-trunc"
	src := newSrc("ns", "src", kindName)
	src.Spec.Sync.MaxScopesPerPass = 1
	id, sec := authFixtures("ns")
	c := newClient(t, src, id, sec)

	fk := &fakeKind{
		name:   kindName,
		source: relsource.Source{Name: "fakekindtruncsync"},
		pages: []relsync.ScopePage{
			{Scopes: []relsync.Scope{{ID: "C1", ResourceType: "fake_scope"}}, Next: relsync.Cursor{Token: "1"}, Complete: false},
			{Scopes: []relsync.Scope{{ID: "C2", ResourceType: "fake_scope"}}, Next: relsync.Cursor{}, Complete: true},
		},
		members: map[relsync.ScopeID][]spicedb.Tuple{"C1": {tup("C1", "alice")}},
	}
	relsync.Register(fk)

	sdb := &fakeSpiceDB{writer: &fakeRelWriter{}, reader: &fakeReader{}}
	r := &Reconciler{Client: c, SecretReader: newSecretReader(c), SpiceDB: sdb}

	key := types.NamespacedName{Namespace: "ns", Name: "src"}
	_, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: key})
	require.NoError(t, err)

	var got spiceboxv1alpha1.RelationshipSource
	require.NoError(t, c.Get(context.Background(), key, &got))
	assert.False(t, got.Status.Sync.EnumComplete,
		"a truncated (budget-cut) enumeration must never be reported as complete — doing so would arm reaping against scopes never reached")
}

// MAJOR 5 (whole-branch review): a total enumeration failure — a revoked
// token, missing_scope, a network outage — must not read as Ready=True on a
// source that never synced a single tuple. Before this fix, relsync.Pass's
// error return is reserved for something it cannot even attempt to recover
// from, so a ListScopes failure folds into ScopeErrors and Pass still
// returns nil — Reconcile took the success path (applySyncResult always
// calls conditions.SetTrue) regardless of EnumComplete/Processed.
func TestReconcile_TotalEnumerationFailureSetsReadyFalse(t *testing.T) {
	const kindName = "fakekind-enumfail"
	src := newSrc("ns", "src", kindName)
	id, sec := authFixtures("ns")
	c := newClient(t, src, id, sec)

	fk := &fakeKind{
		name:    kindName,
		source:  relsource.Source{Name: "fakekindenumfailsync"},
		listErr: fmt.Errorf("slack conversations.list: missing_scope"),
	}
	relsync.Register(fk)

	sdb := &fakeSpiceDB{writer: &fakeRelWriter{}, reader: &fakeReader{}}
	r := &Reconciler{Client: c, SecretReader: newSecretReader(c), SpiceDB: sdb}

	key := types.NamespacedName{Namespace: "ns", Name: "src"}
	_, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: key})
	require.NoError(t, err, "relsync.Pass itself must not error — enumeration failure is a ScopeError, not a Pass failure")

	var got spiceboxv1alpha1.RelationshipSource
	require.NoError(t, c.Get(context.Background(), key, &got))
	cond := conditions.Find(got.Status.Conditions, spiceboxv1alpha1.RelationshipSourceConditionReady)
	require.NotNil(t, cond, "Ready condition must be set")
	assert.Equal(t, metav1.ConditionFalse, cond.Status,
		"a pass that enumerated and processed nothing must never read Ready=True/Synced")
	assert.Equal(t, spiceboxv1alpha1.ReasonRelationshipSourceEnumerationFailed, cond.Reason)
	assert.Contains(t, cond.Message, "missing_scope", "the underlying failure must be named, not just a generic reason")
}

// The counterpart: a budget-truncated enumeration that still fetched at
// least one scope is real progress, not a total failure, and must keep
// reporting Ready=True/Synced.
func TestReconcile_TruncatedButProductivePassStillReportsSynced(t *testing.T) {
	const kindName = "fakekind-enumpartial"
	src := newSrc("ns", "src", kindName)
	src.Spec.Sync.MaxScopesPerPass = 1
	id, sec := authFixtures("ns")
	c := newClient(t, src, id, sec)

	fk := &fakeKind{
		name:   kindName,
		source: relsource.Source{Name: "fakekindenumpartialsync"},
		pages: []relsync.ScopePage{
			{Scopes: []relsync.Scope{{ID: "C1", ResourceType: "fake_scope"}}, Next: relsync.Cursor{Token: "1"}, Complete: false},
			{Scopes: []relsync.Scope{{ID: "C2", ResourceType: "fake_scope"}}, Next: relsync.Cursor{}, Complete: true},
		},
		members: map[relsync.ScopeID][]spicedb.Tuple{"C1": {tup("C1", "alice")}},
	}
	relsync.Register(fk)

	sdb := &fakeSpiceDB{writer: &fakeRelWriter{}, reader: &fakeReader{}}
	r := &Reconciler{Client: c, SecretReader: newSecretReader(c), SpiceDB: sdb}

	key := types.NamespacedName{Namespace: "ns", Name: "src"}
	_, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: key})
	require.NoError(t, err)

	var got spiceboxv1alpha1.RelationshipSource
	require.NoError(t, c.Get(context.Background(), key, &got))
	cond := conditions.Find(got.Status.Conditions, spiceboxv1alpha1.RelationshipSourceConditionReady)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionTrue, cond.Status,
		"EnumComplete=false alone must not fail Ready — a pass that fetched at least one scope made real progress")
	assert.Equal(t, spiceboxv1alpha1.ReasonRelationshipSourceSynced, cond.Reason)
}

// Status is controller-owned and set-on-change: a reconcile that changes
// nothing must not rewrite the object.
func TestReconcile_UnchangedPassDoesNotChurnStatus(t *testing.T) {
	const kindName = "fakekind-unchanged"
	src := newSrc("ns", "src", kindName)
	id, sec := authFixtures("ns")
	c := newClient(t, src, id, sec)

	members := []spicedb.Tuple{tup("C1", "alice")}
	fk := &fakeKind{
		name:    kindName,
		source:  relsource.Source{Name: "fakekindunchangedsync"},
		pages:   []relsync.ScopePage{{Scopes: []relsync.Scope{{ID: "C1", ResourceType: "fake_scope"}}, Complete: true}},
		members: map[relsync.ScopeID][]spicedb.Tuple{"C1": members},
	}
	relsync.Register(fk)

	// SpiceDB already holds exactly what upstream reports, sentinel included,
	// so every pass hits the hash short-circuit and writes/prunes/reaps
	// nothing.
	sdb := &fakeSpiceDB{
		writer: &fakeRelWriter{},
		reader: &fakeReader{
			owned:  members,
			hashes: map[string]string{"fake_scope:C1": relsync.HashTuples(members)},
		},
	}
	// An advancing clock, or the SECOND reconcile never reaches
	// applySyncResult at all: a completed cycle arms passPacer's hold for the
	// sync interval, and a held reconcile writes no status — so this test
	// would keep passing while proving nothing about the no-churn comparison
	// it exists to protect.
	r := &Reconciler{Client: c, SecretReader: newSecretReader(c), SpiceDB: sdb, Now: newIntervalClock().now}

	key := types.NamespacedName{Namespace: "ns", Name: "src"}
	_, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: key})
	require.NoError(t, err)

	var first spiceboxv1alpha1.RelationshipSource
	require.NoError(t, c.Get(context.Background(), key, &first))
	require.NotNil(t, first.Status.Sync.LastSyncTime, "precondition: the first pass completes a cycle and stamps lastSyncTime")

	_, err = r.Reconcile(context.Background(), reconcile.Request{NamespacedName: key})
	require.NoError(t, err)

	var second spiceboxv1alpha1.RelationshipSource
	require.NoError(t, c.Get(context.Background(), key, &second))
	assert.Equal(t, first.ResourceVersion, second.ResourceVersion,
		"a no-change pass must not write status at all, or the self-watch (no predicate) re-enqueues forever")
	require.NotNil(t, second.Status.Sync.LastSyncTime)
	assert.Equal(t, *first.Status.Sync.LastSyncTime, *second.Status.Sync.LastSyncTime)
	assert.Equal(t, first.Status.Sync.ResumeAfter, second.Status.Sync.ResumeAfter)
	assert.Equal(t, first.Status.Sync.EnumComplete, second.Status.Sync.EnumComplete)
}

// A pass that changed SpiceDB records what it did, so an operator can tell
// "ran and did nothing" from "ran and wrote 40 edges".
func TestReconcile_RecordsWhatThePassDid(t *testing.T) {
	st := &spiceboxv1alpha1.RelationshipSourceStatus{}
	obj := &spiceboxv1alpha1.RelationshipSource{}
	obj.Generation = 1

	moved := applySyncResult(obj, st, true, relsync.PassResult{
		Processed: 3, Written: 40, Pruned: 2, ReapedScopes: 1, JoinMisses: 7,
		EnumComplete: true, CycleComplete: true,
	}, true, false)

	require.True(t, moved, "a pass that wrote must move status")
	require.NotNil(t, st.Sync.LastPass, "counts must be recorded")
	assert.Equal(t, int32(3), st.Sync.LastPass.ScopesProcessed)
	assert.Equal(t, int32(40), st.Sync.LastPass.Written)
	assert.Equal(t, int32(2), st.Sync.LastPass.Pruned)
	assert.Equal(t, int32(1), st.Sync.LastPass.ReapedScopes)
	assert.Equal(t, int32(7), st.Sync.LastPass.JoinMisses)
	assert.NotNil(t, st.Sync.LastPass.FinishedAt, "a recorded pass is stamped")
	assert.False(t, st.Sync.LastPass.Scoped, "this call passed scoped=false")

	// A scoped (event-driven, single-scope) pass must record that fact too —
	// scoped=false is also the zero value, so a literal that drops the field
	// entirely would still pass every assertion above.
	scopedSt := &spiceboxv1alpha1.RelationshipSourceStatus{}
	scopedObj := &spiceboxv1alpha1.RelationshipSource{}
	scopedObj.Generation = 1
	require.True(t, applySyncResult(scopedObj, scopedSt, true, relsync.PassResult{
		Processed: 1, Written: 1, EnumComplete: true, CycleComplete: true,
	}, true, true))
	assert.True(t, scopedSt.Sync.LastPass.Scoped, "this call passed scoped=true")
}

// The complement of the loop guard: a pass that wrote nothing but whose
// counts changed MUST move status, or the JoinMisses alert signal never
// reaches an operator. passWrote (controller.go) is false whenever
// Written/Pruned/ReapedScopes are all zero, so JoinMisses (and Processed)
// are the only counts that can move on a wrote=false pass — and JoinMisses
// going 0->40 is exactly the broken-identity-join signal this field exists
// to surface (see RelationshipSourcePassStats.JoinMisses's doc). If LastPass
// were assigned after the moved check instead of before it, this count
// change would never be compared, `moved` would stay false, and the signal
// would never reach etcd.
func TestReconcile_ChangedJoinMissesMovesStatusEvenWithNoWrite(t *testing.T) {
	st := &spiceboxv1alpha1.RelationshipSourceStatus{}
	obj := &spiceboxv1alpha1.RelationshipSource{}
	obj.Generation = 1
	base := relsync.PassResult{Processed: 3, EnumComplete: true, CycleComplete: true}
	require.True(t, applySyncResult(obj, st, true, base, true, false))

	degraded := base
	degraded.JoinMisses = 40
	assert.True(t, applySyncResult(obj, st, false, degraded, false, false),
		"a count change with no write must still persist")
	assert.Equal(t, int32(40), st.Sync.LastPass.JoinMisses)
}

// THE loop guard. The controller's self-watch has no predicate, so a status
// write re-enqueues this reconcile and its upstream Pass. If LastPass carries
// a freshly-stamped FinishedAt on a pass that changed nothing, `moved` is true
// forever and the source hammers its upstream API until someone notices.
//
// Two identical no-op passes in a row: the second must not move status.
func TestReconcile_RepeatedNoOpPassDoesNotChurnLastPass(t *testing.T) {
	st := &spiceboxv1alpha1.RelationshipSourceStatus{}
	obj := &spiceboxv1alpha1.RelationshipSource{}
	obj.Generation = 1

	res := relsync.PassResult{Processed: 3, EnumComplete: true, CycleComplete: true}

	// First pass lands and stamps.
	require.True(t, applySyncResult(obj, st, true, res, true, false))
	require.NotNil(t, st.Sync.LastPass.FinishedAt)
	// Captured BY VALUE, not the pointer: an implementation that restamps
	// FinishedAt in place (same pointer, new *time.Time value written through
	// it) would satisfy a pointer-equality/pointer-held comparison vacuously.
	firstStamp := *st.Sync.LastPass.FinishedAt

	// Second, identical pass wrote nothing.
	moved := applySyncResult(obj, st, false, res, false, false)

	assert.False(t, moved,
		"an identical no-op pass must not move status: the self-watch would re-enqueue forever")
	require.NotNil(t, st.Sync.LastPass.FinishedAt)
	assert.Equal(t, firstStamp, *st.Sync.LastPass.FinishedAt,
		"FinishedAt must be carried, not restamped, or the comparison always differs")
}

// newKindConflictFixtures returns two RelationshipSources naming the same
// spec.kind, with older's creationTimestamp genuinely earlier than newer's.
func newKindConflictFixtures() (older, newer *spiceboxv1alpha1.RelationshipSource) {
	older = newSrc("ns", "older", "sharedkind")
	older.CreationTimestamp = metav1.NewTime(time.Now().Add(-1 * time.Hour))
	newer = newSrc("ns", "newer", "sharedkind")
	newer.CreationTimestamp = metav1.NewTime(time.Now())
	return older, newer
}

// Two RelationshipSources naming the same spec.kind mutually annihilate each
// other's reap (see the package doc): the controller must refuse the newer
// one, deterministically, by creationTimestamp.
func TestReconcile_KindClaimConflict_OlderProceedsNewerParked(t *testing.T) {
	older, newer := newKindConflictFixtures()
	c := newClient(t, older, newer)
	sdb := &fakeSpiceDB{writer: &fakeRelWriter{}, reader: &fakeReader{}}
	r := &Reconciler{Client: c, SecretReader: newSecretReader(c), SpiceDB: sdb}

	// The OLDER CR must recognize itself as the sole incumbent and fall
	// through PAST the kind-claim gate — "sharedkind" is never registered in
	// this test, so it lands on KindUnregistered next, never KindClaimed.
	oKey := types.NamespacedName{Namespace: "ns", Name: "older"}
	_, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: oKey})
	require.NoError(t, err)
	var gotOlder spiceboxv1alpha1.RelationshipSource
	require.NoError(t, c.Get(context.Background(), oKey, &gotOlder))
	oReady := conditions.Find(gotOlder.Status.Conditions, spiceboxv1alpha1.RelationshipSourceConditionReady)
	require.NotNil(t, oReady)
	assert.Equal(t, spiceboxv1alpha1.ReasonRelationshipSourceKindUnregistered, oReady.Reason,
		"the older CR must pass the kind-claim gate cleanly")

	// The NEWER CR must be parked, naming the older as incumbent.
	nKey := types.NamespacedName{Namespace: "ns", Name: "newer"}
	_, err = r.Reconcile(context.Background(), reconcile.Request{NamespacedName: nKey})
	require.NoError(t, err)
	var gotNewer spiceboxv1alpha1.RelationshipSource
	require.NoError(t, c.Get(context.Background(), nKey, &gotNewer))
	nReady := conditions.Find(gotNewer.Status.Conditions, spiceboxv1alpha1.RelationshipSourceConditionReady)
	require.NotNil(t, nReady)
	assert.Equal(t, metav1.ConditionFalse, nReady.Status)
	assert.Equal(t, spiceboxv1alpha1.ReasonRelationshipSourceKindClaimed, nReady.Reason)
	assert.Contains(t, nReady.Message, "older", "the message must name the incumbent")
	assert.Empty(t, sdb.writerCalls, "a parked source must never bind a writer or sync")
}

// The reverse reconcile order must reach the exact same verdict: incumbency
// is decided by creationTimestamp alone, never by which CR happens to
// reconcile first.
func TestReconcile_KindClaimConflict_SameOutcomeInReverseReconcileOrder(t *testing.T) {
	older, newer := newKindConflictFixtures()
	c := newClient(t, older, newer)
	sdb := &fakeSpiceDB{writer: &fakeRelWriter{}, reader: &fakeReader{}}
	r := &Reconciler{Client: c, SecretReader: newSecretReader(c), SpiceDB: sdb}

	// NEWER reconciles first this time.
	nKey := types.NamespacedName{Namespace: "ns", Name: "newer"}
	_, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: nKey})
	require.NoError(t, err)
	var gotNewer spiceboxv1alpha1.RelationshipSource
	require.NoError(t, c.Get(context.Background(), nKey, &gotNewer))
	nReady := conditions.Find(gotNewer.Status.Conditions, spiceboxv1alpha1.RelationshipSourceConditionReady)
	require.NotNil(t, nReady)
	assert.Equal(t, metav1.ConditionFalse, nReady.Status)
	assert.Equal(t, spiceboxv1alpha1.ReasonRelationshipSourceKindClaimed, nReady.Reason)

	oKey := types.NamespacedName{Namespace: "ns", Name: "older"}
	_, err = r.Reconcile(context.Background(), reconcile.Request{NamespacedName: oKey})
	require.NoError(t, err)
	var gotOlder spiceboxv1alpha1.RelationshipSource
	require.NoError(t, c.Get(context.Background(), oKey, &gotOlder))
	oReady := conditions.Find(gotOlder.Status.Conditions, spiceboxv1alpha1.RelationshipSourceConditionReady)
	require.NotNil(t, oReady)
	assert.Equal(t, spiceboxv1alpha1.ReasonRelationshipSourceKindUnregistered, oReady.Reason,
		"the older CR must still be recognized as incumbent even though the newer one reconciled first")
}

// realSlackRateLimit wraps a GENUINE *slackapi.RateLimitedError — slack-go's
// real 429 type, which carries its backoff as a FIELD (RetryAfter
// time.Duration), not a method — into something satisfying the
// Reconciler's RetryAfter interface (RetryAfter() time.Duration).
//
// This mirrors exactly what pkg/channels/channelkinds/slack's
// (unexported) withRetryAfter does for the real Slack relsync.Kind:
// Minor 6 of the whole-branch review found that the ONLY test of this path
// used a hand-built fixture (a type production never produces) that
// happened to already satisfy the interface by construction — which
// proved the reconciler's dispatch works, but said nothing about whether
// Slack's REAL error ever reaches it in a form the dispatch can use. Using
// the genuine slack-go type here, adapted the same way production adapts
// it, is what closes that gap.
type realSlackRateLimit struct{ *slackapi.RateLimitedError }

func (e realSlackRateLimit) RetryAfter() time.Duration { return e.RateLimitedError.RetryAfter }

// A kind's error carrying a Retry-After overrides the normal sync interval,
// without losing the progress the pass already wrote.
func TestReconcile_HonoursRetryAfterFromAScopeError(t *testing.T) {
	const kindName = "fakekind-retryafter"
	src := newSrc("ns", "src", kindName)
	id, sec := authFixtures("ns")
	c := newClient(t, src, id, sec)

	fk := &fakeKind{
		name:   kindName,
		source: relsource.Source{Name: "fakekindretryaftersync"},
		pages:  []relsync.ScopePage{{Scopes: []relsync.Scope{{ID: "C1", ResourceType: "fake_scope"}}, Complete: true}},
		fetchErrs: map[relsync.ScopeID]error{"C1": fmt.Errorf("upstream: %w",
			realSlackRateLimit{&slackapi.RateLimitedError{RetryAfter: 37 * time.Second}})},
	}
	relsync.Register(fk)

	sdb := &fakeSpiceDB{writer: &fakeRelWriter{}, reader: &fakeReader{}}
	r := &Reconciler{Client: c, SecretReader: newSecretReader(c), SpiceDB: sdb, SyncInterval: time.Hour}

	key := types.NamespacedName{Namespace: "ns", Name: "src"}
	res, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: key})
	require.NoError(t, err)

	assert.Equal(t, 37*time.Second, res.RequeueAfter,
		"a 429's Retry-After must override the normal (1h) sync interval")

	var got spiceboxv1alpha1.RelationshipSource
	require.NoError(t, c.Get(context.Background(), key, &got))
	ready := conditions.Find(got.Status.Conditions, spiceboxv1alpha1.RelationshipSourceConditionReady)
	require.NotNil(t, ready)
	assert.Equal(t, metav1.ConditionTrue, ready.Status,
		"one scope's transient error is not a pass failure — Ready stays True")
}

// The kind-claim conflict is announced once, not re-announced on every
// reconcile interval for as long as an operator takes to resolve it.
func TestReconcile_PublishesKindClaimConflictOnlyOnce(t *testing.T) {
	older, newer := newKindConflictFixtures()
	c := newClient(t, older, newer)
	sdb := &fakeSpiceDB{writer: &fakeRelWriter{}, reader: &fakeReader{}}

	var mu sync.Mutex
	var published []channelevents.MonitoringEvent
	publish := func(_ string, data []byte) error {
		var ev channelevents.MonitoringEvent
		if err := json.Unmarshal(data, &ev); err != nil {
			return err
		}
		mu.Lock()
		published = append(published, ev)
		mu.Unlock()
		return nil
	}
	r := &Reconciler{Client: c, SecretReader: newSecretReader(c), SpiceDB: sdb, MonitoringPublish: publish}

	key := types.NamespacedName{Namespace: "ns", Name: "newer"}
	_, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: key})
	require.NoError(t, err)
	_, err = r.Reconcile(context.Background(), reconcile.Request{NamespacedName: key})
	require.NoError(t, err)

	require.Len(t, published, 1, "the conflict must be announced once, not on every reconcile interval")
	assert.Equal(t, channelevents.MonitoringTransitionFailed, published[0].Transition)
	assert.Equal(t, spiceboxv1alpha1.ReasonRelationshipSourceKindClaimed, published[0].Reason)
}

// Fix round 1: requeue() used to write status unconditionally on every
// failure path, including a park that never changes reconcile over
// reconcile. SetupWithManager watches this object with no predicate, so
// that status-only write re-enqueued the same reconcile forever — a tight
// loop, not a calm poll at failureRetryInterval. Asserts on ResourceVersion,
// the only signal that discriminates a real no-op from one that merely
// recomputed the same bytes and wrote them anyway.
func TestReconcile_ParkedStatusDoesNotChurnOnRepeatedReconcile(t *testing.T) {
	older, newer := newKindConflictFixtures()
	c := newClient(t, older, newer)
	sdb := &fakeSpiceDB{writer: &fakeRelWriter{}, reader: &fakeReader{}}
	r := &Reconciler{Client: c, SecretReader: newSecretReader(c), SpiceDB: sdb}

	key := types.NamespacedName{Namespace: "ns", Name: "newer"}
	_, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: key})
	require.NoError(t, err)
	var first spiceboxv1alpha1.RelationshipSource
	require.NoError(t, c.Get(context.Background(), key, &first))
	ready := conditions.Find(first.Status.Conditions, spiceboxv1alpha1.RelationshipSourceConditionReady)
	require.NotNil(t, ready)
	require.Equal(t, spiceboxv1alpha1.ReasonRelationshipSourceKindClaimed, ready.Reason,
		"precondition: parked on the first reconcile")

	_, err = r.Reconcile(context.Background(), reconcile.Request{NamespacedName: key})
	require.NoError(t, err)
	var second spiceboxv1alpha1.RelationshipSource
	require.NoError(t, c.Get(context.Background(), key, &second))

	assert.Equal(t, first.ResourceVersion, second.ResourceVersion,
		"a repeated, unchanged park must not rewrite status, or the self-watch re-enqueues forever")
}

// The same fix, for the OTHER failure path this repo will actually hit
// today: no relsync.Kind is registered anywhere in the tree yet (Slack's
// arrives in a later task), so every RelationshipSource currently lands
// here on every single reconcile. A churning write here is not a
// theoretical edge case.
func TestReconcile_UnregisteredKindStatusDoesNotChurnOnRepeatedReconcile(t *testing.T) {
	src := newSrc("ns", "src", "neverregistered")
	c := newClient(t, src)
	sdb := &fakeSpiceDB{writer: &fakeRelWriter{}, reader: &fakeReader{}}
	r := &Reconciler{Client: c, SecretReader: newSecretReader(c), SpiceDB: sdb}

	key := types.NamespacedName{Namespace: "ns", Name: "src"}
	_, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: key})
	require.NoError(t, err)
	var first spiceboxv1alpha1.RelationshipSource
	require.NoError(t, c.Get(context.Background(), key, &first))
	ready := conditions.Find(first.Status.Conditions, spiceboxv1alpha1.RelationshipSourceConditionReady)
	require.NotNil(t, ready)
	require.Equal(t, spiceboxv1alpha1.ReasonRelationshipSourceKindUnregistered, ready.Reason,
		"precondition: this kind is never registered anywhere in this test binary")

	_, err = r.Reconcile(context.Background(), reconcile.Request{NamespacedName: key})
	require.NoError(t, err)
	var second spiceboxv1alpha1.RelationshipSource
	require.NoError(t, c.Get(context.Background(), key, &second))

	assert.Equal(t, first.ResourceVersion, second.ResourceVersion,
		"a repeated 'kind unregistered' failure must not rewrite status every reconcile — "+
			"this is the state of EVERY RelationshipSource today, not an edge case")
}

// The tie-break case the sort exists for: two CRs sharing the exact same
// CreationTimestamp (created by the same manifest apply), differing only in
// namespace/name. The winner must be the same regardless of which one
// reconciles first.
func TestReconcile_KindClaimConflict_TieBreaksByNamespaceThenName(t *testing.T) {
	ts := metav1.NewTime(time.Now())
	// a's namespace sorts AFTER b's, so a must lose the tie-break even
	// though its own name would sort first among the two names.
	a := newSrc("zzz-ns", "aaa-name", "sharedkind")
	a.CreationTimestamp = ts
	b := newSrc("aaa-ns", "zzz-name", "sharedkind")
	b.CreationTimestamp = ts
	c := newClient(t, a, b)
	sdb := &fakeSpiceDB{writer: &fakeRelWriter{}, reader: &fakeReader{}}
	r := &Reconciler{Client: c, SecretReader: newSecretReader(c), SpiceDB: sdb}

	// Reconcile b (the namespace-tie-break winner) first.
	bKey := types.NamespacedName{Namespace: "aaa-ns", Name: "zzz-name"}
	_, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: bKey})
	require.NoError(t, err)
	var gotB spiceboxv1alpha1.RelationshipSource
	require.NoError(t, c.Get(context.Background(), bKey, &gotB))
	bReady := conditions.Find(gotB.Status.Conditions, spiceboxv1alpha1.RelationshipSourceConditionReady)
	require.NotNil(t, bReady)
	assert.Equal(t, spiceboxv1alpha1.ReasonRelationshipSourceKindUnregistered, bReady.Reason,
		"b's namespace sorts first on an exact CreationTimestamp tie, so b must be the incumbent")

	// Reconcile a (the loser) second — order must not matter to the verdict.
	aKey := types.NamespacedName{Namespace: "zzz-ns", Name: "aaa-name"}
	_, err = r.Reconcile(context.Background(), reconcile.Request{NamespacedName: aKey})
	require.NoError(t, err)
	var gotA spiceboxv1alpha1.RelationshipSource
	require.NoError(t, c.Get(context.Background(), aKey, &gotA))
	aReady := conditions.Find(gotA.Status.Conditions, spiceboxv1alpha1.RelationshipSourceConditionReady)
	require.NotNil(t, aReady)
	assert.Equal(t, metav1.ConditionFalse, aReady.Status)
	assert.Equal(t, spiceboxv1alpha1.ReasonRelationshipSourceKindClaimed, aReady.Reason)
	assert.Contains(t, aReady.Message, "aaa-ns", "the message must name the incumbent's namespace")
}
