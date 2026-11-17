package installcmd

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/authzed/openagentprimitives/pkg/platform/apimage"
	"github.com/authzed/openagentprimitives/pkg/platform/bootstrap"
	"github.com/authzed/openagentprimitives/pkg/platform/manifests"
)

// bootstrapJobFile is the overlay file holding the pre-start secret-minting Job.
const bootstrapJobFile = "bootstrap-job.yaml"

// bootstrapJobBaseName is the stable prefix every bootstrap Job name carries; the
// content hash is appended so a changed pod template is a new object.
const bootstrapJobBaseName = "spicebox-bootstrap-secrets"

// bootstrapIdentity names the Job's ServiceAccount, Role and RoleBinding, and
// is its pod's app.kubernetes.io/name label — the selector of the bootstrap
// NetworkPolicy (config/networkpolicy/bootstrap.yaml) that lets it reach the
// apiserver. Deliberately not spicebox-operator: the operator Services select
// on that label, and the Job pod would join their endpoints.
const bootstrapIdentity = "spicebox-bootstrap"

// bootstrapJobArgs is the single source for the Job's entrypoint args, consumed
// by BOTH buildBootstrapJob and the name hash so the two cannot disagree about
// what the Job runs.
func bootstrapJobArgs(cfg RenderConfig) []string {
	args := []string{"bootstrap-secrets"}
	if cfg.externalSpiceDB() {
		args = append(args, "--skip-spicedb-token")
	}
	if cfg.ExternalPostgres {
		args = append(args, "--skip-postgres-token")
	}
	return args
}

// bootstrapJobName names the Job by a hash of what it runs and what it mints: the
// resolved operator image ref, the entrypoint args, and
// bootstrap.MintSetFingerprint — the last so an upgrade that adds a Secret or
// widens a NATS grant re-runs the Job even under an unchanged (mutable) image
// tag. A Job's
// spec.template is immutable once applied, so a GitOps re-sync after an operator
// image bump would try to patch it and be rejected (Argo/Flux SyncFailed). A
// content-addressed name instead makes a changed template a NEW object the
// controller prunes-and-creates, while a byte-identical re-render hashes the same
// → same name → a no-op apply (preserving SSA idempotency). The image ref is
// substituted the same way the emitted manifest's is, so the hash tracks exactly
// what will be written.
func bootstrapJobName(cfg RenderConfig) (string, error) {
	img, err := manifests.Substitute([]byte(apimage.Operator.LocalRef()), cfg.Tags)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	h.Write(img)
	h.Write([]byte{0})
	for _, a := range bootstrapJobArgs(cfg) {
		h.Write([]byte(a))
		h.Write([]byte{0})
	}
	h.Write([]byte(bootstrap.MintSetFingerprint()))
	return bootstrapJobBaseName + "-" + hex.EncodeToString(h.Sum(nil))[:10], nil
}

// renderBootstrapJob builds the Job that mints the internal Secrets a
// GitOps-applied install needs, plus the ServiceAccount, Role and RoleBinding it
// runs as, as marshaled YAML with the operator image rewritten by the render's
// substitution. It runs the operator image's `bootstrap-secrets` entrypoint.
//
// The Job has its own identity rather than the operator's: it needs
// deployments/patch (a NATS re-mint rolls the Deployments mounting the creds),
// which the operator itself never uses, and a namespaced Role confines that to
// agentprimitives-system instead of growing the operator's ClusterRole.
//
// Sync-waves order it after its prerequisites and before every consumer of what
// it mints (render_syncwaves.go).
func renderBootstrapJob(cfg RenderConfig) ([]byte, error) {
	name, err := bootstrapJobName(cfg)
	if err != nil {
		return nil, err
	}
	var docs [][]byte
	for _, obj := range append(bootstrapRBAC(), any(buildBootstrapJob(cfg, name))) {
		y, err := marshalClean(obj)
		if err != nil {
			return nil, err
		}
		docs = append(docs, y)
	}
	// Rewrite the operator image ref (and apply any token links — a no-op here).
	return processBaseBytes(bytes.Join(docs, []byte("---\n")), cfg)
}

// bootstrapRBAC is exactly what MintAllInternalSecrets calls: Secret get/create/
// update, and list+patch on Deployments for the roll after a NATS re-mint.
func bootstrapRBAC() []any {
	meta := metav1.ObjectMeta{
		Name:        bootstrapIdentity,
		Namespace:   bootstrap.TokenSecretNamespace,
		Annotations: map[string]string{argoSyncWaveAnnotation: syncWaveBootstrapPrereqs},
	}
	return []any{
		&corev1.ServiceAccount{
			TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "ServiceAccount"},
			ObjectMeta: meta,
		},
		&rbacv1.Role{
			TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "Role"},
			ObjectMeta: meta,
			Rules: []rbacv1.PolicyRule{
				{APIGroups: []string{""}, Resources: []string{"secrets"}, Verbs: []string{"get", "create", "update"}},
				{APIGroups: []string{"apps"}, Resources: []string{"deployments"}, Verbs: []string{"list", "patch"}},
			},
		},
		&rbacv1.RoleBinding{
			TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "RoleBinding"},
			ObjectMeta: meta,
			RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "Role", Name: bootstrapIdentity},
			Subjects:   []rbacv1.Subject{{Kind: "ServiceAccount", Name: bootstrapIdentity, Namespace: bootstrap.TokenSecretNamespace}},
		},
	}
}

func buildBootstrapJob(cfg RenderConfig, name string) *batchv1.Job {
	backoff := int32(6)
	return &batchv1.Job{
		TypeMeta: metav1.TypeMeta{APIVersion: "batch/v1", Kind: "Job"},
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   bootstrap.TokenSecretNamespace,
			Annotations: map[string]string{argoSyncWaveAnnotation: syncWaveBootstrapJob},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit: &backoff,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"app.kubernetes.io/name": bootstrapIdentity},
				},
				Spec: corev1.PodSpec{
					ServiceAccountName: bootstrapIdentity,
					RestartPolicy:      corev1.RestartPolicyOnFailure,
					Containers: []corev1.Container{{
						Name:  "bootstrap-secrets",
						Image: apimage.Operator.LocalRef(),
						Args:  bootstrapJobArgs(cfg),
					}},
				},
			},
		},
	}
}
