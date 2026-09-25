package useridentity

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// PutTokenRequest is the input to PutToken.
type PutTokenRequest struct {
	// Subject is the canonical SpiceDB subject ("user:<base64(email)>").
	// metadata.name of the resulting UserIdentity is NameForSubject(Subject).
	Subject identity.Subject

	// CredentialName is the credential name as agents declare it (e.g.
	// "github-pat"). The master Secret is named
	// MasterSecretName(uiName, CredentialName).
	CredentialName string

	// Token is the bearer-token bytes to persist as data["token"].
	Token string

	// DisplayName is an optional human-friendly label written to
	// spec.displayName. Empty leaves the existing value untouched.
	DisplayName string

	// ProviderID and SubjectID are the link-time attestation: which provider
	// live-verified this credential, and the provider's STABLE id for the
	// account it authenticated as (builtins.VerifyResult.ProviderID /
	// .SubjectID). They are recorded on the master Secret for the UserIdentity
	// reconciler to turn into an identity edge — see
	// pkg/controllers/useridentity/attested_edge.go.
	//
	// A pair, never one alone: an id is stable only within its provider's
	// id-space, so half of it names nothing. Both empty is the ordinary case —
	// verification was skipped, the provider declares no subject id field, or
	// the check could not conclude — and records nothing rather than guessing.
	ProviderID string
	SubjectID  string
}

// PutToken upserts a static bearer credential on the UserIdentity for the given
// subject: it writes the token to the master Secret in IdentitiesNamespace, then
// upserts the credential entry on the cluster-scoped UserIdentity. Idempotent —
// a repeat with the same subject + credential just replaces the Secret value.
// Returns a non-nil error on any sub-step failure; partial-success states are not
// exposed, and the caller can retry.
//
// `oap user-identity put-token` and identityd's /link/submit handler both go
// through here, so they produce indistinguishable cluster state. That is also
// why the link-time attestation is recorded HERE and not at each caller: every
// route a human links a credential through lands in this function, so a new one
// gets the attestation without having to remember it.
func PutToken(ctx context.Context, c client.Client, req PutTokenRequest) error {
	if req.Subject == "" || req.CredentialName == "" || req.Token == "" {
		return fmt.Errorf("useridentity.PutToken: subject, credentialName, and token are required")
	}

	name := NameForSubject(req.Subject)
	secretName := MasterSecretName(name, req.CredentialName)

	// 1. Master Secret.
	var sec corev1.Secret
	secKey := client.ObjectKey{Namespace: spiceboxv1alpha1.IdentitiesNamespace, Name: secretName}
	switch err := c.Get(ctx, secKey, &sec); {
	case apierrors.IsNotFound(err):
		sec = corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: spiceboxv1alpha1.IdentitiesNamespace},
			Type:       corev1.SecretTypeOpaque,
			Data:       map[string][]byte{"token": []byte(req.Token)},
		}
		SetAttestation(&sec, req.ProviderID, req.SubjectID)
		if err := c.Create(ctx, &sec); err != nil {
			return fmt.Errorf("create master secret: %w", err)
		}
	case err != nil:
		return fmt.Errorf("get master secret: %w", err)
	default:
		if sec.Data == nil {
			sec.Data = map[string][]byte{}
		}
		sec.Data["token"] = []byte(req.Token)
		// Record WHICH provider account this value authenticated as. A no-op
		// when either half is empty, so a re-link the provider could not check
		// leaves the previous attestation standing rather than erasing it.
		SetAttestation(&sec, req.ProviderID, req.SubjectID)
		if err := c.Update(ctx, &sec); err != nil {
			return fmt.Errorf("update master secret: %w", err)
		}
	}

	// 2. UserIdentity.
	cred := spiceboxv1alpha1.AgentCredential{
		Name: req.CredentialName,
		Type: "static",
		Static: &spiceboxv1alpha1.StaticCredentialSource{
			SecretRef: spiceboxv1alpha1.SecretKeyRef{Name: secretName, Key: "token"},
		},
	}
	var ui spiceboxv1alpha1.UserIdentity
	switch err := c.Get(ctx, client.ObjectKey{Name: name}, &ui); {
	case apierrors.IsNotFound(err):
		ui = spiceboxv1alpha1.UserIdentity{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: spiceboxv1alpha1.UserIdentitySpec{
				Subject:     req.Subject.String(),
				DisplayName: req.DisplayName,
				Credentials: []spiceboxv1alpha1.AgentCredential{cred},
			},
		}
		stampRotationFingerprint(&ui, req.CredentialName, req.Token)
		if err := c.Create(ctx, &ui); err != nil {
			return fmt.Errorf("create UserIdentity: %w", err)
		}
	case err != nil:
		return fmt.Errorf("get UserIdentity: %w", err)
	default:
		ui.Spec.Subject = req.Subject.String()
		if req.DisplayName != "" {
			ui.Spec.DisplayName = req.DisplayName
		}
		ui.Spec.Credentials = UpsertCredential(ui.Spec.Credentials, cred)
		stampRotationFingerprint(&ui, req.CredentialName, req.Token)
		if err := c.Update(ctx, &ui); err != nil {
			return fmt.Errorf("update UserIdentity: %w", err)
		}
	}
	return nil
}

// UpsertCredential replaces the credential with matching Name in creds, or
// appends c when no match exists. Exported so the `oap` CLI's local helpers stay
// aligned with this package's notion of upsert.
func UpsertCredential(creds []spiceboxv1alpha1.AgentCredential, c spiceboxv1alpha1.AgentCredential) []spiceboxv1alpha1.AgentCredential {
	for i := range creds {
		if creds[i].Name == c.Name {
			creds[i] = c
			return creds
		}
	}
	return append(creds, c)
}

// RotationFingerprintAnnotation carries a fingerprint of the most recently linked
// credential's (name, value) pair. PutToken and PutOAuthToken restamp it on every
// link so a same-name re-link with a DIFFERENT value changes the UserIdentity
// object: the credential Spec entry references the master Secret by deterministic
// name only, never by value, so without this a value-only rotation is a no-op
// Update that never fires the AgentSession UserIdentity watch. With it, the
// changed annotation is a real write, the watch re-enqueues the subject's
// non-terminal sessions, and the passthrough reconcile re-projects and emits a
// per-session credential invalidation.
const RotationFingerprintAnnotation = "useridentity.agentprimitives.authzed.com/rotation-fingerprint"

// stampRotationFingerprint records a fingerprint of credName + valueMaterial in
// ui's RotationFingerprintAnnotation — PutToken passes the bearer token,
// PutOAuthToken the access+refresh pair — so a same-name re-link with a CHANGED
// value produces a real UserIdentity write. Idempotent: identical inputs yield
// the same annotation, so a same-value re-link stays a no-op.
func stampRotationFingerprint(ui *spiceboxv1alpha1.UserIdentity, credName, valueMaterial string) {
	if ui.Annotations == nil {
		ui.Annotations = map[string]string{}
	}
	ui.Annotations[RotationFingerprintAnnotation] = credValueFingerprint(credName, valueMaterial)
}

// credValueFingerprint is a non-reversible SHA-256 over the credential name and
// value material. It never carries the value itself; a hash of a high-entropy
// token is not brute-forceable, so it is safe to store on the cluster object.
func credValueFingerprint(credName, valueMaterial string) string {
	sum := sha256.Sum256([]byte(credName + "\x00" + valueMaterial))
	return hex.EncodeToString(sum[:])
}
