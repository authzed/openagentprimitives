package sessioncmd

import (
	"bytes"
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/kube"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	// Registers the shipped credkind.Kinds against THIS test binary's registry.
	// cmd/oap's main.go carries the same blank import for the real binary; a
	// test binary is its own process and would otherwise dispatch into an empty
	// registry, which fails closed and would make every assertion below a
	// tautology about the failure path rather than about the answer.
	_ "github.com/authzed/openagentprimitives/pkg/platform/identity/credkind/imports"
	"github.com/authzed/openagentprimitives/pkg/steelthread"
)

// ghCredsSecret is a GitHub App credentials Secret with the four keys the
// wizard writes. The values are obvious fixtures; only their presence and which
// key they sit under matter here.
func ghCredsSecret(ns, name string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Data: map[string][]byte{
			"app-id":          []byte("fixture-only-app-id-424242"),
			"installation-id": []byte("fixture-only-installation-id-777777"),
			"private-key":     []byte("fixture-only-private-key-material"),
			"webhook-secret":  []byte("fixture-only-webhook-secret-material"),
		},
	}
}

// publicByName indexes a gather's answer as name -> Public, so a case can state
// the whole expectation as one map and a failure names the key. The VALUES are
// deliberately not carried through: this file never compares one, and a helper
// that returned them would be one refactor away from printing one.
func publicByName(got []steelthread.LiveSecret) map[string]bool {
	out := map[string]bool{}
	for _, s := range got {
		out[s.Name] = s.Public
	}
	return out
}

// TestGatherLiveSecrets_MarksOnlyTheDeclaredPublicKeys is the gathering half of
// the fix, and the case it is drawn from is the one that refused.
//
// A GitHub App's Secret holds a private key and a webhook secret beside an app
// id and an installation id. The first two are the credential; GitHub publishes
// the other two, and it puts the installation id in the BODY of every webhook it
// delivers — so a capture that treats "read out of a Secret" as "is a secret"
// refuses every triggered GitHub session forever, over a value the delivery
// record cannot omit and redaction must not touch.
//
// Three rows, because the same Secret is named by two registries and either
// reference can exist alone: a Channel that mints its own tokens needs no
// AgentIdentity credential, and an identity credential can be declared for an
// agent with no github Channel. Closing one seam only would leave the other
// configuration still refusing.
func TestGatherLiveSecrets_MarksOnlyTheDeclaredPublicKeys(t *testing.T) {
	const ns = "default"
	const secretName = "demo-gh-creds"

	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-session", Namespace: ns},
	}
	ghChannel := &spiceboxv1alpha1.Channel{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-gh", Namespace: ns},
		Spec: spiceboxv1alpha1.ChannelSpec{
			Kind:           "github",
			CredentialsRef: spiceboxv1alpha1.ChannelCredentialsRef{SecretName: secretName},
		},
	}
	ghIdentity := &spiceboxv1alpha1.AgentIdentity{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-id", Namespace: ns},
		Spec: spiceboxv1alpha1.AgentIdentitySpec{
			Credentials: []spiceboxv1alpha1.AgentCredential{{
				Name: "github-app", Type: "githubApp",
				GitHubApp: &spiceboxv1alpha1.GitHubAppCredentialSource{
					SecretRef: spiceboxv1alpha1.SecretRef{Name: secretName},
				},
			}},
		},
	}

	cases := []struct {
		name string
		in   steelthread.FixtureInput
	}{
		{
			name: "the Channel alone declares: the two published identifiers are public",
			in:   steelthread.FixtureInput{Channels: []*spiceboxv1alpha1.Channel{ghChannel}},
		},
		{
			name: "the identity credential alone declares: same answer",
			in:   steelthread.FixtureInput{Identity: ghIdentity},
		},
		{
			name: "both declare the same Secret: unioned, still the same answer",
			in: steelthread.FixtureInput{
				Channels: []*spiceboxv1alpha1.Channel{ghChannel}, Identity: ghIdentity,
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var warn bytes.Buffer
			got := gatherLiveSecrets(context.Background(),
				&kube.Bundle{Controller: fakeChannelClient(t, ghCredsSecret(ns, secretName)), Namespace: ns},
				sess, tc.in, &warn)

			assert.Empty(t, warn.String(), "every kind involved is registered; nothing should warn")
			require.Len(t, got, 4, "one entry per non-empty data key")

			assert.Equal(t, map[string]bool{
				secretName + "/app-id":          true,
				secretName + "/installation-id": true,
				secretName + "/private-key":     false,
				secretName + "/webhook-secret":  false,
			}, publicByName(got),
				"the two GitHub publishes are public; the signing key and the delivery secret are not")
		})
	}
}

