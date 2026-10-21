package directcredentials

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/credkind"
)

func onePasswordCred(name, secretName string) spiceboxv1alpha1.AgentCredential {
	return spiceboxv1alpha1.AgentCredential{
		Name: name, Type: "directCredentials",
		DirectCredentials: &spiceboxv1alpha1.DirectCredentialsSource{
			Source:    "onepassword",
			SecretRef: spiceboxv1alpha1.DirectCredentialSecretRef{Name: secretName, Key: "token"},
			Vault:     "Engineering",
		},
	}
}

func secretCred(name, secretName string) spiceboxv1alpha1.AgentCredential {
	return spiceboxv1alpha1.AgentCredential{
		Name: name, Type: "directCredentials",
		DirectCredentials: &spiceboxv1alpha1.DirectCredentialsSource{
			Source:    "secret",
			SecretRef: spiceboxv1alpha1.DirectCredentialSecretRef{Name: secretName},
		},
	}
}

func TestKind_Facts(t *testing.T) {
	k := Kind{}
	assert.Equal(t, "directCredentials", k.Type())
	assert.True(t, k.Minted(), "a value is fetched fresh at use time, never cached by the broker")
	assert.False(t, k.NeedsRefresh(), "nothing is stored, so nothing is refreshed")
	assert.False(t, k.Projectable(), "the value never projects into a per-session Secret")
	assert.Equal(t, "Direct credentials (website logins)", k.DisplayName())
}

// TestValidOn_AgentIdentityOnly: the login is authored on the bot's own
// identity, not passed through from a human subject.
func TestValidOn_AgentIdentityOnly(t *testing.T) {
	assert.True(t, credkind.ValidOnScope(Kind{}, credkind.ScopeAgentIdentity))
	assert.False(t, credkind.ValidOnScope(Kind{}, credkind.ScopeUserIdentity))
	assert.False(t, credkind.ValidOnScope(Kind{}, credkind.ScopeSessionUserIdentity))
}

func TestHasBlock(t *testing.T) {
	assert.False(t, Kind{}.HasBlock(spiceboxv1alpha1.AgentCredential{Type: "directCredentials"}),
		"no block populated")
	assert.True(t, Kind{}.HasBlock(onePasswordCred("demo-dc-cred", "demo-dc-secret")))
}

func TestSecretRefPath(t *testing.T) {
	assert.Equal(t, []string{"directCredentials", "secretRef", "name"}, Kind{}.SecretRefPath())
}

