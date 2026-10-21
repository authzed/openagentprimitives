// Pure-function tests for the per-session browser pod builder: the name
// helpers, DeriveBrowserEffectiveSites, BuildBrowserPolicyDoc, and
// BuildBrowserPod (chrome command flags, token handling, pod shape).
package agentsession_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/agentsession"
	"github.com/authzed/openagentprimitives/pkg/platform/cloud"
)

// browserToolboxSpec builds a minimal BrowserToolboxSpec for tests.
func browserToolboxSpec(mode string, sites ...spiceboxv1alpha1.BrowserSite) spiceboxv1alpha1.BrowserToolboxSpec {
	return spiceboxv1alpha1.BrowserToolboxSpec{
		Access:      spiceboxv1alpha1.BrowserAccess{Mode: mode, Sites: sites},
		Interaction: "act",
		State:       "ephemeral",
	}
}

// resolvedBrowserToolbox builds a ResolvedBrowserToolbox the way the
// reconciler would (Task 8b): EffectiveSites derived from Spec, fixed
// GatePort.
func resolvedBrowserToolbox(ref string, spec spiceboxv1alpha1.BrowserToolboxSpec) spiceboxv1alpha1.ResolvedBrowserToolbox {
	return spiceboxv1alpha1.ResolvedBrowserToolbox{
		Name:           ref,
		Ref:            ref,
		Spec:           spec,
		EffectiveSites: agentsession.DeriveBrowserEffectiveSites(spec),
		GatePort:       9223,
	}
}

func chromeContainer(t *testing.T, pod *corev1.Pod) corev1.Container {
	t.Helper()
	for _, c := range pod.Spec.Containers {
		if c.Name == "chrome" {
			return c
		}
	}
	t.Fatalf("no chrome container in pod %q", pod.Name)
	return corev1.Container{}
}

func gateContainer(t *testing.T, pod *corev1.Pod) corev1.Container {
	t.Helper()
	for _, c := range pod.Spec.Containers {
		if c.Name == "gate" {
			return c
		}
	}
	t.Fatalf("no gate container in pod %q", pod.Name)
	return corev1.Container{}
}

func hasVolumeMount(mounts []corev1.VolumeMount, name string) bool {
	for _, m := range mounts {
		if m.Name == name {
			return true
		}
	}
	return false
}

func hasPrefixArg(args []string, prefix string) bool {
	for _, a := range args {
		if strings.HasPrefix(a, prefix) {
			return true
		}
	}
	return false
}

func TestBrowserPodName(t *testing.T) {
	sess := sessForPod("s1")
	assert.Equal(t, "s1-browser-web", agentsession.BrowserPodName(sess, "web"))
}

func TestBrowserTokenSecretName(t *testing.T) {
	sess := sessForPod("s1")
	assert.Equal(t, "s1-browser-web-token", agentsession.BrowserTokenSecretName(sess, "web"))
}

func TestBrowserPolicyConfigMapName(t *testing.T) {
	sess := sessForPod("s1")
	assert.Equal(t, "s1-browser-web-policy", agentsession.BrowserPolicyConfigMapName(sess, "web"))
}

func TestBrowserNetworkPolicyName(t *testing.T) {
	sess := sessForPod("s1")
	assert.Equal(t, "s1-browser-web-netpol", agentsession.BrowserNetworkPolicyName(sess, "web"))
}

func TestDeriveBrowserEffectiveSites(t *testing.T) {
	spec := browserToolboxSpec("allowlist",
		spiceboxv1alpha1.BrowserSite{Host: "AA.com"},
		spiceboxv1alpha1.BrowserSite{Host: "exact.example.com", Exact: true},
	)

	got := agentsession.DeriveBrowserEffectiveSites(spec)
	require.Len(t, got, 2, "1:1 with spec.Access.Sites")

	assert.Equal(t, "aa.com", got[0].Host, "host canonicalized (lowercased)")
	assert.False(t, got[0].Exact)
	assert.Equal(t, "explicit", got[0].Source)

	assert.Equal(t, "exact.example.com", got[1].Host)
	assert.True(t, got[1].Exact, "Exact preserved")
	assert.Equal(t, "explicit", got[1].Source)

	// Deterministic: repeated calls on the same input produce an identical
	// result -- this is what makes the status write idempotent.
	again := agentsession.DeriveBrowserEffectiveSites(spec)
	assert.Equal(t, got, again)
}

