//go:build integration

package admind_test

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-logr/logr/testr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/web/admind"
	// Blank-imported for its cloud.Strategy registration (the "unmanaged
	// cluster" default) — envtest has no real cloud provider, so this is the
	// Strategy pkg/web/admind's capacity hook dispatches to. In production this
	// same registration is internal/cmd/operator's job (internal/cmd/operator/cloudimports.go);
	// this test binary has no such binary wiring it, so it supplies its own,
	// exactly as a real consumer's main package would.
	"github.com/authzed/openagentprimitives/pkg/controllers/testenv"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	_ "github.com/authzed/openagentprimitives/pkg/platform/cloud/unmanaged"
	"github.com/authzed/openagentprimitives/pkg/platform/oap"
	"github.com/authzed/openagentprimitives/pkg/platform/oap/instance"
	"github.com/authzed/openagentprimitives/test/oaptest"
)

// packedDemoAgentBytes loads and packs the shared oaptest fixture into
// real .oap tar bytes — the same shape a real upload or registry pull would
// hand the endpoint, so the test exercises oap.Unpack too, not just a bare
// in-memory Bundle.
func packedDemoAgentBytes(t *testing.T) []byte {
	t.Helper()
	folder, err := oap.FromFolder(oaptest.WriteBundle(t))
	require.NoError(t, err, "load demo-agent fixture folder")
	packed, err := oap.Pack(folder)
	require.NoError(t, err, "pack demo-agent fixture")
	return packed
}

// packedBundleWithPod packs an otherwise-fine bundle whose manifests smuggle a
// privileged Pod alongside a throwaway AgentClass — the exact confused-deputy
// payload the Kind allowlist exists to reject. oap.Pack does not validate, so
// this produces well-formed .oap bytes the endpoint must refuse at Preflight
// (Bundle.Validate), never apply.
func packedBundleWithPod(t *testing.T) []byte {
	t.Helper()
	manifests := "apiVersion: agentprimitives.authzed.com/v1alpha1\n" +
		"kind: AgentClass\n" +
		"metadata:\n  name: throwaway-agent\n" +
		"spec:\n  systemPrompt:\n    inline: throwaway\n" +
		"---\n" +
		"apiVersion: v1\n" +
		"kind: Pod\n" +
		"metadata:\n  name: evil-pod\n" +
		"spec:\n  containers:\n  - name: c\n    image: busybox\n"
	b := &oap.Bundle{
		Manifest:  &oap.Manifest{OapFormatVersion: "1", Agent: oap.Agent{Name: "evil-agent", Version: "1.0.0"}},
		Manifests: []byte(manifests),
	}
	packed, err := oap.Pack(b)
	require.NoError(t, err, "pack bundle carrying a Pod")
	return packed
}

// newOapInstallAdmind builds an Admind whose K8s client is the envtest
// (real-apiserver) client — server-side apply needs a real apiserver, unlike
// the rest of this package's tests, which use the controller-runtime fake
// client. Checker allows "install_agent" only for canonical "granted".
// Clientset is left nil — the capacity check's fail-safe "not configured"
// path, exercised explicitly by TestOapInstall_NilClientset_InstallsAndWarns
// but also implicitly by every other test in this file that installs
// something (none of the oaptest fixture's CRs are a SpiceboxClass, so the
// capacity hook would find nothing to check even with a real Clientset).
func newOapInstallAdmind(t *testing.T, env *testenv.Env) *admind.Admind {
	t.Helper()
	a, err := admind.New(admind.Config{
		Mem:     memory.NewLocal(inmem.NewBackend()),
		K8s:     env.Client,
		Checker: stubChecker{allow: map[string]bool{"granted": true}},
		Token:   "test-token",
		Logger:  testr.New(t),
	})
	require.NoError(t, err)
	return a
}

