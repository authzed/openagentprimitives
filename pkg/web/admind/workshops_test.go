package admind_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/go-logr/logr/testr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/platform/artifactstore"
	"github.com/authzed/openagentprimitives/pkg/platform/artifactstore/blob"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/platform/oap"
	"github.com/authzed/openagentprimitives/pkg/web/admind"
	"github.com/authzed/openagentprimitives/test/oaptest"
)

// --- fixtures ---------------------------------------------------------------

const (
	wkToken     = "test-admind-token"
	wkAdmin     = "user:demo-admin" // holds install_agent / view_sessions / kill_session
	wkNonAdmin  = "user:builder-x"  // holds nothing — the confused-deputy caller
	wkAdminID   = "demo-admin"      // canonical form the checker keys on
	wkWsNS      = "ns-x"            // the Workshop CR's own namespace (session ns)
	wkWsName    = "ws-session-workshop"
	wkSessionNS = "ns-x"
	wkSessionNm = "ws-session"
	wkStarter   = "builder-x"
	wkInstallNS = "agents"     // the admin-chosen install TARGET namespace
	wkInstallNm = "weather-ai" // the admin-chosen install TARGET name
	wkSecretVal = "sk-demo"    // the demo PAT the admin types; MUST never surface
)

// wsChecker answers CheckPlatformPermission per (permission, canonical) so a
// test can grant install_agent to one subject and deny it to another — the
// whole point of the 403 case.
type wsChecker struct {
	grants map[string]bool // "<permission>|<canonical>" → allowed
	err    error
}

func (c wsChecker) CheckPlatformPermission(_ context.Context, permission string, canonical identity.CanonicalUserID, _ bool) (bool, error) {
	if c.err != nil {
		return false, c.err
	}
	return c.grants[permission+"|"+canonical.String()], nil
}
func (c wsChecker) ListPlatformAdmins(context.Context) ([]string, error) { return nil, nil }
func (c wsChecker) CheckAgentIdentityUpdateCredential(context.Context, string, string, identity.CanonicalUserID) (bool, error) {
	return false, nil
}

// wsAdminChecker grants the demo admin every workshop-page permission (they all
// alias can_admin in production).
func wsAdminChecker() wsChecker {
	return wsChecker{grants: map[string]bool{
		"install_agent|" + wkAdminID: true,
		"view_sessions|" + wkAdminID: true,
		"kill_session|" + wkAdminID:  true,
	}}
}

// seedWorkshopBundle packs the shared oaptest fixture, stores it in a mem
// artifactstore under a workshop-export ref, and returns the ref + digest the
// Workshop.status.export would carry.
func seedWorkshopBundle(t *testing.T) (ref, digest string, store *blob.Store) {
	t.Helper()
	dir := oaptest.WriteBundle(t)
	b, err := oap.FromFolder(dir)
	require.NoError(t, err)
	raw, err := oap.Pack(b)
	require.NoError(t, err)
	store = blob.NewMem()
	r, err := store.Put(context.Background(), "workshop-export", bytes.NewReader(raw))
	require.NoError(t, err)
	sum := sha256.Sum256(raw)
	return string(r), hex.EncodeToString(sum[:]), store
}

func workshopCR(export *spiceboxv1alpha1.WorkshopExport, opts ...func(*spiceboxv1alpha1.Workshop)) *spiceboxv1alpha1.Workshop {
	ws := &spiceboxv1alpha1.Workshop{
		ObjectMeta: metav1.ObjectMeta{Namespace: wkWsNS, Name: wkWsName},
		Spec: spiceboxv1alpha1.WorkshopSpec{
			Session:          spiceboxv1alpha1.NamespacedRef{Namespace: wkSessionNS, Name: wkSessionNm},
			StarterCanonical: wkStarter,
			SidecarToolbox:   "builder-sidecar",
			InstallRequest:   &spiceboxv1alpha1.WorkshopInstallRequest{SuggestedName: wkInstallNm},
		},
		Status: spiceboxv1alpha1.WorkshopStatus{
			Phase:  spiceboxv1alpha1.WorkshopPhaseReady,
			Export: export,
		},
	}
	for _, o := range opts {
		o(ws)
	}
	return ws
}

func exportRef(ref, digest string) *spiceboxv1alpha1.WorkshopExport {
	return &spiceboxv1alpha1.WorkshopExport{ArtifactRef: ref, Digest: digest, ExportedAt: metav1.Now()}
}

