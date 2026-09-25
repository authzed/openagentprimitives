package spicedb

import (
	"context"
	"fmt"
	"os"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	authzspicedb "github.com/authzed/openagentprimitives/pkg/authz/spicedb"
)

// NewClientFromEnv constructs a SpiceDB client from the standard SPICEDB_*
// env vars. Used by session grant/revoke/participants.
func NewClientFromEnv() (*authzspicedb.Client, error) {
	endpoint := os.Getenv(authzspicedb.EnvEndpoint)
	token := os.Getenv(authzspicedb.EnvToken)
	insecure := os.Getenv(authzspicedb.EnvInsecure) == "true"
	if endpoint == "" || token == "" {
		return nil, fmt.Errorf("%s and %s must be set", authzspicedb.EnvEndpoint, authzspicedb.EnvToken)
	}
	return authzspicedb.NewClient(endpoint, token, insecure)
}

// ClusterDialer establishes a SpiceDB client by reaching the in-cluster
// SpiceDB — port-forwarding the Service and reading the preshared token from
// the install-time Secret. Injected into NewAuthzClient so the env-vs-cluster
// decision is unit-testable without a real cluster.
type ClusterDialer func(ctx context.Context) (*authzspicedb.Client, func(), error)

// NewAuthzClient builds a SpiceDB client for the platform admin commands.
//
// It prefers the SPICEDB_* env vars when BOTH endpoint and token are set (an
// explicit override — e.g. running in-cluster, or against a manual
// port-forward). Otherwise it falls back to dialCluster, which resolves the
// connection from the cluster's own settings so the operator doesn't have to
// hand-set env vars just to grant an admin.
//
// The returned cleanup func is always non-nil on success and MUST be called by
// the caller — it closes the client and tears down any port-forward the
// cluster path opened.
func NewAuthzClient(ctx context.Context, dialCluster ClusterDialer) (*authzspicedb.Client, func(), error) {
	endpoint := os.Getenv(authzspicedb.EnvEndpoint)
	token := os.Getenv(authzspicedb.EnvToken)
	insecure := os.Getenv(authzspicedb.EnvInsecure) == "true"
	if endpoint != "" && token != "" {
		cl, err := authzspicedb.NewClient(endpoint, token, insecure)
		if err != nil {
			return nil, nil, err
		}
		return cl, func() { _ = cl.Close() }, nil
	}
	if dialCluster == nil {
		return nil, nil, fmt.Errorf(
			"%s and %s not set and no cluster connection available to resolve them from the cluster's settings",
			authzspicedb.EnvEndpoint, authzspicedb.EnvToken)
	}
	return dialCluster(ctx)
}

// ClusterAuthzDialer returns a ClusterDialer bound to g. It port-forwards the
// in-cluster SpiceDB and reads its preshared token from the install-time
// Secret — the same path `oap agent chat` uses — so `oap platform` commands work
// against a cluster with nothing but a kubeconfig.
func ClusterAuthzDialer(g *apcmd.Globals) ClusterDialer {
	return func(ctx context.Context) (*authzspicedb.Client, func(), error) {
		b, err := g.Bundle()
		if err != nil {
			return nil, nil, fmt.Errorf(
				"connect to cluster to resolve SpiceDB settings (or set %s/%s to override): %w",
				authzspicedb.EnvEndpoint, authzspicedb.EnvToken, err)
		}
		return DialViaPortForward(ctx, b)
	}
}