// newOversizedCapacityAdmind builds an Admind wired with a real typed
// Clientset against the envtest apiserver, and creates one Node small enough
// (1Gi memory) that packedOversizedSpiceboxClassBundle's 4Gi ask provably
// exceeds it — the shared fixture every capacity-question admind test below
// builds on. nodeName must be unique per test (a real object in the shared
// envtest apiserver each testenv.Start(t) boots fresh, but two Creates of the
// same name within one test's cluster would themselves collide).
func newOversizedCapacityAdmind(t *testing.T, env *testenv.Env, nodeName string) *admind.Admind {
	t.Helper()
	require.NoError(t, env.Client.Create(context.Background(), &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: nodeName},
		Status: corev1.NodeStatus{
			Allocatable: corev1.ResourceList{
				corev1.ResourceCPU:              resource.MustParse("2"),
				corev1.ResourceMemory:           resource.MustParse("1Gi"),
				corev1.ResourceEphemeralStorage: resource.MustParse("10Gi"),
			},
		},
	}), "create a small Node so the class is provably oversized")

	cs, err := kubernetes.NewForConfig(env.Cfg)
	require.NoError(t, err, "build a typed clientset against the envtest apiserver")

	a, err := admind.New(admind.Config{
		Mem:       memory.NewLocal(inmem.NewBackend()),
		K8s:       env.Client,
		Checker:   stubChecker{allow: map[string]bool{"granted": true}},
		Token:     "test-token",
		Logger:    testr.New(t),
		Clientset: cs,
	})
	require.NoError(t, err)
	return a
}

// packedOversizedSpiceboxClassBundle packs a minimal AgentClass + a
// SpiceboxClass requesting far more memory (4Gi) than the small Node the
// capacity tests below create — the fixture for the admind capacity-question
// tests. A made-up fixture name, never an example's name.
func packedOversizedSpiceboxClassBundle(t *testing.T) []byte {
	t.Helper()
	b := &oap.Bundle{
		Manifest: &oap.Manifest{
			OapFormatVersion: "1",
			Agent:            oap.Agent{Name: "oversized-agent", Version: "1.0.0"},
		},
		Manifests: []byte("apiVersion: agentprimitives.authzed.com/v1alpha1\n" +
			"kind: AgentClass\n" +
			"metadata:\n" +
			"  name: oversized-agent\n" +
			"spec:\n" +
			"  systemPrompt:\n" +
			"    inline: hi\n" +
			"---\n" +
			"apiVersion: agentprimitives.authzed.com/v1alpha1\n" +
			"kind: SpiceboxClass\n" +
			"metadata:\n" +
			"  name: oversized-sandbox\n" +
			"spec:\n" +
			"  resources:\n" +
			"    cpu: \"500m\"\n" +
			"    memory: \"4Gi\"\n" +
			"    ephemeralStorage: \"1Gi\"\n"),
	}
	packed, err := oap.Pack(b)
	require.NoError(t, err, "pack oversized-SpiceboxClass fixture")
	return packed
}