func TestDeriveBrowserEffectiveSites_Empty(t *testing.T) {
	spec := browserToolboxSpec("allowlist")
	assert.Empty(t, agentsession.DeriveBrowserEffectiveSites(spec))
}

// TestDeriveBrowserEffectiveSites_DropsUncanonicalizableHost proves the
// fail-closed drop branch: a host CanonicalHost rejects (embedded space here;
// the CRD Pattern forbids it, but the derivation must not rely on that) is
// DROPPED while derivation continues -- the valid neighbors still come
// through, canonicalized, in order, Source "explicit". This branch is what
// resolves the Task 1 review finding (browserpolicy.Match assumes canonical
// hosts), so it is pinned by test, not inspection.
func TestDeriveBrowserEffectiveSites_DropsUncanonicalizableHost(t *testing.T) {
	spec := browserToolboxSpec("allowlist",
		spiceboxv1alpha1.BrowserSite{Host: "aa.com"},
		spiceboxv1alpha1.BrowserSite{Host: "bad host"},     // fails CanonicalHost (whitespace)
		spiceboxv1alpha1.BrowserSite{Host: "EXAMPLE.org."}, // valid: lowercased + trailing dot stripped
	)

	got := agentsession.DeriveBrowserEffectiveSites(spec)
	assert.Equal(t, []spiceboxv1alpha1.BrowserEffectiveSite{
		{Host: "aa.com", Source: "explicit"},
		{Host: "example.org", Source: "explicit"},
	}, got, "exactly the two valid entries, canonicalized, order preserved; the invalid host is dropped")
}

func TestBuildBrowserPolicyDoc(t *testing.T) {
	spec := browserToolboxSpec("blocklist", spiceboxv1alpha1.BrowserSite{Host: "evil.example"})
	rt := resolvedBrowserToolbox("web", spec)

	doc := agentsession.BuildBrowserPolicyDoc(rt)
	assert.Equal(t, "blocklist", doc.Mode)
	require.Len(t, doc.Sites, 1)
	assert.Equal(t, "evil.example", doc.Sites[0].Host)
	assert.False(t, doc.Sites[0].Exact)
}

func TestBuildBrowserPodFlags(t *testing.T) {
	sess := sessForPod("s1")
	rt := resolvedBrowserToolbox("web", browserToolboxSpec("allowlist"))

	t.Run("unsandboxed cloud kind appends --no-sandbox", func(t *testing.T) {
		pod, err := agentsession.BuildBrowserPod(sess, rt, "ghcr.io/x/browser:v1",
			cloud.BrowserChromeSandbox{Sandboxed: false, Reason: "no verified unprivileged userns"})
		require.NoError(t, err)
		c := chromeContainer(t, pod)
		assert.Contains(t, c.Command, "--headless=new")
		assert.Contains(t, c.Command, "--proxy-server=127.0.0.1:3128")
		assert.Contains(t, c.Command, "--proxy-bypass-list=<-loopback>")
		assert.Contains(t, c.Command, "--no-sandbox")
		assert.Contains(t, c.Command, "about:blank")
		assert.True(t, hasPrefixArg(c.Command, "--user-agent=Mozilla/5.0"), "UA flag present")
		assert.True(t, hasPrefixArg(c.Command, "--user-data-dir=/home/chrome/profile"), "user-data-dir flag present")
		// Crashpad under ReadOnlyRootFilesystem: without these, chromium exits
		// 133 at boot on a real kubelet ("--database is required").
		assert.Contains(t, c.Command, "--disable-crash-reporter")
		assert.Contains(t, c.Command, "--disable-breakpad")
		assert.Contains(t, c.Command, "--crash-dumps-dir=/tmp")
		assert.Contains(t, c.Env, corev1.EnvVar{Name: "HOME", Value: "/home/chrome/profile"},
			"HOME points at the writable profile emptyDir")
	})

	t.Run("sandboxed cloud kind omits --no-sandbox", func(t *testing.T) {
		pod, err := agentsession.BuildBrowserPod(sess, rt, "ghcr.io/x/browser:v1",
			cloud.BrowserChromeSandbox{Sandboxed: true, Reason: "verified unprivileged userns"})
		require.NoError(t, err)
		c := chromeContainer(t, pod)
		assert.NotContains(t, c.Command, "--no-sandbox")
	})
}

