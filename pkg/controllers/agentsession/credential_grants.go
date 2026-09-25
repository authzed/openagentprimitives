// credential_grants.go — the operator's declarative writer for externaltoken
// token-use authorization. Before a runner pod is dispatched, the reconciler
// enumerates every credential the session is entitled to present to an
// upstream, derives a stable externaltoken credID + (for value-bindable creds)
// a per-session-keyed value hash, and reconciles the resulting authorized_token
// grants in SpiceDB so the runner/ToolCall pre-send check (externaltoken
// use_token) can fail closed on any token the operator did not bless.
//
// The write is a full declarative diff, not an append: a credential removed
// from the AgentClass/identity between reconciles has its grant deleted, and an
// unchanged surface produces zero SpiceDB writes (idempotent, SSA-friendly).
package agentsession

import (
	"context"
	"errors"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/authzed/openagentprimitives/pkg/agent/runner"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	clikind "github.com/authzed/openagentprimitives/pkg/platform/identity/authkind/cli"
	mcpkind "github.com/authzed/openagentprimitives/pkg/platform/identity/authkind/mcp"
	credkindregistry "github.com/authzed/openagentprimitives/pkg/platform/identity/credkind/registry"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/credresolve"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/externaltoken"
	"github.com/authzed/openagentprimitives/pkg/tools/adoptkit"
)

// TokenGranter is the subset of *spicedb.Client reconcileCredentialGrants needs.
// Declared as an interface (not *spicedb.Client) both for the pure unit test's
// fake and per the repo typed-nil rule for reconciler-injected dependencies.
type TokenGranter interface {
	TouchAuthorizedToken(ctx context.Context, ns, name, credID, authorizedValueHash string) error
	TouchAuthorizedTokenIdentity(ctx context.Context, ns, name, credID string) error
	DeleteAuthorizedToken(ctx context.Context, ns, name, credID string) error
	ListAuthorizedTokens(ctx context.Context, ns, name string) ([]externaltoken.AuthorizedTokenGrant, error)
}

// TokenChecker is the subset of *spicedb.Client materializeSidecarSecret needs
// for its one-time pre-handout use_token check. Declared as an interface (not
// *spicedb.Client) per the repo's typed-nil rule and so tests can inject a
// fake. Mirrors pkg/controllers/toolcall.TokenChecker's shape exactly;
// redeclared locally rather than imported because this package has no other
// reason to depend on the toolcall controller package.
type TokenChecker interface {
	CheckUseToken(ctx context.Context, ns, name, credID, presentedValueHash string, fullyConsistent bool) (bool, error)
}

// desiredGrant is one credential the session is authorized to use. An
// IdentityOnly grant is uncaveated (no value binding) — for credentials whose
// presented value the operator can't pin at reconcile time: federated (minted
// fresh per use) and oauth (refreshed out-of-band by the runner). All other
// (static) grants carry a value hash the use_token caveat binds to.
type desiredGrant struct {
	CredID       string
	ValueHash    string
	IdentityOnly bool
}

