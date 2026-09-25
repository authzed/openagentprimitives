// Package mcpserver reconciles MCPServer CRs. The reconciler probes the
// MCP server's tools/list endpoint, validates the allowlist + compiles every
// CEL constraint, and reflects the outcome on Valid / Reachable conditions.
// Auth resolution at probe time is unauthenticated; runtime auth resolves
// the AgentIdentity credential named by spec.auth.credential at session startup.
package mcpserver

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/cel-go/cel"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	toolscache "k8s.io/client-go/tools/cache"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/authz/adoptguard"
	"github.com/authzed/openagentprimitives/pkg/authz/observe"
	"github.com/authzed/openagentprimitives/pkg/authz/observe/fromcrd"
	"github.com/authzed/openagentprimitives/pkg/authz/pinning"
	mcppin "github.com/authzed/openagentprimitives/pkg/authz/pinning/kinds/mcp"
	"github.com/authzed/openagentprimitives/pkg/authz/relwrites"
	"github.com/authzed/openagentprimitives/pkg/authz/revocation"
	"github.com/authzed/openagentprimitives/pkg/controllers/conditions"
	apreconcile "github.com/authzed/openagentprimitives/pkg/controllers/internal/reconcile"
	"github.com/authzed/openagentprimitives/pkg/tools/mcp/probe"
	mcpspec "github.com/authzed/openagentprimitives/pkg/tools/mcp/spec"
)

// +kubebuilder:rbac:groups=agentprimitives.authzed.com,resources=mcpservers,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=agentprimitives.authzed.com,resources=mcpservers/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=agentprimitives.authzed.com,resources=agentidentities,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch

// Reconciler reconciles MCPServer objects.
type Reconciler struct {
	Client             client.Client
	HTTP               *http.Client  // optional override for tests
	RevalidateInterval time.Duration // 0 → 5m
	// SecretReader is the guarded Secret reader. Reads are gated to secrets
	// the operator has adopted (carrying AdoptedLabel) or the fixed-infra
	// allowlist. Injected from main.go; reserved for auth-secret validation
	// when credential probe reads are added.
	SecretReader *adoptguard.SecretReader
	// RevokePublisher, when non-nil, receives tool-origin revocation events
	// on CR deletion so running sessions deny that origin immediately.
	// Best-effort: a nil publisher is tolerated for local-dev / no-NATS runs.
	RevokePublisher *revocation.Publisher
}

func (r *Reconciler) interval() time.Duration {
	if r.RevalidateInterval == 0 {
		return 5 * time.Minute
	}
	return r.RevalidateInterval
}

