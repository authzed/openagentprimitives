package settings

import (
	"fmt"
	"strings"

	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/pinning/registry"
	resolver "github.com/authzed/openagentprimitives/pkg/platform/settings"
)

// PinningKindsError returns "" when every limits.pinning rule and bypass in the
// spec names a kind registered in the pinning registry, or a descriptive message
// naming the first unrecognised kind and listing the valid set. nil pinning
// passes with no error.
func PinningKindsError(spec *v1.SettingsSpec) string {
	if spec.Limits == nil || spec.Limits.Pinning == nil {
		return ""
	}
	all := registry.All()
	valid := make([]string, 0, len(all))
	known := make(map[string]bool, len(all))
	for _, k := range all {
		known[k.Name()] = true
		valid = append(valid, k.Name())
	}
	validList := strings.Join(valid, ", ")

	for _, r := range spec.Limits.Pinning.Rules {
		if !known[r.Kind] {
			return fmt.Sprintf("pinning rule kind %q is not registered; valid kinds: [%s]", r.Kind, validList)
		}
	}
	for _, b := range spec.Limits.Pinning.Bypass {
		if !known[b.Kind] {
			return fmt.Sprintf("pinning bypass kind %q is not registered; valid kinds: [%s]", b.Kind, validList)
		}
	}
	return ""
}

// SelfConsistencyError describes why a Settings spec's defaults contradict its
// own limits, or "" if it is self-consistent. It resolves the spec's own
// defaults against its own limits; an allowlist/clamp violation means a default
// can never apply. ModelMissingCredential/ModelMissingModel are NOT
// inconsistencies (a name-only default model legitimately has no apiKey at
// this tier).
func SelfConsistencyError(spec *v1.SettingsSpec) string {
	in := resolver.Inputs{Cluster: spec}
	if spec.Defaults != nil {
		if spec.Defaults.Model != nil {
			in.ClassModel = &v1.ModelConfig{Provider: spec.Defaults.Model.Provider, Name: spec.Defaults.Model.Name}
		}
		in.ClassBudget = spec.Defaults.Budget
	}
	_, vs := resolver.Resolve(in)
	for i := range vs {
		switch vs[i].Reason {
		case resolver.ReasonModelMissingCredential, resolver.ReasonModelMissingModel:
			continue
		}
		return vs[i].Message
	}
	return ""
}
