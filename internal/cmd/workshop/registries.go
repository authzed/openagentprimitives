// registries.go blank-imports the same self-registering Kind packages
// internal/cmd/runner/main.go links, so a registry lookup inside this
// sidecar — Task 2's `inventory` tool foremost — reports the cluster's REAL
// registered set rather than whatever subset this binary happened to pull in
// transitively. Kept as its own file (mirroring the runner's own convention
// of naming these blank imports explicitly) so "what does the workshop
// sidecar register" is answerable by reading one file.
//
// The channel-kind and artifact-renderer blocks below must be kept in sync
// with internal/cmd/runner/main.go's own blank-import block;
// tools_inventory_test.go's TestInventory_RegistryParityWithRunner AST-parses
// both files and fails when they diverge.
//
// The sandbox-kind block is the one exception: it mirrors
// internal/cmd/operator/main.go, not the runner's — the runner never
// provisions a sandbox (the operator's SpiceboxSession/ToolCall controllers
// do), so there is no runner-side blank import for `inventory`'s
// SandboxClasses field to mirror. TestInventory_RegistryParityWithRunner
// checks this block against the operator instead, and says so.
//
// This file stayed in internal/cmd/workshop (package main) when the rest of
// this package's tool files moved to pkg/tools/workshopmcp: it blank-imports
// 26 self-registering packages, and dragging those into every importer of
// workshopmcp — including test/e2e, which links workshopmcp to drive the
// server in-process — would be a real coupling and compile-cost hit for a
// harness that doesn't need any of them registered. The consequence is
// deliberate: `inventory`'s registry-derived fields (ChannelKinds, Renderers,
// SandboxBackends, …), read in-process by a harness that links only
// workshopmcp, report whatever THAT harness itself links — which is the more
// honest answer for e2e than a copy of this binary's own registered set.
// pkg/tools/workshopmcp's OWN unit tests are the one exception: they need a
// real registered set to keep asserting what they asserted before the split,
// so pkg/tools/workshopmcp/registries_test.go blank-imports the
// channel-kind/artifact-renderer/sandbox-kind subset of this list — a
// _test.go file, so it never reaches any non-test importer of workshopmcp.
package main

import (
	_ "github.com/authzed/openagentprimitives/pkg/agent/completion/kinds/artifactdelivery"
	_ "github.com/authzed/openagentprimitives/pkg/agent/completion/kinds/plansteps"
	_ "github.com/authzed/openagentprimitives/pkg/agent/completion/kinds/triggerconcluded"
	_ "github.com/authzed/openagentprimitives/pkg/agent/llm/openai"       // registers "openai"
	_ "github.com/authzed/openagentprimitives/pkg/agent/postsession/cost" // registers the session-cost SessionEnd hook
	_ "github.com/authzed/openagentprimitives/pkg/agent/session/state/deliveries"
	_ "github.com/authzed/openagentprimitives/pkg/agent/session/state/openingsummary"
	_ "github.com/authzed/openagentprimitives/pkg/authz/contentguard/kinds/promptinjection" // register prompt-injection inspector
	_ "github.com/authzed/openagentprimitives/pkg/authz/contentguard/kinds/urlallowlist"    // register url-allowlist inspector
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelassets/css"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelassets/html"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelassets/image"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelassets/svg"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/agent"   // register agent (session-to-session) kind
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/bento"   // register bento (cron) kind
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/browser" // register browser (webd-hosted) kind
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/fake"    // register fake kind (tests/e2e)
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/github"  // register github kind: input-only, but its INPUT binding is what the trigger-status capability resolves
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/local"   // register local (TUI) kind
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/slack"   // register slack kind
	_ "github.com/authzed/openagentprimitives/pkg/memory/kinds/all"
	_ "github.com/authzed/openagentprimitives/pkg/platform/identity/authkind/loader"
	_ "github.com/authzed/openagentprimitives/pkg/platform/identity/credkind/imports" // register the static/oauth/federated/githubApp credkind.Kinds the broker dispatches to via registry.Get
	_ "github.com/authzed/openagentprimitives/pkg/platform/workspacekinds/git"        // register git workspace-source driver
	_ "github.com/authzed/openagentprimitives/pkg/tools/sandboxkinds/agentsandbox"    // register the agent-sandbox backend (BYO; skipped when its CRDs are absent) — mirrors the operator, see header comment
	_ "github.com/authzed/openagentprimitives/pkg/tools/sandboxkinds/pod"             // register the built-in sandbox backend — mirrors the operator, see header comment
	_ "github.com/authzed/openagentprimitives/pkg/tools/toolkitstream/claude"         // registers claude-stream-json
)