func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var srv spiceboxv1alpha1.MCPServer
	if cont, err := apreconcile.LoadInto(ctx, r.Client, req.NamespacedName, &srv); !cont {
		return ctrl.Result{}, err
	}
	if srv.DeletionTimestamp != nil {
		return ctrl.Result{}, nil
	}

	// Status as read, for finalSuccess's set-on-meaningful-change stamp of
	// LastValidatedAt.
	statusBefore := *srv.Status.DeepCopy()

	// Phase-local state shared between phases.
	var (
		live   []probe.Tool
		probed bool
		pc     = &probe.Client{HTTP: r.HTTP, URL: srv.Spec.Server.URL}
	)

	probeServer := func(ctx context.Context) apreconcile.Outcome {
		// Probe without auth — runtime auth flows through AgentIdentity
		// bindings at session startup, not at controller-probe time.
		// Many real MCP servers (Linear, GitHub) refuse unauthenticated
		// probes — that's expected and orthogonal to spec validity.
		// Reachable=False signals the probe failed; Valid is computed
		// independently from the spec (CEL + structural).
		got, err := pc.ListTools(ctx, "", "")
		if err != nil {
			conditions.SetFalse(&srv, &srv.Status.Conditions,
				spiceboxv1alpha1.MCPServerConditionReachable,
				spiceboxv1alpha1.ReasonMCPServerProbeFailed, err.Error())
			// Continue so spec-validity phases still run.
			return apreconcile.Continue()
		}
		live = got
		probed = true
		conditions.SetTrue(&srv, &srv.Status.Conditions,
			spiceboxv1alpha1.MCPServerConditionReachable,
			spiceboxv1alpha1.ReasonMCPServerProbeOK)
		// Mirror the live tool list into status regardless of allowlist
		// outcome so users can see what the server actually exposes when
		// diagnosing drift.
		names := make([]string, 0, len(live))
		for _, t := range live {
			names = append(names, t.Name)
		}
		sort.Strings(names)
		srv.Status.ObservedTools = names
		return apreconcile.Continue()
	}

	// pinCheck verifies the live tools/list manifest hash against the recorded
	// baseline and maintains status.Pin + the PinDrift condition.
	//
	// Condition polarity: PinDrift is True=healthy (no drift / verified), False=problem.
	// This matches the Valid/Reachable idiom used by sibling conditions on MCPServer:
	// True means "everything is fine", False means "something requires attention".
	// Reason PinMatch carries the healthy True; reasons PinDrifted and PinVerifyFailed
	// carry False.
	pinCheck := func(ctx context.Context) apreconcile.Outcome {
		logger := log.FromContext(ctx)
		if !probed {
			// Server is unreachable — set PinDrift=False only if there's something
			// to compare against (a baseline or assertion). No baseline = nothing to
			// report yet (TOFU baseline will be recorded on first successful probe).
			if srv.Spec.PinnedManifestHash != "" || srv.Status.Pin != nil {
				conditions.SetFalse(&srv, &srv.Status.Conditions,
					spiceboxv1alpha1.PinDriftCondition,
					spiceboxv1alpha1.ReasonPinVerifyFailed,
					"server unreachable; live manifest hash unknown")
			}
			return apreconcile.Continue()
		}

		liveHash, err := mcppin.CanonicalManifestHash(live)
		if err != nil {
			conditions.SetFalse(&srv, &srv.Status.Conditions,
				spiceboxv1alpha1.PinDriftCondition,
				spiceboxv1alpha1.ReasonPinVerifyFailed, err.Error())
			return apreconcile.Continue()
		}

		// serverInfo.version: best-effort audit metadata only. Self-reported by the
		// server, so not a security property — purely for human-readable diagnostics.
		var serverVersion string
		if info, ierr := pc.Initialize(ctx, "", ""); ierr == nil {
			serverVersion = info.Version
		} else {
			logger.Info("mcp initialize for serverInfo failed (audit metadata only)",
				"server", srv.Name, "err", ierr.Error())
		}

		// Refreeze annotation handling: when the operator has set the
		// pin-refreeze annotation, we try to re-freeze the baseline to the
		// annotated value. This must happen BEFORE the baseline/drift
		// evaluation so that a just-honored refreeze sets PinMatch, not drift.
		if refreezeVal, hasRefreeze := srv.Annotations[spiceboxv1alpha1.AnnotationPinRefreeze]; hasRefreeze && refreezeVal != "" {
			if refreezeVal == liveHash {
				// Spec assertion (frozen strength) wins if it disagrees with the
				// refreeze request — the operator must edit the spec directly.
				if srv.Spec.PinnedManifestHash != "" && srv.Spec.PinnedManifestHash != liveHash {
					// Spec assertion conflicts: reject; leave annotation so the
					// user sees it pending. Drift message explains the assertion wins.
					msg := fmt.Sprintf(
						"tools/list manifest drifted from pinned baseline: %s -> %s (server claims version %q); "+
							"refreeze annotation to %s not honored: spec.pinnedManifestHash assertion %s wins; edit spec to accept",
						srv.Spec.PinnedManifestHash, liveHash, serverVersion, refreezeVal, srv.Spec.PinnedManifestHash)
					conditions.SetFalse(&srv, &srv.Status.Conditions,
						spiceboxv1alpha1.PinDriftCondition,
						spiceboxv1alpha1.ReasonPinDrifted, msg)
					return apreconcile.Continue()
				}
				// Honored: clear the annotation via a metadata Update BEFORE
				// mutating status. This updates srv.ResourceVersion so the
				// subsequent RunPhases Status().Update won't conflict.
				delete(srv.Annotations, spiceboxv1alpha1.AnnotationPinRefreeze)
				if err := r.Client.Update(ctx, &srv); err != nil {
					return apreconcile.FailWith(fmt.Errorf("clearing pin-refreeze annotation: %w", err))
				}
				// Rewrite the pin baseline with a fresh ObservedAt.
				now := metav1.Now()
				refreezeStrength := string(pinning.StrengthUnpinned)
				if srv.Spec.PinnedManifestHash != "" {
					refreezeStrength = string(pinning.StrengthFrozen)
				}
				srv.Status.Pin = &spiceboxv1alpha1.PinRecord{
					Kind:       mcppin.KindName,
					Strength:   refreezeStrength,
					Digest:     liveHash,
					Version:    serverVersion,
					ObservedAt: &now,
					Details:    map[string]string{"toolCount": strconv.Itoa(len(live))},
				}
				conditions.SetTrue(&srv, &srv.Status.Conditions,
					spiceboxv1alpha1.PinDriftCondition, spiceboxv1alpha1.ReasonPinMatch)
				return apreconcile.Continue()
			}
			// Stale refreeze: live moved; fall through to the switch below.
			// The default case re-reads the annotation directly and appends
			// a note about the failed refreeze to the drift message.
		}

		// Determine the effective baseline. Spec assertion (frozen strength) wins;
		// fall back to a prior TOFU observation recorded in status.
		baseline := srv.Spec.PinnedManifestHash
		strength := string(pinning.StrengthFrozen)
		if baseline == "" {
			strength = string(pinning.StrengthUnpinned)
			if srv.Status.Pin != nil {
				baseline = srv.Status.Pin.Digest
			}
		}

		switch {
		case baseline == "":
			// First observation: no assertion and no prior record — record TOFU baseline.
			now := metav1.Now()
			srv.Status.Pin = &spiceboxv1alpha1.PinRecord{
				Kind:       mcppin.KindName,
				Strength:   strength,
				Digest:     liveHash,
				Version:    serverVersion,
				ObservedAt: &now,
				Details:    map[string]string{"toolCount": strconv.Itoa(len(live))},
			}
			conditions.SetTrue(&srv, &srv.Status.Conditions,
				spiceboxv1alpha1.PinDriftCondition, spiceboxv1alpha1.ReasonPinMatch)

		case liveHash == baseline:
			// Hashes match — preserve ObservedAt to avoid churning status on idle reconciles.
			if srv.Status.Pin == nil || srv.Status.Pin.Digest != liveHash {
				// No prior record, or digest changed to now match (rare edge case):
				// write a fresh record.
				now := metav1.Now()
				srv.Status.Pin = &spiceboxv1alpha1.PinRecord{
					Kind:       mcppin.KindName,
					Strength:   strength,
					Digest:     liveHash,
					Version:    serverVersion,
					ObservedAt: &now,
					Details:    map[string]string{"toolCount": strconv.Itoa(len(live))},
				}
			} else {
				// Digest unchanged — update mutable audit fields without churning ObservedAt.
				// Replace Details entirely so a prior observedDriftDigest key is cleared.
				srv.Status.Pin.Strength = strength
				srv.Status.Pin.Version = serverVersion
				srv.Status.Pin.Details = map[string]string{"toolCount": strconv.Itoa(len(live))}
			}
			conditions.SetTrue(&srv, &srv.Status.Conditions,
				spiceboxv1alpha1.PinDriftCondition, spiceboxv1alpha1.ReasonPinMatch)

		default:
			// Live hash diverged from baseline.
			msg := fmt.Sprintf(
				"tools/list manifest drifted from pinned baseline: %s -> %s (server claims version %q); set spec.pinnedManifestHash to the new hash to accept",
				baseline, liveHash, serverVersion)
			// If there's a stale refreeze annotation (value != liveHash), append a note.
			if refreezeVal, hasRefreeze := srv.Annotations[spiceboxv1alpha1.AnnotationPinRefreeze]; hasRefreeze && refreezeVal != "" {
				msg += fmt.Sprintf("; refreeze to %s not honored: live identity is %s", refreezeVal, liveHash)
			}
			conditions.SetFalse(&srv, &srv.Status.Conditions,
				spiceboxv1alpha1.PinDriftCondition,
				spiceboxv1alpha1.ReasonPinDrifted, msg)
			// Record the live identity machine-readably so `oap pin update --current`
			// can accept drift without re-probing. Guard is load-bearing: on a
			// frozen-assert first-observe the pin may be nil and we must not panic.
			if srv.Status.Pin != nil {
				if srv.Status.Pin.Details == nil {
					srv.Status.Pin.Details = make(map[string]string)
				}
				srv.Status.Pin.Details["observedDriftDigest"] = liveHash
			}
			// Baseline is NOT auto-updated on drift — operator must explicitly accept.
		}
		return apreconcile.Continue()
	}

	allowlistDriftCheck := func(ctx context.Context) apreconcile.Outcome {
		if !probed {
			return apreconcile.Continue()
		}
		liveLookup := map[string]bool{}
		for _, t := range live {
			liveLookup[t.Name] = true
		}
		var missing []string
		for _, t := range srv.Spec.Tools {
			if !liveLookup[t.Name] {
				missing = append(missing, t.Name)
			}
		}
		if len(missing) > 0 {
			sort.Strings(missing)
			msg := fmt.Sprintf("allowlist names not on server: %v", missing)
			conditions.SetFalse(&srv, &srv.Status.Conditions,
				spiceboxv1alpha1.MCPServerConditionValid,
				spiceboxv1alpha1.ReasonMCPServerAllowlistDrift, msg)
			return apreconcile.StopAfter()
		}
		return apreconcile.Continue()
	}

	// The routeViaSessionGrantCheck phase that lived here is gone with the
	// field it policed. It required every routeViaSessionGrant permission to
	// reference a `wildcard: true` relation, because the post-approval walk
	// re-evaluated the permission for a user who lacked it and could only pass
	// through such a leaf. Approvals now write the grant on the resource
	// pointing at the session, so nothing needs a wildcard and there is no
	// invariant left to enforce.

	// labelsCompileCheck verifies that every per-tool labels block
	// compiles cleanly as CEL. Per-block: when (bool), forEach
	// (dyn list), label.resourceType / label.id / label.name (all
	// string). A malformed expression here would only surface at
	// runtime as a per-tool log entry (labels are non-fatal), so
	// admission-time catching saves operators from a silent
	// "approvals don't show names" outcome with no obvious cause.
	labelsCompileCheck := func(ctx context.Context) apreconcile.Outcome {
		var bad []string
		compile := func(label, expr string, want *cel.Type) {
			if expr == "" {
				return
			}
			env, err := cel.NewEnv(
				cel.Variable("args", cel.DynType),
				cel.Variable("result", cel.DynType),
				cel.Variable("item", cel.DynType),
			)
			if err != nil {
				bad = append(bad, fmt.Sprintf("%s: env: %v", label, err))
				return
			}
			ast, iss := env.Compile(expr)
			if iss != nil && iss.Err() != nil {
				bad = append(bad, fmt.Sprintf("%s: %s", label, iss.Err().Error()))
				return
			}
			if want != cel.DynType && ast.OutputType() != want && ast.OutputType() != cel.DynType {
				bad = append(bad, fmt.Sprintf("%s: expected %s, got %s",
					label, want, ast.OutputType()))
			}
		}
		for _, tl := range srv.Spec.Tools {
			for i, lb := range tl.Labels {
				prefix := fmt.Sprintf("tools[%s].labels[%d]", tl.Name, i)
				compile(prefix+".when", lb.When, cel.BoolType)
				compile(prefix+".forEach", lb.ForEach, cel.DynType)
				compile(prefix+".label.resourceType", lb.Label.ResourceType, cel.StringType)
				compile(prefix+".label.id", lb.Label.ID, cel.StringType)
				compile(prefix+".label.name", lb.Label.Name, cel.StringType)
			}
		}
		if len(bad) > 0 {
			conditions.SetFalse(&srv, &srv.Status.Conditions,
				spiceboxv1alpha1.MCPServerConditionValid,
				spiceboxv1alpha1.ReasonMCPServerLabelsCompileError,
				strings.Join(bad, "; "))
			return apreconcile.StopAfter()
		}
		return apreconcile.Continue()
	}

	// crossRefCheck verifies that every resourceType referenced from a
	// tool's Permission.Check or PermissionVariants[*].Check is declared
	// in this MCPServer's spec.spiceDBSchema.resources (or is the
	// implicit `user` type). The composer concatenates per-MCPServer
	// fragments into a single SpiceDB schema; a dangling resourceType
	// here would yield a schema that fails server-side compilation at
	// AgentClass guardian-write time. Catching it locally on the
	// MCPServer surfaces the broken reference next to its source CR
	// instead of in a downstream AgentClass error message. Slice-4 T18.
	// authCredentialCheck enforces the no-inference contract: an MCPServer
	// that declares spec.auth.provider must ALSO declare spec.auth.credential
	// explicitly. setup-identity and the runner descriptor both resolve the
	// credential name via passthroughcatalog.CredentialNameForServer, which
	// returns "" when Credential is empty — there is no metadata.name
	// fallback. Without an explicit credential, the runner injects no
	// auth header at session start and the upstream MCP returns 401, with
	// no actionable signal at admission time. Surface it here.
	authCredentialCheck := func(ctx context.Context) apreconcile.Outcome {
		if srv.Spec.Auth.Provider != "" && srv.Spec.Auth.Credential == "" {
			conditions.SetFalse(&srv, &srv.Status.Conditions,
				spiceboxv1alpha1.MCPServerConditionValid,
				spiceboxv1alpha1.ReasonMCPServerAuthCredentialMissing,
				"spec.auth.provider is set but spec.auth.credential is empty; declare the credential name explicitly (no metadata.name fallback)")
			return apreconcile.StopAfter()
		}
		return apreconcile.Continue()
	}

	crossRefCheck := func(ctx context.Context) apreconcile.Outcome {
		declared := map[string]bool{"user": true}
		if srv.Spec.SpiceDBSchema != nil {
			for _, r := range srv.Spec.SpiceDBSchema.Resources {
				declared[r.Name] = true
			}
			for _, name := range extractRawZedDefinitions(srv.Spec.SpiceDBSchema.RawZed) {
				declared[name] = true
			}
		}
		var missing []string
		for _, tl := range srv.Spec.Tools {
			if tl.Permission != nil && tl.Permission.Check != nil && tl.Permission.Check.ResourceType != "" {
				if !declared[tl.Permission.Check.ResourceType] {
					missing = append(missing, fmt.Sprintf("tools[%s].permission.check.resourceType=%q", tl.Name, tl.Permission.Check.ResourceType))
				}
			}
			for j, v := range tl.PermissionVariants {
				if v.Check.Check != nil && v.Check.Check.ResourceType != "" && !declared[v.Check.Check.ResourceType] {
					missing = append(missing, fmt.Sprintf("tools[%s].permissionVariants[%d].check.resourceType=%q", tl.Name, j, v.Check.Check.ResourceType))
				}
			}
		}
		if len(missing) > 0 {
			conditions.SetFalse(&srv, &srv.Status.Conditions,
				spiceboxv1alpha1.MCPServerConditionValid,
				spiceboxv1alpha1.ReasonMCPServerSpicedbSchemaMissing,
				strings.Join(missing, "; "))
			return apreconcile.StopAfter()
		}
		return apreconcile.Continue()
	}

	compileCEL := func(ctx context.Context) apreconcile.Outcome {
		sp, ferr := srv.Spec.ToSpec()
		if ferr != nil {
			conditions.SetFalse(&srv, &srv.Status.Conditions,
				spiceboxv1alpha1.MCPServerConditionValid,
				spiceboxv1alpha1.ReasonMCPServerConstraintCompileError,
				fmt.Sprintf("ToSpec: %v", ferr))
			return apreconcile.StopAfter()
		}
		if _, cerr := mcpspec.Compile(sp); cerr != nil {
			conditions.SetFalse(&srv, &srv.Status.Conditions,
				spiceboxv1alpha1.MCPServerConditionValid,
				spiceboxv1alpha1.ReasonMCPServerConstraintCompileError, cerr.Error())
			return apreconcile.StopAfter()
		}
		// writesRelationships AND observes CEL, both compiled through the SAME
		// env relwrites executes with (observes via the sibling observe
		// package, which itself compiles through relwrites). Without this the
		// expressions were first compiled during a live tool call, so a server
		// with an uncompilable expression reported Valid=True and failed
		// mid-turn in front of the user. The sibling `labels` surface was
		// already validated here; this closes that inconsistency.
		if msg := compileToolCEL(&srv); msg != "" {
			conditions.SetFalse(&srv, &srv.Status.Conditions,
				spiceboxv1alpha1.MCPServerConditionValid,
				spiceboxv1alpha1.ReasonMCPServerRelationshipsCompileError, msg)
			return apreconcile.StopAfter()
		}
		return apreconcile.Continue()
	}

	finalSuccess := func(ctx context.Context) apreconcile.Outcome {
		srv.Status.ObservedGeneration = srv.Generation
		conditions.SetTrue(&srv, &srv.Status.Conditions,
			spiceboxv1alpha1.MCPServerConditionValid,
			spiceboxv1alpha1.ReasonMCPServerAllowlistResolved)
		// Hold LastValidatedAt at its prior value, then let StampIfMoved bump it
		// only when this pass observed something new. conditions.Set* is already
		// dedupe-on-equal-state, so the clock is the sole per-pass churn.
		srv.Status.LastValidatedAt = statusBefore.LastValidatedAt
		apreconcile.StampIfMoved(statusBefore, srv.Status, &srv.Status.LastValidatedAt)
		return apreconcile.Continue()
	}

	// siteURLCheck validates the optional SiteURL field before any
	// network work. Cheap, local — catches bad schemes (file://, ftp://,
	// etc.) at admission time rather than at icon-resolve time.
	siteURLCheck := func(ctx context.Context) apreconcile.Outcome {
		if err := validateSiteURL(srv.Spec.SiteURL); err != nil {
			conditions.SetFalse(&srv, &srv.Status.Conditions,
				spiceboxv1alpha1.MCPServerConditionValid,
				spiceboxv1alpha1.ReasonSpecInvalid, err.Error())
			return apreconcile.StopAfter()
		}
		return apreconcile.Continue()
	}

	// CEL compile first (cheap, local — broken specs short-circuit before
	// we waste a probe roundtrip). Cross-ref check next (also local) so a
	// dangling resourceType is caught at the source MCPServer rather than
	// at downstream AgentClass schema-composition time. Then probe (sets
	// Reachable; failure is non-fatal for Valid). Then allowlist drift
	// check (only meaningful when probe succeeded; gated internally).
	// finalSuccess unconditionally stamps Valid=True — Reachable is
	// independent.
	phases := []apreconcile.Phase{
		siteURLCheck,
		compileCEL,
		labelsCompileCheck,
		crossRefCheck,
		authCredentialCheck,
		probeServer,
		pinCheck,
		allowlistDriftCheck,
		finalSuccess,
	}
	if err := apreconcile.RunPhases(ctx, r.Client, &srv, phases); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: r.interval()}, nil
}

