// Package agentclass reconciles AgentClass CRs. The reconciler validates
// the spec shape, confirms referenced Secret + ConfigMap (if any) exist
// without reading their bytes, and surfaces Valid=True/False with a reason.
//
// Plan 2 adds validation for non-empty toolBundles: class existence, toolspec
// existence, collision detection, and agentIdentity resolution.
package agentclass

import (
	"context"
	stderrors "errors"
	"fmt"
	"os"
	"reflect"
	"regexp"
	"runtime/debug"
	"sort"
	"strings"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	harnessregistry "github.com/authzed/openagentprimitives/pkg/agent/harness/registry"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/meta/capability"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/adoptguard"
	guardianschema "github.com/authzed/openagentprimitives/pkg/authz/guardian/schema"
	"github.com/authzed/openagentprimitives/pkg/authz/spicedb"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	chregistry "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/registry"
	"github.com/authzed/openagentprimitives/pkg/controllers/conditions"
	apreconcile "github.com/authzed/openagentprimitives/pkg/controllers/internal/reconcile"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/authkind"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/passthroughcatalog"
	"github.com/authzed/openagentprimitives/pkg/platform/oap/instance"
	"github.com/authzed/openagentprimitives/pkg/platform/preferences"
	"github.com/authzed/openagentprimitives/pkg/platform/settingswiring"
	"github.com/authzed/openagentprimitives/pkg/tools/adoptkit"
	embeddedtoolkits "github.com/authzed/openagentprimitives/toolkits"
)

// schemaMismatchRetry paces re-validation of a class whose tool checks against
// a permission the live SpiceDB schema does not (yet) declare.
//
// Short enough that a class waiting only on its fragment being composed
// becomes Valid within a few seconds of install, and long enough that a class
// with a genuine typo costs one cheap reconcile per interval rather than a hot
// loop. The condition states the exact missing pair throughout, so a class
// stuck here is diagnosable without reading logs.
const schemaMismatchRetry = 5 * time.Second

// +kubebuilder:rbac:groups=agentprimitives.authzed.com,resources=agentclasses,verbs=get;list;watch
// +kubebuilder:rbac:groups=agentprimitives.authzed.com,resources=agentclasses/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=agentprimitives.authzed.com,resources=agentidentities,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch
// +kubebuilder:rbac:groups=agentprimitives.authzed.com,resources=clusteragentsettings,verbs=get;list;watch
// +kubebuilder:rbac:groups=agentprimitives.authzed.com,resources=agentsettings,verbs=get;list;watch
// +kubebuilder:rbac:groups=agentprimitives.authzed.com,resources=mcpservers,verbs=get;list;watch
// +kubebuilder:rbac:groups=agentprimitives.authzed.com,resources=spiceboxtoolspecs,verbs=get;list;watch
// +kubebuilder:rbac:groups=agentprimitives.authzed.com,resources=spiceboxclasses,verbs=get;list;watch
// +kubebuilder:rbac:groups=agentprimitives.authzed.com,resources=spiceboxtoolkits,verbs=get;list;watch
// +kubebuilder:rbac:groups=agentprimitives.authzed.com,resources=sidecartoolboxes,verbs=get;list;watch
// +kubebuilder:rbac:groups=agentprimitives.authzed.com,resources=workspacesources,verbs=get;list;watch
// +kubebuilder:rbac:groups=agentprimitives.authzed.com,resources=agentuis,verbs=get;list;watch
// +kubebuilder:rbac:groups=agentprimitives.authzed.com,resources=skills,verbs=get;list;watch
// +kubebuilder:rbac:groups=agentprimitives.authzed.com,resources=clusterskills,verbs=get;list;watch

// admind's authorized oap-install endpoint, co-hosted in this operator binary,
// installs a .oap bundle by server-side-applying its CRs, and SSA needs
// `create`+`patch` on every kind a bundle may carry — additive to the read-only
// rules above. The markers live here rather than beside admind because
// controller-gen scans only pkg/apis/... and pkg/controllers/...; admind's
// cluster-health read markers sit in the agentsession controller for the same
// reason.
// +kubebuilder:rbac:groups=agentprimitives.authzed.com,resources=agentclasses;agentidentities;channels;mcpservers;sidecartoolboxes;spiceboxclasses;spiceboxtoolkits;spiceboxtoolspecs,verbs=create;patch
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=patch

// PlatformLinker is the reconciler's narrow view of SpiceDB writes: the single
// tuple linking an AgentClass to the singleton platform object. Satisfied by
// *spicedb.Client.
//
// An implementation MUST report a name that can never be linked — one outside
// SpiceDB's object_id charset — as spicedb.ErrUnrepresentableObjectID. The
// reconciler branches on it to stop retrying and surface the cause on the CR;
// returned as any other error it would requeue forever against a condition no
// retry can change.
type PlatformLinker interface {
	EnsureAgentClassPlatform(ctx context.Context, ns, name string) error
}

type Reconciler struct {
	Client    client.Client
	APIReader client.Reader
	// SpiceDBSchema is optional. When nil, schema validation is skipped
	// (test fixtures / disconnected clusters). When set, the controller
	// validates per-tool permission references against the live SpiceDB
	// schema lazily — only on AgentClass.metadata.Generation bump (or a
	// referenced MCPServer Generation bump, which already enqueues the
	// AgentClass via the existing watch wiring).
	SpiceDBSchema spicedb.SchemaReader

	// PlatformLinker writes the agentclass#platform link that makes
	// agentclass#start_session satisfiable — the ONLY reason a class can appear
	// in the browser's agent picker, since agentclass#starter ships
	// unpopulated. Optional in the same sense SpiceDBSchema is: nil in test
	// fixtures and disconnected clusters, where the consequence is a picker
	// that lists nothing rather than a broken reconcile.
	//
	// Declared as the INTERFACE, not *spicedb.Client, so a caller that never
	// assigns it leaves a genuine nil interface here rather than a typed-nil
	// pointer that passes `!= nil` and panics on first use (AGENTS.md's
	// nil-interface rule; this package's sibling SpiceDBSchema field is where
	// that bug actually shipped).
	PlatformLinker PlatformLinker

	// StarterLinker writes agentclass#starter from spec.authz.session.allowedStarters.
	// Optional in the same sense PlatformLinker is (nil in tests and local dev
	// without SpiceDB): nil logs and skips, and a class with an allowlist then
	// refuses every session, which is the fail-closed reading.
	StarterLinker StarterLinker

	// SecretReader is the guarded Secret reader. When set, the reconciler
	// adopts CR-referenced Secrets via adoptkit.AdoptSecret before reading
	// them through the label-filtered cache. When nil, falls back to the
	// unguarded reader (for tests that do not inject adoptguard).
	SecretReader *adoptguard.SecretReader

	// ConfigMapReader is the guarded ConfigMap reader. When set, the reconciler
	// adopts CR-referenced ConfigMaps via adoptkit.AdoptConfigMap before reading
	// them through the label-filtered cache. When nil, falls back to the
	// unguarded reader (for tests that do not inject adoptguard).
	ConfigMapReader *adoptguard.ConfigMapReader

	// AllowTestProvider gates whether model.provider="test" is
	// accepted by validation. False in production binaries; the
	// e2e harness sets it true. When false, an AgentClass with
	// provider=test gets Valid=False reason=TestProviderNotAllowed
	// and no session can spawn.
	AllowTestProvider bool

	// MaxDelegationDepth bounds how many edges deep spec.subagents rosters may
	// chain (AgentClass A can delegate to B, which delegates to C, ...) before
	// ValidateRoster refuses the class. Rosters are static, so the whole
	// delegation graph is knowable at admission time — this is a config-time
	// bound, not a runtime depth counter (those are the industry-standard
	// approach and demonstrably do not hold).
	MaxDelegationDepth int
}