// TestOapInstall_NilClientset_InstallsAndWarns pins admind.Config.Clientset's
// fail-safe contract: unset (as every other test in this file leaves it)
// means the capacity preflight is skipped with a notice, never an install
// that silently drops it or refuses to serve.
func TestOapInstall_NilClientset_InstallsAndWarns(t *testing.T) {
	env := testenv.Start(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	const ns = "admind-oap-install-nil-clientset"
	require.NoError(t, env.Client.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}), "create test namespace")

	a := newOapInstallAdmind(t, env)
	h := a.Handler()

	req := oapInstallMultipartRequest(t, "test-token", "user:granted", packedDemoAgentBytes(t), map[string]string{
		"namespace": ns,
		"values":    `{"demoToken":"fake-token-value"}`,
	})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	var resp struct {
		Name     string   `json:"name"`
		Warnings []string `json:"warnings"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "demo-class", resp.Name)
	require.NotEmpty(t, resp.Warnings, "a nil Clientset must surface a notice, never skip silently")
	assert.Contains(t, resp.Warnings[0], "capacity check skipped")
}

// TestOapInstall_OversizedSpiceboxClass_400ListsCapacityQuestion is the
// admind-specific behavior this task adds: unlike the desktop installer,
// which auto-fits a capacity clamp from pkg/platform/capacityfit's own suggested
// Default because it has no form to ask with, admind treats an unanswered
// capacity question the same as an unanswered manifest question — a 400 whose
// body lists the question (Default included, for the UI to pre-fill) rather
// than silently shrinking the class.
func TestOapInstall_OversizedSpiceboxClass_400ListsCapacityQuestion(t *testing.T) {
	env := testenv.Start(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	const ns = "admind-oap-install-oversized"
	require.NoError(t, env.Client.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}), "create test namespace")

	a := newOversizedCapacityAdmind(t, env, "admind-oap-install-small-node")
	h := a.Handler()

	req := oapInstallMultipartRequest(t, "test-token", "user:granted", packedOversizedSpiceboxClassBundle(t), map[string]string{
		"namespace": ns,
	})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	require.Equal(t, http.StatusBadRequest, w.Code, "body: %s", w.Body.String())

	var resp struct {
		Error     string         `json:"error"`
		Questions []oap.Question `json:"questions"`
		Warnings  []string       `json:"warnings"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Len(t, resp.Questions, 1, "the oversized class's one exceeded dimension (memory) must be listed")
	require.NotEmpty(t, resp.Warnings, "the suggested clamp's own explanation must ride along with the question, not be dropped")
	assert.Contains(t, resp.Warnings[0], "oversized-sandbox")
	q := resp.Questions[0]
	assert.Equal(t, "capacity.oversized-sandbox.memory", q.Name)
	assert.NotEmpty(t, q.Default, "the UI can pre-fill the suggested clamp value")
	assert.NotEmpty(t, q.Validation)

	// An unconfirmed capacity clamp must apply NOTHING — not even the AgentClass
	// the SpiceboxClass rode in on.
	var ac spiceboxv1alpha1.AgentClass
	getErr := env.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: "oversized-agent"}, &ac)
	assert.True(t, apierrors.IsNotFound(getErr), "nothing may be applied until the admin answers the capacity question")

	// Re-POST with the admin's explicit answer (below the 1Gi ceiling): now it
	// installs, and the applied SpiceboxClass carries the clamped value, not the
	// bundle's original 4Gi ask.
	req2 := oapInstallMultipartRequest(t, "test-token", "user:granted", packedOversizedSpiceboxClassBundle(t), map[string]string{
		"namespace": ns,
		"values":    `{"capacity.oversized-sandbox.memory":"768Mi"}`,
	})
	w2 := httptest.NewRecorder()
	h.ServeHTTP(w2, req2)
	require.Equal(t, http.StatusOK, w2.Code, "body: %s", w2.Body.String())

	var sc spiceboxv1alpha1.SpiceboxClass
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Name: "oversized-sandbox"}, &sc), "SpiceboxClass is cluster-scoped: no namespace on the key")
	assert.Equal(t, "768Mi", sc.Spec.Resources.Memory.String(), "the admin's explicit answer must be applied verbatim")
}