// SetupWithManager registers the reconciler. AgentIdentity changes re-enqueue
// all MCPServers in the same namespace, because bindings (mcp:<server-name>)
// now live on the AgentIdentity rather than on the MCPServer spec.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	if err := ctrl.NewControllerManagedBy(mgr).
		For(&spiceboxv1alpha1.MCPServer{}).
		Watches(
			&spiceboxv1alpha1.AgentIdentity{},
			handler.EnqueueRequestsFromMapFunc(r.MapAgentIdentityToMCPServers),
		).
		Complete(r); err != nil {
		return err
	}
	if r.RevokePublisher != nil {
		inf, err := mgr.GetCache().GetInformer(context.Background(), &spiceboxv1alpha1.MCPServer{})
		if err != nil {
			return err
		}
		revokeLog := ctrl.Log.WithName("mcpserver-revoke")
		if _, err := inf.AddEventHandler(toolscache.ResourceEventHandlerFuncs{
			// This is the ONE revocation trigger in the operator with no
			// durable anchor: the sibling publishers record their observation
			// on the object's status and re-derive it on the next reconcile,
			// but here the object is already gone, so nothing can be
			// re-derived and this notification is the only signal there will
			// ever be. EmitDeleteRevoke therefore retries in process, and says
			// out loud what a final failure costs. In a goroutine because the
			// backoff blocks and an informer handler must not.
			DeleteFunc: func(obj any) {
				go EmitDeleteRevoke(context.Background(), r.RevokePublisher, revokeLog, obj, DeleteRevokeBackoff)
			},
		}); err != nil {
			return err
		}
	}
	return nil
}

