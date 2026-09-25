package externalurl

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

func newCM(url string) *corev1.ConfigMap {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: Namespace,
			Name:      spiceboxv1alpha1.IdentitydExternalURLConfigMap,
		},
	}
	if url != "" {
		cm.Data = map[string]string{"url": url}
	}
	return cm
}

// newCMFor builds a ConfigMap with a given name + key for NewProviderFor tests.
func newCMFor(name, key, url string) *corev1.ConfigMap {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: Namespace,
			Name:      name,
		},
	}
	if url != "" {
		cm.Data = map[string]string{key: url}
	}
	return cm
}

// TestProvider_BootstrapValueWinsUntilFirstPoll pins that callers can
// pass in a startup URL (from the env-var mount) and Get() returns it
// immediately, before Run has been called.
func TestProvider_BootstrapValueWinsUntilFirstPoll(t *testing.T) {
	cs := fake.NewSimpleClientset()
	p := NewProvider(cs, zap.New(), "https://from-env.example/")
	assert.Equal(t, "https://from-env.example/", p.Get())
}

// TestProvider_RunPicksUpConfigMapChanges pins the central behavior:
// while Run is polling, edits to the ConfigMap's "url" key are
// reflected in Get() without restarting the goroutine.
func TestProvider_RunPicksUpConfigMapChanges(t *testing.T) {
	cs := fake.NewSimpleClientset(newCM("https://initial.example/"))
	p := NewProvider(cs, zap.New(), "").WithInterval(20 * time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go p.Run(ctx)

	require.Eventually(t, func() bool {
		return p.Get() == "https://initial.example/"
	}, time.Second, 10*time.Millisecond, "first poll must surface the seeded URL")

	// Mutate the ConfigMap as `oap init --local` would.
	cm, err := cs.CoreV1().ConfigMaps(Namespace).Get(ctx,
		spiceboxv1alpha1.IdentitydExternalURLConfigMap, metav1.GetOptions{})
	require.NoError(t, err)
	cm.Data["url"] = "https://updated.example/"
	_, err = cs.CoreV1().ConfigMaps(Namespace).Update(ctx, cm, metav1.UpdateOptions{})
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		return p.Get() == "https://updated.example/"
	}, time.Second, 10*time.Millisecond, "next poll must pick up the mutated URL")
}

// TestProvider_GetErrorDoesNotClearCachedURL — a transient API
// failure must not strand callers on the empty string. Provider keeps
// the last-known-good value.
func TestProvider_GetErrorDoesNotClearCachedURL(t *testing.T) {
	cs := fake.NewSimpleClientset() // no ConfigMap → Get returns NotFound
	p := NewProvider(cs, zap.New(), "https://bootstrap.example/").WithInterval(20 * time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go p.Run(ctx)
	time.Sleep(120 * time.Millisecond)

	assert.Equal(t, "https://bootstrap.example/", p.Get(),
		"NotFound on the ConfigMap must NOT clobber the previously-cached URL — a transient API miss shouldn't strand the link minter")
}

// TestProvider_EmptyURLKeyDoesNotClearCache — operator removed the
// key by mistake. Don't clobber.
func TestProvider_EmptyURLKeyDoesNotClearCache(t *testing.T) {
	cs := fake.NewSimpleClientset(newCM("")) // no Data
	p := NewProvider(cs, zap.New(), "https://bootstrap.example/").WithInterval(20 * time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go p.Run(ctx)
	time.Sleep(120 * time.Millisecond)

	assert.Equal(t, "https://bootstrap.example/", p.Get(),
		"absent url key must NOT clobber the previously-cached URL")
}

// TestNewProviderFor_ReadsCustomConfigMapAndKey verifies that NewProviderFor
// reads from a ConfigMap name + data key distinct from the identityd defaults.
// This is the path used by the webd URL provider.
func TestNewProviderFor_ReadsCustomConfigMapAndKey(t *testing.T) {
	const (
		cmName  = "spicebox-webd-external-url"
		cmKey   = "trusted-url"
		webdURL = "https://webd.example.com/"
	)
	cs := fake.NewSimpleClientset(newCMFor(cmName, cmKey, webdURL))
	p := NewProviderFor(cs, zap.New(), cmName, cmKey, "").WithInterval(20 * time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go p.Run(ctx)

	require.Eventually(t, func() bool {
		return p.Get() == webdURL
	}, time.Second, 10*time.Millisecond, "NewProviderFor must surface the custom ConfigMap URL")
}

// TestNewProvider_DelegatesToNewProviderFor verifies that NewProvider still
// targets the identityd ConfigMap + "url" key after the refactor that
// introduced NewProviderFor.
func TestNewProvider_DelegatesToNewProviderFor(t *testing.T) {
	const identitydURL = "https://identityd.example.com/"
	cs := fake.NewSimpleClientset(newCM(identitydURL))
	p := NewProvider(cs, zap.New(), "").WithInterval(20 * time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go p.Run(ctx)

	require.Eventually(t, func() bool {
		return p.Get() == identitydURL
	}, time.Second, 10*time.Millisecond, "NewProvider must still read the identityd ConfigMap + url key")
}