func newWorkshopAdmind(t *testing.T, checker admind.PlatformChecker, store artifactstore.Store, objs ...client.Object) (http.Handler, client.Client) {
	t.Helper()
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).
		WithStatusSubresource(&spiceboxv1alpha1.Workshop{}).
		WithObjects(objs...).Build()
	a, err := admind.New(admind.Config{
		Mem:           memory.NewLocal(inmem.NewBackend()),
		K8s:           c,
		Checker:       checker,
		Token:         wkToken,
		Logger:        testr.New(t),
		ArtifactStore: store,
	})
	require.NoError(t, err)
	return a.Handler(), c
}

// installPath is POST /admin/v1/workshops/{ns}/{name}/install for the seeded workshop.
func installPath() string  { return "/admin/v1/workshops/" + wkWsNS + "/" + wkWsName + "/install" }
func declinePath() string  { return "/admin/v1/workshops/" + wkWsNS + "/" + wkWsName + "/decline" }
func workshopPath() string { return "/admin/v1/workshops/" + wkWsNS + "/" + wkWsName }

func installBody(t *testing.T, values map[string]string) string {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"namespace": wkInstallNS,
		"name":      wkInstallNm,
		"values":    values,
	})
	require.NoError(t, err)
	return string(body)
}

func agentClassCount(t *testing.T, c client.Client, ns string) int {
	t.Helper()
	var list spiceboxv1alpha1.AgentClassList
	require.NoError(t, c.List(context.Background(), &list, client.InNamespace(ns)))
	return len(list.Items)
}

func getWorkshop(t *testing.T, c client.Client) *spiceboxv1alpha1.Workshop {
	t.Helper()
	var got spiceboxv1alpha1.Workshop
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: wkWsNS, Name: wkWsName}, &got))
	return &got
}

// --- (a) list ---------------------------------------------------------------

func TestWorkshopList_AdminListsWorkshops(t *testing.T) {
	ref, digest, store := seedWorkshopBundle(t)
	h, _ := newWorkshopAdmind(t, wsAdminChecker(), store, workshopCR(exportRef(ref, digest)))

	w := do(t, h, http.MethodGet, "/admin/v1/workshops", wkToken, wkAdmin, "")
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())

	var rows []struct {
		Namespace         string `json:"namespace"`
		Name              string `json:"name"`
		Session           string `json:"session"`
		Starter           string `json:"starter"`
		PendingInstall    bool   `json:"pendingInstall"`
		PendingCapability bool   `json:"pendingCapability"`
		Exported          bool   `json:"exported"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &rows))
	require.Len(t, rows, 1)
	assert.Equal(t, wkWsNS, rows[0].Namespace)
	assert.Equal(t, wkWsName, rows[0].Name)
	assert.Equal(t, wkSessionNS+"/"+wkSessionNm, rows[0].Session)
	assert.Equal(t, wkStarter, rows[0].Starter)
	assert.True(t, rows[0].PendingInstall, "an exported, undecided install request is pending")
	assert.False(t, rows[0].PendingCapability)
	assert.True(t, rows[0].Exported)
}

func TestWorkshopList_NonAdminForbidden(t *testing.T) {
	ref, digest, store := seedWorkshopBundle(t)
	h, _ := newWorkshopAdmind(t, wsAdminChecker(), store, workshopCR(exportRef(ref, digest)))

	w := do(t, h, http.MethodGet, "/admin/v1/workshops", wkToken, wkNonAdmin, "")
	assert.Equal(t, http.StatusForbidden, w.Code, "a non-admin cannot list workshops")
}

// --- (b) install from the artifactstore -------------------------------------

func TestWorkshopInstall_AdminInstallsFromArtifactStore(t *testing.T) {
	ref, digest, store := seedWorkshopBundle(t)
	targetNS := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: wkInstallNS}}
	h, c := newWorkshopAdmind(t, wsAdminChecker(), store, workshopCR(exportRef(ref, digest)), targetNS)

	w := do(t, h, http.MethodPost, installPath(), wkToken, wkAdmin, installBody(t, map[string]string{"demoToken": wkSecretVal}))
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())

	var resp struct {
		Name string `json:"name"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, wkInstallNm, resp.Name)

	// The bundle was applied under the OPERATOR SA into the admin-chosen target.
	assert.Positive(t, agentClassCount(t, c, wkInstallNS), "install must create an AgentClass in the target namespace")

	// status.install records the decision, the approver, and where it landed.
	got := getWorkshop(t, c)
	require.NotNil(t, got.Status.Install)
	assert.Equal(t, spiceboxv1alpha1.WorkshopInstallPhaseInstalled, got.Status.Install.Phase)
	assert.Equal(t, wkAdminID, got.Status.Install.ApprovedBy)
	assert.Equal(t, wkInstallNS+"/"+wkInstallNm, got.Status.Install.InstalledRef)

	// Invariant #5: the typed secret value never appears in the response body.
	assert.NotContains(t, w.Body.String(), wkSecretVal, "no secret value may appear in a response")
}

