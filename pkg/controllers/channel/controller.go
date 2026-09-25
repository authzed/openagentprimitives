// Package channel reconciles Channel CRDs.
//
// The reconciler validates references (Secret, AgentClass, AgentIdentity) and
// surfaces the result as the `Valid` condition. It does NOT manage transport —
// the `Connected` condition is owned by channelsd, not the operator. channelsd
// patches Connected directly via the Channel/status subresource.
package channel

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/adoptguard"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/outputbind"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/registry"
	"github.com/authzed/openagentprimitives/pkg/controllers/conditions"
	apreconcile "github.com/authzed/openagentprimitives/pkg/controllers/internal/reconcile"
	"github.com/authzed/openagentprimitives/pkg/platform/cloud"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/tools/adoptkit"
)

// +kubebuilder:rbac:groups=agentprimitives.authzed.com,resources=channels,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=agentprimitives.authzed.com,resources=channels/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=agentprimitives.authzed.com,resources=agentclasses,verbs=get;list;watch
// +kubebuilder:rbac:groups=agentprimitives.authzed.com,resources=agentidentities,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch

// PublicEndpoint is where this cluster is reachable from, and so where a
// provider must be told to deliver this Channel's webhooks. WATCH is not
// optional alongside get;list: SetupWithManager registers an informer on the
// type, and an informer without the watch verb takes the whole operator down
// on "failed to wait for caches to sync" — at deploy time, with nothing in
// the build to have caught it. Declared here as well as on the PublicEndpoint
// reconciler because this controller needs it independently; a generated
// ClusterRole is the union of every marker, so relying on that one would let
// its removal silently take this with it.
// +kubebuilder:rbac:groups=agentprimitives.authzed.com,resources=publicendpoints,verbs=get;list;watch

// The per-Channel webhook-secret grant (webhookrbac.go) is a Role +
// RoleBinding the operator stamps in the Channel's own namespace. Declared
// here as well as on the AgentSession reconciler because this controller needs
// it independently — a generated ClusterRole is the union of every marker, so
// dropping the AgentSession one would silently take this with it.
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=roles;rolebindings,verbs=get;list;watch;create;patch

// Reconciler reconciles Channel objects.
type Reconciler struct {
	Client client.Client
	// SecretReader is the guarded Secret reader. Reads are gated to secrets
	// the operator has adopted (carrying AdoptedLabel) or the fixed-infra
	// allowlist. Must be set before Reconcile is called.
	SecretReader *adoptguard.SecretReader

	// ExternalBaseURL returns this cluster's externally reachable base URL —
	// the same live-updating accessor shape channelsd's pipeline watchers use
	// (see pkg/x/externalurl.Provider.Get) — used to build the webhook URL
	// this cluster actually serves, via channelevents.WebhookPathFor, for the
	// WebhookURLDrift check. Nil, or a func returning "", skips the check
	// entirely rather than comparing against a bare path with no host: that
	// would read as permanent drift for every provider-registered webhook,
	// which is worse than not checking.
	ExternalBaseURL func() string

	// GitHubAPIBaseURL overrides the GitHub API host the WebhookURLDrift
	// check talks to for kind=github Channels. Empty means the github kind's
	// real default (api.github.com). This is the only seam that lets a test
	// redirect CheckWebhookURLDrift's outbound call, since
	// appprovision.NewHTTPClient defaults to the real API otherwise;
	// production leaves this empty.
	//
	// Deliberately github-specific on an otherwise kind-agnostic Reconciler
	// — a real smell, kept anyway because the alternative (a
	// config-per-kind override abstraction) has exactly one consumer today
	// and would be speculative. Generalize this only when a second
	// channelkinds.WebhookURLDriftChecker implementation actually needs its
	// own override; until then a single field beats an abstraction built
	// for a membership of one.
	GitHubAPIBaseURL string
}

func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("channel", req.NamespacedName.String())

	var ch spiceboxv1alpha1.Channel
	if cont, err := apreconcile.LoadInto(ctx, r.Client, req.NamespacedName, &ch); !cont {
		return ctrl.Result{}, err
	}
	if ch.DeletionTimestamp != nil {
		return ctrl.Result{}, nil
	}

	reason, message, ok, classUID := r.validate(ctx, &ch)

	status := metav1.ConditionFalse
	if ok {
		status = metav1.ConditionTrue
	}
	conditions.Set(&ch, &ch.Status.Conditions, metav1.Condition{
		Type:   spiceboxv1alpha1.ChannelConditionValid,
		Status: status, Reason: reason, Message: message,
	})
	ch.Status.ObservedGeneration = ch.Generation
	// ResolvedAgentClassUID is frozen at first Valid=True; never cleared.
	if ok && ch.Status.ResolvedAgentClassUID == "" && classUID != "" {
		ch.Status.ResolvedAgentClassUID = classUID
	}

	// Set the InformationLeakageReady condition on this Channel based on
	// whether its kind satisfies the bound AgentClass's policy. Skipped
	// for monitoring-role channels (no AgentClass).
	r.reconcileInfoLeakageCondition(ctx, &ch)

	// Keep this Channel's provider-registered webhook URL agreeing with where
	// the cluster serves, and set WebhookURLDrift, for whichever kind
	// implements channelkinds.WebhookURLDriftChecker /
	// channelkinds.WebhookURLRepointer (github today); a no-op for every
	// other kind. Never fails the reconcile — a webhook that needs attention
	// is a status finding, not a retry condition. requeueAfter schedules
	// whatever is owed next (a throttled re-check, a failed write's retry);
	// zero means this reconcile made no outbound call at all (no capability,
	// no known external URL, no Secret) and so has nothing to schedule.
	requeueAfter := r.reconcileWebhookURL(ctx, &ch)

	// Grant webd the read of this Channel's credentials Secret it needs to
	// verify a delivery, scoped to that one Secret in this namespace — see
	// webhookrbac.go. A no-op for a kind that receives nothing over HTTP.
	//
	// The error is deliberately held until AFTER the status write rather than
	// returned straight away: the Valid condition computed above is the only
	// thing telling an operator what this reconcile found, and dropping it to
	// report an RBAC failure would trade one silent symptom for another. It is
	// then returned, so the reconcile is retried with backoff and the failure
	// reaches controller-runtime's error log rather than being swallowed.
	rbacErr := r.ensureWebhookSecretRBAC(ctx, &ch)

	if err := r.Client.Status().Update(ctx, &ch); err != nil {
		return ctrl.Result{}, err
	}
	if rbacErr != nil {
		logger.Info("webhook-secret RBAC could not be applied; inbound deliveries for this Channel will fail at webd",
			"kind", ch.Spec.Kind, "secret", ch.Spec.CredentialsRef.SecretName, "err", rbacErr.Error())
		return ctrl.Result{}, rbacErr
	}
	logger.V(1).Info("reconciled", "valid", ok, "reason", reason)
	return ctrl.Result{RequeueAfter: requeueAfter}, nil
}

