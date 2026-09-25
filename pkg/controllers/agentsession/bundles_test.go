package agentsession_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/agentsession"
)

func TestBuildBundleSession(t *testing.T) {
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "review", Namespace: "default", UID: "uid-1"},
	}
	bundle := spiceboxv1alpha1.ToolBundle{
		Name:      "code",
		Class:     "toolbelt",
		Toolspecs: []string{"git-readonly", "gh-readonly"},
	}
	identity := "code-id"

	got := agentsession.BuildBundleSession(sess, bundle, identity, "", nil, nil, nil)
	assert.Equal(t, "review-code", got.Name, "bundle session name (<session>-<bundle>)")
	assert.Equal(t, "default", got.Namespace, "namespace inherited from owner")
	assert.Equal(t, "toolbelt", got.Spec.Class, "spec.class from bundle")
	assert.Equal(t, "code-id", got.Spec.Agent, "spec.agent from identity arg")
	assert.Len(t, got.Spec.Toolspecs, 2, "spec.toolspecs propagated from bundle")
	require.Len(t, got.OwnerReferences, 1, "owner reference should be set")
	assert.Equal(t, sess.UID, got.OwnerReferences[0].UID, "owner ref UID matches AgentSession")
}

func TestBuildBundleSession_SharedWorkspace(t *testing.T) {
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "rb", Namespace: "default", UID: "u1"},
	}
	bundle := spiceboxv1alpha1.ToolBundle{Name: "code", Class: "coding-class"}
	got := agentsession.BuildBundleSession(sess, bundle, "ai-1", "rb-workspace", nil, nil, nil)

	assert.Equal(t, spiceboxv1alpha1.WorkspaceShared, got.Spec.Workspace.Mode)
	assert.Equal(t, "rb-workspace", got.Spec.Workspace.SharedClaimName)
}

func TestBuildBundleSession_IsolatedWhenNoClaim(t *testing.T) {
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "rb", Namespace: "default", UID: "u1"},
	}
	bundle := spiceboxv1alpha1.ToolBundle{Name: "code", Class: "coding-class"}
	got := agentsession.BuildBundleSession(sess, bundle, "ai-1", "", nil, nil, nil)

	assert.Equal(t, spiceboxv1alpha1.WorkspaceIsolated, got.Spec.Workspace.Mode)
	assert.Empty(t, got.Spec.Workspace.SharedClaimName)
}

func TestBuildBundleSession_GitBundleGetsHardeningEnv(t *testing.T) {
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "rb", Namespace: "default", UID: "u1"},
	}
	bundle := spiceboxv1alpha1.ToolBundle{Name: "git", Class: "git-class"}
	got := agentsession.BuildBundleSession(sess, bundle, "ai-1", "rb-workspace", nil, nil, nil)

	assert.Equal(t, "/dev/null", got.Spec.DefaultEnv["GIT_CONFIG_GLOBAL"])
	assert.Equal(t, "1", got.Spec.DefaultEnv["GIT_CONFIG_NOSYSTEM"])
	assert.Equal(t, "0", got.Spec.DefaultEnv["GIT_TERMINAL_PROMPT"])
}

func TestBuildBundleSession_NonGitBundleNoHardeningEnv(t *testing.T) {
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "rb", Namespace: "default", UID: "u1"},
	}
	bundle := spiceboxv1alpha1.ToolBundle{Name: "code", Class: "coding-class"}
	got := agentsession.BuildBundleSession(sess, bundle, "ai-1", "rb-workspace", nil, nil, nil)
	assert.Empty(t, got.Spec.DefaultEnv, "non-git bundles get no default env")
}

func TestBuildBundleSession_ThreadsSkillBundlesOntoSpec(t *testing.T) {
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "rb", Namespace: "default", UID: "u1"},
	}
	bundle := spiceboxv1alpha1.ToolBundle{Name: "code", Class: "coding-class"}
	skills := []spiceboxv1alpha1.ResolvedSkillBundle{
		{CanonicalName: "github.com/org/repo//skills/a@v1", LocalName: "a", MountName: "a-abc123", ConfigMapName: "skillbundle-rb-a-abc123", Digest: "sha256:aa"},
		{CanonicalName: "github.com/org/repo//skills/b@v1", LocalName: "b", MountName: "b-def456", ConfigMapName: "skillbundle-rb-b-def456", Digest: "sha256:bb"},
	}

	got := agentsession.BuildBundleSession(sess, bundle, "ai-1", "", skills, nil, nil)

	require.Len(t, got.Spec.Mounts, 2, "both resolved skill bundles mirrored onto spec.mounts")
	// Name (the pod volume name) stays the hashed MountName -- it only needs
	// to be Kubernetes-safe and collision-free; MountPath is what Claude Code
	// actually discovers, so it must be the LOCAL skill name instead.
	assert.Equal(t, "a-abc123", got.Spec.Mounts[0].Name)
	assert.Equal(t, "/skills/a", got.Spec.Mounts[0].MountPath)
	assert.Equal(t, spiceboxv1alpha1.MountFormatTarGz, got.Spec.Mounts[0].Format)
	require.NotNil(t, got.Spec.Mounts[0].Source.ConfigMapRef)
	assert.Equal(t, "skillbundle-rb-a-abc123", got.Spec.Mounts[0].Source.ConfigMapRef.Name)
	assert.Equal(t, "b-def456", got.Spec.Mounts[1].Name)
	assert.Equal(t, "/skills/b", got.Spec.Mounts[1].MountPath)
	require.NotNil(t, got.Spec.Mounts[1].Source.ConfigMapRef)
	assert.Equal(t, "skillbundle-rb-b-def456", got.Spec.Mounts[1].Source.ConfigMapRef.Name)
}

