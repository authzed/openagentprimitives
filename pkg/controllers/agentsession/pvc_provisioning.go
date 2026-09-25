package agentsession

import (
	"context"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// effectiveBundleReadyDeadline returns the bundle-readiness deadline for this
// session's workspace StorageClass: the extended filestoreBundleReadyDeadline
// for a slow-cold-start class (GKE Filestore, whose first multishare instance
// takes minutes to create), otherwise the default bundleReadyDeadline. Fail-safe
// — no class, or any StorageClass read error, yields the default.
func (r *Reconciler) effectiveBundleReadyDeadline(ctx context.Context) time.Duration {
	if r.WorkspaceStorageClass == "" {
		return bundleReadyDeadline
	}
	var sc storagev1.StorageClass
	if err := r.Client.Get(ctx, client.ObjectKey{Name: r.WorkspaceStorageClass}, &sc); err != nil {
		return bundleReadyDeadline
	}
	if sc.Provisioner == "filestore.csi.storage.gke.io" {
		return filestoreBundleReadyDeadline
	}
	return bundleReadyDeadline
}

// firstPVCProvisioningFailure returns a message describing the provider's
// ProvisioningFailed reason for the first of pvcNames that is still unbound and
// has one — so a booting session can fail fast with the ACTUAL storage cause
// (Filestore's "less than minimum share size", a disabled cloud API, quota, a
// missing class) instead of waiting out bundleReadyDeadline behind the generic
// "did not become Ready" text. A bound PVC is never reported: it provisioned
// fine, and any stale failure event predates the bind.
//
// Reads are uncached (r.APIReader when wired): the operator does not watch
// Events, so its cache would return an empty list. Fail-safe in every degenerate
// case — a read error, a bound PVC, or no ProvisioningFailed event — returns
// ("", false), leaving the existing bundle-deadline backstop to catch it.
func (r *Reconciler) firstPVCProvisioningFailure(ctx context.Context, namespace string, pvcNames []string) (string, bool) {
	reader := r.eventReader()
	for _, name := range pvcNames {
		if name == "" {
			continue
		}
		var pvc corev1.PersistentVolumeClaim
		if err := reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &pvc); err != nil {
			continue
		}
		if pvc.Status.Phase == corev1.ClaimBound {
			continue // provisioned fine; any failure event predates the bind
		}
		var events corev1.EventList
		if err := reader.List(ctx, &events, client.InNamespace(namespace)); err != nil {
			continue
		}
		// In-code filter (not a field selector) so this works against both the
		// uncached APIReader and a fake client without a registered index.
		var latest *corev1.Event
		for i := range events.Items {
			e := &events.Items[i]
			if e.InvolvedObject.Name != name || e.Reason != "ProvisioningFailed" {
				continue
			}
			if latest == nil || e.LastTimestamp.After(latest.LastTimestamp.Time) {
				latest = e
			}
		}
		if latest != nil && isPermanentProvisioningFailure(latest.Message) {
			return fmt.Sprintf("workspace storage PVC %q could not be provisioned by its StorageClass: %s",
				name, strings.TrimSpace(latest.Message)), true
		}
	}
	return "", false
}

// isPermanentProvisioningFailure classifies a ProvisioningFailed event message
// as a failure that waiting cannot fix (so fail fast) versus one that resolves
// on retry (so keep waiting for the bundle deadline). It fails SAFE: an
// unrecognized message is treated as transient, so an unknown error never turns
// a slow-but-progressing provision into a dead session.
//
// The distinction matters because GKE Filestore multishare emits retryable
// errors WHILE it works: creating the backing instance surfaces
// "code = Aborted … instances busy … instancecreate" and "code = DeadlineExceeded"
// on every attempt until the instance exists. Fast-failing on those would kill
// every first-of-a-cold-pool session. Permanent failures — a request the backend
// rejects outright (InvalidArgument / minimum share size), a disabled API or
// missing permission (PermissionDenied), a missing class (NotFound), exhausted
// quota — never clear by waiting.
func isPermanentProvisioningFailure(msg string) bool {
	// Transient wins if present: a message that carries both (e.g. a retry that
	// mentions an earlier code) is safer treated as retryable.
	for _, transient := range []string{"Aborted", "DeadlineExceeded", "Unavailable", "instances busy", "already exists", "instancecreate"} {
		if strings.Contains(msg, transient) {
			return false
		}
	}
	for _, permanent := range []string{"InvalidArgument", "PermissionDenied", "NotFound", "minimum share size", "is disabled", "Quota", "quota", "exceeded"} {
		if strings.Contains(msg, permanent) {
			return true
		}
	}
	return false
}

// eventReader prefers the uncached APIReader (Events are not in the operator's
// cache) and falls back to the cached client when it is not wired (tests).
func (r *Reconciler) eventReader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}