// validate returns (reason, message, ok, classUID).
func (r *Reconciler) validate(ctx context.Context, ch *spiceboxv1alpha1.Channel) (string, string, bool, types.UID) {
	// Every derived fact this function publishes is recomputed from scratch on
	// each pass, so the previous pass's answer is cleared before any of the
	// checks that can return early. A Channel that stopped satisfying the owner
	// rule — or that stopped being readable at all — must not keep advertising
	// a subject-set that is no longer in force; an owner carries approve and
	// fork, so a stale one reads as authority nobody currently has.
	ch.Status.DerivedSessionOwner = ""

	// 1. Kind must be registered, then it owns its own spec validation
	//    (slack rejects spec.fake!=nil, fake rejects spec.slack!=nil,
	//    future kinds enforce their own block).
	k, ok := registry.Get(ch.Spec.Kind)
	if !ok {
		return spiceboxv1alpha1.ReasonChannelSpecInvalid,
			fmt.Sprintf(`unknown kind %q`, ch.Spec.Kind), false, ""
	}
	if err := k.ValidateSpec(ch); err != nil {
		return spiceboxv1alpha1.ReasonChannelSpecInvalid, err.Error(), false, ""
	}

	// 1b. A kind that cannot attribute an inbound message to a human —
	//     Kind.UserAttributable() == false, bento and github today — has no
	//     per-user identity to derive an acting subject from, so the Channel
	//     MUST declare one. Checked here, off the registry predicate, rather
	//     than re-implemented in each such kind's ValidateSpec: the rule
	//     belongs to the predicate, and a future non-attributable kind that
	//     forgets it would otherwise reach the same state.
	//
	//     Leaving it empty is not a soft failure. The AgentClass this Channel
	//     binds to goes Valid=False for the whole class (see agentclass's
	//     userLessChannelMissingAuthz), taking down its paired output Channel
	//     with it; and past that gate the inbound pipeline's service-subject
	//     branch never fires, so started_by falls through to a phantom
	//     non-service subject — the exact substitution the SECURITY note on
	//     that branch exists to prevent. Reporting it on the Channel itself is
	//     what points a human at the field they actually have to fix.
	//
	//     role=output produces no inbound at all, and role=monitoring is a
	//     framework-event sink bound to no agent; attribution is moot for both.
	//     Both exclusions live inside registry.IsUserlessInput, the one place
	//     that answers "does this Channel's inbound carry a human" for the
	//     several rules that turn on it.
	if registry.IsUserlessInput(ch) && ch.Spec.AuthzSubject == "" {
		return spiceboxv1alpha1.ReasonChannelSpecInvalid,
			fmt.Sprintf(`kind %q cannot attribute inbound messages to a user; spec.authzSubject is required (format "service:<id>")`, ch.Spec.Kind),
			false, ""
	}

	// 1c. spec.channelHistory.enabled requires the kind to implement
	// channelkinds.ChannelHistoryReader (slack does today).
	if ch.Spec.ChannelHistory != nil && ch.Spec.ChannelHistory.Enabled {
		if _, ok := k.(channelkinds.ChannelHistoryReader); !ok {
			return spiceboxv1alpha1.ReasonChannelSpecInvalid,
				fmt.Sprintf("kind %q does not support channel history (spec.channelHistory.enabled)", ch.Spec.Kind),
				false, ""
		}
	}

	// 2. Secret exists. Only when a credentials Secret is actually referenced
	// (some future kinds may have no creds).
	var sec *corev1.Secret
	if ch.Spec.CredentialsRef.SecretName != "" {
		secretRef := types.NamespacedName{Namespace: ch.Namespace, Name: ch.Spec.CredentialsRef.SecretName}
		ownerRef := types.NamespacedName{Namespace: ch.Namespace, Name: ch.Name}
		// Adopt the referenced Secret — metadata-only SSA that stamps AdoptedLabel
		// so subsequent reads via SecretReader are permitted. Adopt's existence
		// check uses the live reader (r.SecretReader.Reader), so a not-yet-adopted
		// Secret (absent from the label-filtered cache) is seen. A NotFound from
		// adopt drives the user-visible SecretMissing reason — the same signal the
		// dropped cached pre-check produced — without silently minting an empty
		// Secret via the SSA apply.
		if err := adoptkit.AdoptSecret(ctx, r.SecretReader.Reader, r.Client, secretRef, ownerRef, "Channel"); err != nil {
			if apierrors.IsNotFound(err) {
				return spiceboxv1alpha1.ReasonChannelSecretMissing,
					fmt.Sprintf(`Secret %q not found`, ch.Spec.CredentialsRef.SecretName), false, ""
			}
			logger := log.FromContext(ctx).WithValues("channel", ch.Name)
			logger.Info("adoptkit.AdoptSecret failed, requeuing", "secret", secretRef, "err", err)
			return spiceboxv1alpha1.ReasonChannelSecretMissing,
				fmt.Sprintf("adopt Secret %q: %v", ch.Spec.CredentialsRef.SecretName, err), false, ""
		}
		s, err := r.SecretReader.Get(ctx, secretRef)
		if err != nil {
			if apierrors.IsNotFound(err) {
				return spiceboxv1alpha1.ReasonChannelSecretMissing,
					fmt.Sprintf(`Secret %q not found`, ch.Spec.CredentialsRef.SecretName), false, ""
			}
			return spiceboxv1alpha1.ReasonChannelSecretMissing, err.Error(), false, ""
		}
		sec = s
	}

	// 3. Secret has the required keys the kind declares.
	if sec != nil {
		for _, key := range k.RequiredSecretKeys(ch) {
			if _, ok := sec.Data[key]; !ok {
				return spiceboxv1alpha1.ReasonChannelSecretKeyMissing,
					fmt.Sprintf(`Secret %q missing key %q`, sec.Name, key), false, ""
			}
		}
	}

	// role=monitoring Channels are framework-event sinks, not bound to
	// an AgentClass. Validate the monitoring destination, then skip the
	// AgentClass / AgentIdentity reference checks.
	if ch.Spec.Role == spiceboxv1alpha1.ChannelRoleMonitoring {
		// Slack monitoring channels require outputDefaults.channelId so the
		// framework knows where to post events. The slack kind's ValidateSpec
		// also enforces this; this guard makes the requirement explicit at the
		// controller layer too.
		if ch.Spec.Slack != nil &&
			(ch.Spec.Slack.OutputDefaults == nil || ch.Spec.Slack.OutputDefaults.ChannelID == "") {
			return spiceboxv1alpha1.ReasonChannelSpecInvalid,
				`spec.slack.outputDefaults.channelId is required when role="monitoring"`, false, ""
		}
		return spiceboxv1alpha1.ReasonChannelAllReferencesResolve, "", true, ""
	}

	// AgentClass is required for every non-monitoring role.
	if ch.Spec.AgentClass == "" {
		return spiceboxv1alpha1.ReasonChannelSpecInvalid,
			`spec.agentClass is required unless role="monitoring"`, false, ""
	}

	// 4. AgentClass exists + Valid=True.
	var class spiceboxv1alpha1.AgentClass
	if err := r.Client.Get(ctx, types.NamespacedName{
		Namespace: ch.Namespace, Name: ch.Spec.AgentClass,
	}, &class); err != nil {
		if apierrors.IsNotFound(err) {
			return spiceboxv1alpha1.ReasonChannelAgentClassMissing,
				fmt.Sprintf(`AgentClass %q not found`, ch.Spec.AgentClass), false, ""
		}
		return spiceboxv1alpha1.ReasonChannelAgentClassMissing, err.Error(), false, ""
	}
	if !classIsValid(&class) {
		return spiceboxv1alpha1.ReasonChannelAgentClassNotValid,
			fmt.Sprintf(`AgentClass %q is not Valid=True`, class.Name), false, class.UID
	}

	// 4b. A role=output Channel bound to a class whose sessions can be BORN with
	// no human on them must configure its own destination.
	//
	// Nothing inbound supplies one: the class's input is a webhook payload or a
	// cron tick, not a message in a thread to reply into. Refused at apply time
	// because the alternative is a Channel that reports Valid=True with nowhere
	// to post, whose failure waits until the agent finishes its work — long past
	// install, in the one moment the reply matters. The same missing field also
	// disables spec.owner.ownerless.fromOutputChannel as an owner source (a
	// kind's OwnerGroupRef is derived from the destination), so a Channel in
	// that state would be broken twice over and say so nowhere.
	//
	// Keyed off the class's DERIVED status.userlessInput rather than re-listing
	// and re-classifying the class's input Channels here — one derivation, many
	// rules (see AgentClassStatus.UserlessInput). Reading a plain bool is sound
	// because the AgentClass reconciler stamps it before deciding Valid, and the
	// check immediately above has just required Valid=True.
	//
	// The destination question is asked through outputbind.Anchor — the same
	// kind-owned derivation the outbound relay seeds a session from, so the two
	// cannot disagree — and the kind's own error names the field to fill in
	// without this controller knowing any kind by name.
	//
	// role=both is deliberately untouched: it is its own origin and destination,
	// and its inbound carries the thread to reply into. role=monitoring keeps
	// its own destination rule above, for its own reason (bound to no agent, so
	// there is never an inbound at all).
	if ch.Spec.Role == spiceboxv1alpha1.ChannelRoleOutput && class.Status.UserlessInput {
		if _, _, err := outputbind.Anchor(ch); err != nil {
			return spiceboxv1alpha1.ReasonChannelOutputDestinationMissing,
				fmt.Sprintf("AgentClass %q has no user-attributable input: no inbound message supplies a destination, so this role=%q Channel must configure its own — %v",
					class.Name, spiceboxv1alpha1.ChannelRoleOutput, err),
				false, class.UID
		}
	}

	// 5. Owner-policy resolvability.
	//
	// Determine whether the channel kind provides a starting user on each
	// inbound event. We probe with a dummy event; the result is binary
	// (provides / does not provide) regardless of the event contents.
	providesStarter := false
	if so, ok := k.(channelkinds.SessionOwnerProvider); ok {
		_, providesStarter = so.SessionOwner(channelkinds.InboundEvent{
			ExternalIDs: channelkinds.ExternalIdentity{Kind: identity.Kind(ch.Spec.Kind), ExternalID: "probe"},
		})
	}
	// Both "no starter" refusals below ask the same question: when an inbound
	// on this Channel CREATES a session, where does that session's owner come
	// from? A kind that spawns nothing has no such session to answer for --
	// every session bound to it was created by something else, which resolved
	// the owner from its own inputs. The `agent` kind is the case that makes
	// the distinction load-bearing: the SubagentRequest controller creates a
	// conversational child and binds a kind=agent Channel to it, and the child's
	// owner comes from the started-by annotations it inherits from its parent,
	// never from this Channel. Refusing here would make every such Channel
	// permanently Valid=False for a question it is not the answer to.
	//
	// It gates only these two refusals. The ownerCeiling checks and the
	// passthrough forbid-override check below still apply to every kind: those
	// are about a policy the Channel DECLARES, which is wrong whether or not
	// anything is ever spawned here.
	spawnsSessions := k.SpawnsSessionOnInbound()
	pol := ch.Spec.Owner
	passthrough := class.Spec.IdentityMode == spiceboxv1alpha1.IdentityModeUserPassthrough

	if passthrough {
		if !providesStarter && spawnsSessions {
			return spiceboxv1alpha1.ReasonChannelSpecInvalid,
				"passthrough AgentClass requires an input channel that provides a starting user; this kind does not", false, class.UID
		}
		if pol != nil && (pol.Explicit != "" || pol.Ownerless != nil) {
			return spiceboxv1alpha1.ReasonChannelSpecInvalid,
				"passthrough AgentClass forbids Channel owner.explicit/ownerless (owner is the starter)", false, class.UID
		}
	} else {
		if !providesStarter && spawnsSessions {
			hasExplicit := pol != nil && pol.Explicit != ""
			hasOwnerless := pol != nil && pol.Ownerless != nil && (pol.Ownerless.Permission != "" || pol.Ownerless.FromOutputChannel)
			// A Channel that declared nothing is not necessarily a Channel with
			// no owner. The people in the room this agent's output lands in are
			// an owner, and they are the one an install can reach without
			// anyone writing a per-install identifier down — which is what made
			// this refusal a mandatory hand-patch on every fresh
			// webhook-driven agent rather than a real misconfiguration.
			//
			// Asked only when nothing was declared: an authored owner is an
			// intent and is never second-guessed, and the lookup is an API read
			// worth skipping when its answer cannot be used. Recorded on status
			// because an owner carries approve/fork/manage_scope, not merely
			// interact — see ChannelStatus.DerivedSessionOwner.
			if !hasExplicit && !hasOwnerless {
				derived := r.derivedOwnerGroupRef(ctx, ch, &class)
				if derived == "" {
					return spiceboxv1alpha1.ReasonChannelSpecInvalid,
						"ownerless input requires spec.owner.explicit or spec.owner.ownerless (permission / fromOutputChannel)", false, class.UID
				}
				ch.Status.DerivedSessionOwner = derived
			}
		}
		if ceil := class.Spec.GetOwnerCeiling(); ceil != nil && pol != nil {
			if ceil.StarterOnly && (pol.Explicit != "" || pol.Ownerless != nil) {
				return spiceboxv1alpha1.ReasonChannelSpecInvalid,
					"AgentClass ownerCeiling.starterOnly forbids Channel owner.explicit/ownerless", false, class.UID
			}
			if ceil.Fixed != "" && pol.Explicit != "" && pol.Explicit != ceil.Fixed {
				return spiceboxv1alpha1.ReasonChannelSpecInvalid,
					fmt.Sprintf("AgentClass ownerCeiling.fixed pins owner to %q; Channel may not override", ceil.Fixed), false, class.UID
			}
		}
	}

	// 6. A role=input Channel delivers its reply into a DIFFERENT Channel; that
	// target must resolve now, not at 09:00 on a Monday when the cron fires.
	// role=both is self-contained (both origin and destination) and asserts
	// nothing here.
	if ch.Spec.Role == spiceboxv1alpha1.ChannelRoleInput {
		outCh, err := outputbind.Resolve(ctx, r.Client, ch.Namespace, ch.Spec.AgentClass)
		if err != nil {
			return spiceboxv1alpha1.ReasonChannelOutputBindingUnresolvable, err.Error(), false, class.UID
		}
		if _, _, err := outputbind.Anchor(outCh); err != nil {
			return spiceboxv1alpha1.ReasonChannelOutputBindingUnresolvable, err.Error(), false, class.UID
		}
	}

	// 7. AgentIdentity (if specified) exists.
	if ch.Spec.AgentIdentity != "" {
		var ident spiceboxv1alpha1.AgentIdentity
		if err := r.Client.Get(ctx, types.NamespacedName{
			Namespace: ch.Namespace, Name: ch.Spec.AgentIdentity,
		}, &ident); err != nil {
			if apierrors.IsNotFound(err) {
				return spiceboxv1alpha1.ReasonChannelAgentIdentityMissing,
					fmt.Sprintf(`AgentIdentity %q not found`, ch.Spec.AgentIdentity), false, class.UID
			}
			return spiceboxv1alpha1.ReasonChannelAgentIdentityMissing, err.Error(), false, class.UID
		}
	}

	return spiceboxv1alpha1.ReasonChannelAllReferencesResolve, "", true, class.UID
}

