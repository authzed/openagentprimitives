//go:build e2e

package e2e

import (
	"bytes"
	"crypto/ed25519"

	"github.com/authzed/openagentprimitives/pkg/agent/restartmarker"
)

// testMarkerSigner is the connector identity this suite's pipeline signs
// restart markers as. The operator refuses an unsigned status.pendingRestart,
// and the pipeline refuses to write one, so a fork-trigger scenario needs it.
var testMarkerSigner = restartmarker.NewSigner(
	ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x3d}, ed25519.SeedSize)),
	restartmarker.Publisher,
)
