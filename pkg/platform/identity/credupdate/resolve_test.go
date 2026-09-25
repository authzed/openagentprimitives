package credupdate_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/authzed/openagentprimitives/pkg/agent/tool/originfmt"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	// Registers the static/oauth/federated credkind Kinds so
	// credresolve.SourceFor / Descriptors's registry.Get(cred.Type) dispatch
	// resolves in this package's tests.
	_ "github.com/authzed/openagentprimitives/pkg/platform/identity/credkind/imports"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/credresolve"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/credupdate"
)

func resolveScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, spiceboxv1alpha1.AddToScheme(s))
	require.NoError(t, corev1.AddToScheme(s))
	return s
}

// twoCredentialIdentity returns a credresolve.RuntimeIdentity whose
// Credentials slice holds TWO entries sharing the exact name
// ("demo-token") that TestResolveOrigin_AmbiguousRefuses' MCPServer
// declares as spec.auth.credential. That is what makes the ambiguity
// genuine: mcpkind.SetupRequirements emits exactly one requirement per
// MCPServer (its single spec.auth.credential — there is no MCPServer spec
// shape that yields two), so credresolve.Descriptors itself can never see
// more than one requirement for a single "mcpserver/<name>" origin, and its
// findCredential lookup returns on the FIRST name match — it would silently
// pick demo-secret-a and never notice demo-secret-b exists. The real
// ambiguity ResolveOrigin must catch is exactly this: two credentials in
// the identity's own catalog answering to the same name, which a
// first-match lookup would resolve by silent guess. See
// countCredentialsNamed in resolve.go and task-3-report.md for the full
// writeup of why a two-REQUIREMENT MCPServer isn't constructible.
func twoCredentialIdentity(t *testing.T) credresolve.RuntimeIdentity {
	t.Helper()
	return credresolve.RuntimeIdentity{
		Credentials: []spiceboxv1alpha1.AgentCredential{
			{
				Name: "demo-token",
				Type: "static",
				Static: &spiceboxv1alpha1.StaticCredentialSource{
					SecretRef: spiceboxv1alpha1.SecretKeyRef{Name: "demo-secret-a", Key: "token"},
				},
			},
			{
				Name: "demo-token",
				Type: "static",
				Static: &spiceboxv1alpha1.StaticCredentialSource{
					SecretRef: spiceboxv1alpha1.SecretKeyRef{Name: "demo-secret-b", Key: "token"},
				},
			},
		},
		Namespace: "default",
		Label:     "AgentIdentity demo-agent",
	}
}

// TestResolveOrigin_Refusals pins each refusal by its EXACT reason sentence,
// not merely by its Determination. Every refusal here carries the same
// Determination (NoCredential) and the same Tier (TierNone), so asserting only
// those would let a case pass having reached a COMPLETELY DIFFERENT refusal
// than the one it names -- an unrecognized-kind case that silently started
// being rejected as malformed, or as "resolved fine but the identity has no
// such credential", would still be green. The reason text is the only field
// that distinguishes them, so it is the field asserted.
func TestResolveOrigin_Refusals(t *testing.T) {
	cases := []struct {
		name       string
		origin     string
		objects    []client.Object
		wantReason string
	}{
		{
			name:    "an origin naming no MCPServer refuses NoCredential by name",
			origin:  "mcpserver/absent",
			objects: nil,
			wantReason: `MCPServer "absent" was not found in namespace "default", ` +
				`so its credential cannot be resolved.`,
		},
		{
			// THE FAIL-CLOSED REGRESSION GUARD for widening ResolveOrigin past
			// "mcpserver". "sandbox" is a deliberate choice: sandbox tools are
			// origin-LESS, so no writer can ever compose "sandbox/..." -- if this
			// kind is ever accepted, something has started guessing.
			name:    "an origin kind no writer composes refuses as UNRECOGNIZED, not as malformed",
			origin:  "sandbox/demo-toolkit",
			objects: nil,
			wantReason: `The failing tool's origin ("sandbox/demo-toolkit") names an origin kind ("sandbox") ` +
				`this platform does not resolve credentials for, so there is no credential to update.`,
		},
		{
			name:    "an empty origin refuses as MALFORMED, before any kind is considered",
			origin:  "",
			objects: nil,
			wantReason: `The failing tool's origin ("") is not a well-formed "<kind>/<name>" origin, ` +
				`so there is no credential to update.`,
		},
		{
			name:    "a bare kind with no name refuses as MALFORMED, never as that kind",
			origin:  "toolkit/",
			objects: nil,
			wantReason: `The failing tool's origin ("toolkit/") is not a well-formed "<kind>/<name>" origin, ` +
				`so there is no credential to update.`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := fake.NewClientBuilder().WithScheme(resolveScheme(t)).WithObjects(tc.objects...).Build()

			res, err := credupdate.ResolveOrigin(context.Background(), c, credupdate.ResolveInput{
				Namespace: "default",
				Origin:    tc.origin,
			})

			require.NoError(t, err, "resolution refusals are outcomes, not errors")
			require.NotNil(t, res.Refusal, "expected a refusal")
			assert.Equal(t, spiceboxv1alpha1.CredentialUpdateDeterminationNoCredential, res.Refusal.Determination)
			assert.Equal(t, credupdate.TierNone, res.Refusal.Tier)
			assert.Equal(t, tc.wantReason, res.Refusal.Reason,
				"the refusal must be the one this case names, not a different refusal with the same determination")
		})
	}
}