func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	mapSecret := func(ctx context.Context, o client.Object) []reconcile.Request {
		var list spiceboxv1alpha1.AgentClassList
		if err := r.Client.List(ctx, &list, client.InNamespace(o.GetNamespace())); err != nil {
			log.FromContext(ctx).Info("list AgentClasses for Secret watch failed; dropping re-enqueue (self-heals on next resync)",
				"secret", o.GetName(), "namespace", o.GetNamespace(), "err", err.Error())
			return nil
		}
		var out []reconcile.Request
		for i := range list.Items {
			ac := &list.Items[i]
			if ac.Spec.Model != nil && ac.Spec.Model.APIKey.Name == o.GetName() {
				out = append(out, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(ac)})
			}
		}
		return out
	}
	mapCM := func(ctx context.Context, o client.Object) []reconcile.Request {
		var list spiceboxv1alpha1.AgentClassList
		if err := r.Client.List(ctx, &list, client.InNamespace(o.GetNamespace())); err != nil {
			log.FromContext(ctx).Info("list AgentClasses for ConfigMap watch failed; dropping re-enqueue (self-heals on next resync)",
				"configmap", o.GetName(), "namespace", o.GetNamespace(), "err", err.Error())
			return nil
		}
		var out []reconcile.Request
		for i := range list.Items {
			ac := &list.Items[i]
			if ac.Spec.SystemPrompt.ConfigMapRef != nil && ac.Spec.SystemPrompt.ConfigMapRef.Name == o.GetName() {
				out = append(out, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(ac)})
			}
		}
		return out
	}
	mapChannel := func(_ context.Context, obj client.Object) []reconcile.Request {
		ch, ok := obj.(*spiceboxv1alpha1.Channel)
		if !ok || ch.Spec.AgentClass == "" {
			return nil
		}
		return []reconcile.Request{{
			NamespacedName: types.NamespacedName{
				Namespace: ch.Namespace, Name: ch.Spec.AgentClass,
			},
		}}
	}
	// Re-enqueue any AgentClass that binds the changed AgentIdentity (via
	// spec.agentIdentity or a per-bundle agentIdentity override). Both the
	// binding-coverage validation (validateBundles + validateMCPServers)
	// AND the identity-validity propagation depend on the identity's spec
	// and status; without this watch the AgentClass would stay stale —
	// either at Valid=False reason=AgentIdentityBindingMissing after the
	// user runs setup-identity, or at Valid=False reason=AgentIdentityInvalid
	// after the identity's empty Secret is filled and the identity flips
	// Valid=True. Mirrors the agentidentity controller's Secret→identity
	// map-func: a status-only update on the identity still fires this watch.
	mapIdentityToClasses := func(ctx context.Context, o client.Object) []reconcile.Request {
		var list spiceboxv1alpha1.AgentClassList
		if err := r.Client.List(ctx, &list, client.InNamespace(o.GetNamespace())); err != nil {
			log.FromContext(ctx).Info("list AgentClasses for AgentIdentity watch failed; dropping re-enqueue (self-heals on next resync)",
				"agentidentity", o.GetName(), "namespace", o.GetNamespace(), "err", err.Error())
			return nil
		}
		var out []reconcile.Request
		for i := range list.Items {
			ac := &list.Items[i]
			bound := ac.Spec.AgentIdentity == o.GetName()
			for _, b := range ac.Spec.ToolBundles {
				if b.AgentIdentity == o.GetName() {
					bound = true
					break
				}
			}
			if bound {
				out = append(out, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(ac)})
			}
		}
		return out
	}
	// Re-enqueue any AgentClass whose mcpServers[].ref points at the
	// changed MCPServer. Without this watch, a Valid AgentClass that
	// references an MCPServer can stay stuck at Valid=False
	// reason=AgentClassMCPServerInvalid when the MCPServer flips to
	// Valid=True after the AgentClass last reconciled — the production
	// race we hit at 16:01:41Z when both reconciled in the same
	// instant and the AgentClass observed "no Valid condition yet".
	mapMCPServer := func(ctx context.Context, o client.Object) []reconcile.Request {
		var list spiceboxv1alpha1.AgentClassList
		if err := r.Client.List(ctx, &list, client.InNamespace(o.GetNamespace())); err != nil {
			log.FromContext(ctx).Info("list AgentClasses for MCPServer watch failed; dropping re-enqueue (self-heals on next resync)",
				"mcpserver", o.GetName(), "namespace", o.GetNamespace(), "err", err.Error())
			return nil
		}
		var out []reconcile.Request
		for i := range list.Items {
			ac := &list.Items[i]
			for _, ref := range ac.Spec.MCPServers {
				if ref.Ref == o.GetName() {
					out = append(out, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(ac)})
					break
				}
			}
		}
		return out
	}
	// Re-enqueue any AgentClass whose toolBundles[].toolspecs name the
	// changed SpiceboxToolspec. Without this watch, `oap agent install` —
	// which applies the AgentClass and its toolspecs in the same instant —
	// wedges the class at Valid=False reason=ToolspecMissing ("has no
	// Valid condition yet") forever when the AgentClass reconciles before
	// the toolspec controller stamps Valid. SpiceboxToolspec is
	// cluster-scoped, so list AgentClasses across all namespaces.
	mapToolspecToClasses := func(ctx context.Context, o client.Object) []reconcile.Request {
		var list spiceboxv1alpha1.AgentClassList
		if err := r.Client.List(ctx, &list); err != nil {
			log.FromContext(ctx).Info("list AgentClasses for SpiceboxToolspec watch failed; dropping re-enqueue (self-heals on next resync)",
				"spiceboxtoolspec", o.GetName(), "err", err.Error())
			return nil
		}
		var out []reconcile.Request
		for i := range list.Items {
			ac := &list.Items[i]
		bundles:
			for _, b := range ac.Spec.ToolBundles {
				for _, tsName := range b.Toolspecs {
					if tsName == o.GetName() {
						out = append(out, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(ac)})
						break bundles
					}
				}
			}
		}
		return out
	}
	// Re-enqueue any AgentClass whose toolBundles[].class names the changed
	// SpiceboxClass — same install-ordering wedge as the toolspec watch,
	// parked at Valid=False reason=ClassMissing. Cluster-scoped, so list
	// AgentClasses across all namespaces.
	mapSpiceboxClassToClasses := func(ctx context.Context, o client.Object) []reconcile.Request {
		var list spiceboxv1alpha1.AgentClassList
		if err := r.Client.List(ctx, &list); err != nil {
			log.FromContext(ctx).Info("list AgentClasses for SpiceboxClass watch failed; dropping re-enqueue (self-heals on next resync)",
				"spiceboxclass", o.GetName(), "err", err.Error())
			return nil
		}
		var out []reconcile.Request
		for i := range list.Items {
			ac := &list.Items[i]
			for _, b := range ac.Spec.ToolBundles {
				if b.Class == o.GetName() {
					out = append(out, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(ac)})
					break
				}
			}
		}
		return out
	}
	// Re-enqueue any AgentClass that reaches the changed SpiceboxToolkit
	// through toolBundles[].toolspecs -> SpiceboxToolspec.spec.toolkit.name.
	// The enforcing-mode subcommand walk (validatePermissions ->
	// validateToolkitSubcommands) makes class validity a function of the
	// toolkit's spec, and setInvalid returns a bare ctrl.Result{} with no
	// requeue, so without this watch a class parked at Valid=False
	// reason=ToolPermissionMissing never re-reconciles when the toolkit is
	// repaired. The reference is indirect, so resolve toolkit -> toolspec
	// names first, then match those against each class's bundles. Both
	// SpiceboxToolkit and SpiceboxToolspec are cluster-scoped, so list
	// AgentClasses across all namespaces.
	mapToolkitToClasses := func(ctx context.Context, o client.Object) []reconcile.Request {
		var specs spiceboxv1alpha1.SpiceboxToolspecList
		if err := r.Client.List(ctx, &specs); err != nil {
			log.FromContext(ctx).Info("list SpiceboxToolspecs for SpiceboxToolkit watch failed; dropping re-enqueue (self-heals on next resync)",
				"spiceboxtoolkit", o.GetName(), "err", err.Error())
			return nil
		}
		named := map[string]struct{}{}
		for i := range specs.Items {
			if specs.Items[i].Spec.Toolkit.Name == o.GetName() {
				named[specs.Items[i].Name] = struct{}{}
			}
		}
		if len(named) == 0 {
			return nil
		}
		var list spiceboxv1alpha1.AgentClassList
		if err := r.Client.List(ctx, &list); err != nil {
			log.FromContext(ctx).Info("list AgentClasses for SpiceboxToolkit watch failed; dropping re-enqueue (self-heals on next resync)",
				"spiceboxtoolkit", o.GetName(), "err", err.Error())
			return nil
		}
		var out []reconcile.Request
		for i := range list.Items {
			ac := &list.Items[i]
		toolkitBundles:
			for _, b := range ac.Spec.ToolBundles {
				for _, tsName := range b.Toolspecs {
					if _, hit := named[tsName]; hit {
						out = append(out, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(ac)})
						break toolkitBundles
					}
				}
			}
		}
		return out
	}
	// Re-enqueue any AgentClass whose sidecarToolboxes[].ref names the
	// changed SidecarToolbox — validateSidecarToolboxes reads the toolbox's
	// Valid condition, so a class parked at Valid=False
	// reason=AgentClassSidecarToolboxInvalid must re-reconcile when the
	// toolbox's status lands. Namespaced, so list in its namespace only.
	mapSidecarToolboxToClasses := func(ctx context.Context, o client.Object) []reconcile.Request {
		var list spiceboxv1alpha1.AgentClassList
		if err := r.Client.List(ctx, &list, client.InNamespace(o.GetNamespace())); err != nil {
			log.FromContext(ctx).Info("list AgentClasses for SidecarToolbox watch failed; dropping re-enqueue (self-heals on next resync)",
				"sidecartoolbox", o.GetName(), "namespace", o.GetNamespace(), "err", err.Error())
			return nil
		}
		var out []reconcile.Request
		for i := range list.Items {
			ac := &list.Items[i]
			for _, ref := range ac.Spec.SidecarToolboxes {
				if ref.Ref == o.GetName() {
					out = append(out, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(ac)})
					break
				}
			}
		}
		return out
	}
	// Re-enqueue any AgentClass whose spec.workspaceSource.ref names the
	// changed WorkspaceSource — validateWorkspaceSource reads the source's
	// Valid condition, so a class parked at Valid=False
	// reason=WorkspaceSourceInvalid must re-reconcile when the source's
	// status lands. Namespaced, so list in its namespace only.
	mapWorkspaceSourceToClasses := func(ctx context.Context, o client.Object) []reconcile.Request {
		var list spiceboxv1alpha1.AgentClassList
		if err := r.Client.List(ctx, &list, client.InNamespace(o.GetNamespace())); err != nil {
			log.FromContext(ctx).Info("list AgentClasses for WorkspaceSource watch failed; dropping re-enqueue (self-heals on next resync)",
				"workspacesource", o.GetName(), "namespace", o.GetNamespace(), "err", err.Error())
			return nil
		}
		var out []reconcile.Request
		for i := range list.Items {
			ac := &list.Items[i]
			if ac.Spec.WorkspaceSource != nil && ac.Spec.WorkspaceSource.Ref == o.GetName() {
				out = append(out, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(ac)})
			}
		}
		return out
	}
	// Re-enqueue any AgentClass whose spec.agentUI.ref names the changed
	// AgentUI — validateAgentUI reads the UI's Valid condition, so a class
	// parked at Valid=False reason=AgentUIMissing/AgentUIInvalid must
	// re-reconcile when the UI appears or its status flips True. This closes
	// the loop with the AgentUI controller, which already watches AgentClass
	// (MapAgentClassToAgentUIs) for the opposite direction. Namespaced, so
	// list in its namespace only.
	mapAgentUIToClasses := func(ctx context.Context, o client.Object) []reconcile.Request {
		var list spiceboxv1alpha1.AgentClassList
		if err := r.Client.List(ctx, &list, client.InNamespace(o.GetNamespace())); err != nil {
			log.FromContext(ctx).Info("list AgentClasses for AgentUI watch failed; dropping re-enqueue (self-heals on next resync)",
				"agentui", o.GetName(), "namespace", o.GetNamespace(), "err", err.Error())
			return nil
		}
		var out []reconcile.Request
		for i := range list.Items {
			ac := &list.Items[i]
			if ac.Spec.AgentUI != nil && ac.Spec.AgentUI.Ref == o.GetName() {
				out = append(out, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(ac)})
			}
		}
		return out
	}
	// Re-enqueue any AgentClass that opts into the changed Skill (by canonical
	// name in spec.skills). Without this watch, a class parked at Valid=False
	// reason=AgentClassSkillMissing/Invalid would stay parked after the Skill
	// materializes or flips Valid=True. A namespace Skill is only visible to
	// AgentClasses in its own namespace, so we list there. Mirrors mapMCPServer.
	mapSkillToClasses := func(ctx context.Context, o client.Object) []reconcile.Request {
		sk, ok := o.(*spiceboxv1alpha1.Skill)
		if !ok {
			return nil
		}
		var list spiceboxv1alpha1.AgentClassList
		if err := r.Client.List(ctx, &list, client.InNamespace(o.GetNamespace())); err != nil {
			log.FromContext(ctx).Info("list AgentClasses for Skill watch failed; dropping re-enqueue (self-heals on next resync)",
				"skill", o.GetName(), "namespace", o.GetNamespace(), "err", err.Error())
			return nil
		}
		var out []reconcile.Request
		for i := range list.Items {
			ac := &list.Items[i]
			for _, s := range ac.Spec.Skills {
				if s.Ref == sk.Spec.CanonicalName {
					out = append(out, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(ac)})
					break
				}
			}
		}
		return out
	}
	// Re-enqueue any AgentClass (in ANY namespace) that opts into the changed
	// ClusterSkill's canonical name. A ClusterSkill is visible cluster-wide, so
	// a list across all namespaces is required (vs. mapSkillToClasses' single
	// namespace).
	mapClusterSkillToClasses := func(ctx context.Context, o client.Object) []reconcile.Request {
		csk, ok := o.(*spiceboxv1alpha1.ClusterSkill)
		if !ok {
			return nil
		}
		var list spiceboxv1alpha1.AgentClassList
		if err := r.Client.List(ctx, &list); err != nil {
			log.FromContext(ctx).Info("list AgentClasses for ClusterSkill watch failed; dropping re-enqueue (self-heals on next resync)",
				"clusterskill", o.GetName(), "err", err.Error())
			return nil
		}
		var out []reconcile.Request
		for i := range list.Items {
			ac := &list.Items[i]
			for _, s := range ac.Spec.Skills {
				if s.Ref == csk.Spec.CanonicalName {
					out = append(out, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(ac)})
					break
				}
			}
		}
		return out
	}
	// A ClusterAgentSettings change may affect all AgentClasses (cross-namespace).
	mapClusterSettingsToClasses := func(ctx context.Context, o client.Object) []reconcile.Request {
		var list spiceboxv1alpha1.AgentClassList
		if err := r.Client.List(ctx, &list); err != nil {
			log.FromContext(ctx).Info("list AgentClasses for ClusterAgentSettings watch failed; dropping re-enqueue (self-heals on next resync)",
				"clusteragentsettings", o.GetName(), "err", err.Error())
			return nil
		}
		out := make([]reconcile.Request, 0, len(list.Items))
		for i := range list.Items {
			out = append(out, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&list.Items[i])})
		}
		return out
	}
	// An AgentSettings change affects AgentClasses in its namespace only.
	mapNamespaceSettingsToClasses := func(ctx context.Context, o client.Object) []reconcile.Request {
		var list spiceboxv1alpha1.AgentClassList
		if err := r.Client.List(ctx, &list, client.InNamespace(o.GetNamespace())); err != nil {
			log.FromContext(ctx).Info("list AgentClasses for AgentSettings watch failed; dropping re-enqueue (self-heals on next resync)",
				"agentsettings", o.GetName(), "namespace", o.GetNamespace(), "err", err.Error())
			return nil
		}
		out := make([]reconcile.Request, 0, len(list.Items))
		for i := range list.Items {
			out = append(out, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&list.Items[i])})
		}
		return out
	}
	// Re-enqueue any AgentClass whose spec.subagents names the changed
	// AgentClass. The roster DAG validation above (ValidateRoster) reads
	// every sibling AgentClass on each reconcile and calls setInvalid when a
	// named roster member does not exist yet, but setInvalid returns a bare
	// ctrl.Result{} with no requeue -- so without this watch, applying a
	// parent class before its roster members (an ordinary `kubectl apply -f
	// dir/`) lands the parent at Valid=False/RosterInvalid and leaves it
	// there until the 10-hour resync, even though the missing class shows up
	// moments later and Valid=False gates session start for the whole class,
	// not just delegation. Same-namespace only: roster validation itself
	// only lists siblings in ac.Namespace.
	mapClassToDependentClasses := func(ctx context.Context, o client.Object) []reconcile.Request {
		var list spiceboxv1alpha1.AgentClassList
		if err := r.Client.List(ctx, &list, client.InNamespace(o.GetNamespace())); err != nil {
			log.FromContext(ctx).Info("list AgentClasses for AgentClass roster watch failed; dropping re-enqueue (self-heals on next resync)",
				"agentclass", o.GetName(), "namespace", o.GetNamespace(), "err", err.Error())
			return nil
		}
		var out []reconcile.Request
		for i := range list.Items {
			ac := &list.Items[i]
			for _, name := range ac.Spec.RosterNames() {
				if name == o.GetName() {
					out = append(out, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(ac)})
					break
				}
			}
		}
		return out
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&spiceboxv1alpha1.AgentClass{}).
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(mapSecret)).
		Watches(&corev1.ConfigMap{}, handler.EnqueueRequestsFromMapFunc(mapCM)).
		Watches(&spiceboxv1alpha1.Channel{}, handler.EnqueueRequestsFromMapFunc(mapChannel)).
		Watches(&spiceboxv1alpha1.AgentIdentity{}, handler.EnqueueRequestsFromMapFunc(mapIdentityToClasses)).
		Watches(&spiceboxv1alpha1.MCPServer{}, handler.EnqueueRequestsFromMapFunc(mapMCPServer)).
		Watches(&spiceboxv1alpha1.SpiceboxToolspec{}, handler.EnqueueRequestsFromMapFunc(mapToolspecToClasses)).
		Watches(&spiceboxv1alpha1.SpiceboxClass{}, handler.EnqueueRequestsFromMapFunc(mapSpiceboxClassToClasses)).
		Watches(&spiceboxv1alpha1.SpiceboxToolkit{}, handler.EnqueueRequestsFromMapFunc(mapToolkitToClasses)).
		Watches(&spiceboxv1alpha1.SidecarToolbox{}, handler.EnqueueRequestsFromMapFunc(mapSidecarToolboxToClasses)).
		Watches(&spiceboxv1alpha1.WorkspaceSource{}, handler.EnqueueRequestsFromMapFunc(mapWorkspaceSourceToClasses)).
		Watches(&spiceboxv1alpha1.AgentUI{}, handler.EnqueueRequestsFromMapFunc(mapAgentUIToClasses)).
		Watches(&spiceboxv1alpha1.Skill{}, handler.EnqueueRequestsFromMapFunc(mapSkillToClasses)).
		Watches(&spiceboxv1alpha1.AgentClass{}, handler.EnqueueRequestsFromMapFunc(mapClassToDependentClasses)).
		Watches(&spiceboxv1alpha1.ClusterSkill{}, handler.EnqueueRequestsFromMapFunc(mapClusterSkillToClasses)).
		Watches(&spiceboxv1alpha1.ClusterAgentSettings{}, handler.EnqueueRequestsFromMapFunc(mapClusterSettingsToClasses)).
		Watches(&spiceboxv1alpha1.AgentSettings{}, handler.EnqueueRequestsFromMapFunc(mapNamespaceSettingsToClasses)).
		Complete(r)
}