// applyGrantDiff reconciles the session's authorized_token grants toward
// `desired`. It reads the current grants once, indexes them by credID, and:
//   - federated desired grant → TouchAuthorizedTokenIdentity only if absent
//     (an existing grant with an empty AuthorizedValueHash already matches);
//   - value-bound desired grant → TouchAuthorizedToken only if absent OR the
//     stored AuthorizedValueHash differs from the desired ValueHash;
//   - any current credID not in `desired` → DeleteAuthorizedToken.
//
// It joins errors across the whole diff rather than short-circuiting, so a
// single failing write does not strand the remaining grants un-reconciled.
// A ListAuthorizedTokens failure is fatal (the diff can't be computed against
// unknown state). Pure: no Secret reads, no enumeration — unit-tested with a
// fake TokenGranter.
func applyGrantDiff(ctx context.Context, g TokenGranter, ns, name string, desired []desiredGrant) error {
	logger := log.FromContext(ctx)
	current, err := g.ListAuthorizedTokens(ctx, ns, name)
	if err != nil {
		return fmt.Errorf("list current token grants: %w", err)
	}
	currentByCredID := make(map[string]externaltoken.AuthorizedTokenGrant, len(current))
	for _, c := range current {
		currentByCredID[c.CredID] = c
	}
	desiredCredIDs := make(map[string]struct{}, len(desired))

	var errs []error
	for _, d := range desired {
		desiredCredIDs[d.CredID] = struct{}{}
		cur, present := currentByCredID[d.CredID]
		switch {
		case d.IdentityOnly:
			// Identity-only: an existing grant (of either shape) already
			// satisfies it; only write when entirely absent.
			if !present {
				if err := g.TouchAuthorizedTokenIdentity(ctx, ns, name, d.CredID); err != nil {
					errs = append(errs, fmt.Errorf("touch identity grant cred=%s: %w", d.CredID, err))
				} else {
					logger.Info("token grant: wrote identity-only authorization",
						"session", ns+"/"+name, "credID", d.CredID)
				}
			}
		default:
			if !present || cur.AuthorizedValueHash != d.ValueHash {
				if err := g.TouchAuthorizedToken(ctx, ns, name, d.CredID, d.ValueHash); err != nil {
					errs = append(errs, fmt.Errorf("touch value grant cred=%s: %w", d.CredID, err))
				} else {
					logger.Info("token grant: wrote value-bound authorization",
						"session", ns+"/"+name, "credID", d.CredID)
				}
			}
		}
	}
	for credID := range currentByCredID {
		if _, want := desiredCredIDs[credID]; want {
			continue
		}
		if err := g.DeleteAuthorizedToken(ctx, ns, name, credID); err != nil {
			errs = append(errs, fmt.Errorf("delete stale grant cred=%s: %w", credID, err))
		} else {
			logger.Info("token grant: revoked stale authorization",
				"session", ns+"/"+name, "credID", credID)
		}
	}
	return errors.Join(errs...)
}

// existingArgsHashKey returns the per-session HMAC key from the session's
// already-materialized Secret WITHOUT minting one. This is the read-only
// counterpart to the mint-or-read the main reconcile performs much later (in
// the step that also provisions the runner's RBAC + Secret); sweepRevokedGrants
// needs the key long before that step, and on code paths that must not
// provision anything.
//
// A missing Secret is not an error: it means the session has never booted, so
// reconcileCredentialGrants has never run for it and there are no grants to
// revoke. Returns (nil, nil) for that case and for a Secret predating the
// args-hash key.
func (r *Reconciler) existingArgsHashKey(ctx context.Context, sess *spiceboxv1alpha1.AgentSession) ([]byte, error) {
	sec, err := r.getSecret(ctx, types.NamespacedName{
		Namespace: sess.Namespace, Name: MemoryTokenSecretName(sess),
	})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return sec.Data[agentSessionSecretArgsHashKey], nil
}

// sweepRevokedGrants runs the credential-grant diff from the TOP of the
// reconcile, before any gate that can park it.
//
// Why a second call site exists at all: the diff is the ONLY thing that deletes
// a revoked credential's authorized_token grant, and the later call site, before
// the sidecar loop and runner dispatch, is reachable only by a session that gets
// that far. Many earlier returns do not — the AgentClass Valid gate, the
// fatal-settings and image-pin gates, the bundle-retry requeue, and most often
// the idle-sleep/archive park that is the resting state of every
// channel-attached agent. Nothing re-enqueues a session when its class goes
// invalid, so a revoke lost to one of those returns is not deferred but DROPPED:
// the grant survives indefinitely and the revoked credential keeps passing
// use_token. Revoking a credential and the class going Valid=False come from the
// SAME AgentIdentity write, so the two race on every revoke.
//
// It reads only already-persisted state — the per-session Secret's args-hash key
// and status.effectiveSettings — so it is safe on a path that must not
// provision. Stale settings cannot cause a false delete: the live grant set was
// written under those same settings, and nil settings count as unconstrained, so
// this pass can only compute a superset of what a fresh pass would.
//
// Best-effort by design: a failure is logged, not fatal. The session may be
// parked or terminal, where markBootFailed would overwrite a terminal
// FailureReason with a transient SpiceDB error. The boot path keeps its
// fail-closed behaviour at the later call site.
func (r *Reconciler) sweepRevokedGrants(
	ctx context.Context,
	sess *spiceboxv1alpha1.AgentSession,
	ac *spiceboxv1alpha1.AgentClass,
) {
	if r.TokenGranter == nil {
		return
	}
	logger := log.FromContext(ctx)
	argsHashKey, err := r.existingArgsHashKey(ctx, sess)
	if err != nil {
		logger.Info("revocation sweep: could not read the per-session args-hash key; skipping this pass",
			"session", sess.Namespace+"/"+sess.Name, "err", err.Error())
		return
	}
	if len(argsHashKey) == 0 {
		// Never booted (no per-session Secret yet) — no grants have been
		// written, so there is nothing to revoke. The later call site writes
		// the initial set once the key is minted.
		return
	}
	if err := r.reconcileCredentialGrants(ctx, sess, ac, argsHashKey); err != nil {
		logger.Info("revocation sweep: grant diff failed; a revoked credential may still hold its grant",
			"session", sess.Namespace+"/"+sess.Name, "err", err.Error())
	}
}

