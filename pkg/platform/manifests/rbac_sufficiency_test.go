//go:build integration

package manifests_test

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	authzv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/agentsession"
	"github.com/authzed/openagentprimitives/pkg/controllers/testenv"
	"github.com/authzed/openagentprimitives/pkg/platform/manifests"
)

// rbac_sufficiency_test.go is the RBAC test-then-minimize harness. It applies
// the RBAC objects that ACTUALLY ship (the embedded install.yaml bundle + the
// embedded channelsd manifests), then asserts — via SubjectAccessReview against
// a real (envtest) apiserver that enforces RBAC — that each component's
// ServiceAccount is granted every (verb, resource) its code needs.
//
// The required-access tables below are the hand-maintained source of truth for
// "what the code needs", seeded from a usage audit of each component's client
// calls + informers. A new Kubernetes client call MUST add a row here, or this
// test goes red — which is the whole point: it moves the "missing RBAC" failure
// from a deploy-time "failed to wait for caches to sync" crash to a CI failure,
// and it is the green guard that makes minimizing the roles safe.
//
// SubjectAccessReview (not a real mutating op) is used so create/delete/exec
// verbs are checked with no side effects. Each component carries a negative
// control (a verb it must NOT have) so a green run proves the apiserver is
// actually enforcing RBAC — the assertions are never vacuous.
//
// KNOWN BLIND SPOT: because the tables are hand-maintained, this harness fails
// on a WRONG row (a required tuple the role does not grant) but stays green on
// a MISSING one — a client call whose row nobody wrote is invisible here, which
// is how a component can ship with no rules at all for an API group. When
// adding a Kubernetes client call or an informer, the row is part of the change.

const apGroup = "agentprimitives.authzed.com"

// req is one required (or, for negatives, forbidden) access tuple.
type req struct {
	group, resource, verb        string
	namespace, subresource, name string
}

func (r req) String() string {
	s := r.verb + " " + r.group + "/" + r.resource
	if r.subresource != "" {
		s += "/" + r.subresource
	}
	if r.namespace != "" {
		s += " in ns=" + r.namespace
	}
	if r.name != "" {
		s += " name=" + r.name
	}
	return s
}

// nsName identifies a namespaced Role.
type nsName struct{ ns, name string }

// component is one ServiceAccount and the access it must (and must not) have.
type component struct {
	name     string // human label
	saUser   string // "system:serviceaccount:<ns>:<sa>"
	required []req
	// forbidden is the non-vacuity control: at least one tuple the role must
	// NOT grant. A green required-set + a denied forbidden tuple proves the
	// apiserver enforces RBAC.
	forbidden []req

	// The role(s) this component's SA is bound to. Used by the MINIMALITY
	// check to read the actually-granted rules and assert nothing is granted
	// beyond required ∪ allow.
	clusterRoles []string
	nsRoles      []nsName
	// allow lists grants that are legitimately broader than the code's direct
	// use: framework-required (leader-election leases/events) and documented
	// forward-looking grants. Everything granted must be in required ∪ allow,
	// or minimality fails (flagging an over-grant to trim or justify).
	allow []req
	// skipMinimality opts a component out of the granted⊆required∪allow check
	// (e.g. a third-party image whose role we don't own/minimize).
	skipMinimality bool
}

// key normalizes a tuple to "group|resource[/subresource]|verb" for set
// comparison. Namespace and resourceName are intentionally ignored: a
// name-pinned grant is justified by the same (group,resource,verb) need.
func (r req) key() string {
	res := r.resource
	if r.subresource != "" {
		res += "/" + r.subresource
	}
	return r.group + "|" + res + "|" + r.verb
}

func TestRBACSufficiency(t *testing.T) {
	env := testenv.Start(t)
	ctx := context.Background()

	cs, err := kubernetes.NewForConfig(env.Cfg)
	require.NoError(t, err, "build clientset for SubjectAccessReview")

	// Namespaces the bound Roles live in (RoleBindings need their ns to exist).
	for _, ns := range []string{"agentprimitives-system", "agentprimitives-identities", "ap-workspace-storage"} {
		_ = env.Client.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
	}

	// Apply the RBAC that actually ships: the install.yaml bundle + the
	// separately-embedded channelsd manifests.
	applyManifestRBAC(t, ctx, env.Client, manifests.Install)
	chBlobs, err := manifests.ChannelsD()
	require.NoError(t, err, "load embedded channelsd manifests")
	for _, b := range chBlobs {
		applyManifestRBAC(t, ctx, env.Client, b)
	}

	// The runner's per-session Role is built at runtime per session (covered by
	// agentsession's impersonation e2e test); the STATIC cluster-scoped reads it
	// relies on come from the spicebox-toolspec-reader ClusterRole, whose
	// binding is also per-session. Bind a test SA to it so we can exercise its
	// rules here.
	const runnerTestSA = "system:serviceaccount:default:rbac-test-runner"
	require.NoError(t, env.Client.Create(ctx, &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "rbac-test-toolspec-reader"},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "spicebox-toolspec-reader"},
		Subjects:   []rbacv1.Subject{{Kind: "ServiceAccount", Namespace: "default", Name: "rbac-test-runner"}},
	}), "bind test SA to spicebox-toolspec-reader")

	// The workshop sidecar's per-workshop cluster-scoped reads (inventory:
	// ClusterAgentSettings/ClusterSkill/SpiceboxClass) come from the
	// spicebox-workshop-reader ClusterRole, whose ClusterRoleBinding is minted
	// per workshop by pkg/controllers/workshop (BuildWorkshopRBAC's readerCRB).
	// That per-workshop binding isn't shipped in any static manifest, so — same
	// technique as the runner's toolspec-reader block above — bind a synthetic
	// test SA directly to the shipped ClusterRole rather than running the full
	// Workshop reconciler (which pkg/controllers/workshop's OWN envtest,
	// rbac_sufficiency_integration_test.go, already does against a REAL
	// per-workshop binding).
	const workshopReaderTestSA = "system:serviceaccount:default:rbac-test-workshop-reader"
	require.NoError(t, env.Client.Create(ctx, &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "rbac-test-workshop-reader"},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "spicebox-workshop-reader"},
		Subjects:   []rbacv1.Subject{{Kind: "ServiceAccount", Namespace: "default", Name: "rbac-test-workshop-reader"}},
	}), "bind test SA to spicebox-workshop-reader")

	// The runner's per-session NAMESPACED Role is not shipped in any manifest —
	// agentsession.BuildRunnerRBAC constructs it at runtime, per session — so
	// none of the applies above cover it, and a missing rule there is invisible
	// to every other suite: unit tests hand the tools a fake client, and
	// integration/e2e hand the in-process runner the envtest ADMIN client. That
	// is how request_credential_update shipped able to 403 on its very first
	// List. Build the MAXIMAL variant (every conditional rule emitted), apply
	// it, and put it under the same sufficiency + minimality checks as the
	// shipped roles.
	//
	// MAXIMAL is the limit of what this harness can see: a rule that is emitted
	// here but disappears in a NARROWER session shape still passes. Which rules
	// each shape emits is asserted by TestBuildRunnerRBACRuleSetPerSessionShape
	// (pkg/controllers/agentsession/rbac_shapes_test.go); this one asserts that
	// the apiserver grants what those rules say. Both are needed — a conditional
	// rule added to BuildRunnerRBAC belongs in that table AND in the
	// required-access set below.
	runnerSessionSA := applyRunnerSessionRBAC(t, ctx, env.Client)

	components := []component{
		operatorComponent(),
		webdComponent(),
		runnerToolspecReaderComponent(runnerTestSA),
		runnerSessionRoleComponent(runnerSessionSA),
		channelsdComponent(),
		workspaceProvisionerComponent(),
		workshopReaderComponent(workshopReaderTestSA),
	}

	for _, comp := range components {
		t.Run(comp.name, func(t *testing.T) {
			for _, r := range comp.required {
				ok := allowed(t, ctx, cs, comp.saUser, r)
				assert.Truef(t, ok, "REQUIRED but DENIED: %s lacks %q — under-broad RBAC (would 403 at runtime)", comp.name, r.String())
			}
			require.NotEmpty(t, comp.forbidden, "every component needs a non-vacuity control")
			for _, r := range comp.forbidden {
				ok := allowed(t, ctx, cs, comp.saUser, r)
				assert.Falsef(t, ok, "FORBIDDEN but ALLOWED: %s unexpectedly has %q (control proves enforcement)", comp.name, r.String())
			}

			// MINIMALITY: every granted (group,resource,verb) must be in
			// required ∪ allow. An extra means the role grants something the
			// code doesn't use and isn't justified — trim it, or add it to
			// `allow` with a reason. Together with sufficiency this pins the
			// granted set to exactly the narrow set.
			if comp.skipMinimality {
				return
			}
			ok := map[string]bool{}
			for _, r := range comp.required {
				ok[r.key()] = true
			}
			for _, r := range comp.allow {
				ok[r.key()] = true
			}
			for _, k := range grantedKeys(t, ctx, env.Client, comp) {
				assert.Truef(t, ok[k], "OVER-BROAD: %s grants %q which is neither required nor allow-listed — trim it or justify in allow", comp.name, k)
			}
		})
	}
}

