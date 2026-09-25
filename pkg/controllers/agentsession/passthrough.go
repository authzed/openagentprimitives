package agentsession

import (
	"context"
	"fmt"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	lifecyclecore "github.com/authzed/openagentprimitives/pkg/agent/session/lifecycle"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/conditions"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	credkindregistry "github.com/authzed/openagentprimitives/pkg/platform/identity/credkind/registry"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/passthrough"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/useridentity"
)

// enterpriseSignInSentinel is inserted into the missing-credentials list when
// the session has federated MCPServer targets but the user has not yet
// authenticated via the enterprise IdP. It is a sentinel — the parkAwaitingCredentials
// path treats it like any missing credential name, prompting the user to act.
// The identityd portal recognises this sentinel and renders a "Sign in with
// your enterprise account" prompt instead of the usual credential-link form.
const enterpriseSignInSentinel = "<enterprise sign-in>"

// RequiredCredentials is a thin re-export of pkg/platform/identity/passthrough.Required.
//
// The resolver lives in pkg/platform/identity/passthrough so identityd can import
// it without the operator-controllers layering dependency that previously
// forced suggested.go to maintain a partial duplicate. This wrapper is
// kept solely so existing in-package callers (parkAwaitingCredentials,
// envtest fixtures) don't churn; new code should import
// pkg/platform/identity/passthrough directly.
func RequiredCredentials(ctx context.Context, c client.Client, ac *spiceboxv1alpha1.AgentClass) ([]string, error) {
	return passthrough.Required(ctx, c, ac)
}

// BuildSessionUserIdentity narrows the user's UserIdentity catalog to the
// subset covering requiredCredentialNames and returns the per-session
// projection CR (owned by the AgentSession) plus the sorted list of
// credential names the user has not linked. A nil ui (no UserIdentity at
// all) yields an empty subset and every required name reported missing.
// The returned CR's status is NOT set here — the caller stamps it.
//
// subject is the session's authoritative canonical subject
// (sess.Annotations[AnnotationStartedByCanonicalID]). It is threaded
// explicitly so that synthesized type=federated credentials always reference
// IdPIdentitySecretName(subject) — the correct IdP-identity Secret — even
// when ui is nil (federated-only users who have no UserIdentity CR).
//
// feds is the list of federated MCPServer targets (from
// passthrough.RequiredFederated). For each, a synthesized type=federated
// AgentCredential is appended to suid.Spec.Credentials — these servers never
// require a user link and so never appear in the missing slice.
func BuildSessionUserIdentity(
	sess *spiceboxv1alpha1.AgentSession,
	ui *spiceboxv1alpha1.UserIdentity,
	subject string,
	requiredCredentialNames []string,
	feds []passthrough.FederatedTarget,
) (*spiceboxv1alpha1.SessionUserIdentity, []string) {

	suid := &spiceboxv1alpha1.SessionUserIdentity{
		ObjectMeta: metav1.ObjectMeta{
			Name:            sess.Name,
			Namespace:       sess.Namespace,
			OwnerReferences: sessionOwnerRef(sess),
		},
		Spec: spiceboxv1alpha1.SessionUserIdentitySpec{
			AgentSession: sess.Name,
		},
	}
	if ui != nil {
		suid.Spec.UserIdentity = ui.Name
		suid.Spec.Subject = ui.Spec.Subject
	}

	have := map[string]*spiceboxv1alpha1.AgentCredential{}
	if ui != nil {
		for i := range ui.Spec.Credentials {
			c := &ui.Spec.Credentials[i]
			have[c.Name] = c
		}
	}

	var missing []string
	for _, name := range requiredCredentialNames {
		c, ok := have[name]
		if !ok {
			missing = append(missing, name)
			continue
		}
		suid.Spec.Credentials = append(suid.Spec.Credentials, *c)
	}

	// Synthesize a type=federated credential for each federated MCPServer. These
	// are minted on demand from the user's IdP identity, need no user link, and
	// so never enter the missing slice. The IdP-identity Secret name MUST derive
	// from the authoritative session subject, not from ui: a federated-only user
	// logged in through the enterprise IdP has no UserIdentity CR, and deriving
	// from a nil ui yields "" — a Secret name that can never resolve, which the
	// runner experiences as a silent hang.
	for _, f := range feds {
		suid.Spec.Credentials = append(suid.Spec.Credentials, spiceboxv1alpha1.AgentCredential{
			Name: f.CredentialName,
			Type: "federated",
			Federated: &spiceboxv1alpha1.FederatedCredentialSource{
				Resource:          f.Resource,
				ResourceServerURL: f.ResourceServerURL,
				IdPSecretRef:      spiceboxv1alpha1.SecretRef{Name: useridentity.IdPIdentitySecretName(subject)},
			},
		})
	}

	sort.Strings(missing)
	return suid, missing
}

