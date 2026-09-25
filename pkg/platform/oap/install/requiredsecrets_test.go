// pkg/platform/oap/install/requiredsecrets_test.go
//
// Unit tests for the questions install synthesizes from a bundle's
// requires.secrets declarations.
//
// The defect these pin: a bundle declared a Secret it needs, install applied
// every CR, reported success, and left the agent at Valid=False through
// AgentClass -> AgentIdentity -> "credentials[...]: secret missing" — because
// nothing had ever asked for the credential. The remedy was a `kubectl create
// secret generic` in the bundle's README, run BEFORE installing.
package install

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/authzed/openagentprimitives/pkg/platform/oap"
)

const reqSecretNS = "req-secret-ns"

// liveSecret is a Secret already sitting in the install's target namespace.
// Its data is deliberately non-empty so a probe that pulled the value bytes
// would have something to pull — see TestRequiredSecretQuestions_ProbeIsMetadataOnly.
func liveSecret(name string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: reqSecretNS, Name: name},
		Data:       map[string][]byte{"api-key": []byte("already-here")},
	}
}

// secretReader is a fake cluster holding exactly objs. The clientgo scheme is
// registered because the probe reads a metav1.PartialObjectMetadata, which the
// fake resolves through the scheme like any other typed object.
func secretReader(t *testing.T, objs ...client.Object) client.Reader {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, scheme.AddToScheme(s), "register the clientgo scheme")
	return fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()
}

// reqSecretManifest is a manifest declaring exactly rs plus qs. Nothing else
// about the manifest matters here: RequiredSecretQuestions reads only
// requires.secrets and the question names.
func reqSecretManifest(rs []oap.RequiredSecret, qs []oap.Question) *oap.Manifest {
	return &oap.Manifest{
		OapFormatVersion: "1",
		Agent:            oap.Agent{Name: "fixture-agent", Version: "1.0.0"},
		Requires:         oap.Requires{Secrets: rs},
		Questions:        qs,
	}
}

// questionNames is the synthesized set, by name, for comparison against an
// expectation written in declaration order. Nil for an empty set, so "asked
// nothing" is written as nil in the table rather than as an empty literal.
func questionNames(qs []oap.Question) []string {
	if len(qs) == 0 {
		return nil
	}
	out := make([]string, 0, len(qs))
	for _, q := range qs {
		out = append(out, q.Name)
	}
	return out
}

func TestRequiredSecretQuestions(t *testing.T) {
	cases := []struct {
		name       string
		declared   []oap.RequiredSecret
		questions  []oap.Question
		live       []client.Object
		namePrefix string
		wantNames  []string
		wantNotice string // substring every notice set must carry; "" means no notices
	}{
		{
			name:      "keyed and absent: one question per declared key, in declaration order",
			declared:  []oap.RequiredSecret{{Name: "widget-token", Keys: []string{"api-key", "org"}}},
			wantNames: []string{"requires.secrets.widget-token.api-key", "requires.secrets.widget-token.org"},
		},
		{
			name:      "keyed and already present: not asked — this is the orExisting case",
			declared:  []oap.RequiredSecret{{Name: "widget-token", Keys: []string{"api-key"}}},
			live:      []client.Object{liveSecret("widget-token")},
			wantNames: nil,
		},
		{
			name:     "keyed, absent, and the bundle wired its own question: not asked twice",
			declared: []oap.RequiredSecret{{Name: "widget-token", Keys: []string{"api-key"}, Question: "widgetToken"}},
			questions: []oap.Question{{
				Name: "widgetToken", Type: oap.QSecret, Prompt: "Widget token",
				Secret: &oap.SecretQuestion{CreateSecret: &oap.SecretTarget{Name: "widget-token", Key: "api-key"}},
			}},
			wantNames: nil,
		},
		{
			name:       "keyless and absent: never asked, reported instead",
			declared:   []oap.RequiredSecret{{Name: "oauth-creds", Purpose: "the token set a setup flow mints"}},
			wantNames:  nil,
			wantNotice: "oauth-creds",
		},
		{
			name:      "keyless and present: neither asked nor reported",
			declared:  []oap.RequiredSecret{{Name: "oauth-creds"}},
			live:      []client.Object{liveSecret("oauth-creds")},
			wantNames: nil,
		},
		{
			name:      "nothing declared: an unrelated Secret in the namespace is never asked for",
			declared:  nil,
			live:      []client.Object{liveSecret("someone-elses-secret")},
			wantNames: nil,
		},
		{
			name:       "--name install: the probe checks the PREFIXED Secret this install would create",
			declared:   []oap.RequiredSecret{{Name: "widget-token", Keys: []string{"api-key"}}},
			live:       []client.Object{liveSecret("widget-token")}, // the UNprefixed one
			namePrefix: "demo-",
			wantNames:  []string{"requires.secrets.widget-token.api-key"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			qs, notices, err := RequiredSecretQuestions(context.Background(),
				secretReader(t, tc.live...), reqSecretManifest(tc.declared, tc.questions), reqSecretNS, tc.namePrefix)
			require.NoError(t, err, "a readable cluster must not fail the probe")

			assert.Equal(t, tc.wantNames, questionNames(qs))
			if tc.wantNotice == "" {
				assert.Empty(t, notices, "nothing here is owed the operator a warning")
				return
			}
			require.Len(t, notices, 1, "one notice per declaration install cannot collect")
			assert.Contains(t, notices[0], tc.wantNotice, "the notice must name the Secret the operator has to create")
		})
	}
}