// reconcileCredentialGrants enumerates every credential this session is entitled
// to present to an upstream, derives its externaltoken grant, and reconciles the
// authorized_token relationships in SpiceDB via applyGrantDiff. It runs after
// credential materialization and BEFORE the runner pod is dispatched, so the
// runner's use_token pre-send check always has an up-to-date grant set.
//
// Each surface is resolved the SAME way its consumer resolves it, so the grant
// and the presented credential cannot diverge:
//   - MCPServer credentials, through the session's runtime identity exactly as
//     the runner does — the surface the runner's MCP pre-send check verifies.
//   - Bundle/toolkit CLI credentials, exactly as the runner's sandbox tool
//     builder resolves them — the surface the ToolCall reconciler's sandbox
//     use_token check verifies. Without it a sandbox tool presenting a toolkit's
//     token is denied on first use, having no matching grant.
//   - Sidecar-toolbox upstream credentials, via resolveSidecarCredential, the
//     same lookup materializeSidecarSecret performs for its pre-handout check.
//   - userPassthrough static credentials, whose VALUES the operator projects
//     into the per-session Secret sandbox tools read from.
//
// Value binding: federated credentials are minted per use and oauth ones are
// refreshed out-of-band, so both get an IDENTITY-ONLY grant. Their presented
// value cannot be pinned at reconcile time, and a value-bound caveat would
// false-deny after any mint or refresh — an already-expired oauth token would
// get no grant at all. Static credentials resolve their value and bind it
// through a per-session-keyed hash.
//
// Fail-closed, with two exceptions mirroring the runner's own tolerance so this
// write never regresses a session that would otherwise boot: a static value that
// genuinely fails to resolve fails the reconcile, because the session must not
// dispatch with an un-authorizable credential; but an absent MCPServer CR is
// skipped, since the runner performs the authoritative MCP validation and
// surfaces the precise reason.
func (r *Reconciler) reconcileCredentialGrants(
	ctx context.Context,
	sess *spiceboxv1alpha1.AgentSession,
	ac *spiceboxv1alpha1.AgentClass,
	argsHashKey []byte,
) error {
	logger := log.FromContext(ctx)

	rid, suid, effectiveMode, err := r.sessionRuntimeIdentity(ctx, sess, ac)
	if err != nil {
		return err
	}

	// Collect credential sources, de-duplicated by credID (the same credential
	// can back multiple MCP servers, back multiple bundles/toolkits, or overlap
	// the passthrough set).
	sourceByCredID := make(map[string]spiceboxv1alpha1.CredentialSource)

	// valuePrefixByCredID carries the ValuePrefix (commonly "Bearer ") that a
	// header-injected MCP credential's raw value is prepended with at its point
	// of use. The runner's use_token check presents a hash of the EXACT value
	// about to go upstream, prefix included, so the grant written below must hash
	// under the SAME transform or every legitimate MCP call with a non-empty
	// prefix denies. Populated only by the MCP loop: every other surface injects
	// through a plain env var with no prefix, and those are hashed raw,
	// consistent with the zero value this map returns for an absent key.
	//
	// Structural limit: one map keyed by credID, and the durable grant carries
	// exactly one authorized_value_hash per credID. A credID used on BOTH a
	// header-injected and an env-injected surface in the SAME session can be
	// authorized under only one presented-value form — whichever loop runs last
	// wins, and the other surface's hash never matches. Supporting both needs
	// per-injection hashes keyed by (credID, injection-kind). Not needed while a
	// credential is bound to one auth surface per AgentIdentity binding, but
	// worth revisiting if that stops holding.
	valuePrefixByCredID := make(map[string]string)

	// MCP servers.
	for _, ref := range ac.Spec.MCPServers {
		var cr spiceboxv1alpha1.MCPServer
		if err := r.Client.Get(ctx, types.NamespacedName{Namespace: sess.Namespace, Name: ref.Ref}, &cr); err != nil {
			if apierrors.IsNotFound(err) {
				logger.Info("token grant: MCPServer not found yet; skipping (runner performs authoritative MCP validation)",
					"session", sess.Namespace+"/"+sess.Name, "mcpServer", ref.Ref)
				continue
			}
			return fmt.Errorf("get MCPServer %q for token grants: %w", ref.Ref, err)
		}
		reqs := mcpkind.New().SetupRequirements(ctx, mcpkind.NewTarget(&cr))
		descs, derr := credresolve.Descriptors(reqs, rid, ref.CredentialRemap)
		if derr != nil {
			// The runner owns the authoritative, user-facing MCP-auth-resolution
			// failure; don't mask it with a TokenGrantFailed reason here. The
			// grant for this server is simply not written (harmless until the
			// pre-send check exists), and the runner fails the session cleanly.
			logger.Info("token grant: MCP credential resolution failed; skipping this server's grant",
				"session", sess.Namespace+"/"+sess.Name, "mcpServer", ref.Ref, "err", derr.Error())
			continue
		}
		for _, d := range descs {
			credID := externaltoken.CredID(d.Source)
			sourceByCredID[credID] = d.Source
			if d.Inject.Header != nil {
				valuePrefixByCredID[credID] = d.Inject.Header.ValuePrefix
			}
		}
	}

	// Bundle/toolkit CLI credentials. This MUST derive byte-identical credential
	// Sources to what the runner stamps onto ToolCall.Spec.Credentials, or the
	// sandbox use_token check will not find the grant and denies the tool on
	// first use. Parity is STRUCTURAL: every credID-relevant step calls the SAME
	// shared function the runner's sandbox tool builder calls — the
	// effective-settings gate, the per-bundle CLI identity, the toolkit
	// resolution, the CLI setup requirements, and the credential descriptors.
	// Only the loop skeleton is mirrored, and it derives no credID of its own.
	// Keep it in sync with the runner's bundle loop.
	//
	// Iterating ac.Spec.ToolBundles rather than sess.Status.BundleSessions covers
	// the full declared surface regardless of provisioning timing. An extra grant
	// for a not-yet-provisioned bundle is harmless — the check only asks that a
	// grant EXISTS for a credID actually presented — whereas a missing one is the
	// exact deny-on-first-use bug this closes.
	for _, bundleCfg := range ac.Spec.ToolBundles {
		bundleRID, brErr := runner.BundleRuntimeIdentity(ctx, r.Client, sess.Namespace, rid, ac, bundleCfg, effectiveMode)
		if brErr != nil {
			// The runner performs the authoritative, user-facing bundle-identity
			// resolution; don't mask it here. This bundle's grants are simply not
			// written (harmless until the runner actually resolves it and presents
			// a credential), and the runner fails the session cleanly.
			logger.Info("token grant: bundle runtime identity resolution failed; skipping this bundle's grants",
				"session", sess.Namespace+"/"+sess.Name, "bundle", bundleCfg.Name, "err", brErr.Error())
			continue
		}
		for _, tsName := range bundleCfg.Toolspecs {
			var ts spiceboxv1alpha1.SpiceboxToolspec
			if err := r.Client.Get(ctx, types.NamespacedName{Name: tsName}, &ts); err != nil {
				if apierrors.IsNotFound(err) {
					logger.Info("token grant: SpiceboxToolspec not found yet; skipping (runner performs authoritative toolspec resolution)",
						"session", sess.Namespace+"/"+sess.Name, "bundle", bundleCfg.Name, "toolspec", tsName)
					continue
				}
				return fmt.Errorf("get SpiceboxToolspec %q for bundle %q token grants: %w", tsName, bundleCfg.Name, err)
			}
			// Mirror the runner's allowlist gate: a toolkit the runner skips must
			// not be blessed a grant (and one it injects must be).
			if !runner.ToolkitAllowed(sess.Status.EffectiveSettings, ts.Spec.Toolkit.Name) {
				continue
			}
			tk, tkErr := runner.ResolveToolkitForBundle(ctx, r.Client, ts.Spec.Toolkit.Name)
			if tkErr != nil {
				logger.Info("token grant: toolkit did not resolve; skipping its grants (runner surfaces the authoritative failure)",
					"session", sess.Namespace+"/"+sess.Name, "bundle", bundleCfg.Name, "toolkit", ts.Spec.Toolkit.Name, "err", tkErr.Error())
				continue
			}
			reqs := clikind.New().SetupRequirements(ctx, clikind.NewTarget(tk))
			descs, dErr := credresolve.Descriptors(reqs, bundleRID, bundleCfg.CredentialRemap)
			if dErr != nil {
				logger.Info("token grant: bundle CLI credential resolution failed; skipping this toolspec's grant",
					"session", sess.Namespace+"/"+sess.Name, "bundle", bundleCfg.Name, "toolspec", tsName, "err", dErr.Error())
				continue
			}
			for _, d := range descs {
				sourceByCredID[externaltoken.CredID(d.Source)] = d.Source
			}
		}
	}

	// Sidecar-toolbox upstream credentials. Unlike MCP/bundle credentials,
	// sidecar credIDs are operator-internal: both the grant-write (here) and
	// the pre-handout use_token check (materializeSidecarSecret, later in
	// this same reconcile) run in the operator — there is no runner-side
	// derivation to keep parity with, only these two operator call sites
	// agreeing with each other. resolveSidecarCredential is the single
	// shared derivation both use.
	//
	// Iterating ac.Spec.SidecarToolboxes — the declared surface — rather
	// than sess.Status.ResolvedSidecarToolboxes mirrors the bundle loop's
	// ac.Spec.ToolBundles choice above and, critically, means this enumerates
	// correctly even on the pass where reconcileCredentialGrants runs BEFORE
	// the sidecar materialization loop computes that status field (the
	// ordering the pre-handout check requires — see the call site in
	// controller.go).
	for _, ref := range ac.Spec.SidecarToolboxes {
		var tb spiceboxv1alpha1.SidecarToolbox
		if err := r.Client.Get(ctx, types.NamespacedName{Namespace: sess.Namespace, Name: ref.Ref}, &tb); err != nil {
			if apierrors.IsNotFound(err) {
				// The sidecar materialization loop performs the authoritative,
				// user-facing "SidecarToolbox missing" boot failure; don't mask
				// it with a TokenGrantFailed reason here.
				logger.Info("token grant: SidecarToolbox not found yet; skipping (sidecar materialization performs the authoritative check)",
					"session", sess.Namespace+"/"+sess.Name, "sidecar", ref.Ref)
				continue
			}
			return fmt.Errorf("get SidecarToolbox %q for token grants: %w", ref.Ref, err)
		}
		_, _, src, ok, cerr := r.resolveSidecarCredential(ctx, sess, ac, ref.Ref, tb.Spec.UpstreamAuth)
		if cerr != nil {
			// A structural resolution failure (AgentIdentity missing, credential
			// not found) — materializeSidecarSecret hits the identical failure
			// later this same reconcile and reports it authoritatively via
			// markBootFailed; don't duplicate that here.
			logger.Info("token grant: sidecar credential resolution failed; skipping this sidecar's grant",
				"session", sess.Namespace+"/"+sess.Name, "sidecar", ref.Ref, "err", cerr.Error())
			continue
		}
		if !ok {
			// No upstream credential declared for this sidecar (echo-example path).
			continue
		}
		sourceByCredID[externaltoken.CredID(src)] = src
	}

	// userPassthrough projected credentials: their values are projected into
	// the per-session Secret sandbox tools read from. suid is non-nil only for
	// a userPassthrough session.
	//
	// Built with credresolve.SourceFor — the SAME function the resolver uses to
	// derive the source at use time — rather than assembled here. It used to be
	// assembled here, with Type hardcoded to "static" while the resolver used
	// cred.Type, and the two ends of that join are compared by hash:
	// externaltoken.CredID digests Type along with the other coordinates. The
	// moment a second Projectable kind exists the writer would grant one object
	// id and the runner would check another, so every sandbox tool call would
	// be fail-closed denied — a total, silent loss of tool calling with nothing
	// naming the cause. Two copies that must agree byte-for-byte is the defect;
	// one call site is the fix.
	if suid != nil {
		projectedSrcs, pErr := projectedGrantSources(ctx, suid, rid)
		if pErr != nil {
			return pErr
		}
		for _, src := range projectedSrcs {
			sourceByCredID[externaltoken.CredID(src)] = src
		}
	}

	// Resolve each unique source into a desiredGrant.
	desired := make([]desiredGrant, 0, len(sourceByCredID))
	for credID, src := range sourceByCredID {
		// Classify through the credkind registry rather than switching on
		// src.Type. An unregistered type fails the reconcile closed here rather
		// than falling through to resolveGrantValue, which would ask for a
		// value from a credential type that may not have one.
		k, kErr := credkindregistry.Get(src.Type)
		if kErr != nil {
			return fmt.Errorf("classify token grant cred=%s (%s/%s): %w", credID, src.Namespace, src.Name, kErr)
		}
		// A Minted credential (federated: minted per use) and one that
		// NeedsRefresh (oauth: refreshed out-of-band by the runner's JIT
		// refresh) both present a value the operator can't pin at reconcile
		// time; a value-bound caveat would false-deny after any mint/refresh,
		// and an already-expired oauth token would get no grant at all.
		// Authorize them by identity only.
		if k.Minted() || k.NeedsRefresh() {
			desired = append(desired, desiredGrant{CredID: credID, IdentityOnly: true})
			continue
		}
		// Only static credentials reach here, and a static value never expires —
		// so a resolution error is a real failure: fail closed rather than
		// dispatch with an un-authorizable credential.
		val, rerr := r.resolveGrantValue(ctx, sess, src)
		if rerr != nil {
			return fmt.Errorf("resolve value for token grant cred=%s (%s/%s): %w",
				credID, src.Namespace, src.Name, rerr)
		}
		// Hash under the same transform the checking side presents (see
		// valuePrefixByCredID's doc): valuePrefixByCredID[credID] is "" for
		// every non-header-injected credID, so this is a no-op string
		// concatenation for the bundle/sidecar/passthrough surfaces.
		desired = append(desired, desiredGrant{
			CredID:    credID,
			ValueHash: externaltoken.ValueHash(argsHashKey, valuePrefixByCredID[credID]+val),
		})
	}

	return applyGrantDiff(ctx, r.TokenGranter, sess.Namespace, sess.Name, desired)
}