// TestGatherLiveSecrets_AnUndeclaringKindLeavesEveryKeySecret is the fail-closed
// default, gathered rather than asserted on the struct: a Secret reached through
// a kind that declares nothing must come back with nothing marked.
//
// This is the answer a credential type or channel kind added later gets for
// free, and it must be the SAFE one — otherwise adding a type silently opens a
// hole in a leak scan nobody would think to re-check.
func TestGatherLiveSecrets_AnUndeclaringKindLeavesEveryKeySecret(t *testing.T) {
	const ns = "default"
	const secretName = "demo-slack-creds"

	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-session", Namespace: ns},
	}
	ch := &spiceboxv1alpha1.Channel{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-slack", Namespace: ns},
		Spec: spiceboxv1alpha1.ChannelSpec{
			Kind:           "slack",
			CredentialsRef: spiceboxv1alpha1.ChannelCredentialsRef{SecretName: secretName},
		},
	}

	var warn bytes.Buffer
	got := gatherLiveSecrets(context.Background(),
		&kube.Bundle{Controller: fakeChannelClient(t,
			secretWith(ns, secretName, "bot-token", "xoxb-fixture-only-000111222333")), Namespace: ns},
		sess, steelthread.FixtureInput{Channels: []*spiceboxv1alpha1.Channel{ch}}, &warn)

	require.Len(t, got, 1)
	assert.False(t, got[0].Public,
		"slack declares no public keys, so every key of its Secret stays scanned")
	assert.Empty(t, warn.String())
}

// TestGatherLiveSecrets_AnUnregisteredKindWarnsAndDeclaresNothing is the other
// fail-closed edge, and it is the one a silent implementation would get wrong.
//
// An unknown kind and a kind that declares nothing hand back the same empty
// list. Only the error distinguishes them, and only a warning puts that error
// where an operator will see it — this repo has already paid for one silent
// error on this path.
func TestGatherLiveSecrets_AnUnregisteredKindWarnsAndDeclaresNothing(t *testing.T) {
	const ns = "default"
	const secretName = "demo-mystery-creds"

	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-session", Namespace: ns},
	}
	ch := &spiceboxv1alpha1.Channel{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-mystery", Namespace: ns},
		Spec: spiceboxv1alpha1.ChannelSpec{
			Kind:           "nosuchkind",
			CredentialsRef: spiceboxv1alpha1.ChannelCredentialsRef{SecretName: secretName},
		},
	}

	var warn bytes.Buffer
	got := gatherLiveSecrets(context.Background(),
		&kube.Bundle{Controller: fakeChannelClient(t,
			secretWith(ns, secretName, "token", "fixture-only-mystery-token")), Namespace: ns},
		sess, steelthread.FixtureInput{Channels: []*spiceboxv1alpha1.Channel{ch}}, &warn)

	require.Len(t, got, 1, "the Secret is still READ and still scanned; only the declaration is missing")
	assert.False(t, got[0].Public)
	assert.Contains(t, warn.String(), "nosuchkind",
		"the warning must name the kind, or an operator cannot fix the wiring")
	assert.Contains(t, warn.String(), "treated as secret",
		"the warning must say which way it failed, since both ways return no keys")
}