// grantedKeys reads the component's bound roles and returns every granted
// (group,resource[/subresource],verb) as a normalized key.
func grantedKeys(t *testing.T, ctx context.Context, cl client.Client, comp component) []string {
	t.Helper()
	var rules []rbacv1.PolicyRule
	for _, name := range comp.clusterRoles {
		var cr rbacv1.ClusterRole
		require.NoError(t, cl.Get(ctx, client.ObjectKey{Name: name}, &cr), "get ClusterRole %s", name)
		rules = append(rules, cr.Rules...)
	}
	for _, nr := range comp.nsRoles {
		var role rbacv1.Role
		require.NoError(t, cl.Get(ctx, client.ObjectKey{Namespace: nr.ns, Name: nr.name}, &role), "get Role %s/%s", nr.ns, nr.name)
		rules = append(rules, role.Rules...)
	}
	var keys []string
	for _, ru := range rules {
		for _, g := range ru.APIGroups {
			for _, res := range ru.Resources {
				// /status and /finalizers are controller-gen conventions on
				// owned resources (you can only write the status/finalizers of
				// CRs you reconcile) — not a privilege-escalation surface, and
				// controller-gen emits get;update;patch as a set. Minimality
				// targets the security-relevant grants (main resources + core /
				// rbac / networking), not status-verb hygiene.
				if strings.HasSuffix(res, "/status") || strings.HasSuffix(res, "/finalizers") {
					continue
				}
				for _, v := range ru.Verbs {
					keys = append(keys, g+"|"+res+"|"+v)
				}
			}
		}
	}
	return keys
}

// allowed runs a SubjectAccessReview for saUser against the tuple and returns
// the apiserver's authorization decision.
func allowed(t *testing.T, ctx context.Context, cs *kubernetes.Clientset, saUser string, r req) bool {
	t.Helper()
	sar := &authzv1.SubjectAccessReview{
		Spec: authzv1.SubjectAccessReviewSpec{
			User: saUser,
			ResourceAttributes: &authzv1.ResourceAttributes{
				Namespace:   r.namespace,
				Verb:        r.verb,
				Group:       r.group,
				Resource:    r.resource,
				Subresource: r.subresource,
				Name:        r.name,
			},
		},
	}
	out, err := cs.AuthorizationV1().SubjectAccessReviews().Create(ctx, sar, metav1.CreateOptions{})
	require.NoError(t, err, "SubjectAccessReview create")
	return out.Status.Allowed
}

// applyManifestRBAC decodes a (possibly multi-doc) manifest blob and creates
// every RBAC / ServiceAccount / Namespace object in it. Non-RBAC docs (CRDs,
// CRs, Deployments) fail to decode against the client-go scheme and are
// skipped — we only need the authorization graph here.
func applyManifestRBAC(t *testing.T, ctx context.Context, cl client.Client, blob []byte) {
	t.Helper()
	dec := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(blob), 4096)
	for {
		var raw runtime.RawExtension
		if err := dec.Decode(&raw); err != nil {
			if err == io.EOF {
				return
			}
			continue
		}
		if len(raw.Raw) == 0 {
			continue
		}
		obj, _, derr := clientgoscheme.Codecs.UniversalDeserializer().Decode(raw.Raw, nil, nil)
		if derr != nil {
			continue // not a core/rbac kind (CRD, CR, etc.) — skip
		}
		switch obj.(type) {
		case *rbacv1.ClusterRole, *rbacv1.ClusterRoleBinding, *rbacv1.Role, *rbacv1.RoleBinding, *corev1.ServiceAccount, *corev1.Namespace:
			o := obj.(client.Object)
			if err := cl.Create(ctx, o); err != nil && !apierrors.IsAlreadyExists(err) {
				t.Fatalf("apply %T %s: %v", o, o.GetName(), err)
			}
		}
	}
}

// ---- required-access tables (hand-maintained source of truth) ----

// reqs expands a {resource(/subresource): verbs} spec into req tuples for one
// API group. Compact way to declare a component's full used-verb set.
func reqs(group string, spec map[string][]string) []req {
	var out []req
	for res, verbs := range spec {
		r := req{group: group}
		if i := strings.IndexByte(res, '/'); i >= 0 {
			r.resource, r.subresource = res[:i], res[i+1:]
		} else {
			r.resource = res
		}
		for _, v := range verbs {
			rr := r
			rr.verb = v
			out = append(out, rr)
		}
	}
	return out
}