func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	defer func() {
		if rec := recover(); rec != nil {
			fmt.Fprintf(os.Stderr, "AGENTCLASS PANIC: %v\n%s\n", rec, debug.Stack())
			panic(rec) // re-panic so controller-runtime still counts it
		}
	}()
	var ac spiceboxv1alpha1.AgentClass
	if cont, err := apreconcile.LoadInto(ctx, r.Client, req.NamespacedName, &ac); !cont {
		return ctrl.Result{}, err
	}
	if ac.DeletionTimestamp != nil {
		return ctrl.Result{}, nil
	}

	// The agentclass#platform link, written BEFORE every validation gate below. A
	// class that fails validation is exactly the one an admin may need to start a
	// session of to diagnose it, so the link must not depend on the class being
	// healthy; the condition it stamps rides out on whichever status write this
	// reconcile makes, and every terminating path below writes status.
	//
	// The error is CAPTURED, not returned here: a link failure must not pre-empt
	// validation, or a SpiceDB blip would leave every AgentClass with no Valid
	// condition at all and stall everything waiting on one. It is returned at the
	// end of the happy path, which is what keeps a transient failure retrying
	// with backoff. agentidentity's own link follows the same ordering.
	//
	// Honest limitation: a path that returns early on an invalid class drops the
	// captured error rather than requeuing, so the link retries on the next
	// reconcile instead of a backoff timer. Acceptable because those paths mean a
	// human must edit the class anyway, and any edit re-reconciles.
	linkErr := r.ensurePlatformLink(ctx, &ac)

	// The starter set rides beside the platform link, under the same ordering
	// and the same "return the error at the end of the happy path" rule.
	linkErr = stderrors.Join(linkErr, r.ensureStarterLinks(ctx, &ac))

	// Mirror the oap-source install-provenance annotation into status.oapInstall,
	// so provenance survives an external editor stripping the annotation. Done
	// early so every terminating path below persists it alongside whatever else
	// that path stamps, with no second status write. An absent annotation leaves
	// OapInstall untouched; a malformed one logs and leaves status unchanged,
	// since retrying cannot fix corrupted text.
	//
	// The annotation is SSA-owned by the install and is a pure function of the
	// bundle — ref, digest, version, sourceKind, and NO timestamp — so a
	// byte-identical re-install stays an SSA no-op. The wall-clock installedAt is
	// controller-owned instead: a controller may read the clock, the install path
	// must not, or the applied annotation would drift on every run. It is stamped
	// set-once per (sourceRef, digest), so a fresh install or an upgrade to a new
	// digest records "now" while an idempotent re-install preserves the original
	// time, oapInstallEqual sees no change, and no status write fires.
	if raw, ok := ac.Annotations[instance.AnnotationOapSource]; ok {
		if src, err := instance.ParseOapSource(raw); err != nil {
			log.FromContext(ctx).Info("agentclass: malformed oap-source annotation",
				"agentclass", ac.Name, "err", err)
		} else {
			desired := &spiceboxv1alpha1.OapInstallStatus{
				SourceRef:  src.Ref,
				Digest:     src.Digest,
				Version:    src.Version,
				SourceKind: src.SourceKind,
			}
			if cur := ac.Status.OapInstall; cur != nil &&
				cur.SourceRef == desired.SourceRef && cur.Digest == desired.Digest {
				desired.InstalledAt = cur.InstalledAt // same install → preserve original time
			} else {
				desired.InstalledAt = metav1.Now() // first install or upgrade → stamp now
			}
			if !oapInstallEqual(ac.Status.OapInstall, desired) {
				ac.Status.OapInstall = desired
			}
		}
	}

	// Validate prompt source: exactly one of inline/configMapRef.
	hasInline := ac.Spec.SystemPrompt.Inline != ""
	hasCM := ac.Spec.SystemPrompt.ConfigMapRef != nil
	if hasInline == hasCM {
		return r.setInvalid(ctx, &ac, spiceboxv1alpha1.ReasonSpecInvalid,
			"systemPrompt: exactly one of inline or configMapRef must be set")
	}

	reader := client.Reader(r.Client)
	if r.APIReader != nil {
		reader = r.APIReader
	}

	// Resolve ConfigMap source if used.
	if hasCM {
		key := types.NamespacedName{Namespace: ac.Namespace, Name: ac.Spec.SystemPrompt.ConfigMapRef.Name}
		ownerRef := types.NamespacedName{Namespace: ac.Namespace, Name: ac.Name}
		// Adopt's existence check uses the live reader so a not-yet-adopted
		// ConfigMap (absent from the label-filtered cache) is seen. Prefer the
		// guarded reader's live APIReader when wired; else the local `reader`
		// (APIReader-or-Client) preserves the unguarded test path.
		cmAdoptReader := reader
		if r.ConfigMapReader != nil {
			cmAdoptReader = r.ConfigMapReader.Reader
		}
		if err := adoptkit.AdoptConfigMap(ctx, cmAdoptReader, r.Client, key, ownerRef, "AgentClass"); err != nil {
			if errors.IsNotFound(err) {
				return r.setInvalid(ctx, &ac, spiceboxv1alpha1.ReasonConfigMapMissing,
					fmt.Sprintf("systemPrompt.configMapRef: ConfigMap %q not found", key.Name))
			}
			log.FromContext(ctx).Info("adoptkit.AdoptConfigMap failed, requeuing",
				"agentclass", ac.Name, "configmap", key, "err", err)
			return ctrl.Result{}, err
		}
		var cm *corev1.ConfigMap
		if r.ConfigMapReader != nil {
			var err error
			cm, err = r.ConfigMapReader.Get(ctx, key)
			if err != nil {
				if errors.IsNotFound(err) {
					return r.setInvalid(ctx, &ac, spiceboxv1alpha1.ReasonConfigMapMissing,
						fmt.Sprintf("systemPrompt.configMapRef: ConfigMap %q not found", key.Name))
				}
				return ctrl.Result{}, err
			}
		} else {
			var raw corev1.ConfigMap
			if err := reader.Get(ctx, key, &raw); err != nil {
				if errors.IsNotFound(err) {
					return r.setInvalid(ctx, &ac, spiceboxv1alpha1.ReasonConfigMapMissing,
						fmt.Sprintf("systemPrompt.configMapRef: ConfigMap %q not found", key.Name))
				}
				return ctrl.Result{}, err
			}
			cm = &raw
		}
		if _, ok := cm.Data[ac.Spec.SystemPrompt.ConfigMapRef.Key]; !ok {
			return r.setInvalid(ctx, &ac, spiceboxv1alpha1.ReasonConfigMapKeyMissing,
				fmt.Sprintf("systemPrompt.configMapRef: ConfigMap %q has no key %q",
					key.Name, ac.Spec.SystemPrompt.ConfigMapRef.Key))
		}
	}

	// Validate toolBundles. The resolved toolspecs come back with the verdict
	// because validating a bundle already fetches every one of them: the
	// fact-source walk below needs their `observes` declarations, and a second
	// list of the same objects is a second thing to keep in step.
	var resolvedToolspecs []spiceboxv1alpha1.SpiceboxToolspec
	if len(ac.Spec.ToolBundles) > 0 {
		specs, reason, msg := validateBundles(ctx, reader, ac.Namespace, ac.Spec.ToolBundles, ac.Spec.AgentIdentity, ac.Spec.IdentityMode)
		if reason != "" {
			return r.setInvalid(ctx, &ac, reason, msg)
		}
		resolvedToolspecs = specs
	}

	// Validate spec.config against spec.configSchema, and cross-check that every
	// config.X a bound toolspec constraint reads is declared. Fail-closed: an
	// invalid config (or an undeclared reference) marks the class not-ready
	// rather than surfacing later as an opaque runtime tool-call deny. Run after
	// toolBundles resolve so the cross-check sees the actual bound toolspecs.
	if err := validateConfig(&ac.Spec, configRefsFromToolspecs(resolvedToolspecs)); err != nil {
		return r.setInvalid(ctx, &ac, spiceboxv1alpha1.ReasonConfigInvalid, err.Error())
	}

	// Validate spec.userPreferences for self-consistency (duplicate keys, a
	// default outside its own enum/pattern/type). Fail-closed for the same
	// reason as spec.config above: an invalid schema would otherwise surface
	// later as an opaque failure when a session tries to resolve a user's
	// preferences rather than as a diagnosable not-ready class.
	if err := preferences.ValidateSchema(ac.Spec.UserPreferences); err != nil {
		return r.setInvalid(ctx, &ac, spiceboxv1alpha1.ReasonUserPreferencesInvalid, err.Error())
	}

	// Slot declarations are validated UNCONDITIONALLY, before any of the
	// tool-shaped validation below.
	//
	// They describe the class's own instance axis, so their correctness has
	// nothing to do with whether the class happens to reference an MCPServer or
	// a toolkit. Validating them inside validatePermissions (which only runs
	// when one of those is present) meant a class declaring slots and no tools
	// was never checked at all — including the renamed-boundEntities rejection,
	// whose entire purpose is to refuse to run a class whose instance
	// constraints would otherwise be silently dropped.
	if reason, msg := validateSlotsMigration(&ac); reason != "" {
		return r.setInvalid(ctx, &ac, reason, msg)
	}
	compiledSlots, slotDeclReason, slotDeclMsg := validateSlotDeclarations(&ac)
	if slotDeclReason != "" {
		return r.setInvalid(ctx, &ac, slotDeclReason, slotDeclMsg)
	}

	// Delegation-mode ceiling validation, run UNCONDITIONALLY rather than
	// inside the roster gate below: a spec.subagentModes entry with no
	// spec.subagents at all is precisely the case the check exists to catch,
	// and gating it on a non-empty roster would let that one shape through.
	if reason, msg := validateSubagentModes(&ac); reason != "" {
		return r.setInvalid(ctx, &ac, reason, msg)
	}

	// Roster DAG validation. An opted-in-but-invalid dependency marks the class
	// not-ready rather than failing at delegation time, matching how every other
	// declared dependency behaves here.
	if len(ac.Spec.RosterNames()) > 0 {
		var classes spiceboxv1alpha1.AgentClassList
		if err := r.Client.List(ctx, &classes, client.InNamespace(ac.Namespace)); err != nil {
			return ctrl.Result{}, fmt.Errorf("list agentclasses for roster validation: %w", err)
		}
		rosters := make(map[string][]string, len(classes.Items))
		for i := range classes.Items {
			rosters[classes.Items[i].Name] = classes.Items[i].Spec.RosterNames()
		}
		if err := ValidateRoster(ac.Name, rosters, r.MaxDelegationDepth); err != nil {
			return r.setInvalid(ctx, &ac, spiceboxv1alpha1.ReasonRosterInvalid, err.Error())
		}
	}

	// Validate sidecarToolboxes (Phase 1 of SidecarToolbox), then gather the
	// CRs themselves.
	//
	// ABOVE the tool-shaped validation below rather than after it, because the
	// plan-example walk in validatePermissions judges an authored example
	// against the class's declarable surface, and a SidecarToolbox's tools are
	// part of that surface: the shipped agent-builder's only declarer of
	// `perm:change:workshop_draft` IS its sidecar, so an example naming that
	// handle was refused while the class was perfectly correct. Validating
	// first is what keeps gatherSidecarToolboxes' documented precondition true
	// — every ref exists and is Valid by the time it runs, so its own error
	// path stays exceptional.
	//
	// Side effect of the position, same shape as the settings reorder below: a
	// class whose sidecarToolboxes AND mcpServers are both invalid now reports
	// the sidecar reason, where it previously reported the MCPServer one. Both
	// are Valid=False with a named cause; neither hides the other, since fixing
	// the first surfaces the second on the next reconcile.
	if len(ac.Spec.SidecarToolboxes) > 0 {
		if reason, msg := validateSidecarToolboxes(ctx, reader, ac.Namespace, ac.Spec.SidecarToolboxes); reason != "" {
			return r.setInvalid(ctx, &ac, reason, msg)
		}
	}
	sidecarToolboxes, stbErr := gatherSidecarToolboxes(ctx, reader, ac.Namespace, ac.Spec.SidecarToolboxes)
	if stbErr != nil {
		return ctrl.Result{}, fmt.Errorf("gather sidecarToolboxes: %w", stbErr)
	}

	// Slice-2 toolkit-subcommand walk: only fires in enforcing mode.
	// Resolved regardless of whether MCPServers are present so toolBundle-
	// only AgentClasses get the same strictness.
	toolkits, err := listToolkitsFor(ctx, reader, &ac)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Validate mcpServers (Plan 2+3 of MCP umbrella).
	var resolvedServers []spiceboxv1alpha1.MCPServer
	if len(ac.Spec.MCPServers) > 0 {
		if reason, msg := validateMCPServers(ctx, reader, ac.Namespace, ac.Spec.MCPServers, ac.Spec.AgentIdentity, ac.Spec.IdentityMode); reason != "" {
			return r.setInvalid(ctx, &ac, reason, msg)
		}

		// Structural per-tool permission validation (slice 1 + slice 2).
		servers, err := listMCPServersFor(ctx, reader, &ac)
		if err != nil {
			return ctrl.Result{}, err
		}
		resolvedServers = servers
		if reason, msg := validatePermissions(&ac, servers, toolkits, sidecarToolboxes); reason != "" {
			return r.setInvalid(ctx, &ac, reason, msg)
		}

		// Inter-MCPServer SpiceDB schema conflict detection (slice-4 T19).
		// The guardian schema composer concatenates per-MCPServer
		// fragments into one SpiceDB schema. Identical declarations of a
		// resource across fragments dedupe, but mismatched declarations
		// of the same resource name cannot be merged — surface that as
		// Valid=False on the AgentClass so operators catch it at apply
		// time rather than at guardian-write time downstream. The intra-
		// MCPServer cross-ref case (dangling resourceType) is handled
		// in pkg/controllers/mcpserver/controller.go (T18).
		var fragments []*spiceboxv1alpha1.SpiceDBSchemaFragment
		for i := range servers {
			if servers[i].Spec.SpiceDBSchema != nil {
				fragments = append(fragments, servers[i].Spec.SpiceDBSchema)
			}
		}
		if _, err := guardianschema.EmitSpicedbSchema(fragments); err != nil {
			return r.setInvalid(ctx, &ac,
				spiceboxv1alpha1.ReasonAgentClassSpicedbSchemaConflict, err.Error())
		}

		// Live SpiceDB schema validation (6.4). Lazy: only re-runs when
		// the AgentClass Generation has advanced past the generation for
		// which SchemaValidated=True was last recorded.
		if r.SpiceDBSchema != nil {
			cond := conditions.Find(ac.Status.Conditions, spiceboxv1alpha1.AgentClassConditionSchemaValidated)
			needsValidate := cond == nil ||
				cond.Status != metav1.ConditionTrue ||
				cond.ObservedGeneration != ac.Generation
			if needsValidate {
				reason, msg, err := validateAgainstSchema(ctx, r.SpiceDBSchema, servers)
				if err != nil {
					// Transient SpiceDB/fetch failure — NOT a spec problem.
					// Return the error so controller-runtime retries with
					// backoff. Parking at Valid=False here would strand the
					// class until an unrelated event re-enqueued it, since no
					// watch fires on SpiceDB recovery.
					log.FromContext(ctx).Info("AgentClass schema validation deferred: SpiceDB unreachable, will retry",
						"agentclass", ac.Name, "err", err.Error())
					return ctrl.Result{}, err
				}
				if reason != "" {
					// A mismatch is EITHER a spec property or a fragment that
					// has not been composed yet, and this cannot tell them
					// apart — so it requeues rather than parking.
					//
					// It used to park with "the MCPServer watch recovers it",
					// and that recovery cannot fire for the second case: the
					// guardian composes MCPServer fragments into SpiceDB
					// asynchronously, and it deliberately writes NOTHING to the
					// status of a server whose fragment was always valid ("no
					// condition is added just to say fine"). So a class that
					// reconciled before its fragment landed saw a schema
					// without it, parked, and then waited on a watch that had
					// nothing left to fire — permanently invalid, with a
					// perfectly good spec.
					//
					// One MCPServer usually hid this: that server's OWN status
					// writes (Reachable/PinDrift/Valid) tend to land after the
					// compose and re-trigger the class by accident. A class
					// referencing two servers is where the ordering stopped
					// being lucky.
					//
					// The cost of requeueing a genuinely-broken class is one
					// reconcile per interval, and the condition still says
					// exactly what is wrong the whole time.
					conditions.SetFalse(&ac, &ac.Status.Conditions,
						spiceboxv1alpha1.AgentClassConditionSchemaValidated, reason, msg)
					res, err := r.setInvalid(ctx, &ac, reason, msg)
					if err != nil {
						return res, err
					}
					res.RequeueAfter = schemaMismatchRetry
					return res, nil
				}
				conditions.SetTrue(&ac, &ac.Status.Conditions,
					spiceboxv1alpha1.AgentClassConditionSchemaValidated, "Validated")
			}
		}
	} else if len(toolkits) > 0 {
		// toolBundle-only AgentClass (no MCPServers): still walk the
		// resolved toolkits for slice-2 enforcing-mode validation. We
		// pass an empty MCPServer slice so the slice-1 walk is a no-op.
		if reason, msg := validatePermissions(&ac, nil, toolkits, sidecarToolboxes); reason != "" {
			return r.setInvalid(ctx, &ac, reason, msg)
		}
	}

	// Slot auto-fill cannot reach a sandbox tool: it writes a NAMED argument and
	// a sandbox tool takes argv, so the value is ignored by the tool and by its
	// permission check alike, silently. Refuse the combination here rather than
	// let an opt-in bind nothing. Runs outside the MCPServer branches above
	// because it depends only on the class's own slots and toolBundles.
	sandboxToolNames, err := sandboxToolNamesFor(ctx, reader, &ac)
	if err != nil {
		return ctrl.Result{}, err
	}
	if reason, msg := validateSlotAutofill(&ac, sandboxToolNames); reason != "" {
		return r.setInvalid(ctx, &ac, reason, msg)
	}

	if reason, msg := validateWorkspaceSource(ctx, reader, ac.Namespace, ac.Spec.WorkspaceSource); reason != "" {
		return r.setInvalid(ctx, &ac, reason, msg)
	}

	if reason, msg := validateAgentUI(ctx, reader, ac.Namespace, ac.Spec.AgentUI); reason != "" {
		return r.setInvalid(ctx, &ac, reason, msg)
	}

	// Validate the structural, cluster-access-free skill-staging rules first
	// (cheap, no cluster round-trip): AgentSkill.Name uniqueness and each
	// toolBundle's stageSkills references an existing, sandbox-or-both
	// targeted skill.
	if reason, msg := validateSkillsSpec(&ac.Spec); reason != "" {
		return r.setInvalid(ctx, &ac, reason, msg)
	}

	// Validate opted-in skills: each canonical name in spec.skills must
	// resolve to a materialized Skill/ClusterSkill that is Valid=True, or the
	// class is parked here (fail-closed) rather than the runner silently
	// log-and-skipping it. Watches on Skill/ClusterSkill recover the class
	// once the skill materializes.
	if reason, msg := validateSkills(ctx, reader, ac.Namespace, ac.Spec.Skills); reason != "" {
		return r.setInvalid(ctx, &ac, reason, msg)
	}

	// Surface the disabled-mode signal as a status condition. Operators
	// see the warning via `kubectl describe agentclass`; runner-side
	// session-start also emits a one-time channel notification (see
	// pkg/agent/runner/loop.go DisabledNotify).
	switch effectiveToolAuthMode(&ac) {
	case toolAuthModeDisabled:
		conditions.Set(&ac, &ac.Status.Conditions, metav1.Condition{
			Type:    spiceboxv1alpha1.AgentClassConditionToolAuthDisabled,
			Status:  metav1.ConditionTrue,
			Reason:  spiceboxv1alpha1.ReasonToolAuthBypassed,
			Message: "Per-tool authz checks are disabled for this class; tool calls will not be validated against SpiceDB.",
		})
	default:
		// Explicitly stamp False so a class that flipped FROM disabled
		// TO enforcing/permissive loses the warning.
		conditions.Set(&ac, &ac.Status.Conditions, metav1.Condition{
			Type:   spiceboxv1alpha1.AgentClassConditionToolAuthDisabled,
			Status: metav1.ConditionFalse,
			Reason: "ToolAuthEnforced",
		})
	}

	// Write the owned AgentSessionGrants from the union of resolved-tool
	// Permission.Check pairs. Non-fatal: failures surface via the
	// AgentSessionGrantsWritten condition; the overall AgentClass reconcile
	// still proceeds to Valid=True so transient API-server hiccups don't
	// flap the headline status.
	if err := r.writeAgentSessionGrants(ctx, &ac, resolvedServers); err != nil {
		conditions.SetFalse(&ac, &ac.Status.Conditions,
			spiceboxv1alpha1.AgentClassConditionAgentSessionGrantsWritten,
			spiceboxv1alpha1.ReasonAgentSessionGrantsWriteFailed, err.Error())
	} else {
		conditions.SetTrue(&ac, &ac.Status.Conditions,
			spiceboxv1alpha1.AgentClassConditionAgentSessionGrantsWritten,
			spiceboxv1alpha1.ReasonAgentSessionGrantsWritten)
	}

	// Validate sessionInteractPermission format.
	if sip := ac.Spec.GetAuthz().GetSession().InteractPermission; sip != "" {
		if !sessionInteractPermissionRE.MatchString(sip) {
			return r.setInvalid(ctx, &ac, spiceboxv1alpha1.ReasonSpecInvalid,
				fmt.Sprintf(
					"spec.authz.session.interactPermission %q must match <type>:<id>#<relation> (e.g. \"group:engineering#member\")",
					sip))
		}
	}

	// Validate ownerCeiling: starterOnly and fixed are mutually exclusive;
	// fixed, when set, must be a valid SpiceDB subject ref.
	if ceil := ac.Spec.GetOwnerCeiling(); ceil != nil {
		if ceil.StarterOnly && ceil.Fixed != "" {
			return r.setInvalid(ctx, &ac, spiceboxv1alpha1.ReasonSpecInvalid,
				"spec.authz.ownerCeiling: starterOnly and fixed are mutually exclusive")
		}
		if ceil.Fixed != "" && !subjectRefRE.MatchString(ceil.Fixed) {
			return r.setInvalid(ctx, &ac, spiceboxv1alpha1.ReasonSpecInvalid,
				fmt.Sprintf("spec.authz.ownerCeiling.fixed %q must be a subject ref objType:objId[#relation]", ceil.Fixed))
		}
	}

	// Validate spec.harness names a harness registered in this binary.
	if err := validateHarness(ac.Spec.Harness); err != nil {
		return r.setInvalid(ctx, &ac, spiceboxv1alpha1.ReasonSpecInvalid,
			fmt.Sprintf("spec.harness: %v", err))
	}

	// Recompute BoundChannels: list all Channels in the namespace that target
	// this AgentClass, regardless of their own validity.
	var channels spiceboxv1alpha1.ChannelList
	if err := r.Client.List(ctx, &channels, client.InNamespace(ac.Namespace)); err != nil {
		return ctrl.Result{}, err
	}
	// The same walk collects the bound Channels that can bring a session into
	// existence with no human on it, because that set answers two different
	// questions and must not be derived twice: the fact published on
	// status.userlessInput below, and which field such a binding is missing
	// (userLessChannelMissingAuthz). Walking the live list rather than
	// re-Getting each status.boundChannels entry also means no read can fail
	// here and be skipped without a word.
	//
	// It collects the role=output Channels for the same reason: the interact
	// policy derived below comes from the one this class's work is delivered
	// into, and re-listing to find it would be a second walk free to disagree
	// with this one about which Channels are bound.
	bound := []spiceboxv1alpha1.BoundChannelRef{}
	var userlessInputs []*spiceboxv1alpha1.Channel
	var outputChannels []*spiceboxv1alpha1.Channel
	for i := range channels.Items {
		ch := &channels.Items[i]
		if ch.Spec.AgentClass != ac.Name {
			continue
		}
		bound = append(bound, spiceboxv1alpha1.BoundChannelRef{Name: ch.Name, Kind: ch.Spec.Kind})
		if isUserlessSessionSource(ch) {
			userlessInputs = append(userlessInputs, ch)
		}
		if ch.Spec.Role == spiceboxv1alpha1.ChannelRoleOutput {
			outputChannels = append(outputChannels, ch)
		}
	}
	sort.Slice(outputChannels, func(i, j int) bool { return outputChannels[i].Name < outputChannels[j].Name })
	sort.Slice(bound, func(i, j int) bool { return bound[i].Name < bound[j].Name })
	sort.Slice(userlessInputs, func(i, j int) bool { return userlessInputs[i].Name < userlessInputs[j].Name })
	if !reflect.DeepEqual(ac.Status.BoundChannels, bound) {
		ac.Status.BoundChannels = bound
	}

	// Publish "a session of this class can be born with no human on it" for
	// every consumer that keys off it — the interact-permission derivation and
	// the missing-authz refusal below, the Channel controller's destination
	// rule on this class's role=output Channel, and the runner's service-subject
	// fallback. An OBSERVATION derived from the walk above, never authored.
	//
	// Stamped HERE, before any of the validation that can return, so a consumer
	// that has observed Valid=True on this class has observed a derived value:
	// `false` there means "derived false", not "not yet derived".
	ac.Status.UserlessInput = len(userlessInputs) > 0

	// Publish the interact policy this class needs and did not declare. Stamped
	// immediately after the fact it depends on, and before the refusal that
	// reads it, so a class that CAN be satisfied is never refused on a value
	// this reconcile was about to supply. Empty when nothing was derived — see
	// AgentClassStatus.DerivedSessionInteractPermission.
	ac.Status.DerivedSessionInteractPermission = derivedSessionInteractPermission(
		&ac, outputChannels, log.FromContext(ctx).WithValues("agentclass", ac.Name))

	// Validate the start-gate fields. It sits HERE, below the Channel walk,
	// because one of its rules reads status.userlessInput: a start gate admits
	// only people, so a class bound to an input that carries none can never
	// start a session, and that is a fact about the Channels, not the spec.
	// Below the derivation stamp too, so a class refused for that reason still
	// publishes what this pass derived — an operator fixing the binding does not
	// have to wait a reconcile to see the rest.
	//
	// Side effect of the position, same shape as the settings reorder below: a
	// start-gate refusal now persists BoundChannels/UserlessInput/Derived… in
	// the same write, where it previously returned above them.
	if res, invalid, err := r.validateStartGate(ctx, &ac); invalid {
		return res, err
	}

	// Builder-class rules (spec §1.0, §1.2): a class the cluster tier
	// sanctions as a builder must list its starters, and no class outside a
	// workshop namespace may reference that workshop's labeled toolspecs.
	// Sits here, alongside the start gate it shares its (result, invalid,
	// err) shape with — both are decidable from the class + cluster
	// settings/status already in hand, before the settings resolution below.
	if res, invalid, err := r.validateBuilderClass(ctx, &ac); invalid || err != nil {
		return res, err
	}

	// Resolve the 4-tier settings (class-only) and stamp the snapshot +
	// SettingsAccepted condition. A fatal violation (e.g. a disallowed model)
	// does NOT block Valid here — it is surfaced as SettingsAccepted=False.
	// The AgentSession reconciler independently re-resolves settings per session
	// (ResolveForSession) and refuses on its own fatal violation; this class-level
	// condition is for operator visibility.
	//
	// Resolved BEFORE slot keying below: the admin veto
	// (EffectiveSettings.RequireStandingFor) feeds resolveSlotValueKeying's
	// per-slot Standing.
	//
	// Side effect of the reorder: setInvalid below (slot keying rejection, the
	// userless-channel check) does a full Status().Update, so a class that now
	// fails one of those checks persists EffectiveSettings + SettingsAccepted
	// in the same write where it previously would not have. No consumer reads
	// either field conditioned on Valid, so this is benign-to-better — the
	// class carries its settings snapshot slightly earlier in its lifecycle —
	// not a behavior change to treat as accidental.
	if eff, vs, err := settingswiring.ResolveForClass(ctx, r.Client, &ac); err != nil {
		return ctrl.Result{}, fmt.Errorf("resolve settings: %w", err)
	} else {
		ac.Status.EffectiveSettings = &eff
		settingswiring.StampAccepted(&ac, &ac.Status.Conditions, spiceboxv1alpha1.AgentClassConditionSettingsAccepted, vs)
	}

	// Publish how a value becomes an object id for each declared slot, so every
	// writer of a slot grant mints the id the tool's own Check will compute.
	// Derived from the tools rather than authored — a second copy would drift,
	// and drift here is silent: the grant is written, never matched, no error.
	// toolkitsForKeying, NOT the `toolkits` above: that list is scoped to
	// CR-authored toolkits because it feeds validation, and git/gh ship
	// EMBEDDED. A cluster has no SpiceboxToolkit CRs, so keying off it published
	// a git_repo slot with no chain at all — read downstream as "the raw value
	// IS the object id". See toolkitsForKeying.
	keyingToolkits, tkErr := toolkitsForKeying(ctx, reader, &ac)
	if tkErr != nil {
		return ctrl.Result{}, fmt.Errorf("resolve toolkits for slot keying: %w", tkErr)
	}
	var requireStandingFor []string
	if ac.Status.EffectiveSettings != nil {
		requireStandingFor = ac.Status.EffectiveSettings.RequireStandingFor
	}
	// Publish where each fact a slot precondition reads can come from, and
	// refuse the one shape admission can prove nothing will ever satisfy: a
	// precondition whose every producer is gated by the very slot it holds
	// shut.
	//
	// Runs HERE rather than beside validateSlotDeclarations because it is not
	// an internal-consistency claim about the declaration — it is a claim about
	// the declaration against the class's resolved tools, and those are only
	// resolved by this point. Unconditional for the same reason every
	// derivation below it is: a class reaching this line is checked whether it
	// carries MCPServers, toolBundles, both, or neither, so no shape slips past
	// unvalidated. Hosting it inside validatePermissions, which only runs on
	// two of those four, is the inert-validation shape this plan has already
	// hit twice.
	//
	// FIRST of the slot derivations, and stamped before its own refusal, so the
	// dependency list survives every later refusal too: an operator debugging a
	// class refused for an undeclared standing can still read what its
	// preconditions were waiting on.
	factSources, factReason, factMsg := resolveFactSources(compiledSlots, resolvedServers, resolvedToolspecs, keyingToolkits)
	if !reflect.DeepEqual(ac.Status.FactSources, factSources) {
		ac.Status.FactSources = factSources
	}
	if factReason != "" {
		return r.setInvalid(ctx, &ac, factReason, factMsg)
	}

	// sidecarToolboxes was gathered above, beside its own validation: their
	// spicedbSchema fragments join standing + slot resolution alongside
	// MCPServer/SpiceboxToolkit fragments, and their TOOLS join the declarable
	// surface the plan-example walk up there judges an example against.
	resolvedSlots, slotReason, slotMsg := resolveSlotValueKeying(&ac, resolvedServers, keyingToolkits, sidecarToolboxes, requireStandingFor)
	if slotReason != "" {
		return r.setInvalid(ctx, &ac, slotReason, slotMsg)
	}
	if !reflect.DeepEqual(ac.Status.ResolvedSlots, resolvedSlots) {
		ac.Status.ResolvedSlots = resolvedSlots
	}

	// Publish the human phrase declared for each (resourceType, permission)
	// pair, so an approval card can render "Push commits to the repository"
	// instead of a wire-format handle without re-reading every schema
	// fragment. Same source lists as slot keying above (resolvedServers,
	// keyingToolkits, sidecarToolboxes): all three kinds carry SpiceDBSchema
	// fragments, and this walks a different part of the same ones.
	resolvedTitles := resolvePermissionTitles(resolvedServers, keyingToolkits, sidecarToolboxes)
	if !reflect.DeepEqual(ac.Status.ResolvedPermissionTitles, resolvedTitles) {
		ac.Status.ResolvedPermissionTitles = resolvedTitles
	}

	// Publish the declared icon/label/name presentation for each resource
	// type, so an approval card can render an instance line with a derived
	// label and a real link instead of the wire type name beside a bare URL.
	// Same source lists and same reasoning as resolvedTitles above.
	resolvedDisplays := resolveResourceDisplays(resolvedServers, keyingToolkits, sidecarToolboxes)
	if !reflect.DeepEqual(ac.Status.ResolvedResourceDisplays, resolvedDisplays) {
		ac.Status.ResolvedResourceDisplays = resolvedDisplays
	}

	// Publish, per RESOURCE TYPE, how approval for it is governed. Separate from
	// ResolvedSlots[].Standing because a tool's check can name a type the class
	// never declared as a slot, and the approval router needs an answer for that
	// type too.
	//
	// This one can refuse. A missing display or title degrades to a fallback; a
	// missing standing decides who may approve, and there is no fallback that is
	// safe in both directions — so an undeclared type invalidates the class and
	// says which type and what to add.
	resolvedStandings, standingErr := resolveResourceStandings(resolvedServers, keyingToolkits, sidecarToolboxes, standingVeto(requireStandingFor))
	if standingErr != nil {
		return r.setInvalid(ctx, &ac, spiceboxv1alpha1.ReasonSlotDeclarationInvalid, standingErr.Error())
	}
	if !reflect.DeepEqual(ac.Status.ResolvedResourceStandings, resolvedStandings) {
		ac.Status.ResolvedResourceStandings = resolvedStandings
	}

	// Refuse a class whose slot precondition would raise a waiver card no one
	// could answer. A Refused verdict routes its waiver to the precondition's
	// approvers[] or, unset, the slot type's resolved standing; when neither
	// names a subject the pool is empty by shape, and the appealable gate the
	// author wrote would dead-end as a "no one has standing to approve" crash at
	// first dispatch. Runs HERE, after resolveResourceStandings, because the
	// default pool is those very standings — a spec-only check could not see
	// them. It proves only the STRUCTURAL empty case, never "SpiceDB holds zero
	// subjects today"; see validatePreconditionApprovers.
	if reason, msg := validatePreconditionApprovers(&ac, resolvedStandings); reason != "" {
		return r.setInvalid(ctx, &ac, reason, msg)
	}

	// Non-user-attributable input Channels (e.g. bento) MUST carry both
	// AgentClass.spec.authz.session.interactPermission and Channel.spec.authzSubject
	// or the spawned AgentSession has no SpiceDB subject to attribute work
	// to AND no broad grant for humans to interact with the session.
	if chName, missing := userLessChannelMissingAuthz(&ac, userlessInputs); chName != "" {
		return r.setInvalid(ctx, &ac, spiceboxv1alpha1.ReasonAgentClassUserLessMissingAuthz,
			fmt.Sprintf("input Channel %q has UserAttributable=false but %s is empty", chName, missing))
	}

	// Information-leakage binding validation: ensure each bound channel's
	// kind satisfies the capability level required by the policy.
	r.reconcileInfoLeakageCondition(ctx, &ac)

	// Validate the effective model after settings resolution. The explicit
	// spec.model (when set) is checked for the test-provider guard first
	// (it's about what the author chose). Then we validate the resolved
	// effective model's apiKey Secret.
	//
	// When the effective model name is empty (class omits model + no tier
	// default), this is NOT a validity error — it surfaces via the
	// SettingsAccepted condition from the resolution above. Skip apiKey validation.
	if ac.Spec.Model != nil && ac.Spec.Model.Provider == "test" && !r.AllowTestProvider {
		return r.setInvalid(ctx, &ac, spiceboxv1alpha1.ReasonTestProviderNotAllowed,
			`model.provider="test" is for the e2e test harness only; production deployments must use a real provider`)
	}
	// Also guard inherited test provider from a tier.
	if ac.Spec.Model == nil && ac.Status.EffectiveSettings != nil &&
		ac.Status.EffectiveSettings.Model.Provider == "test" && !r.AllowTestProvider {
		return r.setInvalid(ctx, &ac, spiceboxv1alpha1.ReasonTestProviderNotAllowed,
			`model.provider="test" is for the e2e test harness only; production deployments must use a real provider`)
	}
	// Validate a tenant-supplied inline apiKey Secret only when one is set. A
	// model resolved from the cluster model catalog carries no tenant apiKey
	// (APIKey.Name == ""; its central token lives in status.modelTokenSource and
	// is validated by the settings webhook + the AgentSession reconciler at pod
	// create, fail-closed). A fatally-unresolved model (e.g. one denied by
	// deniedModels) likewise resolves no apiKey. In both cases there is nothing
	// to adopt here — reaching for an empty-named Secret would error the
	// reconcile ("resource name may not be empty").
	if ac.Status.EffectiveSettings != nil &&
		ac.Status.EffectiveSettings.Model.Name != "" &&
		ac.Status.EffectiveSettings.Model.APIKey.Name != "" {
		em := ac.Status.EffectiveSettings.Model
		skKey := types.NamespacedName{Namespace: ac.Namespace, Name: em.APIKey.Name}
		ownerRef := types.NamespacedName{Namespace: ac.Namespace, Name: ac.Name}
		// Adopt's existence check uses the live reader so a not-yet-adopted Secret
		// (absent from the label-filtered cache) is seen. Prefer the guarded
		// reader's live APIReader when wired; else the local `reader`
		// (APIReader-or-Client) preserves the unguarded test path.
		secAdoptReader := reader
		if r.SecretReader != nil {
			secAdoptReader = r.SecretReader.Reader
		}
		if err := adoptkit.AdoptSecret(ctx, secAdoptReader, r.Client, skKey, ownerRef, "AgentClass"); err != nil {
			if errors.IsNotFound(err) {
				return r.setInvalid(ctx, &ac, spiceboxv1alpha1.ReasonSecretMissing,
					fmt.Sprintf("model.apiKey.secretRef: Secret %q not found", em.APIKey.Name))
			}
			log.FromContext(ctx).Info("adoptkit.AdoptSecret failed, requeuing",
				"agentclass", ac.Name, "secret", skKey, "err", err)
			return ctrl.Result{}, err
		}
		var sec *corev1.Secret
		if r.SecretReader != nil {
			var err error
			sec, err = r.SecretReader.Get(ctx, skKey)
			if err != nil {
				if errors.IsNotFound(err) {
					return r.setInvalid(ctx, &ac, spiceboxv1alpha1.ReasonSecretMissing,
						fmt.Sprintf("model.apiKey.secretRef: Secret %q not found", em.APIKey.Name))
				}
				return ctrl.Result{}, err
			}
		} else {
			var raw corev1.Secret
			if err := reader.Get(ctx, skKey, &raw); err != nil {
				if errors.IsNotFound(err) {
					return r.setInvalid(ctx, &ac, spiceboxv1alpha1.ReasonSecretMissing,
						fmt.Sprintf("model.apiKey.secretRef: Secret %q not found", em.APIKey.Name))
				}
				return ctrl.Result{}, err
			}
			sec = &raw
		}
		if _, ok := sec.Data[em.APIKey.Key]; !ok {
			return r.setInvalid(ctx, &ac, spiceboxv1alpha1.ReasonSecretKeyMissing,
				fmt.Sprintf("model.apiKey.secretRef: Secret %q has no key %q", em.APIKey.Name, em.APIKey.Key))
		}
	}

	// spec.capabilities is validated statically but never blocks readiness:
	// an unknown key or malformed config is graceful (dropped at assembly
	// time, see pkg/agent/tool/meta/capability), so CapabilitiesValid is
	// purely diagnostic and must NOT feed into AgentClassConditionValid
	// below.
	if problems, anyUnknown := classifyCapabilityProblems(&ac); len(problems) == 0 {
		conditions.SetTrue(&ac, &ac.Status.Conditions,
			spiceboxv1alpha1.AgentClassConditionCapabilitiesValid, spiceboxv1alpha1.ReasonCapabilitiesValid)
	} else {
		reason := spiceboxv1alpha1.ReasonInvalidCapabilityConfig
		if anyUnknown {
			reason = spiceboxv1alpha1.ReasonUnknownCapability
		}
		conditions.SetFalse(&ac, &ac.Status.Conditions,
			spiceboxv1alpha1.AgentClassConditionCapabilitiesValid, reason, strings.Join(problems, "; "))
		log.FromContext(ctx).Info("agentclass has invalid capabilities (graceful, not blocking readiness)",
			"agentclass", ac.Namespace+"/"+ac.Name, "problems", strings.Join(problems, "; "))
	}

	// A roster with nowhere to send it: spec.subagents names one or more
	// classes to delegate to, but nothing in spec.capabilities grants the
	// subagents capability that offers the delegate/reply_to_subagent tools
	// in the first place (it is DefaultOn() == false — an AgentClass must
	// ask for it). The documented build flow (agent-builder's builder-agent
	// skill) adds a roster entry without necessarily granting the
	// capability in the same step, so a class can reconcile fully Valid=True
	// and still never hand work to anyone on its roster, with no diagnosis
	// anywhere. Purely diagnostic, same posture as CapabilitiesValid: never
	// feeds into AgentClassConditionValid, since an author may deliberately
	// stage the roster before granting the capability.
	if reason, msg := subagentsCapabilityProblem(&ac); reason == "" {
		conditions.SetTrue(&ac, &ac.Status.Conditions,
			spiceboxv1alpha1.AgentClassConditionSubagentsCapabilityGranted, spiceboxv1alpha1.ReasonSubagentsCapabilityGranted)
	} else {
		conditions.SetFalse(&ac, &ac.Status.Conditions,
			spiceboxv1alpha1.AgentClassConditionSubagentsCapabilityGranted, reason, msg)
		log.FromContext(ctx).Info("agentclass declares a roster with no subagents capability grant; delegate is never offered (graceful, not blocking readiness)",
			"agentclass", ac.Namespace+"/"+ac.Name, "subagents", ac.Spec.Subagents)
	}

	// All references resolve.
	ac.Status.ObservedGeneration = ac.Generation
	conditions.SetTrue(&ac, &ac.Status.Conditions,
		spiceboxv1alpha1.AgentClassConditionValid, spiceboxv1alpha1.ReasonAllReferencesResolve)
	if err := r.Client.Status().Update(ctx, &ac); err != nil {
		// A status-write failure outranks the link: it is the thing that
		// actually needs retrying first, and returning it keeps linkErr's own
		// retry alive too, since the next reconcile re-runs both.
		return ctrl.Result{}, err
	}
	// Status has converged; now surface the link failure so it retries with
	// backoff. Without this the picker stays empty for this class and nothing
	// in the reconcile ever says so again.
	return ctrl.Result{}, linkErr
}

