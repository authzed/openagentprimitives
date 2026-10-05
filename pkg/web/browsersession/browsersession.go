// Package browsersession creates the cluster objects a browser-channel
// AgentSession is made of: the ephemeral Channel, its owner-ref'd credentials
// Secret, and the AgentSession itself, plus the SpiceDB started_by relation
// that gives the session a resolvable owner.
//
// Two surfaces start such a session — the built-in web chat and the agent-UI
// page's start path — and its object shape is read back by callers neither
// owns: channelsd's inbound correlation (channel-key label), the operator's
// owner resolver (started_by annotation), the chat registry's dormant-session
// listing (channel-kind label). One writer keeps those readers honest.
package browsersession

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/go-logr/logr"
	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkey"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/browser"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// ErrUnknownAgentClass covers both "the named AgentClass does not exist" (a
// genuine NotFound) and "it exists but has not reached Valid=True". One
// sentinel, because the caller's remedy is the same and the distinction is not
// the viewer's business; the wrapped message separates them for the operator.
//
// It is deliberately NOT returned for a Get that FAILED — throttling, a
// timeout, an RBAC refusal, a webhook stall. Folding those in makes a caller
// answer "this agent is not ready" when the class is fine and the read failed.
// A caller must be able to tell "the answer is no" from "there was no answer".
//
// Because a transient failure is NOT this sentinel, a caller may answer it with
// a 500 without weakening the enumeration-oracle property that makes an unknown
// class and an unauthorized one share one refusal message
// (pkg/web/webui/sessions/start.go): a 500 says a read failed, and nothing
// about whether the named class exists.
var ErrUnknownAgentClass = errors.New("unknown or not-yet-valid agent class")

// StartChecker answers the class start gate for the viewer. Satisfied by
// *spicedb.Client (CheckAgentClassStart). Returned as the INTERFACE by Deps
// so a nil check is honest.
type StartChecker interface {
	CheckAgentClassStart(ctx context.Context, ns, name, permission string, canonicalID identity.CanonicalUserID) (bool, error)
}

// ErrNotAnAllowedStarter reports that the class declares an allowlist the
// viewer is not on. Callers map it to the SAME copy as an unknown class, so a
// refusal cannot be used to enumerate which classes exist.
var ErrNotAnAllowedStarter = errors.New("not an allowed starter of this agent class")

// Deps is what Create needs from its host process.
//
// Authz returns an INTERFACE. A construction site that assigns a typed-nil
// *spicedb.Client into it produces a non-nil interface whose method call
// panics — see AGENTS.md's typed-nil rule. Declare the backing field as
// authz.Granter and assign it only once a real client exists.
type Deps interface {
	K8s() client.Client
	Authz() authz.Granter
	Logger() logr.Logger
	StartChecker() StartChecker
}

// Params is one start request.
type Params struct {
	// Namespace is where every object is created. Required.
	Namespace string
	// AgentClass names the class in Namespace. Create Gets it — for its UID
	// (the Channel's owner reference) and its Valid condition — so the caller
	// need not, and the two cannot disagree about readiness.
	AgentClass string
	// Prompt is the opening user message, landing on spec.prompt.inline.
	// REQUIRED and non-blank: runner.ResolvePrompt errors unless exactly one
	// of inline/configMapRef is set, so an empty prompt fails at startup rather
	// than waiting for input. Create never invents one — platform-authored text
	// in an agent's turn zero is not this package's to write.
	Prompt string
	// Subject is the viewer's canonical SpiceDB subject ("user:<canonical>"),
	// as webui.SubjectFromContext yields it. Required.
	Subject identity.Subject
	// SessionName lets a caller that must reserve the name BEFORE the create
	// (the chat registry reserves a slot atomically with its concurrency cap)
	// supply it. Empty means Create generates one via NewSessionName.
	SessionName string
}

