package manifests

import (
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"
)

// InjectStorageClass sets spec.storageClassName on a single-document PVC or
// StatefulSet manifest (the latter on every volumeClaimTemplate), re-marshaling
// to YAML. A non-PVC/non-StatefulSet document, or an empty class, returns the
// input unchanged. This is how oap install steers the bundled Postgres/Neo4j
// PVCs onto a cloud-resolved RWO class without editing the embedded files.
func InjectStorageClass(in []byte, class string) ([]byte, error) {
	if class == "" {
		return in, nil
	}
	docs, err := Split(in)
	if err != nil {
		return nil, fmt.Errorf("InjectStorageClass: split: %w", err)
	}
	if len(docs) != 1 {
		return nil, fmt.Errorf("InjectStorageClass: expected 1 document, got %d", len(docs))
	}
	u := docs[0]
	switch u.GetKind() {
	case "PersistentVolumeClaim":
		if err := unstructured.SetNestedField(u.Object, class, "spec", "storageClassName"); err != nil {
			return nil, fmt.Errorf("InjectStorageClass: set PVC storageClassName: %w", err)
		}
	case "StatefulSet":
		tmpls, found, err := nestedSlice(u.Object, "spec", "volumeClaimTemplates")
		if err != nil {
			return nil, fmt.Errorf("InjectStorageClass: read volumeClaimTemplates: %w", err)
		}
		if !found {
			return in, nil
		}
		for i := range tmpls {
			tmpl, ok := tmpls[i].(map[string]any)
			if !ok {
				return nil, fmt.Errorf("InjectStorageClass: volumeClaimTemplates[%d] is not a map", i)
			}
			if err := unstructured.SetNestedField(tmpl, class, "spec", "storageClassName"); err != nil {
				return nil, fmt.Errorf("InjectStorageClass: set template[%d] storageClassName: %w", i, err)
			}
			tmpls[i] = tmpl
		}
		if err := unstructured.SetNestedSlice(u.Object, tmpls, "spec", "volumeClaimTemplates"); err != nil {
			return nil, fmt.Errorf("InjectStorageClass: write volumeClaimTemplates: %w", err)
		}
	default:
		return in, nil
	}
	out, err := yaml.Marshal(u.Object)
	if err != nil {
		return nil, fmt.Errorf("InjectStorageClass: marshal: %w", err)
	}
	return out, nil
}

// nestedString / nestedSlice are thin re-exports of the unstructured accessors
// so callers (and tests) in this package don't import unstructured directly.
func nestedString(obj map[string]any, fields ...string) (string, bool, error) {
	return unstructured.NestedString(obj, fields...)
}
func nestedSlice(obj map[string]any, fields ...string) ([]any, bool, error) {
	return unstructured.NestedSlice(obj, fields...)
}

// NestedStringForTest exposes unstructured.NestedString to package-external
// tests (cmd/oap) that verify storageClassName injection.
func NestedStringForTest(obj map[string]any, fields ...string) (string, bool, error) {
	return unstructured.NestedString(obj, fields...)
}
