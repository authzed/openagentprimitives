package installcmd

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/kube"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/progress"
	"github.com/authzed/openagentprimitives/pkg/platform/cloud"
)

// StatefulResolveOptions controls RWO stateful-storage resolution. Driven by
// oap install's --stateful-storage-class flag.
type StatefulResolveOptions struct {
	// ExplicitClass, when non-empty, short-circuits detection: the bundled
	// Postgres/Neo4j PVCs are pinned to it directly (no create, no probe).
	ExplicitClass string
}

// createStorageClass creates sc, tolerating AlreadyExists (idempotent re-run).
func createStorageClass(ctx context.Context, kc kubernetes.Interface, sc *storagev1.StorageClass) error {
	_, err := kc.StorageV1().StorageClasses().Create(ctx, sc, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		return nil
	}
	return err
}

// runStatefulStorageWork performs the side-effecting half of the stateful-storage
// decision, BEFORE the data-plane pipeline applies the (immutable) Postgres/Neo4j
// PVCs: emit the decision message, create the resolved StorageClass when one is
// needed, and — when Probe is set — verify the node pool can ACTUALLY attach an
// RWO volume of that class via a pod-running probe. A probe or create failure is
// a HARD error: stateful storage is mandatory, there is no silent degrade.
func runStatefulStorageWork(ctx, recheckCtx context.Context, dec cloud.StatefulDecision, bundle *kube.Bundle, rep progress.Reporter) error {
	if dec.ClassName == "" {
		return nil // trust the cluster default unchanged
	}
	rep.Info("resolve stateful storage")
	if dec.Message != "" {
		rep.Info("%s", dec.Message)
	}
	if dec.CreateClass != nil {
		if err := createStorageClass(ctx, bundle.Typed, dec.CreateClass); err != nil {
			return fmt.Errorf("create stateful StorageClass %q: %w", dec.CreateClass.Name, err)
		}
		rep.Info("created stateful StorageClass %s", dec.CreateClass.Name)
	}
	if dec.Probe {
		probe, cleanup, perr := cloud.NewProvisioningProbe(ctx, bundle.Typed, dec.ClassName, cloud.ProbeOptions{
			AccessMode:        corev1.ReadWriteOnce,
			WaitForPodRunning: true,
		})
		if perr != nil {
			return fmt.Errorf("probe stateful storage class %q: %w", dec.ClassName, perr)
		}
		defer cleanup()
		if _, perr := awaitProvisioningProbe(ctx, recheckCtx, "stateful storage "+dec.ClassName, dec.ClassName, probe, rep); perr != nil {
			return perr
		}
	}
	rep.Info("stateful storage class: %s", dec.ClassName)
	return nil
}

// resolveStatefulChoice computes the RWO stateful-storage decision up front:
// the explicit-override short-circuit, else the cloud strategy's pure-detection
// Resolve. Read-only on the cluster. Called BEFORE buildCoreComponents so the
// class name is known both for the plan summary and the manifest injection; the
// side-effecting work (StorageClass create + probe) is performed later by
// runStatefulStorageWork. rep carries the strategy's Resolve narration through
// the install reporter so it does not corrupt the checklist's live region.
func resolveStatefulChoice(ctx context.Context, bundle *kube.Bundle, strat cloud.Strategy, opts StatefulResolveOptions, rep progress.Reporter) (cloud.StatefulDecision, error) {
	if opts.ExplicitClass != "" {
		// Operator override: pin directly, trust it (no create, no probe).
		return cloud.StatefulDecision{ClassName: opts.ExplicitClass}, nil
	}
	dec, err := strat.StatefulStorage().Resolve(ctx, cloud.StatefulParams{
		Clients:  cloudClients(bundle),
		Reporter: newCloudReporterRep(rep),
	})
	if err != nil {
		return cloud.StatefulDecision{}, fmt.Errorf("resolve stateful storage: %w", err)
	}
	return dec, nil
}
