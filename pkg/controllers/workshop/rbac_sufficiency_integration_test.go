//go:build integration

package workshop_test

import (
	"context"
	"os"
	"path/filepath"
	goruntime "runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	authzv1 "k8s.io/api/authorization/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	sigyaml "sigs.k8s.io/yaml"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/testenv"
	"github.com/authzed/openagentprimitives/pkg/controllers/workshop"
	"github.com/authzed/openagentprimitives/pkg/memory/tokens"
)

// applyToolwriterClusterRole installs the shipped spicebox-workshop-toolwriter
// ClusterRole from its SOURCE manifest, so the cluster-scoped SubjectAccess
// Reviews below resolve the workshop SA's tool grant against the exact verbs
// that actually ship (config/manager/workshop-toolwriter.yaml) rather than a
// hand-copied list — re-add delete/get there and the DENIED rows go red. The
// Workshop reconciler creates the per-workshop ClusterRoleBinding to this
// ClusterRole; the ClusterRole itself is a static manifest, so the envtest must
// load it for the grant to bind.
func applyToolwriterClusterRole(t *testing.T, ctx context.Context, env *testenv.Env) {
	t.Helper()
	_, thisFile, _, ok := goruntime.Caller(0)
	require.True(t, ok, "locate this test file to derive the repo root")
	repoRoot := filepath.Join(filepath.Dir(thisFile), "..", "..", "..")
	b, err := os.ReadFile(filepath.Join(repoRoot, "config", "manager", "workshop-toolwriter.yaml"))
	require.NoError(t, err, "read the workshop-toolwriter ClusterRole source manifest")
	var cr rbacv1.ClusterRole
	require.NoError(t, sigyaml.Unmarshal(b, &cr), "unmarshal the workshop-toolwriter ClusterRole")
	require.NotEmpty(t, cr.Name, "the source manifest must name the ClusterRole")
	if err := env.Client.Create(ctx, &cr); err != nil && !apierrors.IsAlreadyExists(err) {
		require.NoError(t, err, "create the shipped toolwriter ClusterRole")
	}
}

// applyReaderClusterRole is applyToolwriterClusterRole's twin for the OTHER
// static ClusterRole the Workshop reconciler binds per workshop
// (config/manager/workshop-reader.yaml) — loaded from its source manifest for
// the same reason: the SARs below must resolve against the exact verbs that
// actually ship, not a hand-copied list.
func applyReaderClusterRole(t *testing.T, ctx context.Context, env *testenv.Env) {
	t.Helper()
	_, thisFile, _, ok := goruntime.Caller(0)
	require.True(t, ok, "locate this test file to derive the repo root")
	repoRoot := filepath.Join(filepath.Dir(thisFile), "..", "..", "..")
	b, err := os.ReadFile(filepath.Join(repoRoot, "config", "manager", "workshop-reader.yaml"))
	require.NoError(t, err, "read the workshop-reader ClusterRole source manifest")
	var cr rbacv1.ClusterRole
	require.NoError(t, sigyaml.Unmarshal(b, &cr), "unmarshal the workshop-reader ClusterRole")
	require.NotEmpty(t, cr.Name, "the source manifest must name the ClusterRole")
	if err := env.Client.Create(ctx, &cr); err != nil && !apierrors.IsAlreadyExists(err) {
		require.NoError(t, err, "create the shipped reader ClusterRole")
	}
}

