package main

// Blank-import the pkg/platform/cloud strategy packages so their init() functions run
// and register their cloud.Strategy implementations before admind's oap-install
// capacity check (pkg/web/admind/oapinstall_capacity.go) calls cloud.Detect/cloud.For.
// Without these imports the registry has no KeyDefault kind registered, and
// cloud.Default()/cloud.Detect's fallback returns an error rather than a
// Strategy — surfaced as a capacity-check skip notice, not a nil-interface
// panic. Mirrors cmd/oap/cloudimports.go, which every OTHER Strategy consumer
// already requires for the same reason. pkg/platform/cloud/unmanaged specifically must
// be linked here: it is the one that registers KeyDefault; pkg/platform/cloud/local
// registers only the opt-in `local` kind.
import (
	_ "github.com/authzed/openagentprimitives/pkg/platform/cloud/aks"
	_ "github.com/authzed/openagentprimitives/pkg/platform/cloud/desktop"
	_ "github.com/authzed/openagentprimitives/pkg/platform/cloud/eks"
	_ "github.com/authzed/openagentprimitives/pkg/platform/cloud/gke"
	_ "github.com/authzed/openagentprimitives/pkg/platform/cloud/local"
	_ "github.com/authzed/openagentprimitives/pkg/platform/cloud/unmanaged"
)