// resolveSidecarCredential resolves which AgentIdentity credential (if any)
// backs a sidecar toolbox's upstream credential, and derives its externaltoken
// CredentialSource via the same credresolve.SourceFor call the MCP/bundle
// enumeration above uses. This is the SINGLE place that picks the identity
// (session override, else AgentClass default) and looks up the
// "<ref>-creds" credential entry — reconcileCredentialGrants (enumeration,
// before the credential's value is even resolved) and materializeSidecarSecret
// (handout-check + the actual value resolution, later in the same reconcile)
// both call this, so they can never derive different Sources — and therefore
// different externaltoken credIDs — for the same sidecar.
//
// Returns ok=false (zero Source, nil error) when the sidecar declares no
// upstream credential need — no envVar, no provider, or no identity to
// resolve against (the echo-example path). This mirrors
// materializeSidecarSecret's own early-return condition exactly.
func (r *Reconciler) resolveSidecarCredential(
	ctx context.Context,
	sess *spiceboxv1alpha1.AgentSession,
	ac *spiceboxv1alpha1.AgentClass,
	ref string,
	upstreamAuth spiceboxv1alpha1.SidecarToolboxUpstream,
) (ident *spiceboxv1alpha1.AgentIdentity, cred *spiceboxv1alpha1.AgentCredential, src spiceboxv1alpha1.CredentialSource, ok bool, err error) {
	envVar := upstreamAuth.EnvVar
	provider := upstreamAuth.Provider

	// Pick the AgentIdentity: session override wins, else AgentClass default.
	identityName := sess.Spec.AgentIdentity
	if identityName == "" {
		identityName = ac.Spec.AgentIdentity
	}
	if envVar == "" || provider == "" || identityName == "" {
		return nil, nil, spiceboxv1alpha1.CredentialSource{}, false, nil
	}

	var id spiceboxv1alpha1.AgentIdentity
	if err := r.Client.Get(ctx, types.NamespacedName{Namespace: sess.Namespace, Name: identityName}, &id); err != nil {
		return nil, nil, spiceboxv1alpha1.CredentialSource{}, false, fmt.Errorf("get AgentIdentity %q: %w", identityName, err)
	}

	// The credential name round-trips with the toolbox: authkind's
	// SetupRequirements, which suggests "<SidecarToolbox name>-creds". Using
	// the same derivation here ties the credential the user created via
	// `oap agent setup-identity` to the one resolved at session boot.
	credName := ref + "-creds"
	c := findSidecarCredential(id.Spec.Credentials, credName)
	if c == nil {
		return nil, nil, spiceboxv1alpha1.CredentialSource{}, false, fmt.Errorf(
			"AgentIdentity %q has no credential %q for sidecar %q (run `oap agent setup-identity`)",
			identityName, credName, ref)
	}

	rid := credresolve.RuntimeIdentityFromAgentIdentity(&id)
	src, err = credresolve.SourceFor(c, rid)
	if err != nil {
		return nil, nil, spiceboxv1alpha1.CredentialSource{}, false, fmt.Errorf("resolve source for sidecar credential %q: %w", credName, err)
	}
	return &id, c, src, true, nil
}