// operatorComponent declares the operator's COMPLETE used-verb set (every
// controller's actual client calls + informers), so minimality proves the
// generated ClusterRole grants exactly this ∪ allow and nothing more.
func operatorComponent() component {
	const sa = "system:serviceaccount:agentprimitives-system:spicebox-operator"
	g := func(res, verb string) req { return req{group: apGroup, resource: res, verb: verb} }
	core := func(res, verb string) req { return req{resource: res, verb: verb} }
	lease := func(verb string) req { return req{group: "coordination.k8s.io", resource: "leases", verb: verb} }

	required := reqs(apGroup, map[string][]string{
		"agentsessions": {"get", "list", "watch", "create", "update", "patch", "delete"},
		// agentclasses/agentidentities/channels/mcpservers/sidecartoolboxes/
		// spicebox{classes,toolkits,toolspecs} carry create+patch beyond the
		// reconcilers' own get;list;watch(;update;patch) needs — admind's
		// authorized POST /agents/oap-install (pkg/web/admind/oapinstall.go, hosted in
		// this same operator binary) server-side-applies a .oap bundle's CRs
		// across exactly these kinds. See the RBAC markers in
		// pkg/controllers/agentclass/controller.go.
		"agentclasses":         {"get", "list", "watch", "create", "patch"},
		"agentidentities":      {"get", "list", "watch", "create", "patch"},
		"agentsettings":        {"get", "list", "watch"},
		"clusteragentsettings": {"get", "list", "watch"},
		// channels additionally carries DELETE, which the other admind-applied
		// kinds above do not: the SubagentRequest reconciler creates the
		// `agent` Channel a conversational child talks over, and its rollback
		// path deletes that Channel when a later step of the same delegation
		// fails (pkg/controllers/subagentrequest/controller.go, rollback).
		// Without the verb every partially-applied delegation would strand one.
		"channels":         {"get", "list", "watch", "update", "patch", "create", "delete"},
		"mcpservers":       {"get", "list", "watch", "update", "patch", "create"},
		"sidecartoolboxes": {"get", "list", "watch", "update", "patch", "create"},
		// The WorkspaceSource reconciler materializes/refreshes a shared base
		// checkout: get;list;watch to reconcile, update;patch for spec/status.
		// No create/delete — WorkspaceSources are author-owned (users, a future
		// .oap bundle), and the base PVC + materialize/refresh Jobs the reconciler
		// owns are torn down by owner-ref GC, not an explicit client Delete.
		"workspacesources": {"get", "list", "watch", "update", "patch"},
		// The agentui reconciler's own RBAC markers
		// (pkg/controllers/agentui/controller.go) request only get;list;watch:
		// For(&AgentUI{}) needs list+watch, Client.Get needs get. Read-only on
		// the main resource — AgentUI is namespace-scoped and author-owned, and
		// this controller writes only its status (agentuis/status, filtered
		// from minimality below like every other status subresource).
		"agentuis":          {"get", "list", "watch"},
		"spiceboxclasses":   {"get", "list", "watch", "create", "patch"}, // minimized: update dropped
		"spiceboxsessions":  {"get", "list", "watch", "create", "update", "patch", "delete"},
		"spiceboxtoolspecs": {"get", "list", "watch", "create", "patch"},
		"spiceboxtoolkits":  {"get", "list", "watch", "create", "patch"},
		// The spiceboxtoolchain reconciler (get + cache list/watch) and the
		// spiceboxsession reconciler's resolveToolchains (Get on every bind)
		// both read the toolchain catalog. Read-only: nothing creates or
		// mutates a SpiceboxToolchain outside the CRD author.
		"spiceboxtoolchains": {"get", "list", "watch"},
		"spicedbbootstraps":  {"get", "list", "watch", "update", "patch"},
		// The PublicEndpoint reconciler owns the CR's status and finalizer; the
		// channel reconciler watches it, because the public address moving is
		// what makes a registered webhook URL stale.
		"publicendpoints":     {"get", "list", "watch", "update", "patch"},
		"toolcalls":           {"get", "list", "watch", "create", "update", "patch", "delete"},
		"artifactrenders":     {"get", "list", "watch", "create", "update", "patch"},
		"agentsessiongrants":  {"get", "list", "watch", "create", "update"},          // minimized: patch;delete dropped
		"skills":              {"get", "list", "watch", "create", "update", "patch"}, // delete → allow (documented)
		"clusterskills":       {"get", "list", "watch", "create", "update", "patch"}, // delete → allow (documented)
		"skillsources":        {"get", "list", "watch"},
		"clusterskillsources": {"get", "list", "watch"},
		// update;patch on the MAIN resource (beyond the usual status-only
		// writes) is the UserIdentity finalizer's: the reconciler adds it on
		// first reconcile and removes it once the deletion pass has withdrawn
		// every sole_user edge the catalog's subject still holds (see
		// pkg/controllers/useridentity's withdrawUnclaimedSoleIdentities).
		// Writing a finalizer is a metadata write on the object itself, so the
		// useridentities/finalizers grant alone does not cover it.
		"useridentities":           {"get", "list", "watch", "update", "patch"},
		"sessionuseridentities":    {"get", "list", "watch", "create", "patch"}, // operator uses Status().Patch, not main Update
		"clusteridentityproviders": {"get", "list", "watch"},
		// The RelationshipSource reconciler Gets/Lists/Watches the main resource
		// to reconcile (markers in pkg/controllers/relationshipsource/doc.go); no
		// create/update/patch/delete — RelationshipSources are author-owned, like
		// WorkspaceSources above. The extra List (beyond the informer's own) is
		// findIncumbent's cluster-wide scan enforcing one-source-per-kind — every
		// namespace, not just the reconciled CR's own, so `list` has to be
		// cluster-scoped rather than narrowed to a namespace.
		"relationshipsources": {"get", "list", "watch"},
		// The CredentialUpdateRequest reconciler only Gets/Lists/Watches the main
		// object (List for the ask-budget check) and writes via Status().Patch;
		// it never Creates/Updates/Patches/Deletes the spec. `create` is here for
		// a DIFFERENT reason: the operator writes the per-session runner Role
		// that grants the request_credential_update meta-tool `create`, and K8s
		// privilege-escalation prevention forbids granting a verb the granter
		// lacks. Same delegation the operator already holds for toolcalls and
		// artifactrenders (markers in pkg/controllers/agentsession/controller.go).
		"credentialupdaterequests": {"get", "list", "watch", "create"},
		// SessionHold: two reconcilers in this same operator binary touch it.
		// The AgentSession reconciler only Gets/Lists/Watches (activeHoldFor's
		// List, and the SessionHold watch that re-enqueues the held session —
		// markers in pkg/controllers/agentsession/controller.go). `create` is
		// the plan-gate denial-streak tripper's own (pkg/authz/plangate/hold,
		// marker in pkg/controllers/agentsession/hold.go) — it runs in-process
		// in the operator, not a separate binary, so its grant lands here.
		// The SessionHold controller itself (pkg/controllers/sessionhold) adds
		// `create`, for cascadeHold fanning a hold out across a delegation
		// subtree (one SessionHold per descendant) — not declared on this
		// controller's own marker before the cascade (get;list;watch only).
		// It never deletes a SessionHold: releaseCascade only ever writes
		// Phase/Determination/ReleasedBy on a cascaded hold, which then
		// persists after release exactly like every other released hold in
		// this codebase (a manual hold, a tripper's), GC'd by its own
		// owner-ref when its session is eventually deleted — see
		// cascade.go's releaseCascade doc for why an earlier iteration's
		// confirm-then-delete cleanup step was removed rather than kept.
		// Otherwise unchanged: get;update;patch on sessionholds/status
		// (excluded from this minimality table below, like every other status
		// subresource).
		"sessionholds": {"get", "list", "watch", "create"},
		// SubagentRequest controller (pkg/controllers/subagentrequest): Gets its
		// own CR by name and the manager's For(&SubagentRequest{}) needs
		// list;watch for the informer. No update/patch on the main resource —
		// the reconciler writes only status. It also lists AgentClass (roster
		// re-validation) and Gets/Creates/Deletes AgentSession (the delegated
		// child, plus the compensating delete on a failed lineage-tuple write);
		// both of those already have their own broader rows above. `create` is
		// here for the same DIFFERENT reason as credentialupdaterequests above:
		// the operator writes the per-session runner Role that grants the
		// delegate meta-tool `create` on subagentrequests, and K8s
		// privilege-escalation prevention forbids granting a verb the granter
		// lacks (marker in pkg/controllers/agentsession/controller.go).
		// `delete` is reclaimTerminal's: a resolved delegation is deleted once
		// it has been kept for DefaultTerminalRetention, which cascades the
		// child session and their `agent` Channel. A live parent's finished
		// delegations are reclaimed by nothing else.
		"subagentrequests": {"get", "list", "watch", "create", "delete"},
	})
	// Status subresources the operator actually writes. grantedKeys skips every
	// */status rule when checking MINIMALITY (controller-gen emits them as a set
	// on every owned resource, and they are not a privilege-escalation surface),
	// so a status grant only ever appears here -- on the SUFFICIENCY side, where
	// a SubjectAccessReview proves the shipped role really allows the write.
	//
	// Without this row, deleting the credentialupdaterequests/status marker left
	// this harness green while every determination 403'd and every request stuck
	// in Pending: the reconciler writes nothing else, so nothing else would fail.
	// Same shape as channelsd's own credentialupdaterequests/status row.
	required = append(required, req{
		group: apGroup, resource: "credentialupdaterequests", subresource: "status", verb: "patch"})
	// SubagentRequest reconciler writes status via Status().Update (a real
	// PUT/"update", not a merge patch), so the verb under test here is
	// "update", not "patch" like its sibling above.
	required = append(required, req{
		group: apGroup, resource: "subagentrequests", subresource: "status", verb: "update"})
	// RelationshipSource reconciler also writes status via Status().Update
	// (pkg/controllers/relationshipsource/controller.go), same shape as
	// subagentrequests above: "update", not "patch".
	required = append(required, req{
		group: apGroup, resource: "relationshipsources", subresource: "status", verb: "update"})
	required = append(required, reqs("", map[string][]string{
		"secrets": {"get", "list", "watch", "create", "update", "patch"},
		// patch: admind's oap-install endpoint SSA-applies a bundle-created
		// ConfigMap same as any other bundled kind; delete stays allow-only.
		"configmaps":      {"get", "list", "watch", "create", "update", "patch"},
		"pods":            {"get", "list", "watch", "create", "delete"},
		"pods/exec":       {"create"},
		"serviceaccounts": {"get", "list", "watch", "create", "patch"},
		// delete: the session-storage retention sweep (agentsession's
		// reconcileStorageReclaim) deletes a terminal session's workspace +
		// snapshot-store claims once retention elapses past finishedAt.
		"persistentvolumeclaims": {"get", "list", "watch", "create", "delete"},
		// list: the agentsession boot path (firstPVCProvisioningFailure) lists a
		// namespace's Events via the uncached APIReader to surface the workspace
		// PVC's actual ProvisioningFailed reason and fail fast. Uncached, so no
		// get/watch — only list. (create/patch stay allow-listed for the
		// leader-election EventRecorder.)
		"events": {"list"},
		// watch: the workspacevolume janitor's PV informer maps node liveness;
		// get/list: provablyUnschedulableBundle compares a stuck bundle pod's
		// request to max allocatable, and the janitor Gets the affined node.
		"nodes": {"get", "list", "watch"},
		// The workspacevolume janitor watches Released hostPath PVs of the
		// workspace StorageClass and deletes the ones stranded on removed
		// nodes (their provisioner-side reclaim can never run).
		"persistentvolumes": {"get", "list", "watch", "delete"},
		// Workshop controller (pkg/controllers/workshop, spec §1.2/1.3): get is
		// the namespace-collision check (reads via APIReader, direct — not the
		// cache, so no list/watch informer to back); create provisions the
		// workshop namespace; delete is the teardown reconciler's explicit
		// Namespace delete once every layer inside it has been reversed.
		"namespaces": {"get", "create", "delete"},
		// SSA-applying the deny-all quota onto a namespace that has never held
		// one requires BOTH create and patch — server-side apply's create-via-
		// PATCH is authorized on the `patch` verb only when the object already
		// exists; a brand-new object also needs `create` (confirmed against a
		// real RBAC-enforcing apiserver, not assumed).
		"resourcequotas": {"create", "patch"},
		// The LimitRange applied beside that quota, by the same SSA call shape
		// and so for the same create+patch reason. It is not optional garnish:
		// the quota makes requests+limits mandatory on every container in the
		// workshop namespace, and this is what defaults them, so without the
		// grant every pod a person's own test run creates is refused.
		"limitranges": {"create", "patch"},
	})...)
	// Workshop CR (pkg/controllers/workshop): get/list/watch back the
	// manager's For(&Workshop{}) informer and the per-reconcile Get; update is
	// EnsureFinalizer's plain (non-status) Update that adds
	// FinalizerWorkshop. Status is a separate subresource, excluded from
	// minimality like every other owned resource's status.
	required = append(required, reqs(apGroup, map[string][]string{
		"workshops": {"get", "list", "watch", "update"},
		// WorkshopProbe CR (pkg/controllers/workshopprobe): get/list/watch back the
		// manager's For(&WorkshopProbe{}) informer and the per-reconcile Get. No
		// plain update — the reconciler writes status ONLY (Status().Update) and
		// carries no finalizer (the probe pod/NetworkPolicy/ConfigMap are owner-ref
		// GC'd), so unlike Workshop above it needs no non-status update. Status is a
		// separate subresource, excluded from minimality like every owned status.
		"workshopprobes": {"get", "list", "watch"},
	})...)
	required = append(required, reqs(rbacv1.GroupName, map[string][]string{
		"roles":               {"get", "list", "watch", "create", "patch", "delete"},
		"rolebindings":        {"get", "list", "watch", "create", "patch", "delete"},
		"clusterrolebindings": {"create", "delete"}, // minimized: get;list;watch dropped
	})...)
	// get;list;watch: the agentsession reconciler reads the workspace StorageClass
	// through the CACHED client — clampSizeToClassFloor rounds a PVC request up to
	// the class's minimum, effectiveBundleReadyDeadline stretches the bundle
	// deadline for a slow-cold-start (Filestore) class. A cached read backs its
	// Get with an informer, which needs list+watch; SCs rarely change and the
	// deadline check runs every ~2s while a bundle provisions, so caching beats an
	// uncached per-reconcile apiserver hit.
	required = append(required, reqs("storage.k8s.io", map[string][]string{
		"storageclasses": {"get", "list", "watch"},
	})...)
	required = append(required, reqs("networking.k8s.io", map[string][]string{
		"networkpolicies": {"get", "list", "watch", "create", "update", "patch", "delete"}, // update: sidecartoolbox probe-netpol convergence (ensureProbeNetpol)
	})...)
	required = append(required, reqs("batch", map[string][]string{
		"jobs": {"get", "list", "watch", "create", "update", "patch", "delete"},
	})...)
	// admind's cluster-health snapshot reads component Deployments/StatefulSets
	// (replica/ready counts) for the admin Overview — read-only, no watch.
	required = append(required, reqs("apps", map[string][]string{
		"deployments":  {"get", "list"},
		"statefulsets": {"get", "list"},
	})...)
	// The same health snapshot sums live PodMetrics (metrics.k8s.io) for the
	// namespace CPU/Memory rollup — read-only, best-effort.
	required = append(required, reqs("metrics.k8s.io", map[string][]string{
		"pods": {"get", "list"},
	})...)
	// The agent-sandbox sandbox kind (pkg/tools/sandboxkinds/agentsandbox) reconciles a
	// peer CRD's Sandbox: Get (Ensure/Status/Executor), Create (Ensure), Delete
	// (Teardown), and list;watch for the Owns() informer the spiceboxsession
	// controller registers from Runtime.Watches(). No update/patch — the runtime
	// never mutates a Sandbox in place (several spec fields are immutable).
	//
	// This row exists because the operator registers that informer whenever the
	// CRD is merely PRESENT: the kind's availability check is RESTMapper
	// discovery, which needs no RBAC on the resource, so a cluster running
	// agent-sandbox but opted into no agent-sandbox class still starts the
	// informer. Missing rules there are not a degraded feature — list/watch
	// 403s, the cache never syncs, and the operator crash-loops.
	required = append(required, reqs("agents.x-k8s.io", map[string][]string{
		"sandboxes": {"get", "list", "watch", "create", "delete"},
	})...)
	// Status is read through the main-resource Get above, not the subresource;
	// this is the conventional owned-status grant, asserted here (sufficiency
	// only — grantedKeys skips */status for minimality) so dropping the marker
	// is caught rather than silently narrowing the role.
	required = append(required, req{
		group: "agents.x-k8s.io", resource: "sandboxes", subresource: "status", verb: "get"})
	// Pre-warming will have pkg/tools/sandboxkinds/agentsandbox reconcile THREE more
	// agent-sandbox CRDs, in a DIFFERENT API group — extensions.agents.x-k8s.io, not
	// agents.x-k8s.io above. This grant lands ahead of that reconcile code so the
	// role is never behind the informer it must back. Same discovery-only presence
	// check as Sandbox: the RESTMapper gate needs no RBAC, so a missing grant here
	// does not fail validation — it 403s the informer's list/watch and crash-loops
	// the operator, on a cluster that never opted into pre-warming. get;list;watch
	// is mandatory even though the code will only Get by name, because the
	// controller-runtime client reads through the cache and starts an informer on
	// the first Get.
	required = append(required, reqs("extensions.agents.x-k8s.io", map[string][]string{
		// A SandboxTemplate is IMMUTABLE by construction: templateNameFor hashes
		// the rendered PodSpec into the object name, so any change mints a NEW
		// template and ensureTemplate returns early on a hit without writing.
		// The only thing ReconcilePool ever rewrites is the WARM POOL — its
		// templateRef and replica count — so update belongs there and nowhere
		// else. Templates are created and (once unreferenced) deleted.
		"sandboxtemplates": {"get", "list", "watch", "create", "delete"},
		"sandboxwarmpools": {"get", "list", "watch", "create", "update", "delete"},
		// Claims are only ever created (checkout) and deleted (release); never
		// rewritten in place.
		"sandboxclaims": {"get", "list", "watch", "create", "delete"},
	})...)

	return component{
		name: "operator", saUser: sa,
		clusterRoles: []string{"spicebox-operator"},
		nsRoles:      []nsName{{ns: "agentprimitives-system", name: "spicebox-operator-debug"}},
		required:     required,
		allow: []req{
			// Leader election (controller-runtime manager) — looks unused by
			// reconcilers but the manager needs the full lease lifecycle + events.
			lease("get"), lease("list"), lease("watch"), lease("create"), lease("update"), lease("patch"), lease("delete"),
			core("events", "create"), core("events", "patch"),
			// Documented forward-looking grants (held without an RBAC change for a
			// future explicit-cleanup path; child pruning relies on owner-ref GC today).
			core("configmaps", "delete"),
			g("skills", "delete"), g("clusterskills", "delete"),
			// The Workshop controller's own SA (namespace, not the object) is
			// session-owned and cascade-deleted; nothing in this codebase yet
			// explicitly deletes one. Held for a future explicit-cleanup path,
			// the same bargain as configmaps/skills/clusterskills above.
			core("serviceaccounts", "delete"),
			// Not directly exercised by the Workshop reconciler (writes go
			// through create+patch only, confirmed against a real apiserver —
			// see the required-table comment); declared for parity with every
			// other resource this controller's namespace-collision Get could in
			// principle be asked to enumerate.
			core("resourcequotas", "get"), core("resourcequotas", "list"), core("resourcequotas", "watch"),
			core("limitranges", "get"), core("limitranges", "list"), core("limitranges", "watch"),
			core("namespaces", "list"), core("namespaces", "watch"),
			// Workshop CR (pkg/controllers/workshop): get/list/watch/update are
			// this reconciler's own; create/delete/patch are NOT exercised by
			// it — create is the AgentSession reconciler's future
			// ensureWorkshop hook (same operator binary, same ClusterRole,
			// spec §1.1), delete is defensive (owner-ref GC needs no RBAC of
			// its own to remove a dependent), and patch mirrors the
			// admind-oap-install SSA pattern already documented above for
			// agentclasses/agentidentities/etc.
			g("workshops", "create"), g("workshops", "delete"), g("workshops", "patch"),
			// WIDENING (deliberate, spec §1.2 — Kubernetes privilege-escalation
			// prevention): the Workshop controller grants the per-workshop SA
			// CRUD on this closed kind set inside the workshop namespace
			// (pkg/controllers/workshop/rbac.go, workshopKindResources). A
			// RoleBinding can only grant a verb its own granter already holds,
			// so the operator's ClusterRole must hold delete/update on every one
			// of these even though NO operator reconciler calls
			// delete/update on them directly — see the widening comment above
			// pkg/controllers/workshop/controller.go's RBAC markers, and the
			// SAR proof in pkg/controllers/workshop/rbac_sufficiency_integration_test.go
			// that the grant actually reaches only the workshop SA, scoped to
			// its own namespace, and never the runner SA.
			g("agentclasses", "delete"), g("agentclasses", "update"),
			g("agentidentities", "delete"), g("agentidentities", "update"),
			g("mcpservers", "delete"),
			g("sidecartoolboxes", "delete"),
			g("agentuis", "create"), g("agentuis", "delete"), g("agentuis", "patch"), g("agentuis", "update"),
			g("subagentrequests", "patch"), g("subagentrequests", "update"),
			g("spiceboxtoolspecs", "delete"), g("spiceboxtoolspecs", "update"),
			g("spiceboxtoolkits", "delete"), g("spiceboxtoolkits", "update"),
			// workshopprobes:create is a grant-not-call widening: the operator
			// never creates a WorkshopProbe (the ap-workshop sidecar does, in W),
			// but the Workshop controller grants the workshop SA
			// workshopprobes:create, and privilege-escalation prevention needs
			// the granter to hold it. See workshopprobe controller RBAC markers
			// and TestOperatorCanGrantTheWorkshopRole.
			g("workshopprobes", "create"),
			// secrets:delete is the SAME shape of widening as the block above,
			// for a DIFFERENT grantee: BuildWorkshopBrowserRBAC
			// (pkg/controllers/workshop/rbac.go) hands the browser (webd)
			// Service Account delete on Secrets in the workshop namespace — the
			// creds Secret a person's own test session writes — and no
			// reconciler in this operator binary ever deletes a Secret itself
			// (the bearer-token Secret this same package writes is set-once and
			// outlives the workshop). See the widening comment on the
			// secrets +kubebuilder:rbac marker in
			// pkg/controllers/workshop/controller.go.
			core("secrets", "delete"),
		},
		forbidden: []req{
			core("nodes", "delete"),
			// Minimized-away grants — MUST stay denied (regression guard).
			{group: rbacv1.GroupName, resource: "clusterrolebindings", verb: "get"},
			g("spiceboxclasses", "update"),
			g("agentsessiongrants", "delete"),
			core("configmaps", "deletecollection"),
			// WorkspaceSource least-privilege: the reconciler never deletes a
			// WorkspaceSource (author-owned). Guard the trim so a re-broadened
			// marker fails here. (persistentvolumeclaims delete moved to
			// required: the session-storage retention sweep now legitimately
			// deletes a terminal session's scratch claims — the enforcement
			// control it doubled as is carried by nodes delete above.)
			g("workspacesources", "delete"),
			// The agent-sandbox runtime creates and deletes a Sandbox but never
			// rewrites one; several spec fields are immutable anyway.
			{group: "agents.x-k8s.io", resource: "sandboxes", verb: "update"},
			// A SandboxClaim is only ever created and deleted, never rewritten in
			// place — unlike its sibling sandboxwarmpools, which DOES get update.
			// Granting update here would be an over-grant; assert it stays denied
			// so minimality is proven, not assumed.
			{group: "extensions.agents.x-k8s.io", resource: "sandboxclaims", verb: "update"},
			// Same, for the same reason, one object over: a SandboxTemplate is
			// immutable by construction (hash-named from the rendered PodSpec, so
			// a change mints a new one), and no code path updates one. The only
			// Client.Update in the pre-warming path is on a SandboxWarmPool.
			{group: "extensions.agents.x-k8s.io", resource: "sandboxtemplates", verb: "update"},
		},
	}
}