// TestWorkshopRBACSufficiency is the sufficiency harness for the ONE Role
// this package builds at runtime (the workshop namespace's Role is
// provisioned per-workshop, never shipped as a static manifest, so it
// belongs in THIS package's envtest — pkg/platform/manifests/
// rbac_sufficiency_test.go covers only the static, shipped ClusterRoles).
//
// It asserts BOTH directions of lockdown layer 1.2 against a REAL,
// RBAC-enforcing apiserver via SubjectAccessReview (side-effect-free even
// for create/delete verbs — the same technique
// pkg/platform/manifests/rbac_sufficiency_test.go's allowed() uses):
//
//   - the workshop SA gets EXACTLY the closed kind set, scoped to its OWN
//     workshop namespace;
//   - the runner SA — every other identity in the session's namespace,
//     including the one every AgentSession already has — gets NONE of it.
//
// A green run without the negative controls would be vacuous: an
// authorizer that ALWAYS allows would pass every positive assertion too.
func TestWorkshopRBACSufficiency(t *testing.T) {
	env := testenv.Shared(t)
	ctx := context.Background()

	sess := builderSession("wsbuild-sar", "placeholder-overwritten-by-apiserver")
	sess.Spec = spiceboxv1alpha1.AgentSessionSpec{
		Class:  "workshop-test-class",
		Prompt: spiceboxv1alpha1.PromptSource{Inline: "build something"},
	}
	require.NoError(t, env.Client.Create(ctx, sess), "create AgentSession")

	ws := sanctionedWorkshop(sess)
	require.NoError(t, env.Client.Create(ctx, ws), "create Workshop")

	r := &workshop.Reconciler{
		Client:    env.Client,
		APIReader: env.Client,
		Tuples:    &fakeTuples{},
		Tokens:    tokens.NewRegistry(),
	}
	key := types.NamespacedName{Namespace: ws.Namespace, Name: ws.Name}
	reconcileUntilReady(t, ctx, r, key)

	var got spiceboxv1alpha1.Workshop
	require.NoError(t, env.Client.Get(ctx, key, &got))
	nsName := got.Status.Namespace
	require.NotEmpty(t, nsName)

	// The reconciler created the per-workshop ClusterRoleBinding to the shipped
	// spicebox-workshop-toolwriter ClusterRole; load that ClusterRole so the
	// cluster-scoped tool-kind SARs below actually resolve the grant.
	applyToolwriterClusterRole(t, ctx, env)
	// Same for the reader CRB (inventory's read of ClusterAgentSettings/
	// ClusterSkill/SpiceboxClass).
	applyReaderClusterRole(t, ctx, env)

	cs, err := kubernetes.NewForConfig(env.Cfg)
	require.NoError(t, err, "build clientset for SubjectAccessReview")

	workshopSAUser := "system:serviceaccount:" + ws.Namespace + ":" + spiceboxv1alpha1.WorkshopServiceAccountName(sess.Name)
	runnerSAUser := "system:serviceaccount:" + ws.Namespace + ":" + sess.Name + "-runner-sa"

	const apGroup = "agentprimitives.authzed.com"

	cases := []struct {
		name string
		user string
		req  authzv1.ResourceAttributes
		want bool
	}{
		{
			name: "workshop SA: ALLOWED create agentclasses in its own workshop namespace",
			user: workshopSAUser,
			req:  authzv1.ResourceAttributes{Namespace: nsName, Verb: "create", Group: apGroup, Resource: "agentclasses"},
			want: true,
		},
		{
			name: "workshop SA: DENIED get secrets in the workshop namespace",
			user: workshopSAUser,
			req:  authzv1.ResourceAttributes{Namespace: nsName, Verb: "get", Group: "", Resource: "secrets"},
			want: false,
		},
		{
			name: "workshop SA: DENIED create pods in the workshop namespace",
			user: workshopSAUser,
			req:  authzv1.ResourceAttributes{Namespace: nsName, Verb: "create", Group: "", Resource: "pods"},
			want: false,
		},
		{
			name: "workshop SA: DENIED create agentclasses in a DIFFERENT namespace",
			user: workshopSAUser,
			req:  authzv1.ResourceAttributes{Namespace: "some-other-namespace", Verb: "create", Group: apGroup, Resource: "agentclasses"},
			want: false,
		},
		{
			name: "workshop SA: DENIED get secrets in the SESSION's namespace",
			user: workshopSAUser,
			req:  authzv1.ResourceAttributes{Namespace: sess.Namespace, Verb: "get", Group: "", Resource: "secrets"},
			want: false,
		},
		{
			name: "runner SA: DENIED create agentclasses in the workshop namespace — the runner gets NOTHING (layer 1.2)",
			user: runnerSAUser,
			req:  authzv1.ResourceAttributes{Namespace: nsName, Verb: "create", Group: apGroup, Resource: "agentclasses"},
			want: false,
		},

		// WorkshopProbe (plan-3a, Task 5's own narrower rule, distinct from
		// workshopKindResources' CRUD set above): the workshop SA gets exactly
		// create/get/list/watch — enough for probe_image/cli_help
		// (internal/cmd/workshop/tools_probe.go) to create a probe and poll it
		// back — and NOT update/delete, since a probe is fire-once,
		// spec-immutable, and its status is written only by the operator's
		// WorkshopProbe controller.
		{
			name: "workshop SA: ALLOWED create workshopprobes in its own workshop namespace",
			user: workshopSAUser,
			req:  authzv1.ResourceAttributes{Namespace: nsName, Verb: "create", Group: apGroup, Resource: "workshopprobes"},
			want: true,
		},
		{
			name: "workshop SA: ALLOWED get workshopprobes in its own workshop namespace",
			user: workshopSAUser,
			req:  authzv1.ResourceAttributes{Namespace: nsName, Verb: "get", Group: apGroup, Resource: "workshopprobes"},
			want: true,
		},
		{
			name: "workshop SA: ALLOWED list workshopprobes in its own workshop namespace",
			user: workshopSAUser,
			req:  authzv1.ResourceAttributes{Namespace: nsName, Verb: "list", Group: apGroup, Resource: "workshopprobes"},
			want: true,
		},
		{
			name: "workshop SA: ALLOWED watch workshopprobes in its own workshop namespace",
			user: workshopSAUser,
			req:  authzv1.ResourceAttributes{Namespace: nsName, Verb: "watch", Group: apGroup, Resource: "workshopprobes"},
			want: true,
		},
		{
			name: "workshop SA: DENIED update workshopprobes — fire-once, spec-immutable, controller-owned status",
			user: workshopSAUser,
			req:  authzv1.ResourceAttributes{Namespace: nsName, Verb: "update", Group: apGroup, Resource: "workshopprobes"},
			want: false,
		},
		{
			name: "workshop SA: DENIED delete workshopprobes — reaped with the workshop namespace, not individually",
			user: workshopSAUser,
			req:  authzv1.ResourceAttributes{Namespace: nsName, Verb: "delete", Group: apGroup, Resource: "workshopprobes"},
			want: false,
		},
		{
			name: "workshop SA: DENIED create workshopprobes in a DIFFERENT namespace",
			user: workshopSAUser,
			req:  authzv1.ResourceAttributes{Namespace: "some-other-namespace", Verb: "create", Group: apGroup, Resource: "workshopprobes"},
			want: false,
		},
		{
			name: "runner SA: DENIED create workshopprobes in the workshop namespace — the runner gets NOTHING (layer 1.2)",
			user: runnerSAUser,
			req:  authzv1.ResourceAttributes{Namespace: nsName, Verb: "create", Group: apGroup, Resource: "workshopprobes"},
			want: false,
		},

		// SubagentRequest (task-7, plan-4b sidecar test tools): part of
		// workshopKindResources above, so it gets the SAME full CRUD grant as
		// agentclasses — the four sidecar test tools (create/get/list/delete)
		// author SubagentRequest CRs in the workshop namespace to simulate a
		// class-builder session invoking a subagent's tools without a real
		// runner ever needing to exist. This locks that grant against a future
		// trim to workshopKindResources.
		{
			name: "workshop SA: ALLOWED create subagentrequests in its own workshop namespace",
			user: workshopSAUser,
			req:  authzv1.ResourceAttributes{Namespace: nsName, Verb: "create", Group: apGroup, Resource: "subagentrequests"},
			want: true,
		},
		{
			name: "workshop SA: ALLOWED get subagentrequests in its own workshop namespace",
			user: workshopSAUser,
			req:  authzv1.ResourceAttributes{Namespace: nsName, Verb: "get", Group: apGroup, Resource: "subagentrequests"},
			want: true,
		},
		{
			name: "workshop SA: ALLOWED list subagentrequests in its own workshop namespace",
			user: workshopSAUser,
			req:  authzv1.ResourceAttributes{Namespace: nsName, Verb: "list", Group: apGroup, Resource: "subagentrequests"},
			want: true,
		},
		{
			name: "workshop SA: ALLOWED watch subagentrequests in its own workshop namespace",
			user: workshopSAUser,
			req:  authzv1.ResourceAttributes{Namespace: nsName, Verb: "watch", Group: apGroup, Resource: "subagentrequests"},
			want: true,
		},
		{
			name: "workshop SA: ALLOWED delete subagentrequests in its own workshop namespace",
			user: workshopSAUser,
			req:  authzv1.ResourceAttributes{Namespace: nsName, Verb: "delete", Group: apGroup, Resource: "subagentrequests"},
			want: true,
		},
		{
			name: "workshop SA: DENIED create subagentrequests in a DIFFERENT namespace — the Role is scoped to its own workshop namespace, it does not leak",
			user: workshopSAUser,
			req:  authzv1.ResourceAttributes{Namespace: "some-other-namespace", Verb: "create", Group: apGroup, Resource: "subagentrequests"},
			want: false,
		},
		{
			name: "runner SA: DENIED create subagentrequests in the workshop namespace — the verb is the workshop SA's, not universal (layer 1.2)",
			user: runnerSAUser,
			req:  authzv1.ResourceAttributes{Namespace: nsName, Verb: "create", Group: apGroup, Resource: "subagentrequests"},
			want: false,
		},

		// The two cluster-scoped tool kinds (empty Namespace): the workshop SA
		// gets create (the ONE verb the admission webhook can gate, dialed as
		// CREATE/UPDATE) but NOT the two verbs no ValidatingWebhook can
		// intercept and RBAC cannot scope by name or label — delete (a
		// cluster-wide destructive reach on production and other workshops'
		// tool CRs) and get (a cross-tenant read by name).
		{
			name: "workshop SA: ALLOWED create spiceboxtoolspecs (cluster-scoped, gated by the webhook)",
			user: workshopSAUser,
			req:  authzv1.ResourceAttributes{Verb: "create", Group: apGroup, Resource: "spiceboxtoolspecs"},
			want: true,
		},
		{
			name: "workshop SA: DENIED delete spiceboxtoolspecs cluster-wide — unscopable destructive verb",
			user: workshopSAUser,
			req:  authzv1.ResourceAttributes{Verb: "delete", Group: apGroup, Resource: "spiceboxtoolspecs"},
			want: false,
		},
		{
			name: "workshop SA: DENIED get spiceboxtoolspecs cluster-wide — cross-tenant read by name",
			user: workshopSAUser,
			req:  authzv1.ResourceAttributes{Verb: "get", Group: apGroup, Resource: "spiceboxtoolspecs"},
			want: false,
		},
		{
			name: "workshop SA: ALLOWED create spiceboxtoolkits (cluster-scoped, gated by the webhook)",
			user: workshopSAUser,
			req:  authzv1.ResourceAttributes{Verb: "create", Group: apGroup, Resource: "spiceboxtoolkits"},
			want: true,
		},
		{
			name: "workshop SA: DENIED delete spiceboxtoolkits cluster-wide — unscopable destructive verb",
			user: workshopSAUser,
			req:  authzv1.ResourceAttributes{Verb: "delete", Group: apGroup, Resource: "spiceboxtoolkits"},
			want: false,
		},
		{
			name: "workshop SA: DENIED get spiceboxtoolkits cluster-wide — cross-tenant read by name",
			user: workshopSAUser,
			req:  authzv1.ResourceAttributes{Verb: "get", Group: apGroup, Resource: "spiceboxtoolkits"},
			want: false,
		},

		// The three cluster-scoped kinds `inventory` reads (empty Namespace):
		// get;list only, granted by the reader ClusterRoleBinding this
		// reconciler creates alongside the toolwriter one.
		{
			name: "workshop SA: ALLOWED list clusteragentsettings — inventory's ceilings read",
			user: workshopSAUser,
			req:  authzv1.ResourceAttributes{Verb: "list", Group: apGroup, Resource: "clusteragentsettings"},
			want: true,
		},
		{
			name: "workshop SA: ALLOWED get clusteragentsettings — inventory's ceilings read",
			user: workshopSAUser,
			req:  authzv1.ResourceAttributes{Verb: "get", Group: apGroup, Resource: "clusteragentsettings"},
			want: true,
		},
		{
			name: "workshop SA: DENIED update clusteragentsettings — read-only grant",
			user: workshopSAUser,
			req:  authzv1.ResourceAttributes{Verb: "update", Group: apGroup, Resource: "clusteragentsettings"},
			want: false,
		},
		{
			name: "workshop SA: ALLOWED list clusterskills — inventory's cluster-skill catalog read",
			user: workshopSAUser,
			req:  authzv1.ResourceAttributes{Verb: "list", Group: apGroup, Resource: "clusterskills"},
			want: true,
		},
		{
			name: "workshop SA: ALLOWED list spiceboxclasses — inventory's sandbox-class catalog read",
			user: workshopSAUser,
			req:  authzv1.ResourceAttributes{Verb: "list", Group: apGroup, Resource: "spiceboxclasses"},
			want: true,
		},
		{
			name: "workshop SA: DENIED delete spiceboxclasses — read-only grant",
			user: workshopSAUser,
			req:  authzv1.ResourceAttributes{Verb: "delete", Group: apGroup, Resource: "spiceboxclasses"},
			want: false,
		},

		// The own-Workshop grant (task-8, plan-5a): crRole/crBinding
		// (rbac.go:277) live in the SESSION's namespace (ws.Namespace, same as
		// sess.Namespace here) and pin get/update/patch by resourceName to
		// exactly this Workshop — the channel a later request_* tool would
		// self-patch through. update is a verb the channelsd
		// WorkshopCredentialWatcher does NOT need (it holds only
		// workshops/status patch, task-8 Step 1) — this proves the sidecar's
		// OWN grant on the main resource, distinct from that watcher's.
		{
			name: "workshop SA: ALLOWED update its own Workshop by resourceName in the session namespace",
			user: workshopSAUser,
			req:  authzv1.ResourceAttributes{Namespace: sess.Namespace, Verb: "update", Group: apGroup, Resource: "workshops", Name: ws.Name},
			want: true,
		},
		{
			name: "workshop SA: DENIED update its own Workshop's name in a DIFFERENT namespace — the RoleBinding does not leak",
			user: workshopSAUser,
			req:  authzv1.ResourceAttributes{Namespace: "some-other-namespace", Verb: "update", Group: apGroup, Resource: "workshops", Name: ws.Name},
			want: false,
		},
		{
			name: "runner SA: DENIED update the Workshop in the session namespace — the own-CR grant is the workshop SA's, not universal (layer 1.2)",
			user: runnerSAUser,
			req:  authzv1.ResourceAttributes{Namespace: sess.Namespace, Verb: "update", Group: apGroup, Resource: "workshops", Name: ws.Name},
			want: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sar := &authzv1.SubjectAccessReview{
				Spec: authzv1.SubjectAccessReviewSpec{
					User:               tc.user,
					ResourceAttributes: &tc.req,
				},
			}
			out, err := cs.AuthorizationV1().SubjectAccessReviews().Create(ctx, sar, metav1.CreateOptions{})
			require.NoError(t, err, "SubjectAccessReview create")
			assert.Equal(t, tc.want, out.Status.Allowed, "user=%s verb=%s resource=%s namespace=%s", tc.user, tc.req.Verb, tc.req.Resource, tc.req.Namespace)
		})
	}
}
