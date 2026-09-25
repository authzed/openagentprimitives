package steelthread

import (
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/credkind"
	credkindregistry "github.com/authzed/openagentprimitives/pkg/platform/identity/credkind/registry"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/useridentity"
)

// The person a replayed userPassthrough session runs as.
//
// A userPassthrough class draws its tool credentials from the STARTER's own
// catalog, and parks in AwaitingCredentials until that person has linked every
// credential the agent needs. The live starter is a real user whose linked
// credentials live in their own UserIdentity; the replay has no such person, so
// the capture emits one — a UserIdentity for the bundle's own default user,
// carrying the same credential NAMES the run resolved, backed by placeholder
// Secrets.
//
// The email is the same one the replay harness defaults to, and the bundle says
// so out loud rather than relying on that: rewriteUserIdentity reports it and
// Capture stamps it onto Bundle.DefaultUser, so the name this fixture derives
// and the user the harness sends as cannot drift apart. Derived, never
// transcribed — identity.EmailReference is the same canonicalization the
// channel kinds perform on an inbound sender, so a change to the encoding moves
// both ends together.
const fixtureUserEmail = "user@example.com"

// fixtureUserSubject is the canonical SpiceDB subject fixtureUserEmail resolves
// to. An email always canonicalizes, so the error branch is unreachable; it is
// checked rather than discarded because a future encoding change that could
// fail must not become a silently empty subject.
func fixtureUserSubject() (identity.Subject, error) {
	subj, err := identity.EmailReference(fixtureUserEmail).Subject()
	if err != nil {
		return "", fmt.Errorf("steelthread: RewriteFixture: canonicalize the fixture user %q: %w",
			fixtureUserEmail, err)
	}
	return subj, nil
}

// rewriteUserIdentity turns the session's own SessionUserIdentity — the
// resolved projection of the starter's catalog, narrowed to exactly the
// credentials the run used — into the cluster-scoped UserIdentity a replayed
// userPassthrough session resolves, plus the placeholder Secrets backing it and
// the namespace they live in.
//
// Read from the SESSION's projection rather than from the live user's
// UserIdentity, for the reason this branch has now learned three times: the
// catalog holds whatever that person has ever linked, while the projection
// holds what THIS run resolved. Emitting the catalog would give the replay
// credentials the session never had.
//
// # What is rewritten, and what is not
//
// The credential NAMES and TYPES ride through untouched: they are what the
// class requires and what the passthrough gate compares against, so changing
// either would make the replay park on a credential the run did not need or
// sail past one it did.
//
// The SUBJECT and every Secret NAME are rewritten. The live subject identifies
// a real person and the live Secret names are derived from it
// (useridentity.MasterSecretName over useridentity.NameForSubject), so carrying
// either forward would write a real user's identity into a repo and would name
// Secrets no replay can produce. Both are re-derived from the fixture user
// through the same two functions the operator uses, so the emitted UserIdentity
// is addressed exactly as a real one is.
//
// The placeholder VALUES come from placeholderCredentialValue, the same
// shape-aware substitution the AgentIdentity rewrite uses: a credential
// resolving to a provider with a declared token shape gets a value that
// provider's own validator accepts, because a value it rejects would park the
// replayed session on a credential that looks unlinked.
//
// The Namespace document is emitted with them because envtest creates no
// namespaces and the operator resolves a passthrough credential's Secret from
// IdentitiesNamespace unconditionally. It leads the file: the harness applies a
// file's documents in order, so the namespace exists before the Secrets that
// land in it.
func rewriteUserIdentity(live *spiceboxv1alpha1.SessionUserIdentity) (FixtureFile, []MintedCredential, error) {
	subject, err := fixtureUserSubject()
	if err != nil {
		return FixtureFile{}, nil, err
	}
	uiName := useridentity.NameForSubject(subject)

	docs := []any{&corev1.Namespace{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Namespace"},
		ObjectMeta: metav1.ObjectMeta{Name: spiceboxv1alpha1.IdentitiesNamespace},
	}}

	ui := &spiceboxv1alpha1.UserIdentity{
		TypeMeta: typeMeta("UserIdentity"),
		// Cluster-scoped: no namespace, unlike every other emitted CR.
		ObjectMeta: metav1.ObjectMeta{Name: uiName},
		Spec: spiceboxv1alpha1.UserIdentitySpec{
			Subject: subject.String(),
		},
	}

	var minted []MintedCredential
	for _, cred := range live.Spec.Credentials {
		k, err := credkindregistry.Get(cred.Type)
		if err != nil {
			return FixtureFile{}, nil, fmt.Errorf(
				"steelthread: RewriteFixture: SessionUserIdentity %q credential %q: %w", live.Name, cred.Name, err)
		}
		// A minted credential arrives here for the same reason it does on an
		// AgentIdentity, and a placeholder Secret stands in for it no better:
		// the passthrough resolve calls the same broker. Named after the
		// EMITTED UserIdentity, not the live SessionUserIdentity, so a reader
		// looking for it finds it in the fixture.
		if mc := mintedCredentialFor(k, uiName, cred); mc.Label != "" {
			minted = append(minted, mc)
		}
		out := *cred.DeepCopy()
		if k.SecretRef(out) == nil {
			// A federated credential is minted on demand from the user's IdP
			// identity and has no backing Secret to placeholder. It is still
			// carried: the passthrough gate synthesizes one per federated
			// target and never reports it missing, so dropping it here would
			// change what the replayed session resolves.
			ui.Spec.Credentials = append(ui.Spec.Credentials, out)
			continue
		}
		secretName := useridentity.MasterSecretName(uiName, cred.Name)
		repointed, err := repointCredentialSecret(k, out, secretName)
		if err != nil {
			return FixtureFile{}, nil, fmt.Errorf(
				"steelthread: RewriteFixture: SessionUserIdentity %q credential %q: %w", live.Name, cred.Name, err)
		}

		keys := k.RequiredSecretKeys(repointed)
		if len(keys) == 0 {
			keys = []string{genericSecretKey}
		}
		data := make(map[string]string, len(keys))
		for _, key := range keys {
			val, err := placeholderCredentialValue(cred.Name, key)
			if err != nil {
				return FixtureFile{}, nil, err
			}
			data[key] = val
		}
		docs = append(docs, &corev1.Secret{
			TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
			ObjectMeta: metav1.ObjectMeta{
				Name:      secretName,
				Namespace: spiceboxv1alpha1.IdentitiesNamespace,
			},
			Type:       corev1.SecretTypeOpaque,
			StringData: data,
		})
		ui.Spec.Credentials = append(ui.Spec.Credentials, repointed)
	}

	doc, err := marshalDocs(append(docs, ui))
	if err != nil {
		return FixtureFile{}, nil, err
	}
	// Sorts after 00-secret.yaml and before 01-identity.yaml, so the class's
	// own placeholder Secrets stay first and the AgentIdentity — which a
	// userPassthrough class ignores for credentialed tools, but may still
	// declare — still follows.
	return FixtureFile{Name: "00a-useridentity.yaml", YAML: doc}, minted, nil
}

