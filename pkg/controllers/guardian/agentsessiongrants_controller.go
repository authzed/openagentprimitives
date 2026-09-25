// Package guardian holds the operator-side controllers that compose
// per-AgentClass authz requirements (AgentSessionGrants CRs) into the
// SpiceDB `agentsession` definition.
package guardian

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	guardianschema "github.com/authzed/openagentprimitives/pkg/authz/guardian/schema"
	"github.com/authzed/openagentprimitives/pkg/authz/spicedb"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/registry"
	"github.com/authzed/openagentprimitives/pkg/controllers/conditions"
	"github.com/authzed/openagentprimitives/pkg/tools/toolspec/toolkit"
	"github.com/authzed/openagentprimitives/toolkits"
)

// +kubebuilder:rbac:groups=agentprimitives.authzed.com,resources=spicedbbootstraps,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=agentprimitives.authzed.com,resources=spicedbbootstraps/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=agentprimitives.authzed.com,resources=spicedbbootstraps/finalizers,verbs=update
// +kubebuilder:rbac:groups=agentprimitives.authzed.com,resources=agentsessiongrants,verbs=get;list;watch
// +kubebuilder:rbac:groups=agentprimitives.authzed.com,resources=agentsessiongrants/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=agentprimitives.authzed.com,resources=mcpservers,verbs=get;list;watch
// +kubebuilder:rbac:groups=agentprimitives.authzed.com,resources=mcpservers/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=agentprimitives.authzed.com,resources=agentclasses,verbs=get;list;watch
// +kubebuilder:rbac:groups=agentprimitives.authzed.com,resources=spiceboxtoolkits,verbs=get;list;watch
// +kubebuilder:rbac:groups=agentprimitives.authzed.com,resources=spiceboxtoolkits/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=agentprimitives.authzed.com,resources=sidecartoolboxes,verbs=get;list;watch
// +kubebuilder:rbac:groups=agentprimitives.authzed.com,resources=sidecartoolboxes/status,verbs=get;update;patch

// Reconciler watches AgentSessionGrants CRs cluster-wide, computes the
// union of GrantPairs, and writes the SpiceDB agentsession schema via
// the injected SchemaIO. Patches each CR's status with SchemaIncluded.
type Reconciler struct {
	Client   client.Client
	SchemaIO guardianschema.SchemaIO
	// Writer is used to TOUCH and DELETE the relationships declared by
	// SpiceDBBootstrap CRs. May be nil — in that case the bootstrap
	// relationship-sync phase is a no-op and all SpiceDBBootstrap CRs
	// surface RelationshipsApplied=False reason=SpiceDBUnavailable.
	Writer spicedb.BootstrapWriter

	// Reader is used to read back what SpiceDB actually holds for the
	// relations this pass's desired set claims, so drift between belief
	// (r.lastDesired) and reality can be observed — see bootstrap_drift.go.
	// May be nil — in that case drift detection is a no-op, logged once per
	// pass rather than silently, and every other phase of Reconcile is
	// unaffected. NewReconciler does not set this; callers assign it
	// directly (mirrors Writer's nil-tolerant contract) — see
	// internal/cmd/operator/main.go and test/e2e/harness.go.
	//
	// Always assign a *spicedb.Client (or any real implementation) directly
	// into this field, never through an intermediate pointer variable that
	// might itself be nil: a typed-nil pointer stored in this interface
	// produces a non-nil interface whose method call panics, and the
	// `r.Reader == nil` guard in detectBootstrapDrift cannot see through it.
	// This is the exact trap AGENTS.md's "Nil interfaces" section documents
	// against this file's Writer field.
	Reader Reader

	// MonitoringPublish publishes the drift-detection report onto the fixed
	// monitoring subject, which channelsd fans out to role=monitoring
	// Channels. Nil when NATS is unconfigured; detectBootstrapDrift's report
	// still reaches a log in that case (see bootstrap_drift.go's
	// publishDrift) — only where the observation is reported changes, never
	// whether it is made. It is a func type, not an interface, so a nil
	// here is a true nil.
	MonitoringPublish channelevents.PublishFunc

	// clock supplies the drift monitoring event's timestamp; nil defaults
	// to time.Now. Unexported because production has exactly one clock:
	// this is a test seam, not a dependency to wire. Mirrors
	// pkg/controllers/useridentity's own clock field. Reachable from
	// guardian_test via SetClockForTest (export_test.go).
	clock func() time.Time

	// lastDesired is the in-process refcount snapshot from the previous
	// reconcile. Empty on controller start; not persisted to etcd.
	lastDesired *DesiredMap

	// BuiltinToolkits supplies the compile-time toolkit set whose SpiceDB
	// schema fragments are composed alongside MCPServer and SpiceDBBootstrap
	// ones. NewReconciler defaults it to toolkits.All.
	//
	// Injected rather than called directly so this controller's behaviour does
	// not depend on the contents of toolkits/*.yaml. Reading the global made
	// every guardian test transitively assert the shipped toolkit set: adding
	// github_repo and git_repo turned "no CRs compose to the empty schema" —
	// which seven tests pin, and which is a real property — into a schema write
	// on a cluster with nothing on it. A test that means "nothing contributes"
	// can now say so.
	//
	// nil is treated as toolkits.All, NOT as "no toolkits": losing these
	// fragments silently puts a cluster back to `object definition not found`
	// on every gated call, so the safe reading is the default.
	BuiltinToolkits func() []toolkit.Toolkit

	// debounce window: collapses multiple rapid REDUNDANT reconciles before
	// each schema write. The controller workqueue already serializes
	// per-key; this debounces the WRITE.
	//
	// It gates only passes whose schema inputs match lastInputSig. A pass
	// carrying new desired state is never deferred — see the gate in
	// Reconcile for why that distinction is load-bearing.
	debounce  time.Duration
	lastWrite time.Time

	// lastInputSig fingerprints the schema-determining inputs (grant pairs +
	// fragments) of the last pass that completed without error. Empty until
	// the first such pass, which is why the debounce can never suppress the
	// first compose of anything.
	lastInputSig string
}

// builtinToolkits resolves the injected set, defaulting to the compile-time
// builtins. Nil-safe so a Reconciler built by struct literal still ships them.
func (r *Reconciler) builtinToolkits() []toolkit.Toolkit {
	if r.BuiltinToolkits == nil {
		return toolkits.All()
	}
	return r.BuiltinToolkits()
}

