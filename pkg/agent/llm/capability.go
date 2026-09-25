package llm

// Capability names a provider-neutral, model-level feature that a caller may
// need to branch on — e.g. whether a model can emit or ingest files natively
// rather than through a text-only round trip. Per-model capability data lives
// in pkg/agent/llm/models; this file defines only the vocabulary, so that
// registry and Provider.Capabilities share one type.
type Capability string

const (
	// CapNativeFileOut marks a model that can emit files (images, documents,
	// etc.) as native output content rather than requiring the caller to
	// synthesize them from text.
	CapNativeFileOut Capability = "native_file_out"

	// CapNativeFileIn marks a model that can ingest files as native input
	// content rather than requiring the caller to pre-process them into text.
	CapNativeFileIn Capability = "native_file_in"
)

// CapabilitySet is an unordered collection of Capability values. The zero
// value is not usable — construct with NewCapabilitySet (an empty call
// yields a valid, empty set).
type CapabilitySet map[Capability]struct{}

// NewCapabilitySet builds a CapabilitySet from the given capabilities. Calling
// it with no arguments yields a valid, empty set (Has always returns false).
func NewCapabilitySet(caps ...Capability) CapabilitySet {
	set := make(CapabilitySet, len(caps))
	for _, c := range caps {
		set[c] = struct{}{}
	}
	return set
}

// Has reports whether the set contains the given capability.
func (s CapabilitySet) Has(c Capability) bool {
	_, ok := s[c]
	return ok
}