// derivedOwnerGroupRef returns the membership subject-set that will own sessions
// inbound on ch when nothing else supplies an owner, or "" when there is nothing
// to derive from.
//
// It mirrors, at apply time, the lookup the operator performs per session
// (agentsession.Reconciler.outputChannelGroupRef): a role=input Channel's reply
// lands in its class's single role=output sibling, and every other inbound role
// is its own destination — the same split channelsd applies when it decides
// whether to set AgentSession.spec.outputChannel. The two must agree, or this
// controller reports Valid=True for a Channel whose sessions the operator will
// then refuse to give an owner.
//
// The class's ownerCeiling is the veto: an admin who pinned the owner, or
// restricted it to the session's starter, has already answered this question,
// and a derived owner would never pass through the config-level refusal that
// enforces it (see v1alpha1.OwnerMayComeFromOutputChannel).
//
// An unresolvable output binding yields "" rather than an error: the caller's
// own refusal already names the field a human has to fill in, and the more
// precise output-binding refusal follows on a later pass once an owner exists.
// It is logged so the reason a Channel stayed refused is greppable rather than
// inferred from an absence.
func (r *Reconciler) derivedOwnerGroupRef(ctx context.Context, ch *spiceboxv1alpha1.Channel, class *spiceboxv1alpha1.AgentClass) string {
	if !spiceboxv1alpha1.OwnerMayComeFromOutputChannel(class.Spec.GetOwnerCeiling()) {
		return ""
	}
	outCh := ch // every inbound role but `input` is its own destination
	if ch.Spec.Role == spiceboxv1alpha1.ChannelRoleInput {
		resolved, err := outputbind.Resolve(ctx, r.Client, ch.Namespace, ch.Spec.AgentClass)
		if err != nil {
			log.FromContext(ctx).V(1).Info("no owner derivable: this Channel's reply target does not resolve",
				"channel", ch.Namespace+"/"+ch.Name, "agentClass", ch.Spec.AgentClass, "err", err.Error())
			return ""
		}
		outCh = resolved
	}
	return registry.OwnerGroupRefForChannel(outCh)
}