// NewReconciler constructs a Reconciler with the default 5s debounce.
//
// A nil SchemaIO is tolerated structurally — the reconciler short-
// circuits its Run() path when SchemaIO is nil so a misconfigured
// operator (e.g. SPICEDB_ENDPOINT unset but the controller still
// registered) degrades to a no-op rather than panicking on the first
// ASG event. internal/cmd/operator/main.go guards against ever registering
// the controller with a nil SchemaIO, but this belt-and-suspenders
// matters because the controller-runtime panic recovery is silent.
//
// A nil Writer is also tolerated: the relationship-sync phase becomes
// a no-op and every SpiceDBBootstrap surfaces RelationshipsApplied=
// False/SpiceDBUnavailable. This keeps schema composition working
// even when the operator is misconfigured for relationship writes.
func NewReconciler(c client.Client, io guardianschema.SchemaIO, w spicedb.BootstrapWriter) *Reconciler {
	return &Reconciler{
		Client:          c,
		SchemaIO:        io,
		Writer:          w,
		BuiltinToolkits: toolkits.All,
		debounce:        5 * time.Second,
		lastDesired:     NewDesiredMap(),
	}
}

func (r *Reconciler) SetupWithManager(mgr manager.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&spiceboxv1alpha1.AgentSessionGrants{}).
		// Slice-4: MCPServer.spec.spiceDBSchema fragments are composed
		// into the unified schema written by this reconciler — so a
		// create/update/delete of any MCPServer must re-trigger the
		// composition pass. The mapper enqueues every existing ASG;
		// the reconciler's debounce collapses the resulting burst.
		Watches(
			&spiceboxv1alpha1.MCPServer{},
			handler.EnqueueRequestsFromMapFunc(r.mcpServerToAllASGs),
		).
		// Slice-5: SpiceDBBootstrap.spec.spicedbSchema fragments are
		// composed into the unified schema written by this reconciler
		// alongside MCPServer fragments. A create/update/delete must
		// re-trigger composition; the mapper mirrors mcpServerToAllASGs.
		Watches(
			&spiceboxv1alpha1.SpiceDBBootstrap{},
			handler.EnqueueRequestsFromMapFunc(r.bootstrapToAllASGs),
		).
		// SpiceboxToolkit.spec.spicedbSchema fragments are composed here too,
		// so the same rule applies: a CR toolkit that declares a resource type
		// must re-trigger composition, or its definitions land only when some
		// unrelated MCPServer or Bootstrap event happens to fire. Reading a
		// resource in the compose pass without watching it is how a dependency
		// silently goes stale. (Builtin toolkits need no watch — they are
		// compile-time embedded and change only with the image.)
		Watches(
			&spiceboxv1alpha1.SpiceboxToolkit{},
			handler.EnqueueRequestsFromMapFunc(r.bootstrapToAllASGs),
		).
		// SidecarToolbox.spec.spicedbSchema fragments are composed here too (a
		// declared sidecar tool's per-datum audiences derive from them), so a
		// create/update/delete must re-trigger composition — same rule and mapper
		// as MCPServer/Bootstrap. Reading it in the compose pass without watching
		// it is how the dependency silently goes stale.
		Watches(
			&spiceboxv1alpha1.SidecarToolbox{},
			handler.EnqueueRequestsFromMapFunc(r.bootstrapToAllASGs),
		).
		Complete(r)
}

// mcpServerToAllASGs maps an MCPServer change to a reconcile request
// for every AgentSessionGrants CR in the cluster. The reconciler reads
// everything cluster-wide on each pass; any one enqueue is sufficient
// to trigger recomposition, but we enqueue all to make the trigger
// resilient if a single ASG is mid-delete.
// guardianBootstrapTickerKey is the synthetic request name the mapper
// uses when there are no AgentSessionGrants on the cluster yet. The
// Reconciler ignores the request name (it lists everything cluster-
// wide), but controller-runtime needs at least one request to
// trigger Reconcile. Without this, a fresh deploy never writes the
// MCPServer schema fragments to SpiceDB until the first ASG is
// created — and the first ASG can't be created because the AgentClass
// can't validate against the empty schema. Bootstrap loop.
var guardianBootstrapTickerKey = client.ObjectKey{Namespace: "", Name: "__bootstrap_tick__"}

func (r *Reconciler) mcpServerToAllASGs(ctx context.Context, _ client.Object) []reconcile.Request {
	logger := log.FromContext(ctx).WithName("guardian.asg.mapper")
	var list spiceboxv1alpha1.AgentSessionGrantsList
	if err := r.Client.List(ctx, &list); err != nil {
		// Surface — controller-runtime's mapper contract has no error
		// return, so a silent nil here would defeat the
		// "never silently drop errors" rule. Logging gives operators
		// a grep target if MCPServer changes mysteriously fail to
		// retrigger reconciliation.
		logger.Info("guardian.asg.mapper: list AgentSessionGrants failed", "err", err.Error())
		return nil
	}
	// Fresh-deploy bootstrap: enqueue a synthetic trigger so the
	// reconciler runs even when no ASGs exist yet. The Reconciler
	// reads MCPServer fragments cluster-wide and composes the schema
	// from those alone (pairs = []) — exactly what we need for the
	// MCPServer-driven schema to land before any AgentClass tries to
	// validate against it.
	if len(list.Items) == 0 {
		logger.Info("guardian.asg.mapper: no ASGs yet; firing bootstrap tick")
		return []reconcile.Request{{NamespacedName: guardianBootstrapTickerKey}}
	}
	reqs := make([]reconcile.Request, 0, len(list.Items))
	for i := range list.Items {
		reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&list.Items[i])})
	}
	return reqs
}

// bootstrapToAllASGs mirrors mcpServerToAllASGs — any change to any
// SpiceDBBootstrap re-triggers a global compose pass via either the
// list of existing ASGs or the bootstrap-tick sentinel.
func (r *Reconciler) bootstrapToAllASGs(ctx context.Context, _ client.Object) []reconcile.Request {
	logger := log.FromContext(ctx).WithName("guardian.bootstrap.mapper")
	var list spiceboxv1alpha1.AgentSessionGrantsList
	if err := r.Client.List(ctx, &list); err != nil {
		logger.Info("bootstrapToAllASGs: list AgentSessionGrants failed", "err", err.Error())
		return nil
	}
	if len(list.Items) == 0 {
		logger.Info("bootstrapToAllASGs: no ASGs yet; firing bootstrap tick")
		return []reconcile.Request{{NamespacedName: guardianBootstrapTickerKey}}
	}
	reqs := make([]reconcile.Request, 0, len(list.Items))
	for i := range list.Items {
		reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&list.Items[i])})
	}
	return reqs
}