// BuildPassthroughSecretRBAC produces the Role + RoleBinding, both in the
// agentprimitives-identities namespace, that grant a passthrough session's
// runner ServiceAccount READ on EXACTLY the type=oauth MASTER Secrets the
// session needs — resourceName-scoped, nothing else. type=static credential
// VALUES are projected into a per-session Secret in the session namespace
// instead (see BuildPassthroughCredentialSecretRBAC), so the runner no longer
// needs cross-namespace read of static masters.
//
// `get`, deliberately, and NOTHING more. A Kubernetes read returns every key of
// a Secret, so this grant is bounded by what the master holds: tokens only. The
// RFC 6749 redemption material that would make a refresh_token independently
// usable — token_endpoint, client_id, client_secret — lives in a sibling Secret
// this Role does not name, so a compromised runner reads a refresh token it has
// no endpoint, client identity, or authenticator to redeem. `update` is withheld
// for the same reason: with the material out of reach the runner cannot refresh
// at all, so the verb would be pure attack surface — a prompt-injected runner
// clobbering the human's credential. Refresh belongs to the operator's refresh
// reconcilers, which see both Secrets; the runner raises a loud error instead.
//
// Two things elsewhere must hold for that bound to be real:
//
//   - A credential whose redemption material still sits on the master this Role
//     names is not yet bounded as described. The useridentity refresh reconciler
//     migrates those into the sibling.
//   - The sibling is absent from resourceNames, so the runner's Get of it
//     returns Forbidden rather than NotFound, and refresh treats that as a
//     REFUSAL — never as licence to fall back to the master's own keys. A caller
//     denied the material is denied the write-back too, and would otherwise
//     consume a rotating refresh token it cannot persist and wedge the
//     credential.
//
// These objects live in a different namespace than the AgentSession, so they
// cannot carry an owner reference; the finalizer deletes them by their
// deterministic names.
//
// Empty secretNames returns (nil, nil): a Role with empty ResourceNames would
// grant access to ALL Secrets in the namespace, and an all-static session has no
// oauth master to read. The caller must skip nil objects.
func BuildPassthroughSecretRBAC(sess *spiceboxv1alpha1.AgentSession, secretNames []string) (*rbacv1.Role, *rbacv1.RoleBinding) {
	if len(secretNames) == 0 {
		return nil, nil
	}
	roleName := PassthroughRoleName(sess)
	role := &rbacv1.Role{
		TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "Role"},
		ObjectMeta: metav1.ObjectMeta{Name: roleName, Namespace: spiceboxv1alpha1.IdentitiesNamespace},
		Rules: []rbacv1.PolicyRule{{
			APIGroups:     []string{""},
			Resources:     []string{"secrets"},
			ResourceNames: secretNames,
			Verbs:         []string{"get"},
		}},
	}
	rb := &rbacv1.RoleBinding{
		TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "RoleBinding"},
		ObjectMeta: metav1.ObjectMeta{Name: roleName, Namespace: spiceboxv1alpha1.IdentitiesNamespace},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: roleName},
		Subjects: []rbacv1.Subject{{
			Kind:      "ServiceAccount",
			Name:      sess.Name + "-runner-sa", // matches BuildRunnerRBAC's SA name
			Namespace: sess.Namespace,
		}},
	}
	return role, rb
}

