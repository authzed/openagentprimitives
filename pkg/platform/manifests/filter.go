package manifests

import "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

// FilterByInstallTier returns the docs from the input slice whose labels
// match labelKey=labelValue. Used by oap install to separate optional
// regions (e.g. workspace-provisioner) from the always-apply base.
func FilterByInstallTier(docs []*unstructured.Unstructured, labelKey, labelValue string) (matching, rest []*unstructured.Unstructured) {
	for _, d := range docs {
		labels := d.GetLabels()
		if labels[labelKey] == labelValue {
			matching = append(matching, d)
		} else {
			rest = append(rest, d)
		}
	}
	return
}