func TestBuildBrowserPodTokenNeverLiteral(t *testing.T) {
	sess := sessForPod("s1")
	// Sentinel woven only into the ref (and so the derived Secret NAME) --
	// BuildBrowserPod never accepts a raw token VALUE as an argument at all,
	// so this also proves structurally that no literal secret value can
	// leak into the pod it builds.
	const sentinelRef = "super-secret-sentinel-token-value"
	rt := resolvedBrowserToolbox(sentinelRef, browserToolboxSpec("allowlist"))

	pod, err := agentsession.BuildBrowserPod(sess, rt, "ghcr.io/x/browser:v1", cloud.BrowserChromeSandbox{})
	require.NoError(t, err)

	gate := gateContainer(t, pod)
	var tokenEnv *corev1.EnvVar
	for i := range gate.Env {
		if gate.Env[i].Name == "APBROWSERGATE_TOKEN" {
			tokenEnv = &gate.Env[i]
		}
	}
	require.NotNil(t, tokenEnv, "gate must set APBROWSERGATE_TOKEN")
	assert.Empty(t, tokenEnv.Value, "no literal token value")
	require.NotNil(t, tokenEnv.ValueFrom, "token must be sourced via ValueFrom, never a literal Value")
	require.NotNil(t, tokenEnv.ValueFrom.SecretKeyRef, "token must come from a SecretKeyRef")
	assert.Equal(t, agentsession.BrowserTokenSecretName(sess, sentinelRef), tokenEnv.ValueFrom.SecretKeyRef.Name)
	assert.Equal(t, "token", tokenEnv.ValueFrom.SecretKeyRef.Key)

	raw, err := json.Marshal(pod)
	require.NoError(t, err)
	assert.NotContains(t, string(raw), `"value":"`+sentinelRef+`"`,
		"the sentinel must never appear as a literal env Value (it legitimately appears in names)")
}

