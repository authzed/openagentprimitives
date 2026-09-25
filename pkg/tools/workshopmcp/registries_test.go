package workshopmcp

// registries_test.go blank-imports the same channel-kind, artifact-renderer,
// and sandbox-kind packages internal/cmd/workshop/registries.go does — NOT
// the whole 26-package list there, just the three registries
// TestInventory_ReportsRegisteredSets and TestInventory_RegistryParityWithRunner
// (tools_inventory_test.go) read via chregistry.Names(), rendererNames(), and
// sandboxregistry.Keys().
//
// Before Task 1 of this move, server.go/tools_inventory.go and registries.go
// were one package (internal/cmd/workshop), so any `go test` of that package
// linked registries.go's blank imports for free and those tests observed a
// real, non-empty registered set. Task 1 moved the tool files here to
// pkg/tools/workshopmcp but deliberately left registries.go behind (see its
// own header comment) so that importing workshopmcp — chiefly test/e2e,
// driving the server in-process — does not also drag in 26 self-registering
// packages it has no use for. Without this file, THIS package's own test
// binary would link none of them either, and the two tests above would
// observe empty sets — silently weakening assertions the move must not
// change.
//
// This is a test file: Go compiles it only into pkg/tools/workshopmcp's own
// `go test` binary, never into another package that imports workshopmcp
// (test/e2e included) — so it restores this package's pre-move test
// behavior without reintroducing the coupling/compile cost registries.go's
// own comment warns against. It is not exercised by, or a substitute for,
// registries.go's AST-parity check against internal/cmd/runner/main.go
// (TestInventory_RegistryParityWithRunner's "source" subtests already cover
// that against the real registries.go); keep this list's channel-kind and
// artifact-renderer entries in sync with registries.go's by hand if either
// changes — nothing currently automates that for this file.
import (
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelassets/css"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelassets/html"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelassets/image"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelassets/svg"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/agent"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/bento"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/browser"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/fake"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/github"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/local"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/slack"
	_ "github.com/authzed/openagentprimitives/pkg/tools/sandboxkinds/agentsandbox"
	_ "github.com/authzed/openagentprimitives/pkg/tools/sandboxkinds/pod"
)