// Created is everything the caller needs to keep working with the session it
// just started. The class and created objects are returned with their
// API-server UIDs and resourceVersions populated.
type Created struct {
	// Class is the live class used to authorize and shape this creation.
	Class     *spiceboxv1alpha1.AgentClass
	Session   *spiceboxv1alpha1.AgentSession
	Channel   *spiceboxv1alpha1.Channel
	Creds     *corev1.Secret
	Identity  channelkinds.ExternalIdentity
	Principal identity.Principal
	// Owner is the subject Create stamped on AnnotationStartedByCanonicalID
	// and wrote to started_by, derived from Params.Subject via DeriveIdentity.
	// Ownership checks must compare against THIS, not Params.Subject, which
	// has not been through the canonicalization round trip.
	Owner identity.Subject
}

// StartFunc is Create with its Deps already bound. Surfaces hold this rather
// than a Deps: it is a FUNC type, so a nil check on it is honest (an
// interface-typed collaborator can be a non-nil interface wrapping a nil
// pointer), and a surface whose host cannot start sessions simply has none.
type StartFunc func(ctx context.Context, p Params) (Created, error)

// Create writes the Channel, its credentials Secret, and the AgentSession,
// then writes the started_by relation.
//
// It returns as soon as those exist and does NOT wait for readiness — the
// operator has not yet minted the per-session memory-token Secret and no runner
// pod is scheduled. A caller needing readiness must wait for it itself.
//
// Rollback has one rule: BEFORE the AgentSession create succeeds, a failure
// best-effort deletes the Channel and Secret so no orphan is left. AFTER it,
// nothing is deleted — the session is live and its lifecycle belongs to the
// operator, which may legitimately park it AwaitingCredentials or bring it up
// slowly. Deleting a created session on a later error is how a session vanishes
// out from under the user.
func Create(ctx context.Context, d Deps, p Params) (_ Created, retErr error) {
	if d == nil {
		return Created{}, errors.New("browsersession: nil Deps")
	}
	k8s := d.K8s()
	if k8s == nil {
		return Created{}, errors.New("browsersession: nil K8s client")
	}
	granter := d.Authz()
	if granter == nil {
		return Created{}, errors.New("browsersession: nil Authz (Granter) dependency")
	}
	if p.Namespace == "" {
		return Created{}, errors.New("browsersession: Namespace is required")
	}
	if strings.TrimSpace(p.Prompt) == "" {
		return Created{}, errors.New("browsersession: Prompt is required")
	}

	ext, principal, err := DeriveIdentity(p.Subject)
	if err != nil {
		return Created{}, fmt.Errorf("derive browser-session identity: %w", err)
	}
	canonical, err := principal.Canonical()
	if err != nil {
		return Created{}, fmt.Errorf("canonicalize browser-session principal: %w", err)
	}
	owner := canonical.Subject()

	var ac spiceboxv1alpha1.AgentClass
	if err := k8s.Get(ctx, client.ObjectKey{Namespace: p.Namespace, Name: p.AgentClass}, &ac); err != nil {
		// Only a NotFound is a verdict on the class. Anything else — throttling,
		// a timeout, an RBAC refusal, a webhook stall — is the control plane
		// failing to answer, and returning ErrUnknownAgentClass for it makes
		// every caller tell the viewer their agent is broken and log that the
		// class is not ready, both of which are false. The cause is wrapped so
		// the caller's log carries it.
		if !apierrors.IsNotFound(err) {
			return Created{}, fmt.Errorf("get agent class %q in %q: %w", p.AgentClass, p.Namespace, err)
		}
		return Created{}, fmt.Errorf("%w: %q", ErrUnknownAgentClass, p.AgentClass)
	}
	if !AgentClassReady(&ac) {
		return Created{}, fmt.Errorf("%w: %q is not ready", ErrUnknownAgentClass, p.AgentClass)
	}

	// The start gate, asked here so a refused person gets an immediate answer
	// rather than a session that fails a moment later. The AgentSession
	// reconciler asks again (it is the invariant for every entry path); this
	// is the courtesy, not the enforcement.
	if perm := ac.StartGatePermission(); perm != "" {
		chk := d.StartChecker()
		if chk == nil {
			return Created{}, fmt.Errorf("%w: no start checker configured", ErrNotAnAllowedStarter)
		}
		ok, err := chk.CheckAgentClassStart(ctx, p.Namespace, p.AgentClass, perm, canonical)
		if err != nil {
			return Created{}, fmt.Errorf("browsersession: start gate for %s/%s: %w", p.Namespace, p.AgentClass, err)
		}
		if !ok {
			return Created{}, fmt.Errorf("%w: %s on %q", ErrNotAnAllowedStarter, perm, p.AgentClass)
		}
	}

	sessionName := p.SessionName
	if sessionName == "" {
		sessionName = NewSessionName(p.AgentClass)
	}

	// sessionCreated latches true the instant the AgentSession Create succeeds,
	// gating the rollback defers below: before it, the partial Channel/creds
	// infra is deleted to avoid an orphan; after it, nothing is — not the
	// session, nor the Channel it owner-ref-cascades from.
	sessionCreated := false

	ch := &spiceboxv1alpha1.Channel{
		TypeMeta: metav1.TypeMeta{
			APIVersion: spiceboxv1alpha1.SchemeBuilder.GroupVersion.String(),
			Kind:       "Channel",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      sessionName + "-chan",
			Namespace: p.Namespace,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: spiceboxv1alpha1.SchemeBuilder.GroupVersion.String(),
				Kind:       "AgentClass",
				Name:       ac.Name,
				UID:        ac.UID,
			}},
		},
		Spec: spiceboxv1alpha1.ChannelSpec{
			Kind:           browser.KindName,
			Role:           spiceboxv1alpha1.ChannelRoleBoth,
			AgentClass:     ac.Name,
			SessionScope:   "user",
			CredentialsRef: spiceboxv1alpha1.ChannelCredentialsRef{SecretName: sessionName + "-chan-creds"},
		},
	}
	if err := k8s.Create(ctx, ch); err != nil {
		return Created{}, fmt.Errorf("create browser Channel: %w", err)
	}
	defer func() {
		if retErr != nil && !sessionCreated {
			if err := k8s.Delete(context.Background(), ch); err != nil && !apierrors.IsNotFound(err) {
				d.Logger().Info("browsersession: rollback delete Channel failed", "channel", ch.Namespace+"/"+ch.Name, "err", err.Error())
			}
		}
	}()
	// Re-Get so ch.UID is populated for the creds Secret's + the
	// AgentSession's owner refs.
	if err := k8s.Get(ctx, client.ObjectKey{Namespace: p.Namespace, Name: ch.Name}, ch); err != nil {
		return Created{}, fmt.Errorf("re-get browser Channel for UID: %w", err)
	}

	// creds is OWNED by the Channel, not merely referenced by
	// spec.credentialsRef.secretName, so owner-reference GC cascades its
	// deletion even when a hard crash skips explicit teardown. Without it the
	// Secret's only persistent ancestor is the never-deleted AgentClass.
	creds := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ch.Spec.CredentialsRef.SecretName,
			Namespace: p.Namespace,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: spiceboxv1alpha1.SchemeBuilder.GroupVersion.String(),
				Kind:       "Channel",
				Name:       ch.Name,
				UID:        ch.UID,
			}},
		},
	}
	if err := k8s.Create(ctx, creds); err != nil {
		return Created{}, fmt.Errorf("create chat creds Secret: %w", err)
	}
	defer func() {
		if retErr != nil && !sessionCreated {
			if err := k8s.Delete(context.Background(), creds); err != nil && !apierrors.IsNotFound(err) {
				d.Logger().Info("browsersession: rollback delete creds Secret failed", "secret", creds.Namespace+"/"+creds.Name, "err", err.Error())
			}
		}
	}()

	key := browser.ChannelKey(p.Namespace, ch.Name)
	sess := &spiceboxv1alpha1.AgentSession{
		TypeMeta: metav1.TypeMeta{
			APIVersion: spiceboxv1alpha1.SchemeBuilder.GroupVersion.String(),
			Kind:       "AgentSession",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      sessionName,
			Namespace: p.Namespace,
			Labels: map[string]string{
				spiceboxv1alpha1.LabelChannelName: ch.Name,
				spiceboxv1alpha1.LabelChannelKind: browser.KindName,
				spiceboxv1alpha1.LabelChannelKey:  channelkey.LabelValue(key),
			},
			Annotations: map[string]string{
				spiceboxv1alpha1.AnnotationStartedByExternalID:  ext.ExternalID.String(),
				spiceboxv1alpha1.AnnotationStartedByCanonicalID: owner.String(),
				// Always the authenticated idp email (DeriveIdentity sets both
				// ExternalID and Email to it). Feeds IdentityChoiceGate.requester()
				// so an ask|dynamic session's identity prompt canonicalizes to the
				// same subject the requester's own click does.
				spiceboxv1alpha1.AnnotationStartedByEmail: ext.Email.String(),
			},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: spiceboxv1alpha1.SchemeBuilder.GroupVersion.String(),
				Kind:       "Channel",
				Name:       ch.Name,
				UID:        ch.UID,
			}},
		},
		Spec: spiceboxv1alpha1.AgentSessionSpec{
			Class:  ac.Name,
			Prompt: spiceboxv1alpha1.PromptSource{Inline: p.Prompt},
			InputChannel: &spiceboxv1alpha1.ChannelBinding{
				Name:              ch.Name,
				Kind:              browser.KindName,
				Key:               key,
				Capabilities:      (&browser.Kind{}).Capabilities(),
				NATSSubjectPrefix: channelevents.SubjectPrefix(p.Namespace, sessionName),
			},
		},
	}
	if err := k8s.Create(ctx, sess); err != nil {
		return Created{}, fmt.Errorf("create AgentSession: %w", err)
	}
	// The session now exists: latch so the Channel/creds rollback defers no
	// longer fire. There is deliberately NO AgentSession rollback defer — from
	// here every error path leaves the object intact for the operator.
	sessionCreated = true

	if err := granter.TouchStartedBy(ctx, p.Namespace, sessionName, canonical); err != nil {
		return Created{}, fmt.Errorf("write started_by relationship: %w", err)
	}

	return Created{
		Class:     &ac,
		Session:   sess,
		Channel:   ch,
		Creds:     creds,
		Identity:  ext,
		Principal: principal,
		Owner:     owner,
	}, nil
}