// TestOapInstall_OversizedSpiceboxClass_WithInstallName_400QuestionNameMatchesInstall
// is the C1 regression: an install `name` renames every bundled CR
// (instance.Rename, "<name>-" prefix) BEFORE install.Install invokes its
// ExtraQuestions hook, and capacityfit.Questions names each synthesized
// question from the CR's OWN (already-renamed) metadata.name. A 400 peek that
// looked at pre-rename CRs would ask for "capacity.oversized-sandbox.memory"
// while the real Install call could only ever be satisfied by
// "capacity.acme-oversized-sandbox.memory" — an unescapable dead end where no
// answer the UI collects would ever be accepted. This asserts the 400 names
// the RENAMED question, and that answering exactly that key installs
// successfully with the class renamed AND clamped.
func TestOapInstall_OversizedSpiceboxClass_WithInstallName_400QuestionNameMatchesInstall(t *testing.T) {
	env := testenv.Start(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	const ns = "admind-oap-install-oversized-named"
	require.NoError(t, env.Client.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}), "create test namespace")

	a := newOversizedCapacityAdmind(t, env, "admind-oap-install-named-small-node")
	h := a.Handler()

	const installName = "acme"
	req := oapInstallMultipartRequest(t, "test-token", "user:granted", packedOversizedSpiceboxClassBundle(t), map[string]string{
		"namespace": ns,
		"name":      installName,
	})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	require.Equal(t, http.StatusBadRequest, w.Code, "body: %s", w.Body.String())

	var resp struct {
		Questions []oap.Question `json:"questions"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Len(t, resp.Questions, 1)
	// The question must be named from the RENAMED SpiceboxClass
	// ("acme-oversized-sandbox"), not the bundle's own pre-rename name — this
	// is the exact key a caller must answer for the second POST to be accepted.
	wantName := "capacity.acme-oversized-sandbox.memory"
	assert.Equal(t, wantName, resp.Questions[0].Name, "the 400 must name the question install.Install itself will ask for")

	req2 := oapInstallMultipartRequest(t, "test-token", "user:granted", packedOversizedSpiceboxClassBundle(t), map[string]string{
		"namespace": ns,
		"name":      installName,
		"values":    `{"` + wantName + `":"768Mi"}`,
	})
	w2 := httptest.NewRecorder()
	h.ServeHTTP(w2, req2)
	require.Equal(t, http.StatusOK, w2.Code, "answering the EXACT question the 400 named must install; body: %s", w2.Body.String())

	var sc spiceboxv1alpha1.SpiceboxClass
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Name: installName + "-oversized-sandbox"}, &sc), "the renamed, clamped SpiceboxClass must exist")
	assert.Equal(t, "768Mi", sc.Spec.Resources.Memory.String())
}

// TestOapInstall_OversizedSpiceboxClass_OutOfRangeAnswer_400NotServerError is
// the I3 regression: a capacity answer that fails its own CEL bound (here,
// above the 1Gi ceiling) is the admin's own typo, not a server fault. Before
// this fix it reached install.Install unvalidated and any failure there
// became a flat 500; the pre-validate call over `extra` catches it first.
func TestOapInstall_OversizedSpiceboxClass_OutOfRangeAnswer_400NotServerError(t *testing.T) {
	env := testenv.Start(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	const ns = "admind-oap-install-oversized-badanswer"
	require.NoError(t, env.Client.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}), "create test namespace")

	a := newOversizedCapacityAdmind(t, env, "admind-oap-install-badanswer-small-node")
	h := a.Handler()

	req := oapInstallMultipartRequest(t, "test-token", "user:granted", packedOversizedSpiceboxClassBundle(t), map[string]string{
		"namespace": ns,
		// 8Gi is well above the 1Gi node's ceiling — fails the question's own
		// CEL Validation, exactly the class of mistake an admin can make.
		"values": `{"capacity.oversized-sandbox.memory":"8Gi"}`,
	})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	require.Equal(t, http.StatusBadRequest, w.Code, "an out-of-range capacity answer is a client error (400), not a server fault; body: %s", w.Body.String())

	var ac spiceboxv1alpha1.AgentClass
	getErr := env.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: "oversized-agent"}, &ac)
	assert.True(t, apierrors.IsNotFound(getErr), "a rejected answer must apply nothing")
}

// TestOapInstall_OversizedSpiceboxClass_EmptyAnswer_StillAsksAgain is the I3
// regression for a cleared form field: the admin UI writes "" for a field the
// operator cleared, which must NOT be treated as "answered with an empty
// quantity" (that would fail CEL validation with a confusing parse error
// instead of simply re-asking the same question).
func TestOapInstall_OversizedSpiceboxClass_EmptyAnswer_StillAsksAgain(t *testing.T) {
	env := testenv.Start(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	const ns = "admind-oap-install-oversized-emptyanswer"
	require.NoError(t, env.Client.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}), "create test namespace")

	a := newOversizedCapacityAdmind(t, env, "admind-oap-install-emptyanswer-small-node")
	h := a.Handler()

	req := oapInstallMultipartRequest(t, "test-token", "user:granted", packedOversizedSpiceboxClassBundle(t), map[string]string{
		"namespace": ns,
		"values":    `{"capacity.oversized-sandbox.memory":""}`,
	})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	require.Equal(t, http.StatusBadRequest, w.Code, "body: %s", w.Body.String())

	var resp struct {
		Error     string         `json:"error"`
		Questions []oap.Question `json:"questions"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Len(t, resp.Questions, 1, "an empty-string answer must re-ask the same question, not be accepted as a value")
	assert.Equal(t, "capacity.oversized-sandbox.memory", resp.Questions[0].Name)
}

