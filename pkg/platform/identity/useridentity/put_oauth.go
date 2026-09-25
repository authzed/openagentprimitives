package useridentity

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/adoptguard"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/refresh"
)

// PutOAuthTokenRequest carries everything the OAuth callback handler
// learned from the provider's token-exchange response.
type PutOAuthTokenRequest struct {
	// Subject is the canonical SpiceDB subject ("user:<...>").
	// metadata.name of the resulting UserIdentity is NameForSubject(Subject).
	Subject identity.Subject

	// CredentialName is the credential name as agents declare it (e.g.
	// "linear-oauth"). The master Secret is named
	// MasterSecretName(uiName, CredentialName).
	CredentialName string

	// AccessToken is the current OAuth access token. Required.
	AccessToken string

	// RefreshToken is the long-lived refresh token. May be empty if the
	// provider does not issue one; absent from the Secret when empty.
	RefreshToken string

	// ExpiresAt is the expiry as a Unix epoch seconds value. Zero means
	// "unknown / never expires"; the expires_at key is omitted from the
	// Secret when zero (RFC 6749 §5.1: expires_in is OPTIONAL).
	ExpiresAt int64

	// TokenType is typically "Bearer". Written when non-empty.
	TokenType string

	// Scope is the space-separated granted scopes string. May be empty;
	// absent from the Secret when empty.
	Scope string

	// TokenEndpoint is the provider's RFC 6749 token endpoint, discovered during
	// the authorization flow. Required whenever RefreshToken is set: the refresh
	// Secret is its only durable home, and pkg/platform/identity/refresh reads it
	// from there at every refresh. Absent from that Secret when empty.
	TokenEndpoint string

	// ClientID is the OAuth client the tokens were issued to — for a
	// dynamically-registered client (RFC 7591), the one DCR minted for this link.
	// Presented on every subsequent refresh, so it is persisted alongside the
	// tokens rather than left in the flow's transient state. Absent when empty.
	ClientID string

	// ClientSecret is the client's secret, for confidential clients only. Public
	// (PKCE-only) clients have none and omit the key entirely; a confidential
	// client that omits it gets invalid_client at refresh time. Absent when empty.
	ClientSecret string
}