// TestValidateSpec covers both source arms plus the rejection rows the brief
// calls out by name: missing vault with onepassword, vault set with secret,
// a bad key value for each arm, and a missing secretRef.name.
//
// wantNotContain pins the sibling-wide message convention: errors name the
// offending FIELD (and the credential), never the field's VALUE — the same
// shape static/oauth/federated/githubapp's ValidateSpec messages all have.
func TestValidateSpec(t *testing.T) {
	cases := []struct {
		name           string
		cred           spiceboxv1alpha1.AgentCredential
		wantErr        string
		wantNotContain string
	}{
		{
			name:    "missing directCredentials block: rejected",
			cred:    spiceboxv1alpha1.AgentCredential{Name: "demo-dc-cred", Type: "directCredentials"},
			wantErr: "block required for type=directCredentials",
		},
		{
			name: "bad source value: rejected, message does not echo the value",
			cred: spiceboxv1alpha1.AgentCredential{Name: "demo-dc-cred", Type: "directCredentials",
				DirectCredentials: &spiceboxv1alpha1.DirectCredentialsSource{
					Source:    "ldap",
					SecretRef: spiceboxv1alpha1.DirectCredentialSecretRef{Name: "demo-dc-secret"},
				}},
			wantErr:        "source must be",
			wantNotContain: "ldap",
		},
		{
			name: "missing secretRef.name: rejected",
			cred: spiceboxv1alpha1.AgentCredential{Name: "demo-dc-cred", Type: "directCredentials",
				DirectCredentials: &spiceboxv1alpha1.DirectCredentialsSource{
					Source: "secret",
				}},
			wantErr: "secretRef.name required",
		},
		{
			name: "onepassword missing vault: rejected",
			cred: spiceboxv1alpha1.AgentCredential{Name: "demo-dc-cred", Type: "directCredentials",
				DirectCredentials: &spiceboxv1alpha1.DirectCredentialsSource{
					Source:    "onepassword",
					SecretRef: spiceboxv1alpha1.DirectCredentialSecretRef{Name: "demo-dc-secret"},
				}},
			wantErr: "vault required when source=onepassword",
		},
		{
			name: "secret with vault set: rejected, message does not echo the value",
			cred: spiceboxv1alpha1.AgentCredential{Name: "demo-dc-cred", Type: "directCredentials",
				DirectCredentials: &spiceboxv1alpha1.DirectCredentialsSource{
					Source:    "secret",
					SecretRef: spiceboxv1alpha1.DirectCredentialSecretRef{Name: "demo-dc-secret"},
					Vault:     "Engineering",
				}},
			wantErr:        "vault must be empty when source=secret",
			wantNotContain: "Engineering",
		},
		{
			// The bad key is "credential-value", not "password": "password" is
			// a substring of the message's legitimate "source=onepassword", so
			// the no-echo assertion would false-positive on it.
			name: "onepassword bad key: rejected, message does not echo the value",
			cred: spiceboxv1alpha1.AgentCredential{Name: "demo-dc-cred", Type: "directCredentials",
				DirectCredentials: &spiceboxv1alpha1.DirectCredentialsSource{
					Source:    "onepassword",
					SecretRef: spiceboxv1alpha1.DirectCredentialSecretRef{Name: "demo-dc-secret", Key: "credential-value"},
					Vault:     "Engineering",
				}},
			wantErr:        "secretRef.key must be",
			wantNotContain: "credential-value",
		},
		{
			name: "secret with non-empty key: rejected, message does not echo the value",
			cred: spiceboxv1alpha1.AgentCredential{Name: "demo-dc-cred", Type: "directCredentials",
				DirectCredentials: &spiceboxv1alpha1.DirectCredentialsSource{
					Source:    "secret",
					SecretRef: spiceboxv1alpha1.DirectCredentialSecretRef{Name: "demo-dc-secret", Key: "token"},
				}},
			wantErr:        "secretRef.key must be empty when source=secret",
			wantNotContain: "token",
		},
		{
			name: "well-formed onepassword: accepted",
			cred: onePasswordCred("demo-dc-cred", "demo-dc-secret"),
		},
		{
			name: "well-formed onepassword with empty key: accepted",
			cred: spiceboxv1alpha1.AgentCredential{Name: "demo-dc-cred", Type: "directCredentials",
				DirectCredentials: &spiceboxv1alpha1.DirectCredentialsSource{
					Source:    "onepassword",
					SecretRef: spiceboxv1alpha1.DirectCredentialSecretRef{Name: "demo-dc-secret"},
					Vault:     "Engineering",
				}},
		},
		{
			name: "well-formed secret: accepted",
			cred: secretCred("demo-dc-cred", "demo-dc-secret"),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := Kind{}.ValidateSpec(tc.cred)
			if tc.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
			assert.Contains(t, err.Error(), "demo-dc-cred", "the message must name the offending credential")
			if tc.wantNotContain != "" {
				assert.NotContains(t, err.Error(), tc.wantNotContain,
					"the message must name the offending field, never the field's value")
			}
		})
	}
}