// sessionRuntimeIdentity resolves the RuntimeIdentity the session's credentials
// resolve against, mirroring the runner's effective-mode logic so the operator
// and runner cannot drift. For a userPassthrough session it also returns the
// loaded SessionUserIdentity (so the caller can enumerate the projected static
// set); the returned suid is nil for every other mode. It also returns the
// RESOLVED effective mode ("agent" | "userPassthrough") so the caller can feed
// it to runner.BundleRuntimeIdentity — a per-bundle CLI identity that switches
// on the SAME resolved mode the runner uses (never the raw class IdentityMode).
// projectedGrantSources returns the credential source for each of a
// userPassthrough session's PROJECTED credentials — the ones whose values the
// operator wrote into the per-session Secret sandbox tools read from.
//
// It is a named function purely so a test can hold the WRITER's derivation
// next to the READER's and compare the two CredIDs. That comparison is the
// whole point: the two ends of this join are matched by hash, never by
// reference. externaltoken.CredID digests Type along with the coordinates, so
// a writer that says "static" while the resolver says cred.Type grants one
// object id and checks another. With one Projectable kind registered the two
// spellings agree and nothing shows; with a second, every sandbox tool call is
// fail-closed denied and nothing names the cause.
//
// Which is why this builds nothing itself: credresolve.SourceFor IS the
// resolver's own builder, so the two sides are the same code rather than two
// copies kept in step.
func projectedGrantSources(
	ctx context.Context,
	suid *spiceboxv1alpha1.SessionUserIdentity,
	rid credresolve.RuntimeIdentity,
) ([]spiceboxv1alpha1.CredentialSource, error) {
	projected := staticPassthroughCredentials(ctx, suid)
	out := make([]spiceboxv1alpha1.CredentialSource, 0, len(projected))
	for i := range projected {
		cred := &projected[i]
		src, err := credresolve.SourceFor(cred, rid)
		if err != nil {
			return nil, fmt.Errorf("token grant: projected passthrough credential %q: %w", cred.Name, err)
		}
		out = append(out, src)
	}
	return out, nil
}

