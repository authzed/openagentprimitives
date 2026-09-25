package workspacesource

import (
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/workspacekinds"
)

// SpecToDriver adapts a WorkspaceSource spec into the Kubernetes-free
// workspacekinds.Spec the driver operates on. This is the single apis→driver
// boundary; the driver registry is never handed a CRD type.
func SpecToDriver(s spiceboxv1alpha1.WorkspaceSourceSpec) workspacekinds.Spec {
	paths := make([]workspacekinds.PathRule, 0, len(s.Scope.Paths))
	for _, p := range s.Scope.Paths {
		paths = append(paths, workspacekinds.PathRule{Path: p.Path, Writable: p.Writable})
	}
	return workspacekinds.Spec{
		Kind:    s.Source.Kind,
		Locator: s.Source.Locator,
		Ref:     s.Source.Ref,
		Config:  s.Source.Config,
		Scope:   workspacekinds.Scope{Paths: paths},
	}
}