func webdComponent() component {
	const sa = "system:serviceaccount:agentprimitives-system:spicebox-webd"
	g := func(res, verb string) req { return req{group: apGroup, resource: res, verb: verb} }
	// The built-in local web chat (cluster-kind "local", gated behind
	// InstallProfile().ServesLocalWebChat()) does per-conversation CRUD in
	// the chat namespace ("default", pkg/web/webui/chat's unexported
	// newChatSessionNamespace) via the spicebox-webd-default Role — NOT the
	// ClusterRole. dn() is an apGroup tuple scoped there so the SAR exercises
	// that Role specifically.
	const chatNS = "default"
	dn := func(res, verb string) req { return req{group: apGroup, resource: res, verb: verb, namespace: chatNS} }
	bn := func(res, verb string) req {
		return req{group: apGroup, resource: res, verb: verb, namespace: "agentprimitives-system"}
	}
	return component{
		name: "webd", saUser: sa,
		clusterRoles: []string{"spicebox-webd"},
		nsRoles: []nsName{
			{ns: "agentprimitives-identities", name: "spicebox-webd"},
			{ns: "agentprimitives-system", name: "spicebox-webd-system"},
			// Local web chat's per-conversation CRUD (channels, agentsessions incl.
			// the wake patch/update, creds Secrets) lives in this Role. Listing it
			// here brings its grants under the minimality check too — previously the
			// whole Role was invisible to this harness, which is how the missing
			// wake verbs slipped past CI.
			{ns: chatNS, name: "spicebox-webd-default"},
			// The agent builder is installed into agentprimitives-system (a fixed
			// namespace) and a session must co-locate with its AgentClass, so web
			// chat starts it only if webd can create a Channel/AgentSession/Secret
			// THERE. Listed so its grants come under the minimality check too —
			// its (group,resource,verb) keys duplicate the "default" Role's, which
			// are already required, so it needs no new required/allow entry.
			{ns: "agentprimitives-system", name: "spicebox-webd-builder"},
		},
		required: []req{
			g("useridentities", "get"), g("useridentities", "list"), g("useridentities", "create"),
			g("useridentities", "update"), g("useridentities", "patch"),
			g("agentsessions", "get"),
			// identityd's PurposeWorkshopCredential clicker gate Gets the builder
			// session's Workshop CR by name (cluster-wide — a builder can live in any
			// namespace) to authorize the clicker == the workshop's starter. Without
			// this the gate 403s and no legitimate starter can open the card.
			g("workshops", "get"),
			// The session-start gate's dynamic arm. WorkshopNamespacesFor
			// (internal/cmd/webd/main.go) Lists Workshops cluster-wide — a
			// builder session can live in any namespace — to find the Ready
			// ones the viewer started, whose namespaces the new-session dialog
			// may then start the built class in. Without list the dialog can
			// never preselect a workshop class and "Try it" refuses.
			g("workshops", "list"),
			// The transcript data plane reads the Channel bound to a session in
			// WHATEVER namespace the viewer holds interact in — Registry.rehydrate
			// Gets it to rebuild an in-process sink. The tuple is deliberately
			// scoped to a namespace no webd Role covers, so only the ClusterRole
			// can satisfy it: asking in "default" would be satisfied by the
			// namespaced Role below and would prove nothing. Without this grant,
			// opening a conversation outside "default" fails with Forbidden and
			// surfaces as "session not found".
			{group: apGroup, resource: "channels", verb: "get", namespace: "some-tenant-namespace"},
			// Built-in local web chat (chat namespace, spicebox-webd-default Role):
			// per-conversation Channel + AgentSession + creds-Secret CRUD, and —
			// crucially — WAKING an idle session by PATCHing a wake annotation onto
			// its AgentSession. patch+update were missing from the Role, so a
			// follow-up message to an idle web-chat session 403'd with "cannot patch
			// agentsessions" and the agent never resumed. These namespace-scoped
			// tuples exercise that Role (the ClusterRole grants only agentsessions:get
			// cluster-wide), so dropping the wake verb fails HERE, not in production.
			dn("channels", "get"), dn("channels", "list"),
			dn("channels", "create"), dn("channels", "delete"),
			dn("agentsessions", "list"), dn("agentsessions", "create"),
			dn("agentsessions", "delete"), dn("agentsessions", "patch"),
			dn("agentsessions", "update"),
			{resource: "secrets", verb: "get", namespace: chatNS},
			{resource: "secrets", verb: "create", namespace: chatNS},
			{resource: "secrets", verb: "delete", namespace: chatNS},
			// Agent-builder reachability from web chat (spicebox-webd-builder Role,
			// agentprimitives-system): a session must co-locate with its AgentClass,
			// so starting the builder means webd creating the SAME per-conversation
			// set in the builder's namespace. These prove the widening actually
			// reaches — drop role-builder.yaml and the builder becomes unstartable
			// from the picker, failing HERE rather than as a 500 on a real click.
			bn("channels", "create"), bn("channels", "delete"),
			bn("agentsessions", "create"), bn("agentsessions", "delete"),
			bn("agentsessions", "patch"),
			{resource: "secrets", verb: "create", namespace: "agentprimitives-system"},
			{resource: "secrets", verb: "delete", namespace: "agentprimitives-system"},
			g("sessionuseridentities", "get"),
			{group: apGroup, resource: "sessionuseridentities", subresource: "status", verb: "update"},
			g("mcpservers", "get"), g("mcpservers", "list"),
			g("clusteridentityproviders", "get"),
			// list: webd reads AgentClasses cluster-wide (agent picker / icon
			// resolution).
			//
			// get: STILL REQUIRED. browsersession.Create Gets the named
			// AgentClass by name on every start — for its UID (the Channel's
			// owner reference) and its Valid condition — so both start routes
			// (pkg/web/webui/sessions, pkg/web/webui/agentui) depend on this verb, and
			// pkg/web/webui/sessions' own list join Gets each session's class for
			// its display title (resolveClassTitle). Removing it breaks session
			// creation outright. webd already reads every AgentClass's full
			// content via the cluster-wide list, so the get verb is no broader
			// read-exposure.
			g("agentclasses", "get"), g("agentclasses", "list"),
			// The agent-defined UI's declaration. walkAgentUIDoors Gets the
			// AgentUI named by AgentClass.spec.agentUI.ref, both for the
			// agent-UI page and for a session selected in the shell.
			//
			// Without this grant every agent-UI page load 500s with "This
			// agent's UI isn't available right now" — Forbidden falling into
			// walkAgentUIDoors' non-NotFound arm — while the AgentUI CR reads
			// Valid=True and nothing points at RBAC. See the KNOWN BLIND SPOT
			// at the top of this file: a verb nobody writes down here is
			// invisible to this suite.
			g("agentuis", "get"),
			// The icon resolver both Lists and Gets-by-name SpiceboxToolkits.
			g("spiceboxtoolkits", "get"), g("spiceboxtoolkits", "list"),
			// Master-secret CRUD in the identities namespace.
			{resource: "secrets", verb: "get", namespace: "agentprimitives-identities"},
			{resource: "secrets", verb: "create", namespace: "agentprimitives-identities"},
			{resource: "secrets", verb: "update", namespace: "agentprimitives-identities"},
			{resource: "secrets", verb: "delete", namespace: "agentprimitives-identities"},
			// Pinned reads in the system namespace.
			{resource: "secrets", verb: "get", namespace: "agentprimitives-system", name: "idp-google"},
			// identityd's password-kind ClusterIdentityProvider client-secret
			// read (hosted in-process in webd) — see idp_loader.go step 3 and
			// the macOS desktop bundle's provisionPasswordIdP, both of which
			// write/read the "idp-password" Secret per the passwordkind
			// Wizard's convention.
			{resource: "secrets", verb: "get", namespace: "agentprimitives-system", name: "idp-password"},
			{resource: "configmaps", verb: "get", namespace: "agentprimitives-system", name: "spicebox-webd-external-url"},
			// ServeTransform (internal/cmd/webd/main.go) Gets the ArtifactRender by name to
			// resolve its kind before serving the live-view content frame; without
			// this grant the artifact is served untransformed.
			g("artifactrenders", "get"),
			// Replacing an agent's OWN shared credential
			// (pkg/platform/identityd/credupdate_agentowned.go). Both are READS: the
			// CredentialUpdateRequest list tells identityd whose credential the
			// link covers, and the AgentIdentity get tells it whether the value
			// is pasteable at all. The WRITE is not here — it is submitted to the
			// operator, which authorizes it independently.
			g("credentialupdaterequests", "list"),
			g("agentidentities", "get"),
			// The session-start route's workshop-cap check
			// (pkg/web/webui/sessions/start_workshopcap.go) Gets the singleton
			// ClusterAgentSettings — the cluster tier alone says whether a class
			// is sanctioned for a builder workshop and what the per-starter
			// ceiling is. Cluster-scoped, so only the ClusterRole can grant it.
			// Without it every start of a sanctioned class answers 503 rather
			// than counting the viewer's open workshops.
			g("clusteragentsettings", "get"),
		},
		allow: []req{
			// The webd ClusterRole grants artifactrenders get AND list as a read
			// bundle (config/webd/clusterrole.yaml). ServeTransform only Gets by
			// name today, so list is a forward-looking read grant, not exercised.
			g("artifactrenders", "list"),
		},
		forbidden: []req{
			{resource: "pods", verb: "create", namespace: "agentprimitives-system"},
			// The cluster-wide Channel grant above is a READ. Per-conversation
			// creation and teardown stay in the namespaced Role, so these two
			// verbs must remain unreachable in a namespace no webd Role covers —
			// otherwise a browser-facing pod could mint or destroy a Channel
			// anywhere in the cluster.
			{group: apGroup, resource: "channels", verb: "create", namespace: "some-tenant-namespace"},
			{group: apGroup, resource: "channels", verb: "delete", namespace: "some-tenant-namespace"},
			// webd is browser-facing and must hold NO Secret access outside
			// agentprimitives-identities (its master-Secret Role) and the local
			// web chat's namespace. Replacing an agent's own shared credential
			// deliberately does NOT write here — it is submitted to the operator,
			// which authorizes it independently — so a cluster-wide Secret grant
			// appearing on this ServiceAccount is a regression, not a feature.
			// A tenant namespace no webd Role covers is the discriminator.
			{resource: "secrets", verb: "get", namespace: "some-tenant-namespace"},
			{resource: "secrets", verb: "update", namespace: "some-tenant-namespace"},
		},
	}
}

