package v1alpha1

// ConfigMapKeyRef points at a single key inside a ConfigMap in the same
// namespace as the referencing CR.
type ConfigMapKeyRef struct {
	// Name is the ConfigMap's name.
	Name string `json:"name"`
	// Key is the data key within the ConfigMap.
	Key string `json:"key"`
}
