package installcmd

// The cluster-kind registry is populated by blank imports, which the oap binary
// makes from main (cmd/oap/cloudimports.go). A test binary links only what this
// package's own imports reach; without these, cloud.For and cloud.MustFor
// cannot resolve the keys the install-shape tests below name.
import (
	_ "github.com/authzed/openagentprimitives/pkg/platform/cloud/aks"
	_ "github.com/authzed/openagentprimitives/pkg/platform/cloud/desktop"
	_ "github.com/authzed/openagentprimitives/pkg/platform/cloud/eks"
	_ "github.com/authzed/openagentprimitives/pkg/platform/cloud/gke"
	_ "github.com/authzed/openagentprimitives/pkg/platform/cloud/local"
	_ "github.com/authzed/openagentprimitives/pkg/platform/cloud/unmanaged"
)