// TestBuildBundleSession_MountPathUsesLocalSkillName_NotHashedSlug is the
// observable end-state check the plan exists to guarantee: Claude Code
// discovers a skill only when the sandbox directory name matches the
// SKILL.md frontmatter name (reviewbot's "code-review"). MountName is a
// content-hashed slug ("code-review-a1b2c3d4") kept ONLY for ConfigMap/volume
// naming — it must never leak into the sandbox-visible MountPath, or the
// staged archive, digest, and frontmatter can all be perfectly correct while
// the inner agent still finds nothing and reviews by improvisation.
func TestBuildBundleSession_MountPathUsesLocalSkillName_NotHashedSlug(t *testing.T) {
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "rb", Namespace: "default", UID: "u1"},
	}
	bundle := spiceboxv1alpha1.ToolBundle{Name: "codelike", Class: "codelike-class"}
	skills := []spiceboxv1alpha1.ResolvedSkillBundle{
		{
			CanonicalName: "github.com/demo-org/reviewbot-skills//skills/code-review@v1.0.0",
			LocalName:     "code-review",
			MountName:     "code-review-a1b2c3d4",
			ConfigMapName: "skillbundle-rb-code-review-a1b2c3d4",
			Digest:        "sha256:aa",
			ArchiveDigest: "sha256:bb",
		},
	}

	got := agentsession.BuildBundleSession(sess, bundle, "ai-1", "", skills, nil, nil)

	require.Len(t, got.Spec.Mounts, 1)
	assert.Equal(t, "/skills/code-review", got.Spec.Mounts[0].MountPath,
		"the mount path Claude Code discovers must be the local skill name, not MountName's hashed ConfigMap-safe slug")
	assert.Equal(t, "code-review-a1b2c3d4", got.Spec.Mounts[0].Name,
		"the pod VOLUME name may still use the hashed slug -- it only needs DNS-safety and cross-repo/version uniqueness, not to match anything Claude Code reads")
	assert.Equal(t, "skillbundle-rb-code-review-a1b2c3d4", got.Spec.Mounts[0].Source.ConfigMapRef.Name)
}

func TestBuildBundleSession_NoSkillBundles(t *testing.T) {
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "rb", Namespace: "default", UID: "u1"},
	}
	bundle := spiceboxv1alpha1.ToolBundle{Name: "code", Class: "coding-class"}
	got := agentsession.BuildBundleSession(sess, bundle, "ai-1", "", nil, nil, nil)
	assert.Empty(t, got.Spec.Mounts, "no resolved skill bundles → empty spec.mounts")
}

// The resolved backend rides spec.sandbox, the same per-session override
// channel workspace and defaultEnv already use.
func TestBuildBundleSession_StampsResolvedSandbox(t *testing.T) {
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-agent", Namespace: "demo-ns"},
	}
	bundle := spiceboxv1alpha1.ToolBundle{Name: "demo-bundle", Class: "demo-class"}

	got := agentsession.BuildBundleSession(sess, bundle, "demo-identity", "", nil,
		&spiceboxv1alpha1.SandboxBackend{Kind: "resolved-kind"}, nil)

	require.NotNil(t, got.Spec.Sandbox)
	assert.Equal(t, "resolved-kind", got.Spec.Sandbox.Kind)
}

// A session with no tier-resolved backend leaves the field unset so the
// referenced SpiceboxClass's own setting stands.
func TestBuildBundleSession_NoSandboxLeavesFieldUnset(t *testing.T) {
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-agent", Namespace: "demo-ns"},
	}
	bundle := spiceboxv1alpha1.ToolBundle{Name: "demo-bundle", Class: "demo-class"}

	got := agentsession.BuildBundleSession(sess, bundle, "demo-identity", "", nil, nil, nil)
	assert.Nil(t, got.Spec.Sandbox)
}