func (r *Reconciler) sessionRuntimeIdentity(
	ctx context.Context,
	sess *spiceboxv1alpha1.AgentSession,
	ac *spiceboxv1alpha1.AgentClass,
) (credresolve.RuntimeIdentity, *spiceboxv1alpha1.SessionUserIdentity, string, error) {
	// Effective mode: an ask|dynamic class boots on a provisional agent identity
	// until status.effectiveIdentityMode records the resolved choice — identical
	// to internal/cmd/runner's resolution, and shared with the passthrough gate so the
	// two cannot drift (see ResolveEffectiveIdentityMode for why status may not
	// promote a static class).
	mode := ResolveEffectiveIdentityMode(sess, ac)

	if mode == spiceboxv1alpha1.IdentityModeUserPassthrough {
		var suid spiceboxv1alpha1.SessionUserIdentity
		if err := r.Client.Get(ctx, types.NamespacedName{Namespace: sess.Namespace, Name: sess.Name}, &suid); err != nil {
			return credresolve.RuntimeIdentity{}, nil, mode, fmt.Errorf("get SessionUserIdentity %s/%s for token grants: %w", sess.Namespace, sess.Name, err)
		}
		return credresolve.RuntimeIdentityFromSessionUserIdentity(&suid), &suid, mode, nil
	}

	// agent mode (static agent/unset, or ask|dynamic provisional/resolved-agent).
	// Note: a class-level AgentIdentity of "" still needs the bundle enumeration
	// to run — a ToolBundle may carry its own agentIdentity override — so the
	// caller must not treat a zero RuntimeIdentity here as "no bundle grants".
	if ac.Spec.AgentIdentity == "" {
		return credresolve.RuntimeIdentity{}, nil, mode, nil // no class-level credentials
	}
	var ai spiceboxv1alpha1.AgentIdentity
	if err := r.Client.Get(ctx, types.NamespacedName{Namespace: sess.Namespace, Name: ac.Spec.AgentIdentity}, &ai); err != nil {
		return credresolve.RuntimeIdentity{}, nil, mode, fmt.Errorf("get AgentIdentity %q for token grants: %w", ac.Spec.AgentIdentity, err)
	}
	return credresolve.RuntimeIdentityFromAgentIdentity(&ai), nil, mode, nil
}