// BuildPassthroughCredentialSecretRBAC produces the Role + RoleBinding, both in
// the SESSION namespace, that grant the runner ServiceAccount get on EXACTLY the
// per-session projected credential Secret (where the operator writes type=static
// credential VALUES). The runner's in-process broker reads it for MCP header
// injection; the sandbox ToolCall path is resolved operator-side and does not
// rely on this grant. Read-only (get): the projected copy is never refreshed —
// refresh stays controller-driven on the master.
//
// Unlike the identities-namespace master grant, these objects share the session
// namespace and so carry the AgentSession owner reference — owner-ref GC reaps
// them; no finalizer step is needed. secretName empty → returns (nil, nil).
func BuildPassthroughCredentialSecretRBAC(sess *spiceboxv1alpha1.AgentSession, secretName string) (*rbacv1.Role, *rbacv1.RoleBinding) {
	if secretName == "" {
		return nil, nil
	}
	roleName := sess.Name + "-passthrough-cred-reader"
	role := &rbacv1.Role{
		TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "Role"},
		ObjectMeta: metav1.ObjectMeta{Name: roleName, Namespace: sess.Namespace, OwnerReferences: sessionOwnerRef(sess)},
		Rules: []rbacv1.PolicyRule{{
			APIGroups:     []string{""},
			Resources:     []string{"secrets"},
			ResourceNames: []string{secretName},
			Verbs:         []string{"get"},
		}},
	}
	rb := &rbacv1.RoleBinding{
		TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "RoleBinding"},
		ObjectMeta: metav1.ObjectMeta{Name: roleName, Namespace: sess.Namespace, OwnerReferences: sessionOwnerRef(sess)},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: roleName},
		Subjects: []rbacv1.Subject{{
			Kind:      "ServiceAccount",
			Name:      sess.Name + "-runner-sa", // matches BuildRunnerRBAC's SA name
			Namespace: sess.Namespace,
		}},
	}
	return role, rb
}

// PassthroughRoleName is the deterministic name of a passthrough session's
// Role/RoleBinding in the identities namespace. Derived from the session
// UID so it is globally unique and the finalizer can reap it by name.
func PassthroughRoleName(sess *spiceboxv1alpha1.AgentSession) string {
	return "passthrough-" + string(sess.UID)
}

