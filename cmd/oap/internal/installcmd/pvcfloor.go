package installcmd

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"strings"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	storagev1client "k8s.io/client-go/kubernetes"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/progress"
	"github.com/authzed/openagentprimitives/pkg/platform/cloud"
	"github.com/authzed/openagentprimitives/pkg/platform/manifests"
)

// pvcToCheck identifies one PVC (or one volumeClaimTemplate on a StatefulSet)
// that install will provision, and the StorageClass it resolves to ("" = cluster
// default).
type pvcToCheck struct {
	doc       *unstructured.Unstructured
	tmplIndex int // -1: doc IS a PVC; >=0: index into spec.volumeClaimTemplates
	name      string
	className string
	// clampable is true only for docs install actually applies in place — the
	// base-bundle PVCs. Stateful checks (Postgres/Neo4j) are validate-only
	// copies re-derived from manifests.Split at apply time; clamping them
	// mutates nothing real, so a violation on one of them must always abort,
	// never be offered or honored as a clamp.
	clampable bool
}

// storageClassGetter is the slice of kubernetes.Interface validatePVCFloors
// needs; kubernetes.Interface satisfies it, and the client-go fake clientset
// does too (for tests).
type storageClassGetter = storagev1client.Interface

// validatePVCFloors checks each provisioned PVC's requested size against its
// StorageClass's known disk floor (cloud.StorageFloor). On a violation it
// prompts (interactive) or applies the non-interactive policy: clamp the request
// up to the floor when clampFlag is set, else abort. Clamp mutates the doc in
// place (pure function of doc+floor, SSA-safe). Returns nil to proceed, or an
// aggregated actionable error to abort before any apply.
func validatePVCFloors(ctx context.Context, kc storageClassGetter, checks []pvcToCheck, rep progress.Reporter, in io.Reader, assumeYes, isTTY, clampFlag bool) error {
	scCache := map[string]*storagev1.StorageClass{}
	getSC := func(name string) (*storagev1.StorageClass, error) {
		if name == "" {
			def, err := defaultStorageClass(ctx, kc)
			return def, err
		}
		if sc, ok := scCache[name]; ok {
			return sc, nil
		}
		sc, err := kc.StorageV1().StorageClasses().Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return nil, err
		}
		scCache[name] = sc
		return sc, nil
	}

	var aborts []string
	for _, c := range checks {
		reqStr, ok := readPVCStorage(c)
		if !ok {
			continue // no request set; nothing to validate
		}
		req, err := resource.ParseQuantity(reqStr)
		if err != nil {
			if rep != nil {
				rep.Info("  skipping size check for PVC %s: unparseable storage request %q (%v)", c.name, reqStr, err)
			}
			continue
		}
		sc, err := getSC(c.className)
		if err != nil || sc == nil {
			if rep != nil {
				rep.Info("  skipping size check for PVC %s: StorageClass %q not readable (%v)", c.name, c.className, err)
			}
			continue // can't validate what we can't read; don't hard-fail
		}
		floor, known := cloud.StorageFloor(sc)
		if !known || req.Cmp(floor) >= 0 {
			continue // no floor, or request meets it
		}
		// Violation. Decide clamp vs abort. Clamping only ever applies to
		// clampable (base-bundle) checks: stateful checks are validate-only
		// copies, so a violation there is always an abort and never prompts.
		clamp := false
		if c.clampable {
			clamp = clampFlag
			if !clampFlag && isTTY && !assumeYes {
				clamp = promptClampOrAbort(rep, in, c.name, reqStr, sc.Name, sc.Provisioner, sc.Parameters["type"], floor.String())
			}
		}
		if clamp {
			writePVCStorage(c, floor.String())
			if rep != nil {
				rep.Info("  raised PVC %s from %s to %s to meet the %q floor", c.name, reqStr, floor.String(), sc.Name)
			}
			continue
		}
		msg := fmt.Sprintf(
			"PVC %s requests %s but StorageClass %q (%s/%s) requires >= %s and the PVC would never bind",
			c.name, reqStr, sc.Name, sc.Provisioner, sc.Parameters["type"], floor.String())
		if !c.clampable {
			msg += " (cannot auto-clamp this PVC here — raise its bundled size or its StorageClass)"
		}
		msg += existingPendingHint(ctx, kc, c, floor.String())
		aborts = append(aborts, msg)
	}
	if len(aborts) > 0 {
		return fmt.Errorf(
			"storage preflight: %s.\nRaise the size(s) and re-run, or pass --clamp-undersized-pvcs to raise them automatically, or --artifact-store-url to skip local artifact provisioning",
			strings.Join(aborts, "; "))
	}
	return nil
}

// baseBundlePVCChecks enumerates the PVC docs in the base bundle. They carry no
// storageClassName, so they resolve to the cluster default class.
func baseBundlePVCChecks(baseDocs []*unstructured.Unstructured) []pvcToCheck {
	var checks []pvcToCheck
	for _, d := range baseDocs {
		if d.GetKind() != "PersistentVolumeClaim" {
			continue
		}
		checks = append(checks, pvcToCheck{doc: d, tmplIndex: -1, name: d.GetName(), className: "", clampable: true})
	}
	return checks
}