// validateHarness reports whether spec.harness names a harness registered in
// this binary. An empty value is valid — it means the oap-native default.
// Resolve is fail-closed, so an unknown name surfaces here rather than at
// pod-create time.
func validateHarness(name string) error {
	if _, err := harnessregistry.Resolve(name); err != nil {
		return err
	}
	return nil
}

// validateCapabilities returns human-readable problems with
// ac.Spec.Capabilities: unknown keys and configs that fail
// capability.ValidateGrant (a malformed common {enabled} envelope or a
// capability-specific ParseConfig rejection). Empty slice means valid. Pure
// — safe to call from Reconcile. Deterministic order (sorted by capability
// name) so repeated reconciles of an unchanged spec produce an identical
// condition message (no flapping Status().Update from map-iteration order).
func validateCapabilities(ac *spiceboxv1alpha1.AgentClass) []string {
	problems, _ := classifyCapabilityProblems(ac)
	return problems
}

// classifyCapabilityProblems is validateCapabilities' detailed sibling: it
// additionally reports whether any problem was an unknown-capability-name
// failure (as opposed to a config-parse failure), so Reconcile can pick the
// CapabilitiesValid Reason without re-parsing or resorting to string
// matching on the problem messages.
func classifyCapabilityProblems(ac *spiceboxv1alpha1.AgentClass) (problems []string, anyUnknown bool) {
	names := make([]string, 0, len(ac.Spec.Capabilities))
	for name := range ac.Spec.Capabilities {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		err := capability.ValidateGrant(name, ac.Spec.Capabilities[name].Raw)
		if err == nil {
			continue
		}
		problems = append(problems, err.Error())
		if capability.IsUnknownCapability(err) {
			anyUnknown = true
		}
	}
	return problems, anyUnknown
}