// demoToolkitCR is a cluster-scoped SpiceboxToolkit declaring exactly one
// sensitive env var, which is what cli.Kind.SetupRequirements turns into a
// single credential requirement. metadata.name and spec.name are deliberately
// EQUAL: a toolkit origin carries the toolkit's LOGICAL name (spec.name, what
// SandboxTool.Origin() reads) while the CR is addressed by metadata.name, so
// this fixture is the case where the two coincide. See the ResolveOrigin doc
// comment for what happens when they do not.
func demoToolkitCR(t *testing.T) *spiceboxv1alpha1.SpiceboxToolkit {
	t.Helper()
	return &spiceboxv1alpha1.SpiceboxToolkit{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-toolkit"},
		Spec: spiceboxv1alpha1.SpiceboxToolkitSpec{
			Name:            "demo-toolkit",
			Version:         "1",
			ToolkitRevision: "2026-01-01",
			Target:          spiceboxv1alpha1.ToolkitTarget{Binary: "demotool"},
			Parser:          spiceboxv1alpha1.ToolkitParserConfig{Kind: "declarative"},
			Env: spiceboxv1alpha1.ToolkitEnv{
				Allowed: []spiceboxv1alpha1.ToolkitEnvVar{{
					Name:       "DEMO_TOKEN",
					Sensitive:  true,
					Credential: "demo-token",
					Provider:   "demo-provider",
				}},
			},
		},
	}
}

// oneCredentialIdentity is the minimal identity holding a single static
// credential named name, backed by Secret secretName.
func oneCredentialIdentity(t *testing.T, name, secretName string) credresolve.RuntimeIdentity {
	t.Helper()
	return credresolve.RuntimeIdentity{
		Credentials: []spiceboxv1alpha1.AgentCredential{{
			Name: name,
			Type: "static",
			Static: &spiceboxv1alpha1.StaticCredentialSource{
				SecretRef: spiceboxv1alpha1.SecretKeyRef{Name: secretName, Key: "token"},
			},
		}},
		Namespace: "default",
		Label:     "AgentIdentity demo-agent",
	}
}

// TestResolveOrigin_ToolkitOriginResolves is one half of the fix this file's
// package exists to prove: the runner records auth-failure observations under a
// "toolkit/<name>" origin, and until this landed ResolveOrigin refused every
// one of them, so a CLI credential could never be corroborated end-to-end.
//
// The origin under test is composed by originfmt.ForToolkit -- the SAME
// function the recording side calls -- rather than by a literal repeated here.
// A literal would still pass if the writer's prefix changed, which is exactly
// the divergence that produced the bug.
func TestResolveOrigin_ToolkitOriginResolves(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(resolveScheme(t)).WithObjects(demoToolkitCR(t)).Build()

	res, err := credupdate.ResolveOrigin(context.Background(), c, credupdate.ResolveInput{
		Namespace:         "default",
		Origin:            originfmt.ForToolkit("demo-toolkit"),
		Identity:          oneCredentialIdentity(t, "demo-token", "demo-secret"),
		IdentityKind:      "AgentIdentity",
		IdentityName:      "demo-agent",
		IdentityNamespace: "default",
	})

	require.NoError(t, err)
	require.Nil(t, res.Refusal, "a toolkit origin backed by exactly one credential must resolve, not refuse")
	assert.Equal(t, "demo-token", res.Ref.Credential)
	assert.Equal(t, "demo-provider", res.Ref.ProviderID)
	assert.Equal(t, "AgentIdentity", res.Ref.IdentityKind)
	assert.Equal(t, "demo-agent", res.Ref.Name)
	assert.Equal(t, "demo-secret", res.Descriptor.Source.Name,
		"the descriptor must point at the toolkit credential's own Secret")
}

