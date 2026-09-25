// descriptors.go — the shared, kind-agnostic credential resolver. It maps a
// slice of authkind.CredentialRequirement (emitted by ANY tool Kind — cli,
// mcp, toolspec, sidecar, …) onto CredentialDescriptors: secret-ref pointers
// plus injection shape, never secret bytes. It reads only SuggestedName and
// Inject — it neither knows nor cares which Kind produced the requirements.
package credresolve

import (
	"context"
	"errors"
	"fmt"

	"github.com/go-logr/logr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/authkind"
	credkindregistry "github.com/authzed/openagentprimitives/pkg/platform/identity/credkind/registry"
)

// ErrCredentialMissing is returned when a required credential is absent from
// the identity. Descriptors NEVER returns (nil, nil) for a missing credential —
// the no-op silent path is the bug this redesign removes.
var ErrCredentialMissing = errors.New("credresolve: required credential absent from identity")

// Descriptors maps requirements (from ANY authkind.Kind) to CredentialDescriptors
// — secret-ref pointers, NO secret bytes. `remap` is applied to the credential
// name before lookup (ToolBundle.CredentialRemap / MCP ref remap). Returns a
// typed error (ErrCredentialMissing) when a required credential is not in the
// identity — NEVER (nil, nil). Kind-agnostic: it reads only req.SuggestedName +
// req.Inject.
func Descriptors(reqs []authkind.CredentialRequirement, id RuntimeIdentity, remap map[string]string) ([]spiceboxv1alpha1.CredentialDescriptor, error) {
	out := make([]spiceboxv1alpha1.CredentialDescriptor, 0, len(reqs))
	for _, r := range reqs {
		name := r.SuggestedName
		if m, ok := remap[name]; ok {
			name = m
		}
		cred := findCredential(id.Credentials, name)
		if cred == nil {
			return nil, fmt.Errorf("%w: %q (inject=%s) not in %s", ErrCredentialMissing, name, injectLabel(r.Inject), id.Label)
		}
		src, err := SourceFor(cred, id)
		if err != nil {
			return nil, fmt.Errorf("credential %q: %w", name, err)
		}
		out = append(out, spiceboxv1alpha1.CredentialDescriptor{
			Source: src,
			Inject: injectionToCR(r.Inject),
		})
	}
	return out, nil
}

// Log emits the structured resolution trace (NO secret values). Call after a
// successful Descriptors() to record what bound to what.
func Log(l logr.Logger, id RuntimeIdentity, descs []spiceboxv1alpha1.CredentialDescriptor) {
	for _, d := range descs {
		l.Info("credresolve: resolved",
			"identity", id.Label,
			"secret", d.Source.Namespace+"/"+d.Source.Name+":"+d.Source.Key,
			"inject", injectLabelCR(d.Inject),
			"status", "ok")
	}
}

// Gap is one unmet credential requirement (name absent from the identity).
type Gap struct {
	CredentialName string
	Detail         string
}

// ValidateCoverage returns one Gap per requirement whose (remapped) credential
// name is absent from the identity. Pure — no Secret reads.
func ValidateCoverage(reqs []authkind.CredentialRequirement, id RuntimeIdentity, remap map[string]string) []Gap {
	var gaps []Gap
	for _, r := range reqs {
		name := r.SuggestedName
		if m, ok := remap[name]; ok {
			name = m
		}
		if findCredential(id.Credentials, name) == nil {
			gaps = append(gaps, Gap{CredentialName: name, Detail: "no credential of this name in the identity"})
		}
	}
	return gaps
}

// CheckSecretPresent verifies the credential's Secret + key exist AND are
// non-empty, WITHOUT returning the value. For controller/passthrough validity.
// Returns ErrSecretMissing / ErrSecretKeyMissing / ErrSecretValueEmpty / ErrExpired, or nil.
func CheckSecretPresent(ctx context.Context, c client.Reader, namespace string, cred spiceboxv1alpha1.AgentCredential) error {
	_, err := ResolveSecretValue(ctx, c, namespace, cred) // reads + validates; value discarded
	return err
}

// findCredential returns the credential named name from creds, or nil.
func findCredential(creds []spiceboxv1alpha1.AgentCredential, name string) *spiceboxv1alpha1.AgentCredential {
	for i := range creds {
		if creds[i].Name == name {
			return &creds[i]
		}
	}
	return nil
}

