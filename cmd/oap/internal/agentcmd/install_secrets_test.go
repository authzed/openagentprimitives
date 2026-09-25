package agentcmd

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/kube"
	"github.com/authzed/openagentprimitives/pkg/authz/adoptguard"
)

// This file drives `oap agent install`'s own RunE for a bundle that declares a
// Secret under requires.secrets, against a fake cluster.
//
// The defect: an install of exactly this shape reported success and left the
// agent at Valid=False — AgentClass -> AgentIdentity -> "credentials[...]:
// secret missing" — because nothing had asked for the credential. The bundle's
// README documented a `kubectl create secret generic` to be run BEFORE
// installing, and skipping it still installed cleanly.

const (
	secretFixtureAgent    = "secret-fixture-agent"
	secretFixtureIdentity = "secret-fixture-id"
	secretFixtureName     = "widget-token" // the declared Secret; a made-up fixture name
	secretFixtureKey      = "api-key"
	// The exact --set / --values key an operator supplies for the declaration
	// above. Spelled out rather than derived so a change to the naming rule
	// fails here, at the surface an operator actually types.
	secretFixtureAnswerKey = "requires.secrets.widget-token.api-key"
)

// secretBundleDir writes a self-contained .oap source folder whose one required
// Secret is DERIVED from the bundled AgentIdentity's credential. The credential
// block is written verbatim, so a case can produce the keyed form (a static
// credential naming a Secret + key) or the keyless form (an oauth credential
// naming a whole Secret). requires.secrets is inherited from this graph, never
// authored in oap.yaml.
//
// Package-local rather than the shared oaptest fixture for the same reason
// capacityBundleDir is: that fixture carries its own required questions, which
// every case here would then have to answer for reasons unrelated to the
// property under test.
func secretBundleDir(t *testing.T, credential string) string {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "manifests"), 0o755))
	// agent.name and requires.secrets are inherited from the graph below.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "oap.yaml"), []byte(`
oapFormatVersion: "1"
agent:
  version: "1.0.0"
`), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "manifests", "agent.yaml"), []byte(`
apiVersion: agentprimitives.authzed.com/v1alpha1
kind: AgentClass
metadata:
  name: `+secretFixtureAgent+`
spec:
  description: Fixture agent for the required-secret install tests.
  agentIdentity: `+secretFixtureIdentity+`
---
apiVersion: agentprimitives.authzed.com/v1alpha1
kind: AgentIdentity
metadata:
  name: `+secretFixtureIdentity+`
spec:
  credentials:
`+credential), 0o644))
	return dir
}

// keyedCredential is a static credential naming the Secret AND a key, so
// requires.secrets derives {name, keys:[key]} — the shape whose value install
// collects via a synthesized secret question.
const keyedCredential = `    - name: widget-cred
      type: static
      static:
        secretRef:
          name: ` + secretFixtureName + `
          key: ` + secretFixtureKey + `
`

// keylessCredential is an oauth credential naming the whole Secret, so
// requires.secrets derives {name, keys:[]}. There is no key to write a typed
// answer under, so install never asks for it.
const keylessCredential = `    - name: widget-cred
      type: oauth
      oauth:
        secretRef:
          name: ` + secretFixtureName + `