func TestSecretRef(t *testing.T) {
	assert.Nil(t, Kind{}.SecretRef(spiceboxv1alpha1.AgentCredential{Type: "directCredentials"}),
		"a malformed credential has no Secret to point at; validation reports it, SecretRef does not guess")

	ref := Kind{}.SecretRef(onePasswordCred("demo-dc-cred", "demo-op-secret"))
	require.NotNil(t, ref)
	assert.Equal(t, "demo-op-secret", ref.Name)
	assert.Equal(t, "token", ref.Key, "onepassword's Connect token lives under one named key")

	ref = Kind{}.SecretRef(secretCred("demo-dc-cred", "demo-login-secret"))
	require.NotNil(t, ref)
	assert.Equal(t, "demo-login-secret", ref.Name)
	assert.Empty(t, ref.Key, "the secret backend's login Secret is a multi-key shape, not one named value")
}

func TestRequiredSecretKeys(t *testing.T) {
	assert.Nil(t, Kind{}.RequiredSecretKeys(spiceboxv1alpha1.AgentCredential{Type: "directCredentials"}),
		"nil block must fail closed to no requirement, not a nil dereference")
	assert.Equal(t, []string{"token"}, Kind{}.RequiredSecretKeys(onePasswordCred("demo-dc-cred", "demo-dc-secret")))
	assert.Nil(t, Kind{}.RequiredSecretKeys(secretCred("demo-dc-cred", "demo-dc-secret")),
		"the secret backend's Secret shape is not a fixed single key")
}

// TestPublicSecretKeys_FailsClosed pins the brief's answer directly: every
// key is secret, regardless of source arm or even a malformed/absent block.
func TestPublicSecretKeys_FailsClosed(t *testing.T) {
	assert.Nil(t, Kind{}.PublicSecretKeys(onePasswordCred("demo-dc-cred", "demo-dc-secret")),
		"every key is secret: there is no public identifier in a website login")
	assert.Nil(t, Kind{}.PublicSecretKeys(secretCred("demo-dc-cred", "demo-dc-secret")))
	assert.Nil(t, Kind{}.PublicSecretKeys(spiceboxv1alpha1.AgentCredential{Type: "directCredentials"}))
}

func TestBuildCredential_NotWritableBySetup(t *testing.T) {
	_, err := Kind{}.BuildCredential("demo-dc-cred", "demo-dc-secret", "demo-dc-key")
	require.Error(t, err, "the credential is authored on the identity, so the setup flow cannot write one")
	assert.Contains(t, err.Error(), "demo-dc-cred", "the message must name the offending credential")
}

func TestReadStoredValue_AlwaysErrors(t *testing.T) {
	_, err := Kind{}.ReadStoredValue(context.Background(), nil, "default", onePasswordCred("demo-dc-cred", "demo-dc-secret"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "demo-dc-cred", "the message must name the offending credential")
}

// TestResolve_AlwaysErrors_EvenOnPerfectInput pins the one invariant that
// matters most for this type: it never flows through the broker, no matter
// how well-formed the descriptor handed to Resolve is.
func TestResolve_AlwaysErrors_EvenOnPerfectInput(t *testing.T) {
	_, _, err := Kind{}.Resolve(context.Background(), credkind.Deps{}, spiceboxv1alpha1.CredentialSource{
		Type: "directCredentials", Namespace: "default", Name: "demo-op-secret", Key: "token",
	})
	require.Error(t, err)
	assert.Equal(t,
		"directCredentials never project into env vars or headers; they are fetched at use time by the browser filler",
		err.Error())
}

// TestMintedKindsAreSubjectExclusiveAndStoreNothing locally pins the same
// facts the repo-wide registry sweep
// (pkg/platform/identity/credkind/enum_invariant_test.go) asserts: a minted
// kind serves one subject kind only and stores nothing refreshable.
func TestMintedKindsAreSubjectExclusiveAndStoreNothing(t *testing.T) {
	k := Kind{}
	assert.False(t, credkind.ValidOnScope(k, credkind.ScopeUserIdentity) &&
		credkind.ValidOnScope(k, credkind.ScopeAgentIdentity))
	assert.False(t, k.NeedsRefresh())
	_, err := k.BuildCredential("c", "s", "k")
	assert.Error(t, err)
}
