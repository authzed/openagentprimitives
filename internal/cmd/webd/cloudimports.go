package main

// Blank-import the pkg/platform/cloud strategy packages so their init() functions run
// and register their cloud.Strategy implementations before run() resolves
// --cluster-kind/AP_CLUSTER_KIND via cloud.For. Without these imports the
// registry has no kinds registered, and cloud.For returns an "unknown cluster
// kind" error for every key — surfaced as a fatal startup error, not a
// nil-interface panic. Mirrors internal/cmd/operator/cloudimports.go, which every OTHER
// Strategy consumer already requires for the same reason. pkg/platform/cloud/unmanaged
// specifically must be linked here: it is the one that registers KeyDefault.
// pkg/platform/cloud/desktop and pkg/platform/cloud/local are both opt-in dev kinds `oap desktop`
// and `oap init --local` select explicitly; they must be linked here or webd
// crash-loops on either flow's own stamped AP_CLUSTER_KIND.
import (
	_ "github.com/authzed/openagentprimitives/pkg/platform/cloud/aks"
	_ "github.com/authzed/openagentprimitives/pkg/platform/cloud/desktop"
	_ "github.com/authzed/openagentprimitives/pkg/platform/cloud/eks"
	_ "github.com/authzed/openagentprimitives/pkg/platform/cloud/gke"
	_ "github.com/authzed/openagentprimitives/pkg/platform/cloud/local"
	_ "github.com/authzed/openagentprimitives/pkg/platform/cloud/unmanaged"
)