`

// existingSecret is the declared Secret already sitting in the install's target
// namespace, with a value no install may overwrite.
func existingSecret() *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: capacityFixtureNS, Name: secretFixtureName},
		Data:       map[string][]byte{secretFixtureKey: []byte("value-that-was-already-here")},
	}
}

// getInstalledSecret reads the declared Secret back out of the fake cluster.
func getInstalledSecret(t *testing.T, kb *kube.Bundle) (*corev1.Secret, error) {
	t.Helper()
	var got corev1.Secret
	err := kb.Controller.Get(context.Background(),
		client.ObjectKey{Namespace: capacityFixtureNS, Name: secretFixtureName}, &got)
	return &got, err
}

// secretValue is what a Secret holds under key, wherever this fixture left it.
//
// Install writes the collected credential into stringData, which a real
// apiserver moves into data on admission. The fake client performs no
// admission at all, so a Secret it stored keeps the value in StringData — a
// fixture artifact, not a difference in what install writes. Both are read so
// the assertion says the same thing here and against envtest (see
// pkg/platform/oap/install's TestInstall_AppliesAgentClassAndSecret_ReinstallIsIdempotent,
// which reads Data against a real apiserver).
func secretValue(t *testing.T, sec *corev1.Secret, key string) string {
	t.Helper()
	if v, ok := sec.Data[key]; ok {
		return string(v)
	}
	return sec.StringData[key]
}

// getInstalledAgentClass reads the bundled AgentClass back, so a case can say
// whether the install applied anything at all.
func getInstalledAgentClass(t *testing.T, kb *kube.Bundle) error {
	t.Helper()
	got := &unstructured.Unstructured{}
	got.SetAPIVersion("agentprimitives.authzed.com/v1alpha1")
	got.SetKind("AgentClass")
	return kb.Controller.Get(context.Background(),
		client.ObjectKey{Namespace: capacityFixtureNS, Name: secretFixtureAgent}, got)
}

func TestAgentInstall_DeclaredSecret(t *testing.T) {
	forceNonInteractiveStdin(t)

	t.Run("keyed and absent, answered by --set: the Secret is created from the value supplied", func(t *testing.T) {
		kb := fakeBundle(t, vmNode())
		g := &apcmd.Globals{BundleFn: func() (*kube.Bundle, error) { return kb, nil }}

		out, err := runAgentInstall(t, g, secretBundleDir(t, keyedCredential),
			"--set", secretFixtureAnswerKey+"=sk-fixture-value")
		require.NoErrorf(t, err, "an answered declaration must install cleanly; out=%s", out)
		assert.NotContains(t, out, "sk-fixture-value", "the credential must never be echoed back")

		sec, gerr := getInstalledSecret(t, kb)
		require.NoError(t, gerr, "install must create the Secret the bundle declared")
		assert.Equal(t, "sk-fixture-value", secretValue(t, sec, secretFixtureKey),
			"the value lands under the declared key, where the CRs read it")
		assert.Contains(t, sec.Labels, adoptguard.AdoptedLabel,
			"a hand-created Secret lacks this and is invisible to the operator's Secret watch; an install-created one must not be")
	})

	t.Run("keyed and absent, nobody to ask: refused naming the Secret and the flag, nothing applied", func(t *testing.T) {
		kb := fakeBundle(t, vmNode())
		g := &apcmd.Globals{BundleFn: func() (*kube.Bundle, error) { return kb, nil }}

		out, err := runAgentInstall(t, g, secretBundleDir(t, keyedCredential))
		require.Errorf(t, err, "installing into a permanent Valid=False is the defect; it must refuse instead; out=%s", out)
		assert.Contains(t, err.Error(), secretFixtureName, "the refusal must name the Secret")
		assert.Contains(t, err.Error(), secretFixtureAnswerKey, "the refusal must name the exact key that answers it")
		assert.Contains(t, err.Error(), "--set", "the refusal must name the flag that would supply it")

		assert.Error(t, getInstalledAgentClass(t, kb),
			"a refusal before the first cluster write means no CR is applied")
	})

	t.Run("keyed and already present: not asked, and the existing value is left alone", func(t *testing.T) {
		kb := fakeBundle(t, vmNode(), existingSecret())
		g := &apcmd.Globals{BundleFn: func() (*kube.Bundle, error) { return kb, nil }}

		out, err := runAgentInstall(t, g, secretBundleDir(t, keyedCredential))
		require.NoErrorf(t, err, "a Secret that already exists is the orExisting case and must not be asked for; out=%s", out)
		require.NoError(t, getInstalledAgentClass(t, kb), "the bundle must still install")

		sec, gerr := getInstalledSecret(t, kb)
		require.NoError(t, gerr)
		assert.Equal(t, "value-that-was-already-here", secretValue(t, sec, secretFixtureKey),
			"install must not overwrite a credential it never collected")
	})

	t.Run("keyless and absent: never asked, installed, and reported as the operator's to create", func(t *testing.T) {
		kb := fakeBundle(t, vmNode())
		g := &apcmd.Globals{BundleFn: func() (*kube.Bundle, error) { return kb, nil }}

		out, err := runAgentInstall(t, g, secretBundleDir(t, keylessCredential))
		require.NoErrorf(t, err, "a keyless declaration names no value to type, so it cannot block an install; out=%s", out)
		require.NoError(t, getInstalledAgentClass(t, kb), "the bundle must still install")

		assert.Contains(t, out, secretFixtureName, "the operator must be told which Secret is still owed")

		_, gerr := getInstalledSecret(t, kb)
		assert.Error(t, gerr, "install must not invent a Secret whose keys nobody declared")
	})

	t.Run("undeclared: a --set naming no declaration is refused rather than silently dropped", func(t *testing.T) {
		kb := fakeBundle(t, vmNode())
		g := &apcmd.Globals{BundleFn: func() (*kube.Bundle, error) { return kb, nil }}

		_, err := runAgentInstall(t, g, secretBundleDir(t, keylessCredential),
			"--set", secretFixtureAnswerKey+"=sk-fixture-value")
		require.Error(t, err, "a keyless declaration synthesizes no question, so this key names nothing")
		assert.Contains(t, err.Error(), secretFixtureAnswerKey)
	})
}