// DeriveIdentity maps a canonical webd subject to the external identity and
// principal a browser-channel session is stamped with. webd is IdP-backed, so
// the subject always decodes to a verified email; ExternalID and Email are
// both that email, and Kind is identity.KindIdP.
func DeriveIdentity(subject identity.Subject) (channelkinds.ExternalIdentity, identity.Principal, error) {
	email := identity.DecodeForDisplay(subject.String())
	ext := channelkinds.ExternalIdentity{Kind: "idp", Email: identity.Email(email), ExternalID: identity.RawExternalID(email)}
	const emailVerified = true // subject is always a verified-email Principal from identityd (see doc above)
	principal := identity.IdPUser(identity.Email(email), emailVerified, "" /* displayName */)
	// Validated here rather than left to the caller: a principal that cannot
	// canonicalize (reachable only from a malformed/empty subject) is not a
	// usable identity, and folding the check in gives one derivation with one
	// error path instead of two half-derivations.
	if _, err := principal.Canonical(); err != nil {
		return channelkinds.ExternalIdentity{}, identity.Principal{}, err
	}
	return ext, principal, nil
}

// NewSessionName returns "<agentClass>-<8 hex>". Exported so a caller that
// must reserve the name before creating can generate the same shape.
func NewSessionName(agentClass string) string {
	return fmt.Sprintf("%s-%s", agentClass, uuid.New().String()[:8])
}

// AgentClassReady reports whether ac carries Valid=True. Exported because a
// caller listing classes for a chooser needs the same predicate Create
// enforces, and two spellings of "ready" is how a list offers a class that
// then fails to start.
func AgentClassReady(ac *spiceboxv1alpha1.AgentClass) bool {
	c := meta.FindStatusCondition(ac.Status.Conditions, spiceboxv1alpha1.AgentClassConditionValid)
	return c != nil && c.Status == metav1.ConditionTrue
}
