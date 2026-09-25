package resolve_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/fake" // register "fake" kind
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/resolve"
)

func newScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(s))
	require.NoError(t, spiceboxv1alpha1.AddToScheme(s))
	return s
}

func metaName(ns, name string) metav1.ObjectMeta {
	return metav1.ObjectMeta{Namespace: ns, Name: name}
}

func TestForSessionHappy(t *testing.T) {
	s := newScheme(t)
	ch := &spiceboxv1alpha1.Channel{
		ObjectMeta: metaName("default", "ch1"),
		Spec: spiceboxv1alpha1.ChannelSpec{
			Kind:           "fake",
			CredentialsRef: spiceboxv1alpha1.ChannelCredentialsRef{SecretName: "ch1-creds"},
		},
	}
	sec := &corev1.Secret{
		ObjectMeta: metaName("default", "ch1-creds"),
		Data:       map[string][]byte{"k": []byte("v")},
	}
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metaName("default", "sess1"),
		Spec: spiceboxv1alpha1.AgentSessionSpec{
			InputChannel: &spiceboxv1alpha1.ChannelBinding{Name: "ch1", Kind: "fake"},
		},
	}
	cli := fake.NewClientBuilder().WithScheme(s).WithObjects(ch, sec, sess).Build()

	gotCh, gotSec, k, err := resolve.ForSession(context.Background(), cli, sess)
	require.NoError(t, err)
	assert.Equal(t, "ch1", gotCh.Name)
	assert.Equal(t, "ch1-creds", gotSec.Name)
	require.NotNil(t, k)
	assert.Equal(t, "fake", k.Name())
}

func TestForSessionRejectsKubectlSessions(t *testing.T) {
	s := newScheme(t)
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metaName("default", "sess1"),
		Spec:       spiceboxv1alpha1.AgentSessionSpec{}, // no Channel
	}
	cli := fake.NewClientBuilder().WithScheme(s).WithObjects(sess).Build()

	_, _, _, err := resolve.ForSession(context.Background(), cli, sess)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not channel-attached")
}

func TestForSessionChannelMissing(t *testing.T) {
	s := newScheme(t)
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metaName("default", "sess1"),
		Spec: spiceboxv1alpha1.AgentSessionSpec{
			InputChannel: &spiceboxv1alpha1.ChannelBinding{Name: "missing", Kind: "fake"},
		},
	}
	cli := fake.NewClientBuilder().WithScheme(s).WithObjects(sess).Build()

	_, _, _, err := resolve.ForSession(context.Background(), cli, sess)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "get channel")
}

func TestForSessionSecretMissing(t *testing.T) {
	s := newScheme(t)
	ch := &spiceboxv1alpha1.Channel{
		ObjectMeta: metaName("default", "ch1"),
		Spec: spiceboxv1alpha1.ChannelSpec{
			Kind:           "fake",
			CredentialsRef: spiceboxv1alpha1.ChannelCredentialsRef{SecretName: "missing"},
		},
	}
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metaName("default", "sess1"),
		Spec: spiceboxv1alpha1.AgentSessionSpec{
			InputChannel: &spiceboxv1alpha1.ChannelBinding{Name: "ch1", Kind: "fake"},
		},
	}
	cli := fake.NewClientBuilder().WithScheme(s).WithObjects(ch, sess).Build()

	_, _, _, err := resolve.ForSession(context.Background(), cli, sess)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "get secret")
}

// TestForSessionNoCredentialsRef pins the case a named-but-absent Secret must
// stay distinguishable from: a Channel that references NO Secret at all.
//
// spec.credentialsRef.secretName is required by the CRD but carries no
// MinLength, and the SubagentRequest reconciler leaves it empty on the
// kind=agent Channel a conversational delegated child talks over — there is no
// third party to hold a credential for. Resolving that as `get secret: secrets
// "" not found` broke every caller: channelsd's senderResolver could build no
// Sender, so the outbound relay dropped everything the child tried to say, and
// the runner lost each channel-sourced capability behind one Warn line.
//
// The returned Secret is empty and non-nil, matching what the credential-less
// kinds already in production (local, browser, bento) get from the empty
// Secret they name for CRD-shape reasons.
func TestForSessionNoCredentialsRef(t *testing.T) {
	s := newScheme(t)
	ch := &spiceboxv1alpha1.Channel{
		ObjectMeta: metaName("default", "ch1"),
		Spec:       spiceboxv1alpha1.ChannelSpec{Kind: "fake"}, // no CredentialsRef
	}
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metaName("default", "sess1"),
		Spec: spiceboxv1alpha1.AgentSessionSpec{
			InputChannel: &spiceboxv1alpha1.ChannelBinding{Name: "ch1", Kind: "fake"},
		},
	}
	cli := fake.NewClientBuilder().WithScheme(s).WithObjects(ch, sess).Build()

	gotCh, gotSec, k, err := resolve.ForSession(context.Background(), cli, sess)
	require.NoError(t, err, "a Channel with no credentialsRef must resolve")
	assert.Equal(t, "ch1", gotCh.Name)
	require.NotNil(t, gotSec, "the Secret must be empty, not nil: Deps.Secret is dereferenced")
	assert.Empty(t, gotSec.Data)
	require.NotNil(t, k)
	assert.Equal(t, "fake", k.Name())
}

