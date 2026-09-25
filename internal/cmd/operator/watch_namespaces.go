package main

import (
	"strings"

	"sigs.k8s.io/controller-runtime/pkg/cache"
)

// cacheNamespaces folds the --watch-namespaces flag into the manager cache's
// DefaultNamespaces map. An empty watch set returns nil, which leaves the cache
// cluster-wide (the default, matching most operators). A non-empty set scopes
// every informer/Owns watch to those namespaces — and
// always folds in the operator's own namespace so its fixed-infra Secrets /
// ConfigMaps (NATS identity/TLS, publisher-keys) stay cached even when the watch
// set lists only tenant namespaces. Blank entries are trimmed and dropped.
//
// Scoping the cache is the prerequisite for deploying the operator with
// per-namespace RoleBindings instead of the cluster-wide ClusterRoleBinding: a
// cluster-wide list/watch would 403 under namespaced RBAC.
func cacheNamespaces(watch []string, systemNS string) map[string]cache.Config {
	if len(watch) == 0 {
		return nil
	}
	out := make(map[string]cache.Config, len(watch)+1)
	for _, ns := range watch {
		if ns = strings.TrimSpace(ns); ns != "" {
			out[ns] = cache.Config{}
		}
	}
	if systemNS != "" {
		out[systemNS] = cache.Config{}
	}
	return out
}