// TestResolveOrigin_SidecarToolboxOriginResolves is the other half. The
// credential name is not free-form: the operator materializes a sidecar's
// upstream credential as "<CR name>-creds" (agentsession's
// resolveSidecarCredential), which is the same name
// sidecartoolbox.Kind.SetupRequirements suggests -- so resolving through that
// Kind lands on the credential the session is actually running with.
//
// ResolvedSidecarToolbox.Name is set to something OTHER than Ref on purpose:
// Name is the LLM-facing prefix and Ref is the CR, and only Ref appears in the
// origin. A reader keyed on the wrong one would resolve a nonexistent CR.
func TestResolveOrigin_SidecarToolboxOriginResolves(t *testing.T) {
	box := &spiceboxv1alpha1.SidecarToolbox{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-toolbox", Namespace: "default"},
		Spec: spiceboxv1alpha1.SidecarToolboxSpec{
			Name:    "demo",
			Version: "1",
			UpstreamAuth: spiceboxv1alpha1.SidecarToolboxUpstream{
				Provider: "demo-provider",
				EnvVar:   "DEMO_TOKEN",
			},
		},
	}
	c := fake.NewClientBuilder().WithScheme(resolveScheme(t)).WithObjects(box).Build()

	origin := originfmt.ForSidecar(spiceboxv1alpha1.ResolvedSidecarToolbox{
		Name: "llm-facing-prefix",
		Ref:  "demo-toolbox",
	})

	res, err := credupdate.ResolveOrigin(context.Background(), c, credupdate.ResolveInput{
		Namespace:         "default",
		Origin:            origin,
		Identity:          oneCredentialIdentity(t, "demo-toolbox-creds", "demo-sidecar-secret"),
		IdentityKind:      "AgentIdentity",
		IdentityName:      "demo-agent",
		IdentityNamespace: "default",
	})

	require.NoError(t, err)
	require.Nil(t, res.Refusal, "a sidecar origin backed by exactly one credential must resolve, not refuse")
	assert.Equal(t, "demo-toolbox-creds", res.Ref.Credential)
	assert.Equal(t, "demo-provider", res.Ref.ProviderID)
	assert.Equal(t, "demo-sidecar-secret", res.Descriptor.Source.Name)
}

// TestResolveOrigin_AmbiguousRefuses pins the safety property: when an origin
// binds more than one credential we refuse rather than guess, because
// prompting for the wrong token is worse than not prompting at all.
func TestResolveOrigin_AmbiguousRefuses(t *testing.T) {
	mcp := &spiceboxv1alpha1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-mcp", Namespace: "default"},
		Spec: spiceboxv1alpha1.MCPServerSpec{
			// spec.auth.credential must be set for mcpkind.SetupRequirements to
			// emit a requirement at all (an unset credential name means
			// "unauthenticated" and yields zero requirements — see
			// TestSetupRequirementsUnauthenticatedReturnsNil in
			// pkg/platform/identity/authkind/mcp). Its name must match
			// twoCredentialIdentity's two entries for the ambiguity to bite.
			Auth: spiceboxv1alpha1.MCPServerAuth{Credential: "demo-token"},
		},
	}
	c := fake.NewClientBuilder().WithScheme(resolveScheme(t)).WithObjects(mcp).Build()

	res, err := credupdate.ResolveOrigin(context.Background(), c, credupdate.ResolveInput{
		Namespace: "default",
		Origin:    "mcpserver/demo-mcp",
		Identity:  twoCredentialIdentity(t),
	})

	require.NoError(t, err)
	require.NotNil(t, res.Refusal)
	assert.Equal(t, spiceboxv1alpha1.CredentialUpdateDeterminationAmbiguousCred, res.Refusal.Determination)
	assert.Contains(t, res.Refusal.Reason, "more than one")
}