func channelsdComponent() component {
	const sa = "system:serviceaccount:agentprimitives-system:spicebox-channelsd"
	g := func(res, verb string) req { return req{group: apGroup, resource: res, verb: verb} }
	return component{
		name: "channelsd", saUser: sa,
		clusterRoles: []string{"spicebox-channelsd"},
		// channelsd is a NATS consumer with NO informers — it Gets/Lists via the
		// client and Patches status. These are the verbs its code actually uses.
		required: []req{
			g("channels", "get"), g("channels", "list"),
			{group: apGroup, resource: "channels", subresource: "status", verb: "patch"},
			g("agentclasses", "get"),
			g("agentsessions", "get"), g("agentsessions", "list"), g("agentsessions", "create"), g("agentsessions", "patch"),
			{group: apGroup, resource: "agentsessions", subresource: "status", verb: "patch"},
			// liveInteractiveToolCall lists ToolCalls (label-filtered) to route
			// inbound messages to a live interactive tool's stdin.
			g("toolcalls", "list"),
			g("sessionuseridentities", "get"),
			// The inbound initiative gate reads the SubagentRequest that
			// created a delegated child, to learn whether a person may open a
			// turn into it (pipeline.refuseUninvitedInitiative).
			g("subagentrequests", "get"),
			// CredentialUpdateWatcher polls every Open, undelivered
			// CredentialUpdateRequest cluster-wide and patches status.interactionRef
			// once it publishes.
			g("credentialupdaterequests", "list"),
			{group: apGroup, resource: "credentialupdaterequests", subresource: "status", verb: "patch"},
			// recordChannelIdentity ensure-creates a UserIdentity per human sender
			// and upserts the channel-identity directory onto its status.
			g("useridentities", "get"), g("useridentities", "list"),
			g("useridentities", "create"), g("useridentities", "patch"),
			{group: apGroup, resource: "useridentities", subresource: "status", verb: "patch"},
			g("mcpservers", "list"),
			{resource: "secrets", verb: "get", namespace: "default"},
			{resource: "configmaps", verb: "get", name: "spicebox-webd-external-url"},
			// WorkshopCredentialWatcher lists Workshops cluster-wide (a builder
			// session can live in any namespace) to find undelivered
			// spec.credentialRequests, and patches
			// status.credentialRequests[].deliveredAt once it delivers the card.
			// List only -- it never fetches one by name.
			g("workshops", "list"),
			{group: apGroup, resource: "workshops", subresource: "status", verb: "patch"},
		},
		forbidden: []req{{resource: "secrets", verb: "create", namespace: "default"}},
	}
}

