package agentcmd

// The cluster-kind registry is populated by blank imports, which the oap binary
// makes from main (cmd/oap/cloudimports.go). A test binary links only what this
// package's own imports reach, so without these the registry is empty and
// every capacity/registry decision below degrades to "no default cluster kind
// registered" — a skip, not a failure, which is exactly the shape that would
// let these tests pass while asserting nothing.
import (
	_ "github.com/authzed/openagentprimitives/pkg/platform/cloud/aks"
	_ "github.com/authzed/openagentprimitives/pkg/platform/cloud/desktop"
	_ "github.com/authzed/openagentprimitives/pkg/platform/cloud/eks"
	_ "github.com/authzed/openagentprimitives/pkg/platform/cloud/gke"
	_ "github.com/authzed/openagentprimitives/pkg/platform/cloud/local"
	_ "github.com/authzed/openagentprimitives/pkg/platform/cloud/unmanaged"
)
