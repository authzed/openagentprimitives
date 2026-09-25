// Package setup is the credential-acquisition engine. Its public Store function
// is the single chokepoint that writes credential bytes and mutates the
// AgentIdentity; builtin flows call it, and the engine wraps every
// per-requirement run in a Store call.
package setup

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/authkind"
	credkindregistry "github.com/authzed/openagentprimitives/pkg/platform/identity/credkind/registry"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/setup/builtins"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/useridentity"
)

// StoreRequest is the input to Store.
type StoreRequest struct {
	Namespace    string
	IdentityName string
	Requirement  authkind.CredentialRequirement
	Value        builtins.StoreValue

	// SubjectID is the provider's stable id for the account the credential was
	// live-verified against (builtins.VerifyResult.SubjectID), threaded in by
	// the caller that ran verification — Store performs no network I/O of its
	// own. Empty when verification produced no id (unsupported provider,
	// indeterminate check, or a provider with no subjectIDField declared);
	// Store records nothing in that case rather than guessing.
	SubjectID string
}

// Store persists the credential value(s) into the AgentIdentity's Secret and
// ensures the AgentIdentity has the matching named credential. Idempotent:
// re-running with the same inputs against a pre-existing AgentIdentity +
// credential updates the Secret value only, without duplicating scaffolding.
//
// Secret and spec writes use MergeFrom so keys and fields this call did not
// touch survive; LastSetupAt is a separate status-subresource patch.
func Store(ctx context.Context, c client.Client, req StoreRequest) error {
	if req.Namespace == "" || req.IdentityName == "" {
		return fmt.Errorf("setup.Store: namespace and identity name required")
	}
	if req.Requirement.SuggestedName == "" {
		return fmt.Errorf("setup.Store: requirement.SuggestedName required")
	}

	// 1. Get-or-create AgentIdentity.
	var ai spiceboxv1alpha1.AgentIdentity
	if err := c.Get(ctx, types.NamespacedName{Namespace: req.Namespace, Name: req.IdentityName}, &ai); err != nil {
		if !errors.IsNotFound(err) {
			return fmt.Errorf("get AgentIdentity: %w", err)
		}
		ai = spiceboxv1alpha1.AgentIdentity{
			ObjectMeta: metav1.ObjectMeta{Name: req.IdentityName, Namespace: req.Namespace},
		}
		if err := c.Create(ctx, &ai); err != nil {
			return fmt.Errorf("create AgentIdentity: %w", err)
		}
	}

	// 2. Resolve the write target, honoring a pre-declared credential's
	// secretRef when present, and get-or-create the Secret.
	credName := req.Requirement.SuggestedName
	secretName, secretKey, credType, err := resolveSecretTarget(&ai, req.IdentityName, credName, req.Value)
	if err != nil {
		return err
	}

	var sec corev1.Secret
	getErr := c.Get(ctx, types.NamespacedName{Namespace: req.Namespace, Name: secretName}, &sec)
	creating := errors.IsNotFound(getErr)
	if getErr != nil && !creating {
		return fmt.Errorf("get Secret: %w", getErr)
	}
	if creating {
		sec = corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: req.Namespace},
			Type:       corev1.SecretTypeOpaque,
		}
	}
	if sec.Data == nil {
		sec.Data = map[string][]byte{}
	}

	// 3. Write value bytes into the Secret's Data.
	originalSec := sec.DeepCopy()
	if err := writeValueIntoSecret(&sec, req.Value, secretKey); err != nil {
		return err
	}

	// Record which provider-side account this credential was verified against.
	// A no-op when either half is empty, so a caller that could not verify
	// never overwrites an earlier attestation with silence.
	useridentity.SetAttestation(&sec, req.Requirement.ProviderID, req.SubjectID)

	if creating {
		if err := c.Create(ctx, &sec); err != nil {
			return fmt.Errorf("create Secret: %w", err)
		}
	} else {
		if err := c.Patch(ctx, &sec, client.MergeFrom(originalSec)); err != nil {
			return fmt.Errorf("patch Secret: %w", err)
		}
	}

	// 4. Patch AgentIdentity spec to upsert the named credential. Credential type
	// is immutable: for a name that already exists only the Secret value is
	// updated. If a provider's output shape changes (e.g. static → oauth), the
	// user must delete the old credential entry before re-running setup.
	originalAI := ai.DeepCopy()
	if findCredential(&ai, credName) == nil {
		cred, err := buildCredential(credName, credType, secretName, secretKey)
		if err != nil {
			return fmt.Errorf("setup.Store: %w", err)
		}
		ai.Spec.Credentials = append(ai.Spec.Credentials, cred)
	}
	if err := c.Patch(ctx, &ai, client.MergeFrom(originalAI)); err != nil {
		return fmt.Errorf("patch AgentIdentity: %w", err)
	}

	// 5. Bump LastSetupAt via a separate status subresource patch.
	originalForStatus := ai.DeepCopy()
	now := metav1.NewTime(time.Now())
	ai.Status.LastSetupAt = &now
	if err := c.Status().Patch(ctx, &ai, client.MergeFrom(originalForStatus)); err != nil {
		return fmt.Errorf("patch AgentIdentity status: %w", err)
	}
	return nil
}