// TestWorkshopInstall_PreservesWatcherStamps proves writeWorkshopInstallStatus
// mutates the existing install pointer IN PLACE off a fresh Get: the watcher's
// disjoint RequestedAt/DeliveredAt survive the admin's Installed write (a
// replaced pointer would emit null and delete them, un-deduping the watcher).
func TestWorkshopInstall_PreservesWatcherStamps(t *testing.T) {
	ref, digest, store := seedWorkshopBundle(t)
	targetNS := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: wkInstallNS}}
	stamped := metav1.Now()
	ws := workshopCR(exportRef(ref, digest), func(ws *spiceboxv1alpha1.Workshop) {
		ws.Status.Install = &spiceboxv1alpha1.WorkshopInstallStatus{
			Phase:       spiceboxv1alpha1.WorkshopInstallPhaseRequested,
			RequestedAt: &stamped,
			DeliveredAt: &stamped,
		}
	})
	h, c := newWorkshopAdmind(t, wsAdminChecker(), store, ws, targetNS)

	w := do(t, h, http.MethodPost, installPath(), wkToken, wkAdmin, installBody(t, map[string]string{"demoToken": wkSecretVal}))
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())

	got := getWorkshop(t, c)
	require.NotNil(t, got.Status.Install)
	assert.Equal(t, spiceboxv1alpha1.WorkshopInstallPhaseInstalled, got.Status.Install.Phase)
	assert.NotNil(t, got.Status.Install.RequestedAt, "the watcher's RequestedAt must survive the admin write")
	assert.NotNil(t, got.Status.Install.DeliveredAt, "the watcher's DeliveredAt must survive the admin write")
}

// --- (c) non-admin install is refused before the handler body runs ----------

func TestWorkshopInstall_NonAdminForbiddenNoInstall(t *testing.T) {
	ref, digest, store := seedWorkshopBundle(t)
	targetNS := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: wkInstallNS}}
	h, c := newWorkshopAdmind(t, wsAdminChecker(), store, workshopCR(exportRef(ref, digest)), targetNS)

	w := do(t, h, http.MethodPost, installPath(), wkToken, wkNonAdmin, installBody(t, map[string]string{"demoToken": wkSecretVal}))
	require.Equal(t, http.StatusForbidden, w.Code, "body=%s", w.Body.String())

	assert.Zero(t, agentClassCount(t, c, wkInstallNS), "a denied subject must install nothing")
	assert.Nil(t, getWorkshop(t, c).Status.Install, "a denied subject must not touch status.install")
}

// --- (d) nothing exported yet -----------------------------------------------

func TestWorkshopInstall_NoExportRefused(t *testing.T) {
	// No export recorded; a mem store is present but should never be consulted.
	_, _, store := seedWorkshopBundle(t)
	targetNS := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: wkInstallNS}}
	h, c := newWorkshopAdmind(t, wsAdminChecker(), store, workshopCR(nil), targetNS)

	w := do(t, h, http.MethodPost, installPath(), wkToken, wkAdmin, installBody(t, map[string]string{"demoToken": wkSecretVal}))
	require.GreaterOrEqual(t, w.Code, 400)
	require.Less(t, w.Code, 500, "a workshop with nothing exported is bad input, not a fault; body=%s", w.Body.String())
	assert.Zero(t, agentClassCount(t, c, wkInstallNS))
}

// --- (e) missing required secret question -----------------------------------