// reconcileInfoLeakageCondition sets ChannelConditionInformationLeakageReady
// on the Channel based on whether this Channel's kind satisfies the capability
// level required by the bound AgentClass's informationLeakage policy.
//
// Skipped (no condition written) for monitoring-role channels (no AgentClass).
// Logic mirrors agentclass.Reconciler.reconcileInfoLeakageCondition; both must
// be kept in sync.
func (r *Reconciler) reconcileInfoLeakageCondition(ctx context.Context, ch *spiceboxv1alpha1.Channel) {
	logger := log.FromContext(ctx).WithValues("channel", ch.Name)

	// Monitoring channels have no AgentClass; skip.
	if ch.Spec.Role == spiceboxv1alpha1.ChannelRoleMonitoring || ch.Spec.AgentClass == "" {
		return
	}

	// Look up the AgentClass to read the policy.
	var class spiceboxv1alpha1.AgentClass
	if err := r.Client.Get(ctx, types.NamespacedName{
		Namespace: ch.Namespace, Name: ch.Spec.AgentClass,
	}, &class); err != nil {
		// AgentClass missing/unreadable — the Valid condition already covers
		// this; don't set InformationLeakageReady at all.
		return
	}

	policy := class.Spec.GetAuthz().InformationLeakage
	mode := policy.ResolvedMode()

	k, kindKnown := registry.Get(ch.Spec.Kind)
	cap := channelkinds.CapabilityUnsupported
	if kindKnown {
		if ar, isAR := k.(channelkinds.AudienceResolver); isAR {
			cap = ar.AudienceCapability()
		}
	}

	ready := true
	reason := spiceboxv1alpha1.ReasonInfoLeakageReady
	message := ""

	if mode == "enforcing" || mode == "logging" {
		onUnsup := policy.ResolvedOnUnsupportedChannel()
		singleBypass := policy.ResolvedSingleUserBypass()

		var blockReason, blockMsg string
		switch {
		case cap == channelkinds.CapabilityUnsupported && onUnsup == "blockBinding":
			blockReason = spiceboxv1alpha1.ReasonChannelKindLacksAudienceResolver
			blockMsg = fmt.Sprintf("channel %q kind %q has no AudienceResolver; required by informationLeakage.mode=enforcing", ch.Name, ch.Spec.Kind)
		case cap == channelkinds.CapabilitySingleUser && !singleBypass:
			blockReason = spiceboxv1alpha1.ReasonSingleUserBypassDisabled
			blockMsg = fmt.Sprintf("channel %q is single-user but informationLeakage.singleUserBypass=false", ch.Name)
		}

		if blockReason != "" {
			if mode == "enforcing" {
				ready = false
				reason = blockReason
				message = blockMsg
			} else {
				// mode == "logging": warn, don't block.
				logger.Info("informationLeakage capability warning", "kind", ch.Spec.Kind, "issue", blockReason)
			}
		}
	}

	if ready {
		conditions.SetTrue(ch, &ch.Status.Conditions,
			spiceboxv1alpha1.ChannelConditionInformationLeakageReady, reason)
	} else {
		conditions.SetFalse(ch, &ch.Status.Conditions,
			spiceboxv1alpha1.ChannelConditionInformationLeakageReady, reason, message)
	}
}

