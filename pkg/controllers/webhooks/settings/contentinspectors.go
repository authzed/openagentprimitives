package settings

import (
	"fmt"
	"strings"

	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/contentguard"
	"github.com/authzed/openagentprimitives/pkg/authz/contentguard/registry"
)

// ContentInspectorsError returns "" when every limits.contentInspectors entry
// names a registered inspector AND its config validates (Configure succeeds);
// otherwise a descriptive message. Fail-closed: a misconfigured guard must never
// reach a session. nil contentInspectors passes.
func ContentInspectorsError(spec *v1.SettingsSpec) string {
	if spec.Limits == nil || spec.Limits.ContentInspectors == nil {
		return ""
	}
	all := registry.All()
	valid := make([]string, 0, len(all))
	for _, i := range all {
		valid = append(valid, i.ID())
	}
	var detectorProviders []string
	for idx, ci := range *spec.Limits.ContentInspectors {
		insp, ok := registry.Get(ci.ID)
		if !ok {
			return fmt.Sprintf("contentInspectors[%d].id %q is not registered; valid ids: [%s]",
				idx, ci.ID, strings.Join(valid, ", "))
		}
		if _, err := insp.Configure(ci.Config.Raw); err != nil {
			return fmt.Sprintf("contentInspectors[%d] (%s): invalid config: %v", idx, ci.ID, err)
		}
		// A detector-providing inspector injects a co-located detector sidecar
		// reachable via the single CONTENTGUARD_DETECTOR_ENDPOINT env var on the
		// runner. Two of them would shadow each other (the operator's podspec
		// second write wins), so at most one is supported. The check iterates the
		// registry generically (type-assert), with no branch on a specific id.
		if _, isDP := insp.(contentguard.DetectorProvider); isDP {
			detectorProviders = append(detectorProviders, ci.ID)
		}
	}
	if len(detectorProviders) > 1 {
		return fmt.Sprintf("at most one detector-providing content inspector is supported; got %d: [%s]",
			len(detectorProviders), strings.Join(detectorProviders, ", "))
	}
	return ""
}
