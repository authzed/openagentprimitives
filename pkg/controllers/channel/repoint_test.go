package channel_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/github" // register the github kind
	"github.com/authzed/openagentprimitives/pkg/controllers/channel"
	"github.com/authzed/openagentprimitives/pkg/controllers/testfixtures"
	"github.com/authzed/openagentprimitives/pkg/platform/cloud"
)

// fakeAppAPI stands in for GitHub's App-configuration API: `GET /app` reads
// the currently-registered hook URL back, `PATCH /app/hook/config` writes it.
// It is STATEFUL — a PATCH changes what the next GET reports — because the
// whole point of these tests is what the controller does across the two, and
// a fake whose read never reflects its own write would let "repointed" and
// "did nothing" look alike on the second reconcile.
//
// Counters are mutex-guarded rather than bare ints: the handler runs on the
// server's goroutine and the assertions on the test's.
type fakeAppAPI struct {
	srv *httptest.Server

	mu            sync.Mutex
	registeredURL string
	repointed     []string
	reads         int
	patchStatus   int // 0 means 200; set to fail the write
}

// newFakeAppAPI starts a stand-in App API whose registered hook URL starts at
// registered. It is closed via t.Cleanup, so no test leaves a listener behind.
func newFakeAppAPI(t *testing.T, registered string) *fakeAppAPI {
	t.Helper()
	f := &fakeAppAPI{registeredURL: registered}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case r.Method == http.MethodPatch && r.URL.Path == "/app/hook/config":
			body, err := io.ReadAll(io.LimitReader(r.Body, 1<<16))
			if err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			var update struct {
				URL string `json:"url"`
			}
			if err := json.Unmarshal(body, &update); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			// Recorded before the outcome: an attempted write against an App
			// is the thing the provenance rule forbids, whether or not the
			// provider accepted it.
			f.repointed = append(f.repointed, update.URL)
			if f.patchStatus != 0 {
				w.WriteHeader(f.patchStatus)
				return
			}
			f.registeredURL = update.URL
			_, _ = w.Write([]byte(`{}`))
		case r.Method == http.MethodGet && r.URL.Path == "/app":
			f.reads++
			_, _ = w.Write([]byte(`{"hook_attributes":{"url":"` + f.registeredURL + `"}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// RepointedURLs is every URL the controller ATTEMPTED to write to the App, in
// order, refused writes included. The Channel a write belongs to is
// identifiable from the URL itself: the webhook path carries the Channel's
// namespace and name.
func (f *fakeAppAPI) RepointedURLs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.repointed...)
}

// ReadAppConfigCalls counts reads of the App's configuration — the outbound
// call the drift check throttles, and the one a repoint must never need.
func (f *fakeAppAPI) ReadAppConfigCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reads
}

// FailRepointWith makes every subsequent write fail with the given status.
func (f *fakeAppAPI) FailRepointWith(status int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.patchStatus = status
}

// provisionedByUs stamps the provenance marker a github wizard writes on the
// Channel only when its own exchange registered the App.
func provisionedByUs(ch *spiceboxv1alpha1.Channel) *spiceboxv1alpha1.Channel {
	ch.Annotations = map[string]string{
		channelkinds.AnnotationAppProvisionedBy: channelkinds.AppProvisionedByOAP,
	}
	return ch
}

// readyEndpoint is a webd-targeting PublicEndpoint with a live public URL —
// the state that makes a repoint due. PublicEndpoint is not registered with
// the fake client's status subresource, so the status set here survives.
func readyEndpoint(url string) *spiceboxv1alpha1.PublicEndpoint {
	return endpointWithPhase(url, spiceboxv1alpha1.PublicEndpointPhaseReady)
}

func endpointWithPhase(url, phase string) *spiceboxv1alpha1.PublicEndpoint {
	return &spiceboxv1alpha1.PublicEndpoint{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-endpoint"},
		Spec: spiceboxv1alpha1.PublicEndpointSpec{
			Target: spiceboxv1alpha1.PublicEndpointTarget{
				Namespace: cloud.WebdServiceNamespace,
				Service:   cloud.WebdServiceName,
				Port:      8080,
			},
			Provider: "ngrok",
			LocalURL: "http://localhost:8080",
			AuthTokenRef: spiceboxv1alpha1.ClusterSecretKeyRef{
				Namespace: cloud.WebdServiceNamespace, Name: "demo-tunnel-token", Key: "token",
			},
		},
		Status: spiceboxv1alpha1.PublicEndpointStatus{URL: url, Phase: phase},
	}
}

// newRepointReconciler builds the reconciler under test over the given
// objects, pointed at the stand-in App API. ExternalBaseURL is deliberately
// left nil: a repoint decision must come from the PublicEndpoint's status.url,
// never from the ConfigMap-derived accessor, which also carries the loopback
// address while no tunnel is up.
func newRepointReconciler(t *testing.T, gh *fakeAppAPI, objs ...client.Object) (*channel.Reconciler, client.Client) {
	t.Helper()
	// rbacv1: the reconcile stamps the per-Channel webhook-secret Role +
	// RoleBinding (webhookrbac.go), the same as the operator's own scheme.
	scheme := testfixtures.NewScheme(t, rbacv1.AddToScheme)
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(objs...).
		WithStatusSubresource(&spiceboxv1alpha1.Channel{}).Build()

	r := newUnitReconciler(c)
	r.GitHubAPIBaseURL = gh.srv.URL
	return r, c
}

func reconcileChannel(t *testing.T, r *channel.Reconciler, name string) reconcile.Result {
	t.Helper()
	res, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Namespace: "default", Name: name},
	})
	require.NoError(t, err, "a webhook-URL repoint is reported on status, never by failing the reconcile")
	return res
}

func loadChannel(t *testing.T, c client.Client, name string) *spiceboxv1alpha1.Channel {
	t.Helper()
	var got spiceboxv1alpha1.Channel
	require.NoError(t, c.Get(context.Background(),
		types.NamespacedName{Namespace: "default", Name: name}, &got))
	return &got
}

func driftCondition(ch *spiceboxv1alpha1.Channel) *metav1.Condition {
	return meta.FindStatusCondition(ch.Status.Conditions, spiceboxv1alpha1.ChannelConditionWebhookURLDrift)
}

func driftConditionTrue(ch *spiceboxv1alpha1.Channel) bool {
	c := driftCondition(ch)
	return c != nil && c.Status == metav1.ConditionTrue
}

func driftMessage(ch *spiceboxv1alpha1.Channel) string {
	c := driftCondition(ch)
	if c == nil {
		return ""
	}
	return c.Message
}

// TestRepoint_OnlyTouchesAnAppThisToolProvisioned is the whole authorization
// rule in one test: the provenance marker, and nothing else, is what lets this
// controller make an outward-facing write against someone's GitHub App.
func TestRepoint_OnlyTouchesAnAppThisToolProvisioned(t *testing.T) {
	gh := newFakeAppAPI(t, "https://old.demo.test/webhooks/github/default/legacy")
	marked := provisionedByUs(githubChannel("demo-marked", "gh-creds"))
	unmarked := githubChannel("demo-unmarked", "gh-creds")

	r, c := newRepointReconciler(t, gh, marked, unmarked, githubSecret(t, "gh-creds"),
		readyEndpoint("https://demo-tunnel.test"))

	reconcileChannel(t, r, "demo-marked")
	reconcileChannel(t, r, "demo-unmarked")

	assert.Equal(t, []string{"https://demo-tunnel.test/webhooks/github/default/demo-marked"},
		gh.RepointedURLs(),
		"an App we did not create is never written to — only reported")

	assert.True(t, driftConditionTrue(loadChannel(t, c, "demo-unmarked")),
		"the unmarked one still gets today's behaviour: a finding for a human")
}

// TestRepoint_ClearsTheDriftConditionOnSuccess: a repoint that leaves the
// drift condition True gives the operator a permanent false alarm about a URL
// this controller just corrected.
func TestRepoint_ClearsTheDriftConditionOnSuccess(t *testing.T) {
	gh := newFakeAppAPI(t, "https://old.demo.test/webhooks/github/default/demo-marked")
	marked := provisionedByUs(githubChannel("demo-marked", "gh-creds"))

	r, c := newRepointReconciler(t, gh, marked, githubSecret(t, "gh-creds"),
		readyEndpoint("https://demo-tunnel.test"))

	reconcileChannel(t, r, "demo-marked")

	got := loadChannel(t, c, "demo-marked")
	assert.False(t, driftConditionTrue(got),
		"the URL this controller just wrote is not drift")
	require.NotNil(t, driftCondition(got),
		"a corrected URL is still reported, so an operator can see the controller acted")
}

// TestRepoint_FailureFallsBackToTheDriftCondition: a write GitHub refused
// leaves the webhook pointed somewhere wrong, which is exactly the fact the
// drift condition exists to carry. It must also stay un-recorded, so the next
// reconcile tries again rather than believing the URL is correct.
func TestRepoint_FailureFallsBackToTheDriftCondition(t *testing.T) {
	gh := newFakeAppAPI(t, "https://old.demo.test/webhooks/github/default/demo-marked")
	gh.FailRepointWith(http.StatusNotFound)
	marked := provisionedByUs(githubChannel("demo-marked", "gh-creds"))

	r, c := newRepointReconciler(t, gh, marked, githubSecret(t, "gh-creds"),
		readyEndpoint("https://demo-tunnel.test"))

	res := reconcileChannel(t, r, "demo-marked")

	got := loadChannel(t, c, "demo-marked")
	assert.True(t, driftConditionTrue(got))
	assert.Contains(t, driftMessage(got), "404", "a failed repoint is never silent")
	assert.Contains(t, driftMessage(got), "/app/hook/config",
		"the message names the call that failed, not just that something did")
	assert.Greater(t, res.RequeueAfter, time.Duration(0),
		"a failed repoint must schedule its own retry; the webhook is broken until it lands")

	reconcileChannel(t, r, "demo-marked")
	assert.Len(t, gh.RepointedURLs(), 2,
		"a refused write is not recorded as done, so the next reconcile tries again")
}

// TestRepoint_MakesNoGitHubReadToDecideWhetherToRepoint: the throttle protects
// a rate limit this cluster does not control, and whose exhaustion breaks
// token minting as well. A repoint must not become a reason to read.
func TestRepoint_MakesNoGitHubReadToDecideWhetherToRepoint(t *testing.T) {
	gh := newFakeAppAPI(t, "https://old.demo.test/webhooks/github/default/demo-marked")
	marked := provisionedByUs(githubChannel("demo-marked", "gh-creds"))

	r, _ := newRepointReconciler(t, gh, marked, githubSecret(t, "gh-creds"),
		readyEndpoint("https://demo-tunnel.test"))

	reconcileChannel(t, r, "demo-marked")

	assert.Zero(t, gh.ReadAppConfigCalls(),
		"the expected URL is derived locally; GitHub is called only to write")
}

// TestRepoint_DoesNotWriteAgainOnceTheURLIsCorrect is the write-side of the
// same rate-limit argument: repointing on every reconcile would spend the
// App's budget on a value that has not changed since the last write.
func TestRepoint_DoesNotWriteAgainOnceTheURLIsCorrect(t *testing.T) {
	gh := newFakeAppAPI(t, "https://old.demo.test/webhooks/github/default/demo-marked")
	marked := provisionedByUs(githubChannel("demo-marked", "gh-creds"))

	r, _ := newRepointReconciler(t, gh, marked, githubSecret(t, "gh-creds"),
		readyEndpoint("https://demo-tunnel.test"))

	reconcileChannel(t, r, "demo-marked")
	reconcileChannel(t, r, "demo-marked")

	assert.Len(t, gh.RepointedURLs(), 1,
		"an unchanged expected URL is not a reason to write again")
}

// TestRepoint_CorrectsAChangeMadeAtTheProvider covers the one mismatch local
// state cannot predict: the expected URL never moved, but someone edited the
// App's settings by hand. The throttled read is what finds it, and on an App
// this tool provisioned the answer is to put it back — not to file a finding
// telling a human to do what this controller is allowed to do itself.
func TestRepoint_CorrectsAChangeMadeAtTheProvider(t *testing.T) {
	const expected = "https://demo-tunnel.test/webhooks/github/default/demo-marked"

	gh := newFakeAppAPI(t, "https://someone-elses.demo.test/hook")
	marked := provisionedByUs(githubChannel("demo-marked", "gh-creds"))
	// This URL was written, and landed, before the edit at the provider — so
	// the local comparison has nothing to say and only the read can catch it.
	marked.Status.RepointedWebhookURL = expected

	r, c := newRepointReconciler(t, gh, marked, githubSecret(t, "gh-creds"),
		readyEndpoint("https://demo-tunnel.test"))

	reconcileChannel(t, r, "demo-marked")

	assert.Equal(t, 1, gh.ReadAppConfigCalls(),
		"the periodic read is the only thing that can see an out-of-band edit")
	assert.Equal(t, []string{expected}, gh.RepointedURLs(),
		"an App this tool provisioned is put back, not merely reported")
	assert.False(t, driftConditionTrue(loadChannel(t, c, "demo-marked")),
		"a mismatch this reconcile corrected is not left standing as a finding")
}

// TestRepoint_SkippedWhileTheEndpointIsNotReady guards the fail-closed half:
// a PublicEndpoint that exists but has no live URL yet must not be answered
// from anywhere else. The ConfigMap-derived base carries the LOOPBACK address
// in that window, and writing that to GitHub would replace a working webhook
// with one no delivery can ever reach.
func TestRepoint_SkippedWhileTheEndpointIsNotReady(t *testing.T) {
	gh := newFakeAppAPI(t, "https://old.demo.test/webhooks/github/default/demo-marked")
	marked := provisionedByUs(githubChannel("demo-marked", "gh-creds"))

	r, c := newRepointReconciler(t, gh, marked, githubSecret(t, "gh-creds"),
		endpointWithPhase("", spiceboxv1alpha1.PublicEndpointPhasePending))
	r.ExternalBaseURL = func() string { return "http://127.0.0.1:8080" }

	reconcileChannel(t, r, "demo-marked")

	assert.Empty(t, gh.RepointedURLs(), "a loopback address must never be written to GitHub")
	assert.Zero(t, gh.ReadAppConfigCalls(), "nor is it something to compare against")
	assert.Nil(t, driftCondition(loadChannel(t, c, "demo-marked")),
		"with no address to expect, there is no drift to report")
}

// TestRepoint_SkippedWhenNoEndpointExistsAndTheBaseURLIsALoopback is the OTHER
// half of the loopback refusal, and it was the reachable one:
// TestRepoint_SkippedWhileTheEndpointIsNotReady covers "an endpoint exists but
// has no URL yet", while "no endpoint at all" fell straight through to the
// ConfigMap-derived base.
//
// That state is not exotic. `desktop`'s policy opens an endpoint only on
// demand, so a desktop that has never declared a webhook channel has none —
// while the desktop itself writes http://127.0.0.1:<port> into the ConfigMap
// this base is read from on every boot. The result was a PATCH registering a
// loopback address as the App's webhook URL, retried every drift interval for
// as long as the cluster lived. GitHub accepts that write; only the deliveries
// stop.
func TestRepoint_SkippedWhenNoEndpointExistsAndTheBaseURLIsALoopback(t *testing.T) {
	gh := newFakeAppAPI(t, "https://old.demo.test/webhooks/github/default/demo-marked")
	marked := provisionedByUs(githubChannel("demo-marked", "gh-creds"))

	// No PublicEndpoint on the cluster at all — the one difference from
	// TestRepoint_SkippedWhileTheEndpointIsNotReady.
	r, c := newRepointReconciler(t, gh, marked, githubSecret(t, "gh-creds"))
	r.ExternalBaseURL = func() string { return "http://127.0.0.1:17080" }

	reconcileChannel(t, r, "demo-marked")

	assert.Empty(t, gh.RepointedURLs(), "a loopback address must never be written to GitHub")
	assert.Zero(t, gh.ReadAppConfigCalls(), "nor is it something to compare against")
	assert.Nil(t, driftCondition(loadChannel(t, c, "demo-marked")),
		"with no address to expect, there is no drift to report")
}

// TestRepoint_StillUsesTheBaseURLOnAClusterWithRealIngress is the control for
// the test above: the no-endpoint branch is the ONLY answer a cluster with real
// ingress has, and narrowing it must not close it. A genuine https:// host is
// still the expected address, and a marked App is still repointed onto it.
func TestRepoint_StillUsesTheBaseURLOnAClusterWithRealIngress(t *testing.T) {
	gh := newFakeAppAPI(t, "https://old.demo.test/webhooks/github/default/demo-marked")
	marked := provisionedByUs(githubChannel("demo-marked", "gh-creds"))

	r, _ := newRepointReconciler(t, gh, marked, githubSecret(t, "gh-creds"))
	r.ExternalBaseURL = func() string { return "https://webd.demo.test" }

	reconcileChannel(t, r, "demo-marked")

	assert.Equal(t, []string{"https://webd.demo.test/webhooks/github/default/demo-marked"},
		gh.RepointedURLs(),
		"a cluster reached through its own ingress has no PublicEndpoint and must still get its webhook repointed")
}