// webhookURLDriftCheckInterval bounds how often reconcileWebhookURLDrift
// actually calls out to a provider (github today). Drift is a rare,
// human-caused event — a DNS cutover, an ingress rebuild — not something
// that needs minute-level detection, and an unbounded per-reconcile call
// multiplies by Channel count and by every unrelated status write against a
// rate limit this cluster does not control. A rate-limited App also fails
// the installation-token mint the drift check exists to help operators keep
// working, so the check must never itself be what exhausts that limit.
// Hours, not minutes: generous enough that a busy cluster's normal reconcile
// churn never drives this above a background rate, while still catching
// drift within the working day it was introduced.
const webhookURLDriftCheckInterval = 6 * time.Hour

// webhookRepointRetryInterval is how soon a FAILED repoint is retried. It is
// far shorter than webhookURLDriftCheckInterval because the two failures are
// not comparable: a stale drift reading costs an operator a late finding,
// while a webhook still pointed at an address that no longer exists means
// every delivery is being lost right now, and the agent looks dead to anyone
// opening a pull request.
//
// Not shorter still, because the retry is a WRITE against the same rate limit
// the drift check is throttled for, and a permanently-failing repoint (an App
// deleted at the provider, a revoked key) retries forever. At this interval
// that is a handful of calls an hour, which stays background noise against
// the App's budget while recovering from a transient failure within minutes.
const webhookRepointRetryInterval = 15 * time.Minute

// reconcileWebhookURL keeps this Channel's inbound webhook registration —
// held by a third party, out of band (a GitHub App's hook_attributes.url) —
// agreeing with where the cluster actually serves, and reports the outcome on
// ChannelConditionWebhookURLDrift.
//
// Both halves dispatch by type-asserting the registered Kind, never an
// if kind=="github" branch, so a future second provider-registered-webhook
// kind needs no change here. A kind that implements neither
// channelkinds.WebhookURLDriftChecker nor channelkinds.WebhookURLRepointer
// leaves the condition unset entirely, the same "absent means not applicable"
// shape reconcileInfoLeakageCondition uses for monitoring-role channels.
//
// THE REPOINT DECISION IS MADE FROM LOCAL STATE, and that is the whole point.
// The expected URL is derived here — the cluster's public address plus
// channelevents.WebhookPathFor — and compared against
// status.repointedWebhookURL, the URL this controller last successfully
// wrote. The provider is called only to WRITE. Driving the decision off the
// drift READ instead would leave two bad options: inherit its throttle, and
// every change of address leaves webhooks broken for up to a full interval;
// or bypass it, and spend a rate limit this cluster does not control — whose
// exhaustion also breaks the installation-token mint — on a question already
// answerable locally.
//
// The read still runs, throttled, as the safety net for a change made at the
// provider that no local state can predict. When it finds one on a Channel
// this controller may write to, it corrects it there and then.
//
// This never fails the reconcile and never mutates ch.Spec: a webhook that
// needs a human is a status finding, not a retryable error.
//
// Returns the RequeueAfter the caller should schedule: zero when this
// reconcile made (and needed) no outbound call at all — nothing to
// schedule — otherwise the remaining time until the next check is due
// (throttled), webhookRepointRetryInterval (a write to retry), or the full
// webhookURLDriftCheckInterval.
func (r *Reconciler) reconcileWebhookURL(ctx context.Context, ch *spiceboxv1alpha1.Channel) time.Duration {
	logger := log.FromContext(ctx).WithValues("channel", ch.Name, "namespace", ch.Namespace)

	k, ok := registry.Get(ch.Spec.Kind)
	if !ok {
		return 0 // unknown kind; the Valid condition already covers this
	}
	checker, canCheck := k.(channelkinds.WebhookURLDriftChecker)
	repointer, isRepointer := k.(channelkinds.WebhookURLRepointer)
	// Implementing the capability is not authority to use it: only the
	// provenance marker, stamped by the wizard whose own exchange registered
	// the upstream application, permits an outward-facing write. Checked once
	// here rather than in each kind, so a new kind cannot forget it.
	canRepoint := isRepointer && provisionedByThisTool(ch)
	if !canCheck && !canRepoint {
		return 0 // this kind's webhook, if any, isn't registered with a third party
	}

	expectedURL, known := r.expectedWebhookURL(ctx, logger, ch)
	if !known {
		return 0
	}
	if ch.Spec.CredentialsRef.SecretName == "" {
		return 0 // nothing to authenticate against the provider with
	}

	// The local comparison. No outbound read: a read could not change this
	// answer, because the address the provider SHOULD hold is derived here
	// and the address it was last given is recorded on status.
	if canRepoint && ch.Status.RepointedWebhookURL != expectedURL {
		secrets, ok := r.webhookSecrets(ctx, logger, ch)
		if !ok {
			return 0
		}
		return r.repointWebhookURL(ctx, logger, repointer, ch, secrets, expectedURL)
	}

	if !canCheck {
		return 0 // nothing further to verify; this kind can only be written to
	}

	// Throttle the outbound call itself — see webhookURLDriftCheckInterval's
	// doc. WebhookURLDriftCheckedAt is stamped below on every actual attempt
	// (success, match, or error alike), so a throttled tick here always
	// requeues for the exact remaining time rather than losing track of when
	// the next check is due.
	if last := ch.Status.WebhookURLDriftCheckedAt; last != nil {
		if elapsed := time.Since(last.Time); elapsed < webhookURLDriftCheckInterval {
			return webhookURLDriftCheckInterval - elapsed
		}
	}

	secrets, ok := r.webhookSecrets(ctx, logger, ch)
	if !ok {
		return 0
	}

	registeredURL, drifted, err := checker.CheckWebhookURLDrift(ctx, ch,
		secrets, expectedURL, r.GitHubAPIBaseURL)

	// Stamp the attempt regardless of outcome — this timestamp IS the
	// throttle's clock, so an error path must advance it too, or an
	// unreachable provider would be retried on every reconcile instead of
	// respecting the same interval as a successful check.
	now := metav1.Now()
	ch.Status.WebhookURLDriftCheckedAt = &now

	if err != nil {
		// A failure to REACH the provider is not the same fact as "no
		// drift" — Status=Unknown reports it distinctly rather than
		// silently reading as a clean check. Logged per the no-silent-
		// errors rule; also surfaced on status for an operator who isn't
		// tailing operator logs.
		logger.Info("webhook URL drift check could not reach the provider", "err", err.Error())
		conditions.Set(ch, &ch.Status.Conditions, metav1.Condition{
			Type:    spiceboxv1alpha1.ChannelConditionWebhookURLDrift,
			Status:  metav1.ConditionUnknown,
			Reason:  spiceboxv1alpha1.ReasonChannelWebhookProviderUnreachable,
			Message: fmt.Sprintf("could not read the registered webhook URL: %v", err),
		})
		return webhookURLDriftCheckInterval
	}

	if !drifted {
		conditions.SetFalse(ch, &ch.Status.Conditions,
			spiceboxv1alpha1.ChannelConditionWebhookURLDrift, spiceboxv1alpha1.ReasonChannelWebhookURLMatches, "")
		return webhookURLDriftCheckInterval
	}

	// Drift on an application this tool registered means it was changed at
	// the provider, behind this controller's back — the one case the local
	// comparison above cannot see. The read has already paid for itself;
	// correct it now rather than wait for the address to change again.
	if canRepoint {
		logger.Info("the provider's registered webhook URL was changed out of band; repointing it back",
			"registeredURL", registeredURL, "expectedURL", expectedURL)
		// Whichever is owed sooner wins: the read just ran, so the next one is
		// a full interval out, but a REFUSED write is owed its retry long
		// before that — dropping the write's own answer here would leave a
		// broken webhook unattended for six hours.
		return min(r.repointWebhookURL(ctx, logger, repointer, ch, secrets, expectedURL),
			webhookURLDriftCheckInterval)
	}

	msg := fmt.Sprintf(
		"the webhook URL registered on the provider (%q) does not match where this cluster serves webhooks (%q); "+
			"update it on the provider's own settings — this is never done automatically",
		registeredURL, expectedURL)
	if ch.Spec.GitHub != nil && ch.Spec.GitHub.AppSlug != "" {
		msg += fmt.Sprintf(". See https://github.com/settings/apps/%s", ch.Spec.GitHub.AppSlug)
	}
	conditions.Set(ch, &ch.Status.Conditions, metav1.Condition{
		Type:    spiceboxv1alpha1.ChannelConditionWebhookURLDrift,
		Status:  metav1.ConditionTrue,
		Reason:  spiceboxv1alpha1.ReasonChannelWebhookURLDrifted,
		Message: msg,
	})
	return webhookURLDriftCheckInterval
}

