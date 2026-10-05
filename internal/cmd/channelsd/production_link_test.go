package main

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// The same-package tests import kinds/all. Testing the process registry directly
// would therefore hide a missing production import and corrupt restart seeding.
func TestProductionMemoryKindsWithoutTestImports(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "list", "-deps", ".")
	cmd.Env = append(os.Environ(), "GOPROXY=off", "GOSUMDB=off", "GOTOOLCHAIN=local")
	raw, err := cmd.CombinedOutput()
	require.NoError(t, err, string(raw))
	deps := map[string]bool{}
	for _, name := range strings.Fields(string(raw)) {
		deps[name] = true
	}
	for _, kind := range []string{"sessionobservation", "goalactor", "goalconsent", "interactionhistory"} {
		require.True(t, deps["github.com/authzed/openagentprimitives/pkg/memory/kinds/"+kind], "production channelsd must register every kind in its retained signing chain: %s", kind)
	}
}