// writeValueIntoSecret writes value bytes into sec.Data. For static values
// (Bearer/Kubeconfig) the single value is written to staticKey. OAuth writes
// its fixed multi-key set and ignores staticKey.
func writeValueIntoSecret(sec *corev1.Secret, v builtins.StoreValue, staticKey string) error {
	switch {
	case v.Bearer != "":
		sec.Data[staticKey] = []byte(v.Bearer)
		return nil
	case v.KubeconfigYAML != "":
		sec.Data[staticKey] = []byte(v.KubeconfigYAML)
		return nil
	case v.OAuth != nil:
		sec.Data["access_token"] = []byte(v.OAuth.AccessToken)
		if v.OAuth.RefreshToken != "" {
			sec.Data["refresh_token"] = []byte(v.OAuth.RefreshToken)
		}
		if v.OAuth.ExpiresIn > 0 {
			exp := time.Now().Add(time.Duration(v.OAuth.ExpiresIn) * time.Second).UTC().Format(time.RFC3339)
			sec.Data["expires_at"] = []byte(exp)
		}
		if v.OAuth.TokenEndpoint != "" {
			sec.Data["token_endpoint"] = []byte(v.OAuth.TokenEndpoint)
		}
		if v.OAuth.ClientID != "" {
			sec.Data["client_id"] = []byte(v.OAuth.ClientID)
		}
		if v.OAuth.ClientSecret != "" {
			sec.Data["client_secret"] = []byte(v.OAuth.ClientSecret)
		}
		if v.OAuth.Scope != "" {
			sec.Data["scope"] = []byte(v.OAuth.Scope)
		}
		return nil
	default:
		return fmt.Errorf("setup.Store: StoreValue is empty")
	}
}

// valueShape returns the credential type and the default secret key implied
// by which StoreValue variant is populated. defaultKey is "" for oauth.
func valueShape(v builtins.StoreValue) (credType, defaultKey string, err error) {
	switch {
	case v.Bearer != "":
		return "static", "token", nil
	case v.KubeconfigYAML != "":
		return "static", "kubeconfig", nil
	case v.OAuth != nil:
		return "oauth", "", nil
	default:
		return "", "", fmt.Errorf("setup.Store: StoreValue is empty")
	}
}

// resolveSecretTarget decides which Secret name/key Store writes the credential
// value into. A credential already declared under this name with a populated
// secretRef wins (its Kind's own SecretRef: name+key for a single-key shape
// like static, falling back to the value-shape default key when the declared
// key is empty; name only for a fixed multi-key shape like oauth). Otherwise
// the <identity>-<credential> convention name and the value-shape default key.
func resolveSecretTarget(ai *spiceboxv1alpha1.AgentIdentity, identityName, credName string, v builtins.StoreValue) (secretName, secretKey, credType string, err error) {
	credType, defaultKey, err := valueShape(v)
	if err != nil {
		return "", "", "", err
	}
	if existing := findCredential(ai, credName); existing != nil {
		if existing.Type != "" && existing.Type != credType {
			return "", "", "", fmt.Errorf(
				"setup.Store: credential %q is type=%q but setup produced a %s credential; "+
					"delete the credential from AgentIdentity %q and re-run setup to re-provision it",
				credName, existing.Type, credType, identityName)
		}
		k, kErr := credkindregistry.Get(credType)
		if kErr != nil {
			return "", "", "", kErr
		}
		if ref := k.SecretRef(*existing); ref != nil && ref.Name != "" {
			key := ref.Key
			if key == "" {
				key = defaultKey
			}
			return ref.Name, key, credType, nil
		}
	}
	return identityName + "-" + credName, defaultKey, credType, nil
}

// buildCredential constructs the typed spec block for credType by delegating
// to its registered kind. It returns an error for a type the setup flow
// cannot write (e.g. federated, which is minted, not stored), rather than the
// old switch's silent fall-through to a credential with no block set at all.
func buildCredential(name, credType, secretName, secretKey string) (spiceboxv1alpha1.AgentCredential, error) {
	k, err := credkindregistry.Get(credType)
	if err != nil {
		return spiceboxv1alpha1.AgentCredential{}, err
	}
	return k.BuildCredential(name, secretName, secretKey)
}

func findCredential(a *spiceboxv1alpha1.AgentIdentity, name string) *spiceboxv1alpha1.AgentCredential {
	for i := range a.Spec.Credentials {
		if a.Spec.Credentials[i].Name == name {
			return &a.Spec.Credentials[i]
		}
	}
	return nil
}