// resolveGrantValue reads the underlying token value for a static credential
// source. It adopts the backing Secret so the guarded live reader permits the
// read (mirroring materializeSidecarSecret / the passthrough projection), then
// resolves via the shared credresolve path. Callers MUST branch the
// identity-only types (federated, oauth) off before calling — federated is
// minted not read, and oauth refreshes out-of-band, so neither has a stable
// stored value to bind.
func (r *Reconciler) resolveGrantValue(
	ctx context.Context,
	sess *spiceboxv1alpha1.AgentSession,
	src spiceboxv1alpha1.CredentialSource,
) (string, error) {
	reader := client.Reader(r.Client)
	if r.SecretReader != nil {
		credRef := types.NamespacedName{Namespace: src.Namespace, Name: src.Name}
		ownerRef := types.NamespacedName{Namespace: sess.Namespace, Name: sess.Name}
		if aErr := adoptkit.AdoptSecret(ctx, r.SecretReader.Reader, r.Client, credRef, ownerRef, "AgentSession"); aErr != nil && !apierrors.IsNotFound(aErr) {
			return "", fmt.Errorf("adopt credential Secret %s/%s: %w", src.Namespace, src.Name, aErr)
		}
		reader = r.SecretReader.Reader
	}
	cred, err := credresolve.AgentCredentialFromSource(src)
	if err != nil {
		return "", err
	}
	val, err := credresolve.ResolveSecretValue(ctx, reader, src.Namespace, cred)
	if err != nil {
		return "", err
	}
	return string(val.UnderlyingValue()), nil
}
