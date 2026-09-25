package settings

import (
	"fmt"

	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// ModelCatalogError validates a settings spec's model catalog. "" = OK.
// Cluster entries must carry a complete TokenRef; at most one Default; no
// duplicate names; the Default must not be denied. Namespace entries are
// name-only narrowing refs — TokenRef and Default are forbidden.
func ModelCatalogError(spec *v1.SettingsSpec, isCluster bool) string {
	if spec == nil || spec.ModelCatalog == nil {
		return ""
	}
	denied := map[string]bool{}
	if spec.Limits != nil {
		for _, n := range spec.Limits.DeniedModels {
			denied[n] = true
		}
	}
	seen := map[string]bool{}
	defaults := 0
	for _, e := range *spec.ModelCatalog {
		if e.Name == "" {
			return "catalog entry has an empty name"
		}
		if seen[e.Name] {
			return fmt.Sprintf("duplicate catalog entry %q", e.Name)
		}
		seen[e.Name] = true
		if isCluster {
			if e.TokenRef == nil || e.TokenRef.Namespace == "" || e.TokenRef.Name == "" || e.TokenRef.Key == "" {
				return fmt.Sprintf("cluster catalog entry %q must set tokenRef.{namespace,name,key}", e.Name)
			}
			if e.Default {
				defaults++
				if denied[e.Name] {
					return fmt.Sprintf("default catalog entry %q is also in deniedModels", e.Name)
				}
			}
		} else {
			if e.TokenRef != nil {
				return fmt.Sprintf("namespace catalog entry %q must not set tokenRef (central tokens are cluster-only)", e.Name)
			}
			if e.Default {
				return fmt.Sprintf("namespace catalog entry %q must not set default", e.Name)
			}
		}
	}
	if defaults > 1 {
		return "more than one catalog entry marked default"
	}
	return ""
}