// repointWebhookURL writes expectedURL to the provider and records the
// outcome on the drift condition. On success the URL is remembered on status,
// which is what keeps the next reconcile from writing it again; on failure it
// is deliberately NOT remembered, so the write is retried rather than assumed
// to have landed.
func (r *Reconciler) repointWebhookURL(
	ctx context.Context,
	logger logr.Logger,
	repointer channelkinds.WebhookURLRepointer,
	ch *spiceboxv1alpha1.Channel,
	secrets channelkinds.WebhookSecrets,
	expectedURL string,
) time.Duration {
	if err := repointer.RepointWebhookURL(ctx, ch, secrets, expectedURL, r.GitHubAPIBaseURL); err != nil {
		// Surfaced on status as well as logged: this Channel's deliveries are
		// being lost until the write lands, and an operator who is not
		// tailing operator logs has no other way to learn that.
		logger.Info("repointing this Channel's webhook URL failed; deliveries keep going to the old address until it succeeds",
			"expectedURL", expectedURL, "err", err.Error())
		conditions.Set(ch, &ch.Status.Conditions, metav1.Condition{
			Type:    spiceboxv1alpha1.ChannelConditionWebhookURLDrift,
			Status:  metav1.ConditionTrue,
			Reason:  spiceboxv1alpha1.ReasonChannelWebhookRepointFailed,
			Message: fmt.Sprintf("could not repoint the webhook URL to %q: %v", expectedURL, err),
		})
		return webhookRepointRetryInterval
	}

	ch.Status.RepointedWebhookURL = expectedURL
	conditions.SetFalse(ch, &ch.Status.Conditions,
		spiceboxv1alpha1.ChannelConditionWebhookURLDrift,
		spiceboxv1alpha1.ReasonChannelWebhookURLRepointed,
		fmt.Sprintf("the webhook URL is registered as %q", expectedURL))
	logger.Info("repointed this Channel's webhook URL", "url", expectedURL)
	// Requeue on the drift interval, not immediately: the write is done, and
	// the next thing owed to this Channel is the periodic verification that
	// it is still what the provider holds.
	return webhookURLDriftCheckInterval
}

// provisionedByThisTool reports whether a run of THIS tool registered the
// upstream application the Channel talks to — the fact that authorizes an
// outward-facing write against it. The value is compared, not merely the
// key's presence: a marker written by some other provisioner names an
// application this controller still has no standing to change.
func provisionedByThisTool(ch *spiceboxv1alpha1.Channel) bool {
	return ch.Annotations[channelkinds.AnnotationAppProvisionedBy] == channelkinds.AppProvisionedByOAP
}

// webhookSecrets resolves the Channel's credentials Secret into the form a
// kind's webhook capabilities take. A Secret that cannot be read is logged
// and reported as unavailable rather than returned as an error: neither the
// drift check nor the repoint is worth failing a reconcile over, and the
// Valid condition already carries a missing Secret.
func (r *Reconciler) webhookSecrets(ctx context.Context, logger logr.Logger, ch *spiceboxv1alpha1.Channel) (channelkinds.WebhookSecrets, bool) {
	secretRef := types.NamespacedName{Namespace: ch.Namespace, Name: ch.Spec.CredentialsRef.SecretName}
	sec, err := r.SecretReader.Get(ctx, secretRef)
	if err != nil {
		logger.Info("webhook URL reconcile skipped: Secret unavailable", "secret", secretRef.String(), "err", err.Error())
		return channelkinds.WebhookSecrets{}, false
	}
	return channelkinds.WebhookSecrets{Data: sec.Data}, true
}