func workspaceProvisionerComponent() component {
	const sa = "system:serviceaccount:ap-workspace-storage:ap-workspace-provisioner"
	core := func(res, verb string) req { return req{resource: res, verb: verb} }
	return component{
		// Third-party rancher/local-path-provisioner; its needs are the upstream
		// chart's. Coverage only — we do not minimize a third-party role.
		name: "workspace-provisioner", saUser: sa,
		skipMinimality: true,
		required: []req{
			core("persistentvolumes", "create"), core("persistentvolumes", "delete"),
			core("persistentvolumeclaims", "get"), core("persistentvolumeclaims", "list"),
			core("nodes", "get"), core("pods", "list"),
			core("pods", "create"), core("pods", "delete"),
			{resource: "pods", subresource: "log", verb: "get"},
			{group: "storage.k8s.io", resource: "storageclasses", verb: "get"},
			core("events", "create"),
		},
		forbidden: []req{core("secrets", "get")},
	}
}

// Fixture identifiers for the per-session runner Role. Every one of them is a
// made-up name; the point is only that each conditional rule in
// BuildRunnerRBAC has something to pin to, so the maximal Role is emitted.
const (
	runnerSessionNS       = "default"
	runnerSessionName     = "rbac-test-session"
	runnerSessionClass    = "rbac-test-class"
	runnerSessionPromptCM = "rbac-test-prompt"
	runnerSessionBundle   = "rbac-test-bundle"
	runnerSessionMCP      = "rbac-test-mcp"
	runnerSessionIdentity = "rbac-test-identity"
	runnerSessionCredSec  = "rbac-test-cred"
	runnerSessionChannel  = "rbac-test-channel"
	runnerSessionChanSec  = "rbac-test-channel-creds"
)