// inputSignature fingerprints the inputs that determine the schema a pass would
// compose: the grant pairs and the accepted fragments. Channel-kind fragments
// and session links are deliberately excluded — they come from the in-process
// registry and cannot change between passes of a running operator.
//
// Returns an error rather than a sentinel string so the caller can tell "these
// inputs are unchanged" from "I could not tell", and do the work in the latter
// case.
func inputSignature(pairs []guardianschema.GrantPair, fragments []guardianschema.IdentifiedFragment) (string, error) {
	h := sha256.New()
	for _, p := range pairs {
		fmt.Fprintf(h, "pair\x00%s\x00%s\x00", p.ResourceType, p.Permission)
	}
	for _, f := range fragments {
		// Marshal the fragment content only — the Key/Tier wrapper is identity
		// for reporting, not schema content, so it must not perturb the
		// fingerprint the debounce gate compares across passes.
		b, err := json.Marshal(f.Fragment)
		if err != nil {
			return "", fmt.Errorf("marshal fragment for signature: %w", err)
		}
		h.Write([]byte("frag\x00"))
		h.Write(b)
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithName("guardian.asg").WithValues("name", req.NamespacedName.String())
	started := time.Now()
	logger.Info("guardian.asg: reconcile entry")
	defer func() {
		logger.Info("guardian.asg: reconcile exit", "duration", time.Since(started).String())
	}()

	// Defensive: tolerate a nil SchemaIO injected at construction
	// time. Should be unreachable when wired via internal/cmd/operator (which
	// guards on spiceDBClient != nil before constructing the
	// reconciler), but a panic here would be swallowed silently by
	// controller-runtime's recovery — so we surface it explicitly.
	if r.SchemaIO == nil {
		logger.Info("guardian.asg: SchemaIO is nil; skipping schema composition (no-op)")
		return ctrl.Result{}, nil
	}

	// List ALL AgentSessionGrants cluster-wide — composition is global.
	var list spiceboxv1alpha1.AgentSessionGrantsList
	if err := r.Client.List(ctx, &list); err != nil {
		logger.Info("guardian.asg: list failed", "err", err.Error())
		return ctrl.Result{}, fmt.Errorf("list AgentSessionGrants: %w", err)
	}
	logger.Info("guardian.asg: listed AgentSessionGrants", "count", len(list.Items))

	// Compute the union of pairs.
	var pairs []guardianschema.GrantPair
	for i := range list.Items {
		for _, p := range list.Items[i].Spec.Pairs {
			pairs = append(pairs, guardianschema.GrantPair{
				ResourceType: p.ResourceType, Permission: p.Permission,
			})
		}
	}
	pairs = guardianschema.DedupAndSort(pairs)

	// Union of SLOT pairs — the instance axis. Gathered separately because the
	// composer emits them with the tuple pointing the other way (resource →
	// session), which is what makes the resulting Check per-requester.
	var slots []guardianschema.SlotPair
	for i := range list.Items {
		for _, s := range list.Items[i].Spec.Slots {
			slots = append(slots, guardianschema.SlotPair{
				ResourceType: s.ResourceType, Permission: s.Permission,
			})
		}
	}

	// Slice-4: also list MCPServers cluster-wide and collect spiceDBSchema
	// fragments so the composer can emit the unified schema (scaffold +
	// MCPServer-declared resources + grant relations) in one write.
	var msList spiceboxv1alpha1.MCPServerList
	if err := r.Client.List(ctx, &msList); err != nil {
		logger.Info("guardian.asg: list MCPServers failed", "err", err.Error())
		return ctrl.Result{}, fmt.Errorf("list MCPServers: %w", err)
	}
	// Isolate bad MCPServer fragments in TWO stages before anything reaches
	// RunAll, whose compose→WriteSchema is all-or-nothing: one bad fragment fed
	// in fails the WHOLE cluster-wide compose and flips every AgentSessionGrants
	// to SchemaIncluded=False regardless of tenant — the cross-tenant authz DoS
	// this closes.
	//
	//   1. Per-fragment validation rejects anything invalid ON ITS OWN: a RawZed
	//      syntax error, a redeclared reserved scaffold definition, an internal
	//      conflict.
	//   2. N-way incremental isolation handles survivors that each compose alone
	//      but conflict with EACH OTHER, such as two MCPServers declaring one
	//      resource name with different bodies. It greedily accepts a
	//      conflict-free maximal subset in deterministic order and rejects
	//      whichever later fragment collides with an accepted one.
	//
	// Every rejected MCPServer is marked individually through the guardian-owned
	// SpiceDBSchemaValid condition and excluded from RunAll, so the accepted
	// subset is GUARANTEED to compose and a good tenant's schema is never held
	// hostage by a bad or hostile one.
	var candidates []guardianschema.IdentifiedFragment
	var badMCPServers []invalidMCPServerFragment
	mcpByKey := map[string]*spiceboxv1alpha1.MCPServer{}
	for i := range msList.Items {
		ms := &msList.Items[i]
		frag := ms.Spec.SpiceDBSchema
		if frag == nil {
			continue
		}
		key := ms.Namespace + "/" + ms.Name
		mcpByKey[key] = ms
		if err := guardianschema.ValidateFragment(frag); err != nil {
			badMCPServers = append(badMCPServers, invalidMCPServerFragment{
				mcpServer: ms,
				reason:    spiceboxv1alpha1.MCPServerReasonFragmentInvalid,
				err:       err,
			})
			continue
		}
		candidates = append(candidates, guardianschema.IdentifiedFragment{Key: key, Tier: guardianschema.TierTenant, Fragment: frag})
	}

	// SidecarToolbox is agent-authored, so its spicedbSchema gets the SAME
	// two-stage isolation as MCPServer — per-fragment ValidateFragment here, then
	// the N-way conflict partition below — rather than being fed RAW into RunAll,
	// where one malformed or conflicting fragment would fail the whole
	// cluster-wide compose and stall schema convergence for every session.
	// A rejected fragment is excluded, logged, AND marked False on its own CR's
	// guardian-owned SpiceDBSchemaValid condition (patched below).
	var stbList spiceboxv1alpha1.SidecarToolboxList
	if err := r.Client.List(ctx, &stbList); err != nil {
		logger.Info("guardian.asg: list SidecarToolbox failed", "err", err.Error())
		return ctrl.Result{}, fmt.Errorf("list SidecarToolbox: %w", err)
	}
	sidecarByKey := map[string]*spiceboxv1alpha1.SidecarToolbox{}
	var badSidecarToolboxes []invalidSidecarToolboxFragment
	for i := range stbList.Items {
		stb := &stbList.Items[i]
		frag := stb.Spec.SpiceDBSchema
		if frag == nil || (len(frag.Resources) == 0 && frag.RawZed == "") {
			continue
		}
		key := "sidecartoolbox:" + stb.Namespace + "/" + stb.Name
		sidecarByKey[key] = stb
		if err := guardianschema.ValidateFragment(frag); err != nil {
			badSidecarToolboxes = append(badSidecarToolboxes, invalidSidecarToolboxFragment{
				toolbox: stb, reason: spiceboxv1alpha1.SidecarToolboxReasonFragmentInvalid, err: err,
			})
			logger.Info("guardian.asg: excluding invalid SidecarToolbox schema fragment from compose",
				"sidecartoolbox", stb.Namespace+"/"+stb.Name, "err", err.Error())
			continue
		}
		candidates = append(candidates, guardianschema.IdentifiedFragment{Key: key, Tier: guardianschema.TierTenant, Fragment: frag})
	}

	// SpiceboxToolkit CRs are INSTALLED, not compiled in, so they are tenant
	// input and get the same two-stage isolation as MCPServer and
	// SidecarToolbox. The //go:embed'ed built-in toolkits (toolkits/*.yaml)
	// are a different, compile-time set and are contributed separately below
	// via toolkitFrags — they never go through this candidate path.
	var tkList spiceboxv1alpha1.SpiceboxToolkitList
	if err := r.Client.List(ctx, &tkList); err != nil {
		logger.Info("guardian.asg: list SpiceboxToolkit failed", "err", err.Error())
		return ctrl.Result{}, fmt.Errorf("list SpiceboxToolkit: %w", err)
	}
	toolkitByKey := map[string]*spiceboxv1alpha1.SpiceboxToolkit{}
	var badToolkits []invalidSpiceboxToolkitFragment
	for i := range tkList.Items {
		tk := &tkList.Items[i]
		frag := tk.Spec.SpiceDBSchema
		if frag == nil || (len(frag.Resources) == 0 && frag.RawZed == "") {
			continue
		}
		key := "spiceboxtoolkit:" + tk.Name
		toolkitByKey[key] = tk
		if err := guardianschema.ValidateFragment(frag); err != nil {
			badToolkits = append(badToolkits, invalidSpiceboxToolkitFragment{
				toolkit: tk, reason: spiceboxv1alpha1.SpiceboxToolkitReasonFragmentInvalid, err: err,
			})
			logger.Info("guardian.asg: excluding invalid SpiceboxToolkit schema fragment from compose",
				"spiceboxtoolkit", tk.Name, "err", err.Error())
			continue
		}
		candidates = append(candidates, guardianschema.IdentifiedFragment{Key: key, Tier: guardianschema.TierTenant, Fragment: frag})
	}

	// SpiceDBBootstrap is operator-authored, but its spicedbSchema fragment
	// gets the SAME two-stage isolation as MCPServer, SidecarToolbox and
	// SpiceboxToolkit — ValidateFragment here, then folded into the SAME
	// N-way partition below. Previously this list was read AFTER the
	// partition and every fragment was appended to `fragments` raw
	// (unconditionally, regardless of validity); the only cross-CR check was
	// bootstrap_sync.go's validateBootstraps calling ValidateSchemaConflict
	// over the union, which — on a conflict — stamped Valid=False on EVERY
	// contributing bootstrap, including a good operator-authored one sharing
	// the reconcile with a bad or hostile one. Listed here (moved above the
	// partition) so its candidates can be excluded from `fragments` the same
	// way a bad MCPServer fragment is, rather than reaching RunAll raw.
	var sbList spiceboxv1alpha1.SpiceDBBootstrapList
	if err := r.Client.List(ctx, &sbList); err != nil {
		logger.Info("guardian.asg: list SpiceDBBootstrap failed", "err", err.Error())
		return ctrl.Result{}, fmt.Errorf("list SpiceDBBootstrap: %w", err)
	}
	bootstrapByKey := map[string]*spiceboxv1alpha1.SpiceDBBootstrap{}
	var badBootstraps []invalidBootstrapFragment
	for i := range sbList.Items {
		b := &sbList.Items[i]
		frag := b.Spec.SpiceDBSchema
		if frag == nil || (len(frag.Resources) == 0 && frag.RawZed == "") {
			continue
		}
		key := "spicedbbootstrap:" + b.Namespace + "/" + b.Name
		bootstrapByKey[key] = b
		if err := guardianschema.ValidateFragment(frag); err != nil {
			badBootstraps = append(badBootstraps, invalidBootstrapFragment{
				boot: b, reason: spiceboxv1alpha1.ReasonSpicedbSchemaFragmentInvalid, err: err,
			})
			logger.Info("guardian.asg: excluding invalid SpiceDBBootstrap schema fragment from compose",
				"spicedbbootstrap", b.Namespace+"/"+b.Name, "err", err.Error())
			continue
		}
		candidates = append(candidates, guardianschema.IdentifiedFragment{Key: key, Tier: guardianschema.TierOperator, Fragment: frag})
	}

	// Built-in toolkits: a toolkit's permission checks name a resourceType,
	// and until it could carry the definition, nothing declared them —
	// `gh pr view` gates on github_repo#read and a cluster running a
	// gh-tooled agent under toolCalls.mode=enforcing got `object definition
	// "github_repo" not found` on every gated call.
	//
	// These are compile-time embedded (toolkits/*.yaml, //go:embed) and
	// contributed unconditionally, not gated on some agent referencing them.
	// Two reasons: a definition nothing uses is inert, whereas a definition
	// that arrives only once a toolspec does races the first call that needs
	// it; and the gate here would have to be "some SpiceboxToolspec names
	// this toolkit", which is a watch on a resource this controller does not
	// otherwise track. Most toolkits declare nothing at all (cat, echo,
	// docker) and drop out in ToolkitFragments.
	//
	// SpiceboxToolkit CRs are a different, tenant-input set — installed, not
	// compiled in — and are validated + conflict-isolated above with
	// MCPServer and SidecarToolbox; their accepted fragments are already in
	// `fragments` via the accepted set, so only the built-ins are gathered here.
	toolkitFrags := guardianschema.ToolkitFragments(r.builtinToolkits())

	// The compile-time baseline RunAll composes against: the channel-kind
	// fragments plus the built-in toolkit fragments above. Gathered HERE,
	// before the partition, because the partition must trial-compose every
	// candidate against exactly the schema RunAll writes — otherwise a tenant
	// fragment colliding with a channel-kind OR built-in-toolkit definition
	// passes isolation and freezes the whole cluster's write. They are folded
	// in as bare fragments, not IdentifiedFragment: this baseline is never
	// sorted and never itself subject to isolation — it is the trusted floor
	// the partition trial-composes candidates against — so it carries no
	// Key/Tier for anything to read. Same set is reused at RunAll below.
	schemaBaseline := append(gatherChannelKindFragments(), toolkitFrags...)
	sessionLinks := registry.SessionRelationLinks()

	accepted, conflictRejected := guardianschema.PartitionCompatibleFragments(schemaBaseline, candidates)
	// Carries the accepted fragments' identity (Key/Tier) through rather than
	// flattening to bare *SpiceDBSchemaFragment — the composer cannot name the
	// offending contributor in an error if the name never arrives.
	fragments := append([]guardianschema.IdentifiedFragment{}, accepted...)
	for _, rj := range conflictRejected {
		if ms := mcpByKey[rj.Key]; ms != nil {
			badMCPServers = append(badMCPServers, invalidMCPServerFragment{
				mcpServer: ms,
				reason:    spiceboxv1alpha1.MCPServerReasonFragmentConflict,
				err:       fmt.Errorf("fragment conflicts with an already-accepted MCPServer fragment (%s): %w", rj.DisplacedBy, rj.Err),
			})
			continue
		}
		if stb := sidecarByKey[rj.Key]; stb != nil {
			badSidecarToolboxes = append(badSidecarToolboxes, invalidSidecarToolboxFragment{
				toolbox: stb,
				reason:  spiceboxv1alpha1.SidecarToolboxReasonFragmentConflict,
				err:     fmt.Errorf("fragment conflicts with an already-accepted fragment (%s): %w", rj.DisplacedBy, rj.Err),
			})
			logger.Info("guardian.asg: excluding conflicting SidecarToolbox schema fragment from compose",
				"sidecartoolbox", stb.Namespace+"/"+stb.Name, "err", rj.Err.Error(), "displacedBy", rj.DisplacedBy)
			continue
		}
		if tk := toolkitByKey[rj.Key]; tk != nil {
			badToolkits = append(badToolkits, invalidSpiceboxToolkitFragment{
				toolkit: tk,
				reason:  spiceboxv1alpha1.SpiceboxToolkitReasonFragmentConflict,
				err:     fmt.Errorf("fragment conflicts with an already-accepted fragment (%s): %w", rj.DisplacedBy, rj.Err),
			})
			logger.Info("guardian.asg: excluding conflicting SpiceboxToolkit schema fragment from compose",
				"spiceboxtoolkit", tk.Name, "err", rj.Err.Error(), "displacedBy", rj.DisplacedBy)
			continue
		}
		if b := bootstrapByKey[rj.Key]; b != nil {
			badBootstraps = append(badBootstraps, invalidBootstrapFragment{
				boot:   b,
				reason: spiceboxv1alpha1.ReasonSpicedbSchemaConflict,
				err:    fmt.Errorf("fragment conflicts with an already-accepted fragment (%s): %w", rj.DisplacedBy, rj.Err),
			})
			logger.Info("guardian.asg: excluding conflicting SpiceDBBootstrap schema fragment from compose",
				"spicedbbootstrap", b.Namespace+"/"+b.Name, "err", rj.Err.Error(), "displacedBy", rj.DisplacedBy)
		}
	}
	if len(badMCPServers) > 0 {
		logger.Info("guardian.asg: excluding invalid/conflicting MCPServer schema fragments from compose",
			"excluded", len(badMCPServers), "conflictRejected", len(conflictRejected))
	}
	// Validation status lands promptly regardless of the debounce gate
	// below (mirrors validateBootstraps' rationale) — an operator fixing
	// a bad fragment should see SpiceDBSchemaValid clear without waiting
	// on an unrelated schema write to happen to fire.
	r.patchMCPServerSchemaValidity(ctx, msList.Items, badMCPServers)
	r.patchSidecarToolboxSchemaValidity(ctx, stbList.Items, badSidecarToolboxes)
	r.patchSpiceboxToolkitSchemaValidity(ctx, tkList.Items, badToolkits)
	// SpiceDBBootstrap's own SchemaIncluded stamp for badBootstraps happens
	// further down, AFTER validateBootstraps — not here alongside the other
	// three. Both write to a SpiceDBBootstrap's status.conditions via a plain
	// (non-re-Get) MergeFrom diff computed from the SAME pre-reconcile sbList
	// snapshot; a CRD status patch is a JSON Merge Patch, which replaces an
	// array field wholesale rather than merging per-element, so whichever of
	// the two ran second would silently revert the other's just-landed
	// condition. Ordering validateBootstraps first and re-Getting in
	// patchBootstrapFragmentValidity (see bootstrap_fragment_validation.go)
	// avoids the clobber without adding a re-Get to every bootstrap, every
	// pass — only the (typically few) rejected ones pay for it.
	logger.Info("guardian.asg: listed fragment sources",
		"count", len(msList.Items), "accepted", len(fragments), "excluded", len(badMCPServers),
		"sidecarExcluded", len(badSidecarToolboxes), "toolkitExcluded", len(badToolkits),
		"bootstrapExcluded", len(badBootstraps))

	// (SpiceDBBootstrap, SidecarToolbox and SpiceboxToolkit CR fragments are
	// all validated + conflict-isolated above, alongside MCPServer, and
	// already folded into `fragments` via the accepted set.)
	logger.Info("guardian.asg: listed SpiceDBBootstrap",
		"count", len(sbList.Items),
		"fragments", len(fragments))

	// toolkitFrags (the built-in toolkit set) was gathered above, before the
	// partition, and is already part of schemaBaseline rather than appended to
	// `fragments` here — see the comment at its computation.
	logger.Info("guardian.asg: collected toolkit schema fragments",
		"toolkitCRs", len(tkList.Items),
		"toolkitFragments", len(toolkitFrags),
		"fragments", len(fragments))

	// Finalizer reconciliation per SpiceDBBootstrap. Adds the finalizer
	// to non-deleted CRs; handles delete via the diff in the main flow
	// (the CR drops out of newDesired, so its sole-owned Delete tuples
	// are reaped). After reaping, the finalizer is removed.
	for i := range sbList.Items {
		b := &sbList.Items[i]
		if b.DeletionTimestamp != nil {
			continue // handled below
		}
		if !controllerutil.ContainsFinalizer(b, spiceboxv1alpha1.FinalizerSpiceDBBootstrap) {
			cp := b.DeepCopy()
			controllerutil.AddFinalizer(cp, spiceboxv1alpha1.FinalizerSpiceDBBootstrap)
			if err := r.Client.Patch(ctx, cp, client.MergeFrom(b)); err != nil {
				logger.Info("add finalizer failed", "cr", b.Namespace+"/"+b.Name, "err", err.Error())
				return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
			}
		}
	}

	// 1. Validate bootstraps + stamp Valid. Validation runs even when
	//    debounced so Valid lands promptly for new/edited CRs.
	//    ownershipRefusals carries per-CR ownership-refusal messages for CRs
	//    that otherwise passed validation — see patchBootstrapStatus below,
	//    which surfaces them on RelationshipsApplied rather than Valid.
	invalidBoots, ownershipRefusals := r.validateBootstraps(ctx, sbList.Items)

	// 1a. Stamp SchemaIncluded=False on every bootstrap the fragment
	//     isolation pass rejected, promptly and regardless of the debounce
	//     gate below — mirrors validateBootstraps' own rationale just above.
	//     Runs AFTER validateBootstraps (not alongside patchMCPServer/
	//     SidecarToolbox/SpiceboxToolkitSchemaValidity above) and re-Gets
	//     each rejected CR immediately before patching, so it picks up the
	//     Valid condition validateBootstraps just wrote instead of
	//     clobbering it with a merge patch diffed from the stale
	//     pre-reconcile snapshot. patchBootstrapStatus (after RunAll) is
	//     told which CRs this stamped via excludedBootstrapKeys so its own
	//     generic schemaOK-based True/False stamp never overwrites it.
	r.patchBootstrapFragmentValidity(ctx, badBootstraps)
	excludedBootstrapKeys := make(map[string]struct{}, len(badBootstraps))
	for _, b := range badBootstraps {
		excludedBootstrapKeys[b.boot.Namespace+"/"+b.boot.Name] = struct{}{}
	}

	// A bootstrap whose FRAGMENT was rejected still carries Valid=True (its
	// own spec is fine — see validateBootstraps) so invalidBoots alone does
	// not cover it. Without this union, buildNewDesired treated it as a
	// perfectly normal valid CR: it resolved and TOUCHed spec.relationships
	// that reference a relation only its rejected fragment declared —
	// either failing every touch forever, or worse, landing tuples inside
	// the WINNING fragment's same-named definition, since a rejected
	// fragment stopping composition does not stop the relationship writes
	// that depend on it. Union so buildNewDesired treats a fragment-rejected
	// CR exactly like an L2-invalid one: no NEW relationship writes, and its
	// previously-applied tuples carried forward rather than silently
	// deleted (see buildNewDesired's invalidCRs handling in bootstrap_sync.go).
	//
	// Built as a FRESH map rather than `skipRelationships := invalidBoots` —
	// a plain assignment aliases the same underlying map validateBootstraps
	// returned, so unioning excludedBootstrapKeys into it would silently
	// mutate invalidBoots too, making a name meaning "failed spec validation"
	// also carry fragment-rejected keys. Nothing reads invalidBoots after this
	// point today, so the alias was inert — but a future reader relying on
	// invalidBoots meaning only L2-invalid CRs would be silently wrong.
	skipRelationships := make(map[string]struct{}, len(invalidBoots)+len(excludedBootstrapKeys))
	for key := range invalidBoots {
		skipRelationships[key] = struct{}{}
	}
	for key := range excludedBootstrapKeys {
		skipRelationships[key] = struct{}{}
	}

	// 1b. Recover the claims of any CR that is mid-deletion and that this
	//     process never saw alive (i.e. the operator restarted between the
	//     delete and the finalizer removal). Without this its tuples are
	//     invisible to the diff below and are never reaped.
	r.seedPriorClaimsForDeleting(ctx, sbList.Items)

	// 2. Build newDesired from VALID bootstraps only. Invalid CRs — L2-
	//    invalid (invalidBoots) or fragment-rejected (excludedBootstrapKeys),
	//    unioned above as skipRelationships — have their PREVIOUSLY-applied
	//    tuples carried forward (via r.lastDesired) so a transient or
	//    operator-introduced validation/isolation failure does NOT trigger a
	//    silent DELETE of live SpiceDB state.
	newDesired := r.buildNewDesired(sbList.Items, skipRelationships)

	// 2a. Drift detection: read back, at full consistency, what SpiceDB
	//     actually holds for every relation newDesired claims, and log any
	//     mismatch in either direction. Observe-only — see
	//     bootstrap_drift.go for why this can never influence `diff`,
	//     `newDesired`, or this Reconcile's return value. Runs on every
	//     pass, including ones the debounce gate below will shortly turn
	//     into a no-op write.
	//
	//     r.lastDesired is passed as the convergence history (alreadyKnown):
	//     it is this pass's PRE-reconcile belief, not yet overwritten (that
	//     happens near the end of Reconcile, well after this call and after
	//     seedPriorClaimsForDeleting above has guaranteed it non-nil), so a
	//     tuple newDesired claims for the FIRST time (absent from
	//     lastDesired) is exempt from Missing: nothing has TOUCHed it yet, so
	//     reading it back absent is expected, not drift — the ordinary sync
	//     path (ComputeDiff → applyTouches) is about to write it this same
	//     reconcile regardless of what this call finds. Without this
	//     exemption, the first reconcile of any newly-created
	//     SpiceDBBootstrap — or every tuple, on a fresh install — reads back
	//     100% absent and reports a warning for a surface that is converging
	//     normally. A tuple lastDesired already knows about is held to the
	//     full standard: still missing on read-back means something removed
	//     it after this process itself converged it, which IS drift.
	r.detectBootstrapDrift(ctx, newDesired, r.lastDesired)

	// 3. Compute the diff vs. the previous reconcile's desired set. Pure
	//    in-memory; safe to compute before the debounce gate. The
	//    I/O (DELETE, RunAll, TOUCH) happens together after the gate so
	//    a debounced reconcile never leaves SpiceDB in a half-applied
	//    state (deletes without the matching schema write or vice versa).
	diff := ComputeDiff(r.lastDesired, newDesired)

	// Debounce: rate-limit REDUNDANT passes only.
	//
	// A pass whose schema inputs are unchanged would recompose the same text,
	// so deferring it costs nothing. A pass carrying NEW desired state must
	// not be deferred: everything downstream already believes the relation
	// exists the moment the AgentClass reports Valid — the tool is gated, and
	// approving it writes agentsession:<sess>#grant_<perm>_<resType>. Until
	// the relation is in the schema SpiceDB rejects that write with
	// FailedPrecondition, the paused dispatch never resumes, and the caller is
	// told the approval expired with nobody acting on it, while the approver
	// did act. Because lastWrite advances on every completed pass, gating on
	// elapsed time alone put a pair created just after an unrelated compose
	// (the bootstrap tick that fires when no AgentSessionGrants exist yet) a
	// full debounce interval away from being usable.
	//
	// An unfingerprintable input is treated as changed: do the work rather
	// than defer state we cannot prove is already applied.
	sig, sigErr := inputSignature(pairs, fragments)
	if sigErr != nil {
		logger.Info("guardian.asg: could not fingerprint inputs; proceeding without the debounce",
			"err", sigErr.Error())
	}
	inputsUnchanged := sigErr == nil && r.lastInputSig != "" && sig == r.lastInputSig
	if inputsUnchanged && time.Since(r.lastWrite) < r.debounce {
		logger.Info("guardian.asg: debounced",
			"sinceLastWrite", time.Since(r.lastWrite).String(),
			"debounce", r.debounce.String(),
			"pairs", len(pairs),
			"fragments", len(fragments),
			"toDelete", len(diff.ToDelete),
			"toTouch", len(diff.ToTouch))
		return ctrl.Result{RequeueAfter: r.debounce}, nil
	}

	// 4. DELETE removed tuples BEFORE schema write so that a schema
	//    edit which drops a relation + dependent tuples in the same
	//    apply succeeds (matches spec §4.1). failedDelKeys is needed
	//    below to re-claim transient-failure tuples in lastDesired so
	//    the next reconcile re-queues them (spec §4.4: "DELETE error
	//    (transient): … tuple still tracked so next reconcile retries").
	failedDelKeys, deleteFailures := r.applyDeletes(ctx, diff.ToDelete)

	// 5. Compose + WriteSchema.
	//
	// schemaBaseline and sessionLinks were gathered before the partition (the
	// partition must trial-compose against exactly the schema written here)
	// and are reused as-is.
	//
	// The session link types come from registry.SessionRelationLinks — the same
	// call every gate that must admit exactly what this schema admits reads, so
	// the composed relation lines and those gates cannot name different subject
	// types. They are unioned into the agentsession owner/participant/denied
	// subject lists so the core schema never hard-codes a channel type.
	// Layer-3 SpiceDB disallow surfaces are retired: hard-deny is
	// enforced at Layer 2 (the runner's Scope hook over the session_scope memory
	// doc), so the controller no longer computes disallowed types or injects
	// disallow relations into the schema.
	runStart := time.Now()
	res, runErr := guardianschema.RunAll(ctx, r.SchemaIO, fragments, schemaBaseline, pairs, slots, sessionLinks...)
	r.lastWrite = time.Now()
	logger.Info("guardian.asg: schema.RunAll completed",
		"duration", time.Since(runStart).String(),
		"pairs", len(pairs),
		"fragments", len(fragments),
		"schemaBaselineFragments", len(schemaBaseline),
		"changed", res.Changed,
		"skipped", len(res.SkippedPairs),
		"err", errString(runErr))

	// 6. TOUCH desired tuples AFTER schema write so new defs/relations
	//    are live for the writes that reference them. On a failed
	//    schema write we skip TOUCH (the new defs may not yet exist)
	//    and surface RelationshipsApplied=False via patchBootstrapStatus.
	var applied map[string]struct{}
	var touchFailures int
	if runErr == nil {
		applied, touchFailures = r.applyTouches(ctx, diff.ToTouch)
	} else {
		applied = map[string]struct{}{}
		touchFailures = len(diff.ToTouch)
	}

	// 7. Cache the new desired map for the next reconcile, but only on
	//    schema success: if we never wrote the schema, we also never
	//    issued the TOUCHes, so the prior `lastDesired` still reflects
	//    what's actually in SpiceDB. Caching newDesired on a schema-
	//    failure path would cause us to skip those TOUCHes on the
	//    retry (diff.ToTouch would be empty).
	//
	//    Re-claim tuples whose DELETE failed: copy their ORIGINAL
	//    owners from the prior lastDesired into newDesired so the next
	//    ComputeDiff still sees those tuples as "was in lastDesired,
	//    absent from newDesired" → in to_delete (retry). Without this,
	//    a transient gRPC failure would drop the tuple from both maps
	//    and the next reconcile would never re-issue the DELETE.
	if runErr == nil {
		if len(failedDelKeys) > 0 {
			for _, t := range diff.ToDelete {
				if _, failed := failedDelKeys[t.Key()]; !failed {
					continue
				}
				for _, o := range r.lastDesired.OwnersOf(t.Key()) {
					newDesired.Add(t, o)
				}
			}
		}

		// A tuple whose TOUCH did not land THIS pass — refused by another
		// relsource claimant, or any other write error — was never actually
		// written, and must not enter lastDesired as though it had been.
		// lastDesired is "what this process believes it wrote"; recording a
		// refused tuple there is exactly what wedges a later DELETE forever
		// (the delete-time guard refuses it too — see
		// spicedb.DeleteBootstrapRelationshipVia — so deleteFailures never
		// clears and removeFinalizersOnDeleted never runs, dragging every
		// OTHER co-deleted CR's finalizer removal down with it, since
		// deleteFailures is a global count) and reports it as permanent drift
		// on every pass thereafter (bootstrap_drift.go).
		//
		// priorKnown is r.lastDesired as it stood at the START of this pass
		// (read here, before the reassignment below) — the same convergence
		// history detectBootstrapDrift consulted a few steps up. A tuple
		// already in priorKnown was genuinely written on some earlier pass;
		// THIS pass's touch failing (a transient re-touch hiccup — TOUCH
		// re-issues every desired tuple unconditionally, not just new ones,
		// see ComputeDiff) must not un-write it from belief, so it is carried
		// forward. A tuple absent from priorKnown has never once landed —
		// ownership refusal is static (the same relation is refused on
		// every future pass too, so it could never have succeeded earlier),
		// and any other first-time failure is, by definition, not yet
		// written either — so it is dropped.
		priorKnown := r.lastDesired
		newDesired.Retain(func(key string) bool {
			if _, ok := applied[key]; ok {
				return true
			}
			return priorKnown.Has(key)
		})

		r.lastDesired = newDesired
	}

	if runErr != nil {
		// Non-fatal: patch all ASGs with SchemaIncluded=False, patch
		// SpiceDBBootstraps with SchemaIncluded/RelationshipsApplied
		// reflecting the failure, then requeue.
		//
		// Two distinct reasons, not one: errors.Is against
		// guardianschema.ErrComposeFailed tells apart a failure that never
		// reached io.WriteSchema (the composed text itself failed assembly or
		// validation — the fragment set or the composer is where to look) from
		// an actual SpiceDB write failure. Reporting both as SpiceDBWriteFailed
		// would send an operator debugging a compose-time refusal to SpiceDB's
		// own logs, which show nothing, because nothing was sent.
		schemaFailureReason := "SpiceDBWriteFailed"
		if errors.Is(runErr, guardianschema.ErrComposeFailed) {
			schemaFailureReason = "SchemaComposeFailed"
		}
		for i := range list.Items {
			r.patchSchemaIncluded(ctx, &list.Items[i], false, schemaFailureReason, runErr.Error())
		}
		r.patchBootstrapStatus(ctx, sbList.Items, excludedBootstrapKeys, ownershipRefusals, newDesired, applied, deleteFailures, touchFailures, res, runErr)
		// Finalizer removal — runs AFTER deletes so the CR's tuples
		// are reaped before the K8s GC reclaims the object. Safe on
		// the schema-failure path too: deletes ran BEFORE the schema
		// write, so deleteFailures==0 means the tuples are gone
		// regardless of whether the subsequent schema write failed.
		r.removeFinalizersOnDeleted(ctx, sbList.Items, deleteFailures)
		logger.Info("guardian: schema reconcile failed; will retry",
			"err", runErr.Error(), "pairs", len(pairs))
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	// The pass completed: record what it applied, so a later pass over the
	// same inputs is recognized as redundant and debounced. Stamped only on
	// success — a failed pass must stay eligible to retry immediately, not be
	// mistaken for state already in SpiceDB.
	if sigErr == nil {
		r.lastInputSig = sig
	}

	// Build a set of skipped (resourceType, permission) for quick lookups.
	skipped := map[guardianschema.GrantPair]struct{}{}
	for _, p := range res.SkippedPairs {
		skipped[p] = struct{}{}
	}

	for i := range list.Items {
		asg := &list.Items[i]
		// Did any of THIS CR's pairs get skipped?
		var crSkipped []spiceboxv1alpha1.GrantPair
		for _, p := range asg.Spec.Pairs {
			key := guardianschema.GrantPair{ResourceType: p.ResourceType, Permission: p.Permission}
			if _, isSkipped := skipped[key]; isSkipped {
				crSkipped = append(crSkipped, p)
			}
		}
		if len(crSkipped) == 0 {
			r.patchSchemaIncluded(ctx, asg, true, "AllPairsResolved", "")
		} else {
			msg := fmt.Sprintf("%d pair(s) skipped (definition or permission missing in SpiceDB schema)", len(crSkipped))
			r.patchSchemaIncluded(ctx, asg, false, "SomePairsSkipped", msg)
		}
	}

	// 8. Patch status on each SpiceDBBootstrap.
	r.patchBootstrapStatus(ctx, sbList.Items, excludedBootstrapKeys, ownershipRefusals, newDesired, applied, deleteFailures, touchFailures, res, runErr)

	// 9. Finalizer removal — runs AFTER deletes so the CR's tuples
	//    are reaped before the K8s GC reclaims the object. Only
	//    removes when deleteFailures == 0 (per Task 8 plan); otherwise
	//    the deleting CR's finalizer is held and the retry pass
	//    re-issues the deletes.
	r.removeFinalizersOnDeleted(ctx, sbList.Items, deleteFailures)

	return ctrl.Result{}, nil
}

// errString returns err.Error() or the literal string "<nil>". Used in
// structured logs so the value of an error parameter is always
// visible even on success.
func errString(err error) string {
	if err == nil {
		return "<nil>"
	}
	return err.Error()
}

// gatherChannelKindFragments reads the static schema fragment each registered
// channel kind contributes (those implementing the optional SchemaContributor
// interface). These are the highest-trust, always-present baseline the composed
// cluster schema is built on: read once per reconcile, never per-session, and
// unioned with the built-in toolkit fragments into schemaBaseline, which is fed
// to BOTH the isolation partition and the final RunAll so the two decide over
// identical inputs.
func gatherChannelKindFragments() []*spiceboxv1alpha1.SpiceDBSchemaFragment {
	var out []*spiceboxv1alpha1.SpiceDBSchemaFragment
	for _, kind := range registry.All() {
		if sc, ok := kind.(channelkinds.SchemaContributor); ok {
			if f := sc.SpiceDBSchemaFragment(); f != nil {
				out = append(out, f)
			}
		}
	}
	return out
}

func (r *Reconciler) patchSchemaIncluded(ctx context.Context, asg *spiceboxv1alpha1.AgentSessionGrants, included bool, reason, msg string) {
	status := metav1.ConditionTrue
	if !included {
		status = metav1.ConditionFalse
	}
	cp := asg.DeepCopy()
	cp.Status.ObservedPairCount = int32(len(asg.Spec.Pairs))
	if included {
		// Stamp a fresh ObservedSchemaWrittenAt only on a False/absent→True
		// transition (the schema was newly (re)written this pass). At steady
		// state (already True, e.g. unchanged pairs reconciled again)
		// preserve the existing timestamp so a byte-identical status Patch is
		// a no-op. Stamping metav1.Now() on every included pass — regardless
		// of whether anything changed — made every reconcile mutate the
		// object, which the watch picked back up and requeued: a ~5s-forever
		// reconcile storm doing live SpiceDB reads + cluster-wide Lists.
		wasIncluded := conditions.IsTrue(asg.Status.Conditions, spiceboxv1alpha1.AgentSessionGrantsConditionSchemaIncluded)
		if wasIncluded {
			cp.Status.ObservedSchemaWrittenAt = asg.Status.ObservedSchemaWrittenAt
		} else {
			now := metav1.Now()
			cp.Status.ObservedSchemaWrittenAt = &now
		}
	}
	conditions.Set(cp, &cp.Status.Conditions, metav1.Condition{
		Type:    spiceboxv1alpha1.AgentSessionGrantsConditionSchemaIncluded,
		Status:  status,
		Reason:  reason,
		Message: msg,
	})
	if err := r.Client.Status().Patch(ctx, cp, client.MergeFrom(asg)); err != nil {
		log.FromContext(ctx).Info("guardian: patch ASG status failed",
			"name", asg.Namespace+"/"+asg.Name, "err", err.Error())
	}
}