// SourceFor converts an AgentCredential into a descriptor Source, dispatching
// through the credkind registry rather than switching on cred.Type. Exported
// because externaltoken grant writers that skip the requirements→Descriptors
// path still need the IDENTICAL Source derivation, or their externaltoken.CredID
// stops matching byte-for-byte.
//
// A minted type (credkind.Kind.Minted() == true — today, federated) is
// dispatched first through k.SecretRef(): a minted kind backed by its own
// real Secret (a GitHub App's app-id/private-key/installation-id, say)
// resolves exactly like a stored type's descriptor. Only when the kind
// reports no Secret at all does this fall back to federated's own shape: the
// IdP-identity Secret carried on the credential's Federated block, which
// supplies the subject material for the mint and is never a value store
// itself. A type with neither is a kind-neutral error naming cred.Type — no
// message here may assume the minted kind IS federated. A Projectable
// type (credkind.Kind.Projectable() == true — today, static) resolves from
// the per-session projected Secret instead of its own master SecretRef when
// id.StaticProjection is set (userPassthrough): the operator wrote this
// credential's VALUE into that Secret keyed by credential name, in the
// session namespace — this branch reads only cred.Name and
// id.StaticProjection, deliberately never cred.Static itself, because the
// SessionUserIdentity view this runs against may have narrowed the
// credential down to just its name. Every other stored type resolves from
// id.Namespace (the master / IdP-identity Secret, where JIT refresh stays
// anchored).
//
// Returns an error for an unregistered type — never a zero-value Source, and
// never silently falling back to treating an unknown type as though it were
// type=static (the bug this registry dispatch replaces).
func SourceFor(cred *spiceboxv1alpha1.AgentCredential, id RuntimeIdentity) (spiceboxv1alpha1.CredentialSource, error) {
	k, err := credkindregistry.Get(cred.Type)
	if err != nil {
		return spiceboxv1alpha1.CredentialSource{}, err
	}

	if k.Minted() {
		// A minted kind backed by its own real Secret (SecretRef non-nil, e.g. a
		// GitHub App's app-id/private-key/installation-id) resolves exactly like
		// a stored type's descriptor: this is the ONLY branch that must handle a
		// future minted kind, so it is checked ahead of the federated-shaped
		// fallback below rather than gated behind cred.Type.
		if ref := k.SecretRef(*cred); ref != nil {
			return spiceboxv1alpha1.CredentialSource{
				Type: cred.Type, Namespace: id.Namespace,
				Name: ref.Name, Key: ref.Key,
			}, nil
		}
		// Fallback: federated's own shape. Nothing is stored in a Secret named by
		// SecretRef; the IdP-identity Secret named on the credential's Federated
		// block supplies the subject material for the mint instead.
		if cred.Federated != nil {
			return spiceboxv1alpha1.CredentialSource{
				Type: cred.Type, Namespace: id.Namespace,
				Name:              cred.Federated.IdPSecretRef.Name,
				Resource:          cred.Federated.Resource,
				ResourceServerURL: cred.Federated.ResourceServerURL,
				Scopes:            cred.Federated.Scopes,
			}, nil
		}
		return spiceboxv1alpha1.CredentialSource{}, fmt.Errorf(
			"credresolve: %q is type=%s (minted) but has no locatable Secret to resolve", cred.Name, cred.Type)
	}

	if k.Projectable() && id.StaticProjection != nil {
		// Per-session projection: the operator wrote this credential's VALUE
		// into the projected Secret keyed by credential name, in the session
		// namespace.
		return spiceboxv1alpha1.CredentialSource{
			Type:      cred.Type,
			Namespace: id.StaticProjection.Namespace,
			Name:      id.StaticProjection.SecretName,
			Key:       cred.Name,
		}, nil
	}

	ref := k.SecretRef(*cred)
	if ref == nil {
		return spiceboxv1alpha1.CredentialSource{}, fmt.Errorf(
			"credresolve: %q is type=%s but has no backing Secret to resolve", cred.Name, cred.Type)
	}

	return spiceboxv1alpha1.CredentialSource{
		Type: cred.Type, Namespace: id.Namespace,
		Name: ref.Name, Key: ref.Key,
	}, nil
}

// injectionToCR converts a kind-neutral authkind.Injection into the CR-level
// CredentialInjection carried by a CredentialDescriptor.
func injectionToCR(in authkind.Injection) spiceboxv1alpha1.CredentialInjection {
	cr := spiceboxv1alpha1.CredentialInjection{EnvVar: in.EnvVar}
	if in.Header != nil {
		cr.Header = &spiceboxv1alpha1.HeaderInjection{Name: in.Header.Name, ValuePrefix: in.Header.ValuePrefix}
	}
	return cr
}

// injectLabel renders a non-sensitive label for an authkind.Injection (for
// error messages — never includes a value).
func injectLabel(in authkind.Injection) string {
	if in.Header != nil {
		return "header:" + in.Header.Name
	}
	return "env:" + in.EnvVar
}

// injectLabelCR renders a non-sensitive label for a CR-level injection (for
// the resolution trace — never includes a value).
func injectLabelCR(in spiceboxv1alpha1.CredentialInjection) string {
	if in.Header != nil {
		return "header:" + in.Header.Name
	}
	return "env:" + in.EnvVar
}