// expectedWebhookURL returns the absolute URL this cluster serves ch's inbound
// webhook at, and whether that address is known at all.
//
// A webd-targeting PublicEndpoint is the authority when one exists, because
// its status.url is the single source of truth for how this cluster is
// reached — and, crucially, ITS SILENCE IS AN ANSWER TOO. While such an
// endpoint is not Ready, the ConfigMap-derived ExternalBaseURL holds the
// endpoint's LOOPBACK address (the PublicEndpoint reconciler publishes
// spec.localURL in that state, deliberately, so webd's own links keep
// working). Falling back to it here would compare a provider's registration
// against 127.0.0.1 — reporting drift on every Channel — and, worse, offer
// that address as something to write to the provider, replacing a working
// webhook with one no delivery can ever reach. So a not-yet-Ready endpoint
// yields no answer at all and the whole reconcile stands down until it is up.
//
// With no such endpoint ExternalBaseURL is the answer — but only when it names
// an address a provider could deliver to. Two ways it does not, and neither is
// hypothetical: it can be EMPTY (an install that provisioned a Gateway whose
// host has not landed yet), where "expected" would be a bare path with no host
// that no provider-registered absolute URL can ever equal; and it can be a
// LOOPBACK, which is what a desktop carries whenever no PublicEndpoint exists —
// its policy opens one only on demand, so "no endpoint" there does not mean
// "real ingress". Both are skipped, for the reason the not-Ready case above
// gives.
func (r *Reconciler) expectedWebhookURL(ctx context.Context, logger logr.Logger, ch *spiceboxv1alpha1.Channel) (string, bool) {
	base, known := r.externalBaseURL(ctx, logger)
	if !known {
		return "", false
	}
	return strings.TrimSuffix(base, "/") +
		channelevents.WebhookPathFor(ch.Spec.Kind, ch.Namespace, ch.Name), true
}

// externalBaseURL resolves the base half of expectedWebhookURL. See its doc
// for the precedence and why each "unknown" answer is deliberate.
func (r *Reconciler) externalBaseURL(ctx context.Context, logger logr.Logger) (string, bool) {
	var endpoints spiceboxv1alpha1.PublicEndpointList
	if err := r.Client.List(ctx, &endpoints); err != nil {
		// Fail closed rather than fall back: an unreadable list cannot
		// distinguish "no endpoint, use the ConfigMap" from "an endpoint owns
		// this address and is mid-restart", and only one of those is safe to
		// write to a provider.
		logger.Info("webhook URL reconcile skipped: listing PublicEndpoints failed", "err", err.Error())
		return "", false
	}

	// PublicEndpoint is cluster-scoped and only a webd-targeting one serves
	// the webhook path at all. Two of them are a misconfiguration the
	// PublicEndpoint reconciler already reports; pick deterministically by
	// name so a cluster in that state does not flap a provider's registration
	// between two addresses on alternate reconciles.
	var chosen *spiceboxv1alpha1.PublicEndpoint
	for i := range endpoints.Items {
		e := &endpoints.Items[i]
		if !cloud.IsWebdTarget(e.Spec.Target.Namespace, e.Spec.Target.Service) {
			continue
		}
		if chosen == nil || e.Name < chosen.Name {
			chosen = e
		}
	}

	if chosen == nil {
		if r.ExternalBaseURL == nil || r.ExternalBaseURL() == "" {
			logger.Info("webhook URL reconcile skipped: no PublicEndpoint and no ExternalBaseURL configured")
			return "", false
		}
		base := r.ExternalBaseURL()
		// The ConfigMap this reads is not always a public address. A desktop
		// with no PublicEndpoint at all — its policy opens one only on demand —
		// carries http://127.0.0.1:<port> there, and this branch used to hand
		// that straight on: drift reported against a loopback on every Channel,
		// and, for a provenance-marked one, that address PATCHed onto the
		// provider and retried every drift interval, forever.
		//
		// So the same rule the not-Ready branch below applies is applied here:
		// an address nothing outside this machine can reach is not an answer,
		// and no answer stands the whole reconcile down rather than acting on a
		// wrong one.
		if why := channelkinds.UnreachableWebhookURL(base); why != "" {
			logger.Info("webhook URL reconcile skipped: this cluster's external base URL is not an address a provider can deliver to",
				"externalBaseURL", base, "why", why)
			return "", false
		}
		return base, true
	}

	if chosen.Status.Phase != spiceboxv1alpha1.PublicEndpointPhaseReady || chosen.Status.URL == "" {
		logger.Info("webhook URL reconcile skipped: this cluster's PublicEndpoint has no live URL yet",
			"publicendpoint", chosen.Name, "phase", chosen.Status.Phase)
		return "", false
	}
	return chosen.Status.URL, true
}

// classIsValid reports whether the AgentClass has Valid=True in its status.
func classIsValid(class *spiceboxv1alpha1.AgentClass) bool {
	c := meta.FindStatusCondition(class.Status.Conditions, spiceboxv1alpha1.AgentClassConditionValid)
	if c == nil {
		return false
	}
	return c.Status == metav1.ConditionTrue
}

func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&spiceboxv1alpha1.Channel{}).
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(secretToChannels(mgr.GetClient()))).
		Watches(&spiceboxv1alpha1.AgentClass{}, handler.EnqueueRequestsFromMapFunc(classToChannels(mgr.GetClient()))).
		Watches(&spiceboxv1alpha1.AgentIdentity{}, handler.EnqueueRequestsFromMapFunc(identityToChannels(mgr.GetClient()))).
		Watches(&spiceboxv1alpha1.Channel{}, handler.EnqueueRequestsFromMapFunc(outputChannelToInputChannels(mgr.GetClient()))).
		Watches(&spiceboxv1alpha1.PublicEndpoint{}, handler.EnqueueRequestsFromMapFunc(publicEndpointToChannels(mgr.GetClient()))).
		Complete(r)
}