// oauthMasterSecretNames returns the deduplicated MASTER Secret names for
// every credential whose type NeedsRefresh (today: oauth) in the
// SessionUserIdentity's credential subset — the only master Secrets the
// runner's broker still reads directly from the identities namespace (for the
// access_token). A Projectable credential's VALUE (today: static) is
// projected into a per-session Secret in the session namespace instead
// (its master is no longer read by the runner), so it is deliberately
// excluded here. Dispatches through the credkind registry rather than
// switching on c.Type; a credential whose type nothing registers is logged
// and skipped — fail-closed for a collector that decides what the runner's
// scoped RBAC grants access to.
func oauthMasterSecretNames(ctx context.Context, suid *spiceboxv1alpha1.SessionUserIdentity) []string {
	seen := map[string]bool{}
	var out []string
	for _, c := range suid.Spec.Credentials {
		k, err := credkindregistry.Get(c.Type)
		if err != nil {
			log.FromContext(ctx).Info("passthrough: skipping credential with unclassifiable type from oauth master Secret RBAC",
				"credential", c.Name, "type", c.Type, "err", err.Error())
			continue
		}
		if !k.NeedsRefresh() {
			continue
		}
		// LOCATE the backing Secret through the kind's own SecretRef rather
		// than reading c.OAuth directly: k.SecretRef already guards a
		// misdeclared credential (NeedsRefresh true, typed block nil) by
		// returning nil, and a future NeedsRefresh kind with a differently
		// shaped block is picked up here for free instead of being silently
		// omitted from the runner's scoped RBAC allowlist.
		ref := k.SecretRef(c)
		if ref == nil {
			continue
		}
		n := ref.Name
		if n != "" && !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

// staticPassthroughCredentials returns the SessionUserIdentity's Projectable
// credentials (today: static) — the subset whose VALUES the operator
// projects into the per-session credential Secret. Dispatches through the
// credkind registry rather than switching on c.Type; a credential whose type
// nothing registers is logged and skipped — fail-closed for a collector that
// decides what gets projected.
func staticPassthroughCredentials(ctx context.Context, suid *spiceboxv1alpha1.SessionUserIdentity) []spiceboxv1alpha1.AgentCredential {
	var out []spiceboxv1alpha1.AgentCredential
	for i := range suid.Spec.Credentials {
		c := suid.Spec.Credentials[i]
		k, err := credkindregistry.Get(c.Type)
		if err != nil {
			log.FromContext(ctx).Info("passthrough: skipping credential with unclassifiable type from static projection",
				"credential", c.Name, "type", c.Type, "err", err.Error())
			continue
		}
		if k.Projectable() {
			out = append(out, c)
		}
	}
	return out
}

// defaultCredentialLinkTimeout matches the AgentClass CRD default.
const defaultCredentialLinkTimeout = 30 * time.Minute

// reconcilePassthroughIdentity is the passthrough-identity gate. For an
// identityMode=userPassthrough class it computes the agent's required
// credential names, intersects with the starter's UserIdentity, and:
//   - parks the session in AwaitingCredentials (creating/updating the
//     SessionUserIdentity with MissingCredentials) when anything is missing,
//   - fails the session when the credential-link deadline has passed,
//   - otherwise writes the resolved SessionUserIdentity + scoped RBAC and
//     returns proceed=true.
//
// For an identityMode=agent class it is a no-op (proceed=true). The caller
// returns (result, err) immediately when proceed is false.
func (r *Reconciler) reconcilePassthroughIdentity(
	ctx context.Context, sess *spiceboxv1alpha1.AgentSession, ac *spiceboxv1alpha1.AgentClass,
) (ctrl.Result, bool, error) {
	// The passthrough credential path runs for a static userPassthrough class OR
	// for an ask|dynamic session whose resolved choice was userPassthrough: the
	// runner appended IdentityChoiceResolved{userPassthrough} and the identity
	// gate mirrored it onto status.effectiveIdentityMode. The two interactive
	// modes add no new downstream credential path — they only defer WHICH mode
	// this gate binds for, so it keys off the effective mode, not just the spec.
	//
	// Resolved through the shared helper, NOT by OR-ing the spec against status:
	// status.effectiveIdentityMode is runner-writable, and an OR let a forged
	// userPassthrough on a static agent-mode session reach the
	// BuildPassthroughSecretRBAC apply below — handing that session's runner SA
	// read on the human starter's OAuth master Secrets. The class is
	// authoritative for a static mode; status speaks only for ask|dynamic.
	if ResolveEffectiveIdentityMode(sess, ac) != spiceboxv1alpha1.IdentityModeUserPassthrough {
		return ctrl.Result{}, true, nil
	}

	subject := spiceboxv1alpha1.StartedBySubject(sess).String()
	if subject == "" {
		// A passthrough class needs a human starter; kubectl-driven sessions
		// have none. Fail with a clear reason rather than parking forever.
		// Edge-gate the log append: only fire on the transition INTO Failed, not
		// on every reconcile of an already-failed session, to keep the signed
		// lifecycle log append-once per actual transition (not per reconcile loop).
		if !isTerminalPhase(sess.Status.Phase) {
			if err := r.applyEvent(ctx, sess, lifecyclecore.CredsTimeout{}); err != nil {
				return ctrl.Result{}, false, err
			}
		}
		sess.Status.FailureReason = spiceboxv1alpha1.ReasonMissingStarterSubject
		now := metav1.Now()
		sess.Status.FinishedAt = &now
		conditions.SetFalse(sess, &sess.Status.Conditions,
			spiceboxv1alpha1.AgentSessionConditionCredentialsReady,
			spiceboxv1alpha1.ReasonMissingStarterSubject,
			"identityMode=userPassthrough requires a channel-attached session with a known starter")
		return ctrl.Result{}, false, r.applyStatus(ctx, sess)
	}

	required, err := RequiredCredentials(ctx, r.Client, ac)
	if err != nil {
		// MCPServer not resolvable yet — requeue.
		return ctrl.Result{RequeueAfter: mcpAgentIdentityRequeueAfter}, false, nil
	}

	feds, err := passthrough.RequiredFederated(ctx, r.Client, ac)
	if err != nil {
		// MCPServer not resolvable yet — requeue.
		return ctrl.Result{RequeueAfter: mcpAgentIdentityRequeueAfter}, false, nil
	}

	// A class with no credentialed targets has nothing to GATE, and falls
	// through anyway rather than returning early: the session still needs its
	// SessionUserIdentity written, because sessionRuntimeIdentity Gets it
	// unconditionally for a userPassthrough session and fails the whole
	// session with TokenGrantFailed when it is absent. Everything below is
	// already a no-op for an empty required/feds set — BuildSessionUserIdentity
	// returns an identity with no credentials and nothing missing, both RBAC
	// builders decline to write a Role for an empty name set, and the
	// projection has nothing to project — so the one thing it does is write
	// the identity the runtime path expects, stamped like any other
	// fully-linked one.

	// Load the starter's UserIdentity (cluster-scoped, deterministic name).
	uiName := useridentity.NameForSubject(identity.Subject(subject))
	var loaded spiceboxv1alpha1.UserIdentity
	var ui *spiceboxv1alpha1.UserIdentity
	switch err := r.Client.Get(ctx, client.ObjectKey{Name: uiName}, &loaded); {
	case err == nil:
		ui = &loaded
	case errors.IsNotFound(err):
		ui = nil
	default:
		return ctrl.Result{}, false, err
	}

	// Guard: if the session needs federated credentials but the user has never
	// logged in via the federation-enabled IdP, park with a "sign in once"
	// prompt rather than a credential-link prompt. The IdP-identity Secret
	// (IdPIdentitySecretName) is written by identityd at OIDC callback time;
	// its absence means the user has not authenticated yet.
	if len(feds) > 0 {
		idpSecretName := useridentity.IdPIdentitySecretName(subject)
		var idpSecret corev1.Secret
		switch err := r.Client.Get(ctx, client.ObjectKey{
			Namespace: spiceboxv1alpha1.IdentitiesNamespace,
			Name:      idpSecretName,
		}, &idpSecret); {
		case errors.IsNotFound(err):
			// User has federated targets but has not logged in yet. Park with a
			// sentinel entry so the existing parking path fires and the user sees
			// a "sign in" prompt rather than a credential-link prompt.
			required = append(required, enterpriseSignInSentinel)
		case err != nil:
			return ctrl.Result{}, false, fmt.Errorf("check IdP identity Secret: %w", err)
			// default: Secret exists — user is logged in; synthesize normally.
		}
	}

	suid, missing := BuildSessionUserIdentity(sess, ui, subject, required, feds)
	suid.Spec.Subject = subject

	// No-silent-errors / fail-closed: when no credentials are missing (the
	// user is considered fully logged in), every synthesized federated
	// credential must reference an IdP-identity Secret that actually exists.
	// A missing one (e.g. from a subject-derivation bug) FAILS the session
	// loudly HERE, rather than silently hanging the runner at
	// credential-resolution time. We skip this check when missing is non-empty
	// (the sentinel or regular credential gap already drives parkAwaitingCredentials
	// below; there's no runner to hang in that branch).
	if len(missing) == 0 {
		for i := range suid.Spec.Credentials {
			fc := &suid.Spec.Credentials[i]
			// Classify through the credkind registry rather than switching on
			// fc.Type. This loop runs inside an error-returning reconcile step,
			// so — unlike the two slice-collector helpers above — a credential
			// the system cannot classify fails the reconcile loudly here rather
			// than being silently skipped.
			k, err := credkindregistry.Get(fc.Type)
			if err != nil {
				return ctrl.Result{}, false, fmt.Errorf("classify credential %q: %w", fc.Name, err)
			}
			if !k.Minted() || fc.Federated == nil {
				continue
			}
			var idpSec corev1.Secret
			switch err := r.Client.Get(ctx, client.ObjectKey{
				Namespace: spiceboxv1alpha1.IdentitiesNamespace,
				Name:      fc.Federated.IdPSecretRef.Name,
			}, &idpSec); {
			case errors.IsNotFound(err):
				// Credential-gate failure. Edge-gate the log append so re-reconciles of
				// an already-Failed session do not re-append the same event each time.
				if !isTerminalPhase(sess.Status.Phase) {
					if aerr := r.applyEvent(ctx, sess, lifecyclecore.CredsTimeout{}); aerr != nil {
						return ctrl.Result{}, false, aerr
					}
				}
				sess.Status.FailureReason = spiceboxv1alpha1.ReasonAgentSessionFederatedIdPSecretMissing
				now := metav1.Now()
				sess.Status.FinishedAt = &now
				conditions.SetFalse(sess, &sess.Status.Conditions,
					spiceboxv1alpha1.AgentSessionConditionCredentialsReady,
					spiceboxv1alpha1.ReasonAgentSessionFederatedIdPSecretMissing,
					fmt.Sprintf("federated credential %q references IdP-identity Secret %q which does not exist in %s",
						fc.Name, fc.Federated.IdPSecretRef.Name, spiceboxv1alpha1.IdentitiesNamespace))
				return ctrl.Result{}, false, r.applyStatus(ctx, sess)
			case err != nil:
				return ctrl.Result{}, false, fmt.Errorf("validate federated IdP Secret %q: %w", fc.Federated.IdPSecretRef.Name, err)
			}
		}
	}

	// TypeMeta is required for server-side apply.
	suid.TypeMeta = metav1.TypeMeta{
		APIVersion: spiceboxv1alpha1.SchemeBuilder.GroupVersion.String(),
		Kind:       "SessionUserIdentity",
	}

	// Apply the SessionUserIdentity (server-side apply — idempotent).
	if err := r.Client.Patch(ctx, suid,
		client.Apply, client.ForceOwnership, client.FieldOwner("agentsession-passthrough")); err != nil {
		return ctrl.Result{}, false, fmt.Errorf("apply SessionUserIdentity: %w", err)
	}

	// Re-fetch to drive status, preserving any existing ParkedAt.
	var live spiceboxv1alpha1.SessionUserIdentity
	if err := r.Client.Get(ctx, client.ObjectKey{Namespace: sess.Namespace, Name: sess.Name}, &live); err != nil {
		return ctrl.Result{}, false, err
	}

	if len(missing) > 0 {
		return r.parkAwaitingCredentials(ctx, sess, ac, &live, missing)
	}

	// All required credentials present → project type=static credential VALUES
	// into a per-session Secret (session namespace), write scoped RBAC, and
	// proceed. The projection runs every reconcile so a controller-refreshed
	// master propagates; it is idempotent.
	staticCreds := staticPassthroughCredentials(ctx, suid)
	perSessionSecretName := ""
	var newCredHashes map[string]string
	if len(staticCreds) > 0 {
		h, err := r.materializePassthroughCredentials(ctx, sess, staticCreds)
		if err != nil {
			return ctrl.Result{}, false, fmt.Errorf("materialize passthrough credentials: %w", err)
		}
		newCredHashes = h
		perSessionSecretName = spiceboxv1alpha1.PassthroughCredentialSecretName(sess.Name)
	}

	// Scoped RBAC: read on the oauth masters in the identities namespace (the
	// broker's access_token read) + the per-session projected Secret in the
	// session namespace (MCP header injection). Either may be absent (nil) for
	// a given session.
	var rbacObjs []client.Object
	if role, rb := BuildPassthroughSecretRBAC(sess, oauthMasterSecretNames(ctx, suid)); role != nil {
		rbacObjs = append(rbacObjs, role, rb)
	}
	if role, rb := BuildPassthroughCredentialSecretRBAC(sess, perSessionSecretName); role != nil {
		rbacObjs = append(rbacObjs, role, rb)
	}
	for _, obj := range rbacObjs {
		if err := r.Client.Patch(ctx, obj,
			client.Apply, client.ForceOwnership, client.FieldOwner("agentsession-passthrough")); err != nil {
			return ctrl.Result{}, false, fmt.Errorf("apply passthrough RBAC %T: %w", obj, err)
		}
	}
	prior := live.DeepCopy()
	live.Status.MissingCredentials = nil
	conditions.SetTrue(&live, &live.Status.Conditions,
		spiceboxv1alpha1.SessionUserIdentityConditionReady, spiceboxv1alpha1.ReasonSessionUserIdentityResolved)
	if err := r.Client.Status().Patch(ctx, &live, client.MergeFrom(prior)); err != nil {
		return ctrl.Result{}, false, err
	}
	credsWereReady := conditions.IsTrue(sess.Status.Conditions, spiceboxv1alpha1.AgentSessionConditionCredentialsReady)
	conditions.SetTrue(sess, &sess.Status.Conditions,
		spiceboxv1alpha1.AgentSessionConditionCredentialsReady, spiceboxv1alpha1.ReasonUserIdentityResolved)
	// Credentials linked: record CredsLinked once, on the transition to
	// CredentialsReady (a session that links after parking, or one whose creds
	// were present on the first pass). The projection moves phase back to Pending;
	// the provisioning path downstream then computes the steady-state phase. The
	// edge gate keeps a long-running session from re-appending CredsLinked on
	// every reconcile.
	if !credsWereReady {
		if err := r.applyEvent(ctx, sess, lifecyclecore.CredsLinked{}); err != nil {
			return ctrl.Result{}, false, err
		}
	}
	// Detect a replaced/removed passthrough credential and invalidate the
	// broker cache so a running session picks it up on its next tool call.
	// Sets status.passthroughCredHashes; a failed removal-emit returns an
	// error to requeue (fail-closed).
	if err := r.applyPassthroughCredInvalidation(ctx, sess, newCredHashes); err != nil {
		return ctrl.Result{}, false, err
	}
	// Persist the credentials-resolved transition now rather than relying on a
	// later write in the main reconcile. Under SSA the operator only persists the
	// fields present in its own apply, so this transition must land deterministically
	// here — mirroring parkAwaitingCredentials, which applies its own status.
	if err := r.applyStatus(ctx, sess); err != nil {
		return ctrl.Result{}, false, err
	}
	return ctrl.Result{}, true, nil
}

// parkAwaitingCredentials stamps the SessionUserIdentity's
// MissingCredentials + ParkedAt and either fails the session on deadline
// or sets phase AwaitingCredentials and returns proceed=false.
func (r *Reconciler) parkAwaitingCredentials(
	ctx context.Context, sess *spiceboxv1alpha1.AgentSession, ac *spiceboxv1alpha1.AgentClass,
	suid *spiceboxv1alpha1.SessionUserIdentity, missing []string,
) (ctrl.Result, bool, error) {
	prior := suid.DeepCopy()
	suid.Status.MissingCredentials = missing

	// Build the structured explanation for the credential-request prompt.
	// On transient resolver error: still park, but fall back to humanized
	// credential names (empty Why → the render surface fills it) so the
	// user sees something meaningful.
	expl, err := BuildExplanation(ctx, r.Client, ac, missing)
	if err != nil {
		log.FromContext(ctx).Info("passthrough: build explanation failed; parking with humanized names only",
			"session", sess.Name, "err", err.Error())
		items := make([]spiceboxv1alpha1.CredentialExplanationItem, 0, len(missing))
		for _, name := range missing {
			items = append(items, spiceboxv1alpha1.CredentialExplanationItem{
				Credential: name, Title: passthrough.HumanizeCredName(name),
			})
		}
		expl = &spiceboxv1alpha1.CredentialExplanation{Items: items}
	}
	suid.Status.Explanation = expl

	if suid.Status.ParkedAt == nil {
		now := metav1.Now()
		suid.Status.ParkedAt = &now
	}
	conditions.SetFalse(suid, &suid.Status.Conditions,
		spiceboxv1alpha1.SessionUserIdentityConditionReady,
		spiceboxv1alpha1.ReasonSessionUserIdentityMissing,
		fmt.Sprintf("user has not linked: %v", missing))
	if err := r.Client.Status().Patch(ctx, suid, client.MergeFrom(prior)); err != nil {
		return ctrl.Result{}, false, err
	}

	// Deadline check — measured from max(ParkedAt, LastInteractionAt).
	timeout := defaultCredentialLinkTimeout
	if ac.Spec.CredentialLinkTimeout != nil && ac.Spec.CredentialLinkTimeout.Duration > 0 {
		timeout = ac.Spec.CredentialLinkTimeout.Duration
	}
	deadlineBase := suid.Status.ParkedAt.Time
	if suid.Status.LastInteractionAt != nil && suid.Status.LastInteractionAt.Time.After(deadlineBase) {
		deadlineBase = suid.Status.LastInteractionAt.Time
	}
	remaining := time.Until(deadlineBase.Add(timeout))
	if remaining <= 0 {
		// Credential-link deadline elapsed → Failed. Edge-gate the log append so
		// re-reconciles of an already-Failed session (e.g. the deadline check fires
		// again before the next wake) do not re-append the same event each time.
		if !isTerminalPhase(sess.Status.Phase) {
			if err := r.applyEvent(ctx, sess, lifecyclecore.CredsTimeout{}); err != nil {
				return ctrl.Result{}, false, err
			}
		}
		sess.Status.FailureReason = spiceboxv1alpha1.ReasonCredentialLinkTimeout
		now := metav1.Now()
		sess.Status.FinishedAt = &now
		conditions.SetFalse(sess, &sess.Status.Conditions,
			spiceboxv1alpha1.AgentSessionConditionCredentialsReady,
			spiceboxv1alpha1.ReasonCredentialLinkTimeout,
			fmt.Sprintf("user did not link required credentials within %s", timeout))
		return ctrl.Result{}, false, r.applyStatus(ctx, sess)
	}

	// Park in AwaitingCredentials. Record CredsMissing once, on entry (the
	// session was not already parked); the explicit phase write keeps it parked on
	// every subsequent reconcile without re-appending the same transition.
	original := reconcileOriginal(ctx)
	if original == nil || original.Status.Phase != spiceboxv1alpha1.AgentSessionPhaseAwaitingCredentials {
		if err := r.applyEvent(ctx, sess, lifecyclecore.CredsMissing{}); err != nil {
			return ctrl.Result{}, false, err
		}
	}
	sess.Status.Phase = spiceboxv1alpha1.AgentSessionPhaseAwaitingCredentials
	conditions.SetFalse(sess, &sess.Status.Conditions,
		spiceboxv1alpha1.AgentSessionConditionCredentialsReady,
		spiceboxv1alpha1.ReasonAwaitingUserCredentials,
		fmt.Sprintf("waiting for the user to link: %v", missing))
	if err := r.applyStatus(ctx, sess); err != nil {
		return ctrl.Result{}, false, err
	}
	// Invariant: AwaitingCredentials has no runner. A userPassthrough choice made
	// from AwaitingIdentityChoice (ask|dynamic) may have left the gate runner pod
	// up — the operator keeps it alive in AwaitingIdentityChoice to drive the
	// prompt. Reap it here (idempotent; a static-userPassthrough park that never
	// had a runner is a no-op). Best-effort: log on error, never fail the park.
	if err := r.RunnerFactory.Stop(ctx, sess); err != nil {
		log.FromContext(ctx).Info("stop runner on AwaitingCredentials park",
			"session", sess.Namespace+"/"+sess.Name, "err", err.Error())
	}
	return ctrl.Result{RequeueAfter: remaining}, false, nil
}