// derivedSessionInteractPermission returns the subject-set to publish as this
// class's interact policy because the class declared none, or "" when there is
// nothing to derive.
//
// The value is the membership of the single role=output Channel bound to this
// class — for a Slack destination, slack_channel:<its channelId>#member. It is
// the same subject-set spec.owner.ownerless.fromOutputChannel already resolves
// through, taken from the same registry seam (chregistry.OwnerGroupRefForChannel,
// which is pure and asks the kind rather than reading any kind's fields here),
// so the two cannot name different populations for the same install.
//
// Four things must all hold, and each rules out a way this could be wrong:
//
//   - the class declared nothing. An authored policy is an intent; derivation
//     fills an absence and never overrides one.
//   - status.userlessInput: a session of this class can be born with no human
//     on it (isUserlessSessionSource). A class whose inbound names a person
//     needs no interact policy at all, and one derived here would switch the
//     interact gate on for a class that never asked for it — and, since the
//     derived value is written to SpiceDB as a participant tuple on every new
//     session (pipeline.TouchInteractParticipant), that would be a silent
//     widening of interact, not merely a stale status field.
//   - exactly one role=output Channel is bound. Two make "the channel the work
//     lands in" a guess — the same ambiguity outputbind.Resolve refuses at
//     session time — and none means there is no membership to take.
//   - the ref is well-formed. Rejecting a malformed one here keeps this from
//     publishing a value the refusal below would then accept and every
//     downstream writer reject, which is a far worse failure than staying
//     refused. The channel id inside it is operator-supplied, so this is
//     reachable, and it is logged rather than dropped.
func derivedSessionInteractPermission(
	ac *spiceboxv1alpha1.AgentClass, outputChannels []*spiceboxv1alpha1.Channel, logger logr.Logger,
) string {
	if s := ac.Spec.GetAuthz().GetSession(); s.InteractPermission != "" || s.OnlyStartersInteract || !ac.Status.UserlessInput {
		return ""
	}
	if len(outputChannels) != 1 {
		logger.V(1).Info("no interact permission derivable: this class has no single role=output Channel to take a membership from",
			"outputChannels", len(outputChannels))
		return ""
	}
	outCh := outputChannels[0]
	ref := chregistry.OwnerGroupRefForChannel(outCh)
	if ref == "" {
		logger.V(1).Info("no interact permission derivable: the output Channel's kind names no membership",
			"outputChannel", outCh.Name, "kind", outCh.Spec.Kind)
		return ""
	}
	if !sessionInteractPermissionRE.MatchString(ref) {
		logger.Info("interact permission NOT derived: the output Channel's membership ref is malformed",
			"outputChannel", outCh.Name, "kind", outCh.Spec.Kind, "ref", ref)
		return ""
	}
	return ref
}

