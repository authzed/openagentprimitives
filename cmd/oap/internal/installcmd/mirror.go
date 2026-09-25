package installcmd

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/cliout"
	"github.com/authzed/openagentprimitives/pkg/platform/apimage"
)

// mirrorImageRefs rewrites a workload doc's container + initContainer images to
// their mirrored refs (mirrorMap: original → mirror). No-op when mirrorMap is
// empty or the doc has no matching images.
func mirrorImageRefs(doc *unstructured.Unstructured, mirrorMap map[string]string) error {
	if len(mirrorMap) == 0 {
		return nil
	}
	for _, path := range [][]string{
		{"spec", "template", "spec", "containers"},
		{"spec", "template", "spec", "initContainers"},
	} {
		cs, found, err := unstructured.NestedSlice(doc.Object, path...)
		if err != nil || !found {
			continue
		}
		changed := false
		for i, c := range cs {
			cm, ok := c.(map[string]any)
			if !ok {
				continue
			}
			if img, _ := cm["image"].(string); mirrorMap[img] != "" {
				cm["image"] = mirrorMap[img]
				cs[i] = cm
				changed = true
			}
		}
		if changed {
			if err := unstructured.SetNestedSlice(doc.Object, cs, path...); err != nil {
				return err
			}
		}
	}
	return nil
}

// mirrorWorkspaceHelperImage rewrites the local-path-provisioner helper image
// (busybox) to its mirror. mirrorImageRefs can't reach it because it lives in
// the provisioner's --helper-image arg and inside the helperPod.yaml ConfigMap
// data, not a structured container image field — so without this an air-gapped
// RWX install spawns helper pods that pull docker.io/busybox → ImagePullBackOff.
// No-op when busybox isn't in mirrorMap (--mirror-dependencies off).
func mirrorWorkspaceHelperImage(doc *unstructured.Unstructured, mirrorMap map[string]string) error {
	mirror := mirrorMap["busybox"]
	if mirror == "" {
		return nil
	}
	switch doc.GetKind() {
	case "Deployment":
		containers, found, err := unstructured.NestedSlice(doc.Object, "spec", "template", "spec", "containers")
		if err != nil {
			return fmt.Errorf("mirror helper image: read %s/%s containers: %w", doc.GetKind(), doc.GetName(), err)
		}
		if !found {
			return nil
		}
		changed := false
		for i, c := range containers {
			cm, ok := c.(map[string]any)
			if !ok {
				continue
			}
			args, _ := cm["args"].([]any)
			argChanged := false
			for j, a := range args {
				if s, _ := a.(string); s == "--helper-image=busybox" {
					args[j] = "--helper-image=" + mirror
					argChanged = true
				}
			}
			if argChanged {
				cm["args"] = args
				containers[i] = cm
				changed = true
			}
		}
		if changed {
			if err := unstructured.SetNestedSlice(doc.Object, containers, "spec", "template", "spec", "containers"); err != nil {
				return err
			}
		}
	case "ConfigMap":
		data, found, err := unstructured.NestedMap(doc.Object, "data")
		if err != nil {
			return fmt.Errorf("mirror helper image: read %s/%s data: %w", doc.GetKind(), doc.GetName(), err)
		}
		if !found {
			return nil
		}
		changed := false
		for k, v := range data {
			if s, ok := v.(string); ok && strings.Contains(s, "image: busybox") {
				data[k] = strings.ReplaceAll(s, "image: busybox", "image: "+mirror)
				changed = true
			}
		}
		if changed {
			if err := unstructured.SetNestedMap(doc.Object, data, "data"); err != nil {
				return err
			}
		}
	}
	return nil
}

// mirrorOperatorSnapshotImage appends --snapshot-image=<mirror> to the operator
// Deployment's container args when the snapshot image is being mirrored, so
// air-gapped workspace snapshot/restore Jobs pull from the mirror (the image
// is an operator binary default, not a manifest field mirrorImageRefs can
// reach). No-op when SnapshotImage isn't in mirrorMap.
func mirrorOperatorSnapshotImage(doc *unstructured.Unstructured, mirrorMap map[string]string, operatorName string) error {
	mirror := mirrorMap[apimage.SnapshotImage]
	if mirror == "" || doc.GetKind() != "Deployment" || doc.GetName() != operatorName {
		return nil
	}
	containers, found, err := unstructured.NestedSlice(doc.Object, "spec", "template", "spec", "containers")
	if err != nil {
		return fmt.Errorf("mirror snapshot image: read %s containers: %w", operatorName, err)
	}
	// This runs only for the operator Deployment (guarded above), which always
	// has a container — a missing/empty/malformed one is a packaging bug that
	// would otherwise silently skip --snapshot-image and ImagePullBackOff later.
	if !found || len(containers) == 0 {
		return fmt.Errorf("mirror snapshot image: operator Deployment %s has no containers", operatorName)
	}
	c0, ok := containers[0].(map[string]any)
	if !ok {
		return fmt.Errorf("mirror snapshot image: operator Deployment %s container[0] is not a map", operatorName)
	}
	args, _ := c0["args"].([]any)
	c0["args"] = append(args, "--snapshot-image="+mirror)
	containers[0] = c0
	if err := unstructured.SetNestedSlice(doc.Object, containers, "spec", "template", "spec", "containers"); err != nil {
		return fmt.Errorf("mirror snapshot image: write %s args: %w", operatorName, err)
	}
	return nil
}

// mirrorDependencies copies each public dependency image under registry via
// `docker buildx imagetools create` (registry-to-registry, multi-arch manifest
// preserved, no local pull). Requires the user to be `docker login`'d to both
// the source (if private) and the destination registry.
func mirrorDependencies(ctx context.Context, out io.Writer, registry string) error {
	for _, src := range apimage.DependencyImages {
		dst := apimage.MirrorRef(src, registry)
		cliout.Step(out, "mirror %s -> %s", src, dst)
		cmd := exec.CommandContext(ctx, "docker", "buildx", "imagetools", "create", "-t", dst, src)
		cmd.Stdout = out
		cmd.Stderr = out
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("mirror %s: %w", src, err)
		}
	}
	return nil
}