// newFittingCapacityAdmind builds an Admind wired against a Node roomy
// enough (8Gi memory) that packedOversizedSpiceboxClassBundle's own ask
// (500m/4Gi/1Gi) fits comfortably — the fixture for
// TestOapInstall_StrayCapacityValue_FittingClass_400NotSilentlyDropped, which
// needs capacityfit.Questions to synthesize ZERO questions (the class
// already fits) so a stray capacity.* value has no ExtraQuestions-derived
// question to validate against.
func newFittingCapacityAdmind(t *testing.T, env *testenv.Env, nodeName string) *admind.Admind {
	t.Helper()
	require.NoError(t, env.Client.Create(context.Background(), &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: nodeName},
		Status: corev1.NodeStatus{
			Allocatable: corev1.ResourceList{
				corev1.ResourceCPU:              resource.MustParse("2"),
				corev1.ResourceMemory:           resource.MustParse("8Gi"),
				corev1.ResourceEphemeralStorage: resource.MustParse("20Gi"),
			},
		},
	}), "create a roomy Node so the class provably fits")

	cs, err := kubernetes.NewForConfig(env.Cfg)
	require.NoError(t, err, "build a typed clientset against the envtest apiserver")

	a, err := admind.New(admind.Config{
		Mem:       memory.NewLocal(inmem.NewBackend()),
		K8s:       env.Client,
		Checker:   stubChecker{allow: map[string]bool{"granted": true}},
		Token:     "test-token",
		Logger:    testr.New(t),
		Clientset: cs,
	})
	require.NoError(t, err)
	return a
}

// TestOapInstall_StrayCapacityValue_FittingClass_400NotSilentlyDropped is the
// finding-1 regression: handleOapInstall used to gate its capacity-answer
// pre-validate call (install.Resolve(extra, ...), the only place a
// capacity.* value is checked against "does this name a real question") on
// len(extra) > 0. When the class already fits, the capacity hook derives
// ZERO questions, so a stray capacity.* form value — a typo'd class name, or
// a stale override left over from a since-shrunk class — used to sail
// through unvalidated all the way into install.Install, which either
// silently ignored it or turned it into an opaque 500. Fixed by running the
// pre-validate whenever there is a capacityValues to validate, not only when
// there is a question to validate it against.
func TestOapInstall_StrayCapacityValue_FittingClass_400NotSilentlyDropped(t *testing.T) {
	env := testenv.Start(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	const ns = "admind-oap-install-fitting-strayvalue"
	require.NoError(t, env.Client.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}), "create test namespace")

	a := newFittingCapacityAdmind(t, env, "admind-oap-install-fitting-node")
	h := a.Handler()

	req := oapInstallMultipartRequest(t, "test-token", "user:granted", packedOversizedSpiceboxClassBundle(t), map[string]string{
		"namespace": ns,
		// The class fits this roomy node, so the capacity hook synthesizes NO
		// question at all — this key names no question that exists.
		"values": `{"capacity.oversized-sandbox.memory":"768Mi"}`,
	})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	require.Equal(t, http.StatusBadRequest, w.Code, "a capacity.* value naming no synthesized question must be rejected as unknown, not silently dropped; body: %s", w.Body.String())
	assert.Contains(t, w.Body.String(), "capacity.oversized-sandbox.memory", "the 400 must name the offending key")

	var ac spiceboxv1alpha1.AgentClass
	getErr := env.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: "oversized-agent"}, &ac)
	assert.True(t, apierrors.IsNotFound(getErr), "nothing may be applied when a stray capacity value is rejected")
}