// TestRequiredSecretQuestions_ShapeIsAnOrdinaryCreateSecretQuestion pins that a
// synthesized question is the SAME shape a bundle author writes by hand:
// type=secret with a createSecret target and orExisting. That is what makes
// Resolve materialize it into a SecretSpec and Install create it, rather than
// needing a second pipeline of its own.
func TestRequiredSecretQuestions_ShapeIsAnOrdinaryCreateSecretQuestion(t *testing.T) {
	qs, _, err := RequiredSecretQuestions(context.Background(), secretReader(t),
		reqSecretManifest([]oap.RequiredSecret{{
			Name:    "widget-token",
			Keys:    []string{"api-key"},
			Purpose: "the API key the agent authenticates with",
		}}, nil), reqSecretNS, "demo-")
	require.NoError(t, err)
	require.Len(t, qs, 1)
	q := qs[0]

	assert.Equal(t, oap.QSecret, q.Type)
	assert.True(t, q.IsRequired(), "a declared credential is not optional — the agent cannot run without it")
	assert.Contains(t, q.Description, "the API key the agent authenticates with", "the declaration's purpose is what the operator reads")
	require.NotNil(t, q.Secret, "type=secret requires a secret block")
	assert.True(t, q.Secret.OrExisting)
	require.NotNil(t, q.Secret.CreateSecret, "without a createSecret target Resolve refuses the answer it collected")
	// The DECLARED name, not the prefixed one: install.Install renames the
	// Secrets it creates alongside the CRs that reference them, so prefixing
	// here would produce "demo-demo-widget-token".
	assert.Equal(t, "widget-token", q.Secret.CreateSecret.Name)
	assert.Equal(t, "api-key", q.Secret.CreateSecret.Key)

	// The name lives under a prefix ValidateQuestions refuses a MANIFEST
	// question for, which is what keeps a bundle from declaring a question that
	// collides with one of these in Resolve's answer map.
	assert.True(t, strings.HasPrefix(q.Name, oap.RequiredSecretQuestionPrefix), "q.Name = %q", q.Name)
	assert.Error(t, reqSecretManifest(nil, []oap.Question{{Name: q.Name, Type: oap.QString,
		Binding: []oap.Binding{{Target: "AgentClass/a#spec.x"}}}}).ValidateQuestions(),
		"a bundle must not be able to author a question in the synthesized namespace")
}