func TestForSessionUnknownKind(t *testing.T) {
	s := newScheme(t)
	ch := &spiceboxv1alpha1.Channel{
		ObjectMeta: metaName("default", "ch1"),
		Spec: spiceboxv1alpha1.ChannelSpec{
			Kind:           "no-such-kind",
			CredentialsRef: spiceboxv1alpha1.ChannelCredentialsRef{SecretName: "ch1-creds"},
		},
	}
	sec := &corev1.Secret{ObjectMeta: metaName("default", "ch1-creds")}
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metaName("default", "sess1"),
		Spec: spiceboxv1alpha1.AgentSessionSpec{
			InputChannel: &spiceboxv1alpha1.ChannelBinding{Name: "ch1", Kind: "no-such-kind"},
		},
	}
	cli := fake.NewClientBuilder().WithScheme(s).WithObjects(ch, sec, sess).Build()

	_, _, _, err := resolve.ForSession(context.Background(), cli, sess)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no-such-kind")
}

func TestForChannelNilChannel(t *testing.T) {
	s := newScheme(t)
	cli := fake.NewClientBuilder().WithScheme(s).Build()

	_, _, err := resolve.ForChannel(context.Background(), cli, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nil channel")
}

// TestForSession_PrefersOutputChannel verifies that when both InputChannel and
// OutputChannel are set, ForSession resolves from OutputChannel.
func TestForSession_PrefersOutputChannel(t *testing.T) {
	s := newScheme(t)
	// "in" is the input channel; "out" is the output channel. Both use kind
	// "fake" so they are both registered, but we distinguish them by Channel name.
	inCh := &spiceboxv1alpha1.Channel{
		ObjectMeta: metaName("default", "in"),
		Spec: spiceboxv1alpha1.ChannelSpec{
			Kind:           "fake",
			CredentialsRef: spiceboxv1alpha1.ChannelCredentialsRef{SecretName: "in-creds"},
		},
	}
	outCh := &spiceboxv1alpha1.Channel{
		ObjectMeta: metaName("default", "out"),
		Spec: spiceboxv1alpha1.ChannelSpec{
			Kind:           "fake",
			CredentialsRef: spiceboxv1alpha1.ChannelCredentialsRef{SecretName: "out-creds"},
		},
	}
	inSec := &corev1.Secret{ObjectMeta: metaName("default", "in-creds")}
	outSec := &corev1.Secret{ObjectMeta: metaName("default", "out-creds")}
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metaName("default", "sess-out"),
		Spec: spiceboxv1alpha1.AgentSessionSpec{
			InputChannel:  &spiceboxv1alpha1.ChannelBinding{Name: "in", Kind: "fake"},
			OutputChannel: &spiceboxv1alpha1.ChannelBinding{Name: "out", Kind: "fake"},
		},
	}
	cli := fake.NewClientBuilder().WithScheme(s).WithObjects(inCh, outCh, inSec, outSec, sess).Build()

	gotCh, gotSec, k, err := resolve.ForSession(context.Background(), cli, sess)
	require.NoError(t, err)
	assert.Equal(t, "out", gotCh.Name)
	assert.Equal(t, "out-creds", gotSec.Name)
	require.NotNil(t, k)
	assert.Equal(t, "fake", k.Name())
}

// TestForSession_FallsBackToInputChannel verifies that when OutputChannel is
// nil, ForSession falls back to InputChannel (legacy single-Channel flow).
func TestForSession_FallsBackToInputChannel(t *testing.T) {
	s := newScheme(t)
	ch := &spiceboxv1alpha1.Channel{
		ObjectMeta: metaName("default", "in"),
		Spec: spiceboxv1alpha1.ChannelSpec{
			Kind:           "fake",
			CredentialsRef: spiceboxv1alpha1.ChannelCredentialsRef{SecretName: "in-creds"},
		},
	}
	sec := &corev1.Secret{ObjectMeta: metaName("default", "in-creds")}
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metaName("default", "sess-in"),
		Spec: spiceboxv1alpha1.AgentSessionSpec{
			InputChannel: &spiceboxv1alpha1.ChannelBinding{Name: "in", Kind: "fake"},
			// OutputChannel intentionally nil — legacy flow.
		},
	}
	cli := fake.NewClientBuilder().WithScheme(s).WithObjects(ch, sec, sess).Build()

	gotCh, gotSec, k, err := resolve.ForSession(context.Background(), cli, sess)
	require.NoError(t, err)
	assert.Equal(t, "in", gotCh.Name)
	assert.Equal(t, "in-creds", gotSec.Name)
	require.NotNil(t, k)
	assert.Equal(t, "fake", k.Name())
}

func TestForChannelHappy(t *testing.T) {
	s := newScheme(t)
	ch := &spiceboxv1alpha1.Channel{
		ObjectMeta: metaName("default", "ch1"),
		Spec: spiceboxv1alpha1.ChannelSpec{
			Kind:           "fake",
			CredentialsRef: spiceboxv1alpha1.ChannelCredentialsRef{SecretName: "ch1-creds"},
		},
	}
	sec := &corev1.Secret{ObjectMeta: metaName("default", "ch1-creds")}
	cli := fake.NewClientBuilder().WithScheme(s).WithObjects(ch, sec).Build()

	gotSec, k, err := resolve.ForChannel(context.Background(), cli, ch)
	require.NoError(t, err)
	assert.Equal(t, "ch1-creds", gotSec.Name)
	require.NotNil(t, k)
	assert.Equal(t, "fake", k.Name())
}