// PutOAuthToken upserts a master Secret in the multi-key OAuth shape that
// pkg/controllers/useridentity/refresh_controller.go reads, then upserts
// the UserIdentity catalog entry as a type=oauth credential. Mirrors
// PutToken's semantics for PAT credentials.
//
// The credential is written as TWO Secrets, because Kubernetes RBAC cannot
// scope a grant to individual keys:
//
//   - the MASTER (MasterSecretName) holds what a consumer of the credential
//     needs — access_token (always), token_type / refresh_token / scope (when
//     non-empty), expires_at (RFC 3339, when ExpiresAt != 0). A userPassthrough
//     session's runner is granted read on this one.
//   - the REFRESH sibling (refresh.MaterialSecretName) holds the redemption
//     material — token_endpoint / client_id / client_secret, each when non-empty
//     — which only a refresher ever needs. Nothing grants the runner this Secret,
//     so a compromised runner reads a refresh token it has no endpoint, client
//     identity or authenticator to redeem.
//
// Idempotent: repeated calls with the same subject + credential replace both
// Secrets' values and the UserIdentity entry in place.
func PutOAuthToken(ctx context.Context, c client.Client, req PutOAuthTokenRequest) error {
	if req.Subject == "" || req.CredentialName == "" || req.AccessToken == "" {
		return fmt.Errorf("useridentity.PutOAuthToken: subject, credentialName, and accessToken are required")
	}
	// Fail closed on the one combination that is never correct: a refresh token
	// with nowhere to redeem it. The sibling Secret is the only durable home for
	// the token endpoint — the discovered value otherwise lives only in the flow's
	// single-use in-process state — so persisting the pair without it yields a
	// credential that can never be refreshed: parked at Refresh=False, every use
	// failing ErrExpired, recoverable only by a manual re-link. Refusing at link
	// time makes the unsafe combination unrepresentable.
	if req.RefreshToken != "" && req.TokenEndpoint == "" {
		return fmt.Errorf("useridentity.PutOAuthToken: %q carries a refreshToken but no tokenEndpoint; "+
			"the credential could never be refreshed", req.CredentialName)
	}

	uiName := NameForSubject(req.Subject)
	secretName := MasterSecretName(uiName, req.CredentialName)

	// Build Secret data — only include optional keys when non-empty / non-zero.
	data := map[string][]byte{
		"access_token": []byte(req.AccessToken),
	}
	if req.TokenType != "" {
		data["token_type"] = []byte(req.TokenType)
	}
	if req.RefreshToken != "" {
		data["refresh_token"] = []byte(req.RefreshToken)
	}
	if req.ExpiresAt != 0 {
		// refresh_controller.go parses expires_at with time.RFC3339.
		t := time.Unix(req.ExpiresAt, 0).UTC()
		data["expires_at"] = []byte(t.Format(time.RFC3339))
	}
	if req.Scope != "" {
		data["scope"] = []byte(req.Scope)
	}
	// Redemption material goes to the sibling Secret, NOT here — see
	// refresh.MaterialSecretName. pkg/platform/identity/refresh reads it from
	// there on every refresh; without it the credential is write-once.
	redemption := map[string][]byte{}
	if req.TokenEndpoint != "" {
		redemption["token_endpoint"] = []byte(req.TokenEndpoint)
	}
	if req.ClientID != "" {
		redemption["client_id"] = []byte(req.ClientID)
	}
	if req.ClientSecret != "" {
		redemption["client_secret"] = []byte(req.ClientSecret)
	}

	// 1. Master Secret.
	var sec corev1.Secret
	secKey := client.ObjectKey{Namespace: spiceboxv1alpha1.IdentitiesNamespace, Name: secretName}
	switch err := c.Get(ctx, secKey, &sec); {
	case apierrors.IsNotFound(err):
		sec = corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      secretName,
				Namespace: spiceboxv1alpha1.IdentitiesNamespace,
			},
			Type: corev1.SecretTypeOpaque,
			Data: data,
		}
		if err := c.Create(ctx, &sec); err != nil {
			return fmt.Errorf("useridentity.PutOAuthToken: create master secret: %w", err)
		}
	case err != nil:
		return fmt.Errorf("useridentity.PutOAuthToken: get master secret: %w", err)
	default:
		// The mirror image of putRefreshSecret's refusal, for the same reason:
		// credential names are free-form, so this master's name can equal
		// refresh.MaterialSecretName of ANOTHER credential's master. Which write
		// must be refused depends only on link order, so both check. Overwriting
		// would keep the sibling's Labels and OwnerReferences, leaving this
		// credential's tokens in a Secret labelled as the other credential's refresh
		// material and GC-owned by its master — and that credential's next re-link
		// would write its redemption material into a Secret the UserIdentity lists
		// as THIS credential's oauth SecretRef, which the passthrough runner's Role
		// names.
		if _, ok := sec.Labels[refresh.MaterialSecretLabel]; ok {
			return fmt.Errorf("useridentity.PutOAuthToken: Secret %q already exists and is another "+
				"credential's OAuth refresh material; credential %q cannot use it — rename the "+
				"colliding credential", secretName, req.CredentialName)
		}
		// Wholesale replace, which also strips any redemption key co-located in the
		// master, so a re-link migrates the credential onto the split shape.
		sec.Data = data
		if err := c.Update(ctx, &sec); err != nil {
			return fmt.Errorf("useridentity.PutOAuthToken: update master secret: %w", err)
		}
	}

	// 2. Refresh sibling. Written after the master so it can carry an
	//    ownerReference to it: unlinking the credential deletes the master, and
	//    owner-ref GC reaps the redemption material with it rather than leaving a
	//    DCR client secret behind for a credential that no longer exists.
	if err := putRefreshSecret(ctx, c, &sec, redemption); err != nil {
		return err
	}

	// 3. UserIdentity catalog entry.
	cred := spiceboxv1alpha1.AgentCredential{
		Name: req.CredentialName,
		Type: "oauth",
		OAuth: &spiceboxv1alpha1.OAuthCredentialSource{
			SecretRef: spiceboxv1alpha1.SecretRef{Name: secretName},
		},
	}
	var ui spiceboxv1alpha1.UserIdentity
	switch err := c.Get(ctx, client.ObjectKey{Name: uiName}, &ui); {
	case apierrors.IsNotFound(err):
		ui = spiceboxv1alpha1.UserIdentity{
			ObjectMeta: metav1.ObjectMeta{Name: uiName},
			Spec: spiceboxv1alpha1.UserIdentitySpec{
				Subject:     req.Subject.String(),
				Credentials: []spiceboxv1alpha1.AgentCredential{cred},
			},
		}
		stampRotationFingerprint(&ui, req.CredentialName, req.AccessToken+"\x00"+req.RefreshToken)
		if err := c.Create(ctx, &ui); err != nil {
			return fmt.Errorf("useridentity.PutOAuthToken: create UserIdentity: %w", err)
		}
	case err != nil:
		return fmt.Errorf("useridentity.PutOAuthToken: get UserIdentity: %w", err)
	default:
		ui.Spec.Credentials = UpsertCredential(ui.Spec.Credentials, cred)
		// Re-authorizing the SAME credential name yields a byte-identical Spec
		// entry (SecretRef is name-only), so without a fingerprint stamp this
		// Update is a no-op that never fires the AgentSession UserIdentity watch.
		stampRotationFingerprint(&ui, req.CredentialName, req.AccessToken+"\x00"+req.RefreshToken)
		if err := c.Update(ctx, &ui); err != nil {
			return fmt.Errorf("useridentity.PutOAuthToken: update UserIdentity: %w", err)
		}
	}
	return nil
}