// spiceDBResourceInfo is the slice of one SpiceDBResource both halves of
// the routeViaSessionGrant invariant need: which of its relations are
// wildcards, and what each of its permissions expands to.
type spiceDBResourceInfo struct {
	wildcardRelations map[string]struct{}
	permExprs         map[string]string
}

// indexSpiceDBResources builds resourceName → spiceDBResourceInfo. A nil
// fragment yields an empty index, so callers need no nil guard.
func indexSpiceDBResources(frag *spiceboxv1alpha1.SpiceDBSchemaFragment) map[string]spiceDBResourceInfo {
	out := map[string]spiceDBResourceInfo{}
	if frag == nil {
		return out
	}
	for _, r := range frag.Resources {
		info := spiceDBResourceInfo{
			wildcardRelations: map[string]struct{}{},
			permExprs:         map[string]string{},
		}
		for _, rel := range r.Relations {
			if rel.Wildcard {
				info.wildcardRelations[rel.Name] = struct{}{}
			}
		}
		for _, p := range r.Permissions {
			info.permExprs[p.Name] = p.Expr
		}
		out[r.Name] = info
	}
	return out
}

// forEachToolCheck invokes fn once per non-nil PermissionCheck declared on
// the server's tools — the base permission and every variant — with a
// label naming the exact spec path so a condition message can be pasted
// back into the YAML that produced it.
func forEachToolCheck(srv *spiceboxv1alpha1.MCPServer, fn func(label string, c *authz.PermissionCheck)) {
	for _, tl := range srv.Spec.Tools {
		if tl.Permission != nil && tl.Permission.Check != nil {
			fn(fmt.Sprintf("tools[%s].permission.check", tl.Name), tl.Permission.Check)
		}
		for j, v := range tl.PermissionVariants {
			if v.Check.Check != nil {
				fn(fmt.Sprintf("tools[%s].permissionVariants[%d].check", tl.Name, j), v.Check.Check)
			}
		}
	}
}

