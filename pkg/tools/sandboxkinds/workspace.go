package sandboxkinds

import (
	"fmt"
	"sort"
)

// DomainKubernetesPVC is the workspace domain of backends whose workspace is a
// Kubernetes PersistentVolumeClaim. Backends reporting it can share a workspace
// with each other, and it is the only domain an in-cluster workspace-source
// reconcile pod can write.
const DomainKubernetesPVC = "kubernetes-pvc"

// CheckWorkspaceDomains reports whether a set of bundles that share one
// workspace can actually share it.
//
// Supports(FeatureSharedWorkspace) cannot answer this: two backends can each
// support shared workspaces and still be unable to share with EACH OTHER,
// because they store the workspace in different worlds. WorkspaceDomain names
// that world, and sharing requires agreement.
//
// resolved maps bundle name to its already-resolved Kind — a parameter rather
// than a registry lookup, so this package never imports its own registry and a
// consumer links only the backends it blank-imports. An unregistered kind name
// is refused by the caller's registry.Get, not here.
//
// Two independent rules, not one:
//   - hasWorkspaceSource pins EVERY bundle's domain to Kubernetes storage,
//     because an in-cluster reconcile pod can only write a Kubernetes volume —
//     whether or not bundles share a workspace with each other.
//   - sharedWorkspace gates cross-bundle domain AGREEMENT: bundles with their
//     own isolated workspaces never need to agree on where it lives.
//
// Failing closed matters more than usual here: the alternative is a session
// that starts, hands the second bundle an empty directory, and says nothing.
//
// NOT YET WIRED — tested, but with no production caller. Latent rather than
// wrong: both shipped kinds return DomainKubernetesPVC, so no reachable input
// can make it error. It goes live when a third backend registers a different
// domain. Its home is the bundle loop in
// pkg/controllers/agentsession/controller.go, the only place holding the
// workspace claim, the workspace overlay and the per-bundle resolved kinds at
// once; wiring it adds a rejection path there.
func CheckWorkspaceDomains(resolved map[string]Kind, sharedWorkspace, hasWorkspaceSource bool) error {
	if len(resolved) == 0 {
		return nil
	}

	names := make([]string, 0, len(resolved))
	for n := range resolved {
		names = append(names, n)
	}
	sort.Strings(names)

	if hasWorkspaceSource {
		for _, bundle := range names {
			k := resolved[bundle]
			domain := k.WorkspaceDomain()
			if domain != DomainKubernetesPVC {
				return fmt.Errorf("bundle %q uses sandbox kind %q (workspace domain %q), but a workspace source "+
					"is configured and its reconciler writes a Kubernetes volume", bundle, k.Name(), domain)
			}
		}
	}

	if !sharedWorkspace {
		return nil
	}

	type holder struct{ bundle, kind, domain string }
	var first *holder

	for _, bundle := range names {
		k := resolved[bundle]
		kind := k.Name()
		domain := k.WorkspaceDomain()
		if domain == "" {
			return fmt.Errorf("bundle %q uses sandbox kind %q, which cannot share a workspace; "+
				"use an isolated workspace or a different backend", bundle, kind)
		}
		if first == nil {
			first = &holder{bundle: bundle, kind: kind, domain: domain}
			continue
		}
		if domain != first.domain {
			return fmt.Errorf("bundles %q (sandbox kind %q, workspace domain %q) and %q "+
				"(sandbox kind %q, workspace domain %q) share a workspace but store it in different "+
				"places; use one backend for both, or give them isolated workspaces",
				first.bundle, first.kind, first.domain, bundle, kind, domain)
		}
	}
	return nil
}
