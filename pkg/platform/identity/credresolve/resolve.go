// Package credresolve materializes the underlying Secret value for a stored
// credential, returning it wrapped in a sensitive.SensitiveValue so the bytes
// can't be accidentally logged or serialized before the injection site. It
// dispatches through the credkind registry rather than switching on
// cred.Type, so a new stored type needs no change here. The expiry gate for
// oauth credentials lives in oauth.Kind's own ReadStoredValue, not in this
// package — each kind is its own gate.
package credresolve

import (
	"context"
	"errors"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/adoptguard"
	credkindregistry "github.com/authzed/openagentprimitives/pkg/platform/identity/credkind/registry"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/federation"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/refresh"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/sensitive"
)

// ErrExpired is returned when an oauth credential's expires_at is in
// the past. Callers translate this into AgentCredentialExpired.
var ErrExpired = errors.New("credresolve: oauth credential expired")

// ErrSecretMissing / ErrSecretKeyMissing / ErrSecretValueEmpty surface
// specific failure modes for callers that need to distinguish them.
// ErrSecretValueEmpty rejects a present-but-empty value for both static and
// oauth credentials — an empty value injected as e.g. GITHUB_TOKEN="" is
// worse than a missing one (it reads as "logged out").
var (
	ErrSecretMissing    = errors.New("credresolve: secret missing")
	ErrSecretKeyMissing = errors.New("credresolve: secret key missing")
	ErrSecretValueEmpty = errors.New("credresolve: secret value is empty")
)

// ErrSecretNotAdopted reports a Secret that EXISTS but carries no adoption
// label, so a read through the operator's label-filtered Secret cache cannot
// see it and returns NotFound.
//
// It is deliberately NOT wrapped around ErrSecretMissing. The two demand
// opposite responses from a human — create the Secret, versus wait for (or
// unblock) the AgentIdentity reconcile that adopts the one already there — and
// an operator told "secret missing" about a Secret sitting in front of them
// spends the time looking in the wrong place. Callers branching on
// errors.Is(err, ErrSecretMissing) therefore stop matching once a read is
// classified, which is the point.
var ErrSecretNotAdopted = errors.New("credresolve: secret exists but is not adopted by the operator")

// ExplainSecretMissing refines a "secret missing" verdict that came from a read
// through the operator's ADOPTION-FILTERED Secret cache, where NotFound
// collapses two different conditions into one.
//
// live must be an UNFILTERED reader (the manager's uncached APIReader) or nil.
// The outcomes:
//
//   - err is nil or is not ErrSecretMissing → returned untouched. Nothing else
//     is ever reclassified as an adoption problem.
//   - live is nil → returned untouched. Without a second, unfiltered opinion
//     the two states are genuinely indistinguishable, and guessing between them
//     would be worse than the imprecise message.
//   - the Secret is absent from the live reader too → ErrSecretMissing stands.
//   - the Secret is THERE and carries no adoption label → ErrSecretNotAdopted,
//     naming the Secret and the label whose absence hides it.
//   - the Secret is there AND adopted → ErrSecretMissing stands. The filtered
//     cache is merely lagging its informer, which the next resolve clears; there
//     is no operator action to name.
//
// The probe is METADATA-ONLY. Reading the value bytes of a Secret the operator
// has not adopted is precisely the overreach adoptguard exists to prevent, so
// this asks for a PartialObjectMetadata and learns only existence + labels.
//
// A probe that itself fails leaves the original verdict standing and names the
// probe failure alongside it, so a classification that could not run is visible
// rather than silently indistinguishable from "genuinely absent".
func ExplainSecretMissing(ctx context.Context, live client.Reader, ns, name string, err error) error {
	if err == nil || !errors.Is(err, ErrSecretMissing) || live == nil {
		return err
	}
	var meta metav1.PartialObjectMetadata
	meta.SetGroupVersionKind(corev1.SchemeGroupVersion.WithKind("Secret"))
	if gerr := live.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &meta); gerr != nil {
		if client.IgnoreNotFound(gerr) == nil {
			return err // genuinely absent; the original verdict was right
		}
		return fmt.Errorf("%w (could not check whether it is merely unadopted: %v)", err, gerr)
	}
	if _, adopted := meta.GetLabels()[adoptguard.AdoptedLabel]; adopted {
		return err // adopted; the label-filtered cache is only lagging
	}
	return fmt.Errorf("%w: %s/%s (it exists but carries no %s label, so the operator's Secret cache cannot see it; "+
		"the AgentIdentity reconcile that references it stamps the label — recreating a Secret drops it)",
		ErrSecretNotAdopted, ns, name, adoptguard.AdoptedLabel)
}

