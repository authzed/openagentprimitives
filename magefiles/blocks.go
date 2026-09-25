//go:build mage
// +build mage

package main

import (
	"fmt"

	"github.com/magefile/mage/mg"

	"github.com/authzed/openagentprimitives/pkg/gen/blockcapture"
)

// Blocks captures real Slack Block Kit JSON from OAP's slack channel kind into
// the showcase demo fixtures, so the fake Slack renders what OAP actually sends
// rather than a hand-authored approximation. A change to the slack kind's
// rendering surfaces as a fixture diff.
type Blocks mg.Namespace

// blockFixtureDir lives under the slacksim src so Vite can import the JSON.
const blockFixtureDir = "showcase/demos/slacksim/src/fixtures/blockkit"

// Capture drives the slack sender for each specimen (approval / message / plan)
// and writes one JSON fixture per surface.
func (Blocks) Capture() error {
	if err := blockcapture.Generate(blockFixtureDir); err != nil {
		return err
	}
	fmt.Printf("blockcapture: wrote fixtures to %s\n", blockFixtureDir)
	return nil
}