// oapInstallMultipartRequest builds a multipart/form-data POST for
// /admin/v1/agents/oap-install: a "file" upload part carrying oapBytes plus
// the given text fields (namespace, name, values, ...).
func oapInstallMultipartRequest(t *testing.T, token, subject string, oapBytes []byte, fields map[string]string) *http.Request {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", "agent.oap")
	require.NoError(t, err)
	_, err = fw.Write(oapBytes)
	require.NoError(t, err)
	for k, v := range fields {
		require.NoError(t, mw.WriteField(k, v))
	}
	require.NoError(t, mw.Close())

	req := httptest.NewRequest(http.MethodPost, "/admin/v1/agents/oap-install", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if subject != "" {
		req.Header.Set("X-Admin-Subject", subject)
	}
	return req
}

// TestOapInstall_GrantedSubject_InstallsBundleWithProvenance is the Task 3
// happy path: an authorized subject uploads a packed demo-agent .oap with the
// one required question answered → 200, and the AgentClass lands in the
// target namespace carrying the oap-source provenance annotation with
// SourceKind "file" (an upload, not a registry pull).
func TestOapInstall_GrantedSubject_InstallsBundleWithProvenance(t *testing.T) {
	env := testenv.Start(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	const ns = "admind-oap-install-granted"
	require.NoError(t, env.Client.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}), "create test namespace")

	a := newOapInstallAdmind(t, env)
	h := a.Handler()

	req := oapInstallMultipartRequest(t, "test-token", "user:granted", packedDemoAgentBytes(t), map[string]string{
		"namespace": ns,
		"values":    `{"demoToken":"fake-token-value"}`,
	})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	var resp struct {
		Name           string   `json:"name"`
		AppliedKinds   []string `json:"appliedKinds"`
		SecretsCreated int      `json:"secretsCreated"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "demo-class", resp.Name)
	assert.Contains(t, resp.AppliedKinds, "AgentClass")
	assert.Equal(t, 1, resp.SecretsCreated)

	var ac spiceboxv1alpha1.AgentClass
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: "demo-class"}, &ac), "AgentClass must exist in the target namespace")

	raw, ok := ac.Annotations[instance.AnnotationOapSource]
	require.True(t, ok, "AgentClass must carry the oap-source provenance annotation")
	src, err := instance.ParseOapSource(raw)
	require.NoError(t, err)
	assert.Equal(t, "file", src.SourceKind, "an uploaded bundle's provenance is sourceKind=file")

	var sec corev1.Secret
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: "demo-pat"}, &sec), "Secret created from the demoToken answer")
	assert.Equal(t, "fake-token-value", string(sec.Data["token"]))
}

// TestOapInstall_UngrantedSubject_Forbidden confirms the route is actually
// gated by a.require("install_agent", ...): a subject the fake checker denies
// never reaches the handler, regardless of body content.
func TestOapInstall_UngrantedSubject_Forbidden(t *testing.T) {
	env := testenv.Start(t)
	a := newOapInstallAdmind(t, env)
	h := a.Handler()

	req := oapInstallMultipartRequest(t, "test-token", "user:other", packedDemoAgentBytes(t), map[string]string{
		"namespace": "default",
	})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	assert.Equal(t, http.StatusForbidden, w.Code, "install_agent must gate the route")
}

// TestOapInstall_MissingRequiredQuestion_400ListsQuestion is the Task 3/4
// non-interactive-resolve regression: POSTing the demo-agent bundle with no
// "values" leaves its one required question (demoToken — a QSecret with no
// default) unanswered. The endpoint must never prompt; it reports the unmet
// question's FULL typed schema (not just its name) so the admin UI can render
// the right input kind (a masked field for type=secret) without a second
// round trip — and never leaks a secret VALUE (there isn't one yet).
func TestOapInstall_MissingRequiredQuestion_400ListsQuestion(t *testing.T) {
	env := testenv.Start(t)
	a := newOapInstallAdmind(t, env)
	h := a.Handler()

	req := oapInstallMultipartRequest(t, "test-token", "user:granted", packedDemoAgentBytes(t), map[string]string{
		"namespace": "default",
	})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	require.Equal(t, http.StatusBadRequest, w.Code, "body: %s", w.Body.String())

	var resp struct {
		Error     string         `json:"error"`
		Questions []oap.Question `json:"questions"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Len(t, resp.Questions, 1, "exactly the one unanswered required question must be listed")
	q := resp.Questions[0]
	assert.Equal(t, "demoToken", q.Name, "the unanswered required question must be named")
	assert.Equal(t, oap.QSecret, q.Type, "the UI needs the type to render a masked input")
	assert.Equal(t, "Demo PAT", q.Prompt)
	assert.Nil(t, q.Required, "unset Required means default-required=true, mirrored by IsRequired()")
}

// TestOapInstall_DisallowedKind_400AppliesNothing is the confused-deputy
// regression at the endpoint boundary: a granted subject uploads a bundle that
// smuggles a Pod alongside a throwaway AgentClass. Preflight's Bundle.Validate
// must reject it with a 400 and NOTHING may be applied — neither the Pod nor
// the throwaway AgentClass lands, proving the allowlist gates before any write
// under the operator's elevated identity.
func TestOapInstall_DisallowedKind_400AppliesNothing(t *testing.T) {
	env := testenv.Start(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	const ns = "admind-oap-install-disallowed"
	require.NoError(t, env.Client.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}), "create test namespace")

	a := newOapInstallAdmind(t, env)
	h := a.Handler()

	req := oapInstallMultipartRequest(t, "test-token", "user:granted", packedBundleWithPod(t), map[string]string{
		"namespace": ns,
	})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	require.Equal(t, http.StatusBadRequest, w.Code, "a bundle carrying a Pod must be refused; body: %s", w.Body.String())
	assert.Contains(t, w.Body.String(), "disallowed resource kind")

	// Nothing applied: the throwaway AgentClass the payload rode in on must not exist.
	var ac spiceboxv1alpha1.AgentClass
	err := env.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: "throwaway-agent"}, &ac)
	assert.True(t, apierrors.IsNotFound(err), "the refused install must apply nothing, not even its throwaway AgentClass")

	// And the Pod itself must never have been created.
	var pod corev1.Pod
	err = env.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: "evil-pod"}, &pod)
	assert.True(t, apierrors.IsNotFound(err), "the smuggled Pod must never be applied")
}