// applyRunnerSessionRBAC builds the per-session runner ServiceAccount + Role +
// RoleBinding through the SAME function the AgentSession reconciler calls
// (agentsession.BuildRunnerRBAC), creates them in envtest, and returns the SA's
// user string. The AgentClass/AgentSession fixture switches on every optional
// branch (configMapRef prompt, MCP servers, workspace source, bundle sessions,
// an MCP identity + its Secrets, a channel binding) so the Role under test is
// the widest one the builder can emit — a rule that only appears for one of
// those shapes still gets checked.
func applyRunnerSessionRBAC(t *testing.T, ctx context.Context, cl client.Client) string {
	t.Helper()
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{
			Name: runnerSessionName, Namespace: runnerSessionNS,
			UID: "00000000-0000-0000-0000-00000000rbac",
		},
		Spec: spiceboxv1alpha1.AgentSessionSpec{
			Class: runnerSessionClass,
			InputChannel: &spiceboxv1alpha1.ChannelBinding{
				Name: runnerSessionChannel, Kind: "fake", Key: "dm:rbac-test",
			},
		},
	}
	ac := &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: runnerSessionClass, Namespace: runnerSessionNS},
		Spec: spiceboxv1alpha1.AgentClassSpec{
			SystemPrompt: spiceboxv1alpha1.PromptSource{
				ConfigMapRef: &spiceboxv1alpha1.ConfigMapKeyRef{Name: runnerSessionPromptCM, Key: "prompt"},
			},
			MCPServers:      []spiceboxv1alpha1.AgentClassMCPServerRef{{Name: "up", Ref: runnerSessionMCP}},
			WorkspaceSource: &spiceboxv1alpha1.AgentClassWorkspaceSourceRef{Ref: "rbac-test-ws"},
		},
	}
	sa, role, rb, _, _ := agentsession.BuildRunnerRBAC(
		sess, ac,
		"rbac-test-memory-token",
		[]string{runnerSessionBundle},
		[]string{runnerSessionChanSec},
		"", "", "", "", "",
		runnerSessionIdentity,
		[]string{runnerSessionCredSec},
	)
	// The builder stamps an ownerReference at the AgentSession, which this
	// harness never creates (it runs no reconcilers). Strip it: what is under
	// test is the RULE SET, and an ownerRef to a nonexistent object would only
	// invite GC surprises.
	for _, o := range []client.Object{sa, role, rb} {
		o.SetOwnerReferences(nil)
		require.NoErrorf(t, cl.Create(ctx, o), "create per-session runner %T", o)
	}
	return "system:serviceaccount:" + runnerSessionNS + ":" + sa.Name
}