// compileToolCEL compile-checks every tool's writesRelationships CEL through
// relwrites' own environment — the same one that evaluates them at dispatch
// time, so a spec that validates here is guaranteed to compile there. Named
// for both CEL slots it validates, not just the one it started with.
//
// It also compile-checks every tool's observes blocks, through the sibling
// pkg/authz/observe package, which itself compiles through this same
// relwrites environment (see observe.ValidateBlock). writesRelationships and
// observes are unrelated CEL slots — the former emits SpiceDB tuples, the
// latter memory facts — but both run after a successful tool call and both
// want the same "validated here means it will compile at dispatch" guarantee,
// so they share this one admission check rather than each growing its own.
//
// Returns "" when everything compiles, else a message naming each offending
// (tool, block index, field) so an author can fix the spec from the condition
// alone. All blocks are checked rather than short-circuiting: reporting one
// error per apply turns a multi-error spec into a slow guessing game.
func compileToolCEL(srv *spiceboxv1alpha1.MCPServer) string {
	var problems []string
	for _, tl := range srv.Spec.Tools {
		for i, w := range tl.WritesRelationships {
			if err := relwrites.ValidateBlock(relwrites.Block{
				When:    w.When,
				ForEach: w.ForEach,
				Tuple: relwrites.Tuple{
					Resource: w.Tuple.Resource,
					Relation: w.Tuple.Relation,
					Subject:  w.Tuple.Subject,
				},
			}); err != nil {
				problems = append(problems,
					fmt.Sprintf("tool %q writesRelationships[%d]: %v", tl.Name, i, err))
			}
		}
		for i, ob := range tl.Observes {
			if err := observe.ValidateBlock(fromcrd.FromCRD(ob)); err != nil {
				problems = append(problems,
					fmt.Sprintf("tool %q observes[%d]: %v", tl.Name, i, err))
			}
		}
	}
	if len(problems) == 0 {
		return ""
	}
	return strings.Join(problems, "; ")
}