// repointCredentialSecret returns cred with its backing Secret NAME replaced,
// written through the field path the credential's own registered kind declares
// (credkind.Kind.SecretRefPath) rather than by switching on cred.Type.
//
// Same mechanism pkg/platform/oap/instance uses to repoint a bundled
// credential, and for the same reason: a kind added later declares its own path
// and is rewritten correctly here with no edit. The kind's SecretRef and
// SecretRefPath are pinned to agree by credkind's own test, so reading through
// one and writing through the other cannot address different fields.
//
// A kind with no declared path keeps its credential unchanged; the caller has
// already skipped the kinds with no backing Secret at all.
func repointCredentialSecret(
	k credkind.Kind, cred spiceboxv1alpha1.AgentCredential, secretName string,
) (spiceboxv1alpha1.AgentCredential, error) {
	path := k.SecretRefPath()
	if len(path) == 0 {
		return cred, nil
	}
	obj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&cred)
	if err != nil {
		return spiceboxv1alpha1.AgentCredential{}, fmt.Errorf("convert credential to unstructured: %w", err)
	}
	if err := unstructured.SetNestedField(obj, secretName, path...); err != nil {
		return spiceboxv1alpha1.AgentCredential{}, fmt.Errorf("set %s: %w", strings.Join(path, "."), err)
	}
	var out spiceboxv1alpha1.AgentCredential
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(obj, &out); err != nil {
		return spiceboxv1alpha1.AgentCredential{}, fmt.Errorf("convert credential back: %w", err)
	}
	if got := k.SecretRef(out); got == nil || got.Name != secretName {
		// The path wrote somewhere SecretRef does not read from. Refusing here
		// keeps the failure at the rewrite; letting it through emits a
		// UserIdentity pointing at a Secret the fixture never wrote, and the
		// replayed session parks on a credential that looks unlinked.
		return spiceboxv1alpha1.AgentCredential{}, fmt.Errorf(
			"repointing the backing Secret through %s did not change what SecretRef reads",
			strings.Join(path, "."))
	}
	return out, nil
}