// TestRequiredSecretQuestions_ProbeIsMetadataOnly pins that deciding whether to
// ask never pulls an existing Secret's value bytes into the installer. The
// question is "does this exist", and a typed Get would answer it by reading a
// credential the operator did not ask this command to handle.
func TestRequiredSecretQuestions_ProbeIsMetadataOnly(t *testing.T) {
	s := runtime.NewScheme()
	require.NoError(t, scheme.AddToScheme(s))

	var sawKinds []string
	r := fake.NewClientBuilder().WithScheme(s).WithObjects(liveSecret("widget-token")).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				sawKinds = append(sawKinds, fmt.Sprintf("%T", obj))
				return c.Get(ctx, key, obj, opts...)
			},
		}).Build()

	_, _, err := RequiredSecretQuestions(context.Background(), r,
		reqSecretManifest([]oap.RequiredSecret{{Name: "widget-token", Keys: []string{"api-key"}}}, nil), reqSecretNS, "")
	require.NoError(t, err)
	assert.Equal(t, []string{"*v1.PartialObjectMetadata"}, sawKinds,
		"existence is a metadata question; a typed Secret Get would read the credential's bytes to answer it")
}

// TestRequiredSecretQuestions_UnreadableClusterIsFatal covers the fail-closed
// half: a probe that could not run must not be read as "the Secret is present".
// Guessing that way is how the install reports success and the agent sits at
// Valid=False, which is the whole defect.
func TestRequiredSecretQuestions_UnreadableClusterIsFatal(t *testing.T) {
	s := runtime.NewScheme()
	require.NoError(t, scheme.AddToScheme(s))
	r := fake.NewClientBuilder().WithScheme(s).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
				return assert.AnError
			},
		}).Build()

	_, _, err := RequiredSecretQuestions(context.Background(), r,
		reqSecretManifest([]oap.RequiredSecret{{Name: "widget-token", Keys: []string{"api-key"}}}, nil), reqSecretNS, "")
	require.Error(t, err, "an unreadable cluster must abort the install, not silently skip the question")
	assert.Contains(t, err.Error(), "widget-token", "the error must name the Secret whose presence could not be settled")
}

// TestRequiredSecretQuestions_NamespaceRequired covers the one caller mistake
// that would otherwise probe the wrong scope: a namespaced Get with no
// namespace reads whatever the reader defaults to, which is not the namespace
// the Secret will be created in.
func TestRequiredSecretQuestions_NamespaceRequired(t *testing.T) {
	_, _, err := RequiredSecretQuestions(context.Background(), secretReader(t),
		reqSecretManifest([]oap.RequiredSecret{{Name: "widget-token", Keys: []string{"api-key"}}}, nil), "", "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "namespace")
}

// TestResolve_SynthesizedSecretQuestion covers the join: the questions above
// are answered through the SAME resolver a manifest's own questions go through,
// and come out as the SecretSpec install.Install creates.
func TestResolve_SynthesizedSecretQuestion(t *testing.T) {
	qs, _, err := RequiredSecretQuestions(context.Background(), secretReader(t),
		reqSecretManifest([]oap.RequiredSecret{{Name: "widget-token", Keys: []string{"api-key"}}}, nil), reqSecretNS, "")
	require.NoError(t, err)
	require.Len(t, qs, 1)

	t.Run("--set answers it: the value becomes the Secret install creates", func(t *testing.T) {
		ans, secrets, err := Resolve(qs, "", map[string]string{"requires.secrets.widget-token.api-key": "sk-fixture-value"}, false)
		require.NoError(t, err)
		assert.Empty(t, ans, "a secret answer never reaches the answer map that overlays CR fields")
		require.Len(t, secrets, 1)
		assert.Equal(t, SecretSpec{Name: "widget-token", Key: "api-key", Value: "sk-fixture-value"}, secrets[0])
	})

	t.Run("nobody answers it and nobody can be asked: refused, naming the question", func(t *testing.T) {
		_, _, err := Resolve(qs, "", nil, false)
		require.Error(t, err)

		var missing *MissingAnswersError
		require.ErrorAs(t, err, &missing,
			"the refusal must be typed, so a caller can name the flag that would answer it — the flag differs per surface")
		assert.Equal(t, []string{"requires.secrets.widget-token.api-key"}, missing.Names)
		assert.Contains(t, err.Error(), "widget-token", "the message must name the Secret")
	})
}
