package v1alpha1

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/authzed/openagentprimitives/pkg/platform/workspacekinds"
)

// ValidateWorkspaceSourceSpec performs structural validation of a
// WorkspaceSource spec (kind/locator present, scope paths safe, base size
// parseable). Driver-specific validation (that the kind is registered and the
// locator is well-formed for that kind) happens in the controller, which can
// import the driver registry; this apis-package function must not.
func ValidateWorkspaceSourceSpec(name string, s WorkspaceSourceSpec) error {
	var problems []string
	if strings.TrimSpace(s.Source.Kind) == "" {
		problems = append(problems, "source.kind is required")
	}
	if strings.TrimSpace(s.Source.Locator) == "" {
		problems = append(problems, "source.locator is required")
	}
	for i, p := range s.Scope.Paths {
		if err := workspacekinds.ValidateScopePath(p.Path); err != nil {
			problems = append(problems, fmt.Sprintf("scope.paths[%d]: %v", i, err))
		}
	}
	if s.Base.Size != "" {
		if _, err := resource.ParseQuantity(s.Base.Size); err != nil {
			problems = append(problems, fmt.Sprintf("base.size %q is not a valid quantity", s.Base.Size))
		}
	}
	if s.Base.Refresh != "" && s.Base.Refresh != "onDemand" {
		if d, err := time.ParseDuration(s.Base.Refresh); err != nil {
			problems = append(problems, fmt.Sprintf("base.refresh %q is not \"onDemand\" or a valid duration", s.Base.Refresh))
		} else if d < time.Minute {
			problems = append(problems, fmt.Sprintf("base.refresh %q must be \"onDemand\" or a duration >= 1m", s.Base.Refresh))
		}
	}
	if len(problems) == 0 {
		return nil
	}
	sort.Strings(problems)
	return fmt.Errorf("workspacesource %q invalid: %s", name, strings.Join(problems, "; "))
}

// Scope-path safety (absolute, "..", "~", backslash, whitespace-padded) is
// enforced by the shared workspacekinds.ValidateScopePath above — one rule set
// for both this reconcile-time CRD check (Valid=False on a bad path) and the
// driver's own independent re-check when it builds commands (defense in depth;
// there is no admission webhook or CEL rule wired for WorkspaceSource).
