package publicendpoint

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/kube"
	"github.com/authzed/openagentprimitives/pkg/platform/cloud"
)

// seededTokenSecret is a Secret already holding a token, standing in for a
// cluster whose operator supplied one earlier.
func seededTokenSecret() *corev1.Secret {
	sec := &corev1.Secret{}
	sec.Namespace = cloud.WebdServiceNamespace
	sec.Name = NgrokAuthTokenSecret
	sec.Data = map[string][]byte{NgrokAuthTokenKey: []byte("supplied-earlier")}
	return sec
}

// keylessTokenSecret is a Secret that EXISTS and holds no token: an operator
// who created it by hand and stopped, an External Secrets sync that has not
// landed a value, a key spelled differently. It is the object whose mere
// presence must not be read as an answer.
func keylessTokenSecret() *corev1.Secret {
	sec := &corev1.Secret{}
	sec.Namespace = cloud.WebdServiceNamespace
	sec.Name = NgrokAuthTokenSecret
	sec.Data = map[string][]byte{"unrelated": []byte("not the token")}
	return sec
}

// TestEnsureNgrokAuthTokenSecret_AsksWhenThereIsSomeoneToAsk covers the whole
// decision: the environment answers first because a scripted run has nobody at
// a terminal, an operator is asked only when nothing else answered, and a
// cluster that already holds the credential is never asked at all.
func TestEnsureNgrokAuthTokenSecret_AsksWhenThereIsSomeoneToAsk(t *testing.T) {
	cases := []struct {
		name string
		env  string
		// seed is the Secret already in the cluster, or nil for none.
		seed       *corev1.Secret
		ask        TokenPrompter
		wantAsked  bool
		wantSecret string // "" means no usable token stored
		wantSays   string
	}{
		{
			name:       "nobody to ask and nothing in the environment: the endpoint stays Pending, and says so",
			ask:        nil,
			wantSecret: "",
			wantSays:   ngrokAuthTokenEnv,
		},
		{
			name:       "an interactive run with nothing in the environment is asked, and the answer lands",
			ask:        func(context.Context) (string, error) { return "typed-by-the-operator", nil },
			wantAsked:  true,
			wantSecret: "typed-by-the-operator",
		},
		{
			name:       "the environment answers first: a scripted run is never blocked on a prompt",
			env:        "from-the-shell",
			ask:        func(context.Context) (string, error) { return "typed-by-the-operator", nil },
			wantAsked:  false,
			wantSecret: "from-the-shell",
		},
		{
			// The cluster answers first, so a credential that is already there
			// is neither overwritten from a stale shell nor re-asked for.
			name:       "a cluster that already holds the credential is not asked, nor told it lacks one",
			seed:       seededTokenSecret(),
			ask:        func(context.Context) (string, error) { return "typed-by-the-operator", nil },
			wantAsked:  false,
			wantSecret: "supplied-earlier",
			wantSays:   "leaving it alone",
		},
		{
			name:       "an operator who declines leaves the endpoint Pending rather than storing an empty credential",
			ask:        func(context.Context) (string, error) { return "   ", nil },
			wantAsked:  true,
			wantSecret: "",
		},
		// The three rows below are all the same fact: what settles the question
		// is a Secret that HOLDS a token, never one that merely exists.
		//
		// A Secret present with no token key answered nothing, so reading its
		// presence as an answer suppressed the environment AND the prompt at
		// once. Nothing failed: the endpoint sat at Pending/AuthTokenSecretMissing
		// with the run reporting it had left a working credential alone, and the
		// one person who could fix it was never asked.
		{
			name:       "a Secret with no token key does not suppress the environment",
			seed:       keylessTokenSecret(),
			env:        "from-the-shell",
			ask:        func(context.Context) (string, error) { return "typed-by-the-operator", nil },
			wantAsked:  false,
			wantSecret: "from-the-shell",
		},
		{
			name:       "a Secret with no token key does not suppress the prompt either",
			seed:       keylessTokenSecret(),
			ask:        func(context.Context) (string, error) { return "typed-by-the-operator", nil },
			wantAsked:  true,
			wantSecret: "typed-by-the-operator",
		},
		{
			name:       "a Secret with no token key and nobody to ask: still Pending, and still says so",
			seed:       keylessTokenSecret(),
			ask:        nil,
			wantSecret: "",
			wantSays:   ngrokAuthTokenEnv,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(ngrokAuthTokenEnv, tc.env)
			ctx := context.Background()

			b := fake.NewClientBuilder().WithScheme(kube.Scheme)
			if tc.seed != nil {
				b = b.WithObjects(tc.seed)
			}
			c := b.Build()

			// Wrapped rather than counted inside each case's closure so
			// "was it asked" is observed the same way for every row,
			// including the nil one.
			var asked bool
			ask := tc.ask
			if ask != nil {
				inner := ask
				ask = func(ctx context.Context) (string, error) {
					asked = true
					return inner(ctx)
				}
			}

			var out bytes.Buffer
			require.NoError(t, ensureNgrokAuthTokenSecret(ctx, &out, c, ask))

			assert.Equal(t, tc.wantAsked, asked, "whether the operator was prompted")

			// The claim is about the stored TOKEN, not about the object: a run
			// that got no usable answer must leave none behind, whether that
			// means no Secret at all or the keyless one it was handed.
			var stored string
			var sec corev1.Secret
			err := c.Get(ctx, types.NamespacedName{
				Namespace: cloud.WebdServiceNamespace, Name: NgrokAuthTokenSecret,
			}, &sec)
			switch {
			case err == nil:
				stored = string(sec.Data[NgrokAuthTokenKey])
			case apierrors.IsNotFound(err):
				require.Empty(t, tc.wantSecret, "a stored token was expected, but no Secret exists")
			default:
				require.NoError(t, err, "reading back the Secret")
			}
			assert.Equal(t, tc.wantSecret, stored, "the token the cluster ends up holding")

			if tc.wantSays != "" {
				assert.Contains(t, out.String(), tc.wantSays)
			}
			assert.NotContains(t, out.String(), "typed-by-the-operator",
				"a credential is never echoed back into the transcript")
			assert.NotContains(t, out.String(), "from-the-shell",
				"nor is one taken from the environment")
		})
	}
}

// TestEnsureNgrokAuthTokenSecret_APromptFailureIsNotSwallowed: an operator
// interrupting the question, or the terminal dying mid-answer, must not read as
// "declined". That would leave a run which just announced it was making this
// cluster public with no credential and no complaint.
func TestEnsureNgrokAuthTokenSecret_APromptFailureIsNotSwallowed(t *testing.T) {
	t.Setenv(ngrokAuthTokenEnv, "")
	ctx := context.Background()
	c := fake.NewClientBuilder().WithScheme(kube.Scheme).Build()

	var out bytes.Buffer
	err := ensureNgrokAuthTokenSecret(ctx, &out, c, func(context.Context) (string, error) {
		return "", errors.New("interrupted")
	})

	require.Error(t, err)
	assert.ErrorContains(t, err, "interrupted",
		"the reason the question could not be answered must survive")
}