// leadingStringLiteral extracts the value of a double-quoted or
// single-quoted string literal at the start of a CEL expression,
// ignoring anything concatenated after it. Escapes are not interpreted:
// a literal containing one is reported as not statically knowable.
func leadingStringLiteral(expr string) (string, bool) {
	s := strings.TrimSpace(expr)
	if len(s) < 2 {
		return "", false
	}
	quote := s[0]
	if quote != '"' && quote != '\'' {
		return "", false
	}
	end := strings.IndexByte(s[1:], quote)
	if end < 0 {
		return "", false
	}
	lit := s[1 : 1+end]
	if strings.ContainsRune(lit, '\\') {
		return "", false
	}
	return lit, true
}

// validateSiteURL ensures SiteURL, when set, is a parseable http or
// https URL. Empty string is allowed (the field is +optional).
func validateSiteURL(siteURL string) error {
	if siteURL == "" {
		return nil
	}
	u, err := url.Parse(siteURL)
	if err != nil {
		return fmt.Errorf("siteURL %q: %w", siteURL, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("siteURL %q: scheme must be http or https (got %q)", siteURL, u.Scheme)
	}
	if u.Host == "" {
		return fmt.Errorf("siteURL %q: missing host", siteURL)
	}
	return nil
}

// MapAgentIdentityToMCPServers returns reconcile requests for every MCPServer
// in the same namespace. Bindings now live on AgentIdentity (mcp:<name>), so
// any identity change may affect any server in the namespace.
func (r *Reconciler) MapAgentIdentityToMCPServers(ctx context.Context, obj client.Object) []reconcile.Request {
	id, ok := obj.(*spiceboxv1alpha1.AgentIdentity)
	if !ok {
		return nil
	}
	var list spiceboxv1alpha1.MCPServerList
	if err := r.Client.List(ctx, &list, client.InNamespace(id.Namespace)); err != nil {
		log.FromContext(ctx).Info("list MCPServers for AgentIdentity watch failed; dropping re-enqueue (self-heals on next resync)",
			"agentidentity", id.Name, "namespace", id.Namespace, "err", err.Error())
		return nil
	}
	out := make([]reconcile.Request, 0, len(list.Items))
	for i := range list.Items {
		out = append(out, reconcile.Request{
			NamespacedName: client.ObjectKeyFromObject(&list.Items[i]),
		})
	}
	return out
}

// rawZedDefinitionRE matches lines that declare a SpiceDB definition.
// The pattern is anchored at the start-of-line (with leading whitespace
// allowed) so commented-out occurrences inside multi-line bodies don't
// match. Caveat declarations are intentionally not matched.
var rawZedDefinitionRE = regexp.MustCompile(`(?m)^\s*definition\s+([A-Za-z_][A-Za-z0-9_]*)\s*\{`)

// extractRawZedDefinitions returns the names of every `definition <name>`
// block in the raw .zed text. Used by the MCPServer validator's
// cross-reference check to honor RawZed fragments alongside structured
// Resources.
func extractRawZedDefinitions(rawZed string) []string {
	if rawZed == "" {
		return nil
	}
	matches := rawZedDefinitionRE.FindAllStringSubmatch(rawZed, -1)
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		out = append(out, m[1])
	}
	return out
}