// statefulPVCChecks returns the Postgres PVC and the Neo4j volumeClaimTemplate,
// resolving to statefulClass ("" = cluster default). Validate-only: a violation
// always aborts (clampable: false) — these docs are throwaway copies re-derived
// from manifests.Split at apply time, so clamping them would mutate nothing
// real (these sizes are bundle-fixed at >= 4Gi and class-injected, so a
// violation is near-impossible, but the abort path stays correct if that ever
// changes).
func statefulPVCChecks(statefulClass string) ([]pvcToCheck, error) {
	var checks []pvcToCheck
	add := func(parts [][]byte) error {
		for _, b := range parts {
			docs, err := manifests.Split(b)
			if err != nil {
				return err
			}
			for _, d := range docs {
				switch d.GetKind() {
				case "PersistentVolumeClaim":
					checks = append(checks, pvcToCheck{doc: d, tmplIndex: -1, name: d.GetName(), className: statefulClass, clampable: false})
				case "StatefulSet":
					tmpls, found, _ := unstructured.NestedSlice(d.Object, "spec", "volumeClaimTemplates")
					if !found {
						continue
					}
					for i, raw := range tmpls {
						tmpl, _ := raw.(map[string]any)
						name, _, _ := unstructured.NestedString(tmpl, "metadata", "name")
						checks = append(checks, pvcToCheck{doc: d, tmplIndex: i, name: d.GetName() + "/" + name, className: statefulClass, clampable: false})
					}
				}
			}
		}
		return nil
	}
	pg, err := manifests.Postgres()
	if err != nil {
		return nil, err
	}
	if err := add(pg); err != nil {
		return nil, err
	}
	neo, err := manifests.Neo4j()
	if err != nil {
		return nil, err
	}
	if err := add(neo); err != nil {
		return nil, err
	}
	return checks, nil
}

// existingPendingHint returns a remediation hint when the PVC already exists on
// the cluster and is stuck Pending — e.g. a prior install created it undersized.
// Its request field is immutable, so the fix is delete+recreate at the floor
// (this preflight can only stop a FRESH provision from going below the floor; it
// cannot repair an already-wedged claim). Best-effort: any read error yields "".
func existingPendingHint(ctx context.Context, kc storageClassGetter, c pvcToCheck, floor string) string {
	if c.tmplIndex >= 0 {
		return "" // a volumeClaimTemplate is not a standalone PVC to Get
	}
	ns := c.doc.GetNamespace()
	if ns == "" {
		return ""
	}
	pvc, err := kc.CoreV1().PersistentVolumeClaims(ns).Get(ctx, c.name, metav1.GetOptions{})
	if err != nil {
		return "" // absent/unreadable: it will be created fresh at the corrected size
	}
	if pvc.Status.Phase == corev1.ClaimPending {
		return fmt.Sprintf(
			" (note: %s/%s already exists and is Pending; its size is immutable — scale the operator to 0, delete the PVC, and recreate it at >= %s)",
			ns, c.name, floor)
	}
	return ""
}

// defaultStorageClass returns the cluster's default StorageClass (annotated
// is-default-class=true), or (nil, nil) if none is marked default.
func defaultStorageClass(ctx context.Context, kc storageClassGetter) (*storagev1.StorageClass, error) {
	list, err := kc.StorageV1().StorageClasses().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	for i := range list.Items {
		if list.Items[i].Annotations["storageclass.kubernetes.io/is-default-class"] == "true" {
			return &list.Items[i], nil
		}
	}
	return nil, nil
}

// readPVCStorage returns the storage request string for a check's PVC or
// volumeClaimTemplate, and whether it was found.
func readPVCStorage(c pvcToCheck) (string, bool) {
	if c.tmplIndex < 0 {
		s, found, _ := unstructured.NestedString(c.doc.Object, "spec", "resources", "requests", "storage")
		return s, found
	}
	tmpls, found, _ := unstructured.NestedSlice(c.doc.Object, "spec", "volumeClaimTemplates")
	if !found || c.tmplIndex >= len(tmpls) {
		return "", false
	}
	tmpl, _ := tmpls[c.tmplIndex].(map[string]any)
	s, found, _ := unstructured.NestedString(tmpl, "spec", "resources", "requests", "storage")
	return s, found
}

// writePVCStorage sets the storage request string on a check's PVC or
// volumeClaimTemplate.
func writePVCStorage(c pvcToCheck, size string) {
	if c.tmplIndex < 0 {
		_ = unstructured.SetNestedField(c.doc.Object, size, "spec", "resources", "requests", "storage")
		return
	}
	tmpls, _, _ := unstructured.NestedSlice(c.doc.Object, "spec", "volumeClaimTemplates")
	tmpl, _ := tmpls[c.tmplIndex].(map[string]any)
	_ = unstructured.SetNestedField(tmpl, size, "spec", "resources", "requests", "storage")
	_ = unstructured.SetNestedSlice(c.doc.Object, tmpls, "spec", "volumeClaimTemplates")
}

// promptClampOrAbort asks the user to abort (default) or clamp a sub-floor PVC.
// Returns true to clamp. Reads a single line via rep.Suspend so it composes with
// the checklist live region.
func promptClampOrAbort(rep progress.Reporter, in io.Reader, name, req, class, provisioner, diskType, floor string) bool {
	var clamp bool
	rep.Suspend(func(out io.Writer, sin io.Reader) {
		src := in
		if src == nil {
			src = sin
		}
		fmt.Fprintf(out, "PVC %s requests %s but StorageClass %s (%s/%s) requires >= %s.\n[A]bort / [c]lamp to %s? (default: abort): ",
			name, req, class, provisioner, diskType, floor, floor)
		line, _ := bufio.NewReader(src).ReadString('\n')
		switch strings.ToLower(strings.TrimSpace(line)) {
		case "c", "clamp":
			clamp = true
		}
	})
	return clamp
}
