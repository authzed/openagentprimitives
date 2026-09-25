package desktopcmd

// The cluster-kind registry is populated by blank imports, which the oap binary
// makes from main (cmd/oap/cloudimports.go). A test binary links only what this
// package's own imports reach; without these, `oap desktop`'s own kind and the
// `default` fallback its capacity check falls back to are both unresolvable,
// and the assertions below measure a skip instead of a decision.
import (
	_ "github.com/authzed/openagentprimitives/pkg/platform/cloud/aks"
	_ "github.com/authzed/openagentprimitives/pkg/platform/cloud/desktop"
	_ "github.com/authzed/openagentprimitives/pkg/platform/cloud/eks"
	_ "github.com/authzed/openagentprimitives/pkg/platform/cloud/gke"
	_ "github.com/authzed/openagentprimitives/pkg/platform/cloud/local"
	_ "github.com/authzed/openagentprimitives/pkg/platform/cloud/unmanaged"

	// The channel kinds ensureWebhookTunnel resolves a wired Channel's
	// spec.kind against. Declared here rather than left to arrive through some
	// other package's imports: whether a kind receives webhooks is what decides
	// that a tunnel is opened at all, and an unregistered kind answers "no
	// webhook" — a green test that measured nothing.
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/github"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/slack"
)