// isUserlessSessionSource reports whether ch can bring a session into existence
// with no human attached to it. Both halves are required:
//
//   - chregistry.IsUserlessInput — the Channel has an inbound role and its
//     kind cannot attribute that inbound to a person (github, bento, agent).
//   - Kind.SpawnsSessionOnInbound — an inbound on this Channel may create a
//     session that did not previously exist.
//
// It is the predicate behind status.userlessInput, and it is asked HERE, where
// that fact is DERIVED, because every rule keyed off the fact is about a
// session the inbound BROUGHT INTO EXISTENCE — one with no starter to take an
// attribution, an interact grant, or a reply destination from. A kind that
// spawns nothing has no such session: every session on such a Channel is
// pre-created, by whoever bound the Channel to it, carrying its own
// attribution and its own standing. Deriving the fact with that half folded in
// is what makes all of its consumers agree by construction instead of by N
// matching edits, and N is not stable — the branch that first needed this had
// one consumer skipping and two not.
//
// The kind that makes it load-bearing is `agent`: the SubagentRequest
// controller binds a kind=agent Channel (UserAttributable=false,
// SpawnsSessionOnInbound=false) to the CHILD's AgentClass for the life of a
// delegation. Counting it would, for as long as any delegation is live, make
// the child's own class demand an interact permission it never declared,
// publish the membership of an unrelated role=output Channel as its effective
// interact policy (a real SpiceDB participant tuple per session), and flip that
// output Channel to Valid=False for want of an outputDefaults destination —
// all from an object the operator created, on a class that changed nothing.
//
// Excluding such a Channel here leaves nothing unchecked: agent.Kind.
// ValidateSpec requires spec.authzSubject outright, so an agent Channel with no
// counterparty is refused as an invalid Channel (by the Channel reconciler)
// rather than as an invalid class. A kind that opts out of this rule owes its
// own answer the same way.
//
// Fail-closed on a kind the registry does not know: it stays IN the set, so the
// rules stay on. Unreachable today — IsUserlessInput has already answered false
// for a kind it cannot Get — and that is the direction to be unreachable in.
func isUserlessSessionSource(ch *spiceboxv1alpha1.Channel) bool {
	if !chregistry.IsUserlessInput(ch) {
		return false
	}
	kind, ok := chregistry.Get(ch.Spec.Kind)
	return !ok || kind.SpawnsSessionOnInbound()
}

// userLessChannelMissingAuthz checks the AgentClass-level invariant that any
// bound Channel that can bring a session into existence with no human on it
// REQUIRES (a) an effective interact permission — declared or derived — AND
// (b) Channel.spec.authzSubject non-empty.
//
// Both halves are about that session: it needs a subject to attribute work to
// (authzSubject) and a broad grant so a human can reach it afterwards
// (interactPermission), because with no per-user identity on the inbound there
// is no starter to derive either from.
//
// Returns the offending Channel name + the missing-field string when
// the rule fires; returns "" "" when the rule passes.
//
// userless is the set Reconcile already walked for status.userlessInput, so
// this function decides only WHICH field one of them is missing, never WHICH
// Channels are in the set. That split is the point: the predicate has one home
// (isUserlessSessionSource, published on status.userlessInput) that other
// controllers read, and this function keeps the half only the AgentClass can
// answer, since only it sees the class's own authz block.
func userLessChannelMissingAuthz(
	ac *spiceboxv1alpha1.AgentClass, userless []*spiceboxv1alpha1.Channel,
) (channelName, missing string) {
	for _, ch := range userless {
		var lacks []string
		// The EFFECTIVE policy: what the class declared, or what this reconcile
		// derived onto status a few steps earlier. Reading only the declared
		// half would refuse a class whose policy is already in force and whose
		// value the operator can see on the object.
		if ac.EffectiveSessionInteractPermission() == "" {
			lacks = append(lacks, "AgentClass.spec.authz.session.interactPermission")
		}
		if ch.Spec.AuthzSubject == "" {
			lacks = append(lacks, fmt.Sprintf("Channel %q.spec.authzSubject", ch.Name))
		}
		if len(lacks) > 0 {
			return ch.Name, strings.Join(lacks, " and ")
		}
	}
	return "", ""
}

// reconcileInfoLeakageCondition sets the InformationLeakageReady condition on
// the AgentClass based on whether each bound channel's kind satisfies the
// capability level required by spec.authz.informationLeakage. The condition is
// always written (True or False); it does not affect the Valid condition —
// operators see it as an independent signal.
//
// Rules (only when mode=enforcing):
//   - CapabilityUnsupported + onUnsupportedChannel=blockBinding → False/ChannelKindLacksAudienceResolver
//   - CapabilitySingleUser + singleUserBypass=false → False/SingleUserBypassDisabled
//   - All other capability/policy combinations → True
//
// When mode=logging, the condition is always True; a warning log line is
// emitted for any channel that would block under enforcing (per AGENTS.md
// "Never silently drop errors"). When mode=disabled, always True, no log.
func (r *Reconciler) reconcileInfoLeakageCondition(ctx context.Context, ac *spiceboxv1alpha1.AgentClass) {
	logger := log.FromContext(ctx).WithValues("agentclass", ac.Name)
	policy := ac.Spec.GetAuthz().InformationLeakage
	mode := policy.ResolvedMode()

	ready := true
	reason := spiceboxv1alpha1.ReasonInfoLeakageReady
	message := ""

	if mode == "enforcing" || mode == "logging" {
		onUnsup := policy.ResolvedOnUnsupportedChannel()
		singleBypass := policy.ResolvedSingleUserBypass()

		for _, bc := range ac.Status.BoundChannels {
			k, ok := chregistry.Get(bc.Kind)
			cap := channelkinds.CapabilityUnsupported
			if ok {
				if ar, isAR := k.(channelkinds.AudienceResolver); isAR {
					cap = ar.AudienceCapability()
				}
			}

			var blockReason, blockMsg string
			switch {
			case cap == channelkinds.CapabilityUnsupported && onUnsup == "blockBinding":
				blockReason = spiceboxv1alpha1.ReasonChannelKindLacksAudienceResolver
				blockMsg = fmt.Sprintf("channel %q kind %q has no AudienceResolver; required by informationLeakage.mode=enforcing", bc.Name, bc.Kind)
			case cap == channelkinds.CapabilitySingleUser && !singleBypass:
				blockReason = spiceboxv1alpha1.ReasonSingleUserBypassDisabled
				blockMsg = fmt.Sprintf("channel %q is single-user but informationLeakage.singleUserBypass=false", bc.Name)
			}

			if blockReason == "" {
				continue
			}
			if mode == "enforcing" {
				ready = false
				reason = blockReason
				message = blockMsg
				break
			}
			// mode == "logging": warn but don't block.
			logger.Info("informationLeakage capability warning", "channel", bc.Name, "kind", bc.Kind, "issue", blockReason)
		}
	}

	if ready {
		conditions.SetTrue(ac, &ac.Status.Conditions,
			spiceboxv1alpha1.AgentClassConditionInformationLeakageReady, reason)
	} else {
		conditions.SetFalse(ac, &ac.Status.Conditions,
			spiceboxv1alpha1.AgentClassConditionInformationLeakageReady, reason, message)
	}
}

// oapInstallEqual reports whether a and b carry the same oap-install
// provenance, so the mirror step in Reconcile only touches ac.Status.OapInstall
// (and thus only triggers a Status().Update) when the annotation actually
// changed. InstalledAt is compared via time.Time.Equal (same instant), not
// struct equality, so a re-parse of an unchanged annotation never flaps.
func oapInstallEqual(a, b *spiceboxv1alpha1.OapInstallStatus) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.SourceRef == b.SourceRef &&
		a.Digest == b.Digest &&
		a.Version == b.Version &&
		a.SourceKind == b.SourceKind &&
		a.InstalledAt.Time.Equal(b.InstalledAt.Time)
}

