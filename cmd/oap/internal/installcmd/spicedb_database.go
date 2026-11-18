package installcmd

import (
	"context"
	"fmt"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/yaml"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/progress"
)

// createDBScript idempotently creates the 'spicedb' database in the shared
// Postgres instance. Connecting via the memory-database admin URI is fine —
// CREATE DATABASE can be issued from any connection. Guarded by pg_database so
// a re-run (existing PVC, re-install) is a no-op.
//
// It first waits up to ~5 minutes (60 × 5s, each connect bounded by
// PGCONNECT_TIMEOUT) for Postgres to answer, so a pod that starts before
// Postgres exists — the GitOps single-pass apply, where nothing orders this Job
// after Postgres — survives the cold start instead of burning its backoff on
// refused connects. If Postgres never answers, the existence check below fails
// and the pod exits non-zero, so the Job's backoffLimit still bounds the total.
const createDBScript = `set -eu
i=0
until psql "$POSTGRES_URI" -tAc "SELECT 1" >/dev/null 2>&1; do
  i=$((i+1))
  if [ "$i" -ge 60 ]; then
    echo "postgres not reachable after $i attempts"
    break
  fi
  echo "waiting for postgres ($i)"
  sleep 5
done
if psql "$POSTGRES_URI" -tAc "SELECT 1 FROM pg_database WHERE datname='spicedb'" | grep -q 1; then
  echo "database spicedb already exists"
else
  psql "$POSTGRES_URI" -c "CREATE DATABASE spicedb"
  echo "database spicedb created"
fi`

// installCreateDBDeadlineSeconds bounds the createdb Job oap install applies.
// Install applies it only after Postgres is Ready, so a run past this is a
// netpol-blocked/hung connect, and failing fast surfaces it instead of quietly
// burning the createdb wait deadline.
const installCreateDBDeadlineSeconds int64 = 120

// buildSpiceDBDatabaseJob renders a one-shot Job that ensures the 'spicedb'
// database exists. The operator's migration Job creates SpiceDB's tables inside
// it but never creates the database itself.
//
// activeDeadlineSeconds of 0 omits the deadline. The GitOps overlay must: its
// Job is created in the same apply as Postgres, the deadline runs from Job
// creation (pod Pending on its Secret and on Postgres included), and a GitOps
// controller does not recreate an unchanged Job that ended in DeadlineExceeded
// — so a cold start slower than the deadline would leave the database
// uncreated for good. Without it, the script's bounded wait and backoffLimit
// bound the Job instead.
func buildSpiceDBDatabaseJob(activeDeadlineSeconds int64) ([]byte, error) {
	backoff := int32(6)
	var activeDeadline *int64
	if activeDeadlineSeconds > 0 {
		activeDeadline = &activeDeadlineSeconds
	}
	job := &batchv1.Job{
		TypeMeta: metav1.TypeMeta{APIVersion: "batch/v1", Kind: "Job"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "spicebox-spicedb-createdb",
			Namespace: "agentprimitives-system",
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:          &backoff,
			ActiveDeadlineSeconds: activeDeadline,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					// Same owner label the spicedb-operator stamps on its
					// managed pods (serving + migration Job). The postgres
					// ingress / spicedb-egress NetworkPolicies select on it, so
					// without this label the createdb pod can't reach Postgres
					// under a default-deny CNI (GKE Autopilot/Cilium/Calico).
					Labels: map[string]string{"authzed.com/cluster": "spicebox-spicedb"},
				},
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyOnFailure,
					Containers: []corev1.Container{{
						Name:    "createdb",
						Image:   "pgvector/pgvector:pg17",
						Command: []string{"sh", "-c", createDBScript},
						Env: []corev1.EnvVar{{
							// Bounds each psql connect, so a netpol-dropped
							// connect cannot hang an attempt indefinitely.
							Name:  "PGCONNECT_TIMEOUT",
							Value: "5",
						}, {
							Name: "POSTGRES_URI",
							ValueFrom: &corev1.EnvVarSource{
								SecretKeyRef: &corev1.SecretKeySelector{
									LocalObjectReference: corev1.LocalObjectReference{Name: "spicebox-postgres-token"},
									Key:                  "uri",
								},
							},
						}},
					}},
				},
			},
		},
	}
	doc, err := yaml.Marshal(job)
	if err != nil {
		return nil, err
	}
	// batchv1.JobStatus has no omitempty fields, so marshaling the typed Job
	// always emits a `status: {}` stanza. This manifest is later server-side
	// applied — a client must not apply a status stanza — so strip it via a
	// map round-trip before returning.
	var m map[string]any
	if err := yaml.Unmarshal(doc, &m); err != nil {
		return nil, err
	}
	delete(m, "status")
	return yaml.Marshal(m)
}

// jobSucceeded polls until the named Job reports at least one successful
// completion. A not-yet-created Job (NotFound) reports not-ready, not an error,
// so it composes with a component that applies the Job then waits. Any other
// error (RBAC Forbidden, a wedged apiserver, ...) is surfaced rather than
// swallowed, so a persistent failure doesn't masquerade as a silent hang. A
// terminally-failed Job (backoff exhausted) is also surfaced immediately
// rather than left to silently burn the wait deadline — the no-silent-hang
// antipattern this repo forbids.
func jobSucceeded(typed kubernetes.Interface, ns, name string) progress.Poll {
	return func(ctx context.Context) (bool, error) {
		j, err := typed.BatchV1().Jobs(ns).Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return false, nil // Job not created yet — keep polling
		}
		if err != nil {
			return false, err // real failure — surface it, don't hang silently
		}
		for _, cond := range j.Status.Conditions {
			if cond.Type == batchv1.JobFailed && cond.Status == corev1.ConditionTrue {
				return false, fmt.Errorf("createdb job failed: %s: %s", cond.Reason, cond.Message)
			}
		}
		return j.Status.Succeeded >= 1, nil
	}
}