func TestWorkshopInstall_MissingRequiredQuestion(t *testing.T) {
	ref, digest, store := seedWorkshopBundle(t)
	targetNS := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: wkInstallNS}}
	h, c := newWorkshopAdmind(t, wsAdminChecker(), store, workshopCR(exportRef(ref, digest)), targetNS)

	// Omit the required "demoToken" secret answer.
	w := do(t, h, http.MethodPost, installPath(), wkToken, wkAdmin, installBody(t, map[string]string{}))
	require.Equal(t, http.StatusBadRequest, w.Code, "body=%s", w.Body.String())

	var resp struct {
		Error     string `json:"error"`
		Questions []struct {
			Name string `json:"name"`
		} `json:"questions"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	names := map[string]bool{}
	for _, q := range resp.Questions {
		names[q.Name] = true
	}
	assert.True(t, names["demoToken"], "the 400 must name the unanswered secret question; got %+v", resp.Questions)
	assert.Zero(t, agentClassCount(t, c, wkInstallNS), "a missing-question install writes nothing")
}

// --- (f) decline ------------------------------------------------------------

func TestWorkshopDecline_WritesDeclined(t *testing.T) {
	ref, digest, store := seedWorkshopBundle(t)
	h, c := newWorkshopAdmind(t, wsAdminChecker(), store, workshopCR(exportRef(ref, digest)))

	w := do(t, h, http.MethodPost, declinePath(), wkToken, wkAdmin, "")
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())

	got := getWorkshop(t, c)
	require.NotNil(t, got.Status.Install)
	assert.Equal(t, spiceboxv1alpha1.WorkshopInstallPhaseDeclined, got.Status.Install.Phase)
	assert.Equal(t, wkAdminID, got.Status.Install.ApprovedBy)
}

func TestWorkshopDecline_NonAdminForbidden(t *testing.T) {
	ref, digest, store := seedWorkshopBundle(t)
	h, c := newWorkshopAdmind(t, wsAdminChecker(), store, workshopCR(exportRef(ref, digest)))

	w := do(t, h, http.MethodPost, declinePath(), wkToken, wkNonAdmin, "")
	require.Equal(t, http.StatusForbidden, w.Code)
	assert.Nil(t, getWorkshop(t, c).Status.Install, "a denied decline must not write status")
}

// --- (g) kill ---------------------------------------------------------------

func TestWorkshopKill_DeletesWorkshopCR(t *testing.T) {
	ref, digest, store := seedWorkshopBundle(t)
	h, c := newWorkshopAdmind(t, wsAdminChecker(), store, workshopCR(exportRef(ref, digest)))

	w := do(t, h, http.MethodDelete, workshopPath(), wkToken, wkAdmin, "")
	assert.Equal(t, http.StatusNoContent, w.Code, "body=%s", w.Body.String())

	err := c.Get(context.Background(), client.ObjectKey{Namespace: wkWsNS, Name: wkWsName}, &spiceboxv1alpha1.Workshop{})
	assert.True(t, apierrors.IsNotFound(err), "the Workshop CR must be deleted; err=%v", err)
}

func TestWorkshopKill_NonAdminForbidden(t *testing.T) {
	ref, digest, store := seedWorkshopBundle(t)
	h, c := newWorkshopAdmind(t, wsAdminChecker(), store, workshopCR(exportRef(ref, digest)))

	w := do(t, h, http.MethodDelete, workshopPath(), wkToken, wkNonAdmin, "")
	require.Equal(t, http.StatusForbidden, w.Code)
	err := c.Get(context.Background(), client.ObjectKey{Namespace: wkWsNS, Name: wkWsName}, &spiceboxv1alpha1.Workshop{})
	assert.NoError(t, err, "a denied kill must not delete the CR")
}

// --- (h) digest binding — the store's bytes must match the recorded digest --

func TestWorkshopInstall_DigestMismatchRefused(t *testing.T) {
	ref, _, store := seedWorkshopBundle(t)
	targetNS := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: wkInstallNS}}
	// The recorded digest disagrees with the stored bytes: store corruption.
	wrong := sha256.Sum256([]byte("not-the-bundle"))
	tampered := exportRef(ref, hex.EncodeToString(wrong[:]))
	h, c := newWorkshopAdmind(t, wsAdminChecker(), store, workshopCR(tampered), targetNS)

	w := do(t, h, http.MethodPost, installPath(), wkToken, wkAdmin, installBody(t, map[string]string{"demoToken": wkSecretVal}))
	require.Equal(t, http.StatusInternalServerError, w.Code, "a digest mismatch is a store-corruption fault; body=%s", w.Body.String())
	assert.Zero(t, agentClassCount(t, c, wkInstallNS), "a digest mismatch must install nothing")
}

// --- nil artifact store fails closed ----------------------------------------

func TestWorkshopInstall_NilStoreFailsClosed(t *testing.T) {
	ref, digest, _ := seedWorkshopBundle(t)
	targetNS := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: wkInstallNS}}
	h, c := newWorkshopAdmind(t, wsAdminChecker(), nil, workshopCR(exportRef(ref, digest)), targetNS)

	w := do(t, h, http.MethodPost, installPath(), wkToken, wkAdmin, installBody(t, map[string]string{"demoToken": wkSecretVal}))
	require.Equal(t, http.StatusInternalServerError, w.Code, "a nil store must fail closed, not panic; body=%s", w.Body.String())
	assert.Zero(t, agentClassCount(t, c, wkInstallNS))
}