func (r *Reconciler) setInvalid(ctx context.Context, ac *spiceboxv1alpha1.AgentClass, reason, msg string) (ctrl.Result, error) {
	return apreconcile.SetInvalid(ctx, ac, &ac.Status.ObservedGeneration, &ac.Status.Conditions,
		spiceboxv1alpha1.AgentClassConditionValid, reason, msg,
		func(ctx context.Context) error { return r.Client.Status().Update(ctx, ac) })
}

func validateMCPServers(
	ctx context.Context,
	reader client.Reader,
	namespace string,
	refs []spiceboxv1alpha1.AgentClassMCPServerRef,
	identityName string,
	identityMode string,
) (string, string) {
	// Look up the identity once for binding-coverage checks. Missing
	// identity is reported by validateBundles when ToolBundles are
	// present; here we just skip coverage for empty/missing identity.
	//
	// Skipped entirely under identityMode=userPassthrough — the user's
	// UserIdentity catalog is per-session, populated as the user links
	// credentials via identityd's portal. The operator's passthrough
	// gate does the equivalent check at session start.
	passthrough := identityMode == spiceboxv1alpha1.IdentityModeUserPassthrough
	var ai *spiceboxv1alpha1.AgentIdentity
	if !passthrough && identityName != "" {
		var fetched spiceboxv1alpha1.AgentIdentity
		if err := reader.Get(ctx, types.NamespacedName{Namespace: namespace, Name: identityName}, &fetched); err == nil {
			ai = &fetched
		}
	}
	// Propagate identity validity: a non-passthrough class whose bound
	// identity is not Valid=True (e.g. empty-Secret credential) must not go
	// Valid, or the AgentSession start gate would pass it through. Runs once
	// per class here (validateMCPServers loads the class-default identity).
	if reason, msg := requireIdentityValid(ai, identityName); reason != "" {
		return reason, msg
	}
	for _, ref := range refs {
		var srv spiceboxv1alpha1.MCPServer
		err := reader.Get(ctx, types.NamespacedName{Namespace: namespace, Name: ref.Ref}, &srv)
		if errors.IsNotFound(err) {
			return spiceboxv1alpha1.ReasonAgentClassMCPServerMissing,
				fmt.Sprintf("mcpServers[%q]: MCPServer/%s not found in namespace %s",
					ref.Name, ref.Ref, namespace)
		}
		if err != nil {
			return spiceboxv1alpha1.ReasonAgentClassMCPServerInvalid,
				fmt.Sprintf("mcpServers[%q]: %v", ref.Name, err)
		}
		cond := meta.FindStatusCondition(srv.Status.Conditions, spiceboxv1alpha1.MCPServerConditionValid)
		if cond == nil || cond.Status != metav1.ConditionTrue {
			msg := "no Valid condition yet"
			if cond != nil {
				msg = fmt.Sprintf("Valid=%s reason=%s", cond.Status, cond.Reason)
			}
			return spiceboxv1alpha1.ReasonAgentClassMCPServerInvalid,
				fmt.Sprintf("mcpServers[%q]: MCPServer/%s is not Valid (%s)",
					ref.Name, ref.Ref, msg)
		}
		// Credential coverage: the bound AgentIdentity must have a
		// credential whose name matches what the MCPServer declares (or
		// the server's metadata.name as fallback) so the runner can
		// resolve the access token at session start. Unauthenticated
		// servers (no auth.provider, no auth.credential) are skipped.
		// Surfaces the missing-credential case in AgentClass.status
		// rather than letting it crash at session start.
		if ai != nil {
			credName := passthroughcatalog.CredentialNameForServer(&srv)
			// Unauthenticated MCP server: no provider and no explicit
			// credential name — no credential needed.
			if srv.Spec.Auth.Provider != "" || srv.Spec.Auth.Credential != "" {
				if !hasCredential(ai, credName) {
					return spiceboxv1alpha1.ReasonAgentIdentityBindingMissing,
						fmt.Sprintf("mcpServers[%q]: AgentIdentity %q has no credential %q — run `oap agent setup-identity <agentclass>` to scaffold it",
							ref.Name, identityName, credName)
				}
			}
		}
	}
	return "", ""
}

func validateSidecarToolboxes(
	ctx context.Context,
	reader client.Reader,
	namespace string,
	refs []spiceboxv1alpha1.AgentClassSidecarToolboxRef,
) (string, string) {
	for _, ref := range refs {
		var tb spiceboxv1alpha1.SidecarToolbox
		err := reader.Get(ctx, types.NamespacedName{Namespace: namespace, Name: ref.Ref}, &tb)
		if errors.IsNotFound(err) {
			return spiceboxv1alpha1.ReasonAgentClassSidecarToolboxMissing,
				fmt.Sprintf("sidecarToolboxes[%q]: SidecarToolbox/%s not found in namespace %s",
					ref.Name, ref.Ref, namespace)
		}
		if err != nil {
			return spiceboxv1alpha1.ReasonAgentClassSidecarToolboxInvalid,
				fmt.Sprintf("sidecarToolboxes[%q]: %v", ref.Name, err)
		}
		cond := meta.FindStatusCondition(tb.Status.Conditions, spiceboxv1alpha1.SidecarToolboxConditionValid)
		if cond == nil {
			return spiceboxv1alpha1.ReasonAgentClassSidecarToolboxInvalid,
				fmt.Sprintf("sidecarToolboxes[%q]: SidecarToolbox/%s has no Valid condition yet",
					ref.Name, ref.Ref)
		}
		if cond.Status != metav1.ConditionTrue {
			return spiceboxv1alpha1.ReasonAgentClassSidecarToolboxInvalid,
				fmt.Sprintf("sidecarToolboxes[%q]: SidecarToolbox/%s is not Valid (%s)",
					ref.Name, ref.Ref, cond.Reason)
		}
	}
	return "", ""
}

// gatherSidecarToolboxes fetches the SidecarToolbox CRs a class references so
// their spicedbSchema fragments join standing + slot resolution the same way
// MCPServer and SpiceboxToolkit fragments do (standingSources), and so their
// TOOLS join the walks that ask what this class can do (ConsequentialHandles,
// declarableSurface).
//
// A Get failure is returned rather than skipped, on every caller: a dropped
// fragment silently loses a resource type's standing and fail-closes its
// per-datum leak approval, and a dropped toolbox silently narrows trifecta leg
// C, which fail-OPENS. Both failures are invisible in the answer itself, so the
// error is the only thing that can carry them.
//
// Reconcile calls it after validateSidecarToolboxes, where every ref is known
// to exist and be Valid and a failure here is therefore exceptional. ClassCanAct
// calls it with no such guarantee — it judges a running session rather than an
// incoming spec — which is exactly why the error path is not optional.
func gatherSidecarToolboxes(
	ctx context.Context,
	reader client.Reader,
	namespace string,
	refs []spiceboxv1alpha1.AgentClassSidecarToolboxRef,
) ([]spiceboxv1alpha1.SidecarToolbox, error) {
	out := make([]spiceboxv1alpha1.SidecarToolbox, 0, len(refs))
	for _, ref := range refs {
		var tb spiceboxv1alpha1.SidecarToolbox
		if err := reader.Get(ctx, types.NamespacedName{Namespace: namespace, Name: ref.Ref}, &tb); err != nil {
			return nil, fmt.Errorf("sidecarToolboxes[%q]: %w", ref.Name, err)
		}
		out = append(out, tb)
	}
	return out, nil
}

func validateWorkspaceSource(ctx context.Context, reader client.Reader, namespace string, ref *spiceboxv1alpha1.AgentClassWorkspaceSourceRef) (string, string) {
	if ref == nil {
		return "", ""
	}
	var ws spiceboxv1alpha1.WorkspaceSource
	err := reader.Get(ctx, types.NamespacedName{Namespace: namespace, Name: ref.Ref}, &ws)
	if errors.IsNotFound(err) {
		return spiceboxv1alpha1.ReasonAgentClassWorkspaceSourceMissing,
			fmt.Sprintf("workspaceSource: WorkspaceSource/%s not found in namespace %s", ref.Ref, namespace)
	}
	if err != nil {
		return spiceboxv1alpha1.ReasonAgentClassWorkspaceSourceInvalid,
			fmt.Sprintf("workspaceSource: %v", err)
	}
	cond := meta.FindStatusCondition(ws.Status.Conditions, spiceboxv1alpha1.WorkspaceSourceConditionValid)
	if cond == nil || cond.Status != metav1.ConditionTrue {
		return spiceboxv1alpha1.ReasonAgentClassWorkspaceSourceInvalid,
			fmt.Sprintf("workspaceSource: WorkspaceSource/%s is not Valid", ref.Ref)
	}
	return "", ""
}

// validateAgentUI mirrors validateWorkspaceSource for spec.agentUI.ref: a class
// opting into an AgentUI must name one that exists in its namespace and is
// itself Valid=True, or the class parks at Valid=False. Opting into an invalid
// dependency marks the agent not-ready — the rule every sibling ref follows.
// Without it, a typo'd or Valid=False ref reports Valid=True and the only
// symptom is an Info log at session start plus an empty Loop.AppTools: a browser
// UI with dead buttons and nothing saying why.
//
// This is also the ONLY place the (AgentUI, AgentClass) PAIR is checked. The
// AgentUI controller sees the grant only from its own side, listing the classes
// referencing it to compute status.eligibleTools; whether this class's ref
// resolves is a property of the pair, and the class is the half that must refuse
// to run.
//
// A Valid=Unknown AgentUI — its controller could not list AgentClasses this
// reconcile — counts as not-Valid here, as validateWorkspaceSource treats a
// missing condition. Unknown means the ceiling is unknown, and admitting a class
// whose browser-tool grant nobody has computed is the fail-open answer. The
// mapAgentUIToClasses watch recovers the class once the AgentUI's status settles.
func validateAgentUI(ctx context.Context, reader client.Reader, namespace string, grant *spiceboxv1alpha1.AgentClassUIGrant) (string, string) {
	if grant == nil {
		return "", ""
	}
	var aui spiceboxv1alpha1.AgentUI
	err := reader.Get(ctx, types.NamespacedName{Namespace: namespace, Name: grant.Ref}, &aui)
	if errors.IsNotFound(err) {
		return spiceboxv1alpha1.ReasonAgentClassAgentUIMissing,
			fmt.Sprintf("agentUI: AgentUI/%s not found in namespace %s", grant.Ref, namespace)
	}
	if err != nil {
		return spiceboxv1alpha1.ReasonAgentClassAgentUIInvalid,
			fmt.Sprintf("agentUI: %v", err)
	}
	cond := meta.FindStatusCondition(aui.Status.Conditions, spiceboxv1alpha1.AgentUIConditionValid)
	if cond == nil || cond.Status != metav1.ConditionTrue {
		return spiceboxv1alpha1.ReasonAgentClassAgentUIInvalid,
			fmt.Sprintf("agentUI: AgentUI/%s is not Valid", grant.Ref)
	}
	return "", ""
}

var bundleNameRE = regexp.MustCompile(`^[a-z0-9_-]{1,32}$`)

// sessionInteractPermissionRE validates AgentClass.spec.authz.session.interactPermission.
// Must match "<type>:<id>#<relation>", e.g. "group:engineering#member".
var sessionInteractPermissionRE = regexp.MustCompile(`^[a-z][a-z0-9_]*:[a-zA-Z0-9_/-]+#[a-z][a-z0-9_]*$`)

// subjectRefRE validates a SpiceDB subject reference for ownerCeiling.fixed.
// Must match "objType:objId" or "objType:objId#relation", where objType starts
// with a lowercase letter; objId may contain letters, digits, dots, @, /, or -.
var subjectRefRE = regexp.MustCompile(`^[a-z][a-z0-9_]*:[A-Za-z0-9._@/-]+(#[a-z][a-z0-9_]*)?$`)