// runnerSessionRoleComponent declares the runner ServiceAccount's COMPLETE
// used-verb set against its per-session Role. Every tuple corresponds to a real
// client call in internal/cmd/runner or a meta tool; name-pinned rules are asserted WITH
// their pinned name, so a rule that stops being pinned to the right object
// fails here too.
func runnerSessionRoleComponent(saUser string) component {
	// g/core/named are all namespace-scoped: the Role is namespaced, so an SAR
	// without a namespace would be evaluated cluster-wide and always deny.
	g := func(res, verb string) req {
		return req{group: apGroup, resource: res, verb: verb, namespace: runnerSessionNS}
	}
	named := func(res, verb, name string) req {
		return req{group: apGroup, resource: res, verb: verb, namespace: runnerSessionNS, name: name}
	}
	return component{
		name: "runner (per-session Role)", saUser: saUser,
		nsRoles: []nsName{{ns: runnerSessionNS, name: runnerSessionName + "-runner"}},
		required: []req{
			// The session object itself + its status: the runner patches the
			// wake-requested-at annotation and writes phase/turn status.
			//
			// StatusPatcher.mutate — the single path behind EVERY runner-owned
			// status observation (observedPins, sidecarReachability,
			// activeWidgets, and the credential-update corroboration recorder's
			// status.credentialAuthFailures) — is a `get agentsessions` followed
			// by a `patch agentsessions/status`, both against THIS session's
			// name. So a new runner-owned status field adds no verb: it rides
			// these two rows, and their name-pinning is what the negative
			// controls below hold in place.
			named("agentsessions", "get", runnerSessionName),
			named("agentsessions", "watch", runnerSessionName),
			named("agentsessions", "patch", runnerSessionName),
			{group: apGroup, resource: "agentsessions", subresource: "status", verb: "get",
				namespace: runnerSessionNS, name: runnerSessionName},
			{group: apGroup, resource: "agentsessions", subresource: "status", verb: "patch",
				namespace: runnerSessionNS, name: runnerSessionName},
			// userPassthrough credential resolution.
			named("sessionuseridentities", "get", runnerSessionName),
			named("sessionuseridentities", "watch", runnerSessionName),
			named("agentclasses", "get", runnerSessionClass),
			// resolveSkills Lists namespace Skills at startup.
			g("skills", "get"), g("skills", "list"), g("skills", "watch"),
			// Sandbox tool dispatch (ToolCall) and artifact_prepare/await.
			g("toolcalls", "create"), g("toolcalls", "get"), g("toolcalls", "list"),
			g("toolcalls", "watch"), g("toolcalls", "delete"),
			g("artifactrenders", "create"), g("artifactrenders", "get"),
			g("artifactrenders", "list"), g("artifactrenders", "watch"),
			// request_credential_update (pkg/agent/tool/meta/credential_update.go):
			// List to reattach to an in-flight request, Create the new one, Get in
			// the poll loop. This row is the reason this component exists — the
			// rule was missing, so every invocation 403'd on the first List and no
			// suite noticed.
			g("credentialupdaterequests", "create"), g("credentialupdaterequests", "get"),
			g("credentialupdaterequests", "list"),
			// delegate (pkg/agent/tool/meta/capability/subagents.go): Create the
			// SubagentRequest, then Get in the poll loop through the same direct
			// client -- no reattach-by-List, unlike credentialupdaterequests.
			g("subagentrequests", "create"), g("subagentrequests", "get"),
			named("spiceboxsessions", "get", runnerSessionBundle),
			named("spiceboxsessions", "watch", runnerSessionBundle),
			named("mcpservers", "get", runnerSessionMCP),
			named("agentidentities", "get", runnerSessionIdentity),
			named("channels", "get", runnerSessionChannel),
			{resource: "configmaps", verb: "get", namespace: runnerSessionNS, name: runnerSessionPromptCM},
			{resource: "secrets", verb: "get", namespace: runnerSessionNS, name: runnerSessionCredSec},
			{resource: "secrets", verb: "get", namespace: runnerSessionNS, name: runnerSessionChanSec},
			// Workspace sync/apply reconcile Job.
			//
			// create, list and watch are UNNAMED because Kubernetes RBAC cannot
			// pin them: resourceNames matches the request's name, and a create has
			// none at admission while list and watch address a collection. get and
			// delete ARE pinned, so they are asserted by name -- asking without one
			// would demand the grant be widened to satisfy the test.
			{group: "batch", resource: "jobs", verb: "create", namespace: runnerSessionNS},
			{group: "batch", resource: "jobs", verb: "list", namespace: runnerSessionNS},
			{group: "batch", resource: "jobs", verb: "watch", namespace: runnerSessionNS},
			{group: "batch", resource: "jobs", verb: "get", namespace: runnerSessionNS, name: "ws-sync-" + runnerSessionName},
			{group: "batch", resource: "jobs", verb: "delete", namespace: runnerSessionNS, name: "ws-apply-" + runnerSessionName},
		},
		forbidden: []req{
			// Pinning control for the Job grant: get and delete carry
			// resourceNames, so a Job this session does not own must be denied.
			// Without this row the pin could be dropped and every "required"
			// assertion above would still pass.
			{group: "batch", resource: "jobs", verb: "get", namespace: runnerSessionNS, name: "someone-elses-job"},
			{group: "batch", resource: "jobs", verb: "delete", namespace: runnerSessionNS, name: "someone-elses-job"},
			// Pinning control: the same verb on a Secret the controller did NOT
			// resolve must be denied. A rule that loses its resourceNames (the
			// easiest way to over-grant here) flips this to allowed.
			{resource: "secrets", verb: "get", namespace: runnerSessionNS, name: "some-other-secret"},
			// The meta tool never deletes a request — owner-ref GC reaps it with
			// the session.
			g("credentialupdaterequests", "delete"),
			// …nor watches one: it polls with Get through the runner's direct,
			// uncached client, so no informer is ever established. The verb was
			// granted and used by nobody; this row keeps it that way.
			g("credentialupdaterequests", "watch"),
			// delegate never lists (no reattach-by-List) or watches (it polls
			// with Get through the runner's direct, uncached client, same as
			// credentialupdaterequests above) — either verb here would grant a
			// capability nothing uses.
			g("subagentrequests", "list"),
			g("subagentrequests", "watch"),
			// A runner may only ask for its OWN respawn.
			named("agentsessions", "patch", "another-session"),
			// …and may only write its OWN status. The credential-update
			// corroboration recorder writes status.credentialAuthFailures, which
			// the CredentialUpdateRequest reconciler reads as independent
			// evidence that a credential stopped authenticating. A runner able
			// to stamp that onto a DIFFERENT session's status could manufacture
			// corroboration for a request it is not party to — so the
			// name-pinning on the status rule, not just on the metadata rule, is
			// load-bearing.
			{group: apGroup, resource: "agentsessions", subresource: "status", verb: "patch",
				namespace: runnerSessionNS, name: "another-session"},
		},
	}
}

func runnerToolspecReaderComponent(saUser string) component {
	g := func(res, verb string) req { return req{group: apGroup, resource: res, verb: verb} }
	return component{
		name: "runner (toolspec-reader ClusterRole)", saUser: saUser,
		clusterRoles: []string{"spicebox-toolspec-reader"},
		required: []req{
			g("spiceboxtoolspecs", "get"),
			g("clusterskills", "list"),
			// Under-broad bug the audit found (expected RED until granted):
			g("spiceboxtoolkits", "get"),
		},
		forbidden: []req{{resource: "secrets", verb: "get", namespace: "default"}},
	}
}

// workshopReaderComponent declares the workshop sidecar's COMPLETE used-verb
// set against the three cluster-scoped, non-secret kinds `inventory`
// (internal/cmd/workshop/tools_inventory.go) reads: ClusterAgentSettings
// (governance ceilings + model catalog), ClusterSkill (the cluster skill
// catalog), and SpiceboxClass (the sandbox class catalog a SidecarToolbox
// names by spec.sandbox.class). get;list only — no watch (inventory answers
// one point-in-time snapshot per call, never subscribes) and no write.
func workshopReaderComponent(saUser string) component {
	g := func(res, verb string) req { return req{group: apGroup, resource: res, verb: verb} }
	return component{
		name: "workshop sidecar (reader ClusterRole)", saUser: saUser,
		clusterRoles: []string{"spicebox-workshop-reader"},
		required: []req{
			g("clusteragentsettings", "get"),
			g("clusteragentsettings", "list"),
			g("clusterskills", "get"),
			g("clusterskills", "list"),
			g("spiceboxclasses", "get"),
			g("spiceboxclasses", "list"),
		},
		forbidden: []req{{resource: "secrets", verb: "get", namespace: "default"}},
	}
}