// publicEndpointToChannels enqueues every Channel whose webhook registration
// could need to move when the cluster's public address changes.
//
// This watch is what makes repointing EVENT-DRIVEN. The address changes when
// a tunnel restarts — an unreserved provider domain is new on every open —
// and until the Channels are re-reconciled their provider still delivers to
// an address that no longer resolves. A poll would have to be either fast
// enough to spend a rate limit or slow enough to leave the agent unreachable
// for hours; the CR whose status IS the address is the right thing to watch.
// PublicEndpoint status is written only when it actually changes (see that
// reconciler), so this does not fire on resyncs.
//
// Only a webd-targeting endpoint publishes an address the webhook path is
// served at, and only a kind that can be repointed or drift-checked has
// anything to do here — everything else returns before the cluster-wide List.
func publicEndpointToChannels(c client.Client) handler.MapFunc {
	return func(ctx context.Context, obj client.Object) []reconcile.Request {
		pe, ok := obj.(*spiceboxv1alpha1.PublicEndpoint)
		if !ok || !cloud.IsWebdTarget(pe.Spec.Target.Namespace, pe.Spec.Target.Service) {
			return nil
		}
		// Cluster-wide: PublicEndpoint is cluster-scoped, and the Channels it
		// affects can be in any namespace.
		var channels spiceboxv1alpha1.ChannelList
		if err := c.List(ctx, &channels); err != nil {
			log.FromContext(ctx).Info("list Channels for PublicEndpoint watch failed; dropping re-enqueue (self-heals on next resync)",
				"publicendpoint", pe.Name, "err", err.Error())
			return nil
		}
		var reqs []reconcile.Request
		for _, ch := range channels.Items {
			k, known := registry.Get(ch.Spec.Kind)
			if !known {
				continue
			}
			_, isRepointer := k.(channelkinds.WebhookURLRepointer)
			_, isChecker := k.(channelkinds.WebhookURLDriftChecker)
			if !isRepointer && !isChecker {
				continue
			}
			reqs = append(reqs, reconcile.Request{
				NamespacedName: types.NamespacedName{Namespace: ch.Namespace, Name: ch.Name},
			})
		}
		return reqs
	}
}

// outputChannelToInputChannels enqueues the role=input Channels bound to the
// same AgentClass when a role=output Channel changes. An input Channel's
// validity depends on that sibling existing and being anchorable, so it must
// re-reconcile when the dependency appears, changes, or disappears.
func outputChannelToInputChannels(c client.Client) handler.MapFunc {
	return func(ctx context.Context, obj client.Object) []reconcile.Request {
		out, ok := obj.(*spiceboxv1alpha1.Channel)
		if !ok || out.Spec.Role != spiceboxv1alpha1.ChannelRoleOutput {
			return nil
		}
		var channels spiceboxv1alpha1.ChannelList
		if err := c.List(ctx, &channels, client.InNamespace(out.Namespace)); err != nil {
			log.FromContext(ctx).Info("list Channels for output-Channel watch failed; dropping re-enqueue (self-heals on next resync)",
				"channel", out.Name, "namespace", out.Namespace, "err", err.Error())
			return nil
		}
		var reqs []reconcile.Request
		for _, ch := range channels.Items {
			if ch.Spec.Role == spiceboxv1alpha1.ChannelRoleInput && ch.Spec.AgentClass == out.Spec.AgentClass {
				reqs = append(reqs, reconcile.Request{
					NamespacedName: types.NamespacedName{Namespace: ch.Namespace, Name: ch.Name},
				})
			}
		}
		return reqs
	}
}

// secretToChannels enqueues all Channels whose CredentialsRef.SecretName
// matches the given Secret in the Channel's namespace.
func secretToChannels(c client.Client) handler.MapFunc {
	return func(ctx context.Context, obj client.Object) []reconcile.Request {
		sec, ok := obj.(*corev1.Secret)
		if !ok {
			return nil
		}
		var channels spiceboxv1alpha1.ChannelList
		if err := c.List(ctx, &channels, client.InNamespace(sec.Namespace)); err != nil {
			log.FromContext(ctx).Info("list Channels for Secret watch failed; dropping re-enqueue (self-heals on next resync)",
				"secret", sec.Name, "namespace", sec.Namespace, "err", err.Error())
			return nil
		}
		var reqs []reconcile.Request
		for _, ch := range channels.Items {
			if ch.Spec.CredentialsRef.SecretName == sec.Name {
				reqs = append(reqs, reconcile.Request{
					NamespacedName: types.NamespacedName{Namespace: ch.Namespace, Name: ch.Name},
				})
			}
		}
		return reqs
	}
}

// identityToChannels enqueues all Channels whose AgentIdentity field matches
// the given AgentIdentity in the Channel's namespace.
//
// validate() fails a Channel closed at AgentIdentityMissing when the named
// AgentIdentity does not exist, and Reconcile returns a bare ctrl.Result{}
// on that path — no requeue. Without this watch a Channel applied ahead of
// its AgentIdentity (bundle install ordering, or an identity created by a
// later `oap identity setup`) stays Valid=False permanently, with no escape
// short of a manual poke or an operator restart.
func identityToChannels(c client.Client) handler.MapFunc {
	return func(ctx context.Context, obj client.Object) []reconcile.Request {
		ident, ok := obj.(*spiceboxv1alpha1.AgentIdentity)
		if !ok {
			return nil
		}
		var channels spiceboxv1alpha1.ChannelList
		if err := c.List(ctx, &channels, client.InNamespace(ident.Namespace)); err != nil {
			log.FromContext(ctx).Info("list Channels for AgentIdentity watch failed; dropping re-enqueue (self-heals on next resync)",
				"agentidentity", ident.Name, "namespace", ident.Namespace, "err", err.Error())
			return nil
		}
		var reqs []reconcile.Request
		for _, ch := range channels.Items {
			if ch.Spec.AgentIdentity == ident.Name {
				reqs = append(reqs, reconcile.Request{
					NamespacedName: types.NamespacedName{Namespace: ch.Namespace, Name: ch.Name},
				})
			}
		}
		return reqs
	}
}

// classToChannels enqueues all Channels whose AgentClass field matches.
func classToChannels(c client.Client) handler.MapFunc {
	return func(ctx context.Context, obj client.Object) []reconcile.Request {
		class, ok := obj.(*spiceboxv1alpha1.AgentClass)
		if !ok {
			return nil
		}
		var channels spiceboxv1alpha1.ChannelList
		if err := c.List(ctx, &channels, client.InNamespace(class.Namespace)); err != nil {
			log.FromContext(ctx).Info("list Channels for AgentClass watch failed; dropping re-enqueue (self-heals on next resync)",
				"agentclass", class.Name, "namespace", class.Namespace, "err", err.Error())
			return nil
		}
		var reqs []reconcile.Request
		for _, ch := range channels.Items {
			if ch.Spec.AgentClass == class.Name {
				reqs = append(reqs, reconcile.Request{
					NamespacedName: types.NamespacedName{Namespace: ch.Namespace, Name: ch.Name},
				})
			}
		}
		return reqs
	}
}
