package main

// Blank-import the pkg/platform/cloud strategy packages so their init() functions
// run and register their cloud.Strategy implementations before any call to
// cloud.Detect or cloud.For. Without these imports the registry is empty and
// cloud.Default() (and cloud.Detect's fallback to it) returns an error rather
// than a Strategy. pkg/platform/cloud/unmanaged, specifically, must be linked for
// cloud.Default() (and Detect's fallback to it) to resolve the `default`
// kind — pkg/platform/cloud/local registers only the opt-in `local` kind and can
// never satisfy KeyDefault.
import (
	_ "github.com/authzed/openagentprimitives/pkg/platform/cloud/aks"
	_ "github.com/authzed/openagentprimitives/pkg/platform/cloud/desktop"
	_ "github.com/authzed/openagentprimitives/pkg/platform/cloud/eks"
	_ "github.com/authzed/openagentprimitives/pkg/platform/cloud/gke"
	_ "github.com/authzed/openagentprimitives/pkg/platform/cloud/local"
	_ "github.com/authzed/openagentprimitives/pkg/platform/cloud/unmanaged"
)
