package installcmd

import (
	"crypto/sha256"
	"encoding/hex"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
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

// bootstrapJobArgs is the single source for the Job's entrypoint args, consumed
// by BOTH buildBootstrapJob and the name hash so the two cannot disagree about
// what the Job runs.
func bootstrapJobArgs(cfg RenderConfig) []string {
	args := []string{"bootstrap-secrets"}
	if cfg.ExternalSpiceDB {
		args = append(args, "--skip-spicedb-token")
	}
	if cfg.ExternalPostgres {
		args = append(args, "--skip-postgres-token")
	}
	return args
}

// bootstrapJobName names the Job by a hash of the two inputs that make its pod
// template vary: the resolved operator image ref and the entrypoint args. A Job's
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
	return bootstrapJobBaseName + "-" + hex.EncodeToString(h.Sum(nil))[:10], nil
}

// renderBootstrapJob builds the Job that mints the internal Secrets a
// GitOps-applied install needs, as marshaled YAML with the operator image
// rewritten by the render's substitution. It reuses the operator's
// ServiceAccount (which already holds Secret create/get/list RBAC) and runs the
// operator image's `bootstrap-secrets` entrypoint.
//
// Ordering is by eventual convergence, not sync-waves: the Job, the namespace,
// and the operator SA it depends on are all in the same bundle, so a negative
// wave would run the Job before its own prerequisites. Applied together, the Job
// retries (backoffLimit) until the namespace/SA exist and the operator
// crash-loops until the tokens appear — both designed-for, self-healing states.
func renderBootstrapJob(cfg RenderConfig) ([]byte, error) {
	name, err := bootstrapJobName(cfg)
	if err != nil {
		return nil, err
	}
	y, err := marshalClean(buildBootstrapJob(cfg, name))
	if err != nil {
		return nil, err
	}
	// Rewrite the operator image ref (and apply any token links — a no-op here).
	return processBaseBytes(y, cfg)
}

func buildBootstrapJob(cfg RenderConfig, name string) *batchv1.Job {
	backoff := int32(6)
	return &batchv1.Job{
		TypeMeta:   metav1.TypeMeta{APIVersion: "batch/v1", Kind: "Job"},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: bootstrap.TokenSecretNamespace},
		Spec: batchv1.JobSpec{
			BackoffLimit: &backoff,
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					ServiceAccountName: "spicebox-operator",
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