// validateBundles checks every toolBundle the class declares and returns the
// SpiceboxToolspecs it resolved on the way.
//
// The toolspecs come back because this walk already fetches every one of them:
// the fact-source walk needs their `observes` declarations, and listing the same
// CRs a second time is a second place for the two views to disagree about which
// toolspecs a class has. Meaningful only on the SUCCESS return — a rejected
// class stopped mid-walk and what it had so far is partial by construction,
// which is why every failure return hands back nil rather than a prefix.
func validateBundles(
	ctx context.Context,
	reader client.Reader,
	namespace string,
	bundles []spiceboxv1alpha1.ToolBundle,
	defaultIdentity string,
	identityMode string,
) (resolved []spiceboxv1alpha1.SpiceboxToolspec, reason, message string) {
	// userPassthrough mode: credentials are sourced per-session from
	// the invoking user's UserIdentity catalog (Slice 1 + Slice 2's
	// passthrough gate). No shared AgentIdentity is required, and
	// credential-coverage validation moves to session-park time where
	// the operator knows which user is asking.
	passthrough := identityMode == spiceboxv1alpha1.IdentityModeUserPassthrough
	seenResolved := map[string]bool{}
	seenName := map[string]bool{}
	for _, b := range bundles {
		if !bundleNameRE.MatchString(b.Name) {
			return nil, spiceboxv1alpha1.ReasonSpecInvalid,
				fmt.Sprintf("toolBundles[%s]: name must match [a-z0-9_-]{1,32}", b.Name)
		}
		if seenName[b.Name] {
			return nil, spiceboxv1alpha1.ReasonSpecInvalid,
				fmt.Sprintf("toolBundles[%s]: duplicate name", b.Name)
		}
		seenName[b.Name] = true

		// SpiceboxClass exists.
		var cls spiceboxv1alpha1.SpiceboxClass
		if err := reader.Get(ctx, types.NamespacedName{Name: b.Class}, &cls); err != nil {
			if errors.IsNotFound(err) {
				return nil, spiceboxv1alpha1.ReasonClassMissing,
					fmt.Sprintf("toolBundles[%s].class: SpiceboxClass %q not found", b.Name, b.Class)
			}
			return nil, spiceboxv1alpha1.ReasonSpecInvalid,
				fmt.Sprintf("toolBundles[%s].class: %v", b.Name, err)
		}

		// Each toolspec exists.
		toolkitToToolspec := map[string]string{} // class-tool-name → toolspec name (collision detection)
		for _, tsName := range b.Toolspecs {
			var ts spiceboxv1alpha1.SpiceboxToolspec
			if err := reader.Get(ctx, types.NamespacedName{Name: tsName}, &ts); err != nil {
				if errors.IsNotFound(err) {
					return nil, spiceboxv1alpha1.ReasonToolspecMissing,
						fmt.Sprintf("toolBundles[%s].toolspecs: SpiceboxToolspec %q not found", b.Name, tsName)
				}
				return nil, spiceboxv1alpha1.ReasonSpecInvalid,
					fmt.Sprintf("toolBundles[%s].toolspecs: %v", b.Name, err)
			}
			// Toolspec must be Valid=True. A Valid=False toolspec (e.g.
			// ToolkitMissing because the revision pin doesn't match the
			// embedded catalog) would otherwise hang every ToolCall at
			// runtime forever — surface it here at apply time.
			vc := meta.FindStatusCondition(ts.Status.Conditions, spiceboxv1alpha1.SpiceboxToolspecConditionValid)
			if vc == nil {
				return nil, spiceboxv1alpha1.ReasonToolspecMissing,
					fmt.Sprintf("toolBundles[%s].toolspecs: SpiceboxToolspec %q has no Valid condition yet", b.Name, tsName)
			}
			if vc.Status != metav1.ConditionTrue {
				return nil, spiceboxv1alpha1.ReasonToolspecMissing,
					fmt.Sprintf("toolBundles[%s].toolspecs: SpiceboxToolspec %q is Valid=%s reason=%s — %s",
						b.Name, tsName, vc.Status, vc.Reason, vc.Message)
			}
			// Collision check: which class tool does this toolspec target?
			classToolName := ""
			for _, ct := range cls.Spec.Tools {
				if ct.Name == ts.Spec.Toolkit.Name {
					classToolName = ct.Name
					break
				}
			}
			if classToolName == "" {
				return nil, spiceboxv1alpha1.ReasonSpecInvalid,
					fmt.Sprintf("toolBundles[%s].toolspecs[%s]: toolkit %q not in class %q's tool catalog",
						b.Name, tsName, ts.Spec.Toolkit.Name, b.Class)
			}
			if prior, exists := toolkitToToolspec[classToolName]; exists {
				return nil, spiceboxv1alpha1.ReasonBundleToolNameCollision,
					fmt.Sprintf("toolBundles[%s]: toolspecs %q and %q both target class tool %q",
						b.Name, prior, tsName, classToolName)
			}
			toolkitToToolspec[classToolName] = tsName

			// Deduplicated across bundles: two bundles may name the same
			// toolspec, and one object listed twice would publish the same
			// producer twice on status.factSources.
			if !seenResolved[tsName] {
				seenResolved[tsName] = true
				resolved = append(resolved, ts)
			}
		}

		// AgentIdentity (override > class default; one of them required for
		// sandbox dispatch unless identityMode=userPassthrough, in which
		// case credentials come from the invoking user's UserIdentity per
		// session and no shared AgentIdentity is required).
		identity := b.AgentIdentity
		if identity == "" {
			identity = defaultIdentity
		}
		var ai spiceboxv1alpha1.AgentIdentity
		if !passthrough {
			if identity == "" {
				return nil, spiceboxv1alpha1.ReasonAgentIdentityMissing,
					fmt.Sprintf("toolBundles[%s]: no AgentIdentity (set spec.agentIdentity or per-bundle agentIdentity)", b.Name)
			}
			if err := reader.Get(ctx, types.NamespacedName{Namespace: namespace, Name: identity}, &ai); err != nil {
				if errors.IsNotFound(err) {
					return nil, spiceboxv1alpha1.ReasonAgentIdentityMissing,
						fmt.Sprintf("toolBundles[%s]: AgentIdentity %q not found in namespace %q", b.Name, identity, namespace)
				}
				return nil, spiceboxv1alpha1.ReasonSpecInvalid,
					fmt.Sprintf("toolBundles[%s]: %v", b.Name, err)
			}
			// Propagate identity validity: the resolved identity (class
			// default or per-bundle override) must be Valid=True before its
			// credential coverage is trusted. Mirrors validateMCPServers; an
			// empty-Secret credential leaves the identity Valid=False and the
			// class must inherit that so the AgentSession gate holds.
			if reason, msg := requireIdentityValid(&ai, identity); reason != "" {
				return nil, reason, msg
			}
		}

		// Credential coverage: each toolspec's sensitive env vars must be
		// backed by a named credential in the identity's catalog. The
		// declared name is the env's explicit credential: (toolkit
		// validation requires it on every sensitive env; a malformed
		// toolkit that omits it surfaces the env name verbatim so coverage
		// fails loudly). CredentialRemap from the bundle is applied before
		// the lookup.
		// Surfaces the missing-credential case in AgentClass.status
		// rather than letting it crash at session start.
		//
		// Skipped under identityMode=userPassthrough — the user's
		// UserIdentity catalog is per-session, populated as the user
		// links credentials via identityd's portal. The operator's
		// passthrough gate (parkAwaitingCredentials) does the equivalent
		// check at session start.
		if !passthrough {
			for _, tsName := range b.Toolspecs {
				reqs := toolspecCredentialRequirements(ctx, reader, tsName)
				for _, req := range reqs {
					credName := req.SuggestedName
					if remapped, ok := b.CredentialRemap[credName]; ok {
						credName = remapped
					}
					if !hasCredential(&ai, credName) {
						return nil, spiceboxv1alpha1.ReasonAgentIdentityBindingMissing,
							fmt.Sprintf("toolBundles[%s]: AgentIdentity %q has no credential %q (required by toolspec %s) — run `oap agent setup-identity` to scaffold it",
								b.Name, identity, credName, tsName)
					}
				}
			}
		}
	}
	return resolved, "", ""
}

// toolspecCredentialRequirements returns the sensitive credential names
// declared by the toolkit backing the named SpiceboxToolspec. Each entry
// is an authkind.CredentialRequirement whose SuggestedName holds the
// explicit credential: name from the env (toolkit validation requires it;
// a malformed toolkit that omits it surfaces the env name verbatim).
// Returns nil when the toolspec or toolkit cannot be resolved — the
// caller treats nil as "no credential requirements" (toolspec validity
// was already checked earlier in the same reconcile).
func toolspecCredentialRequirements(ctx context.Context, reader client.Reader, tsName string) []authkind.CredentialRequirement {
	var ts spiceboxv1alpha1.SpiceboxToolspec
	if err := reader.Get(ctx, client.ObjectKey{Name: tsName}, &ts); err != nil {
		return nil
	}
	tkName := ts.Spec.Toolkit.Name
	if tkName == "" {
		return nil
	}
	// Resolve toolkit sensitive env vars: prefer a SpiceboxToolkit CR,
	// fall back to the embedded compile-time catalog.
	// Mirrors the lookup order in cli.Kind.ResolveTarget.
	var out []authkind.CredentialRequirement
	var sbtk spiceboxv1alpha1.SpiceboxToolkit
	if err := reader.Get(ctx, client.ObjectKey{Name: tkName}, &sbtk); err == nil {
		for _, e := range sbtk.Spec.Env.Allowed {
			if !e.Sensitive {
				continue
			}
			credName := e.Credential
			if credName == "" {
				// Toolkit validation requires credential: on sensitive envs; an
				// empty one here is a malformed toolkit. Surface the env name so
				// coverage fails loudly rather than inventing a lowercased name.
				credName = e.Name
			}
			out = append(out, authkind.CredentialRequirement{SuggestedName: credName})
		}
		return out
	}
	// Embedded fallback.
	for _, tk := range embeddedtoolkits.All() {
		if tk.Name != tkName {
			continue
		}
		for _, e := range tk.Env.Allowed {
			if !e.Sensitive {
				continue
			}
			credName := e.Credential
			if credName == "" {
				// Toolkit validation requires credential: on sensitive envs; an
				// empty one here is a malformed toolkit. Surface the env name so
				// coverage fails loudly rather than inventing a lowercased name.
				credName = e.Name
			}
			out = append(out, authkind.CredentialRequirement{SuggestedName: credName})
		}
		break
	}
	return out
}

// requireIdentityValid checks that the bound AgentIdentity is itself
// Valid=True before its credential coverage is trusted. An AgentIdentity
// with the right credential NAME but an EMPTY backing Secret goes
// Valid=False (reason CredentialEmpty) in its own controller; without
// this check it would still pass the class's name-coverage check and the
// class would go Valid — defeating the AgentSession start gate. Returns
// ("", "") when ai is nil (caller already skips for passthrough/missing
// identity) or when ai is Valid=True.
func requireIdentityValid(ai *spiceboxv1alpha1.AgentIdentity, identityName string) (reason, message string) {
	if ai == nil {
		return "", ""
	}
	c := meta.FindStatusCondition(ai.Status.Conditions, spiceboxv1alpha1.AgentIdentityConditionValid)
	if c != nil && c.Status == metav1.ConditionTrue {
		return "", ""
	}
	detail := "no Valid condition yet"
	if c != nil {
		detail = fmt.Sprintf("Valid=%s reason=%s", c.Status, c.Reason)
	}
	return spiceboxv1alpha1.ReasonAgentIdentityInvalid,
		fmt.Sprintf("AgentIdentity %q is not Valid (%s)", identityName, detail)
}

// hasCredential reports whether the AgentIdentity's credential catalog
// contains an entry with the given name.
func hasCredential(ai *spiceboxv1alpha1.AgentIdentity, name string) bool {
	for _, c := range ai.Spec.Credentials {
		if c.Name == name {
			return true
		}
	}
	return false
}

// ensurePlatformLink writes the agentclass#platform link and records the
// outcome as a condition. It NEVER stops the reconcile — the caller captures
// the error and returns it only after status has converged.
//
// The condition exists because this write's absence is otherwise invisible:
// agentclass#starter ships unpopulated, so this tuple is the only thing making
// agentclass#start_session satisfiable, and without it the browser's agent
// picker is simply empty with nothing anywhere saying why.
func (r *Reconciler) ensurePlatformLink(ctx context.Context, ac *spiceboxv1alpha1.AgentClass) error {
	if r.PlatformLinker == nil {
		log.FromContext(ctx).Info("PlatformLinker not configured; skipping the agentclass#platform link — agentclass#start_session is UNSATISFIABLE for this class, so it can never appear in the browser's agent picker",
			"agentclass", ac.Namespace+"/"+ac.Name)
		// The condition is deliberately NOT stamped here. A nil linker is a
		// binary-wiring fact (local dev, a test fixture), identical for every
		// AgentClass in the process; writing PlatformLinked=False onto every CR
		// would be status noise about the operator, not about the object.
		return nil
	}
	err := r.PlatformLinker.EnsureAgentClassPlatform(ctx, ac.Namespace, ac.Name)
	switch {
	case err == nil:
		conditions.SetTrue(ac, &ac.Status.Conditions,
			spiceboxv1alpha1.AgentClassConditionPlatformLinked, spiceboxv1alpha1.ReasonPlatformLinked)
		return nil

	case stderrors.Is(err, spicedb.ErrUnrepresentableObjectID):
		// PERMANENT, and unfixable without recreating the CR under another name
		// (a Kubernetes name is immutable). Retrying is pointless, so the
		// condition IS the report — this is the one link failure a human must
		// act on rather than wait out.
		log.FromContext(ctx).Info("this AgentClass's name cannot be expressed as a SpiceDB object id, so the agentclass#platform link can NEVER be written and agentclass#start_session is permanently unsatisfiable for it; it will never appear in the browser's agent picker. Recreate it under a name without '.' (or any character outside [a-zA-Z0-9/_|-=+]). NOT retrying",
			"agentclass", ac.Namespace+"/"+ac.Name, "err", err.Error())
		conditions.SetFalse(ac, &ac.Status.Conditions,
			spiceboxv1alpha1.AgentClassConditionPlatformLinked,
			spiceboxv1alpha1.ReasonUnrepresentableClassName, err.Error())
		// nil, not err: a permanent failure returned as an error would requeue
		// forever against a condition no retry can change.
		return nil

	default:
		// Most failures here are transient (SpiceDB unreachable) and the
		// requeue clears them. One is NOT: "object definition `agentclass` not
		// found" means the live SpiceDB schema predates this operator, and no
		// amount of retrying this write fixes it — the guardian controller's
		// RunAll has to compose and write the schema first. That self-heals at
		// operator start on any cluster with an AgentClass, so it is not worth
		// a distinct code path, but it IS worth naming: the two look identical
		// in the logs and the permanent one otherwise reads as a stuck retry.
		log.FromContext(ctx).Info("writing the agentclass#platform link failed; this class stays absent from the browser's agent picker until it succeeds — requeueing. If the error is \"object definition `agentclass` not found\", this is PERMANENT until the guardian controller writes the composed schema, not a transient SpiceDB blip",
			"agentclass", ac.Namespace+"/"+ac.Name, "err", err.Error())
		conditions.SetFalse(ac, &ac.Status.Conditions,
			spiceboxv1alpha1.AgentClassConditionPlatformLinked,
			spiceboxv1alpha1.ReasonPlatformLinkFailed, err.Error())
		return fmt.Errorf("ensure agentclass#platform link: %w", err)
	}
}
