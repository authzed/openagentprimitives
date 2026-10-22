package main

import (
	"github.com/authzed/openagentprimitives/examples/codecovbot/coverage-tool/internal/cli"
	"os"
)

func main() {
	if err := cli.NewCmd().Execute(); err != nil {
		os.Exit(1)
	}
}