func TestBuildBrowserPodShape(t *testing.T) {
	sess := sessForPod("s1")
	rt := resolvedBrowserToolbox("web", browserToolboxSpec("allowlist"))

	pod, err := agentsession.BuildBrowserPod(sess, rt, "ghcr.io/x/browser:v1", cloud.BrowserChromeSandbox{})
	require.NoError(t, err)

	assert.Equal(t, "s1-browser-web", pod.Name)
	assert.Equal(t, "default", pod.Namespace)

	require.Len(t, pod.Spec.Containers, 2, "chrome + gate")
	var names []string
	for _, c := range pod.Spec.Containers {
		names = append(names, c.Name)
	}
	assert.ElementsMatch(t, []string{"chrome", "gate"}, names)

	require.Len(t, pod.OwnerReferences, 1)
	owner := pod.OwnerReferences[0]
	assert.Equal(t, "AgentSession", owner.Kind)
	assert.Equal(t, "s1", owner.Name)
	assert.Equal(t, types.UID("sess-uid-123"), owner.UID)
	require.NotNil(t, owner.Controller)
	assert.True(t, *owner.Controller)
	require.NotNil(t, owner.BlockOwnerDeletion)
	assert.True(t, *owner.BlockOwnerDeletion)

	require.NotNil(t, pod.Spec.AutomountServiceAccountToken)
	assert.False(t, *pod.Spec.AutomountServiceAccountToken, "browser pod must not mount an SA token")

	gate := gateContainer(t, pod)
	chrome := chromeContainer(t, pod)

	// Policy ConfigMap volume mounted in gate only.
	assert.True(t, hasVolumeMount(gate.VolumeMounts, "policy"), "gate must mount the policy volume")
	assert.False(t, hasVolumeMount(chrome.VolumeMounts, "policy"), "chrome must NOT mount the policy volume")

	// Profile emptyDir mounted in chrome only.
	assert.True(t, hasVolumeMount(chrome.VolumeMounts, "profile"), "chrome must mount the profile volume")
	assert.False(t, hasVolumeMount(gate.VolumeMounts, "profile"), "gate must NOT mount the profile volume")

	// Labels identify session + browsertoolbox ref.
	assert.Equal(t, "s1", pod.Labels["agentprimitives.authzed.com/agentsession"])
	assert.Equal(t, "web", pod.Labels["agentprimitives.authzed.com/browsertoolbox"])

	// Gate stays fully hardened: no per-container UID override, standard
	// hardened container security context.
	require.NotNil(t, gate.SecurityContext)
	assert.Nil(t, gate.SecurityContext.RunAsUser, "gate inherits the pod-level UID; no override")
	require.NotNil(t, gate.SecurityContext.ReadOnlyRootFilesystem)
	assert.True(t, *gate.SecurityContext.ReadOnlyRootFilesystem)
	require.NotNil(t, gate.SecurityContext.AllowPrivilegeEscalation)
	assert.False(t, *gate.SecurityContext.AllowPrivilegeEscalation)

	// Gate's startup probe hits /healthz on the fixed gate port.
	require.NotNil(t, gate.StartupProbe)
	require.NotNil(t, gate.StartupProbe.HTTPGet)
	assert.Equal(t, "/healthz", gate.StartupProbe.HTTPGet.Path)
	assert.Equal(t, int32(9223), gate.StartupProbe.HTTPGet.Port.IntVal)

	// Chrome's RunAsUser/RunAsGroup are overridden to match the browser
	// image's one declared account (images/browser/Dockerfile: useradd
	// --uid 65532 chrome); ReadOnlyRootFilesystem/Capabilities/
	// AllowPrivilegeEscalation stay hardened (not relaxed).
	require.NotNil(t, chrome.SecurityContext)
	require.NotNil(t, chrome.SecurityContext.RunAsUser)
	assert.Equal(t, int64(65532), *chrome.SecurityContext.RunAsUser)
	require.NotNil(t, chrome.SecurityContext.RunAsGroup)
	assert.Equal(t, int64(65532), *chrome.SecurityContext.RunAsGroup)
	require.NotNil(t, chrome.SecurityContext.ReadOnlyRootFilesystem)
	assert.True(t, *chrome.SecurityContext.ReadOnlyRootFilesystem, "chrome does not relax ReadOnlyRootFilesystem")
	require.NotNil(t, chrome.SecurityContext.AllowPrivilegeEscalation)
	assert.False(t, *chrome.SecurityContext.AllowPrivilegeEscalation)

	// Pod-level hardened context (RunAsNonRoot, FSGroup) is present.
	require.NotNil(t, pod.Spec.SecurityContext)
	require.NotNil(t, pod.Spec.SecurityContext.RunAsNonRoot)
	assert.True(t, *pod.Spec.SecurityContext.RunAsNonRoot)
	require.NotNil(t, pod.Spec.SecurityContext.FSGroup)
	assert.Equal(t, int64(1000), *pod.Spec.SecurityContext.FSGroup)
}

func TestBuildBrowserPod_EmptyImageRef_Errors(t *testing.T) {
	sess := sessForPod("s1")
	rt := resolvedBrowserToolbox("web", browserToolboxSpec("allowlist"))
	_, err := agentsession.BuildBrowserPod(sess, rt, "", cloud.BrowserChromeSandbox{})
	require.Error(t, err)
}