// putRefreshSecret upserts the sibling Secret holding master's redemption
// material, or deletes it when there is none — a provider that issues no
// refreshable client identity, or a re-link that dropped one, where a stale
// sibling would keep presenting a client the credential is no longer bound to.
//
// The sibling carries an ownerReference to the master so unlinking the credential
// reaps its redemption material, plus the two labels markRefreshMaterial explains.
func putRefreshSecret(ctx context.Context, c client.Client, master *corev1.Secret, redemption map[string][]byte) error {
	name := refresh.MaterialSecretName(master.Name)
	key := client.ObjectKey{Namespace: master.Namespace, Name: name}

	var sib corev1.Secret
	err := c.Get(ctx, key, &sib)
	switch {
	case apierrors.IsNotFound(err):
		if len(redemption) == 0 {
			return nil // nothing to store, nothing to clean up
		}
		sib = corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:            name,
				Namespace:       master.Namespace,
				OwnerReferences: ownedByMaster(master),
			},
			Type: corev1.SecretTypeOpaque,
			Data: redemption,
		}
		markRefreshMaterial(&sib)
		if err := c.Create(ctx, &sib); err != nil {
			return fmt.Errorf("useridentity.PutOAuthToken: create refresh secret %q "+
				"(the credential is stored but cannot be refreshed without it): %w", name, err)
		}
	case err != nil:
		return fmt.Errorf("useridentity.PutOAuthToken: get refresh secret %q: %w", name, err)
	default:
		// Credential names are free-form, so the derived name can collide with
		// the master Secret of a credential literally named "<other>-refresh".
		// Refuse rather than overwrite: writing here would destroy that
		// credential, and its ownerReference would then GC it on unlink.
		if _, ok := sib.Labels[refresh.MaterialSecretLabel]; !ok {
			return fmt.Errorf("useridentity.PutOAuthToken: Secret %q already exists and is not OAuth "+
				"refresh material; credential %q cannot use it — rename the colliding credential",
				name, master.Name)
		}
		if len(redemption) == 0 {
			if err := c.Delete(ctx, &sib); err != nil && !apierrors.IsNotFound(err) {
				return fmt.Errorf("useridentity.PutOAuthToken: delete stale refresh secret %q: %w", name, err)
			}
			return nil
		}
		sib.Data = redemption
		markRefreshMaterial(&sib)
		// Re-point the ownerReference rather than leave whatever is there.
		// Unlink-then-relink (identityd's portal Replace, `oap identity delete-token`
		// plus a fresh auth) deletes the master and re-Creates it with a NEW UID,
		// and owner-ref GC may not have reaped the sibling yet. A ref left pointing
		// at the dead UID has GC delete the material this call just wrote, and every
		// refresh fails until another re-link.
		sib.OwnerReferences = ownedByMaster(master)
		if err := c.Update(ctx, &sib); err != nil {
			return fmt.Errorf("useridentity.PutOAuthToken: update refresh secret %q "+
				"(the credential is stored but cannot be refreshed without it): %w", name, err)
		}
	}
	return nil
}

// ownedByMaster is the sibling's ownerReference: unlinking the credential deletes
// the master, and owner-ref GC reaps its redemption material with it rather than
// leaving a DCR client secret behind. Derived from the master on every write, so
// a re-created master's new UID replaces a reaped one's.
func ownedByMaster(master *corev1.Secret) []metav1.OwnerReference {
	return []metav1.OwnerReference{{
		APIVersion: "v1",
		Kind:       "Secret",
		Name:       master.Name,
		UID:        master.UID,
	}}
}

// markRefreshMaterial stamps the two labels the sibling Secret needs:
// refresh.MaterialSecretLabel, identifying it as redemption material for the
// credential it is named after (the name alone is not proof — see that constant),
// and adoptguard.AdoptedLabel, without which the operator's label-filtered Secret
// informer would not carry it and every refresh would fail with "no readable
// token_endpoint".
func markRefreshMaterial(sec *corev1.Secret) {
	adoptguard.WithAdoptedLabel(sec)
	l := sec.GetLabels()
	l[refresh.MaterialSecretLabel] = "true"
	sec.SetLabels(l)
}
