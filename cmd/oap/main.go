// Package main is the agent-primitives CLI ("oap").
package main

import (
	"fmt"
	"os"

	"github.com/go-logr/logr"
	"k8s.io/klog/v2"

	_ "github.com/authzed/openagentprimitives/pkg/authz/pinning/kinds/cli"
	_ "github.com/authzed/openagentprimitives/pkg/authz/pinning/kinds/image"
	_ "github.com/authzed/openagentprimitives/pkg/authz/pinning/kinds/mcp"
	_ "github.com/authzed/openagentprimitives/pkg/authz/pinning/kinds/oap"
	_ "github.com/authzed/openagentprimitives/pkg/authz/pinning/kinds/skill"
	_ "github.com/authzed/openagentprimitives/pkg/authz/spicedb/relsource/imports" // completes the relsource claim table before `oap memory share` / `oap session capture`'s guarded writers are checked
	"github.com/authzed/openagentprimitives/pkg/platform/deplogs"
	_ "github.com/authzed/openagentprimitives/pkg/platform/identity/authkind/loader"
	_ "github.com/authzed/openagentprimitives/pkg/platform/identity/credkind/imports" // register the static/oauth/federated credkind.Kinds the broker dispatches to via registry.Get
	_ "github.com/authzed/openagentprimitives/pkg/platform/identity/setup/builtins/loader"
)

func main() {
	// `oap` reports its own errors through cobra and the tui package; a
	// dependency's internal logging has never been part of its UX. Same
	// reasoning as the klog.SetLogger(logr.Discard()) below. deplogs owns the
	// why — and owns it for every binary, which is the point: this used to be
	// a private helper here, and the rest of the fleet never got it.
	deplogs.Silence()

	// client-go's portforward writes "Unhandled Error" lines via klog whenever
	// a streaming consumer (e.g. SSE) closes its end of the connection. Those
	// disconnects are normal — silence klog in the CLI so the user's terminal
	// stays clean.
	klog.SetLogger(logr.Discard())

	// A double-clicked oap.app (mage desktop:app) execs this SAME binary with
	// zero arguments — turn that into `oap desktop` before cobra ever sees
	// argv. See main_bundle_launch.go for the full rationale and why this
	// needed no second launcher binary.
	os.Args = maybeInjectDesktopSubcommand(os.Args)

	if err := NewRootCmd().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
