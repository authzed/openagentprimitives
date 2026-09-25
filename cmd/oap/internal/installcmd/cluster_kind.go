// Resolves the cluster kind ONCE, at the CLI edge.
//
// Every downstream consumer takes the resolved cloud.Strategy as a parameter;
// nothing re-detects. This replaces the `localMode`/`allowSharedOrigin` bool
// that used to be threaded through runInit/RunInstall and branched on in
// thirteen places.
package installcmd

import (
	"context"
	"fmt"
	"strings"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/authzed/openagentprimitives/pkg/platform/cloud"
)

// clusterKindFlagUsage is the shared --cluster-kind description for `oap init`
// and `oap install`. Built from cloud.RegisteredKeys() (not a hardcoded
// literal) so a new cloud/<kind> package shows up here automatically instead
// of silently going stale in --help while still being valid at cloud.For.
// Safe as a package-level var initializer: cmd/oap/cloudimports.go
// blank-imports every pkg/platform/cloud/<kind> package in this same package, and Go
// fully initializes (including init()) a package's imports before that
// package's own var initializers run.
var clusterKindFlagUsage = fmt.Sprintf("Cluster kind: %s. "+
	"Omit to detect it from the cluster's node providerIDs (falling back to `default`). "+
	"`local` selects the lightweight developer profile (sqlite memory, in-memory SpiceDB, "+
	"a file:// artifact PVC, local :dev images) and is never auto-detected.",
	strings.Join(cloud.RegisteredKeys(), ", "))

// resolveClusterKind returns the Strategy for this install.
//
// An explicit kind wins; an empty one is detected. Either way the chosen kind
// validates against the cluster actually connected BEFORE any mutation, so a
// wrong --cluster-kind fails fast instead of aborting mid-install and leaving a
// half-deployed, crash-looping cluster behind.
func resolveClusterKind(ctx context.Context, kind string, kc kubernetes.Interface, cfg *rest.Config, contextName string, allowOverride bool) (cloud.Strategy, error) {
	var strat cloud.Strategy
	var err error
	if kind == "" {
		strat, err = cloud.Detect(ctx, kc)
	} else {
		strat, err = cloud.For(kind)
	}
	if err != nil {
		return nil, err
	}
	if err := strat.Validate(ctx, cloud.ValidateParams{
		Clients:       cloud.Clients{Typed: kc},
		RESTConfig:    cfg,
		ContextName:   contextName,
		AllowOverride: allowOverride,
	}); err != nil {
		return nil, err
	}
	return strat, nil
}