// TestResolveOrigin_ExactlyOneResolves is not in the brief's given test
// code, but Step 6 (the "exactly one" / no-refusal path) is otherwise
// untested by any case above — every given test exercises a refusal.
// Added to pin the success path: Ref and Descriptor populated, Refusal nil.
func TestResolveOrigin_ExactlyOneResolves(t *testing.T) {
	mcp := &spiceboxv1alpha1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-mcp", Namespace: "default"},
		Spec: spiceboxv1alpha1.MCPServerSpec{
			Auth: spiceboxv1alpha1.MCPServerAuth{
				Credential: "demo-token",
				Provider:   "demo-provider",
			},
		},
	}
	c := fake.NewClientBuilder().WithScheme(resolveScheme(t)).WithObjects(mcp).Build()

	id := credresolve.RuntimeIdentity{
		Credentials: []spiceboxv1alpha1.AgentCredential{
			{
				Name: "demo-token",
				Type: "static",
				Static: &spiceboxv1alpha1.StaticCredentialSource{
					SecretRef: spiceboxv1alpha1.SecretKeyRef{Name: "demo-secret", Key: "token"},
				},
			},
		},
		Namespace: "default",
		Label:     "AgentIdentity demo-agent",
	}

	res, err := credupdate.ResolveOrigin(context.Background(), c, credupdate.ResolveInput{
		Namespace:         "default",
		Origin:            "mcpserver/demo-mcp",
		Identity:          id,
		IdentityKind:      "AgentIdentity",
		IdentityName:      "demo-agent",
		IdentityNamespace: "default",
	})

	require.NoError(t, err)
	assert.Nil(t, res.Refusal, "exactly one match must not refuse")
	assert.Equal(t, "AgentIdentity", res.Ref.IdentityKind)
	assert.Equal(t, "demo-agent", res.Ref.Name)
	assert.Equal(t, "default", res.Ref.Namespace)
	assert.Equal(t, "demo-token", res.Ref.Credential)
	assert.Equal(t, "demo-provider", res.Ref.ProviderID)
	assert.Equal(t, "demo-secret", res.Descriptor.Source.Name)
	assert.Equal(t, "token", res.Descriptor.Source.Key)
}

// TestResolveOrigin_RefIdentityRoundTripsCallerFields proves
// Ref.IdentityKind/Ref.Name are ResolveInput.IdentityKind/IdentityName
// carried through VERBATIM -- never parsed or inferred from
// credresolve.RuntimeIdentity.Label. Label carries no stability contract
// (see the ResolveInput.IdentityKind doc: it is documented as "used only in
// error messages") and its real value for a session-projected identity is
// "SessionUserIdentity <name>", not "UserIdentity <name>" -- a parsing
// approach silently mislabels every passthrough identity. Identity.Label
// here is deliberately set to a DIFFERENT kind/name than
// IdentityKind/IdentityName to prove nothing reads it for this.
func TestResolveOrigin_RefIdentityRoundTripsCallerFields(t *testing.T) {
	mcp := &spiceboxv1alpha1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-mcp", Namespace: "default"},
		Spec: spiceboxv1alpha1.MCPServerSpec{
			Auth: spiceboxv1alpha1.MCPServerAuth{Credential: "demo-token"},
		},
	}
	c := fake.NewClientBuilder().WithScheme(resolveScheme(t)).WithObjects(mcp).Build()

	id := credresolve.RuntimeIdentity{
		Credentials: []spiceboxv1alpha1.AgentCredential{
			{
				Name: "demo-token",
				Type: "static",
				Static: &spiceboxv1alpha1.StaticCredentialSource{
					SecretRef: spiceboxv1alpha1.SecretKeyRef{Name: "demo-secret", Key: "token"},
				},
			},
		},
		Namespace: "default",
		// Deliberately mismatched vs. IdentityKind/IdentityName below.
		Label: "AgentIdentity wrong-name",
	}

	res, err := credupdate.ResolveOrigin(context.Background(), c, credupdate.ResolveInput{
		Namespace:    "default",
		Origin:       "mcpserver/demo-mcp",
		Identity:     id,
		IdentityKind: "SessionUserIdentity",
		IdentityName: "demo-user-session",
	})

	require.NoError(t, err)
	require.Nil(t, res.Refusal)
	assert.Equal(t, "SessionUserIdentity", res.Ref.IdentityKind,
		"IdentityKind must come from ResolveInput, not Identity.Label")
	assert.Equal(t, "demo-user-session", res.Ref.Name,
		"Name must come from ResolveInput, not Identity.Label")
}

