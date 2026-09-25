package agentsession

import (
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/podspec"
)

// BundleSessionName returns the deterministic SpiceboxSession name for a
// per-bundle session owned by sess.
func BundleSessionName(sess *spiceboxv1alpha1.AgentSession, bundle spiceboxv1alpha1.ToolBundle) string {
	return sess.Name + "-" + bundle.Name
}

// BuildBundleSession returns the SpiceboxSession spec/object the AgentSession
// reconciler will create for one bundle. AgentIdentity binds via spec.agent;
// the toolspec subset is mirrored onto spec.toolspecs to scope which toolspecs
// of the class are active for this session. When claimName is non-empty the
// session is stamped Workspace.Mode=shared with that claim; otherwise it is
// Workspace.Mode=isolated (pod-local /work emptyDir only). skillBundles are the
// AgentSession's resolved skill bundles, mirrored onto spec.mounts so the
// sandbox pod builder mounts + untars them at /skills/<LocalName>/ — every
// bundle session gets the same class-wide skills (a disk-based consumer execs
// against whichever bundle session is in use).
//
// sandbox is the tier-resolved backend for this bundle, or nil when no tier
// expressed a preference — in which case the referenced SpiceboxClass's own
// setting stands.
func BuildBundleSession(
	sess *spiceboxv1alpha1.AgentSession,
	bundle spiceboxv1alpha1.ToolBundle,
	identity string,
	claimName string,
	skillBundles []spiceboxv1alpha1.ResolvedSkillBundle,
	sandbox *spiceboxv1alpha1.SandboxBackend,
	toolConfig map[string]apiextensionsv1.JSON,
) *spiceboxv1alpha1.SpiceboxSession {
	tsRefs := make([]spiceboxv1alpha1.ToolspecRef, 0, len(bundle.Toolspecs))
	for _, ts := range bundle.Toolspecs {
		tsRefs = append(tsRefs, spiceboxv1alpha1.ToolspecRef{Name: ts})
	}

	// skillMounts converts the AgentSession's resolved skill bundles to the
	// native SpiceboxMount shape (Mounts is preferred over the deprecated
	// SkillBundles — see SpiceboxSessionSpec.SkillBundles), routed through the
	// same tarGz-unpack machinery every other archive mount uses. Digest is
	// deliberately fed from ArchiveDigest, not Digest: ResolvedSkillBundle.Digest
	// is a cheap, no-I/O fingerprint used only to short-circuit re-staging (see
	// resolveAndStageSkillBundles) and does not match the ConfigMap's actual
	// bytes, whereas ArchiveDigest is the real sha256 of those bytes — the one
	// value applyMounts' init-container verification can check against.
	//
	// Name and MountPath deliberately come from two DIFFERENT
	// ResolvedSkillBundle fields, answering two different questions. Name
	// (the pod volume name, and — via applyMounts — the shared-unpack
	// subPath and the init container's ConfigMap staging subdir) is
	// sb.MountName: it only has to be Kubernetes-object-name-safe and
	// collision-free across every repo/version a cluster ever stages, which
	// is exactly what its content-hashed slug buys, and nothing in the
	// sandbox ever needs to read that value. MountPath is the directory a
	// disk-based consumer (Claude Code) actually opens, which must equal the
	// staged SKILL.md's frontmatter name for that consumer to discover it at
	// all (resolveAndStageSkillBundles enforces LocalName == frontmatter
	// name) — so MountPath is built from sb.LocalName, never sb.MountName.
	// Using MountName here compiles and every digest/content check still
	// passes, but produces a directory Claude Code never looks in — the
	// silent failure this whole staging feature exists to eliminate.
	skillMounts := make([]spiceboxv1alpha1.SpiceboxMount, 0, len(skillBundles))
	for _, sb := range skillBundles {
		skillMounts = append(skillMounts, spiceboxv1alpha1.SpiceboxMount{
			Name:      sb.MountName,
			MountPath: podspec.SkillsMountPath + "/" + sb.LocalName,
			Format:    spiceboxv1alpha1.MountFormatTarGz,
			Digest:    sb.ArchiveDigest,
			Source: spiceboxv1alpha1.MountSource{
				ConfigMapRef: &corev1.LocalObjectReference{Name: sb.ConfigMapName},
			},
		})
	}

	workspace := spiceboxv1alpha1.WorkspaceConfig{Mode: spiceboxv1alpha1.WorkspaceIsolated}
	if claimName != "" {
		workspace = spiceboxv1alpha1.WorkspaceConfig{
			Mode:            spiceboxv1alpha1.WorkspaceShared,
			SharedClaimName: claimName,
		}
	}

	return &spiceboxv1alpha1.SpiceboxSession{
		TypeMeta: metav1.TypeMeta{
			APIVersion: spiceboxv1alpha1.SchemeBuilder.GroupVersion.String(),
			Kind:       "SpiceboxSession",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      BundleSessionName(sess, bundle),
			Namespace: sess.Namespace,
			Labels: map[string]string{
				"agentprimitives.authzed.com/agentsession": sess.Name,
				"agentprimitives.authzed.com/agentbundle":  bundle.Name,
			},
			OwnerReferences: sessionOwnerRef(sess),
		},
		Spec: spiceboxv1alpha1.SpiceboxSessionSpec{
			Class:      bundle.Class,
			Agent:      identity,
			Toolspecs:  tsRefs,
			Workspace:  workspace,
			DefaultEnv: bundleDefaultEnv(bundle),
			Mounts:     skillMounts,
			Sandbox:    sandbox,
			ToolConfig: toolConfig,
		},
	}
}

// bundleDefaultEnv returns static, non-secret hardening env for known bundle
// kinds. The git bundle is neutered against config-driven code execution; the
// values that vary per call (GIT_DIR/GIT_WORK_TREE) are NOT set here.
func bundleDefaultEnv(bundle spiceboxv1alpha1.ToolBundle) map[string]string {
	if bundle.Name == "git" {
		return map[string]string{
			"GIT_CONFIG_GLOBAL":   "/dev/null",
			"GIT_CONFIG_NOSYSTEM": "1",
			"GIT_TERMINAL_PROMPT": "0",
		}
	}
	return nil
}