// packedBundleWithConfigMap packs a minimal AgentClass alongside a ConfigMap
// named cmName holding {"prompt": cmValue} — the fixture for the admind
// conflict/adopt round trip: a same-named foreign ConfigMap the operator
// created outside this install must collide with the bundle's own copy of it.
func packedBundleWithConfigMap(t *testing.T, cmName, cmValue string) []byte {
	t.Helper()
	manifests := "apiVersion: agentprimitives.authzed.com/v1alpha1\n" +
		"kind: AgentClass\n" +
		"metadata:\n  name: conflict-agent\n" +
		"spec:\n  systemPrompt:\n    inline: fake test agent\n" +
		"---\n" +
		"apiVersion: v1\n" +
		"kind: ConfigMap\n" +
		"metadata:\n  name: " + cmName + "\n" +
		"data:\n  prompt: " + cmValue + "\n"
	b := &oap.Bundle{
		Manifest:  &oap.Manifest{OapFormatVersion: "1", Agent: oap.Agent{Name: "conflict-agent", Version: "1.0.0"}},
		Manifests: []byte(manifests),
	}
	packed, err := oap.Pack(b)
	require.NoError(t, err, "pack bundle carrying a ConfigMap")
	return packed
}

// TestOapInstall_ForeignObjectConflict_Returns409ThenAdopts covers the admin
// UI's adopt round trip: the first POST hits a pre-existing foreign
// ConfigMap and comes back 409 with the conflict list, applying nothing; the
// second POST carries "adopt" naming that conflict and succeeds, seizing it.
func TestOapInstall_ForeignObjectConflict_Returns409ThenAdopts(t *testing.T) {
	env := testenv.Start(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	const ns = "admind-oap-install-conflict"
	require.NoError(t, env.Client.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}), "create test namespace")

	a := newOapInstallAdmind(t, env)
	h := a.Handler()

	foreign := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-prompt", Namespace: ns},
		Data:       map[string]string{"prompt": "original"},
	}
	require.NoError(t, env.Client.Create(ctx, foreign), "create pre-existing foreign ConfigMap")

	req := oapInstallMultipartRequest(t, "test-token", "user:granted", packedBundleWithConfigMap(t, "demo-prompt", "from-bundle"), map[string]string{
		"namespace": ns,
	})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	require.Equal(t, http.StatusConflict, w.Code, "a foreign-object collision is a 409, not a 500; body: %s", w.Body.String())

	// oapInstallConflictsResponse is unexported (this file is package
	// admind_test); mirror its wire shape locally, as every other test in this
	// file already does for the 200/400 bodies below.
	var conflictBody struct {
		Error     string `json:"error"`
		Conflicts []struct {
			Kind      string `json:"kind"`
			Namespace string `json:"namespace"`
			Name      string `json:"name"`
			Secret    bool   `json:"secret"`
		} `json:"conflicts"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &conflictBody))
	require.Len(t, conflictBody.Conflicts, 1, "the AgentClass is fresh; only the ConfigMap collides")
	assert.Equal(t, "ConfigMap", conflictBody.Conflicts[0].Kind)
	assert.Equal(t, "demo-prompt", conflictBody.Conflicts[0].Name)
	assert.Equal(t, ns, conflictBody.Conflicts[0].Namespace)
	assert.False(t, conflictBody.Conflicts[0].Secret)

	// Nothing was written by the refused install: the foreign ConfigMap's data
	// is untouched, and the bundle's AgentClass never landed.
	var untouched corev1.ConfigMap
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: "demo-prompt"}, &untouched))
	assert.Equal(t, "original", untouched.Data["prompt"])
	var ac spiceboxv1alpha1.AgentClass
	getErr := env.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: "conflict-agent"}, &ac)
	assert.True(t, apierrors.IsNotFound(getErr), "a refused install must apply nothing, not even its AgentClass")

	// Re-POST with the adopt list — the UI's confirm step.
	req2 := oapInstallMultipartRequest(t, "test-token", "user:granted", packedBundleWithConfigMap(t, "demo-prompt", "from-bundle"), map[string]string{
		"namespace": ns,
		"adopt":     `["ConfigMap/demo-prompt"]`,
	})
	w2 := httptest.NewRecorder()
	h.ServeHTTP(w2, req2)
	require.Equal(t, http.StatusOK, w2.Code, "body: %s", w2.Body.String())

	var okBody struct {
		Name    string   `json:"name"`
		Adopted []string `json:"adopted"`
	}
	require.NoError(t, json.Unmarshal(w2.Body.Bytes(), &okBody))
	assert.Equal(t, []string{"ConfigMap/demo-prompt"}, okBody.Adopted)

	var seized corev1.ConfigMap
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: "demo-prompt"}, &seized))
	assert.Equal(t, "from-bundle", seized.Data["prompt"], "the adopted ConfigMap converged to the bundle's data")
}

// TestOapInstall_SystemNamespace_400 is the defense-in-depth guard: an install
// targeting a Kubernetes system namespace (or the control plane's own) is
// refused with a 400 before any bundle is even loaded — a crafted payload
// aiming at kube-system / agentprimitives-system never reaches install.Install.
func TestOapInstall_SystemNamespace_400(t *testing.T) {
	env := testenv.Start(t)
	a := newOapInstallAdmind(t, env)
	h := a.Handler()

	for _, ns := range []string{"kube-system", "kube-public", "kube-node-lease", "agentprimitives-system"} {
		t.Run(ns+" → 400", func(t *testing.T) {
			req := oapInstallMultipartRequest(t, "test-token", "user:granted", packedDemoAgentBytes(t), map[string]string{
				"namespace": ns,
				"values":    `{"demoToken":"fake-token-value"}`,
			})
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)
			require.Equal(t, http.StatusBadRequest, w.Code, "system namespace must be refused; body: %s", w.Body.String())
			assert.Contains(t, w.Body.String(), "system namespace")
		})
	}
}