// ResolveSecretValue reads the credential's underlying Secret and produces
// the credential value wrapped in a sensitive.SensitiveValue. For oauth
// credentials, gates on expires_at — past expiration → ErrExpired without
// exposing token bytes. A present-but-empty value → ErrSecretValueEmpty.
//
// This dispatches through the credkind registry to the type's own
// ReadStoredValue rather than switching on cred.Type — the same registry the
// kinds themselves call INTO (their Resolve calls their own ReadStoredValue
// directly, never back through here, so there is no recursion). The
// error returned by a registered type's ReadStoredValue is passed through
// unwrapped, so callers using errors.Is(err, ErrExpired) keep working.
func ResolveSecretValue(ctx context.Context, c client.Reader, namespace string,
	cred spiceboxv1alpha1.AgentCredential) (sensitive.SensitiveValue, error) {
	k, err := credkindregistry.Get(cred.Type)
	if err != nil {
		return sensitive.SensitiveValue{}, err
	}
	return k.ReadStoredValue(ctx, c, namespace, cred)
}

// AgentCredentialFromSource builds the transient AgentCredential
// ResolveSecretValue expects from a descriptor's CredentialSource (the
// value-resolution inverse of credentialSource), via the same credkind
// registry rather than switching on s.Type: BuildCredential's (name,
// secretName, secretKey) shape is exactly this function's inverse for a
// value-resolvable type. Only type=static / type=oauth are value-resolvable
// this way; type=federated is minted, not read, so its BuildCredential always
// errors — like an unregistered type's registry.Get — and that error is
// returned here rather than silently substituted with a guessed shape.
// Callers MUST branch federated off before calling this at all; the two
// non-kind callers (agentsession's grant reconciler, credentialupdaterequest's
// refresh attempt) are both already in error-returning contexts.
// Shared by the inproc broker and the agentsession grant reconciler so their
// Source→value derivations cannot drift.
func AgentCredentialFromSource(s spiceboxv1alpha1.CredentialSource) (spiceboxv1alpha1.AgentCredential, error) {
	k, err := credkindregistry.Get(s.Type)
	if err != nil {
		return spiceboxv1alpha1.AgentCredential{}, err
	}
	return k.BuildCredential(s.Name, s.Name, s.Key)
}

// SubjectMaterial resolves the user's IdP-identity Secret to a fresh
// SubjectMaterial (the subject token + IdP endpoints needed for an ID-JAG
// token exchange). It JIT-refreshes an expired access_token via the oauth
// refresh path before reading the Secret's endpoint fields, so the returned
// Token is always current.
//
// c must be a full client.Client (not just client.Reader) because JIT
// refresh writes the renewed tokens back to the Secret.
//
// This is the canonical implementation shared by the inproc broker's
// federated-credential path and materializeSidecarSecret in the agentsession
// controller.
func SubjectMaterial(ctx context.Context, c client.Client, ns, secretName string) (federation.SubjectMaterial, error) {
	idpCred := spiceboxv1alpha1.AgentCredential{
		Name: secretName, Type: "oauth",
		OAuth: &spiceboxv1alpha1.OAuthCredentialSource{SecretRef: spiceboxv1alpha1.SecretRef{Name: secretName}},
	}
	// Resolve with expiry gate: if expired → JIT-refresh once, then re-read.
	tok, err := ResolveSecretValue(ctx, c, ns, idpCred)
	if errors.Is(err, ErrExpired) {
		if rerr := refresh.Run(ctx, c, ns, idpCred); rerr != nil {
			return federation.SubjectMaterial{}, fmt.Errorf("jit refresh idp-identity %q: %w (original: %w)", secretName, rerr, err)
		}
		tok, err = ResolveSecretValue(ctx, c, ns, idpCred)
	}
	if err != nil {
		return federation.SubjectMaterial{}, fmt.Errorf("resolve idp-identity %q: %w", secretName, err)
	}
	var sec corev1.Secret
	if err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: secretName}, &sec); err != nil {
		return federation.SubjectMaterial{}, fmt.Errorf("read idp-identity secret %q: %w", secretName, err)
	}
	return federation.SubjectMaterial{
		Token:            tok,
		IdPTokenEndpoint: string(sec.Data["token_endpoint"]),
		ClientID:         string(sec.Data["client_id"]),
		ClientSecret:     string(sec.Data["client_secret"]),
	}, nil
}

// GetSecret reads a Secret by namespace/name, translating NotFound into
// ErrSecretMissing so callers can distinguish "no such Secret" from a
// transient read error. Exported so a credkind.Kind's own ReadStoredValue
// (static, oauth) shares this exact translation rather than re-deriving it.
func GetSecret(ctx context.Context, c client.Reader, ns, name string) (*corev1.Secret, error) {
	var sec corev1.Secret
	if err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &sec); err != nil {
		if client.IgnoreNotFound(err) == nil {
			return nil, fmt.Errorf("%w: %s/%s", ErrSecretMissing, ns, name)
		}
		return nil, err
	}
	return &sec, nil
}