// TestResolveOrigin_SessionUserIdentityNamespaceDiverges pins the namespace
// counterpart of the bug above: RuntimeIdentityFromSessionUserIdentity sets
// Identity.Namespace to spiceboxv1alpha1.IdentitiesNamespace (the fixed
// namespace holding the master UserIdentity's oauth/federated Secrets), NOT
// the namespace the SessionUserIdentity CR itself lives in — that CR's own
// namespace is only available via StaticProjection.Namespace (and, here, the
// caller-supplied IdentityNamespace). A Ref.Namespace read from
// Identity.Namespace would point at IdentitiesNamespace and could never
// locate the CR it names — this shape is slice 1's primary case
// (passthrough/user-owned credentials), so it would fire on the main path,
// not an edge case.
func TestResolveOrigin_SessionUserIdentityNamespaceDiverges(t *testing.T) {
	mcp := &spiceboxv1alpha1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-mcp", Namespace: "default"},
		Spec: spiceboxv1alpha1.MCPServerSpec{
			Auth: spiceboxv1alpha1.MCPServerAuth{Credential: "demo-token"},
		},
	}
	c := fake.NewClientBuilder().WithScheme(resolveScheme(t)).WithObjects(mcp).Build()

	const sessionNamespace = "demo-session-ns"

	// Shaped exactly like RuntimeIdentityFromSessionUserIdentity's output:
	// Identity.Namespace is the fixed identities namespace, and
	// StaticProjection points at the session's own namespace — the two
	// deliberately differ, mirroring the real divergence this test guards.
	id := credresolve.RuntimeIdentity{
		Credentials: []spiceboxv1alpha1.AgentCredential{
			{
				Name: "demo-token",
				Type: "static",
				Static: &spiceboxv1alpha1.StaticCredentialSource{
					SecretRef: spiceboxv1alpha1.SecretKeyRef{Name: "demo-secret", Key: "token"},
				},
			},
		},
		Namespace: spiceboxv1alpha1.IdentitiesNamespace,
		Label:     "SessionUserIdentity demo-user-session",
		StaticProjection: &credresolve.StaticCredentialProjection{
			Namespace:  sessionNamespace,
			SecretName: "demo-session-secret",
		},
	}

	res, err := credupdate.ResolveOrigin(context.Background(), c, credupdate.ResolveInput{
		Namespace:         "default",
		Origin:            "mcpserver/demo-mcp",
		Identity:          id,
		IdentityKind:      "SessionUserIdentity",
		IdentityName:      "demo-user-session",
		IdentityNamespace: sessionNamespace,
	})

	require.NoError(t, err)
	require.Nil(t, res.Refusal)
	assert.Equal(t, sessionNamespace, res.Ref.Namespace,
		"Ref.Namespace must be the caller-supplied IdentityNamespace (the CR's own namespace), "+
			"not Identity.Namespace (the identities namespace where Secret refs resolve)")
	assert.NotEqual(t, spiceboxv1alpha1.IdentitiesNamespace, res.Ref.Namespace)
}

// TestResolveOrigin_RemapAppliesBeforeAmbiguityCheck pins that Remap is
// consulted before the duplicate-name ambiguity guard, exactly like
// credresolve.Descriptors' own remap semantics: the MCPServer's suggested
// name ("demo-token") is remapped to "renamed-token", which matches only
// ONE credential in the identity, so this resolves cleanly even though a
// second, differently-named credential is also present.
func TestResolveOrigin_RemapAppliesBeforeAmbiguityCheck(t *testing.T) {
	mcp := &spiceboxv1alpha1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-mcp", Namespace: "default"},
		Spec: spiceboxv1alpha1.MCPServerSpec{
			Auth: spiceboxv1alpha1.MCPServerAuth{Credential: "demo-token"},
		},
	}
	c := fake.NewClientBuilder().WithScheme(resolveScheme(t)).WithObjects(mcp).Build()

	id := credresolve.RuntimeIdentity{
		Credentials: []spiceboxv1alpha1.AgentCredential{
			{
				Name: "renamed-token",
				Type: "static",
				Static: &spiceboxv1alpha1.StaticCredentialSource{
					SecretRef: spiceboxv1alpha1.SecretKeyRef{Name: "demo-secret", Key: "token"},
				},
			},
			{
				Name: "unrelated-token",
				Type: "static",
				Static: &spiceboxv1alpha1.StaticCredentialSource{
					SecretRef: spiceboxv1alpha1.SecretKeyRef{Name: "other-secret", Key: "token"},
				},
			},
		},
		Namespace: "default",
		Label:     "AgentIdentity demo-agent",
	}

	res, err := credupdate.ResolveOrigin(context.Background(), c, credupdate.ResolveInput{
		Namespace:    "default",
		Origin:       "mcpserver/demo-mcp",
		Identity:     id,
		IdentityKind: "AgentIdentity",
		IdentityName: "demo-agent",
		Remap:        map[string]string{"demo-token": "renamed-token"},
	})

	require.NoError(t, err)
	assert.Nil(t, res.Refusal)
	assert.Equal(t, "renamed-token", res.Ref.Credential)
}
